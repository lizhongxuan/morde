package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/config"
)

func TestMCPBehindLoopbackProxyValidatesHostAndOrigin(t *testing.T) {
	handler, err := New(Deps{
		PreviewPortMin: 1024,
		Config:         config.MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 2},
		Domains:        config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: "hosts.example.test"},
		Hosts:          &hostServiceStub{}, Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{}, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{}, Auth: verifierStub{}, Audit: &auditStub{},
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	for _, tc := range []struct {
		name, host, origin string
		status             int
	}{
		{"proxy host", "portal.example.test", "", http.StatusOK},
		{"same origin", "portal.example.test", "https://portal.example.test", http.StatusOK},
		{"cross origin", "portal.example.test", "https://evil.example", http.StatusForbidden},
		{"wrong scheme", "portal.example.test", "http://portal.example.test", http.StatusForbidden},
		{"origin with path", "portal.example.test", "https://portal.example.test/path", http.StatusForbidden},
		{"rebinding host", "evil.example", "", http.StatusForbidden},
		{"portal suffix", "portal.example.test.evil.example", "", http.StatusForbidden},
		{"loopback client", "localhost:7080", "", http.StatusOK},
		{"loopback cross origin", "127.0.0.1:7080", "https://evil.example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"proxy-test","version":"1"}}}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Host = tc.host
			request.Header.Set("Authorization", "Bearer chk_test_secret")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Origin", tc.origin)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", response.StatusCode, tc.status)
			}
		})
	}
}
