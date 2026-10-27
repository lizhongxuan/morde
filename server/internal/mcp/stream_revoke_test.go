package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	server := newRevokeMCPServer(t, verifier)
	session := connectRevokeMCP(t, server.URL, "chk_stream")
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

func newRevokeMCPServer(t *testing.T, verifier app.Verifier) *httptest.Server {
	t.Helper()
	handler, err := New(Deps{
		PreviewPortMin: 1024, Config: config.MCP{Enabled: true, MaxOutputBytes: 65536, ScreenshotMaxWidth: 1280},
		Domains:      config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: "hosts.example.test"},
		Hosts:        &hostServiceStub{value: host.Host{ID: "h1234567", State: host.StateRunning}},
		Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{},
		Auth: verifier, Audit: &auditStub{}, Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func connectRevokeMCP(t *testing.T, endpoint, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "revoke-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: &rotatingBearerTransport{base: http.DefaultTransport, token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
