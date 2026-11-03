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
	value        conversation.Conversation
	key          IdempotencyRecord
	filterHosts  []string
	main         conversation.MainConversation
	mainRecord   IdempotencyRecord
	mainTarget   string
	mainExpected int64
	mainCalls    int
	mainReplayed bool
	mainErr      error
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

func (s *conversationStoreStub) GetMainConversation(context.Context, string) (conversation.MainConversation, error) {
	s.mainCalls++
	return s.main, s.mainErr
}

func (s *conversationStoreStub) EnsureMainConversation(_ context.Context, value conversation.Conversation, record IdempotencyRecord) (conversation.MainConversation, bool, error) {
	s.mainCalls++
	s.mainRecord = record
	if s.main.Status == conversation.MainConversationUninitialized {
		s.main = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: value.ID, Version: 1}
	}
	return s.main, s.mainReplayed, s.mainErr
}

func (s *conversationStoreStub) SetMainConversation(_ context.Context, ownerID, conversationID string, expectedVersion int64, record IdempotencyRecord) (conversation.MainConversation, bool, error) {
	s.mainCalls++
	s.mainRecord = record
	s.mainTarget, s.mainExpected = conversationID, expectedVersion
	if record.OwnerID != ownerID {
		return conversation.MainConversation{}, false, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	s.main = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: conversationID, Version: expectedVersion + 1}
	return s.main, s.mainReplayed, s.mainErr
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

func TestConversationServiceReadsEnsuresAndSwitchesMainConversation(t *testing.T) {
	store := &conversationStoreStub{main: conversation.MainConversation{Status: conversation.MainConversationUninitialized}}
	service, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	value, err := service.GetMain(context.Background(), principal)
	if err != nil || value.Status != conversation.MainConversationUninitialized || store.mainCalls != 1 {
		t.Fatalf("read uninitialized main conversation = %+v, calls=%d, err=%v", value, store.mainCalls, err)
	}
	value, replayed, err := service.EnsureMain(context.Background(), principal, EnsureMainConversationCommand{IdempotencyKey: "main-ensure-001"})
	if err != nil || replayed || value.Status != conversation.MainConversationReady || value.ConversationID == "" || value.Version != 1 ||
		store.mainCalls != 2 || store.mainRecord.OwnerID != principal.UserID || store.mainRecord.Operation != "main_conversation.ensure" ||
		store.mainRecord.Key != "main-ensure-001" || len(store.mainRecord.RequestHash) != 32 {
		t.Fatalf("ensure main conversation = %+v, replay=%t, calls=%d, record=%+v, err=%v", value, replayed, store.mainCalls, store.mainRecord, err)
	}
	value, replayed, err = service.SetMain(context.Background(), principal, SetMainConversationCommand{
		ConversationID: "c7654321", ExpectedVersion: value.Version, IdempotencyKey: "main-switch-001",
	})
	if err != nil || replayed || value.Status != conversation.MainConversationReady || value.ConversationID != "c7654321" || value.Version != 2 ||
		store.mainCalls != 3 || store.mainTarget != "c7654321" || store.mainExpected != 1 || store.mainRecord.Operation != "main_conversation.set" {
		t.Fatalf("switch main conversation = %+v, replay=%t, calls=%d, record=%+v, err=%v", value, replayed, store.mainCalls, store.mainRecord, err)
	}
}

func TestConversationServiceValidatesMainConversationMutations(t *testing.T) {
	store := &conversationStoreStub{main: conversation.MainConversation{Status: conversation.MainConversationUninitialized}}
	service, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	if _, _, err := service.EnsureMain(context.Background(), principal, EnsureMainConversationCommand{IdempotencyKey: "short"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("short ensure key should be rejected: %v", err)
	}
	if _, _, err := service.SetMain(context.Background(), principal, SetMainConversationCommand{
		ConversationID: "bad", ExpectedVersion: -1, IdempotencyKey: "main-switch-002",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid main conversation target/version should be rejected: %v", err)
	}
	if store.mainCalls != 0 {
		t.Fatalf("invalid mutations reached the store %d times", store.mainCalls)
	}
	principal.Scopes = []identity.Scope{identity.ScopeTaskSubmit}
	principal.Kind = identity.KindAPIKey
	if _, err := service.GetMain(context.Background(), principal); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("main conversation read without task read scope should fail: %v", err)
	}
}
