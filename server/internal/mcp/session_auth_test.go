package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestStreamableHTTPSessionUsesCurrentAPIKeyPrincipal(t *testing.T) {
	verifier := &rotatingAPIKeyVerifier{principals: map[string]identity.Principal{
		"chk_a": {
			UserID: "user_a", Role: identity.RoleAdmin, Kind: identity.KindAPIKey, TokenID: "key_a",
			HostIDs: []string{"host_test"}, Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostCreate},
		},
		"chk_b": {
			UserID: "user_b", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "key_b",
			HostIDs: []string{"host_test"}, Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostCreate},
		},
	}}
	hosts := &principalRecordingHostService{hostServiceStub: &hostServiceStub{}}
	handler, err := New(Deps{
		PreviewPortMin: 1024,
		Config:         config.MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 2},
		Domains:        config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
		Hosts:          hosts, Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{}, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{}, Auth: verifier, Audit: &auditStub{},
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	transport := &rotatingBearerTransport{base: http.DefaultTransport, token: "chk_a"}
	httpClient := &http.Client{Transport: transport}
	client := sdk.NewClient(&sdk.Implementation{Name: "session-auth-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true, HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if session.ID() == "" {
		t.Fatal("initialize did not create a stateful session")
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// The same key now resolves to the user's current role. The existing MCP
	// session must use that verified principal for every tool request.
	verifier.set("chk_a", identity.Principal{
		UserID: "user_a", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "key_a",
		HostIDs: []string{"host_test"}, Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostCreate},
	})
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "host_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("host_list result=%+v err=%v", result, err)
	}
	if got := hosts.lastListPrincipal(); got.UserID != "user_a" || got.Role != identity.RoleUser || got.TokenID != "key_a" {
		t.Errorf("host_list principal = %+v, want current user principal", got)
	}

	// A different valid API key cannot use any HTTP method on the first user's
	// session, and the rejected POST must not reach HostService.
	transport.setToken("chk_b")
	createCalls := hosts.createCalls.Load()
	created, callErr := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "host_create", Arguments: map[string]any{"template_id": "python", "idempotency_key": "host-create-key-002"},
	})
	if callErr == nil && created != nil && !created.IsError {
		t.Errorf("host_create with another user's session succeeded: %+v", created)
	}
	if status := transport.lastStatus(); status != http.StatusForbidden {
		t.Errorf("cross-user POST status = %d, want %d", status, http.StatusForbidden)
	}
	if got := hosts.createCalls.Load(); got != createCalls {
		t.Errorf("cross-user POST reached HostService %d times", got-createCalls)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequest(method, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Mcp-Session-Id", session.ID())
		if method == http.MethodGet {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Errorf("cross-user %s: %v", method, err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("cross-user %s status = %d, want %d", method, resp.StatusCode, http.StatusForbidden)
		}
	}
	if got := hosts.createCalls.Load(); got != createCalls {
		t.Errorf("cross-user requests changed HostService create count from %d to %d", createCalls, got)
	}
}

type rotatingAPIKeyVerifier struct {
	mu         sync.RWMutex
	principals map[string]identity.Principal
}

func (v *rotatingAPIKeyVerifier) set(key string, principal identity.Principal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.principals[key] = principal
}

func (v *rotatingAPIKeyVerifier) revoke(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.principals, key)
}

func (v *rotatingAPIKeyVerifier) VerifyAPIKey(_ context.Context, key string) (identity.Principal, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	principal, ok := v.principals[key]
	if !ok {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	return principal, nil
}

func (*rotatingAPIKeyVerifier) VerifySession(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, identity.ErrInvalidCredential
}

func (*rotatingAPIKeyVerifier) ConsumeTicket(context.Context, string) (app.Ticket, error) {
	return app.Ticket{}, identity.ErrInvalidCredential
}

type principalRecordingHostService struct {
	*hostServiceStub
	mu         sync.Mutex
	listCaller identity.Principal
}

func (s *principalRecordingHostService) List(_ context.Context, principal identity.Principal) ([]host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCaller = principal
	return []host.Host{}, nil
}

func (s *principalRecordingHostService) lastListPrincipal() identity.Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCaller
}

type rotatingBearerTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	token  string
	status int
}

func (t *rotatingBearerTransport) setToken(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = token
}

func (t *rotatingBearerTransport) lastStatus() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *rotatingBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	t.mu.Lock()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	t.mu.Unlock()
	resp, err := t.base.RoundTrip(clone)
	if resp != nil {
		t.mu.Lock()
		t.status = resp.StatusCode
		t.mu.Unlock()
	}
	return resp, err
}

var _ app.HostService = (*principalRecordingHostService)(nil)
var _ app.Verifier = (*rotatingAPIKeyVerifier)(nil)
