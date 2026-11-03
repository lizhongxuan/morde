package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type projectConversationRoutesStub struct {
	principal      identity.Principal
	projectID      string
	conversationID string
	command        app.AppendMessageCommand
	listCalls      int
	createCalls    int
	getCalls       int
	messageCalls   int
	appendCalls    int
	replayed       bool
}

func (s *projectConversationRoutesStub) List(_ context.Context, principal identity.Principal, projectID string, _ app.PageRequest) (app.ConversationPage, error) {
	s.listCalls++
	s.principal, s.projectID = principal, projectID
	return app.ConversationPage{Items: []conversation.Conversation{testProjectConversation(projectID)}}, nil
}

func (s *projectConversationRoutesStub) Create(_ context.Context, principal identity.Principal, projectID string, command app.CreateProjectConversationCommand) (conversation.Conversation, bool, error) {
	s.createCalls++
	s.principal, s.projectID = principal, projectID
	value := testProjectConversation(projectID)
	value.Title = command.Title
	return value, s.replayed, nil
}

func (s *projectConversationRoutesStub) Get(_ context.Context, principal identity.Principal, projectID, conversationID string) (conversation.Conversation, error) {
	s.getCalls++
	s.principal, s.projectID, s.conversationID = principal, projectID, conversationID
	return testProjectConversation(projectID), nil
}

func (s *projectConversationRoutesStub) ListMessages(_ context.Context, principal identity.Principal, projectID, conversationID string, _ app.PageRequest) (app.MessagePage, error) {
	s.messageCalls++
	s.principal, s.projectID, s.conversationID = principal, projectID, conversationID
	return app.MessagePage{Items: []conversation.Message{{ID: "m1234567", ConversationID: conversationID,
		OwnerID: "u1234567", ActorID: "u7654321", Role: conversation.RoleUser, Content: "shared",
		TaskID: "q1234567", DeliveryStatus: conversation.DeliveryApplied, CreatedAt: time.Now().UTC()}}}, nil
}

func (s *projectConversationRoutesStub) AppendMessage(_ context.Context, principal identity.Principal, projectID, conversationID string, command app.AppendMessageCommand) (app.TaskSubmissionResult, error) {
	s.appendCalls++
	s.principal, s.projectID, s.conversationID, s.command = principal, projectID, conversationID, command
	message := conversation.Message{ID: "m1234567", ConversationID: conversationID, OwnerID: "u1234567", ActorID: principal.UserID,
		Role: conversation.RoleUser, Content: command.Content, TaskID: "q1234567", DeliveryStatus: conversation.DeliveryPending, CreatedAt: time.Now().UTC()}
	value := apiTestTask()
	value.ID, value.ConversationID, value.OwnerID, value.InitiatorID = "q1234567", conversationID, "u1234567", principal.UserID
	value.ProjectID = projectID
	return app.TaskSubmissionResult{Message: message, Task: value}, nil
}

func testProjectConversation(projectID string) conversation.Conversation {
	now := time.Now().UTC()
	return conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", ProjectID: projectID, Title: "Shared",
		CreatedAt: now, UpdatedAt: now}
}

func projectConversationAPIRouter(principal *identity.Principal, service projectConversationRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectConversationRoutes(router.Group("/api"), service)
	return router
}

func projectConversationAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestProjectConversationRoutesExposeSharedHistoryAndActor(t *testing.T) {
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	service := &projectConversationRoutesStub{}
	router := projectConversationAPIRouter(&principal, service)
	base := "/api/projects/j1234567/conversations"

	got := projectConversationAPIRequest(router, http.MethodGet, base, "", "")
	var page projectConversationPageResponse
	if err := json.Unmarshal(got.Body.Bytes(), &page); err != nil || got.Code != http.StatusOK || len(page.Items) != 1 ||
		page.Items[0].ProjectID != "j1234567" || page.Items[0].OwnerID != "u1234567" || service.listCalls != 1 || service.principal.UserID != principal.UserID {
		t.Fatalf("list project conversations = %d %s page=%#v err=%v", got.Code, got.Body, page, err)
	}

	service.replayed = true
	got = projectConversationAPIRequest(router, http.MethodPost, base, `{"title":"Shared"}`, "project-conv-create-1")
	if got.Code != http.StatusOK || service.createCalls != 1 || service.projectID != "j1234567" || service.principal.UserID != principal.UserID {
		t.Fatalf("replay project conversation create = %d %s service=%#v", got.Code, got.Body, service)
	}

	got = projectConversationAPIRequest(router, http.MethodGet, base+"/c1234567", "", "")
	if got.Code != http.StatusOK || service.getCalls != 1 || service.conversationID != "c1234567" {
		t.Fatalf("get project conversation = %d %s service=%#v", got.Code, got.Body, service)
	}

	got = projectConversationAPIRequest(router, http.MethodGet, base+"/c1234567/messages", "", "")
	var messages projectConversationMessagePageResponse
	if err := json.Unmarshal(got.Body.Bytes(), &messages); err != nil || got.Code != http.StatusOK || len(messages.Items) != 1 ||
		messages.Items[0].ActorID != "u7654321" || messages.Items[0].Content != "shared" || service.messageCalls != 1 {
		t.Fatalf("list project messages = %d %s page=%#v err=%v", got.Code, got.Body, messages, err)
	}
}

func TestProjectConversationAppendUsesCurrentActorAndProjectScope(t *testing.T) {
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	service := &projectConversationRoutesStub{}
	router := projectConversationAPIRouter(&principal, service)
	got := projectConversationAPIRequest(router, http.MethodPost, "/api/projects/j1234567/conversations/c1234567/messages",
		`{"model_id":"p1234567","content":"hello"}`, "project-msg-0001")
	var response appendProjectConversationMessageResponse
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || got.Code != http.StatusAccepted ||
		service.principal.UserID != principal.UserID || service.projectID != "j1234567" || service.conversationID != "c1234567" ||
		service.command.ProjectID != "j1234567" || service.command.IdempotencyKey != "project-msg-0001" || service.command.Content != "hello" ||
		response.Message.ActorID != principal.UserID || response.Task.InitiatorID != principal.UserID || response.Task.OwnerID != "u1234567" || response.Task.ProjectID != "j1234567" {
		t.Fatalf("append project message = %d %s response=%#v service=%#v err=%v", got.Code, got.Body, response, service, err)
	}
}

func TestProjectConversationRoutesRejectMissingIdentityOrInvalidInput(t *testing.T) {
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	service := &projectConversationRoutesStub{}
	base := "/api/projects/j1234567/conversations"
	if got := projectConversationAPIRequest(projectConversationAPIRouter(nil, service), http.MethodGet, base, "", ""); got.Code != http.StatusUnauthorized || service.listCalls != 0 {
		t.Fatalf("unauthenticated project conversation list = %d %s calls=%d", got.Code, got.Body, service.listCalls)
	}
	if got := projectConversationAPIRequest(projectConversationAPIRouter(&principal, nil), http.MethodGet, base, "", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing project conversation service = %d %s", got.Code, got.Body)
	}
	for _, test := range []struct{ method, path, body, key string }{
		{method: http.MethodGet, path: base + "?limit=0"},
		{method: http.MethodPost, path: base, body: `{"title":"bad","owner_id":"u7654321"}`, key: "project-conv-create-2"},
		{method: http.MethodPost, path: base, body: `{"title":"bad"}`},
		{method: http.MethodPost, path: base + "/c1234567/messages", body: `{"model_id":`, key: "project-msg-2"},
	} {
		if got := projectConversationAPIRequest(projectConversationAPIRouter(&principal, service), test.method, test.path, test.body, test.key); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid project conversation request %q = %d %s", test.path, got.Code, got.Body)
		}
	}
	if service.createCalls != 0 || service.appendCalls != 0 {
		t.Fatalf("invalid requests reached service: %#v", service)
	}
}
