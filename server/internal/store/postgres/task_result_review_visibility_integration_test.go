package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestTaskResultReviewMessagesStayOutOfConversationAndTaskContext(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	personalConversation := createMainConversationForTest(t, store, owner.ID)

	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Review visibility", "Internal review message visibility", "review-visibility-project")
	projectConversation, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: mustID(t, domain.NewConversationID), OwnerID: owner.ID, ProjectID: projectValue.ID,
	}, testIdempotency(owner.ID, "project_conversation.create", "review-visibility-conversation"))
	if err != nil {
		t.Fatal(err)
	}

	seed := func(conversationID string) string {
		t.Helper()
		for _, message := range []conversation.Message{
			{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID, ConversationID: conversationID,
				Role: conversation.RoleUser, Content: "Completed Task results are ready for review.", SourceKind: "task_result_review"},
			{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID, ConversationID: conversationID,
				Role: conversation.RoleUser, Content: "Visible conversation message"},
			{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID, ConversationID: conversationID,
				Role: conversation.RoleUser, Content: "A later internal review trigger", SourceKind: "task_result_review"},
		} {
			if err := store.WithTx(ctx, func(tx pgx.Tx) error { return insertMessage(ctx, tx, message) }); err != nil {
				t.Fatal(err)
			}
		}
		return "Visible conversation message"
	}
	visibleContent := seed(personalConversation.ID)
	seed(projectConversation.ID)

	personalPage, err := store.ListMessages(ctx, owner.ID, personalConversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(personalPage.Items) != 1 || personalPage.Items[0].Content != visibleContent {
		t.Fatalf("personal conversation messages = %#v err=%v", personalPage.Items, err)
	}
	projectPage, err := store.ListProjectConversationMessages(ctx, owner.ID, projectValue.ID, projectConversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(projectPage.Items) != 1 || projectPage.Items[0].Content != visibleContent {
		t.Fatalf("project conversation messages = %#v err=%v", projectPage.Items, err)
	}

	var contextValue task.ConversationContext
	var manifest taskConversationContextManifest
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		contextValue, manifest, err = captureTaskConversationContext(ctx, tx, owner.ID, personalConversation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if contextValue.ThroughSequence != 2 || len(contextValue.Messages) != 1 || contextValue.Messages[0].Content != visibleContent ||
		len(manifest.MessageIDs) != 1 || manifest.MessageIDs[0] != personalPage.Items[0].ID {
		t.Fatalf("captured task context = %+v manifest=%+v", contextValue, manifest)
	}
	var restored []task.ConversationContextMessage
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		restored, err = readTaskConversationContextMessages(ctx, tx, owner.ID, personalConversation.ID, 3, nil, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].Content != visibleContent {
		t.Fatalf("restored model context = %#v", restored)
	}
	var projectContext []task.ConversationContextMessage
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		projectContext, err = readTaskConversationContextMessages(ctx, tx, owner.ID, projectConversation.ID, 3, nil, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(projectContext) != 1 || projectContext[0].Content != visibleContent {
		t.Fatalf("project model context = %#v", projectContext)
	}
}

func TestTaskResultReviewMessagesStayOutOfAgentCheckpointContext(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2_000, 4_096)
	submission := newTestTaskSubmission(t, owner.ID, "", "review-checkpoint-task", "initial prompt")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "review-checkpoint-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: task=%q found=%t err=%v", claimed.ID, found, err)
	}
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("review-checkpoint-supervisor", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "review-checkpoint-agent",
		AgentSessionID: "review-checkpoint-session", SupervisorID: "review-checkpoint-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-test", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed {
		t.Fatalf("start Agent: started=%#v replayed=%t err=%v", started, replayed, err)
	}
	fence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AgentSessionID: started.Fence.AgentSessionID, AgentRootSessionID: started.Fence.AgentSessionID,
		AuthorizationVersion: started.Fence.AuthorizationVersion, LeaseOwner: started.Fence.LeaseOwner,
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion}
	synthetic := conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
		ConversationID: created.Conversation.ID, TaskID: created.Task.ID, Role: conversation.RoleUser,
		Content: "Completed Task results are ready for review.", SourceKind: "task_result_review", DeliveryStatus: conversation.DeliveryPending}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error { return insertMessage(ctx, tx, synthetic) }); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := checkpoints.PendingMessages(ctx, fence)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending Agent messages = %#v err=%v", pending, err)
	}

	event := func(sequence int64, id, content string) map[string]any {
		return map[string]any{"seq": sequence, "type": "user/message", "data": map[string]any{"id": id, "role": "user", "content": content}}
	}
	payload, err := json.Marshal(map[string]any{
		"header": map[string]string{"id": fence.AgentSessionID}, "from_seq": 0,
		"events": []any{
			event(0, "2d47aa7b-f4db-4f11-b606-b7102c2c2df2", "initial prompt"),
			event(1, created.Message.ID, created.Message.Content),
			event(2, synthetic.ID, synthetic.Content),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: 0, Through: 2, Digest: digest[:], Payload: payload}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("checkpoint accepted internal review message: %v", err)
	}
	var deliveryStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, synthetic.ID).Scan(&deliveryStatus); err != nil ||
		deliveryStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("rejected synthetic checkpoint delivery status=%q err=%v", deliveryStatus, err)
	}
}
