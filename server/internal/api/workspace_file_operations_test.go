package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type workspaceFileOperationHTTPStub struct {
	WorkspaceSyncService
	result    app.WorkspaceFileOperationResult
	err       error
	principal identity.Principal
	workspace string
	command   app.WorkspaceFileOperationCommand
	key       string
	calls     int
}

func (s *workspaceFileOperationHTTPStub) ApplyFileOperation(_ context.Context, principal identity.Principal, workspaceID string,
	command app.WorkspaceFileOperationCommand, key string) (app.WorkspaceFileOperationResult, error) {
	s.principal, s.workspace, s.command, s.key = principal, workspaceID, command, key
	s.calls++
	return s.result, s.err
}

func newWorkspaceFileOperationHTTPRouter(service workspaceFileOperationService) http.Handler {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession})
		c.Next()
	})
	h := &api{}
	h.registerWorkspaceFileOperations(router.Group("/api"), service)
	return router
}

func TestWorkspaceFileOperationRouteForwardsSessionAndRevisionFence(t *testing.T) {
	service := &workspaceFileOperationHTTPStub{result: app.WorkspaceFileOperationResult{
		WorkspaceID: "w1234567", Action: app.WorkspaceFileMove, SourcePath: "notes/a.txt", TargetPath: "archive/a.txt",
		ExpectedCurrentRevision: 7, Revision: 8,
	}}
	router := newWorkspaceFileOperationHTTPRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/file-operations",
		strings.NewReader(`{"action":"move","source_path":"notes/a.txt","target_path":"archive/a.txt","expected_current_revision":7,"write_enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "file-operation-01")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.calls != 1 || service.workspace != "w1234567" || service.principal.Kind != identity.KindSession ||
		service.command.Action != app.WorkspaceFileMove || service.command.SourcePath != "notes/a.txt" || service.command.TargetPath != "archive/a.txt" ||
		service.command.ExpectedCurrentRevision != 7 || !service.command.WriteEnabled || service.key != "file-operation-01" {
		t.Fatalf("file operation response = %d %s; service = %#v", response.Code, response.Body.String(), service)
	}
	var result app.WorkspaceFileOperationResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Revision != 8 {
		t.Fatalf("file operation response body = %#v, %v", result, err)
	}
}

func TestWorkspaceFileOperationRouteRequiresIdempotencyAndRevision(t *testing.T) {
	service := &workspaceFileOperationHTTPStub{}
	router := newWorkspaceFileOperationHTTPRouter(service)
	for _, test := range []struct {
		name string
		path string
		body string
		key  string
	}{
		{name: "missing idempotency key", path: "/api/workspaces/w1234567/file-operations", body: `{"action":"delete","source_path":"notes/a.txt","expected_current_revision":7,"write_enabled":true}`},
		{name: "missing revision", path: "/api/workspaces/w1234567/file-operations", body: `{"action":"delete","source_path":"notes/a.txt","write_enabled":true}`, key: "file-operation-02"},
		{name: "negative revision", path: "/api/workspaces/w1234567/file-operations", body: `{"action":"delete","source_path":"notes/a.txt","expected_current_revision":-1,"write_enabled":true}`, key: "file-operation-03"},
		{name: "unknown field", path: "/api/workspaces/w1234567/file-operations", body: `{"action":"delete","source_path":"notes/a.txt","expected_current_revision":7,"write_enabled":true,"owner_id":"u7654321"}`, key: "file-operation-04"},
		{name: "invalid workspace", path: "/api/workspaces/invalid/file-operations", body: `{"action":"delete","source_path":"notes/a.txt","expected_current_revision":7,"write_enabled":true}`, key: "file-operation-05"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || service.calls != 0 {
				t.Fatalf("response = %d %s, calls=%d; want 400 without service call", response.Code, response.Body.String(), service.calls)
			}
		})
	}
}

func TestWorkspaceFileOperationRouteReturnsUnavailableWithoutService(t *testing.T) {
	router := newWorkspaceFileOperationHTTPRouter(nil)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/file-operations",
		strings.NewReader(`{"action":"delete","source_path":"notes/a.txt","expected_current_revision":7,"write_enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "file-operation-06")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestWorkspaceFileOperationRouteForwardsDomainErrors(t *testing.T) {
	service := &workspaceFileOperationHTTPStub{err: domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed")}
	router := newWorkspaceFileOperationHTTPRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/file-operations",
		strings.NewReader(`{"action":"delete","source_path":"notes/a.txt","expected_current_revision":7,"write_enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "file-operation-07")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || service.calls != 1 {
		t.Fatalf("response = %d %s, calls=%d; want 409 after one call", response.Code, response.Body.String(), service.calls)
	}
}

func TestWorkspaceFileOperationRoutesAreMountedOnAuthenticatedAPI(t *testing.T) {
	services := newServices()
	fileOperations := &workspaceFileOperationHTTPStub{result: app.WorkspaceFileOperationResult{
		WorkspaceID: testWorkspaceID, Action: app.WorkspaceFileDelete, SourcePath: "notes/a.txt",
		ExpectedCurrentRevision: 2, Revision: 3,
	}}
	deps := services.deps()
	deps.WorkspaceSync = fileOperations
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/file-operations",
		`{"action":"delete","source_path":"notes/a.txt","expected_current_revision":2,"write_enabled":true}`,
		cookie, "https://portal.example.test", "Idempotency-Key", "file-operation-08")
	if response.Code != http.StatusOK || fileOperations.calls != 1 || fileOperations.workspace != testWorkspaceID {
		t.Fatalf("authenticated Workspace file operation = %d %s, calls=%d workspace=%q", response.Code, response.Body, fileOperations.calls, fileOperations.workspace)
	}
	unauthenticated := request(router, http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/file-operations",
		`{"action":"delete","source_path":"notes/a.txt","expected_current_revision":2,"write_enabled":true}`, "", "")
	if unauthenticated.Code != http.StatusUnauthorized || fileOperations.calls != 1 {
		t.Fatalf("unauthenticated file operation = %d %s, calls=%d", unauthenticated.Code, unauthenticated.Body, fileOperations.calls)
	}
}
