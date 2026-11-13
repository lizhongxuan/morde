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
	return f.receiveInConversation(t, f.conversation.ID, clientID, content, input)
}

func (f routeActionsTestFixture) receiveInConversation(t *testing.T, conversationID, clientID, content string,
	input app.ConversationRoutingInput) app.ConversationRouteClaim {
	t.Helper()
	ctx := context.Background()
	input.ModelID = "p1234567"
	input.Budget = task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}
	hash := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: content},
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

func TestConversationRouteJoinWaitsOnOriginalRequestWithoutCreatingTask(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	input := app.ConversationRoutingInput{HostID: "h1234567"}
	original := f.receive(t, "route-join-original", "分析 Morde 项目", input)
	originalSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := f.receive(t, "route-join-duplicate", "分析一下 Morde 项目", input)
	duplicateSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, duplicate)
	if err != nil || len(duplicateSnapshot.Context.RequestCandidates) != 1 ||
		duplicateSnapshot.Context.RequestCandidates[0].MessageID != original.Message.ID ||
		!duplicateSnapshot.Context.RequestCandidates[0].SameExecutionScope {
		t.Fatalf("duplicate routing candidates = %+v err=%v", duplicateSnapshot.Context.RequestCandidates, err)
	}
	if err := f.store.ApplyConversationRoute(ctx, duplicate, duplicateSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: original.Message.ID,
	}); err != nil {
		t.Fatal(err)
	}
	var association, duplicateTaskID, duplicateDelivery, duplicateState, originalState string
	var taskCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(duplicate.duplicate_of_message_id,''),COALESCE(message.task_id,''),
message.delivery_status,duplicate.state,original.state,(SELECT count(*) FROM tasks WHERE conversation_id=$1)
FROM conversation_message_routes duplicate
JOIN conversation_messages message ON message.id=duplicate.message_id
JOIN conversation_message_routes original ON original.message_id=$3
WHERE duplicate.message_id=$2`, f.conversation.ID, duplicate.Message.ID, original.Message.ID).
		Scan(&association, &duplicateTaskID, &duplicateDelivery, &duplicateState, &originalState, &taskCount); err != nil {
		t.Fatal(err)
	}
	if association != original.Message.ID || duplicateTaskID != "" || duplicateDelivery != string(conversation.DeliveryPending) ||
		duplicateState != "routed" || originalState != "processing" || taskCount != 0 {
		t.Fatalf("duplicate was not durably linked while original was processing: association=%q task=%q delivery=%s duplicate=%s original=%s tasks=%d",
			association, duplicateTaskID, duplicateDelivery, duplicateState, originalState, taskCount)
	}
	originalSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, original, originalSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("stale root route was not safely requeued after a join: %v", err)
	}
	resumed, found, err := f.store.ClaimConversationRoute(ctx, "route-join-root-worker", time.Minute)
	if err != nil || !found || resumed.Message.ID != original.Message.ID || resumed.RouteAttempt != original.RouteAttempt+1 {
		t.Fatalf("claim root after joined input changed its context: claim=%+v found=%t err=%v", resumed, found, err)
	}
	resumedSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, resumed)
	if err != nil || !strings.Contains(resumedSnapshot.Context.Content, duplicate.Message.Content) {
		t.Fatalf("reclassified root omitted joined input: content=%q err=%v", resumedSnapshot.Context.Content, err)
	}
	resumedSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, resumed, resumedSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("apply root after joined input refresh: %v", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("joined message created extra task: tasks=%d err=%v", taskCount, err)
	}
	var rootTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, original.Message.ID).Scan(&rootTaskID); err != nil || rootTaskID == "" {
		t.Fatalf("original request task was not created: task=%q err=%v", rootTaskID, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(task_id,'') FROM conversation_messages WHERE id=$1`, duplicate.Message.ID).Scan(&duplicateTaskID); err != nil || duplicateTaskID != "" {
		t.Fatalf("duplicate message entered the task inbox: task=%q err=%v", duplicateTaskID, err)
	}
	page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var duplicateProjection *conversation.Message
	for index := range page.Items {
		if page.Items[index].ID == duplicate.Message.ID {
			duplicateProjection = &page.Items[index]
		}
	}
	if duplicateProjection == nil || duplicateProjection.DuplicateOfMessageID != original.Message.ID ||
		duplicateProjection.RelatedTaskID != rootTaskID || duplicateProjection.TaskID != "" ||
		duplicateProjection.RoutingStatus != conversation.RoutingRouted || duplicateProjection.DeliveryStatus != conversation.DeliveryApplied {
		t.Fatalf("duplicate projection did not mirror the original task: %+v", duplicateProjection)
	}
	reconciled, found, err := f.store.FindConversationMessageByClientID(ctx, f.ownerID, "", f.conversation.ID,
		"route-join-duplicate", f.instanceID)
	if err != nil || !found || reconciled.DuplicateOfMessageID != original.Message.ID || reconciled.RelatedTaskID != rootTaskID ||
		reconciled.TaskID != "" || reconciled.RoutingStatus != conversation.RoutingRouted {
		t.Fatalf("client-id reconciliation lost duplicate association: %+v found=%t err=%v", reconciled, found, err)
	}
}

func TestConversationRouteJoinAppendsRequirementsToActiveTaskRevision(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	input := app.ConversationRoutingInput{HostID: "h1234567"}
	root := f.receive(t, "route-join-revision-root", "整理项目资料并生成设计方案", input)
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, root, rootSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, root.Message.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("create root Task: task=%q err=%v", taskID, err)
	}

	addition := "方案中补充风险、验收标准，并提供可下载的 Markdown 文档。"
	joined := f.receive(t, "route-join-revision-addition", addition, input)
	joinedSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, joined)
	if err != nil || len(joinedSnapshot.Context.RequestCandidates) != 1 ||
		joinedSnapshot.Context.RequestCandidates[0].MessageID != root.Message.ID ||
		joinedSnapshot.Context.RequestCandidates[0].TaskID != taskID ||
		joinedSnapshot.Context.RequestCandidates[0].TaskStatus != task.StatusQueued {
		t.Fatalf("active Task was not offered as a join target: candidates=%+v err=%v", joinedSnapshot.Context.RequestCandidates, err)
	}
	if err := f.store.ApplyConversationRoute(ctx, joined, joinedSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatalf("join active task with new requirements: %v", err)
	}
	var routeState, delivery, association, messageTaskID, taskStatus string
	var taskCount, intentCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT route.state,message.delivery_status,COALESCE(route.duplicate_of_message_id,''),
COALESCE(message.task_id,''),task.status,(SELECT count(*) FROM tasks WHERE conversation_id=$1),
(SELECT count(*) FROM task_revision_intents WHERE message_id=message.id AND state='stopping')
FROM conversation_message_routes route
JOIN conversation_messages message ON message.id=route.message_id
JOIN tasks task ON task.id=$3 AND task.conversation_id=message.conversation_id
WHERE route.message_id=$2`, f.conversation.ID, joined.Message.ID, taskID).
		Scan(&routeState, &delivery, &association, &messageTaskID, &taskStatus, &taskCount, &intentCount); err != nil {
		t.Fatal(err)
	}
	if routeState != "waiting_adjustment" || delivery != "pending" || association != root.Message.ID || messageTaskID != "" ||
		taskStatus != string(task.StatusPausing) || taskCount != 1 || intentCount != 1 {
		t.Fatalf("join did not create one safe Task revision: route=%q delivery=%q association=%q message_task=%q task_status=%q tasks=%d intents=%d",
			routeState, delivery, association, messageTaskID, taskStatus, taskCount, intentCount)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.store.ClaimNextTaskStop(ctx, "route-join-revision-stop-worker", time.Minute)
	if err != nil || !found || stop.Task.ID != taskID {
		t.Fatalf("claim revision stop: found=%t err=%v", found, err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply joined Task revision: applied=%t err=%v", applied, err)
	}
	var actualTaskID, intentState, actualContent string
	var currentRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),intent.state,message.content,task.current_revision
FROM conversation_messages message JOIN task_revision_intents intent ON intent.message_id=message.id
JOIN tasks task ON task.id=intent.task_id WHERE message.id=$1`, joined.Message.ID).
		Scan(&actualTaskID, &intentState, &actualContent, &currentRevision); err != nil || actualTaskID != taskID || intentState != "applied" ||
		actualContent != addition || currentRevision != 1 {
		t.Fatalf("joined input was lost from the same Task revision: task=%q intent=%q content=%q revision=%d err=%v",
			actualTaskID, intentState, actualContent, currentRevision, err)
	}
}

func TestConcurrentConversationRouteJoinsReclassifyAndPreserveBothRevisions(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	input := app.ConversationRoutingInput{HostID: "h1234567"}
	root := f.receive(t, "route-join-race-root", "制定项目实施方案", input)
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, root, rootSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, root.Message.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("create root Task: task=%q err=%v", taskID, err)
	}
	additions := []app.ConversationRouteClaim{
		f.receive(t, "route-join-race-addition-1", "补充数据安全要求。", input),
		f.receive(t, "route-join-race-addition-2", "再补充验收标准。", input),
	}
	snapshots := make([]app.ConversationRouteSnapshot, len(additions))
	for index, claim := range additions {
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		rootJoinable := false
		for _, candidate := range snapshot.Context.RequestCandidates {
			rootJoinable = rootJoinable || candidate.MessageID == root.Message.ID && candidate.TaskID == taskID && candidate.TaskStatus == task.StatusQueued
		}
		if err != nil || !rootJoinable {
			t.Fatalf("join snapshot %d=%+v err=%v", index, snapshot.Context.RequestCandidates, err)
		}
		snapshots[index] = snapshot
	}
	start := make(chan struct{})
	results := make(chan error, len(additions))
	var group sync.WaitGroup
	for index, claim := range additions {
		group.Add(1)
		go func(index int, claim app.ConversationRouteClaim) {
			defer group.Done()
			<-start
			results <- f.store.ApplyConversationRoute(ctx, claim, snapshots[index], app.ConversationRouteDecision{
				Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
			})
		}(index, claim)
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent join was not committed or safely requeued: %v", err)
		}
	}
	var pendingID string
	var revisions, pending int
	if err := f.store.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM task_revision_intents WHERE task_id=$1 AND state='stopping'),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$2 AND state='pending'),
COALESCE((SELECT message_id FROM conversation_message_routes WHERE conversation_id=$2 AND state='pending' ORDER BY message_id LIMIT 1),'')`,
		taskID, f.conversation.ID).Scan(&revisions, &pending, &pendingID); err != nil || revisions != 1 || pending != 1 || pendingID == "" {
		t.Fatalf("concurrent joins did not serialize to one revision plus one reclassification: revisions=%d pending=%d message=%q err=%v",
			revisions, pending, pendingID, err)
	}
	secondPass, found, err := f.store.ClaimConversationRoute(ctx, "route-join-race-worker", time.Minute)
	if err != nil || !found || secondPass.Message.ID != pendingID {
		t.Fatalf("claim the stale join: claim=%+v found=%t err=%v", secondPass, found, err)
	}
	secondSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, secondPass)
	rootJoinable := false
	for _, candidate := range secondSnapshot.Context.RequestCandidates {
		rootJoinable = rootJoinable || candidate.MessageID == root.Message.ID && candidate.TaskID == taskID && candidate.TaskStatus == task.StatusPausing
	}
	if err != nil || !rootJoinable {
		t.Fatalf("reclassified join did not see the active revision: candidates=%+v err=%v", secondSnapshot.Context.RequestCandidates, err)
	}
	if err := f.store.ApplyConversationRoute(ctx, secondPass, secondSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatalf("append second revision to the same Task: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.store.ClaimNextTaskStop(ctx, "route-join-race-stop-worker", time.Minute)
	if err != nil || !found || stop.Task.ID != taskID {
		t.Fatalf("claim shared revision stop: found=%t err=%v", found, err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply both joined revisions: applied=%t err=%v", applied, err)
	}
	var taskCount, appliedMessages, appliedIntents int
	var current, desired int64
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*),max(task.current_revision),max(task.desired_revision),
(SELECT count(*) FROM conversation_messages WHERE id=ANY($2::text[]) AND task_id=$1 AND delivery_status='pending'),
(SELECT count(*) FROM task_revision_intents WHERE task_id=$1 AND message_id=ANY($2::text[]) AND state='applied')
FROM tasks task WHERE task.id=$1`, taskID, []string{additions[0].Message.ID, additions[1].Message.ID}).
		Scan(&taskCount, &current, &desired, &appliedMessages, &appliedIntents); err != nil || taskCount != 1 || current != 2 || desired != 2 ||
		appliedMessages != 2 || appliedIntents != 2 {
		t.Fatalf("concurrent additions were not preserved in one Task: tasks=%d revision=%d/%d messages=%d intents=%d err=%v",
			taskCount, current, desired, appliedMessages, appliedIntents, err)
	}
}

func TestConversationRouteConcurrentNewDecisionReclassifiesAgainstCreatedTask(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	input := app.ConversationRoutingInput{HostID: "h1234567"}
	original := f.receive(t, "route-race-original", "分析 Morde 项目", input)
	originalSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	duplicates := []app.ConversationRouteClaim{
		f.receive(t, "route-race-duplicate-1", "分析 Morde 项目", input),
		f.receive(t, "route-race-duplicate-2", "分析一下 Morde 项目", input),
		f.receive(t, "route-race-duplicate-3", "也帮我分析这个 Morde 项目", input),
	}
	duplicateSnapshots := make([]app.ConversationRouteSnapshot, len(duplicates))
	for index, duplicate := range duplicates {
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, duplicate)
		containsOriginal := false
		for _, candidate := range snapshot.Context.RequestCandidates {
			containsOriginal = containsOriginal || candidate.MessageID == original.Message.ID
		}
		if err != nil || !containsOriginal {
			t.Fatalf("concurrent route snapshot %d=%+v err=%v", index, snapshot, err)
		}
		snapshot.Context.ModelID = "p1234567"
		duplicateSnapshots[index] = snapshot
	}
	originalSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, original, originalSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	for index, duplicate := range duplicates {
		if err := f.store.ApplyConversationRoute(ctx, duplicate, duplicateSnapshots[index],
			app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatalf("stale concurrent decision %d failed: %v", index, err)
		}
	}
	for index, duplicate := range duplicates {
		if index == 0 {
			var duplicateOf, duplicateTaskID, state string
			if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(route.duplicate_of_message_id,''),COALESCE(message.task_id,''),route.state
FROM conversation_message_routes route JOIN conversation_messages message ON message.id=route.message_id
WHERE route.message_id=$1`, duplicate.Message.ID).Scan(&duplicateOf, &duplicateTaskID, &state); err != nil ||
				duplicateOf != original.Message.ID || duplicateTaskID != "" || state != "routed" {
				t.Fatalf("stale exact duplicate was not linked to the original request: duplicate_of=%q task=%q state=%q err=%v",
					duplicateOf, duplicateTaskID, state, err)
			}
			continue
		}
		current, found, err := f.store.ClaimConversationRoute(ctx, "route-actions-test", time.Minute)
		if err != nil || !found || current.Message.ID != duplicate.Message.ID {
			t.Fatalf("reclassified duplicate claim %d=%+v found=%t err=%v", index, current, found, err)
		}
		var attempt int64
		if err := f.store.pool.QueryRow(ctx, `SELECT route_attempt FROM conversation_message_routes WHERE message_id=$1`, duplicate.Message.ID).Scan(&attempt); err != nil || attempt != duplicate.RouteAttempt+1 || current.RouteAttempt != attempt {
			t.Fatalf("duplicate %d route attempt=%d initial=%d claimed=%d err=%v", index, attempt, duplicate.RouteAttempt, current.RouteAttempt, err)
		}
		currentSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, current)
		containsOriginalTask := false
		for _, candidate := range currentSnapshot.Context.RequestCandidates {
			containsOriginalTask = containsOriginalTask || candidate.MessageID == original.Message.ID && candidate.TaskID != ""
		}
		if err != nil || !containsOriginalTask {
			t.Fatalf("refreshed duplicate snapshot %d=%+v err=%v", index, currentSnapshot, err)
		}
		if err := f.store.ApplyConversationRoute(ctx, current, currentSnapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteJoin, TargetMessageID: original.Message.ID,
		}); err != nil {
			t.Fatalf("join duplicate %d to the original request: %v", index, err)
		}
	}
	var taskCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("four concurrent requests created %d tasks, err=%v", taskCount, err)
	}
	for index, duplicate := range duplicates {
		var duplicateOf, duplicateTaskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(route.duplicate_of_message_id,''),COALESCE(message.task_id,'')
FROM conversation_message_routes route JOIN conversation_messages message ON message.id=route.message_id
WHERE route.message_id=$1`, duplicate.Message.ID).Scan(&duplicateOf, &duplicateTaskID); err != nil ||
			duplicateOf != original.Message.ID || duplicateTaskID != "" {
			t.Fatalf("duplicate %d was not linked to the single root: duplicate_of=%q duplicate_task=%q err=%v",
				index, duplicateOf, duplicateTaskID, err)
		}
	}
}

func TestConversationRouteJoinRejectsDifferentExecutionScope(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	original := f.receive(t, "route-join-scope-original", "分析 Morde 项目", app.ConversationRoutingInput{HostID: "h1234567"})
	duplicate := f.receive(t, "route-join-scope-other", "分析 Morde 项目", app.ConversationRoutingInput{HostID: "h7654321"})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Context.RequestCandidates) != 1 || snapshot.Context.RequestCandidates[0].SameExecutionScope {
		t.Fatalf("different execution scope was not represented: %+v", snapshot.Context.RequestCandidates)
	}
	if err := f.store.ApplyConversationRoute(ctx, duplicate, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: original.Message.ID,
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("cross-scope join was accepted: %v", err)
	}
	var state, association string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,COALESCE(duplicate_of_message_id,'') FROM conversation_message_routes WHERE message_id=$1`,
		duplicate.Message.ID).Scan(&state, &association); err != nil || state != "processing" || association != "" {
		t.Fatalf("rejected join changed the message route: state=%s association=%q err=%v", state, association, err)
	}
}

func TestConversationRouteDefaultComputerDoesNotChangeRevisionTargetContext(t *testing.T) {
	for _, test := range []struct {
		name             string
		changeTaskTarget bool
	}{
		{name: "default computer"},
		{name: "explicit task target", changeTaskTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			initial := f.receive(t, "target-intent-initial-001", "Answer my question", app.ConversationRoutingInput{HostID: "h1234567"})
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var taskID, initialIntent string
			if err := f.store.pool.QueryRow(ctx, `SELECT id,file_delivery_intent FROM tasks WHERE conversation_id=$1`, f.conversation.ID).
				Scan(&taskID, &initialIntent); err != nil || initialIntent != "none" {
				t.Fatalf("initial task target intent=%q task=%q err=%v", initialIntent, taskID, err)
			}

			revision := f.receive(t, "target-intent-revision-001", "修复项目代码", app.ConversationRoutingInput{
				ReferenceTaskID: taskID, HostID: "h7654321", WorkspaceID: "w7654321", ChangeTaskTarget: test.changeTaskTarget,
			})
			snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, revision)
			if err != nil || len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != taskID {
				t.Fatalf("revision snapshot=%+v err=%v", snapshot, err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, revision, snapshot, app.ConversationRouteDecision{
				Action: app.ConversationRouteUpdate, TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
				t.Fatal(err)
			}
			stop, found, err := f.store.ClaimNextTaskStop(ctx, "target-intent-stop-worker", time.Minute)
			if err != nil || !found || stop.Task.ID != taskID {
				t.Fatalf("claim target change stop: found=%t err=%v", found, err)
			}
			if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
				t.Fatal(err)
			}
			if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
				t.Fatalf("apply target change revision: applied=%t err=%v", applied, err)
			}
			var actualIntent string
			if err := f.store.pool.QueryRow(ctx, `SELECT file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&actualIntent); err != nil {
				t.Fatal(err)
			}
			wantFileIntent := initialIntent
			if test.changeTaskTarget {
				wantFileIntent = "required"
			}
			if actualIntent != wantFileIntent {
				t.Fatalf("file delivery intent=%q, want %q", actualIntent, wantFileIntent)
			}
		})
	}
}

func TestConversationRouteDirectReplyIsAtomicAndIdempotent(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "route-direct-reply-001", "讲个笑话", app.ConversationRoutingInput{TimeZone: "Asia/Shanghai"})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Context.TimeZone != "Asia/Shanghai" || !snapshot.Context.ServerReceivedAt.Equal(claim.Message.CreatedAt) {
		t.Fatalf("route snapshot lost receipt time context: %+v claim=%+v", snapshot.Context, claim.Message)
	}
	snapshot.Context.ModelID = "p1234567"
	answer := "程序员最怕什么冷？当然是没有缓存的冷启动。"
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: answer,
		ModelOutput: `{"action":"reply","task_id":"","question":"","reply":"程序员最怕什么冷？当然是没有缓存的冷启动。"}`}

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

func TestConversationRouteStaleDecisionReclassifiesWithoutStoppingTask(t *testing.T) {
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
	var attempt int64
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,r.state,r.route_attempt FROM tasks t JOIN conversation_message_routes r ON r.message_id=$2 WHERE t.id=$1`,
		taskID, second.Message.ID).Scan(&taskStatus, &routeStatus, &attempt); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(task.StatusQueued) || routeStatus != "pending" || attempt != second.RouteAttempt+1 {
		t.Fatalf("stale decision stopped Task or skipped automatic reclassification: task=%s route=%s attempt=%d", taskStatus, routeStatus, attempt)
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
	var routeAttempt int64
	if err := f.store.pool.QueryRow(ctx, `SELECT state,route_attempt FROM conversation_message_routes WHERE message_id=$1`, claim.Message.ID).Scan(&state, &routeAttempt); err != nil || state != "failed" || routeAttempt != next.RouteAttempt {
		t.Fatalf("replayed key restarted a later failure: state=%q attempt=%d first_attempt=%d err=%v", state, routeAttempt, next.RouteAttempt, err)
	}
	if err := retry("route-retry-key-002"); err != nil {
		t.Fatalf("new user retry failed: %v", err)
	}
	var retryAttempt int64
	if err := f.store.pool.QueryRow(ctx, `SELECT route_attempt FROM conversation_message_routes WHERE message_id=$1`, claim.Message.ID).Scan(&retryAttempt); err != nil || retryAttempt != next.RouteAttempt+1 {
		t.Fatalf("fresh retry did not receive a new routing attempt: attempt=%d previous=%d err=%v", retryAttempt, next.RouteAttempt, err)
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

func TestConcurrentConversationRouteRetryWithSameKeyCreatesOneAttempt(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "route-retry-concurrent-001", "Retry this message once", app.ConversationRoutingInput{})
	if err := f.store.FailConversationRoute(ctx, claim, "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: f.authorization.TokenID, Scopes: []identity.Scope{identity.ScopeTaskSubmit}}
	const callers = 8
	ready := sync.WaitGroup{}
	ready.Add(callers)
	var group sync.WaitGroup
	group.Add(callers)
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			defer group.Done()
			ready.Done()
			<-start
			results <- f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, claim.Message.ID,
				f.instanceID, "route-retry-concurrent-key-001")
		}()
	}
	ready.Wait()
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("same-key concurrent retry did not replay successfully: %v", err)
		}
	}
	var state string
	var attempt, messages int64
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.route_attempt,
 (SELECT count(*) FROM conversation_messages WHERE conversation_id=r.conversation_id)
FROM conversation_message_routes r WHERE r.message_id=$1`, claim.Message.ID).
		Scan(&state, &attempt, &messages); err != nil || state != "pending" || attempt != claim.RouteAttempt+1 || messages != 1 {
		t.Fatalf("concurrent retry produced multiple attempts or messages: state=%q attempt=%d messages=%d err=%v",
			state, attempt, messages, err)
	}
	retry, found, err := f.store.ClaimConversationRoute(ctx, "route-retry-concurrent-worker", time.Minute)
	if err != nil || !found || retry.Message.ID != claim.Message.ID || retry.RouteAttempt != attempt {
		t.Fatalf("single retry attempt was not claimable: claim=%+v found=%t err=%v", retry, found, err)
	}
}

func TestConversationRouteRetryFromDuplicateTargetsOriginalRoute(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	input := app.ConversationRoutingInput{HostID: "h1234567"}
	root := f.receive(t, "route-retry-duplicate-root", "分析 Morde 项目", input)
	duplicate := f.receive(t, "route-retry-duplicate-copy", "分析一下 Morde 项目", input)
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, duplicate)
	if err != nil || len(snapshot.Context.RequestCandidates) != 1 {
		t.Fatalf("duplicate retry context=%+v err=%v", snapshot.Context.RequestCandidates, err)
	}
	if err := f.store.ApplyConversationRoute(ctx, duplicate, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.FailConversationRoute(ctx, root, "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Kind: identity.KindSession, TokenID: f.authorization.TokenID,
		Scopes: []identity.Scope{identity.ScopeTaskSubmit}}
	if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, duplicate.Message.ID, f.instanceID,
		"route-retry-duplicate-key-001"); err != nil {
		t.Fatalf("retry through duplicate message: %v", err)
	}
	if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, root.Message.ID, f.instanceID,
		"route-retry-duplicate-key-001"); err != nil {
		t.Fatalf("same normalized retry key did not replay through root: %v", err)
	}
	retried, found, err := f.store.ClaimConversationRoute(ctx, "route-retry-duplicate-worker", time.Minute)
	if err != nil || !found || retried.Message.ID != root.Message.ID || retried.RouteAttempt != root.RouteAttempt+1 {
		t.Fatalf("retry did not target the original request: claim=%+v found=%t err=%v", retried, found, err)
	}
	message, found, err := f.store.FindConversationMessageByClientID(ctx, f.ownerID, "", f.conversation.ID,
		"route-retry-duplicate-copy", f.instanceID)
	if err != nil || !found || message.RoutingStatus != conversation.RoutingProcessing ||
		message.DuplicateOfMessageID != root.Message.ID || message.DeliveryStatus != conversation.DeliveryPending {
		t.Fatalf("duplicate did not mirror original retry: %+v found=%t err=%v", message, found, err)
	}
}

func TestConversationRouteContextChangesReclassifyAndFailAfterThreeConflicts(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	prior := f.receive(t, "route-reclass-prior-001", "先前的任务", app.ConversationRoutingInput{})
	priorSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	priorSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, prior, priorSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var priorTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, prior.Message.ID).Scan(&priorTaskID); err != nil {
		t.Fatal(err)
	}
	claim := f.receive(t, "route-reclass-root-001", "分析这个项目", app.ConversationRoutingInput{})
	for conflict := 1; conflict <= 3; conflict++ {
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=desired_revision+1 WHERE id=$1`, priorTaskID); err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatalf("reclassify conflict %d: %v", conflict, err)
		}
		var state, failure string
		var attempt int64
		var retries int16
		var taskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.failure_reason,r.route_attempt,r.auto_retry_count,COALESCE(m.task_id,'')
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).
			Scan(&state, &failure, &attempt, &retries, &taskID); err != nil {
			t.Fatal(err)
		}
		if taskID != "" || retries != int16(conflict) || (conflict < 3 && (state != "pending" || attempt != claim.RouteAttempt+1)) ||
			(conflict == 3 && (state != "failed" || failure != "routing_context_unavailable" || attempt != claim.RouteAttempt)) {
			t.Fatalf("conflict %d produced state=%s failure=%s attempt=%d retries=%d task=%s", conflict, state, failure, attempt, retries, taskID)
		}
		if conflict < 3 {
			next, found, err := f.store.ClaimConversationRoute(ctx, "route-reclass-test-worker", time.Minute)
			if err != nil || !found || next.Message.ID != claim.Message.ID {
				t.Fatalf("claim reclassified route: claim=%+v found=%t err=%v", next, found, err)
			}
			claim = next
		}
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
