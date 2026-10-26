package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/config"
	"morde/server/internal/domain/host"
)

func TestSessionAfterServerWriteTimeout(t *testing.T) {
	if os.Getenv("CH_TEST_MCP_IDLE") != "1" {
		t.Skip("set CH_TEST_MCP_IDLE=1 for the 65-second Streamable HTTP session test")
	}
	handler, err := New(Deps{
		PreviewPortMin: 1024, PreviewPortDeny: []int{8080},
		Config:       config.MCP{Enabled: true, MaxOutputBytes: 65536, ScreenshotMaxWidth: 1280},
		Domains:      config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
		Hosts:        &hostServiceStub{value: host.Host{ID: "host_test", State: host.StateRunning}},
		Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{}, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{}, Auth: verifierStub{}, Audit: &auditStub{},
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var getRequests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getRequests.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.Config.ReadTimeout = 60 * time.Second
	server.Config.WriteTimeout = 60 * time.Second
	server.Config.IdleTimeout = 120 * time.Second
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "idle-verification", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   server.URL,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(65 * time.Second)
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "host_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("tool call after 65 seconds: result=%+v, err=%v", result, err)
	}
	t.Logf("session remains usable after 65s; standalone GET requests observed: %d", getRequests.Load())
}
