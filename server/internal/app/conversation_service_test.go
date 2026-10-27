package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type conversationStoreStub struct {
	TaskStore
	value       conversation.Conversation
	key         IdempotencyRecord
	filterHosts []string
}

func (s *conversationStoreStub) CreateConversation(_ context.Context, value conversation.Conversation, key IdempotencyRecord) (conversation.Conversation, bool, error) {
	s.value, s.key = value, key
	return value, false, nil
}

func (s *conversationStoreStub) GetConversation(_ context.Context, id, ownerID string) (conversation.Conversation, error) {
	if s.value.ID != id || s.value.OwnerID != ownerID {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return s.value, nil
}

func (s *conversationStoreStub) ListConversations(_ context.Context, ownerID string, hostIDs []string, _ PageRequest) (ConversationPage, error) {
	s.filterHosts = hostIDs
	if s.value.OwnerID != ownerID {
		return ConversationPage{Items: []conversation.Conversation{}}, nil
	}
	return ConversationPage{Items: []conversation.Conversation{s.value}}, nil
}

func TestConversationServiceDoesNotBindConversationToHost(t *testing.T) {
	store := &conversationStoreStub{}
	authorizer := &taskAuthorizerStub{}
	service, err := NewConversationService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	value, _, err := service.Create(context.Background(), principal, CreateConversationCommand{
		Title: "personal work", IdempotencyKey: "conversation-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.HostID != "" || authorizer.calls != 0 || store.key.OwnerID != principal.UserID {
		t.Fatalf("unexpected Host or authorization for personal conversation: %+v", value)
	}
	page, err := service.List(context.Background(), principal, PageRequest{})
	if err != nil || len(page.Items) != 1 || store.filterHosts != nil {
		t.Fatalf("account conversation list = %+v, %v", page, err)
	}
	other := principal
	other.UserID = "u7654321"
	if _, err := service.Get(context.Background(), other, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account accessed the conversation: %v", err)
	}
}

func TestConversationServiceChecksOptionalHostAndClientIdentity(t *testing.T) {
	store := &conversationStoreStub{}
	authorizer := &taskAuthorizerStub{err: domain.NewError(domain.CodeForbidden, "not authorized")}
	service, err := NewConversationService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	client := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	_, _, err = service.Create(context.Background(), client, CreateConversationCommand{
		HostID: "h1234567", IdempotencyKey: "conversation-123",
	})
	if domain.CodeOf(err) != domain.CodeForbidden || authorizer.calls != 1 || store.value.ID != "" {
		t.Fatalf("unauthorized Host should not create conversation: %v", err)
	}
	client.ClientInstallationID = ""
	_, _, err = service.Create(context.Background(), client, CreateConversationCommand{IdempotencyKey: "conversation-456"})
	if domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("missing client installation should be rejected: %v", err)
	}
}
