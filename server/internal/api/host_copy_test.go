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
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostCopyHTTPStub struct {
	result    app.HostCopyResult
	err       error
	principal identity.Principal
	sourceID  string
	command   app.HostCopyCommand
	key       string
	calls     int
}

func (s *hostCopyHTTPStub) Copy(_ context.Context, principal identity.Principal, sourceID string,
	command app.HostCopyCommand, key string) (app.HostCopyResult, error) {
	s.principal, s.sourceID, s.command, s.key = principal, sourceID, command, key
	s.calls++
	return s.result, s.err
}

func newHostCopyHTTPRouter(service HostCopyEndpointService) http.Handler {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession})
		c.Next()
	})
	h := &api{options: Options{HostScheme: "https", HostSuffix: "example.test"}}
	h.registerHostCopies(router.Group("/api"), service)
	return router
}

func validHostCopyResult() app.HostCopyResult {
	return app.HostCopyResult{
		SourceHostID: "h1234567", SourceWorkspaceID: "w1234567",
		Host: host.Host{
			ID: "h7654321", OwnerID: "u1234567", Name: "Clone", TemplateID: "std-2c4g", RuntimeProfile: "container",
			State: host.StateCreating, RuntimeGeneration: 1, ControlState: host.ControlAI,
			CPUMilli: 500, MemMiB: 1536, CPULimitMilli: 2000, MemLimitMiB: 4096,
			AIOKeyEnc: []byte("private-aio-key"), RuntimeCredentialEnc: []byte("private-runtime-credential"),
			Workspaces: []workspace.Binding{{
				OwnerID: "u1234567", HostID: "h7654321", Directory: "w7654321", State: workspace.BindingPreparing,
				BindingVersion: 1, DesiredRevision: 1, AppliedRevision: 0, SyncState: workspace.BindingSyncHealthy,
				Workspace: workspace.Workspace{ID: "w7654321", OwnerID: "u1234567", Name: "Clone", State: workspace.StateReady, Revision: 1},
			}},
		},
		Workspace:      app.DeviceWorkspaceInfo{ID: "w7654321", Name: "Clone", Revision: 1, State: workspace.StateReady},
		SourceRevision: 8, State: workspace.CopyCompleted,
	}
}

func TestHostCopyRouteForwardsRequestAndReturnsSanitizedHost(t *testing.T) {
	service := &hostCopyHTTPStub{result: validHostCopyResult()}
	router := newHostCopyHTTPRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/hosts/h1234567/copies", strings.NewReader(`{"name":"Clone","expected_current_revision":8}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "host-copy-01")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.calls != 1 || service.sourceID != "h1234567" ||
		service.command.Name != "Clone" || service.command.ExpectedCurrentRevision != 8 || service.key != "host-copy-01" ||
		service.principal.UserID != "u1234567" || service.principal.Kind != identity.KindSession {
		t.Fatalf("Host copy response = %d %s; service = %#v", response.Code, response.Body.String(), service)
	}
	if strings.Contains(response.Body.String(), "private-aio-key") || strings.Contains(response.Body.String(), "private-runtime-credential") ||
		strings.Contains(response.Body.String(), "aio_key") || strings.Contains(response.Body.String(), "runtime_credential") {
		t.Fatalf("Host copy response exposed a credential: %s", response.Body.String())
	}
	service.result.Replayed = true
	request = httptest.NewRequest(http.MethodPost, "/api/hosts/h1234567/copies", strings.NewReader(`{"name":"Clone","expected_current_revision":8}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "host-copy-01")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.calls != 2 {
		t.Fatalf("replayed Host copy response = %d %s; calls=%d", response.Code, response.Body.String(), service.calls)
	}
}

func TestHostCopyRouteRejectsMalformedRequestsAndServiceErrors(t *testing.T) {
	service := &hostCopyHTTPStub{}
	router := newHostCopyHTTPRouter(service)
	for _, tc := range []struct {
		name       string
		path       string
		key        string
		body       string
		serviceErr error
		status     int
	}{
		{name: "missing key", path: "/api/hosts/h1234567/copies", body: `{"name":"Clone","expected_current_revision":8}`, status: http.StatusBadRequest},
		{name: "missing revision", path: "/api/hosts/h1234567/copies", key: "host-copy-02", body: `{"name":"Clone"}`, status: http.StatusBadRequest},
		{name: "unknown field", path: "/api/hosts/h1234567/copies", key: "host-copy-03", body: `{"name":"Clone","expected_current_revision":8,"force":true}`, status: http.StatusBadRequest},
		{name: "invalid Host", path: "/api/hosts/bad/copies", key: "host-copy-04", body: `{"name":"Clone","expected_current_revision":8}`, status: http.StatusBadRequest},
		{name: "service conflict", path: "/api/hosts/h1234567/copies", key: "host-copy-05", body: `{"name":"Clone","expected_current_revision":8}`, serviceErr: domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed"), status: http.StatusConflict},
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

func TestHostCopyRouteMountedOnAuthenticatedAPI(t *testing.T) {
	services := newServices()
	copyService := &hostCopyHTTPStub{result: validHostCopyResult()}
	deps := services.deps()
	deps.HostCopies = copyService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/hosts/h1234567/copies",
		`{"name":"Clone","expected_current_revision":8}`, cookie, "https://portal.example.test", "Idempotency-Key", "host-copy-06")
	if response.Code != http.StatusCreated || copyService.calls != 1 || copyService.principal.UserID != testUserID {
		t.Fatalf("authenticated Host copy = %d %s; service = %#v", response.Code, response.Body, copyService)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/hosts/h1234567/copies", strings.NewReader(`{"name":"Clone","expected_current_revision":8}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "host-copy-07")
	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, request)
	if unauthenticated.Code != http.StatusUnauthorized || copyService.calls != 1 {
		t.Fatalf("unauthenticated Host copy = %d %s; calls = %d", unauthenticated.Code, unauthenticated.Body.String(), copyService.calls)
	}
}
