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

type workspaceDiffHTTPStub struct {
	diff       app.WorkspaceDiff
	restore    app.WorkspaceRestoreResult
	err        error
	principal  identity.Principal
	workspace  string
	from, to   int64
	command    app.WorkspaceRestoreCommand
	key        string
	diffCalls  int
	resetCalls int
}

func (s *workspaceDiffHTTPStub) GetDiff(_ context.Context, principal identity.Principal, workspaceID string, from, to int64) (app.WorkspaceDiff, error) {
	s.principal, s.workspace, s.from, s.to = principal, workspaceID, from, to
	s.diffCalls++
	return s.diff, s.err
}

func (s *workspaceDiffHTTPStub) Restore(_ context.Context, principal identity.Principal, workspaceID string, command app.WorkspaceRestoreCommand, key string) (app.WorkspaceRestoreResult, error) {
	s.principal, s.workspace, s.command, s.key = principal, workspaceID, command, key
	s.resetCalls++
	return s.restore, s.err
}

func newWorkspaceDiffHTTPRouter(service WorkspaceDiffService) http.Handler {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession})
		c.Next()
	})
	h := &api{}
	h.registerWorkspaceDiff(router.Group("/api"), service)
	return router
}

func TestWorkspaceDiffRouteForwardsAuthorizedRequestAndBounds(t *testing.T) {
	service := &workspaceDiffHTTPStub{diff: app.WorkspaceDiff{WorkspaceID: "w1234567", FromRevision: 3, ToRevision: 4, Changes: []app.WorkspaceDiffChange{}}}
	router := newWorkspaceDiffHTTPRouter(service)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/w1234567/diff?from_revision=3&to_revision=4", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.diffCalls != 1 || service.workspace != "w1234567" || service.from != 3 || service.to != 4 || service.principal.UserID != "u1234567" {
		t.Fatalf("diff response = %d %s; captured = %#v", response.Code, response.Body.String(), service)
	}
	var result app.WorkspaceDiff
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.FromRevision != 3 || result.ToRevision != 4 || result.Changes == nil {
		t.Fatalf("diff response body = %#v, %v", result, err)
	}
	for _, path := range []string{
		"/api/workspaces/w1234567/diff?to_revision=4",
		"/api/workspaces/w1234567/diff?from_revision=3&from_revision=4&to_revision=4",
		"/api/workspaces/invalid/diff?from_revision=3&to_revision=4",
	} {
		bad := httptest.NewRecorder()
		router.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, path, nil))
		if bad.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, bad.Code)
		}
	}
}
func TestWorkspaceRestoreRouteRequiresIdempotencyAndRejectsUnknownFields(t *testing.T) {
	service := &workspaceDiffHTTPStub{restore: app.WorkspaceRestoreResult{
		WorkspaceID: "w1234567", TargetRevision: 1, ExpectedCurrentRevision: 3, Revision: 4,
	}}
	router := newWorkspaceDiffHTTPRouter(service)
	body := `{"target_revision":1,"expected_current_revision":3,"paths":["src/main.go"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/restore", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "restore-01")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.resetCalls != 1 || service.command.TargetRevision != 1 ||
		service.command.ExpectedCurrentRevision != 3 || len(service.command.Paths) != 1 || service.key != "restore-01" {
		t.Fatalf("restore response = %d %s; captured = %#v", response.Code, response.Body.String(), service)
	}

	for _, tc := range []struct {
		name   string
		key    string
		body   string
		status int
	}{
		{name: "missing key", body: body, status: http.StatusBadRequest},
		{name: "unknown field", key: "restore-02", body: `{"target_revision":1,"expected_current_revision":3,"paths":["src/main.go"],"force":true}`, status: http.StatusBadRequest},
		{name: "malformed body", key: "restore-03", body: `{`, status: http.StatusBadRequest},
		{name: "service error", key: "restore-04", body: body, status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "service error" {
				service.err = domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed")
			} else {
				service.err = nil
			}
			req := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/restore", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("POST status = %d, want %d: %s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestWorkspaceDiffRouteReturnsUnavailableWithoutService(t *testing.T) {
	router := newWorkspaceDiffHTTPRouter(nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/w1234567/diff?from_revision=0&to_revision=0", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestWorkspaceDiffRoutesAreMountedOnAuthenticatedAPI(t *testing.T) {
	services := newServices()
	diffService := &workspaceDiffHTTPStub{diff: app.WorkspaceDiff{
		WorkspaceID: testWorkspaceID, FromRevision: 1, ToRevision: 2, Changes: []app.WorkspaceDiffChange{},
	}}
	deps := services.deps()
	deps.WorkspaceDiff = diffService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet,
		"/api/workspaces/"+testWorkspaceID+"/diff?from_revision=1&to_revision=2", cookie, "")
	if response.Code != http.StatusOK || diffService.diffCalls != 1 || diffService.workspace != testWorkspaceID {
		t.Fatalf("authenticated Workspace diff = %d %s, calls=%d workspace=%q", response.Code, response.Body, diffService.diffCalls, diffService.workspace)
	}
	unauthenticated := request(router, http.MethodGet,
		"/api/workspaces/"+testWorkspaceID+"/diff?from_revision=1&to_revision=2", "", "", "")
	if unauthenticated.Code != http.StatusUnauthorized || diffService.diffCalls != 1 {
		t.Fatalf("unauthenticated Workspace diff = %d %s, calls=%d", unauthenticated.Code, unauthenticated.Body, diffService.diffCalls)
	}
}
