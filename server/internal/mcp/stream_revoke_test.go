package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestActiveMCPRequestEndsAfterAPIKeyRevocation(t *testing.T) {
	verifier := &rotatingAPIKeyVerifier{principals: map[string]identity.Principal{
		"chk_stream": {UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567"},
	}}
	server := newTaskMCPServer(t, verifier, newTaskServiceStub(), 65536)
	session := connectTaskMCP(t, server.URL, "chk_stream")
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer chk_stream")
	request.Header.Set("Mcp-Session-Id", session.ID())
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("event stream status = %d, want 200", response.StatusCode)
	}
	streamClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, response.Body)
		close(streamClosed)
	}()
	select {
	case <-streamClosed:
		_ = response.Body.Close()
		t.Fatal("event stream closed before API key revocation")
	default:
	}
	verifier.revoke("chk_stream")

	select {
	case <-streamClosed:
	case <-time.After(apiKeyRevalidationInterval + time.Second):
		_ = response.Body.Close()
		t.Fatal("active MCP event stream was not closed after API key revocation")
	}
	_ = response.Body.Close()
}

func TestActiveMCPToolCancelsWhenHostGrantOrScopeChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		narrow func(*identity.Principal)
	}{
		{name: "host grant removed", narrow: func(p *identity.Principal) { p.HostIDs = nil }},
		{name: "operation scope removed", narrow: func(p *identity.Principal) { p.Scopes = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			principal := identity.Principal{
				UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
				HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerOperate},
			}
			verifier := &rotatingAPIKeyVerifier{principals: map[string]identity.Principal{"chk_change": principal}}
			runtime := &blockingMCPRuntimeTools{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
			handler, err := New(Deps{
				PreviewPortMin: 1024, Config: config.MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 1280},
				Domains:      config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
				Hosts:        &hostServiceStub{value: host.Host{ID: "h1234567", OwnerID: principal.UserID, State: host.StateRunning}},
				Applications: newApplicationStatusServiceStub(), RuntimeTools: runtime, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{},
				Auth: verifier, Audit: &auditStub{}, Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			session := connectTaskMCP(t, server.URL, "chk_change")
			t.Cleanup(func() { close(runtime.release) })
			arguments := map[string]any{
				"host_id": "h1234567", "workspace_id": "w1234567", "idempotency_key": "revoke-call-0001", "command": "printf ok",
			}
			type toolCallOutcome struct {
				result *sdk.CallToolResult
				err    error
			}
			callDone := make(chan toolCallOutcome, 1)
			go func() {
				result, callErr := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "shell_exec", Arguments: arguments})
				callDone <- toolCallOutcome{result: result, err: callErr}
			}()
			select {
			case <-runtime.started:
			case <-time.After(5 * time.Second):
				t.Fatal("runtime tool was not started")
			}

			current := principal
			test.narrow(&current)
			verifier.set("chk_change", current)
			select {
			case <-runtime.canceled:
			case <-time.After(apiKeyRevalidationInterval + 2*time.Second):
				t.Fatal("active tool context was not canceled after its authorization changed")
			}
			select {
			case outcome := <-callDone:
				if outcome.err == nil && (outcome.result == nil || !outcome.result.IsError) {
					t.Fatal("revoked tool call reported success")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("active tool call did not finish after cancellation")
			}

			// A fresh request must use narrowed authority and stop before dispatch.
			nextSession := connectTaskMCP(t, server.URL, "chk_change")
			result, err := nextSession.CallTool(context.Background(), &sdk.CallToolParams{Name: "shell_exec", Arguments: arguments})
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("tool call after authorization downgrade = %#v, %v; want denied", result, err)
			}
			if got := runtime.calls.Load(); got != 1 {
				t.Fatalf("runtime tool dispatches after authorization downgrade = %d, want 1", got)
			}
		})
	}
}

type blockingMCPRuntimeTools struct {
	calls    atomic.Int32
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (s *blockingMCPRuntimeTools) Execute(ctx context.Context, _ identity.Principal, _, _, _, _ string, _ json.RawMessage) (app.MCPRuntimeToolResult, error) {
	s.calls.Add(1)
	close(s.started)
	select {
	case <-ctx.Done():
	case <-s.release:
	}
	close(s.canceled)
	return app.MCPRuntimeToolResult{}, context.Canceled
}
