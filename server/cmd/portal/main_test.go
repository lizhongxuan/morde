package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/config"
)

func TestRequireLoopbackMetrics(t *testing.T) {
	if err := requireLoopbackMetrics("127.0.0.1:7082"); err != nil {
		t.Fatalf("loopback metrics address rejected: %v", err)
	}
	for _, address := range []string{"0.0.0.0:7082", "localhost:7082", "[::1]:7082", "bad"} {
		if err := requireLoopbackMetrics(address); err == nil {
			t.Errorf("requireLoopbackMetrics(%q) accepted a non-127.0.0.1 address", address)
		}
	}
}

func TestCheckNetworkPoliciesPropagatesGuardFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network-policy.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := checkNetworkPolicies(context.Background(), map[string]config.Profile{
		"container": {BaseURL: "http://127.0.0.1:18080", NetworkGuardPath: path},
	})
	if !errors.Is(err, app.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "container") {
		t.Fatalf("checkNetworkPolicies() = %v, want container guard failure", err)
	}
}

func TestMetricsHandlerOnlyExposesMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "portal_test_counter", Help: "test"})
	if err := registry.Register(counter); err != nil {
		t.Fatal(err)
	}
	handler := metricsHandler(registry)
	for _, test := range []struct {
		path string
		code int
	}{{path: "/metrics", code: http.StatusOK}, {path: "/", code: http.StatusNotFound}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code {
			t.Errorf("GET %s = %d, want %d", test.path, response.Code, test.code)
		}
	}
}

func TestPortalCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"migrate"}, {"user"}, {"user", "add"}} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader("pw\n"), &stdout, &stderr); err == nil {
			t.Errorf("run(%q) returned nil error", args)
		}
	}
}

func TestWorkspaceVerifyRejectsUnexpectedArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"workspace", "verify", "unexpected"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments: unexpected") {
		t.Fatalf("workspace verify with unexpected argument error = %v", err)
	}
}

func TestUserAddCommandIsRemoved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"user", "add"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("removed user add command was accepted")
	}
}

func TestParseWorkspaceImportFlags(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		wantID      string
		wantConfig  string
		wantErrPart string
	}{
		{name: "valid", args: []string{"--workspace", "w1234567", "--config", "/tmp/portal.yaml"}, wantID: "w1234567", wantConfig: "/tmp/portal.yaml"},
		{name: "missing workspace", args: []string{"--config", "/tmp/portal.yaml"}, wantErrPart: "--workspace"},
		{name: "invalid workspace", args: []string{"--workspace", "../other"}, wantErrPart: "valid workspace ID"},
		{name: "unexpected argument", args: []string{"--workspace", "w1234567", "extra"}, wantErrPart: "unexpected arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspaceID, configPath, err := parseWorkspaceImportFlags(test.args, &bytes.Buffer{})
			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("parseWorkspaceImportFlags() error = %v, want substring %q", err, test.wantErrPart)
				}
				return
			}
			if err != nil || workspaceID != test.wantID || configPath != test.wantConfig {
				t.Fatalf("parseWorkspaceImportFlags() = (%q, %q, %v), want (%q, %q, nil)", workspaceID, configPath, err, test.wantID, test.wantConfig)
			}
		})
	}
}
