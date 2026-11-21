package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationRouteSteersOnlySafeRunningTaskSupplement(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	initial := f.receive(t, "steer-initial", "Create a short report", app.ConversationRoutingInput{})
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
	running, found, err := f.store.ClaimNextTask(ctx, "steer-task-worker", time.Minute)
	if err != nil || !found || running.ID != taskID {
		t.Fatalf("claim routed Task: task=%q found=%t err=%v", running.ID, found, err)
	}

	supplement := f.receive(t, "steer-supplement", "Also include a brief introduction", app.ConversationRoutingInput{ReferenceTaskID: taskID})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, supplement)
	if err != nil || !app.ValidConversationRouteSteerTarget(taskID, snapshot.Context) {
		t.Fatalf("safe supplement target = %+v err=%v", snapshot.Context, err)
	}
	snapshot.Context.ModelID = "p1234567"
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: taskID,
		ExpectedRevision: snapshot.Context.Candidates[0].Revision,
		ModelOutput:      `{"action":"steer","task_id":"` + taskID + `","acknowledge":false}`}
	if err := f.store.ApplyConversationRoute(ctx, supplement, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var routedTask, delivery, mode, routeState string
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),message.delivery_status,message.delivery_mode,route.state
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id WHERE message.id=$1`, supplement.Message.ID).
		Scan(&routedTask, &delivery, &mode, &routeState); err != nil {
		t.Fatal(err)
	}
	if routedTask != taskID || delivery != "pending" || mode != string(app.AgentMessageDeliverySteer) || routeState != "routed" {
		t.Fatalf("safe supplement route = task %q delivery %q mode %q state %q", routedTask, delivery, mode, routeState)
	}

	changedScope := f.receive(t, "steer-scope-change", "Use the other time zone for this", app.ConversationRoutingInput{
		ReferenceTaskID: taskID, TimeZone: "UTC"})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, changedScope)
	if err != nil || app.ValidConversationRouteSteerTarget(taskID, snapshot.Context) {
		t.Fatalf("scope-changing message was considered a safe supplement: context=%+v err=%v", snapshot.Context, err)
	}
	snapshot.Context.ModelID = "p1234567"
	decision.ExpectedRevision = snapshot.Context.Candidates[0].Revision
	if err := f.store.ApplyConversationRoute(ctx, changedScope, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var unassociatedTask, unassociatedMode, unassociatedState string
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),message.delivery_mode,route.state
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id WHERE message.id=$1`, changedScope.Message.ID).
		Scan(&unassociatedTask, &unassociatedMode, &unassociatedState); err != nil {
		t.Fatal(err)
	}
	if unassociatedTask != "" || unassociatedMode != string(app.AgentMessageDeliveryFollowup) || unassociatedState != "waiting_user" {
		t.Fatalf("unsafe supplement was not held for clarification: task %q mode %q state %q", unassociatedTask, unassociatedMode, unassociatedState)
	}
	if running.Status != task.StatusRunning {
		t.Fatalf("routed Task was not running when supplemented: %s", running.Status)
	}
}
