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
	mainValue    conversation.MainConversation
	getMainCalls int
	ensureCmd    app.EnsureMainConversationCommand
	ensureCalls  int
	setCmd       app.SetMainConversationCommand
	setCalls     int
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

func (s *conversationServiceStub) GetMain(context.Context, identity.Principal) (conversation.MainConversation, error) {
	s.getMainCalls++
	if s.mainValue.Status == "" {
		return conversation.MainConversation{Status: conversation.MainConversationUninitialized}, s.err
	}
	return s.mainValue, s.err
}

func (s *conversationServiceStub) EnsureMain(_ context.Context, _ identity.Principal, command app.EnsureMainConversationCommand) (conversation.MainConversation, bool, error) {
	s.ensureCalls++
	s.ensureCmd = command
	if s.mainValue.Status == "" || s.mainValue.Status == conversation.MainConversationUninitialized {
		s.mainValue = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: "c1234567", Version: 1}
	}
	return s.mainValue, false, s.err
}

func (s *conversationServiceStub) SetMain(_ context.Context, _ identity.Principal, command app.SetMainConversationCommand) (conversation.MainConversation, bool, error) {
	s.setCalls++
	s.setCmd = command
	s.mainValue = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: command.ConversationID, Version: command.ExpectedVersion + 1}
	return s.mainValue, false, s.err
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

func TestMainConversationHandlersUseReadonlyGetAndIdempotentMutations(t *testing.T) {
	service := &conversationServiceStub{mainValue: conversation.MainConversation{Status: conversation.MainConversationUninitialized}}
	engine := taskAPIRouter(nil, service)
	read := httptest.NewRecorder()
	engine.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/me/main-conversation", nil))
	if read.Code != http.StatusOK || service.getMainCalls != 1 || service.ensureCalls != 0 ||
		!strings.Contains(read.Body.String(), `"status":"uninitialized"`) || !strings.Contains(read.Body.String(), `"version":0`) {
		t.Fatalf("read main conversation = %d %s, calls=%d/%d", read.Code, read.Body.String(), service.getMainCalls, service.ensureCalls)
	}

	ensure := httptest.NewRequest(http.MethodPost, "/api/me/main-conversation/ensure", nil)
	ensure.Header.Set("Idempotency-Key", "main-ensure-001")
	ensured := httptest.NewRecorder()
	engine.ServeHTTP(ensured, ensure)
	if ensured.Code != http.StatusOK || service.ensureCalls != 1 || service.ensureCmd.IdempotencyKey != "main-ensure-001" ||
		!strings.Contains(ensured.Body.String(), `"status":"ready"`) || !strings.Contains(ensured.Body.String(), `"conversation_id":"c1234567"`) {
		t.Fatalf("ensure main conversation = %d %s, calls=%d command=%+v", ensured.Code, ensured.Body.String(), service.ensureCalls, service.ensureCmd)
	}

	switchRequest := httptest.NewRequest(http.MethodPut, "/api/me/main-conversation", strings.NewReader(`{"conversation_id":"c7654321","expected_version":1}`))
	switchRequest.Header.Set("Content-Type", "application/json")
	switchRequest.Header.Set("Idempotency-Key", "main-switch-001")
	switched := httptest.NewRecorder()
	engine.ServeHTTP(switched, switchRequest)
	if switched.Code != http.StatusOK || service.setCalls != 1 || service.setCmd.ConversationID != "c7654321" ||
		service.setCmd.ExpectedVersion != 1 || service.setCmd.IdempotencyKey != "main-switch-001" ||
		!strings.Contains(switched.Body.String(), `"version":2`) {
		t.Fatalf("switch main conversation = %d %s, calls=%d command=%+v", switched.Code, switched.Body.String(), service.setCalls, service.setCmd)
	}
}

func TestMainConversationEnsureRejectsMissingKeyAndSwitchRequiresVersion(t *testing.T) {
	service := &conversationServiceStub{}
	engine := taskAPIRouter(nil, service)
	missingKey := httptest.NewRecorder()
	engine.ServeHTTP(missingKey, httptest.NewRequest(http.MethodPost, "/api/me/main-conversation/ensure", nil))
	if missingKey.Code != http.StatusBadRequest || service.ensureCalls != 0 {
		t.Fatalf("ensure without key = %d %s, calls=%d", missingKey.Code, missingKey.Body.String(), service.ensureCalls)
	}
	missingVersion := httptest.NewRequest(http.MethodPut, "/api/me/main-conversation", strings.NewReader(`{"conversation_id":"c1234567"}`))
	missingVersion.Header.Set("Content-Type", "application/json")
	missingVersion.Header.Set("Idempotency-Key", "main-switch-002")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, missingVersion)
	if response.Code != http.StatusBadRequest || service.setCalls != 0 {
		t.Fatalf("switch without expected version = %d %s, calls=%d", response.Code, response.Body.String(), service.setCalls)
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

func TestConversationResponsesIncludePersonalScopeAndMainIdentity(t *testing.T) {
	now := time.Now().UTC()
	service := &conversationServiceStub{
		mainValue:    conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: "c1234567", Version: 3},
		listPage:     app.ConversationPage{Items: []conversation.Conversation{{ID: "c1234567", OwnerID: "u1234567", CreatedAt: now, UpdatedAt: now}}},
		conversation: conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", CreatedAt: now, UpdatedAt: now},
	}
	engine := taskAPIRouter(nil, service)
	list := httptest.NewRecorder()
	engine.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/conversations", nil))
	var listed conversationPageResponse
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Scope != "personal" || !listed.Items[0].IsMain {
		t.Fatalf("conversation list response = %d %+v, body=%s", list.Code, listed, list.Body.String())
	}
	get := httptest.NewRecorder()
	engine.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/conversations/c1234567", nil))
	var single conversationResponse
	if err := json.Unmarshal(get.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if get.Code != http.StatusOK || single.Scope != "personal" || !single.IsMain {
		t.Fatalf("single conversation response = %d %+v, body=%s", get.Code, single, get.Body.String())
	}
}

func TestConversationMessagesReturnChronologicalItems(t *testing.T) {
	older := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	service := &conversationServiceStub{messagePage: app.MessagePage{Items: []conversation.Message{
		{ID: "m1234567", ConversationID: "c1234567", Role: conversation.RoleUser, Content: "first", TaskID: "q1234567", DeliveryStatus: conversation.DeliveryPending, CreatedAt: older},
		{ID: "m7654321", ConversationID: "c1234567", Role: conversation.RoleAssistant, Content: "second", TaskID: "q1234567", DeliveryStatus: conversation.DeliveryApplied, CreatedAt: newer},
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
	if response.Code != http.StatusOK || len(body.Items) != 2 || body.Items[0].Content != "first" || body.Items[1].Content != "second" ||
		body.Items[0].DeliveryStatus != conversation.DeliveryPending || body.Items[1].DeliveryStatus != conversation.DeliveryApplied || service.messageID != "c1234567" {
		t.Fatalf("status=%d messages=%#v id=%s body=%s", response.Code, body.Items, service.messageID, response.Body.String())
	}
}

func TestConversationMessagesReturnTasklessImportedHistoryAndAttachmentIDs(t *testing.T) {
	message := conversation.Message{
		ID: "m1234567", ConversationID: "c1234567", Role: conversation.RoleUser,
		Content: "Imported question", AttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"},
		DeliveryStatus: conversation.DeliveryApplied,
		CreatedAt:      time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	}
	engine := taskAPIRouter(nil, &conversationServiceStub{messagePage: app.MessagePage{Items: []conversation.Message{message}}})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/c1234567/messages", nil))
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(body.Items) != 1 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if _, exists := body.Items[0]["task_id"]; exists {
		t.Fatalf("taskless imported message returned a Task ID field: %s", response.Body)
	}
	var attachmentIDs []string
	if err := json.Unmarshal(body.Items[0]["attachment_ids"], &attachmentIDs); err != nil ||
		len(attachmentIDs) != 1 || attachmentIDs[0] != message.AttachmentIDs[0] {
		t.Fatalf("attachment IDs = %#v, body=%s, err=%v", attachmentIDs, response.Body, err)
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
