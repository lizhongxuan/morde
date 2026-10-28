package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/worker"
)

type taskChatConfiguredModelCatalog struct{}

func (taskChatConfiguredModelCatalog) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

func TestTaskChatCompatibilityReplaysMigratedLegacyExchange(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 53); err != nil {
		t.Fatalf("prepare legacy instant-chat schema: %v", err)
	}
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	conversationID := mustID(t, domain.NewConversationID)
	exchangeID := mustID(t, domain.NewMessageID)
	userMessageID := mustID(t, domain.NewMessageID)
	assistantMessageID := mustID(t, domain.NewMessageID)
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_conversations (id, owner_id, model_id, title)
VALUES ($1, $2, 'p1234567', 'legacy title')`, conversationID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_exchanges
(id, conversation_id, owner_id, request_key, user_message_id, assistant_message_id, user_content, assistant_content)
VALUES ($1,$2,$3,'legacy-replay-001',$4,$5,'old question','old answer')`,
		exchangeID, conversationID, owner.ID, userMessageID, assistantMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 54); err != nil {
		t.Fatalf("migrate legacy instant-chat exchange: %v", err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, taskChatConfiguredModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := app.NewTaskChatCompatibilityService(tasks, conversations)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession}
	command := app.InstantChatSend{ModelID: "p1234567", Content: " old question ", RequestKey: "legacy-replay-001"}
	first, err := compatibility.Send(ctx, principal, command)
	if err != nil || first.Conversation.ID != conversationID || first.Conversation.Title != "legacy title" ||
		first.Message.ID != userMessageID || first.Message.Content != "old question" || first.Reply.ID != assistantMessageID || first.Reply.Content != "old answer" {
		t.Fatalf("legacy response = %#v, err=%v", first, err)
	}
	second, err := compatibility.Send(ctx, principal, command)
	if err != nil || second.Message.ID != first.Message.ID || second.Reply.ID != first.Reply.ID {
		t.Fatalf("legacy retry = %#v, err=%v; first = %#v", second, err, first)
	}
	if _, err := compatibility.Send(ctx, principal, app.InstantChatSend{
		ModelID: "p1234567", Content: "different question", RequestKey: "legacy-replay-001",
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("legacy key with changed request error = %v, want idempotency conflict", err)
	}
	var taskCount, messageCount int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE owner_id=$1),
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$2)`, owner.ID, conversationID).Scan(&taskCount, &messageCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 0 || messageCount != 2 {
		t.Fatalf("legacy retries changed canonical history: tasks=%d messages=%d", taskCount, messageCount)
	}
}

func TestTaskChatCompatibilityPersistsAndReplaysTask(t *testing.T) {
	store, credentials, _, owner := clientLoginFixture(t)
	pool := store.pool
	ctx := context.Background()
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	enableTaskExecutionForTest(t, store, principal)
	tasks, err := app.NewTaskService(store, credentials, taskChatConfiguredModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := app.NewTaskChatCompatibilityService(tasks, conversations)
	if err != nil {
		t.Fatal(err)
	}
	taskWorker, err := worker.NewTaskWorker(worker.TaskWorkerOptions{
		Store: store, Executor: taskChatCompatTestExecutor{store: store}, WorkerID: "task-chat-compat-test",
		LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		select {
		case err := <-workerDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("stop task worker: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("task worker did not stop after cancellation")
		}
	})

	command := app.InstantChatSend{ModelID: "p1234567", Content: "persistent chat task", RequestKey: "compat-pg-request-001"}
	first, err := compatibility.Send(ctx, principal, command)
	if err != nil || first.Reply.Content != "本地验收回复" {
		t.Fatalf("Send() = %#v, %v", first, err)
	}
	second, err := compatibility.Send(ctx, principal, command)
	if err != nil || second.Message.ID != first.Message.ID || second.Reply.ID != first.Reply.ID {
		t.Fatalf("idempotent replay = %#v, %v; first = %#v", second, err, first)
	}
	messages, err := compatibility.Messages(ctx, principal, first.Conversation.ID)
	if err != nil || len(messages) != 2 || messages[0].ID != first.Message.ID || messages[1].ID != first.Reply.ID {
		t.Fatalf("canonical conversation messages = %#v, %v", messages, err)
	}
	var taskCount, completedCount, tasklessAssistantCount int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='completed') FROM tasks WHERE owner_id=$1`, owner.ID).
		Scan(&taskCount, &completedCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE owner_id=$1 AND role='assistant' AND task_id IS NULL`, owner.ID).
		Scan(&tasklessAssistantCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || completedCount != 1 || tasklessAssistantCount != 0 {
		t.Fatalf("compatibility path wrote outside Task flow: tasks=%d completed=%d taskless assistant messages=%d", taskCount, completedCount, tasklessAssistantCount)
	}
}

func enableTaskExecutionForTest(t testing.TB, store *Store, principal identity.Principal) {
	t.Helper()
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewExecutionControlService(store, audit)
	if err != nil {
		t.Fatal(err)
	}
	current, err := control.Status(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Enable(context.Background(), principal, current.Epoch, "test-execution-enable-0001"); err != nil {
		t.Fatal(err)
	}
}
