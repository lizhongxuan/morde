package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

// submitIndependentProjectTaskForTest creates a Task through the standalone
// store API for tests concerned with its project permissions and lifecycle.
func submitIndependentProjectTaskForTest(t *testing.T, ctx context.Context, store *Store,
	ownerID, actorID, projectID, conversationID, modelID, objective, key string, connectorIDs []string) (app.TaskSubmissionResult, error) {
	t.Helper()
	submission := newTestTaskSubmission(t, ownerID, "", key, objective)
	submission.Task.ProjectID, submission.Task.InitiatorID, submission.Task.ConversationID = projectID, actorID, conversationID
	submission.Task.ModelID, submission.Task.Authorization.TokenID = modelID, testSessionTokenID(actorID)
	submission.Message.ConversationID, submission.Message.ActorID = conversationID, actorID
	submission.MCPConnectorIDs = connectorIDs
	submission.Idempotency = testIdempotency(actorID, "task.submit", key)
	return store.SubmitTask(ctx, submission)
}

// routeProjectMessageForTest uses the production message receipt and routing
// transactions while supplying the deterministic new-Task decision under test.
func routeProjectMessageForTest(t *testing.T, ctx context.Context, store *Store, service *app.ProjectConversationService,
	principal identity.Principal, projectID, conversationID, key, content string) app.TaskSubmissionResult {
	t.Helper()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	message, replay, err := service.AcceptMessage(ctx, principal, projectID, conversationID, app.AcceptConversationMessageCommand{
		ServiceInstanceID: instanceID, ClientMessageID: key, IdempotencyKey: key, ModelID: "p1234567", Content: content,
	})
	if err != nil || replay {
		t.Fatalf("accept project message: replay=%t err=%v", replay, err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "project-message-test-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project message: found=%t id=%s err=%v", found, claim.Message.ID, err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListMessages(ctx, principal, projectID, conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == message.ID {
			return app.TaskSubmissionResult{Task: details.Task, Message: item}
		}
	}
	t.Fatalf("routed project message %s not found", message.ID)
	return app.TaskSubmissionResult{}
}
