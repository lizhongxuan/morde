package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/config"
)

func TestCreateRequestMatchesGolden(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(apiKeyHeader) != "osb-secret" {
			t.Errorf("OpenSandbox key header missing")
		}
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"sandbox-1","status":{"state":"Running"}}`)
	}))
	defer server.Close()

	client := newTestClient(server.URL, nil)
	guardCalls := 0
	client.checkNetworkPolicy = func(_ context.Context, path, baseURL string) error {
		guardCalls++
		if path != "/opt/cloudhost/network-policy.sh" {
			t.Errorf("network guard path = %q", path)
		}
		if baseURL != server.URL {
			t.Errorf("network guard base URL = %q, want %q", baseURL, server.URL)
		}
		return nil
	}
	input := app.SandboxCreateRequest{
		Image: "registry.local/cloudhost/host:1.0.0", Entrypoint: []string{"/usr/local/bin/morde-host-entrypoint"}, Timeout: 8 * time.Hour,
		Requests: app.SandboxResources{CPU: "500m", Memory: "1536Mi"},
		Limits:   app.SandboxResources{CPU: "2", Memory: "4Gi"},
		Env: map[string]string{
			"SANDBOX_API_KEY": "redacted", "MORDE_RUNTIME_BOOTSTRAP_KEY": "redacted",
			"MORDE_HOST_ID": "h1234567", "MORDE_RUNTIME_GENERATION": "1",
			"WORKSPACE": "/home/gem/workspace", "TZ": "Asia/Shanghai",
		},
		Metadata: map[string]string{"ch-host": "h1234567", "ch-owner": "cli", "ch-workspace": "", "ch-env": "hostctl"},
		Volumes: []app.SandboxVolume{
			{Name: "profile-data", MountPath: app.ProfileVolumeMountPath, ClaimName: "profile-h1234567", Storage: app.ProfileVolumeStorage},
			{Name: "workspace", MountPath: "/home/gem/workspace", ClaimName: "ws-w1234567", Storage: "20Gi"},
			{Name: "runtime-data", MountPath: app.RuntimeDataVolumeMountPath, ClaimName: "runtime-h1234567", Storage: app.RuntimeDataVolumeStorage},
		},
	}
	created, err := client.Create(context.Background(), input)
	if err != nil || created.ID != "sandbox-1" || created.State != app.SandboxRunning {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	if guardCalls != 1 {
		t.Fatalf("network guard calls = %d, want 1", guardCalls)
	}
	if bytes.Contains(got, []byte("networkPolicy")) {
		t.Fatalf("create request still contains sidecar network policy: %s", got)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "create.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wantCompact bytes.Buffer
	if err := json.Compact(&wantCompact, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantCompact.Bytes()) {
		t.Fatalf("create request differs from golden:\n got: %s\nwant: %s", got, wantCompact.Bytes())
	}
}

func TestCreateDoesNotCallOpenSandboxWhenNetworkGuardFails(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestClient(server.URL, nil)
	guardCalls := 0
	client.checkNetworkPolicy = func(context.Context, string, string) error {
		guardCalls++
		return errors.New("private script output")
	}
	_, err := client.Create(context.Background(), app.SandboxCreateRequest{Image: "image"})
	if !errors.Is(err, app.ErrSandboxUnavailable) {
		t.Fatalf("Create() error = %v, want ErrSandboxUnavailable", err)
	}
	if guardCalls != 1 || requests != 0 {
		t.Fatalf("guard calls=%d OpenSandbox requests=%d, want 1 and 0", guardCalls, requests)
	}
	if strings.Contains(err.Error(), "private script output") {
		t.Fatal("Create() exposed network guard error output")
	}
}

func TestStateMappingAndUnknownReason(t *testing.T) {
	cases := map[string]app.SandboxState{
		"Pending": app.SandboxPending, "Running": app.SandboxRunning, "Paused": app.SandboxUnknown,
		"Pausing": app.SandboxUnknown, "Resuming": app.SandboxUnknown, "Stopping": app.SandboxTerminated, "Terminated": app.SandboxTerminated,
		"Failed": app.SandboxFailed, "future": app.SandboxUnknown,
	}
	for in, want := range cases {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%q) = %q", in, got)
		}
	}
	sandbox := sandboxResponse{Status: sandboxStatus{State: "future", Reason: "reason"}}.sandbox()
	if sandbox.Reason != "reason: future" {
		t.Fatalf("unknown state reason = %q", sandbox.Reason)
	}
}

func TestErrorStatusMappingAndDeleteNotFound(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, app.ErrSandboxNotFound},
		{http.StatusConflict, app.ErrSandboxConflict},
		{http.StatusUnprocessableEntity, app.ErrSandboxUnsupported},
		{http.StatusServiceUnavailable, app.ErrSandboxUnavailable},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		client := newTestClient(server.URL, nil)
		_, err := client.Get(context.Background(), "sandbox")
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d error = %v, want %v", tc.status, err, tc.want)
		}
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	if err := newTestClient(server.URL, nil).Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("Delete(missing) = %v", err)
	}
}

func TestListPaginatesAndEncodesMetadata(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		if got := r.URL.Query().Get("metadata"); got != "ch-env=dev&ch-host=h1234567" {
			t.Errorf("metadata query = %q", got)
		}
		if r.URL.Query().Get("pageSize") != strconv.Itoa(pageSize) {
			t.Errorf("pageSize = %q", r.URL.Query().Get("pageSize"))
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = io.WriteString(w, `{"items":[{"id":"one","status":{"state":"Running"}}],"pagination":{"hasNextPage":true}}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"two","status":{"state":"Paused"}}],"pagination":{"hasNextPage":false}}`)
	}))
	defer server.Close()

	items, err := newTestClient(server.URL, nil).List(context.Background(), map[string]string{"ch-host": "h1234567", "ch-env": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := []string{items[0].ID, items[1].ID}, []string{"one", "two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List IDs = %v", got)
	}
	if !reflect.DeepEqual(pages, []string{"1", "2"}) {
		t.Fatalf("pages = %v", pages)
	}
}

func TestEndpointAddsSchemeAndPreservesPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"endpoint":"127.0.0.1:32000/sandboxes/s1/port/8080","headers":{"X-Route":"ok"}}`)
	}))
	defer server.Close()
	upstream, err := newTestClient(server.URL, nil).Endpoint(context.Background(), "s1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.BaseURL != "http://127.0.0.1:32000/sandboxes/s1/port/8080" || upstream.Headers["X-Route"] != "ok" || upstream.APIKey != "" {
		t.Fatalf("upstream = %#v", upstream)
	}
}

func TestListKeepsRequestTimeoutWhileReadingChunkedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"sandbox-`)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(40 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `streamed","status":{"state":"Running"}}],"pagination":{"hasNextPage":false}}`)
	}))
	defer server.Close()

	client := NewClient(config.Profile{
		Name: "container", BaseURL: server.URL, RequestTimeout: time.Second,
	}, nil, nil)
	got, err := client.List(context.Background(), nil)
	if err != nil || len(got) != 1 || got[0].ID != "sandbox-streamed" || got[0].State != app.SandboxRunning {
		t.Fatalf("List() = %#v, %v; want streamed sandbox response", got, err)
	}
}

func TestRenewRequestTimeoutIncludesChunkedResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"expiresAt":"`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewClient(config.Profile{
		Name: "container", BaseURL: server.URL, RequestTimeout: 80 * time.Millisecond,
	}, nil, nil)
	_, err := client.Renew(context.Background(), "sandbox", time.Now().Add(time.Hour))
	if !errors.Is(err, app.ErrSandboxUnavailable) {
		t.Fatalf("Renew() error = %v; want ErrSandboxUnavailable after response-body timeout", err)
	}
}

func TestMetricsDoNotUseSandboxID(t *testing.T) {
	registry := prometheus.NewRegistry()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":"returned","status":{"state":"Running"}}`)
	}))
	defer server.Close()
	first := newTestClient(server.URL, registry)
	second := newTestClient(server.URL, registry)
	if _, err := first.Get(context.Background(), "sandbox-secret-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Get(context.Background(), "missing"); !errors.Is(err, app.ErrSandboxNotFound) {
		t.Fatalf("Get(missing) error = %v", err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetValue() == "sandbox-secret-id" || label.GetValue() == "missing" {
					t.Fatalf("sandbox ID appeared in metric label %s", label.GetName())
				}
			}
		}
	}
	if len(families) != 2 {
		t.Fatalf("registered metric families = %d", len(families))
	}
}

func TestMetadataEncodingIsDeterministic(t *testing.T) {
	want := url.Values{"ch-owner": {"agent & user/+?=汉字"}, "z-key": {"v=v&x"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded := r.URL.Query().Get("metadata")
		got, err := url.ParseQuery(encoded)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("metadata filter = %#v, %v; want %#v (encoded %q)", got, err, want, encoded)
		}
		_, _ = io.WriteString(w, `{"items":[],"pagination":{"hasNextPage":false}}`)
	}))
	defer server.Close()

	items, err := newTestClient(server.URL, nil).List(context.Background(), map[string]string{"z-key": "v=v&x", "ch-owner": "agent & user/+?=汉字"})
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("List() = %#v, %v", items, err)
	}
}

func TestRenew(t *testing.T) {
	expiresAt := time.Date(2026, time.October, 21, 12, 30, 0, 123, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandboxes/sandbox/renew-expiration":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("renew request = %s, Content-Type %q", r.Method, r.Header.Get("Content-Type"))
			}
			var input struct {
				ExpiresAt time.Time `json:"expiresAt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !input.ExpiresAt.Equal(expiresAt) {
				t.Errorf("renew request expiresAt = %s, %v", input.ExpiresAt, err)
			}
			_, _ = fmt.Fprintf(w, `{"expiresAt":%q}`, expiresAt.Format(time.RFC3339Nano))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(server.URL, nil)
	ctx := context.Background()
	got, err := client.Renew(ctx, "sandbox", expiresAt)
	if err != nil || !got.Equal(expiresAt) {
		t.Fatalf("Renew() = %s, %v; want %s", got, err, expiresAt)
	}
}

func TestRenewAndEndpointRejectMalformedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "renew-expiration") {
			_, _ = io.WriteString(w, "not-json")
			return
		}
		switch r.URL.Path {
		case "/v1/sandboxes/sandbox/endpoints/1":
			_, _ = io.WriteString(w, `{"endpoint":"","headers":null}`)
		case "/v1/sandboxes/sandbox/endpoints/2":
			_, _ = io.WriteString(w, `{"endpoint":"https://user:pass@example.com"}`)
		case "/v1/sandboxes/sandbox/endpoints/3":
			_, _ = io.WriteString(w, `{"endpoint":"ftp://example.com"}`)
		case "/v1/sandboxes/sandbox/endpoints/4":
			_, _ = io.WriteString(w, `{"endpoint":"example.com:8080"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(server.URL, nil)
	if _, err := client.Renew(context.Background(), "sandbox", time.Now()); err == nil || !strings.Contains(err.Error(), "decode opensandbox response") {
		t.Fatalf("Renew(malformed) error = %v", err)
	}
	for port, wantError := range map[int]string{1: "endpoint is empty", 2: "endpoint is invalid", 3: "endpoint is invalid"} {
		if _, err := client.Endpoint(context.Background(), "sandbox", port); err == nil || !strings.Contains(err.Error(), wantError) {
			t.Errorf("Endpoint(%d) error = %v, want %q", port, err, wantError)
		}
	}
	if got, err := client.Endpoint(context.Background(), "sandbox", 4); err != nil || got.BaseURL != "http://example.com:8080" || got.Headers == nil {
		t.Fatalf("Endpoint(no headers) = %#v, %v", got, err)
	}
}

func TestOrchestratorRequestErrorsAndCanceledContext(t *testing.T) {
	client := NewClient(config.Profile{Name: "bad", BaseURL: "http://%zz"}, nil, nil)
	if _, err := client.Get(context.Background(), "sandbox"); err == nil || !strings.Contains(err.Error(), "build opensandbox request") {
		t.Fatalf("Get(invalid base URL) error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestClient("http://127.0.0.1:1", nil).Get(ctx, "sandbox"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get(canceled) error = %v", err)
	}
}

func TestOpenSandboxIntegrationList(t *testing.T) {
	baseURL, apiKey := os.Getenv("CH_OSB_URL"), os.Getenv("CH_OSB_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("set CH_OSB_URL and CH_OSB_API_KEY to run the OpenSandbox integration check")
	}
	client := NewClient(config.Profile{
		Name:           "integration",
		BaseURL:        baseURL,
		APIKey:         apiKey,
		RequestTimeout: 30 * time.Second,
	}, nil, prometheus.NewRegistry())
	if _, err := client.List(context.Background(), nil); err != nil {
		t.Fatalf("list sandboxes from OpenSandbox: %v", err)
	}
}

func newTestClient(baseURL string, registry prometheus.Registerer) *Client {
	client := NewClient(config.Profile{
		Name: "container", BaseURL: baseURL, APIKey: "osb-secret", RequestTimeout: time.Second,
		NetworkGuardPath: "/opt/cloudhost/network-policy.sh",
	}, nil, registry)
	client.checkNetworkPolicy = func(context.Context, string, string) error { return nil }
	return client
}
