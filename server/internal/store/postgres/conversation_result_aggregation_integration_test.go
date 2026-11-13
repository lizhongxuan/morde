package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type resultAggregationFixture struct {
	store        *Store
	ownerID      string
	conversation conversation.Conversation
}

type resultAggregationTask struct {
	ID            string
	ActorID       string
	UserMessageID string
	TurnID        string
	EventSequence int64
	Content       string
}

const resultReviewTestReply = "主助手已整理结果，并保留了每项任务的完成情况。"
const resultReviewTestModelOutput = `{"action":"reply","task_id":"","question":"","reply":"主助手已整理结果，并保留了每项任务的完成情况。"}`

type resultReviewRouteModel struct {
	calls   int
	sources []app.ConversationTaskResultSource
}

func (*resultReviewRouteModel) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (model *resultReviewRouteModel) ClassifyConversationRoute(_ context.Context, _ app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	if value.ResultReview == nil {
		return app.ConversationRouteDecision{}, fmt.Errorf("result review context is missing")
	}
	model.calls++
	model.sources = append(model.sources, value.ResultReview.Sources...)
	return app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: resultReviewTestReply,
		ModelOutput: resultReviewTestModelOutput}, nil
}

func newResultAggregationFixture(t *testing.T) resultAggregationFixture {
	t.Helper()
	store, _ := newTemporaryStore(t)
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	return resultAggregationFixture{store: store, ownerID: owner.ID, conversation: createMainConversationForTest(t, store, owner.ID)}
}

func (f resultAggregationFixture) createTask(t *testing.T, actorID, objective string) resultAggregationTask {
	t.Helper()
	ctx := context.Background()
	taskID := mustID(t, domain.NewTaskID)
	messageID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,project_id,initiator_id,model_id,objective,status,
auth_kind,auth_token_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,execution_location)
VALUES ($1,$2,$3,NULLIF($4,''),$5,'p1234567',$6,'queued','session',$7,600,10,1000,0,'server')`, taskID, f.ownerID,
			f.conversation.ID, f.conversation.ProjectID, actorID, objective, testSessionTokenID(actorID))
		if err != nil {
			return err
		}
		return insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: f.ownerID, ActorID: actorID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, TaskID: taskID, Content: objective})
	})
	if err != nil {
		t.Fatal(err)
	}
	return resultAggregationTask{ID: taskID, ActorID: actorID, UserMessageID: messageID, TurnID: turnID}
}

func (f resultAggregationFixture) complete(t *testing.T, value *resultAggregationTask, content string) bool {
	t.Helper()
	ctx := context.Background()
	var handled bool
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`, f.conversation.ID, f.ownerID).Scan(&locked); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE tasks SET status='completed',result=$2,current_step=$3,event_seq=event_seq+1
WHERE id=$1 RETURNING event_seq`, value.ID, content, value.TurnID).Scan(&value.EventSequence); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload) VALUES ($1,$2,'task.result','{}'::jsonb)`,
			value.ID, value.EventSequence); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, value.ID).Scan(&revision); err != nil {
			return err
		}
		value.Content = content
		var err error
		handled, err = queueConversationResultInTx(ctx, tx, value.ID, revision, value.EventSequence, content, 2*time.Second)
		return err
	})
	if err != nil {
		t.Fatalf("complete result fixture: %v", err)
	}
	return handled
}

func (f resultAggregationFixture) replay(t *testing.T, value resultAggregationTask) {
	t.Helper()
	ctx := context.Background()
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`, f.conversation.ID, f.ownerID).Scan(&locked); err != nil {
			return err
		}
		_, err := queueConversationResultInTx(ctx, tx, value.ID, 0, value.EventSequence, value.Content, 2*time.Second)
		return err
	})
	if err != nil {
		t.Fatalf("replay task result: %v", err)
	}
}

func (f resultAggregationFixture) runNextReview(t *testing.T) *resultReviewRouteModel {
	t.Helper()
	model := &resultReviewRouteModel{}
	routeWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: f.store, Actions: f.store, Models: model, WorkerID: "result-review-test-worker",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := routeWorker.RunOnce(context.Background())
	if err != nil || !found || model.calls != 1 {
		t.Fatalf("route result review: found=%t calls=%d err=%v", found, model.calls, err)
	}
	return model
}

func TestConversationResultReviewUsesLatestSourceTaskModel(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text)
VALUES ('p1234567',$1,'result source model one','https://example.test/v1','fixture-model',decode(repeat('ab',29),'hex'),clock_timestamp(),true),
       ('p2345678',$1,'result source model two','https://example.test/v1','fixture-model',decode(repeat('ef',29),'hex'),clock_timestamp(),true),
       ('p7654321',$1,'unready preference model','https://example.test/v1','fixture-model',decode(repeat('cd',29),'hex'),NULL,false)`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO user_preferences(user_id,selected_model_id) VALUES ($1,'p7654321')
ON CONFLICT (user_id) DO UPDATE SET selected_model_id=EXCLUDED.selected_model_id`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	first := f.createTask(t, f.ownerID, "return the first result")
	latest := f.createTask(t, f.ownerID, "return the latest result")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET model_id='p2345678' WHERE id=$1`, latest.ID); err != nil {
		t.Fatal(err)
	}
	if !f.complete(t, &first, "first task result") || !f.complete(t, &latest, "latest task result") {
		t.Fatal("task result was not queued")
	}
	if _, err := f.store.pool.Exec(ctx, `WITH now AS (SELECT clock_timestamp() AS value)
UPDATE conversation_result_batches b SET created_at=now.value-interval '3 seconds', deadline=now.value-interval '1 second'
FROM now WHERE b.state='pending' AND b.owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.QueueReadyConversationResultReviews(ctx, 10); err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "result-review-model-test", time.Minute)
	if err != nil || !found || claim.Input.ResultReviewID == "" {
		t.Fatalf("claim hidden result review route: found=%t input=%#v err=%v", found, claim.Input, err)
	}
	selected, err := f.store.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != "p2345678" {
		t.Fatalf("result review selected model = %q, want latest source Task model p2345678: %v", selected, err)
	}
}

func TestConversationResultAggregationQueuesHiddenReviewAndReplaysOnce(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first question")
	second := f.createTask(t, f.ownerID, "second question")
	if !f.complete(t, &first, "first answer") {
		t.Fatal("unstreamed result was not queued")
	}
	details, err := f.store.GetTask(ctx, first.ID, f.ownerID)
	if err != nil || !details.ResultMessagePending || details.ResultMessagePublished || details.AssistantMessage != nil ||
		details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != 0 || details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != 0 {
		t.Fatalf("queued task projection = %#v err=%v", details, err)
	}
	var deadline time.Time
	var batchID string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id,deadline FROM conversation_result_batches WHERE state='pending'`).Scan(&batchID, &deadline); err != nil {
		t.Fatal(err)
	}
	const replayers = 4
	var group sync.WaitGroup
	for i := 0; i < replayers; i++ {
		group.Add(1)
		go func() { defer group.Done(); f.replay(t, first) }()
	}
	group.Wait()
	var retainedDeadline time.Time
	var sources int
	if err := f.store.pool.QueryRow(ctx, `SELECT b.deadline,(SELECT count(*) FROM conversation_result_sources WHERE batch_message_id=b.message_id)
FROM conversation_result_batches b WHERE b.message_id=$1`, batchID).Scan(&retainedDeadline, &sources); err != nil || !deadline.Equal(retainedDeadline) || sources != 1 {
		t.Fatalf("replay changed deadline or duplicated source: deadline=%v sources=%d err=%v", retainedDeadline, sources, err)
	}
	if !f.complete(t, &second, "second answer") {
		t.Fatal("second result was not queued")
	}
	var state, triggerID, triggerKind, triggerContent string
	if err := f.store.pool.QueryRow(ctx, `SELECT batch.state,batch.trigger_message_id,message.source_kind,message.content
FROM conversation_result_batches batch JOIN conversation_messages message ON message.id=batch.trigger_message_id
WHERE batch.message_id=$1`, batchID).Scan(&state, &triggerID, &triggerKind, &triggerContent); err != nil ||
		state != "processing" || triggerKind != "task_result_review" || triggerContent != "Completed Task results are ready for review." || triggerID == batchID {
		t.Fatalf("result review route = state %q trigger %q kind %q content %q err=%v", state, triggerID, triggerKind, triggerContent, err)
	}
	var assistantMessages int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'`,
		f.conversation.ID).Scan(&assistantMessages); err != nil || assistantMessages != 0 {
		t.Fatalf("raw Task results were published before review: assistant messages=%d err=%v", assistantMessages, err)
	}
	visibleBeforeReview, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range visibleBeforeReview.Items {
		if message.ID == triggerID || message.SourceKind == "task_result_review" {
			t.Fatalf("internal result review route leaked into conversation messages: %+v", message)
		}
	}
	model := f.runNextReview(t)
	if len(model.sources) != 2 || model.sources[0].TaskID != first.ID || model.sources[0].TaskRevision != 0 ||
		model.sources[0].EventSequence != first.EventSequence || model.sources[0].Content != "first answer" ||
		model.sources[1].TaskID != second.ID || model.sources[1].TaskRevision != 0 ||
		model.sources[1].EventSequence != second.EventSequence || model.sources[1].Content != "second answer" {
		t.Fatalf("coordinator received incorrect result sources: %#v", model.sources)
	}
	var publishedMessageID, routeState string
	if err := f.store.pool.QueryRow(ctx, `SELECT batch.published_message_id,route.state FROM conversation_result_batches batch
JOIN conversation_message_routes route ON route.message_id=batch.trigger_message_id WHERE batch.message_id=$1 AND batch.state='published'`,
		batchID).Scan(&publishedMessageID, &routeState); err != nil || publishedMessageID != batchID || routeState != "routed" {
		t.Fatalf("review publication = message %q route %q err=%v", publishedMessageID, routeState, err)
	}
	provenance, err := f.store.ListConversationResultSources(ctx, f.ownerID, f.conversation.ID, []string{batchID})
	if err != nil || len(provenance[batchID]) != 2 || provenance[batchID][0].TaskID != first.ID ||
		provenance[batchID][0].UserMessageID != first.UserMessageID || provenance[batchID][0].EventSequence != first.EventSequence ||
		provenance[batchID][1].TaskID != second.ID || provenance[batchID][1].UserMessageID != second.UserMessageID ||
		provenance[batchID][1].EventSequence != second.EventSequence {
		t.Fatalf("published source evidence = %#v err=%v", provenance, err)
	}
	page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var replies []conversation.Message
	for _, message := range page.Items {
		if message.Role == conversation.RoleAssistant {
			replies = append(replies, message)
		}
	}
	if len(replies) != 1 || replies[0].ID != batchID || replies[0].Content != resultReviewTestReply || len(replies[0].Sources) != 2 {
		t.Fatalf("reviewed reply = %#v", replies)
	}
	for _, value := range []resultAggregationTask{first, second} {
		details, err := f.store.GetTask(ctx, value.ID, f.ownerID)
		if err != nil || details.ResultReview == nil || details.ResultReview.State != "published" || details.ResultReview.SourceTaskID != value.ID ||
			details.ResultReview.SourceTaskRevision != 0 || details.ResultReview.SourceEventSequence != value.EventSequence ||
			details.ResultReview.ReplyMessageID == nil || *details.ResultReview.ReplyMessageID != batchID {
			t.Fatalf("published task projection for %s = %#v err=%v", value.ID, details, err)
		}
	}
	f.replay(t, first)
	var reviewRoutes int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_result_batches),
 (SELECT count(*) FROM conversation_message_routes WHERE request_metadata->>'result_review_id'=$1),
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$2 AND role='assistant')`, batchID, f.conversation.ID).
		Scan(&sources, &reviewRoutes, &assistantMessages); err != nil || sources != 1 || reviewRoutes != 1 || assistantMessages != 1 {
		t.Fatalf("published replay duplicated review or reply: batches=%d routes=%d replies=%d err=%v", sources, reviewRoutes, assistantMessages, err)
	}
}

func TestConversationRouteKeepsRecentPublishedTaskResultPastHistoryWindow(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'result history model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	result := f.createTask(t, f.ownerID, "summarize the synthetic launch risks")
	if !f.complete(t, &result, "synthetic task result") {
		t.Fatal("task result was not queued")
	}
	f.runNextReview(t)
	var resultMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT published_message_id FROM conversation_result_batches WHERE state='published'`).Scan(&resultMessageID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		for index := 0; index < 10; index++ {
			messageID, err := domain.NewMessageID()
			if err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: f.ownerID, ActorID: f.ownerID,
				ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: fmt.Sprintf("unrelated turn %d", index),
				SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	history := f.routeHistoryForActor(t, f.ownerID)
	resultFound, requestFound := false, false
	for _, message := range history {
		if message.MessageID == resultMessageID && message.Content == resultReviewTestReply {
			resultFound = true
		}
		if message.MessageID == result.UserMessageID && message.Content == "summarize the synthetic launch risks" {
			requestFound = true
		}
	}
	if !resultFound || !requestFound {
		t.Fatalf("route history after ten unrelated turns omitted the published result or its original request: result=%t request=%t history=%#v",
			resultFound, requestFound, history)
	}
}

func TestConversationResultReviewRetryRestoresFailedBatch(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	taskResult := f.createTask(t, f.ownerID, "retry result review")
	otherTask := f.createTask(t, f.ownerID, "stop blocking result review")
	if !f.complete(t, &taskResult, "review after retry") {
		t.Fatal("task result was not queued")
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='failed' WHERE id=$1`, otherTask.ID); err != nil {
		t.Fatalf("stop remaining task before result review: %v", err)
	}
	if count, err := f.store.QueueReadyConversationResultReviews(ctx, 10); err != nil || count != 1 {
		t.Fatalf("result review was not queued: count=%d err=%v", count, err)
	}
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "result-review-retry-test", time.Minute)
	if err != nil || !found || claim.Input.ResultReviewID == "" {
		t.Fatalf("claim hidden result review route: found=%t input=%#v err=%v", found, claim.Input, err)
	}
	if err := f.store.FailConversationRoute(ctx, claim, "routing_context_unavailable"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Kind: identity.KindSession, TokenID: testSessionTokenID(f.ownerID)}
	if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, claim.Message.ID, instanceID, "review-retry-key-001"); err != nil {
		t.Fatalf("retry failed result review: %v", err)
	}
	var batchState, routeState string
	var triggerID string
	var retryAttempt int64
	if err := f.store.pool.QueryRow(ctx, `SELECT batch.state,route.state,batch.trigger_message_id,batch.retry_attempt
FROM conversation_result_batches batch JOIN conversation_message_routes route ON route.message_id=batch.trigger_message_id
WHERE batch.message_id=$1`, claim.Input.ResultReviewID).Scan(&batchState, &routeState, &triggerID, &retryAttempt); err != nil ||
		batchState != "processing" || routeState != "pending" || triggerID != claim.Message.ID || retryAttempt != 1 {
		t.Fatalf("retried review state = batch %q route %q trigger %q attempt %d err=%v", batchState, routeState, triggerID, retryAttempt, err)
	}
	secondClaim, found, err := f.store.ClaimConversationRoute(ctx, "result-review-second-retry-test", time.Minute)
	if err != nil || !found || secondClaim.Message.ID != triggerID {
		t.Fatalf("claim retried hidden result review route: found=%t message=%q err=%v", found, secondClaim.Message.ID, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, secondClaim)
	if err != nil || conversationRouteModelPhase(snapshot.Context) != "result_review_1" {
		t.Fatalf("first retry model phase = %q err=%v", conversationRouteModelPhase(snapshot.Context), err)
	}
	if err := f.store.FailConversationRoute(ctx, secondClaim, "routing_context_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, secondClaim.Message.ID, instanceID, "review-retry-key-002"); err != nil {
		t.Fatalf("retry failed result review again: %v", err)
	}
	thirdClaim, found, err := f.store.ClaimConversationRoute(ctx, "result-review-third-retry-test", time.Minute)
	if err != nil || !found || thirdClaim.Message.ID != triggerID {
		t.Fatalf("claim second retried result review route: found=%t message=%q err=%v", found, thirdClaim.Message.ID, err)
	}
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, thirdClaim)
	if err != nil || conversationRouteModelPhase(snapshot.Context) != "result_review_2" {
		t.Fatalf("second retry model phase = %q err=%v", conversationRouteModelPhase(snapshot.Context), err)
	}
	if err := f.store.ReleaseConversationRoute(ctx, thirdClaim); err != nil {
		t.Fatal(err)
	}
	f.runNextReview(t)
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_batches WHERE message_id=$1`, claim.Input.ResultReviewID).Scan(&batchState); err != nil || batchState != "published" {
		t.Fatalf("retried result review batch state = %q err=%v", batchState, err)
	}
	var routes, batches int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_message_routes WHERE request_metadata->>'result_review_id'=$1),
(SELECT count(*) FROM conversation_result_batches WHERE owner_id=$2)`, claim.Input.ResultReviewID, f.ownerID).Scan(&routes, &batches); err != nil || routes != 1 || batches != 1 {
		t.Fatalf("retry duplicated review identity: routes=%d batches=%d err=%v", routes, batches, err)
	}
}

func TestConversationResultAggregationRecoversDeadlineAndSupersedesOldRevision(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first task")
	f.createTask(t, f.ownerID, "other task")
	if !f.complete(t, &first, "old answer") {
		t.Fatal("result was not queued")
	}
	var batchID string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches WHERE state='pending'`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp() AS now) UPDATE conversation_result_batches
SET created_at=t.now-interval '3 seconds',deadline=t.now-interval '1 second' FROM t WHERE message_id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	restarted := New(f.store.pool)
	if count, err := restarted.QueueReadyConversationResultReviews(ctx, 100); err != nil || count != 0 {
		t.Fatalf("outdated result was queued: count=%d err=%v", count, err)
	}
	var batchState, sourceState string
	var triggerID string
	var messages int
	if err := f.store.pool.QueryRow(ctx, `SELECT b.state,s.state,COALESCE(b.trigger_message_id,''),(SELECT count(*) FROM conversation_messages WHERE id=b.message_id)
FROM conversation_result_batches b JOIN conversation_result_sources s ON s.batch_message_id=b.message_id WHERE b.message_id=$1`, batchID).
		Scan(&batchState, &sourceState, &triggerID, &messages); err != nil || batchState != "superseded" || sourceState != "superseded" || triggerID != "" || messages != 0 {
		t.Fatalf("old revision not suppressed: %s %s trigger=%q messages=%d err=%v", batchState, sourceState, triggerID, messages, err)
	}
}

func TestConversationResultAggregationQueuesSingleAndPreservesStream(t *testing.T) {
	t.Run("no other active task", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "single")
		if !f.complete(t, &value, "single answer") {
			t.Fatal("single result was not handled")
		}
		var state, triggerID string
		var assistants int
		if err := f.store.pool.QueryRow(context.Background(), `SELECT state,trigger_message_id,
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant')
FROM conversation_result_batches`, f.conversation.ID).Scan(&state, &triggerID, &assistants); err != nil || state != "processing" || triggerID == "" || assistants != 0 {
			t.Fatalf("single result was not queued for review: state=%s trigger=%q assistants=%d err=%v", state, triggerID, assistants, err)
		}
	})
	t.Run("already streaming", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "streaming")
		f.createTask(t, f.ownerID, "other active")
		messageID := mustID(t, domain.NewMessageID)
		if err := f.store.WithTx(context.Background(), func(tx pgx.Tx) error {
			message := conversation.Message{ID: messageID, ConversationID: f.conversation.ID, OwnerID: f.ownerID,
				ActorID: f.ownerID, Role: conversation.RoleAssistant, Content: "visible prefix", TaskID: value.ID}
			if err := insertMessage(context.Background(), tx, message); err != nil {
				return err
			}
			if _, err := tx.Exec(context.Background(), `UPDATE conversation_messages SET output_status='streaming',output_task_revision=0 WHERE id=$1`, messageID); err != nil {
				return err
			}
			_, err := tx.Exec(context.Background(), `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible prefix','streaming',0)`, value.ID, messageID, value.TurnID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if f.complete(t, &value, "visible prefix and final text") {
			t.Fatal("streamed result was moved into a batch")
		}
		var count int
		var snapshotID, text string
		if err := f.store.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM conversation_result_batches),message_id,content
FROM task_assistant_message_snapshots WHERE task_id=$1`, value.ID).Scan(&count, &snapshotID, &text); err != nil ||
			count != 0 || snapshotID != messageID || text != "visible prefix" {
			t.Fatalf("visible stream was rewritten: batches=%d id=%q text=%q err=%v", count, snapshotID, text, err)
		}
	})
	t.Run("orphaned streaming snapshot", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "orphaned stream")
		f.createTask(t, f.ownerID, "other active")
		if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible prefix','streaming',0)`,
			value.ID, mustID(t, domain.NewMessageID), value.TurnID); err != nil {
			t.Fatal(err)
		}
		if !f.complete(t, &value, "visible prefix and final text") {
			t.Fatal("orphaned stream snapshot suppressed the result review")
		}
	})
	t.Run("other turn with same prefix", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "new turn")
		f.createTask(t, f.ownerID, "other active")
		messageID := mustID(t, domain.NewMessageID)
		otherTurn := mustID(t, domain.NewStepID)
		if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible prefix','interrupted',0)`, value.ID, messageID, otherTurn); err != nil {
			t.Fatal(err)
		}
		if !f.complete(t, &value, "visible prefix and unrelated final text") {
			t.Fatal("another turn incorrectly suppressed the new result")
		}
		var count int
		if err := f.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversation_result_sources
WHERE task_id=$1 AND state='pending'`, value.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("new turn result was not queued: count=%d err=%v", count, err)
		}
	})
}

func TestConversationResultAggregationDeadlineAndConcurrentQueue(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first task")
	second := f.createTask(t, f.ownerID, "slow task")
	f.complete(t, &first, "first answer")
	if _, err := f.store.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp() AS now) UPDATE conversation_result_batches
SET created_at=t.now-interval '3 seconds',deadline=t.now-interval '1 second' FROM t WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	const workers = 4
	counts := make(chan int, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			count, err := New(f.store.pool).QueueReadyConversationResultReviews(ctx, 100)
			counts <- count
			errs <- err
		}()
	}
	group.Wait()
	close(counts)
	close(errs)
	total := 0
	for count := range counts {
		total += count
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent publishing: %v", err)
		}
	}
	if total != 1 {
		t.Fatalf("concurrent workers queued %d result reviews", total)
	}
	f.complete(t, &second, "later answer")
	f.runNextReview(t)
	f.runNextReview(t)
	var batches, messages int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_result_batches WHERE state='published'),
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant')`, f.conversation.ID).Scan(&batches, &messages); err != nil || batches != 2 || messages != 2 {
		t.Fatalf("post-deadline result was not separate: batches=%d messages=%d err=%v", batches, messages, err)
	}
}

func TestConversationResultAggregationQueuesWhenRemainingTaskStops(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "finished task")
	other := f.createTask(t, f.ownerID, "stopped task")
	f.complete(t, &first, "finished answer")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='failed' WHERE id=$1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := f.store.QueueReadyConversationResultReviews(ctx, 100); err != nil || count != 1 {
		t.Fatalf("result review was not queued after other task stopped: count=%d err=%v", count, err)
	}
	var state, triggerKind string
	if err := f.store.pool.QueryRow(ctx, `SELECT batch.state,message.source_kind FROM conversation_result_batches batch
JOIN conversation_messages message ON message.id=batch.trigger_message_id`).Scan(&state, &triggerKind); err != nil || state != "processing" || triggerKind != "task_result_review" {
		t.Fatalf("stopped-task result route = state %q source %q err=%v", state, triggerKind, err)
	}
}

func TestConversationResultAggregationDoesNotWaitForPausedOrInteractiveTasks(t *testing.T) {
	for _, status := range []string{"waiting_user", "paused", "pausing", "cancelling"} {
		t.Run(status, func(t *testing.T) {
			f := newResultAggregationFixture(t)
			ctx := context.Background()
			first := f.createTask(t, f.ownerID, "finished task")
			other := f.createTask(t, f.ownerID, "not actively running")
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, other.ID, status); err != nil {
				t.Fatal(err)
			}
			f.complete(t, &first, "finished answer")
			var state, triggerID string
			if err := f.store.pool.QueryRow(ctx, `SELECT state,trigger_message_id FROM conversation_result_batches`).Scan(&state, &triggerID); err != nil || state != "processing" || triggerID == "" {
				t.Fatalf("result review waited for %s task: state=%s trigger=%q err=%v", status, state, triggerID, err)
			}
		})
	}
}

func TestConversationResultAggregationDoesNotExtendExpiredWindow(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first task")
	second := f.createTask(t, f.ownerID, "later task")
	f.complete(t, &first, "first answer")
	if _, err := f.store.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp() AS now) UPDATE conversation_result_batches
SET created_at=t.now-interval '3 seconds',deadline=t.now-interval '1 second' FROM t WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	// The second completion itself encounters the expired batch before any
	// review-queue worker runs; it must close that batch rather than extend it.
	f.complete(t, &second, "later answer")
	f.runNextReview(t)
	f.runNextReview(t)
	var batches, messages int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_result_batches WHERE state='published'),
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant')`, f.conversation.ID).Scan(&batches, &messages); err != nil || batches != 2 || messages != 2 {
		t.Fatalf("expired result window was extended: batches=%d messages=%d err=%v", batches, messages, err)
	}
}

func TestConversationResultAggregationConcurrentRevisionCannotPublishOldResult(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "old revision")
	f.createTask(t, f.ownerID, "other work")
	f.complete(t, &first, "old answer")
	if _, err := f.store.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp() AS now) UPDATE conversation_result_batches
SET created_at=t.now-interval '3 seconds',deadline=t.now-interval '1 second' FROM t WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, f.conversation.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		count int
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		count, err := f.store.QueueReadyConversationResultReviews(ctx, 100)
		done <- outcome{count, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("result reviewer bypassed revision transaction: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.count != 0 {
			t.Fatalf("obsolete result was queued: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("result review queue did not resume after revision commit")
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_sources WHERE task_id=$1`, first.ID).Scan(&state); err != nil || state != "superseded" {
		t.Fatalf("revision race result state = %q err=%v", state, err)
	}
}

func TestConversationResultAggregationPreservesLargeResultsAcrossReviews(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first long result")
	second := f.createTask(t, f.ownerID, "second long result")
	one := strings.Repeat("a", 20_000)
	two := strings.Repeat("b", 20_000)
	f.complete(t, &first, one)
	f.complete(t, &second, two)
	firstReview := f.runNextReview(t)
	secondReview := f.runNextReview(t)
	if len(firstReview.sources) != 1 || len(secondReview.sources) != 1 ||
		(firstReview.sources[0].Content != one && firstReview.sources[0].Content != two) ||
		(secondReview.sources[0].Content != one && secondReview.sources[0].Content != two) ||
		firstReview.sources[0].Content == secondReview.sources[0].Content {
		t.Fatalf("large result sources were truncated or combined incorrectly: first=%d second=%d",
			len(firstReview.sources), len(secondReview.sources))
	}
	var messages, exactResults int
	if err := f.store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant' AND content=$2),
 (SELECT count(*) FROM conversation_result_sources WHERE conversation_id=$3 AND octet_length(content)=20000)`,
		f.conversation.ID, resultReviewTestReply, f.conversation.ID).Scan(&messages, &exactResults); err != nil || messages != 2 || exactResults != 2 {
		t.Fatalf("large results were not kept separate from reviewed replies: replies=%d exact sources=%d err=%v", messages, exactResults, err)
	}
}

func TestConversationResultAggregationRevisionRequiresAppliedMessage(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "original request")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, f.conversation.ID).Scan(&locked); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status='completed',result='revised answer',current_step=$2,event_seq=1 WHERE id=$1`, value.ID, value.TurnID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload) VALUES ($1,1,'task.result','{}'::jsonb)`, value.ID); err != nil {
			return err
		}
		_, err := queueConversationResultInTx(ctx, tx, value.ID, 1, 1, "revised answer", 2*time.Second)
		return err
	})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("revision without its applied user message was accepted: %v", err)
	}
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(f.ownerID))
	if err != nil {
		t.Fatal(err)
	}
	modifiedID := mustID(t, domain.NewMessageID)
	hash := sha256.Sum256([]byte("revised user request"))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: modifiedID, OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "updated request"},
		ServiceInstanceID: instanceID, ClientID: "result-revision-message", RequestHash: hash[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$3,$4,$5,0,'applied')`, modifiedID, value.ID, f.ownerID, f.conversation.ID, message.Sequence); err != nil {
		t.Fatal(err)
	}
	if !f.complete(t, &value, "revised answer") {
		t.Fatal("applied revision did not produce a result")
	}
	var sourceMessage string
	var revision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT user_message_id,task_revision FROM conversation_result_sources WHERE task_id=$1`, value.ID).
		Scan(&sourceMessage, &revision); err != nil || sourceMessage != modifiedID || revision != 1 {
		t.Fatalf("revised result source = %q revision=%d err=%v", sourceMessage, revision, err)
	}
}

func TestConversationResultAggregationBatchRevisionUsesLastAppliedMessage(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "original request")
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(f.ownerID))
	if err != nil {
		t.Fatal(err)
	}
	var latestID string
	for revision := int64(0); revision < 3; revision++ {
		latestID = mustID(t, domain.NewMessageID)
		content := fmt.Sprintf("modification %d", revision)
		hash := sha256.Sum256([]byte(content))
		message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
			Message: conversation.Message{ID: latestID, OwnerID: f.ownerID, ActorID: f.ownerID,
				ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: content},
			ServiceInstanceID: instanceID, ClientID: fmt.Sprintf("result-batch-revision-%d", revision), RequestHash: hash[:],
			Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$3,$4,$5,$6,'applied')`, latestID, value.ID, f.ownerID, f.conversation.ID, message.Sequence, revision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=3,desired_revision=3 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if !f.complete(t, &value, "combined revision answer") {
		t.Fatal("batched revision result was not handled")
	}
	var actualID string
	var revision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT user_message_id,task_revision FROM conversation_result_sources WHERE task_id=$1`, value.ID).
		Scan(&actualID, &revision); err != nil || actualID != latestID || revision != 3 {
		t.Fatalf("batched revision source = %q revision=%d err=%v", actualID, revision, err)
	}
}

func TestConversationResultAggregationProjectActorsBatchSeparatelyAndShareVerifiedRouteHistory(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'result model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	p := createProjectStoreFixture(t, ctx, store, owner.ID, "result project", "fixture", "result-project-create")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, p, project.RoleMember,
		p.Version, "result-project-invite", "result-project-accept"); err != nil {
		t.Fatal(err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	value, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{ID: conversationID, OwnerID: owner.ID, ProjectID: p.ID},
		testIdempotency(owner.ID, "project_conversation.create", "result-project-conversation"))
	if err != nil {
		t.Fatal(err)
	}
	f := resultAggregationFixture{store: store, ownerID: owner.ID, conversation: value}
	a1 := f.createTask(t, owner.ID, "owner question one")
	a2 := f.createTask(t, owner.ID, "owner question two")
	b1 := f.createTask(t, member.ID, "member question one")
	b2 := f.createTask(t, member.ID, "member question two")
	f.complete(t, &a1, "owner first answer")
	f.complete(t, &b1, "member first answer")
	var pending int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_batches WHERE state='pending'`).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("project actors shared a batch: pending=%d err=%v", pending, err)
	}
	f.complete(t, &a2, "owner second answer")
	f.complete(t, &b2, "member second answer")
	ownerReview := f.runNextReview(t)
	memberReview := f.runNextReview(t)
	if len(ownerReview.sources) != 2 || len(memberReview.sources) != 2 ||
		ownerReview.sources[0].TaskID != a1.ID || ownerReview.sources[1].TaskID != a2.ID ||
		memberReview.sources[0].TaskID != b1.ID || memberReview.sources[1].TaskID != b2.ID {
		t.Fatalf("project actor result reviews crossed sources: owner=%#v member=%#v", ownerReview.sources, memberReview.sources)
	}
	var ownerMessage, memberMessage string
	if err := store.pool.QueryRow(ctx, `SELECT published_message_id FROM conversation_result_batches WHERE actor_id=$1 AND state='published'`, owner.ID).Scan(&ownerMessage); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT published_message_id FROM conversation_result_batches WHERE actor_id=$1 AND state='published'`, member.ID).Scan(&memberMessage); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListConversationResultSources(ctx, member.ID, value.ID, []string{ownerMessage, memberMessage})
	if err != nil || len(sources[ownerMessage]) != 2 || len(sources[memberMessage]) != 2 ||
		sources[ownerMessage][0].TaskID != a1.ID || sources[memberMessage][0].TaskID != b1.ID {
		t.Fatalf("project source identities = %#v err=%v", sources, err)
	}
	if _, err := store.ListConversationResultSources(ctx, outsider.ID, value.ID, []string{ownerMessage}); err == nil {
		t.Fatal("nonmember read project result sources")
	}
	ownerHistory := f.routeHistoryForActor(t, owner.ID)
	memberHistory := f.routeHistoryForActor(t, member.ID)
	contains := func(values []app.ConversationRouteHistory, id string) bool {
		for _, item := range values {
			if item.MessageID == id {
				return true
			}
		}
		return false
	}
	sharedHistoryIDs := []string{a1.UserMessageID, a2.UserMessageID, b1.UserMessageID, b2.UserMessageID,
		ownerMessage, memberMessage}
	// Batches stay actor-specific, but verified published results are shared project conversation history.
	for actor, history := range map[string][]app.ConversationRouteHistory{"owner": ownerHistory, "member": memberHistory} {
		for _, id := range sharedHistoryIDs {
			if !contains(history, id) {
				t.Fatalf("%s route context omitted a verified published project result source %s: %#v", actor, id, history)
			}
		}
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		for index := 0; index < 10; index++ {
			messageID, err := domain.NewMessageID()
			if err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: owner.ID, ActorID: owner.ID,
				ConversationID: value.ID, Role: conversation.RoleUser, Content: fmt.Sprintf("unrelated project turn %d", index),
				SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ownerHistory = f.routeHistoryForActor(t, owner.ID)
	memberHistory = f.routeHistoryForActor(t, member.ID)
	for actor, history := range map[string][]app.ConversationRouteHistory{"owner": ownerHistory, "member": memberHistory} {
		for _, id := range []string{ownerMessage, memberMessage} {
			if !contains(history, id) {
				t.Fatalf("%s route context omitted a recent published project result outside the newest-message window %s: %#v", actor, id, history)
			}
		}
	}
	for actor, history := range map[string][]app.ConversationRouteHistory{"owner": ownerHistory, "member": memberHistory} {
		own := []string{a1.UserMessageID, a2.UserMessageID}
		if actor == "member" {
			own = []string{b1.UserMessageID, b2.UserMessageID}
		}
		for _, id := range own {
			if !contains(history, id) {
				t.Fatalf("%s route context omitted its published Task request %s: %#v", actor, id, history)
			}
		}
	}
}

func TestConversationResultAggregationDoesNotPublishForRemovedProjectMember(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	p := createProjectStoreFixture(t, ctx, store, owner.ID, "result revoke", "fixture", "result-revoke-project")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, p, project.RoleMember,
		p.Version, "result-revoke-invite", "result-revoke-accept"); err != nil {
		t.Fatal(err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	value, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{ID: conversationID, OwnerID: owner.ID, ProjectID: p.ID},
		testIdempotency(owner.ID, "project_conversation.create", "result-revoke-conversation"))
	if err != nil {
		t.Fatal(err)
	}
	f := resultAggregationFixture{store: store, ownerID: owner.ID, conversation: value}
	first := f.createTask(t, member.ID, "member work")
	f.createTask(t, member.ID, "remaining work")
	f.complete(t, &first, "member result")
	current, err := store.GetProject(ctx, owner.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, p.ID, member.ID, current.Version, "result-revoke-member"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp() AS now) UPDATE conversation_result_batches
SET created_at=t.now-interval '3 seconds',deadline=t.now-interval '1 second' FROM t WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	if count, err := store.QueueReadyConversationResultReviews(ctx, 100); err != nil || count != 0 {
		t.Fatalf("removed project member result was queued: count=%d err=%v", count, err)
	}
	var state, messageID string
	var triggerID string
	var published int
	if err := store.pool.QueryRow(ctx, `SELECT b.state,b.message_id,COALESCE(b.trigger_message_id,''),(SELECT count(*) FROM conversation_messages WHERE id=b.message_id)
FROM conversation_result_batches b WHERE b.actor_id=$1`, member.ID).Scan(&state, &messageID, &triggerID, &published); err != nil || state != "pending" || triggerID != "" || published != 0 {
		t.Fatalf("removed member result was not retained safely: state=%s trigger=%q published=%d err=%v", state, triggerID, published, err)
	}
	if _, err := store.ListConversationResultSources(ctx, member.ID, value.ID, []string{messageID}); err == nil {
		t.Fatal("removed member read project result sources")
	}
}

func (f resultAggregationFixture) routeHistoryForActor(t *testing.T, actorID string) []app.ConversationRouteHistory {
	t.Helper()
	ctx := context.Background()
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(actorID))
	if err != nil {
		t.Fatal(err)
	}
	input := app.ConversationRoutingInput{ProjectID: f.conversation.ProjectID}
	messageID := mustID(t, domain.NewMessageID)
	clientID := "result-context-" + actorID + "-" + messageID
	hash := sha256.Sum256([]byte("route-context-" + clientID))
	_, _, err = f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: messageID, OwnerID: f.ownerID, ActorID: actorID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "follow-up"},
		ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:], Input: input,
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "result-context-worker", time.Minute)
	if err != nil || !found || claim.Message.ActorID != actorID {
		t.Fatalf("claim result context: actor=%s found=%t err=%v", claim.Message.ActorID, found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return checkConversationRouteModelFence(ctx, tx, app.ConversationRouteModelFence{Claim: claim, Context: snapshot.Context}, "p1234567")
	}); err != nil {
		t.Fatalf("route model rejected authorized combined history: %v", err)
	}
	if err := f.store.DeferConversationRoute(ctx, claim, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return snapshot.Context.History
}
