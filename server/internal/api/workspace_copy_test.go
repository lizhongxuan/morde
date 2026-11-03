package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceCopyHTTPStub struct {
	result    app.WorkspaceCopyResult
	err       error
	principal identity.Principal
	sourceID  string
	command   app.WorkspaceCopyCommand
	key       string
	calls     int
}

func (s *workspaceCopyHTTPStub) Copy(_ context.Context, principal identity.Principal, sourceID string,
	command app.WorkspaceCopyCommand, key string) (app.WorkspaceCopyResult, error) {
	s.principal, s.sourceID, s.command, s.key = principal, sourceID, command, key
	s.calls++
	return s.result, s.err
}

func newWorkspaceCopyHTTPRouter(service WorkspaceCopyEndpointService) http.Handler {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession})
		c.Next()
	})
	h := &api{}
	h.registerWorkspaceCopies(router.Group("/api"), service)
	return router
}

func TestWorkspaceCopyRouteForwardsSessionRequestAndReplayStatus(t *testing.T) {
	service := &workspaceCopyHTTPStub{result: app.WorkspaceCopyResult{
		SourceWorkspaceID: "w1234567", Workspace: app.DeviceWorkspaceInfo{ID: "w7654321", Name: "Copy", Revision: 1, State: workspace.StateReady},
		SourceRevision: 3, State: workspace.CopyCompleted,
	}}
	router := newWorkspaceCopyHTTPRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/copies", strings.NewReader(`{"name":"Copy","expected_current_revision":3}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "workspace-copy-01")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.calls != 1 || service.sourceID != "w1234567" ||
		service.command.Name != "Copy" || service.command.ExpectedCurrentRevision != 3 || service.key != "workspace-copy-01" ||
		service.principal.Kind != identity.KindSession {
		t.Fatalf("copy response = %d %s; captured = %#v", response.Code, response.Body.String(), service)
	}

	service.result.Replayed = true
	request = httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/copies", strings.NewReader(`{"name":"Copy","expected_current_revision":3}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "workspace-copy-01")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.calls != 2 {
		t.Fatalf("replayed copy response = %d %s, calls = %d", response.Code, response.Body.String(), service.calls)
	}
}

func TestWorkspaceCopyRouteRejectsMalformedRequestsAndServiceErrors(t *testing.T) {
	service := &workspaceCopyHTTPStub{}
	router := newWorkspaceCopyHTTPRouter(service)
	for _, tc := range []struct {
		name       string
		path       string
		key        string
		body       string
		serviceErr error
		status     int
	}{
		{name: "missing idempotency key", path: "/api/workspaces/w1234567/copies", body: `{"name":"Copy","expected_current_revision":3}`, status: http.StatusBadRequest},
		{name: "missing revision", path: "/api/workspaces/w1234567/copies", key: "copy-02", body: `{"name":"Copy"}`, status: http.StatusBadRequest},
		{name: "unknown field", path: "/api/workspaces/w1234567/copies", key: "copy-03", body: `{"name":"Copy","expected_current_revision":3,"force":true}`, status: http.StatusBadRequest},
		{name: "invalid workspace", path: "/api/workspaces/bad/copies", key: "copy-04", body: `{"name":"Copy","expected_current_revision":3}`, status: http.StatusBadRequest},
		{name: "service conflict", path: "/api/workspaces/w1234567/copies", key: "copy-05", body: `{"name":"Copy","expected_current_revision":3}`, serviceErr: domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed"), status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service.err = tc.serviceErr
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				request.Header.Set("Idempotency-Key", tc.key)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("POST status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}
	if service.calls != 1 {
		t.Fatalf("malformed requests reached service: calls = %d, want only the service-error request", service.calls)
	}
}

func TestWorkspaceCopyRouteMountedOnAuthenticatedAPI(t *testing.T) {
	services := newServices()
	copyService := &workspaceCopyHTTPStub{result: app.WorkspaceCopyResult{
		SourceWorkspaceID: testWorkspaceID, Workspace: app.DeviceWorkspaceInfo{ID: "w7654321", Name: "Copy", Revision: 1, State: workspace.StateReady},
		SourceRevision: 1, State: workspace.CopyCompleted,
	}}
	deps := services.deps()
	deps.WorkspaceCopies = copyService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/copies",
		`{"name":"Copy","expected_current_revision":1}`, cookie, "https://portal.example.test", "Idempotency-Key", "copy-01")
	if response.Code != http.StatusCreated || copyService.calls != 1 || copyService.principal.UserID != testUserID {
		t.Fatalf("authenticated Workspace copy = %d %s; service = %#v", response.Code, response.Body, copyService)
	}
	unauthenticatedRequest := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/copies",
		strings.NewReader(`{"name":"Copy","expected_current_revision":1}`))
	unauthenticatedRequest.Header.Set("Content-Type", "application/json")
	unauthenticatedRequest.Header.Set("Idempotency-Key", "copy-02")
	unauthenticatedRecorder := httptest.NewRecorder()
	router.ServeHTTP(unauthenticatedRecorder, unauthenticatedRequest)
	unauthenticated := unauthenticatedRecorder
	if unauthenticated.Code != http.StatusUnauthorized || copyService.calls != 1 {
		t.Fatalf("unauthenticated Workspace copy = %d %s; calls = %d", unauthenticated.Code, unauthenticated.Body, copyService.calls)
	}
}
