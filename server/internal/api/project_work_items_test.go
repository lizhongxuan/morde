package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectWorkItemRoutesStub struct {
	principal identity.Principal
	projectID string
	itemID    string
	page      app.PageRequest
	create    app.CreateProjectWorkItemCommand
	assignee  string
	reason    string
	taskID    string
	delivery  project.WorkItemDelivery
	expected  int64
	key       string
	calls     map[string]int
	item      project.WorkItem
	pageValue app.ProjectWorkItemPage
}

var _ projectWorkItemRoutesService = (*projectWorkItemRoutesStub)(nil)

func (s *projectWorkItemRoutesStub) List(_ context.Context, principal identity.Principal, projectID string, page app.PageRequest) (app.ProjectWorkItemPage, error) {
	s.record("list", principal, projectID, "", int64(page.Limit), "")
	s.page = page
	return s.pageValue, nil
}

func (s *projectWorkItemRoutesStub) Get(_ context.Context, principal identity.Principal, projectID, itemID string) (project.WorkItem, error) {
	s.record("get", principal, projectID, itemID, 0, "")
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) Create(_ context.Context, principal identity.Principal, projectID string, command app.CreateProjectWorkItemCommand) (project.WorkItem, bool, error) {
	s.record("create", principal, projectID, "", 0, command.IdempotencyKey)
	s.create = command
	return s.item, false, nil
}

func (s *projectWorkItemRoutesStub) Assign(_ context.Context, principal identity.Principal, projectID, itemID, assigneeID string, expected int64, key string) (project.WorkItem, error) {
	s.record("assign", principal, projectID, itemID, expected, key)
	s.assignee, s.expected, s.key = assigneeID, expected, key
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) Block(_ context.Context, principal identity.Principal, projectID, itemID, reason string, expected int64, key string) (project.WorkItem, error) {
	s.record("block", principal, projectID, itemID, expected, key)
	s.reason, s.expected, s.key = reason, expected, key
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) Resume(_ context.Context, principal identity.Principal, projectID, itemID string, expected int64, key string) (project.WorkItem, error) {
	s.record("resume", principal, projectID, itemID, expected, key)
	s.expected, s.key = expected, key
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) LinkTask(_ context.Context, principal identity.Principal, projectID, itemID, taskID string, expected int64, key string) (project.WorkItem, error) {
	s.record("link", principal, projectID, itemID, expected, key)
	s.taskID, s.expected, s.key = taskID, expected, key
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) Deliver(_ context.Context, principal identity.Principal, projectID, itemID string, delivery project.WorkItemDelivery, expected int64, key string) (project.WorkItem, error) {
	s.record("deliver", principal, projectID, itemID, expected, key)
	s.delivery, s.expected, s.key = delivery, expected, key
	return s.item, nil
}

func (s *projectWorkItemRoutesStub) record(name string, principal identity.Principal, projectID, itemID string, version int64, key string) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[name]++
	s.principal, s.projectID, s.itemID = principal, projectID, itemID
	if version != 0 {
		s.expected = version
	}
	if key != "" {
		s.key = key
	}
}

func projectWorkItemAPIRouter(principal *identity.Principal, service projectWorkItemRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectWorkItemRoutes(router.Group("/api"), service)
	return router
}

func projectWorkItemAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
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

func TestProjectWorkItemRoutesForwardPageCreateAndMutationContracts(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	item := project.WorkItem{
		ID: "j7654321", ProjectID: "j1234567", Title: "Ship module", Description: "Scope", AcceptanceCriteria: []string{"tested"},
		AssigneeID: principal.UserID, State: project.WorkItemOpen, TaskIDs: []string{}, Version: 2,
	}
	service := &projectWorkItemRoutesStub{item: item, pageValue: app.ProjectWorkItemPage{Items: []project.WorkItem{item}, NextCursor: "next"}}
	router := projectWorkItemAPIRouter(&principal, service)

	listed := projectWorkItemAPIRequest(router, http.MethodGet, "/api/projects/j1234567/work-items?limit=10&cursor=before", "", "")
	var page projectWorkItemPageResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || listed.Code != http.StatusOK || len(page.Items) != 1 ||
		page.Items[0].ID != item.ID || page.Items[0].AcceptanceCriteria[0] != "tested" || page.NextCursor == nil || *page.NextCursor != "next" ||
		service.page != (app.PageRequest{Limit: 10, Cursor: "before"}) || service.calls["list"] != 1 || !reflect.DeepEqual(service.principal, principal) {
		t.Fatalf("list = %d %s page=%#v service=%#v err=%v", listed.Code, listed.Body, page, service, err)
	}

	created := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items",
		`{"title":"Ship module","description":"Scope","acceptance_criteria":["tested"]}`, "work-item-create-01")
	if created.Code != http.StatusCreated || !reflect.DeepEqual(service.create, app.CreateProjectWorkItemCommand{
		Title: "Ship module", Description: "Scope", AcceptanceCriteria: []string{"tested"}, IdempotencyKey: "work-item-create-01",
	}) || service.calls["create"] != 1 {
		t.Fatalf("create = %d %s command=%#v calls=%#v", created.Code, created.Body, service.create, service.calls)
	}

	assigned := projectWorkItemAPIRequest(router, http.MethodPatch, "/api/projects/j1234567/work-items/j7654321",
		`{"assignee_id":"u7654321","expected_version":2}`, "work-item-assign-01")
	if assigned.Code != http.StatusOK || service.assignee != "u7654321" || service.expected != 2 || service.key != "work-item-assign-01" || service.calls["assign"] != 1 {
		t.Fatalf("assign = %d %s service=%#v", assigned.Code, assigned.Body, service)
	}

	blocked := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items/j7654321/block",
		`{"blocked_reason":"waiting","expected_version":3}`, "work-item-block-01")
	if blocked.Code != http.StatusOK || service.reason != "waiting" || service.expected != 3 || service.key != "work-item-block-01" || service.calls["block"] != 1 {
		t.Fatalf("block = %d %s service=%#v", blocked.Code, blocked.Body, service)
	}

	linked := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items/j7654321/tasks",
		`{"task_id":"q1234567","expected_version":4}`, "work-item-task-01")
	if linked.Code != http.StatusOK || service.taskID != "q1234567" || service.expected != 4 || service.key != "work-item-task-01" || service.calls["link"] != 1 {
		t.Fatalf("link task = %d %s service=%#v", linked.Code, linked.Body, service)
	}

	delivered := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items/j7654321/delivery",
		`{"task_id":"q1234567","workspace_id":"w1234567","revision":8,"expected_version":5}`, "work-item-delivery-01")
	wantDelivery := project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 8}
	if delivered.Code != http.StatusOK || service.delivery != wantDelivery || service.expected != 5 || service.key != "work-item-delivery-01" || service.calls["deliver"] != 1 {
		t.Fatalf("delivery = %d %s service=%#v", delivered.Code, delivered.Body, service)
	}
}

func TestProjectWorkItemRoutesRequirePrincipalAndStrictMutationPreconditions(t *testing.T) {
	service := &projectWorkItemRoutesStub{}
	if got := projectWorkItemAPIRequest(projectWorkItemAPIRouter(nil, service), http.MethodGet, "/api/projects/j1234567/work-items", "", ""); got.Code != http.StatusUnauthorized || service.calls["list"] != 0 {
		t.Fatalf("unauthenticated list = %d %s calls=%#v", got.Code, got.Body, service.calls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectWorkItemAPIRouter(&principal, service)
	if got := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items/j7654321/block", `{"blocked_reason":"waiting","expected_version":2,"state":"delivered"}`, "work-item-block-01"); got.Code != http.StatusBadRequest || service.calls["block"] != 0 {
		t.Fatalf("unknown block field = %d %s calls=%#v", got.Code, got.Body, service.calls)
	}
	if got := projectWorkItemAPIRequest(router, http.MethodPost, "/api/projects/j1234567/work-items/j7654321/resume", `{"expected_version":2}`, ""); got.Code != http.StatusBadRequest || service.calls["resume"] != 0 {
		t.Fatalf("missing idempotency key = %d %s calls=%#v", got.Code, got.Body, service.calls)
	}
}

func TestProjectWorkItemRoutesAreMountedWithoutFencingOtherAPIHandlers(t *testing.T) {
	services := newServices()
	workItems := &projectWorkItemRoutesStub{pageValue: app.ProjectWorkItemPage{Items: []project.WorkItem{}}}
	deps := services.deps()
	deps.ProjectWorkItems = workItems
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	listed := requestWithCookie(router, http.MethodGet, "/api/projects/j1234567/work-items", cookie, "")
	if listed.Code != http.StatusOK || workItems.calls["list"] != 1 || workItems.principal.UserID != testUserID {
		t.Fatalf("project work items = %d %s, calls=%#v principal=%#v", listed.Code, listed.Body, workItems.calls, workItems.principal)
	}
	workspaces := requestWithCookie(router, http.MethodGet, "/api/workspaces", cookie, "")
	if workspaces.Code != http.StatusOK {
		t.Fatalf("workspace route after project work item registration = %d %s", workspaces.Code, workspaces.Body)
	}
}
