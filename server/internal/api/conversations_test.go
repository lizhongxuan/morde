package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type conversationServiceStub struct {
	created      app.CreateConversationCommand
	createCalls  int
	createdValue conversation.Conversation
	replayed     bool
	listCalls    int
	listPage     app.ConversationPage
	getID        string
	conversation conversation.Conversation
	messageID    string
	messagePage  app.MessagePage
	err          error
}

func (s *conversationServiceStub) Create(_ context.Context, _ identity.Principal, command app.CreateConversationCommand) (conversation.Conversation, bool, error) {
	s.createCalls++
	s.created = command
	if s.createdValue.ID != "" {
		return s.createdValue, s.replayed, s.err
	}
	return conversation.Conversation{
		ID: "c1234567", OwnerID: "u1234567", HostID: command.HostID, Title: command.Title,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, s.replayed, s.err
}

func (s *conversationServiceStub) List(context.Context, identity.Principal, app.PageRequest) (app.ConversationPage, error) {
	s.listCalls++
	return s.listPage, s.err
}

func (s *conversationServiceStub) Get(_ context.Context, _ identity.Principal, id string) (conversation.Conversation, error) {
	s.getID = id
	if s.conversation.ID != "" {
		return s.conversation, s.err
	}
	now := time.Now().UTC()
	return conversation.Conversation{ID: id, OwnerID: "u1234567", HostID: "h1234567", CreatedAt: now, UpdatedAt: now}, s.err
}

func (s *conversationServiceStub) ListMessages(_ context.Context, _ identity.Principal, id string, _ app.PageRequest) (app.MessagePage, error) {
	s.messageID = id
	return s.messagePage, s.err
}

func TestConversationCreateHandlerUsesIdempotencyAndReturnsReplayStatus(t *testing.T) {
	service := &conversationServiceStub{replayed: true}
	engine := taskAPIRouter(nil, service)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(`{"host_id":"h1234567","title":"  sprint notes  "}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key-010")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.createCalls != 1 || service.created.HostID != "h1234567" || service.created.IdempotencyKey != "retry-key-010" {
		t.Fatalf("status=%d calls=%d command=%#v body=%s", response.Code, service.createCalls, service.created, response.Body.String())
	}
}

func TestConversationCreateDoesNotRequireAHost(t *testing.T) {
	service := &conversationServiceStub{}
	engine := taskAPIRouter(nil, service)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(`{"title":"cloud work"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "cloud-conv-001")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.createCalls != 1 || service.created.HostID != "" {
		t.Fatalf("hostless conversation = %d %s, command=%#v", response.Code, response.Body, service.created)
	}
}

func TestConversationListReturnsEmptyItemsAndNullCursor(t *testing.T) {
	engine := taskAPIRouter(nil, &conversationServiceStub{})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations", nil))
	var body struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor json.RawMessage   `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Items == nil || string(body.NextCursor) != "null" {
		t.Fatalf("status=%d items=%#v cursor=%s body=%s", response.Code, body.Items, body.NextCursor, response.Body.String())
	}
}

func TestConversationMessagesReturnChronologicalItems(t *testing.T) {
	older := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	service := &conversationServiceStub{messagePage: app.MessagePage{Items: []conversation.Message{
		{ID: "m1234567", ConversationID: "c1234567", Role: conversation.RoleUser, Content: "first", TaskID: "q1234567", CreatedAt: older},
		{ID: "m7654321", ConversationID: "c1234567", Role: conversation.RoleAssistant, Content: "second", TaskID: "q1234567", CreatedAt: newer},
	}}}
	engine := taskAPIRouter(nil, service)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/c1234567/messages", nil))
	var body struct {
		Items []messageResponse `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(body.Items) != 2 || body.Items[0].Content != "first" || body.Items[1].Content != "second" || service.messageID != "c1234567" {
		t.Fatalf("status=%d messages=%#v id=%s body=%s", response.Code, body.Items, service.messageID, response.Body.String())
	}
}

func TestConversationCreateRejectsUnknownFields(t *testing.T) {
	service := &conversationServiceStub{}
	engine := taskAPIRouter(nil, service)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(`{"host_id":"h1234567","extra":"no"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key-011")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.createCalls != 0 {
		t.Fatalf("status=%d create calls=%d body=%s", response.Code, service.createCalls, response.Body.String())
	}
}
