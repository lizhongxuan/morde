package postgres

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

func TestConversationRouteStatusReadsTaskWithoutCreatingOrStopping(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()

	createClaim := f.receive(t, "route-status-create-001", "开始任务 A", app.ConversationRoutingInput{})
	createSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, createClaim)
	if err != nil {
		t.Fatal(err)
	}
	createSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, createClaim, createSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, createClaim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}

	statusClaim := f.receive(t, "route-status-read-001", "任务 A 现在状态如何？", app.ConversationRoutingInput{ReferenceTaskID: taskID})
	statusSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, statusClaim)
	if err != nil || len(statusSnapshot.Context.Candidates) != 1 || statusSnapshot.Context.Candidates[0].TaskID != taskID {
		t.Fatalf("status task candidate = %+v err=%v", statusSnapshot.Context.Candidates, err)
	}
	statusSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, statusClaim, statusSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteStatus, TaskID: taskID, ExpectedRevision: statusSnapshot.Context.Candidates[0].Revision,
	}); err != nil {
		t.Fatal(err)
	}

	var taskStatus, routeStatus, delivery, response, routeTaskID string
	var taskCount, stopCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,r.state,m.delivery_status,COALESCE(m.task_id,''),
	(SELECT count(*) FROM tasks WHERE conversation_id=$2),
	(SELECT count(*) FROM task_stop_requests WHERE task_id=$1),
	(SELECT content FROM conversation_messages WHERE conversation_id=$2 AND role='assistant' ORDER BY sequence DESC LIMIT 1)
FROM tasks t
JOIN conversation_message_routes r ON r.message_id=$3
JOIN conversation_messages m ON m.id=r.message_id
WHERE t.id=$1`, taskID, f.conversation.ID, statusClaim.Message.ID).
		Scan(&taskStatus, &routeStatus, &delivery, &routeTaskID, &taskCount, &stopCount, &response); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(task.StatusQueued) || routeStatus != "routed" || delivery != string(conversation.DeliveryApplied) || routeTaskID != "" {
		t.Fatalf("status action changed task or message projection: task=%s route=%s delivery=%s route_task=%q",
			taskStatus, routeStatus, delivery, routeTaskID)
	}
	if taskCount != 1 || stopCount != 0 || response != "该任务正在等待执行。" {
		t.Fatalf("status action created/stopped a task or returned the wrong status: tasks=%d stops=%d response=%q",
			taskCount, stopCount, response)
	}
}

func TestConversationRouteProviderDoesNotHoldConversationLock(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{})}
	defer releaseRouteTestClient(client)
	store, gateway, _, firstClaim, _ := routeModelFixture(t, false, client)
	ctx := context.Background()
	if err := store.ReleaseConversationRoute(ctx, firstClaim); err != nil {
		t.Fatal(err)
	}
	routeWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: gateway, WorkerID: "route-provider-lock-test",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	type routeResult struct {
		found bool
		err   error
	}
	workerResult := make(chan routeResult, 1)
	go func() {
		found, err := routeWorker.RunOnce(ctx)
		workerResult <- routeResult{found: found, err: err}
	}()
	select {
	case <-client.started:
	case result := <-workerResult:
		t.Fatalf("route worker finished before reaching the model provider: found=%t err=%v", result.found, result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("route worker did not reach the model provider")
	}

	content := "同时接收同一会话的下一条消息"
	input := app.ConversationRoutingInput{ModelID: firstClaim.Input.ModelID,
		PreviousClientMessageID: firstClaim.Message.ClientMessageID,
		Budget:                  task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}}
	requestHash, err := app.ConversationReceiptHash(firstClaim.Message.ConversationID, content, input)
	if err != nil {
		t.Fatal(err)
	}
	acceptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	secondMessage, _, err := store.AcceptConversationMessage(acceptCtx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: firstClaim.Message.OwnerID,
			ActorID: firstClaim.Message.ActorID, ConversationID: firstClaim.Message.ConversationID,
			Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: firstClaim.ServiceInstanceID, ClientID: "route-provider-lock-followup",
		RequestHash: requestHash, Input: input, Authorization: firstClaim.Authorization,
	})
	if err != nil {
		t.Fatalf("accepting a follow-up while provider is blocked waited on a conversation lock: %v", err)
	}

	releaseRouteTestClient(client)
	select {
	case result := <-workerResult:
		if !result.found || result.err != nil {
			t.Fatalf("finish first route after provider release: found=%t err=%v", result.found, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("route worker did not finish after provider release")
	}
	var routeState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, secondMessage.ID).Scan(&routeState); err != nil {
		t.Fatal(err)
	}
	if routeState != "pending" {
		t.Fatalf("follow-up receipt should remain pending for its own routing turn, got %q", routeState)
	}
}
