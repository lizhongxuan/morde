package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type routeActionsTestFixture struct {
	store         *Store
	ownerID       string
	conversation  conversation.Conversation
	instanceID    string
	authorization task.Authorization
}

func newRouteActionsTestFixture(t *testing.T) routeActionsTestFixture {
	t.Helper()
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2000, 4096)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'route test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	return routeActionsTestFixture{store: store, ownerID: owner.ID, instanceID: instanceID,
		conversation:  createMainConversationForTest(t, store, owner.ID),
		authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}}
}

func (f routeActionsTestFixture) receive(t *testing.T, clientID, content string, input app.ConversationRoutingInput) app.ConversationRouteClaim {
	t.Helper()
	ctx := context.Background()
	input.ModelID = "p1234567"
	input.Budget = task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}
	hash := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: hash[:], Input: input, Authorization: f.authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "route-actions-test", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim message = %s found=%t err=%v", claim.Message.ID, found, err)
	}
	return claim
}

func TestConversationRouteCreatesOneHostlessTaskAndRecordsRevisionIntent(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "route-actions-001", "Answer my question", app.ConversationRoutingInput{
		HostID: "h1234567", WorkspaceID: "w1234567"})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || len(snapshot.Context.Candidates) != 0 {
		t.Fatalf("initial snapshot = %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	const callers = 2
	results := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew})
		}()
	}
	group.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if domain.CodeOf(err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("concurrent routing failed: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("concurrent routing produced success=%d conflicts=%d", success, conflicts)
	}
	var taskID, hostID, workspaceID, delivery, state string
	if err := f.store.pool.QueryRow(ctx, `SELECT t.id,COALESCE(t.host_id,''),COALESCE(t.workspace_id,''),m.delivery_status,r.state
FROM conversation_messages m JOIN tasks t ON t.id=m.task_id JOIN conversation_message_routes r ON r.message_id=m.id WHERE m.id=$1`, claim.Message.ID).
		Scan(&taskID, &hostID, &workspaceID, &delivery, &state); err != nil {
		t.Fatal(err)
	}
	if hostID != "" || workspaceID != "" || delivery != "applied" || state != "routed" {
		t.Fatalf("route prematurely bound a tool or repeated initial text: host=%q workspace=%q delivery=%s state=%s", hostID, workspaceID, delivery, state)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "route-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.ConversationContext == nil || len(claimed.ConversationContext.Messages) != 0 {
		t.Fatalf("claim routed Task: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	second := f.receive(t, "route-actions-002", "Only look at revenue", app.ConversationRoutingInput{ReferenceTaskID: taskID})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, second)
	if err != nil || len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != taskID {
		t.Fatalf("revision candidate snapshot = %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	err = f.store.ApplyConversationRoute(ctx, second, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	var taskStatus, routeStatus string
	var intentCount int
	var currentRevision, desiredRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,r.state,t.current_revision,t.desired_revision,(SELECT count(*) FROM task_revision_intents i WHERE i.message_id=r.message_id AND i.state='stopping')
FROM tasks t JOIN conversation_message_routes r ON r.message_id=$2 WHERE t.id=$1`, taskID, second.Message.ID).
		Scan(&taskStatus, &routeStatus, &currentRevision, &desiredRevision, &intentCount); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(task.StatusPausing) || routeStatus != "waiting_adjustment" || currentRevision != 0 || desiredRevision != 1 || intentCount != 1 {
		t.Fatalf("revision did not fence the old execution: task=%s route=%s current=%d desired=%d intents=%d", taskStatus, routeStatus, currentRevision, desiredRevision, intentCount)
	}
	var associated *string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, second.Message.ID).Scan(&associated); err != nil || associated != nil {
		t.Fatalf("revision was delivered to the old Agent inbox: task=%v err=%v", associated, err)
	}
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.StatusRunning, Event: task.Event{Type: "task.step"}}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old task lease committed progress after modification: %v", err)
	}
	third := f.receive(t, "route-actions-003", "Also change the date", app.ConversationRoutingInput{ReferenceTaskID: taskID, PreviousClientMessageID: "route-actions-002"})
	latest, err := f.store.LoadConversationRouteSnapshot(ctx, third)
	if err != nil || len(latest.Context.Candidates) != 1 || latest.Context.Candidates[0].Revision != 1 {
		t.Fatalf("second revision snapshot = %+v err=%v", latest, err)
	}
	latest.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, third, latest, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: taskID, ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	var stops, intents int
	if err := f.store.pool.QueryRow(ctx, `SELECT desired_revision,
(SELECT count(*) FROM task_stop_requests WHERE task_id=$1 AND status='pending'),
(SELECT count(*) FROM task_revision_intents WHERE task_id=$1 AND state='stopping') FROM tasks WHERE id=$1`, taskID).
		Scan(&desiredRevision, &stops, &intents); err != nil || desiredRevision != 2 || stops != 1 || intents != 2 {
		t.Fatalf("consecutive changes did not share one safe stop: desired=%d stops=%d intents=%d err=%v", desiredRevision, stops, intents, err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		t.Fatalf("revision ran before the stop was confirmed: applied=%t err=%v", applied, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.store.ClaimNextTaskStop(ctx, "revision-stop-worker", time.Minute)
	if err != nil || !found || stop.Task.ID != taskID {
		t.Fatalf("claim revision stop: found=%t err=%v", found, err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,settled_at)
VALUES ($1,'task','revision-unknown-call',decode(repeat('01',32),'hex'),'p1234567','test',$2,10,'unknown',clock_timestamp())`, f.ownerID, taskID); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		t.Fatalf("revision ignored unknown model usage: applied=%t err=%v", applied, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=1,output_tokens=1,settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='task' AND call_id='revision-unknown-call'`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_revision_intents SET available_after=clock_timestamp() WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		t.Fatalf("revision ignored revoked credentials: applied=%t err=%v", applied, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_revision_intents SET available_after=clock_timestamp() WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("confirmed revision was not queued: applied=%t err=%v", applied, err)
	}
	var pending, waiting int
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,t.current_revision,t.desired_revision,
 (SELECT count(*) FROM conversation_messages m WHERE m.task_id=t.id AND m.id=ANY($2::text[]) AND m.delivery_status='pending'),
 (SELECT count(*) FROM conversation_message_routes r WHERE r.message_id=ANY($2::text[]) AND r.state='waiting_adjustment')
FROM tasks t WHERE t.id=$1`, taskID, []string{second.Message.ID, third.Message.ID}).
		Scan(&taskStatus, &currentRevision, &desiredRevision, &pending, &waiting); err != nil ||
		taskStatus != string(task.StatusQueued) || currentRevision != 2 || desiredRevision != 2 || pending != 2 || waiting != 2 {
		t.Fatalf("revision queued or acknowledged incorrectly: status=%s current=%d desired=%d pending=%d waiting=%d err=%v", taskStatus, currentRevision, desiredRevision, pending, waiting, err)
	}
}

func TestConversationRouteDirectReplyIsAtomicAndIdempotent(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "route-direct-reply-001", "What is 6 times 7?", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	answer := "42."
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: answer,
		ModelOutput: `{"action":"reply","task_id":"","question":"","reply":"42."}`}

	invalid := []app.ConversationRouteDecision{
		{Action: app.ConversationRouteReply},
		{Action: app.ConversationRouteReply, Reply: answer, TaskID: "q1234567"},
		{Action: app.ConversationRouteReply, Reply: answer, Question: "unexpected"},
		{Action: app.ConversationRouteReply, Reply: strings.Repeat("x", app.MaxConversationRouteReplyRunes+1)},
		{Action: app.ConversationRouteReply, Reply: string([]byte{0xff})},
		{Action: app.ConversationRouteReply, Reply: answer, ModelOutput: string([]byte{0xff})},
	}
	for _, candidate := range invalid {
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, candidate); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid direct reply was accepted: %+v err=%v", candidate, err)
		}
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replayed direct reply did not hit the completed route fence: %v", err)
	}
	var routeState, delivery, taskID string
	var assistantCount, messageCount int
	var storedAnswer string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,m.delivery_status,COALESCE(m.task_id,''),
(SELECT count(*) FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation'),
(SELECT count(*) FROM conversation_messages all_messages WHERE all_messages.conversation_id=m.conversation_id),
(SELECT a.content FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation' LIMIT 1)
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).
		Scan(&routeState, &delivery, &taskID, &assistantCount, &messageCount, &storedAnswer); err != nil {
		t.Fatal(err)
	}
	if routeState != "routed" || delivery != "applied" || taskID != "" || assistantCount != 1 || messageCount != 2 || storedAnswer != answer {
		t.Fatalf("direct reply was not committed exactly once: route=%s delivery=%s task=%s assistants=%d messages=%d answer=%q",
			routeState, delivery, taskID, assistantCount, messageCount, storedAnswer)
	}
}

func TestConversationRouteNeverAppliesRevisionsOutOfMessageOrder(t *testing.T) {
	for _, pendingPredecessor := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_predecessor=%t", pendingPredecessor), func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			initial := f.receive(t, "revision-order-001", "Original request", app.ConversationRoutingInput{})
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var taskID string
			if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
				t.Fatal(err)
			}
			earlier := f.receive(t, "revision-order-002", "Earlier update", app.ConversationRoutingInput{ReferenceTaskID: taskID})
			input := app.ConversationRoutingInput{ReferenceTaskID: taskID}
			if pendingPredecessor {
				input.PreviousClientMessageID = "revision-order-002"
			}
			later := f.receive(t, "revision-order-003", "Later update", input)
			route := func(claim app.ConversationRouteClaim) {
				t.Helper()
				snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
				if err != nil || len(snapshot.Context.Candidates) != 1 {
					t.Fatalf("route snapshot=%+v err=%v", snapshot, err)
				}
				snapshot.Context.ModelID = "p1234567"
				if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
					TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
					t.Fatal(err)
				}
			}
			route(later)
			if !pendingPredecessor {
				route(earlier)
			}
			var laterState, earlierState string
			var intents int
			if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT state FROM conversation_message_routes WHERE message_id=$1),
 (SELECT state FROM conversation_message_routes WHERE message_id=$2),(SELECT count(*) FROM task_revision_intents WHERE task_id=$3)`,
				later.Message.ID, earlier.Message.ID, taskID).Scan(&laterState, &earlierState, &intents); err != nil {
				t.Fatal(err)
			}
			if pendingPredecessor {
				if laterState != "waiting_user" || earlierState != "processing" || intents != 0 {
					t.Fatalf("unresolved predecessor changed task: earlier=%s later=%s intents=%d", earlierState, laterState, intents)
				}
			} else if laterState != "waiting_adjustment" || earlierState != "waiting_user" || intents != 1 {
				t.Fatalf("late earlier message changed revision order: earlier=%s later=%s intents=%d", earlierState, laterState, intents)
			}
		})
	}
}

func TestConversationRouteStaleDecisionCannotStopTask(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	first := f.receive(t, "route-stale-001", "Do work", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, first, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, first.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	second := f.receive(t, "route-stale-002", "Stop it", app.ConversationRoutingInput{ReferenceTaskID: taskID})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, second)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("snapshot = %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=desired_revision+1 WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApplyConversationRoute(ctx, second, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteStop, TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision,
	}); err != nil {
		t.Fatal(err)
	}
	var taskStatus, routeStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,r.state FROM tasks t JOIN conversation_message_routes r ON r.message_id=$2 WHERE t.id=$1`,
		taskID, second.Message.ID).Scan(&taskStatus, &routeStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(task.StatusQueued) || routeStatus != "waiting_user" {
		t.Fatalf("stale decision changed task: task=%s route=%s", taskStatus, routeStatus)
	}
}

func TestConversationRouteStopIsAuthorizedAndLeavesSiblingAlone(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	create := func(clientID, content string) string {
		t.Helper()
		claim := f.receive(t, clientID, content, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatal(err)
		}
		var taskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}
	first := create("route-stop-001", "Work on A")
	second := create("route-stop-002", "Work on B")
	claim := f.receive(t, "route-stop-003", "Stop A", app.ConversationRoutingInput{ReferenceTaskID: first})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || len(snapshot.Context.Candidates) != 2 {
		t.Fatalf("task candidates = %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: first}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked route credential could change a Task: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var firstStatus, secondStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT a.status,b.status FROM tasks a JOIN tasks b ON b.id=$2 WHERE a.id=$1`, first, second).
		Scan(&firstStatus, &secondStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != string(task.StatusCancelling) || secondStatus != string(task.StatusQueued) {
		t.Fatalf("stop affected the wrong Task: first=%s second=%s", firstStatus, secondStatus)
	}
}

func TestConversationRouteRetryReusesMessageAndFencesIdempotency(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "route-retry-001", "Keep this message", app.ConversationRoutingInput{})
	if err := f.store.FailConversationRoute(ctx, claim, "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	assertProjection := func(status conversation.RoutingStatus, reason conversation.RoutingFailureReason) {
		t.Helper()
		page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 10})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != claim.Message.ID || page.Items[0].RoutingStatus != status || page.Items[0].RoutingFailureReason != reason {
			t.Fatalf("message list projection = %+v, err=%v", page, err)
		}
		message, found, err := f.store.FindConversationMessageByClientID(ctx, f.ownerID, "", f.conversation.ID, "route-retry-001", f.instanceID)
		if err != nil || !found || message.ID != claim.Message.ID || message.RoutingStatus != status || message.RoutingFailureReason != reason {
			t.Fatalf("message lookup projection = %+v, found=%t err=%v", message, found, err)
		}
	}
	assertProjection(conversation.RoutingFailed, "model_unavailable")
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	retry := func(key string) error {
		return f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, claim.Message.ID, f.instanceID, key)
	}
	if err := retry("route-retry-key-001"); err != nil {
		t.Fatal(err)
	}
	assertProjection(conversation.RoutingPending, "")
	if err := retry("route-retry-key-001"); err != nil {
		t.Fatalf("identical retry did not replay: %v", err)
	}
	next, found, err := f.store.ClaimConversationRoute(ctx, "route-retry-worker", time.Minute)
	if err != nil || !found || next.Message.ID != claim.Message.ID || next.LeaseVersion != claim.LeaseVersion+1 {
		t.Fatalf("retried route = %+v found=%t err=%v", next, found, err)
	}
	if err := f.store.FailConversationRoute(ctx, next, "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := retry("route-retry-key-001"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, claim.Message.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("replayed key restarted a later failure: state=%q err=%v", state, err)
	}
	if err := retry("route-retry-key-002"); err != nil {
		t.Fatalf("new user retry failed: %v", err)
	}
	other := createPreRetirementTestUser(t, f.store, 1, 2000, 4096)
	principal.UserID, principal.TokenID = other.ID, testSessionTokenID(other.ID)
	if err := retry("route-retry-key-003"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account retried this message: %v", err)
	}
	var count int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`, f.conversation.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry duplicated message: count=%d err=%v", count, err)
	}
}

func TestConversationRouteOversizedMessageIsKeptWithVisibleReason(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	content := strings.Repeat("a", app.MaxConversationAgentInputBytes+1)
	claim := f.receive(t, "route-too-long-001", content, app.ConversationRoutingInput{})
	if err := f.store.FailConversationRoute(ctx, claim, "message_too_long"); err != nil {
		t.Fatal(err)
	}
	message, found, err := f.store.FindConversationMessageByClientID(ctx, f.ownerID, "", f.conversation.ID, "route-too-long-001", f.instanceID)
	if err != nil || !found || message.Content != content || message.RoutingStatus != conversation.RoutingFailed ||
		message.RoutingFailureReason != "message_too_long" || message.TaskID != "" {
		t.Fatalf("oversized message was not retained with a visible reason: found=%t bytes=%d state=%s reason=%s task=%s err=%v",
			found, len(message.Content), message.RoutingStatus, message.RoutingFailureReason, message.TaskID, err)
	}
}

func TestConversationTaskRevisionFenceBlocksProgressAndStreaming(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	message := f.receive(t, "revision-fence-001", "Original request", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, message, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "revision-fence-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim task: found=%t err=%v", found, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AppendAssistantTextDelta(ctx, app.AssistantTextDelta{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, MessageID: mustID(t, domain.NewMessageID), TurnID: mustID(t, domain.NewStepID), Delta: "old text"}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("streaming accepted an unapplied revision: %v", err)
	}
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.StatusRunning, Event: task.Event{Type: "task.step"}}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("progress accepted an unapplied revision: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ClaimNextTask(ctx, "revision-fence-worker-2", time.Minute); err != nil || found {
		t.Fatalf("unapplied revision was claimed: found=%t err=%v", found, err)
	}
}
