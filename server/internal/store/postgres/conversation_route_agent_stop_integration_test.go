package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresConversationRouteStopAllRequiresAgentDecision(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		stop   bool
	}{
		{name: "model stop-all", output: `{"action":"stop_all"}`, stop: true},
		{name: "model reply cannot be replayed as stop-all", output: `{"action":"reply","reply":"我会说明。"}`},
		{name: "missing model output cannot stop", output: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			first := createConversationRouteActionTask(t, f, "agent-stop-first", "Prepare the first report")
			second := createConversationRouteActionTask(t, f, "agent-stop-second", "Prepare the second report")
			claimConversationRouteActionTask(t, f, first, "agent-stop-worker-a")
			claimConversationRouteActionTask(t, f, second, "agent-stop-worker-b")
			other := createOtherUsersRunningRouteTask(t, f)

			claim := f.receive(t, "agent-stop-control", "把我手头还在进行的工作都结束吧", app.ConversationRoutingInput{})
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			decision := app.ConversationRouteDecision{Action: app.ConversationRouteStopAll, ModelOutput: test.output}
			if got := validStoredRouteDecision(decision, snapshot.Context); got != test.stop {
				t.Fatalf("stored decision eligibility = %t, want %t", got, test.stop)
			}
			if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
				t.Fatal(err)
			}
			want := task.StatusRunning
			if test.stop {
				want = task.StatusCancelling
			}
			assertConversationRouteActionTaskState(t, f, first, want, 0, 0)
			assertConversationRouteActionTaskState(t, f, second, want, 0, 0)
			assertConversationRouteActionTaskState(t, f, other, task.StatusRunning, 0, 0)
		})
	}
}

func createOtherUsersRunningRouteTask(t *testing.T, f routeActionsTestFixture) string {
	t.Helper()
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, f.store, 2, 2000, 4096)
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p2345678',$1,'other route test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	chat := createMainConversationForTest(t, f.store, owner.ID)
	hash := sha256.Sum256([]byte("other-agent-stop-task"))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: chat.ID, Role: conversation.RoleUser, Content: "Prepare another user's report"},
		ServiceInstanceID: f.instanceID, ClientID: "other-agent-stop-task", RequestHash: hash[:],
		Input:         app.ConversationRoutingInput{ModelID: "p2345678", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "other-agent-route-worker", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim other user's route: found=%t err=%v", found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p2345678"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimConversationRouteActionTask(t, f, taskID, "other-agent-task-worker")
	return taskID
}
