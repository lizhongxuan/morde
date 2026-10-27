package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type instantChatStoreFake struct {
	conversation InstantChatConversation
	exchange     InstantChatExchange
	history      []InstantChatMessage
	saves        int
}

func (f *instantChatStoreFake) ListInstantChatConversations(context.Context, string) ([]InstantChatConversation, error) {
	return []InstantChatConversation{f.conversation}, nil
}
func (f *instantChatStoreFake) GetInstantChatConversation(_ context.Context, ownerID, id string) (InstantChatConversation, error) {
	if f.conversation.ID != id || f.conversation.OwnerID != ownerID {
		return InstantChatConversation{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return f.conversation, nil
}
func (f *instantChatStoreFake) ListInstantChatMessages(_ context.Context, ownerID, id string) ([]InstantChatMessage, error) {
	if f.history != nil && f.conversation.OwnerID == ownerID && f.conversation.ID == id {
		return f.history, nil
	}
	if f.exchange.OwnerID == ownerID && f.exchange.ConversationID == id {
		return exchangeMessages(f.exchange), nil
	}
	return []InstantChatMessage{}, nil
}
func (f *instantChatStoreFake) GetInstantChatExchangeByKey(_ context.Context, ownerID, key string) (InstantChatConversation, InstantChatExchange, error) {
	if f.exchange.OwnerID == ownerID && f.exchange.RequestKey == key {
		return f.conversation, f.exchange, nil
	}
	return InstantChatConversation{}, InstantChatExchange{}, domain.NewError(domain.CodeNotFound, "not found")
}
func (f *instantChatStoreFake) SaveInstantChatExchange(_ context.Context, conversation InstantChatConversation, exchange InstantChatExchange) (InstantChatConversation, InstantChatExchange, error) {
	f.saves++
	conversation.CreatedAt, conversation.UpdatedAt = time.Now(), time.Now()
	exchange.CreatedAt = time.Now()
	f.conversation, f.exchange = conversation, exchange
	return conversation, exchange, nil
}

type instantChatGatewayFake struct {
	calls int
	call  ModelCall
}

func (f *instantChatGatewayFake) Complete(_ context.Context, call ModelCall) (ModelResponse, error) {
	f.calls++
	f.call = call
	if call.Source != ModelCallChat || call.Request.Endpoint != (ModelEndpoint{}) || len(call.Request.Tools) != 0 ||
		len(call.Request.Input) == 0 || call.Request.Input[len(call.Request.Input)-1].Text != "你好" {
		return ModelResponse{}, domain.NewError(domain.CodeInternal, "bad model call")
	}
	return ModelResponse{Text: "你好！"}, nil
}

func TestInstantChatUsesUnifiedConversationHistory(t *testing.T) {
	store := &instantChatStoreFake{
		conversation: InstantChatConversation{ID: "c1234567", OwnerID: "u1234567", ModelID: "p1234567"},
		history: []InstantChatMessage{
			{Role: "user", Content: "earlier cloud task"},
			{Role: "assistant", Content: "task result"},
		},
	}
	model := &instantChatGatewayFake{}
	service, err := NewInstantChatService(store, model)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Send(context.Background(), taskTestPrincipal(), InstantChatSend{
		ConversationID: "c1234567", ModelID: "p1234567", Content: "你好", RequestKey: "request-12345678",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.call.Request.Input) != 3 || model.call.Request.Input[0].Text != "earlier cloud task" || model.call.Request.Input[1].Text != "task result" {
		t.Fatalf("cloud task history missing from model input: %+v", model.call.Request.Input)
	}
}

func TestInstantChatSendAndIdempotency(t *testing.T) {
	store := &instantChatStoreFake{}
	model := &instantChatGatewayFake{}
	service, err := NewInstantChatService(store, model)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	command := InstantChatSend{ModelID: "p1234567", Content: "你好", RequestKey: "request-12345678"}
	first, err := service.Send(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Message.Role != "user" || first.Reply.Role != "assistant" || first.Reply.Content != "你好！" || first.Conversation.ID == "" {
		t.Fatalf("unexpected reply: %+v", first)
	}
	second, err := service.Send(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Message.ID != second.Message.ID || store.saves != 1 || model.calls != 1 ||
		model.call.CallID != instantChatModelCallID(principal.UserID, command.RequestKey) ||
		model.call.Principal.UserID != principal.UserID || model.call.Principal.Kind != principal.Kind ||
		model.call.Principal.Role != principal.Role {
		t.Fatalf("idempotency failed: saves=%d calls=%d", store.saves, model.calls)
	}
	command.Content = "different request"
	if _, err := service.Send(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reused key should conflict, got %v", err)
	}
	command.Content = "你好"
	command.ConversationID = first.Conversation.ID
	if _, err := service.Send(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("new conversation retry with a target should conflict, got %v", err)
	}
	command.RequestKey = "request-22222222"
	command.ModelID = "p7654321"
	continued, err := service.Send(context.Background(), principal, command)
	if err != nil {
		t.Fatalf("continue conversation: %v", err)
	}
	if continued.Conversation.ID != first.Conversation.ID || store.exchange.ModelID != command.ModelID {
		t.Fatalf("continued exchange did not retain the selected model: %+v", store.exchange)
	}
	if _, err := service.Send(context.Background(), principal, command); err != nil {
		t.Fatalf("retry continuation: %v", err)
	}
	command.ModelID = "p1234567"
	if _, err := service.Send(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with another model should conflict, got %v", err)
	}
	command.ModelID = "p7654321"
	command.ConversationID = ""
	if _, err := service.Send(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("continuation retry without its target should conflict, got %v", err)
	}
}

func TestInstantChatRejectsOtherOwnerAndNonSession(t *testing.T) {
	store := &instantChatStoreFake{conversation: InstantChatConversation{ID: "c1234567", OwnerID: "u1234567", ModelID: "p1234567"}}
	model := &instantChatGatewayFake{}
	service, err := NewInstantChatService(store, model)
	if err != nil {
		t.Fatal(err)
	}
	other := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.Messages(context.Background(), other, "c1234567"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner should not read messages, got %v", err)
	}
	client := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient}
	if _, err := service.List(context.Background(), client); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client credential should not read chat, got %v", err)
	}
}
