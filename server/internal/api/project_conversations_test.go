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
	"morde/server/internal/domain/task"
)

type projectConversationRoutesStub struct {
	principal      identity.Principal
	projectID      string
	conversationID string
	acceptCommand  app.AcceptConversationMessageCommand
	listCalls      int
	createCalls    int
	getCalls       int
	messageCalls   int
	acceptCalls    int
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
		OwnerID: "u1234567", ActorID: "u7654321", Sequence: 1, Role: conversation.RoleUser, Content: "shared",
		TaskID: "q1234567", DeliveryStatus: conversation.DeliveryApplied, CreatedAt: time.Now().UTC()}}}, nil
}

func (s *projectConversationRoutesStub) ListTasks(_ context.Context, principal identity.Principal, projectID, conversationID string, _ app.PageRequest) (app.TaskPage, error) {
	s.principal, s.projectID, s.conversationID = principal, projectID, conversationID
	return app.TaskPage{Items: []task.Task{}}, nil
}

func (s *projectConversationRoutesStub) AcceptMessage(_ context.Context, principal identity.Principal, projectID, conversationID string, command app.AcceptConversationMessageCommand) (conversation.Message, bool, error) {
	s.acceptCalls++
	s.principal, s.projectID, s.conversationID, s.acceptCommand = principal, projectID, conversationID, command
	message := conversation.Message{ID: "m1234567", ConversationID: conversationID, OwnerID: "u1234567", ActorID: principal.UserID,
		Sequence: 2, Role: conversation.RoleUser, Content: command.Content, ClientMessageID: command.ClientMessageID,
		RoutingStatus: conversation.RoutingPending, DeliveryStatus: conversation.DeliveryPending, CreatedAt: time.Now().UTC()}
	return message, false, nil
}

func testProjectConversation(projectID string) conversation.Conversation {
	now := time.Now().UTC()
	return conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", ProjectID: projectID, Title: "Shared",
		CreatedAt: now, UpdatedAt: now}
}

func projectConversationAPIRouter(principal *identity.Principal, service projectConversationRoutesService, taskServices ...TaskService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	if len(taskServices) > 0 {
		(&api{}).registerConversations(router.Group("/api"), nil, taskServices[0])
	}
	RegisterProjectConversationRoutes(router.Group("/api"), service)
	return router
}

func projectConversationAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
	request.Header.Set("X-Morde-Actor-ID", "u7654321")
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
	got = projectConversationAPIRequest(router, http.MethodGet, base+"/c1234567/tasks", "", "")
	var active conversationActiveTaskPageResponse
	if err := json.Unmarshal(got.Body.Bytes(), &active); err != nil || got.Code != http.StatusOK || active.Items == nil || len(active.Items) != 0 ||
		service.projectID != "j1234567" || service.conversationID != "c1234567" {
		t.Fatalf("list active project tasks = %d %s page=%#v err=%v", got.Code, got.Body, active, err)
	}
}

func TestProjectConversationMessageIsAcceptedWithoutStartingTask(t *testing.T) {
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	service := &projectConversationRoutesStub{}
	tasks := &taskServiceStub{}
	router := projectConversationAPIRouter(&principal, service, tasks)
	got := projectConversationAPIRequest(router, http.MethodPost, "/api/projects/j1234567/conversations/c1234567/messages",
		`{"client_message_id":"project-msg-0001","previous_client_message_id":"project-prev-0001","reference_task_id":"q1234567","change_task_target":true,"model_id":"p1234567","content":"hello"}`, "project-msg-0001")
	var response acceptedProjectConversationMessageResponse
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || got.Code != http.StatusAccepted ||
		service.acceptCalls != 1 || service.projectID != "j1234567" || service.conversationID != "c1234567" ||
		service.acceptCommand.ProjectID != "j1234567" || service.acceptCommand.ClientMessageID != "project-msg-0001" ||
		service.acceptCommand.PreviousClientMessageID != "project-prev-0001" || service.acceptCommand.ReferenceTaskID != "q1234567" ||
		!service.acceptCommand.ChangeTaskTarget ||
		service.acceptCommand.ModelID != "p1234567" || service.acceptCommand.Content != "hello" ||
		service.acceptCommand.ServiceInstanceID != "11111111-1111-4111-8111-111111111111" ||
		service.acceptCommand.IdempotencyKey != "project-msg-0001" ||
		response.Message.ActorID != principal.UserID || response.Message.OwnerID != "u1234567" ||
		response.Message.TaskID != "" || response.Message.RoutingStatus != conversation.RoutingPending || response.Replayed || tasks.acceptCalls != 0 {
		t.Fatalf("accept project message = %d %s response=%#v taskService=%#v projectService=%#v err=%v", got.Code, got.Body, response, tasks, service, err)
	}
}

func TestProjectConversationRoutesRejectMissingIdentityOrInvalidInput(t *testing.T) {
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	service := &projectConversationRoutesStub{}
	tasks := &taskServiceStub{}
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
		{method: http.MethodPost, path: base + "/c1234567/messages", body: `{"client_message_id":"project-msg-3","content":"hello"}`},
		{method: http.MethodPost, path: base + "/c1234567/messages", body: `{"client_message_id":"project-msg-4","mode":"new","content":"hello"}`, key: "project-msg-4"},
		{method: http.MethodPost, path: base + "/c1234567/messages", body: `{"client_message_id":"project-msg-5","expected_task_id":"q1234567","content":"hello"}`, key: "project-msg-5"},
	} {
		if got := projectConversationAPIRequest(projectConversationAPIRouter(&principal, service, tasks), test.method, test.path, test.body, test.key); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid project conversation request %q = %d %s", test.path, got.Code, got.Body)
		}
	}
	if service.createCalls != 0 || service.acceptCalls != 0 || tasks.acceptCalls != 0 {
		t.Fatalf("invalid requests reached service: %#v", service)
	}
}
