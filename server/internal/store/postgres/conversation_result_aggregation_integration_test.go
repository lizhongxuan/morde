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

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
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

func TestConversationResultAggregationMergesExactSourcesAndReplays(t *testing.T) {
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
	var content, state string
	var taskID *string
	if err := f.store.pool.QueryRow(ctx, `SELECT m.content,m.task_id,b.state FROM conversation_result_batches b
JOIN conversation_messages m ON m.id=b.published_message_id WHERE b.message_id=$1`, batchID).Scan(&content, &taskID, &state); err != nil ||
		content != "first answer\n\nsecond answer" || taskID != nil || state != "published" {
		t.Fatalf("combined conversation message = %q task=%v state=%q err=%v", content, taskID, state, err)
	}
	provenance, err := f.store.ListConversationResultSources(ctx, f.ownerID, f.conversation.ID, []string{batchID})
	if err != nil || len(provenance[batchID]) != 2 || provenance[batchID][0].TaskID != first.ID ||
		provenance[batchID][0].UserMessageID != first.UserMessageID || provenance[batchID][0].EventSequence != first.EventSequence ||
		provenance[batchID][1].TaskID != second.ID || provenance[batchID][1].UserMessageID != second.UserMessageID ||
		provenance[batchID][1].EventSequence != second.EventSequence {
		t.Fatalf("combined source evidence = %#v err=%v", provenance, err)
	}
	var snapshots int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_assistant_message_snapshots snapshot
JOIN conversation_result_sources source ON source.task_id=snapshot.task_id AND source.batch_message_id=snapshot.message_id
 AND source.task_revision=snapshot.task_revision AND source.event_sequence=snapshot.result_event_sequence
WHERE snapshot.message_id=$1 AND snapshot.status='completed' AND source.state='published'`, batchID).Scan(&snapshots); err != nil || snapshots != 2 {
		t.Fatalf("source snapshots = %d err=%v", snapshots, err)
	}
	for _, value := range []resultAggregationTask{first, second} {
		details, err := f.store.GetTask(ctx, value.ID, f.ownerID)
		if err != nil || details.ResultMessagePending || !details.ResultMessagePublished || details.AssistantMessage == nil ||
			details.AssistantMessage.Message.ID != batchID || details.AssistantMessage.TaskRevision == nil || *details.AssistantMessage.TaskRevision != 0 ||
			details.AssistantMessage.ResultEventSequence == nil || *details.AssistantMessage.ResultEventSequence != value.EventSequence {
			t.Fatalf("published task projection for %s = %#v err=%v", value.ID, details, err)
		}
	}
	f.replay(t, first)
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_batches`).Scan(&sources); err != nil || sources != 1 {
		t.Fatalf("published replay made another batch: %d err=%v", sources, err)
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
	if count, err := restarted.PublishReadyConversationResults(ctx, 100); err != nil || count != 0 {
		t.Fatalf("outdated result was published: count=%d err=%v", count, err)
	}
	var batchState, sourceState string
	var messages int
	if err := f.store.pool.QueryRow(ctx, `SELECT b.state,s.state,(SELECT count(*) FROM conversation_messages WHERE id=b.message_id)
FROM conversation_result_batches b JOIN conversation_result_sources s ON s.batch_message_id=b.message_id WHERE b.message_id=$1`, batchID).
		Scan(&batchState, &sourceState, &messages); err != nil || batchState != "superseded" || sourceState != "superseded" || messages != 0 {
		t.Fatalf("old revision not suppressed: %s %s messages=%d err=%v", batchState, sourceState, messages, err)
	}
}

func TestConversationResultAggregationPublishesSingleAndPreservesStream(t *testing.T) {
	t.Run("no other active task", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "single")
		if !f.complete(t, &value, "single answer") {
			t.Fatal("single result was not handled")
		}
		var state string
		if err := f.store.pool.QueryRow(context.Background(), `SELECT state FROM conversation_result_batches`).Scan(&state); err != nil || state != "published" {
			t.Fatalf("single result was delayed: %s err=%v", state, err)
		}
	})
	t.Run("already streaming", func(t *testing.T) {
		f := newResultAggregationFixture(t)
		value := f.createTask(t, f.ownerID, "streaming")
		f.createTask(t, f.ownerID, "other active")
		messageID := mustID(t, domain.NewMessageID)
		if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible prefix','streaming',0)`, value.ID, messageID, value.TurnID); err != nil {
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

func TestConversationResultAggregationDeadlineAndConcurrentPublish(t *testing.T) {
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
			count, err := New(f.store.pool).PublishReadyConversationResults(ctx, 100)
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
		t.Fatalf("concurrent workers published %d messages", total)
	}
	f.complete(t, &second, "later answer")
	var batches, messages int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_result_batches WHERE state='published'),
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant')`, f.conversation.ID).Scan(&batches, &messages); err != nil || batches != 2 || messages != 2 {
		t.Fatalf("post-deadline result was not separate: batches=%d messages=%d err=%v", batches, messages, err)
	}
}

func TestConversationResultAggregationPublishesWhenRemainingTaskStops(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "finished task")
	other := f.createTask(t, f.ownerID, "stopped task")
	f.complete(t, &first, "finished answer")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='failed' WHERE id=$1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := f.store.PublishReadyConversationResults(ctx, 100); err != nil || count != 1 {
		t.Fatalf("result stayed queued after other task stopped: count=%d err=%v", count, err)
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
			var state string
			if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_batches`).Scan(&state); err != nil || state != "published" {
				t.Fatalf("result waited for %s task: state=%s err=%v", status, state, err)
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
	// polling worker runs; it must publish separately rather than extend it.
	f.complete(t, &second, "later answer")
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
		count, err := f.store.PublishReadyConversationResults(ctx, 100)
		done <- outcome{count, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("result publisher bypassed revision transaction: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.count != 0 {
			t.Fatalf("obsolete result was published: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("result publisher did not resume after revision commit")
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_sources WHERE task_id=$1`, first.ID).Scan(&state); err != nil || state != "superseded" {
		t.Fatalf("revision race result state = %q err=%v", state, err)
	}
}

func TestConversationResultAggregationDoesNotTruncateLargeResults(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	first := f.createTask(t, f.ownerID, "first long result")
	second := f.createTask(t, f.ownerID, "second long result")
	one := strings.Repeat("a", 20_000)
	two := strings.Repeat("b", 20_000)
	f.complete(t, &first, one)
	f.complete(t, &second, two)
	var messages, byteCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(content)),0)
FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'`, f.conversation.ID).Scan(&messages, &byteCount); err != nil ||
		messages != 2 || byteCount != len(one)+len(two) {
		t.Fatalf("large results were truncated or lost: messages=%d bytes=%d err=%v", messages, byteCount, err)
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
	var ownerMessage, memberMessage string
	if err := store.pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches WHERE actor_id=$1 AND state='published'`, owner.ID).Scan(&ownerMessage); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches WHERE actor_id=$1 AND state='published'`, member.ID).Scan(&memberMessage); err != nil {
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
	if count, err := store.PublishReadyConversationResults(ctx, 100); err != nil || count != 0 {
		t.Fatalf("removed project member result published: count=%d err=%v", count, err)
	}
	var state, messageID string
	var published int
	if err := store.pool.QueryRow(ctx, `SELECT b.state,b.message_id,(SELECT count(*) FROM conversation_messages WHERE id=b.message_id)
FROM conversation_result_batches b WHERE b.actor_id=$1`, member.ID).Scan(&state, &messageID, &published); err != nil || state != "pending" || published != 0 {
		t.Fatalf("removed member result was not retained safely: state=%s published=%d err=%v", state, published, err)
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
	hash := sha256.Sum256([]byte("route-context-" + actorID))
	_, _, err = f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: actorID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "follow-up"},
		ServiceInstanceID: instanceID, ClientID: "result-context-" + actorID, RequestHash: hash[:], Input: input,
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
