package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type instantChatIntegrationModel struct {
	calls int
	fail  bool
	last  app.ModelRequest
}

func (m *instantChatIntegrationModel) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	m.calls++
	m.last = request
	if m.fail {
		return app.ModelResponse{}, errors.New("model unavailable")
	}
	if len(request.Tools) != 0 || len(request.Input) == 0 || request.Input[len(request.Input)-1].Text != "你好" {
		return app.ModelResponse{}, errors.New("unexpected chat request")
	}
	return app.ModelResponse{Text: "你好！"}, nil
}

func (*instantChatIntegrationModel) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, nil
}

type instantChatIntegrationGateway struct{ model app.ModelClient }

func (g instantChatIntegrationGateway) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Source != app.ModelCallChat || call.Request.Endpoint != (app.ModelEndpoint{}) {
		return app.ModelResponse{}, errors.New("unexpected chat model call")
	}
	call.Request.Endpoint = app.ModelEndpoint{BaseURL: "https://models.example.test", ModelID: "test"}
	return g.model.Complete(ctx, call.Request)
}

func TestInstantChatServicePersistsReplyWithoutTasks(t *testing.T) {
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 1, 1000, 2048)
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession}
	model := &instantChatIntegrationModel{}
	service, err := app.NewInstantChatService(store, instantChatIntegrationGateway{model})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	command := app.InstantChatSend{ModelID: "p1234567", Content: "你好", RequestKey: "chat-integration-01"}
	first, err := service.Send(ctx, principal, command)
	if err != nil || first.Reply.Content != "你好！" {
		t.Fatalf("send chat: reply=%+v err=%v", first, err)
	}
	messages, err := service.Messages(ctx, principal, first.Conversation.ID)
	if err != nil || len(messages) != 2 || messages[0].Content != "你好" || messages[1].Content != "你好！" {
		t.Fatalf("read persisted chat: messages=%+v err=%v", messages, err)
	}
	second, err := service.Send(ctx, principal, command)
	if err != nil || second.Reply.ID != first.Reply.ID || model.calls != 1 {
		t.Fatalf("idempotent retry: reply=%+v calls=%d err=%v", second, model.calls, err)
	}
	model.fail = true
	command.RequestKey = "chat-integration-02"
	command.ConversationID = first.Conversation.ID
	if _, err := service.Send(ctx, principal, command); err == nil {
		t.Fatal("model failure returned a successful chat response")
	}
	messages, err = service.Messages(ctx, principal, first.Conversation.ID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("model failure saved a reply: messages=%+v err=%v", messages, err)
	}
	var tasks, schedules int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM schedules)`).Scan(&tasks, &schedules); err != nil || tasks != 0 || schedules != 0 {
		t.Fatalf("instant chat created background work: tasks=%d schedules=%d err=%v", tasks, schedules, err)
	}
}

func TestInstantChatExchangePersistsAtomicallyAndChecksOwner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 1, 1000, 2048)
	other := createTestUser(t, store, 1, 1000, 2048)
	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	exchangeID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	userMessageID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	assistantMessageID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	conversation := app.InstantChatConversation{ID: conversationID, OwnerID: owner.ID, ModelID: "p1234567", Title: "你好"}
	exchange := app.InstantChatExchange{ID: exchangeID, ConversationID: conversationID, OwnerID: owner.ID, ModelID: conversation.ModelID,
		RequestKey: "request-12345678", UserMessageID: userMessageID, AssistantMessageID: assistantMessageID,
		UserContent: "你好", AssistantContent: "你好！"}
	savedConversation, savedExchange, err := store.SaveInstantChatExchange(context.Background(), conversation, exchange)
	if err != nil || savedConversation.CreatedAt.IsZero() || savedExchange.CreatedAt.IsZero() {
		t.Fatalf("save exchange: conversation=%+v exchange=%+v error=%v", savedConversation, savedExchange, err)
	}
	if _, err := store.GetInstantChatConversation(context.Background(), other.ID, conversationID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner accessed conversation: %v", err)
	}
	if _, _, err := store.GetInstantChatExchangeByKey(context.Background(), other.ID, exchange.RequestKey); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner accessed exchange: %v", err)
	}
	if _, _, err := store.SaveInstantChatExchange(context.Background(), conversation, exchange); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("duplicate request key was not rejected: %v", err)
	}
	items, err := store.ListInstantChatMessages(context.Background(), owner.ID, conversationID)
	if err != nil || len(items) != 2 || items[0].Content != "你好" || items[1].Content != "你好！" {
		t.Fatalf("saved messages missing: %+v, %v", items, err)
	}
	_, byKey, err := store.GetInstantChatExchangeByKey(context.Background(), owner.ID, exchange.RequestKey)
	if err != nil || byKey.RequestedConversationID != "" {
		t.Fatalf("request target missing from idempotency lookup: %+v, %v", byKey, err)
	}
	var tasks, schedules int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM schedules)`).Scan(&tasks, &schedules); err != nil || tasks != 0 || schedules != 0 {
		t.Fatalf("instant chat created background work: tasks=%d schedules=%d error=%v", tasks, schedules, err)
	}
}

func TestInstantChatCanChangeModelWithoutChangingPastRetries(t *testing.T) {
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 1, 1000, 2048)
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession}
	model := &instantChatIntegrationModel{}
	service, err := app.NewInstantChatService(store, instantChatIntegrationGateway{model})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	firstCommand := app.InstantChatSend{ModelID: "p1234567", Content: "你好", RequestKey: "change-model-first"}
	first, err := service.Send(ctx, principal, firstCommand)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Send(ctx, principal, app.InstantChatSend{ConversationID: first.Conversation.ID,
		ModelID: "p7654321", Content: "你好", RequestKey: "change-model-second"})
	if err != nil || second.Conversation.ID != first.Conversation.ID || second.Conversation.ModelID != "p7654321" {
		t.Fatalf("change model in same conversation: reply=%+v err=%v", second, err)
	}
	replayed, err := service.Send(ctx, principal, firstCommand)
	if err != nil || replayed.Message.ID != first.Message.ID || replayed.Reply.ID != first.Reply.ID || model.calls != 2 {
		t.Fatalf("retry after changing default model: reply=%+v calls=%d err=%v", replayed, model.calls, err)
	}
	var messages, oldWrites int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM retired_instant_chat_exchanges)`, first.Conversation.ID).Scan(&messages, &oldWrites); err != nil || messages != 4 || oldWrites != 0 {
		t.Fatalf("unified message writes: messages=%d old=%d err=%v", messages, oldWrites, err)
	}
}

func TestInstantChatConcurrentStoreRetriesCreateOneExchange(t *testing.T) {
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 1, 1000, 2048)
	conversationID := mustID(t, domain.NewConversationID)
	chat := app.InstantChatConversation{ID: conversationID, OwnerID: owner.ID, ModelID: "p1234567", Title: "hello"}
	exchange := app.InstantChatExchange{ID: mustID(t, domain.NewMessageID), ConversationID: conversationID, OwnerID: owner.ID,
		ModelID: chat.ModelID, RequestKey: "concurrent-chat-key", UserMessageID: mustID(t, domain.NewMessageID),
		AssistantMessageID: mustID(t, domain.NewMessageID), UserContent: "question", AssistantContent: "answer"}
	errorsSeen := make(chan error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _, err := store.SaveInstantChatExchange(context.Background(), chat, exchange)
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	var successes, conflicts int
	for err := range errorsSeen {
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeIdempotencyConflict {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	var messages, keys int
	if err := pool.QueryRow(context.Background(), `SELECT
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM idempotency_keys WHERE owner_id=$2 AND operation='instant_chat.send')`, conversationID, owner.ID).
		Scan(&messages, &keys); err != nil || successes != 1 || conflicts != 1 || messages != 2 || keys != 1 {
		t.Fatalf("concurrent retry: successes=%d conflicts=%d messages=%d keys=%d err=%v", successes, conflicts, messages, keys, err)
	}
}

func TestInstantChatContinuesExistingTaskConversation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	owner := createPreRetirementTestUser(t, store, 1, 1000, 2048)
	ctx := context.Background()
	input := newTestTaskSubmission(t, owner.ID, "", "task-before-chat", "cloud task question")
	input.Task.ModelID = "p1234567"
	created, err := store.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "chat-context-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim task: found=%t err=%v", found, err)
	}
	answer := conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
		OwnerID: owner.ID, Role: conversation.RoleAssistant, Content: "cloud task answer", TaskID: claimed.ID}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: claimed.ID, LeaseOwner: "chat-context-worker",
		LeaseVersion: claimed.Lease.Version, Status: task.StatusRunning,
		Event: task.Event{Type: "task.step", Payload: []byte(`{}`)}, AssistantMessage: &answer}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', lease_owner='', lease_expires_at=NULL,
finished_at=clock_timestamp() WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	model := &instantChatIntegrationModel{}
	service, err := app.NewInstantChatService(store, instantChatIntegrationGateway{model})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.Send(ctx, principal, app.InstantChatSend{ConversationID: created.Conversation.ID,
		ModelID: "p1234567", Content: "你好", RequestKey: "continue-task-chat"}); err != nil {
		t.Fatal(err)
	}
	if len(model.last.Input) != 3 || model.last.Input[0].Text != "cloud task question" || model.last.Input[1].Text != "cloud task answer" ||
		model.last.Input[2].Text != "你好" {
		t.Fatalf("model did not receive task conversation history: %+v", model.last.Input)
	}
	messages, err := service.Messages(ctx, principal, created.Conversation.ID)
	if err != nil || len(messages) != 4 || messages[0].Content != "cloud task question" ||
		messages[1].Content != "cloud task answer" || messages[2].Content != "你好" || messages[3].Content != "你好！" {
		t.Fatalf("unified conversation messages = %+v, err=%v", messages, err)
	}
}
