package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type conversationCreateRecorder struct {
	TaskStore
	values  []conversation.Conversation
	records []IdempotencyRecord
}

func (s *conversationCreateRecorder) CreateConversation(_ context.Context, value conversation.Conversation, record IdempotencyRecord) (conversation.Conversation, bool, error) {
	s.values = append(s.values, value)
	s.records = append(s.records, record)
	return value, false, nil
}

func TestConversationServiceListMessagesReturnsChronologicalPage(t *testing.T) {
	newer := time.Date(2026, 10, 21, 12, 0, 2, 0, time.UTC)
	older := newer.Add(-time.Second)
	store := &taskStoreStub{
		conversations: map[string]conversation.Conversation{
			"c1234567": {ID: "c1234567", OwnerID: "u1234567", HostID: "h1234567"},
		},
		messagePage: MessagePage{Items: []conversation.Message{
			{ID: "m7654321", ConversationID: "c1234567", OwnerID: "u1234567", Role: conversation.RoleUser, Content: "new", TaskID: "q1234567", CreatedAt: newer},
			{ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567", Role: conversation.RoleUser, Content: "old", TaskID: "q1234567", CreatedAt: older},
		}},
	}
	service, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListMessages(context.Background(), taskTestPrincipal(), "c1234567", PageRequest{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Content != "old" || page.Items[1].Content != "new" {
		t.Fatalf("message order = %#v", page.Items)
	}
}
func TestConversationServiceHidesUnauthorizedConversation(t *testing.T) {
	store := &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: "u1234567", HostID: "h1234567"},
	}}
	authorizer := &taskAuthorizerStub{err: domain.NewError(domain.CodeNotFound, "host not found")}
	service, err := NewConversationService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Get(context.Background(), taskTestPrincipal(), "c1234567")
	if domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unauthorized conversation error = %v", err)
	}
}
func TestConversationServiceCreateRequiresIdempotencyAndHostAuthorization(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Create(context.Background(), taskTestPrincipal(), CreateConversationCommand{HostID: "h1234567"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("missing idempotency key error = %v", err)
	}
	if len(store.created) != 0 {
		t.Fatalf("created rows after invalid request = %d", len(store.created))
	}
	_, _, err = service.Create(context.Background(), identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead},
	}, CreateConversationCommand{HostID: "h1234567", IdempotencyKey: "retry-key-006"})
	if domain.CodeOf(err) != domain.CodeForbidden || len(store.created) != 0 {
		t.Fatalf("missing submit scope error=%v created=%d", err, len(store.created))
	}
}

func TestConversationServiceCreateHashesNormalizedTitle(t *testing.T) {
	store := &conversationCreateRecorder{}
	service, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	for _, title := range []string{"  sprint notes  ", "sprint notes"} {
		if _, _, err := service.Create(context.Background(), principal, CreateConversationCommand{
			HostID: "h1234567", Title: title, IdempotencyKey: "retry-key-conv-001",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.values) != 2 || store.values[0].Title != "sprint notes" || store.values[1].Title != "sprint notes" {
		t.Fatalf("stored titles = %#v", store.values)
	}
	if len(store.records) != 2 || !bytes.Equal(store.records[0].RequestHash, store.records[1].RequestHash) {
		t.Fatalf("normalized title hashes differ: %#v", store.records)
	}
}
