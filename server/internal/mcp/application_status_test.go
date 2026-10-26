package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

type applicationStatusServiceStub struct {
	mu        sync.Mutex
	status    app.ApplicationsStatus
	err       error
	calls     int
	principal identity.Principal
	hostID    string
}

func newApplicationStatusServiceStub() *applicationStatusServiceStub {
	return &applicationStatusServiceStub{status: app.ApplicationsStatus{
		RuntimeGeneration: 1, RuntimeInstanceID: "runtime-test", ControlVersion: 2,
		CheckedAt: time.Date(2026, time.October, 24, 1, 2, 3, 0, time.UTC),
		Items: []runtimeclient.ApplicationStatusItem{
			{ID: "codex", Installed: true, Version: "0.164.0", LoginStatus: runtimeclient.ApplicationLoginNotLoggedIn},
			{ID: "wechat", Installed: true, Version: "4.1.13.26", LoginStatus: runtimeclient.ApplicationLoginNotApplicable},
		},
	}}
}

func (s *applicationStatusServiceStub) Applications(_ context.Context, principal identity.Principal, hostID string) (app.ApplicationsStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.principal, s.hostID = principal, hostID
	status := s.status
	if status.HostID == "" {
		status.HostID = hostID
	}
	return status, s.err
}

func (s *applicationStatusServiceStub) snapshot() (int, identity.Principal, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.principal, s.hostID
}

func TestHostCodexStatusMCPIsReadOnlyHostScopedAndSanitized(t *testing.T) {
	principal := identity.Principal{
		UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "token_test",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}
	applications := newApplicationStatusServiceStub()
	runtime := &runtimeComputerToolsStub{}
	audits := &auditStub{}
	session := newApplicationStatusMCPClient(t, principal, applications, runtime, audits)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tool := range listed.Tools {
		if tool.Name == hostCodexStatusToolName {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("Codex status tool is not annotated read-only: %#v", tool.Annotations)
			}
			if !strings.Contains(tool.Description, "does not run commands") {
				t.Fatalf("Codex status description omits its read-only boundary: %q", tool.Description)
			}
		}
	}
	if !found {
		t.Fatal("Codex status tool is not registered")
	}

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: hostCodexStatusToolName, Arguments: map[string]any{"host_id": "h1234567"},
	})
	if err != nil || result.IsError {
		t.Fatalf("Codex status result=%+v err=%v", result, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(textResult(result)), &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"host_id": "h1234567", "installed": true, "version": "0.164.0",
		"login_status": "not_logged_in", "checked_at": "2026-10-24T01:02:03Z",
	} {
		if got[key] != want {
			t.Errorf("Codex status %s = %#v, want %#v", key, got[key], want)
		}
	}
	for _, key := range []string{"runtime_generation", "runtime_instance_id", "control_version", "stdout", "stderr", "auth", "profile_path"} {
		if _, ok := got[key]; ok {
			t.Errorf("Codex status returned internal or sensitive field %q: %#v", key, got)
		}
	}
	calls, gotPrincipal, gotHostID := applications.snapshot()
	if calls != 1 || gotPrincipal.UserID != principal.UserID || gotPrincipal.TokenID != principal.TokenID || gotHostID != "h1234567" {
		t.Fatalf("application status calls=%d principal=%+v host=%q", calls, gotPrincipal, gotHostID)
	}
	if runtime.tool != "" {
		t.Fatalf("Codex status invoked RuntimeTools.Execute for %q", runtime.tool)
	}
	audits.mu.Lock()
	defer audits.mu.Unlock()
	if len(audits.entries) != 1 || audits.entries[0].Action != audit.ActionMCPCall || audits.entries[0].Target != "h1234567" {
		t.Fatalf("Codex status audit entries = %+v", audits.entries)
	}
}

func TestHostCodexStatusMCPRequiresObserveGrantAndRunningOwnerHost(t *testing.T) {
	tests := []struct {
		name       string
		principal  identity.Principal
		serviceErr error
		wantCalls  int
	}{
		{
			name:      "missing observe scope",
			principal: identity.Principal{UserID: "user_test", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}},
			wantCalls: 0,
		},
		{
			name:      "missing Host grant",
			principal: identity.Principal{UserID: "user_test", Kind: identity.KindAPIKey, Scopes: []identity.Scope{identity.ScopeComputerObserve}},
			wantCalls: 0,
		},
		{
			name:       "stopped Host rejected by status service",
			principal:  identity.Principal{UserID: "user_test", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve}},
			serviceErr: domain.NewError(domain.CodeNotFound, "host not found"), wantCalls: 1,
		},
		{
			name:       "non-owner Host rejected by status service",
			principal:  identity.Principal{UserID: "user_test", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve}},
			serviceErr: domain.NewError(domain.CodeNotFound, "host not found"), wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applications := newApplicationStatusServiceStub()
			applications.err = test.serviceErr
			runtime := &runtimeComputerToolsStub{}
			audits := &auditStub{}
			session := newApplicationStatusMCPClient(t, test.principal, applications, runtime, audits)

			result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
				Name: hostCodexStatusToolName, Arguments: map[string]any{"host_id": "h1234567"},
			})
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("unauthorized/unavailable status result=%+v err=%v", result, err)
			}
			if strings.Contains(textResult(result), "0.164.0") {
				t.Fatalf("denied status leaked app data: %s", textResult(result))
			}
			if calls, _, _ := applications.snapshot(); calls != test.wantCalls {
				t.Fatalf("application status calls = %d, want %d", calls, test.wantCalls)
			}
			if runtime.tool != "" {
				t.Fatalf("status call executed RuntimeTools.Execute for %q", runtime.tool)
			}
		})
	}
}

func TestHostCodexStatusMCPRejectsUnsafeProbeResponseAndExtraCommand(t *testing.T) {
	principal := identity.Principal{
		UserID: "user_test", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"},
		Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		status    app.ApplicationsStatus
		wantCalls int
	}{
		{
			name:      "command injection field rejected by schema",
			arguments: map[string]any{"host_id": "h1234567", "command": "cat /home/gem/.codex/auth.json"},
			wantCalls: 0,
		},
		{
			name:      "CLI output or credentials cannot masquerade as version",
			arguments: map[string]any{"host_id": "h1234567"},
			status: app.ApplicationsStatus{
				RuntimeGeneration: 1, RuntimeInstanceID: "runtime-test", ControlVersion: 2,
				CheckedAt: time.Now().UTC(), Items: []runtimeclient.ApplicationStatusItem{{
					ID: "codex", Installed: true, Version: "/home/gem/.codex/auth.json token=secret", LoginStatus: runtimeclient.ApplicationLoginLoggedIn,
				}},
			},
			wantCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			applications := newApplicationStatusServiceStub()
			if test.status.Items != nil {
				applications.status = test.status
			}
			runtime := &runtimeComputerToolsStub{}
			session := newApplicationStatusMCPClient(t, principal, applications, runtime, &auditStub{})

			result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: hostCodexStatusToolName, Arguments: test.arguments})
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("unsafe status result=%+v err=%v", result, err)
			}
			for _, secret := range []string{"auth.json", "token=secret", "cat /home"} {
				if strings.Contains(textResult(result), secret) {
					t.Fatalf("unsafe status leaked %q: %s", secret, textResult(result))
				}
			}
			if calls, _, _ := applications.snapshot(); calls != test.wantCalls {
				t.Fatalf("application status calls = %d, want %d", calls, test.wantCalls)
			}
			if runtime.tool != "" {
				t.Fatalf("status call executed RuntimeTools.Execute for %q", runtime.tool)
			}
		})
	}
}

func newApplicationStatusMCPClient(t *testing.T, principal identity.Principal, applications *applicationStatusServiceStub, runtime *runtimeComputerToolsStub, audits *auditStub) *sdk.ClientSession {
	t.Helper()
	handler, err := New(Deps{
		PreviewPortMin: 1024,
		Config:         config.MCP{Enabled: true, MaxOutputBytes: 4096, ScreenshotMaxWidth: 1280},
		Domains:        config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
		Hosts:          &hostServiceStub{value: host.Host{ID: "h1234567", OwnerID: "user_test", State: host.StateRunning}},
		Applications:   applications, RuntimeTools: runtime, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{},
		Auth: apiKeyPrincipalVerifier{principal: principal}, Audit: audits, Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := sdk.NewClient(&sdk.Implementation{Name: "codex-status-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

var _ applicationStatusService = (*applicationStatusServiceStub)(nil)
