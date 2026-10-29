package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"morde/server/internal/dockerapi"
	"morde/server/internal/storage"
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

func TestOpenSandboxLocalVolumeSurvivesSandboxDeleteAndRecreate(t *testing.T) {
	// The explicit opt-in also attests that this loopback OpenSandbox instance
	// uses the supplied local Docker socket directly; do not use a forwarded or
	// tunneled service here.
	if os.Getenv("CH_OSB_LOCAL_VOLUME_LIFECYCLE_TEST") != "1" {
		t.Skip("set CH_OSB_LOCAL_VOLUME_LIFECYCLE_TEST=1 and the required CH_OSB_LOCAL_* variables to run this local lifecycle check")
	}
	baseURL := os.Getenv("CH_OSB_LOCAL_URL")
	apiKey := os.Getenv("CH_OSB_LOCAL_API_KEY")
	dockerSocket := os.Getenv("CH_OSB_LOCAL_DOCKER_SOCKET")
	testImage := os.Getenv("CH_OSB_LOCAL_TEST_IMAGE")
	if baseURL == "" || apiKey == "" || dockerSocket == "" || testImage == "" {
		t.Fatal("CH_OSB_LOCAL_URL, CH_OSB_LOCAL_API_KEY, CH_OSB_LOCAL_DOCKER_SOCKET, and CH_OSB_LOCAL_TEST_IMAGE are required when the lifecycle opt-in is set")
	}
	if _, err := requireLocalLoopbackURL(baseURL); err != nil {
		t.Fatalf("CH_OSB_LOCAL_URL must be an HTTP(S) URL with a literal loopback IP and explicit port: %v", err)
	}
	if !filepath.IsAbs(dockerSocket) {
		t.Fatal("CH_OSB_LOCAL_DOCKER_SOCKET must be an absolute local Unix socket path")
	}
	socketInfo, err := os.Lstat(dockerSocket)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("CH_OSB_LOCAL_DOCKER_SOCKET must name an existing Unix socket, not a symlink or Docker context")
	}
	testImage = strings.TrimSpace(testImage)
	if testImage == "" {
		t.Fatal("CH_OSB_LOCAL_TEST_IMAGE must name an image already cached in the Docker engine at CH_OSB_LOCAL_DOCKER_SOCKET")
	}

	dockerHTTP := newLocalDockerHTTPClient(dockerSocket)
	imagePresent, err := localDockerResourceExists(t.Context(), dockerHTTP,
		"/images/"+url.PathEscape(testImage)+"/json")
	if err != nil {
		t.Fatalf("inspect explicitly cached local test image: %v", err)
	}
	if !imagePresent {
		t.Skip("configured local test image is not cached in the explicitly supplied Docker engine; image pulls are not permitted by this check")
	}

	volumeDeleter, err := storage.NewDocker((&url.URL{Scheme: "unix", Path: dockerSocket}).String(), os.TempDir(), 1)
	if err != nil {
		t.Fatalf("configure explicit local Docker socket: %v", err)
	}
	runID, err := randomLocalTestID()
	if err != nil {
		t.Fatalf("generate unique local test identity: %v", err)
	}
	claimName := "morde-osb-local-test-" + runID
	claimExists, err := localDockerResourceExists(t.Context(), dockerHTTP,
		"/volumes/"+url.PathEscape(claimName))
	if err != nil {
		t.Fatalf("check unique local test volume: %v", err)
	}
	if claimExists {
		t.Skip("generated test claim already exists; refusing to inspect, use, or delete it")
	}

	profile := config.Profile{Name: "local-volume-lifecycle", BaseURL: baseURL, APIKey: apiKey, RequestTimeout: 30 * time.Second}
	loopbackHTTP := newStrictLoopbackHTTPClient()
	client := NewClient(profile, loopbackHTTP, prometheus.NewRegistry())
	client.checkNetworkPolicy = func(_ context.Context, _, target string) error {
		if _, err := requireLocalLoopbackURL(target); err != nil {
			return fmt.Errorf("local OpenSandbox target is not loopback")
		}
		return nil
	}
	metadata := map[string]string{"ch-env": "morde-local-volume-lifecycle-test", "morde-test-run": runID}
	execdToken, err := randomLocalTestID()
	if err != nil {
		t.Fatalf("generate ephemeral local Execd token: %v", err)
	}

	createdIDs := make(map[string]struct{}, 2)
	cleanupSafe := true
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		listed, listErr := client.List(cleanupCtx, map[string]string{"morde-test-run": runID})
		if listErr != nil {
			t.Errorf("list only this test run's sandboxes during cleanup: %v", listErr)
		}
		ids := make(map[string]struct{}, len(createdIDs)+len(listed))
		for id := range createdIDs {
			ids[id] = struct{}{}
		}
		cleanupOK := listErr == nil && cleanupSafe
		if !cleanupSafe {
			t.Errorf("a sandbox create could not be ownership-verified; refusing to delete the named volume")
		}
		for _, sandbox := range listed {
			if sandbox.ID == "" || sandbox.Metadata["morde-test-run"] != runID {
				t.Errorf("OpenSandbox cleanup listing returned a sandbox outside this test run; refusing to delete it")
				cleanupOK = false
				continue
			}
			ids[sandbox.ID] = struct{}{}
		}
		for id := range ids {
			if err := deleteAndConfirmLocalSandbox(cleanupCtx, client, id, runID); err != nil {
				cleanupOK = false
				t.Errorf("delete test sandbox and confirm termination: %v", err)
			}
		}
		if !cleanupOK {
			return
		}
		if err := volumeDeleter.DeleteVolume(cleanupCtx, claimName); err != nil {
			t.Errorf("delete only this test run's named volume through the explicit local Docker socket: %v", err)
			return
		}
		stillExists, err := localDockerResourceExists(cleanupCtx, dockerHTTP,
			"/volumes/"+url.PathEscape(claimName))
		if err != nil || stillExists {
			t.Errorf("verify cleanup of the unique local test volume: exists=%t err=%v", stillExists, err)
		}
	})

	create := func() app.Sandbox {
		t.Helper()
		created, err := client.Create(t.Context(), app.SandboxCreateRequest{
			Image:      testImage,
			Entrypoint: []string{"/bin/sh", "-c", "sleep 600"},
			Timeout:    10 * time.Minute,
			Requests:   app.SandboxResources{CPU: "100m", Memory: "128Mi"},
			Limits:     app.SandboxResources{CPU: "1", Memory: "512Mi"},
			Env:        map[string]string{"EXECD_ACCESS_TOKEN": execdToken},
			Metadata:   metadata,
			Volumes: []app.SandboxVolume{{
				Name: "workspace", MountPath: "/workspace", ClaimName: claimName, Storage: "64Mi",
			}},
		})
		if err != nil {
			cleanupSafe = false
			t.Fatalf("create local OpenSandbox test sandbox: %v", err)
		}
		if created.ID == "" {
			cleanupSafe = false
			t.Fatal("OpenSandbox Create response omitted the sandbox ID")
		}
		verified, err := waitForLocalSandboxState(t.Context(), client, created.ID, runID, app.SandboxRunning, 90*time.Second)
		if verified {
			createdIDs[created.ID] = struct{}{}
		} else {
			cleanupSafe = false
		}
		if err != nil {
			t.Fatalf("wait for local OpenSandbox test sandbox: %v", err)
		}
		return created
	}

	first := create()
	claimExists, err = localDockerResourceExists(t.Context(), dockerHTTP,
		"/volumes/"+url.PathEscape(claimName))
	if err != nil || !claimExists {
		t.Fatalf("OpenSandbox did not create the unique claim in the explicitly supplied local Docker engine: exists=%t err=%v", claimExists, err)
	}
	firstURL, firstHeaders, err := localExecdEndpoint(t.Context(), client, first.ID)
	if err != nil {
		t.Fatalf("resolve safe local Execd endpoint: %v", err)
	}
	marker := "morde-volume-" + runID
	writeAndRead := "printf '%s' '" + marker + "' > /workspace/morde-persistent-marker && cat /workspace/morde-persistent-marker"
	firstOutput, err := runLocalExecdCommand(t.Context(), loopbackHTTP, firstURL, firstHeaders, execdToken, writeAndRead)
	if err != nil || firstOutput != marker {
		t.Fatalf("write marker through local Execd endpoint %s: output matched=%t err=%v", firstURL, firstOutput == marker, err)
	}

	if err := deleteAndConfirmLocalSandbox(t.Context(), client, first.ID, runID); err != nil {
		t.Fatalf("delete first sandbox and confirm runtime termination: %v", err)
	}
	claimExists, err = localDockerResourceExists(t.Context(), dockerHTTP,
		"/volumes/"+url.PathEscape(claimName))
	if err != nil || !claimExists {
		t.Fatalf("persistent claim disappeared after deleting first sandbox: exists=%t err=%v", claimExists, err)
	}

	second := create()
	secondURL, secondHeaders, err := localExecdEndpoint(t.Context(), client, second.ID)
	if err != nil {
		t.Fatalf("resolve safe restored Execd endpoint: %v", err)
	}
	readback, err := runLocalExecdCommand(t.Context(), loopbackHTTP, secondURL, secondHeaders, execdToken,
		"cat /workspace/morde-persistent-marker")
	if err != nil || readback != marker {
		t.Fatalf("read marker from restored Execd endpoint %s: matched=%t err=%v", secondURL, readback == marker, err)
	}
}

func requireLocalLoopbackURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("expected an HTTP(S) URL without credentials, query, or fragment")
	}
	ip := net.ParseIP(parsed.Hostname())
	port, portErr := strconv.Atoi(parsed.Port())
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return nil, errors.New("URL must use a literal loopback IP and explicit valid port")
	}
	return parsed, nil
}

func TestLocalOpenSandboxEndpointValidation(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		execd       bool
		wantAllowed bool
	}{
		{name: "ipv4 loopback", endpoint: "http://127.0.0.1:18080", wantAllowed: true},
		{name: "ipv6 loopback", endpoint: "https://[::1]:18080", wantAllowed: true},
		{name: "execd proxy", endpoint: "http://127.0.0.1:18080/proxy/44772", execd: true, wantAllowed: true},
		{name: "hostname", endpoint: "http://localhost:18080"},
		{name: "non-loopback ip", endpoint: "http://192.0.2.1:18080"},
		{name: "missing port", endpoint: "http://127.0.0.1"},
		{name: "credentials", endpoint: "http://user@127.0.0.1:18080"},
		{name: "query", endpoint: "http://127.0.0.1:18080?token=value"},
		{name: "unexpected execd path", endpoint: "http://127.0.0.1:18080/admin", execd: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.execd {
				_, err = requireLocalExecdEndpoint(test.endpoint)
			} else {
				_, err = requireLocalLoopbackURL(test.endpoint)
			}
			if allowed := err == nil; allowed != test.wantAllowed {
				t.Fatalf("endpoint allowed=%t, want %t (err=%v)", allowed, test.wantAllowed, err)
			}
		})
	}
}

func requireLocalExecdEndpoint(raw string) (*url.URL, error) {
	parsed, err := requireLocalLoopbackURL(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Path != "" && parsed.Path != "/proxy/44772" {
		return nil, errors.New("expected a direct Execd endpoint or the OpenSandbox /proxy/44772 route")
	}
	return parsed, nil
}

func localExecdEndpoint(ctx context.Context, client *Client, sandboxID string) (*url.URL, map[string]string, error) {
	endpoint, err := client.Endpoint(ctx, sandboxID, 44772)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := requireLocalExecdEndpoint(endpoint.BaseURL)
	return parsed, endpoint.Headers, err
}

func newStrictLoopbackHTTPClient() *http.Client {
	transport := &http.Transport{Proxy: nil}
	return &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			if _, err := requireLocalLoopbackURL(request.URL.String()); err != nil {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func newLocalDockerHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func localDockerResourceExists(ctx context.Context, client *http.Client, path string) (bool, error) {
	endpoint, err := dockerapi.NewEndpointResolver(client).Endpoint(ctx, path)
	if err != nil {
		return false, errors.New("resolve explicit local Docker Engine API version")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("build explicit local Docker inspection request")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("connect to the explicitly supplied local Docker socket")
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("explicit local Docker inspection returned HTTP %d", response.StatusCode)
	}
}

func randomLocalTestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func waitForLocalSandboxState(ctx context.Context, client *Client, sandboxID, runID string, target app.SandboxState, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	verified := false
	for {
		value, err := client.Get(ctx, sandboxID)
		if errors.Is(err, app.ErrSandboxNotFound) && target == app.SandboxTerminated {
			return true, nil
		}
		if err == nil {
			if value.Metadata["morde-test-run"] != runID {
				return false, errors.New("sandbox metadata does not match this test run; refusing to use or delete it")
			}
			verified = true
			if value.State == target {
				return true, nil
			}
			if target == app.SandboxRunning && (value.State == app.SandboxFailed || value.State == app.SandboxTerminated) {
				return true, fmt.Errorf("sandbox entered terminal state %q", value.State)
			}
		} else if !errors.Is(err, app.ErrSandboxUnavailable) && !errors.Is(err, app.ErrSandboxNotFound) {
			return verified, errors.New("read local OpenSandbox state")
		}
		if time.Now().After(deadline) {
			return verified, errors.New("timed out waiting for local OpenSandbox sandbox")
		}
		if err := waitLocalTestPoll(ctx); err != nil {
			return verified, err
		}
	}
}

func deleteAndConfirmLocalSandbox(ctx context.Context, client *Client, sandboxID, runID string) error {
	if err := client.Delete(ctx, sandboxID); err != nil && !errors.Is(err, app.ErrSandboxNotFound) {
		return errors.New("request local OpenSandbox sandbox deletion")
	}
	if _, err := waitForLocalSandboxState(ctx, client, sandboxID, runID, app.SandboxTerminated, 30*time.Second); err != nil {
		return fmt.Errorf("confirm local OpenSandbox sandbox deletion: %w", err)
	}
	return nil
}

func waitLocalTestPoll(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runLocalExecdCommand(ctx context.Context, client *http.Client, endpoint *url.URL, extraHeaders map[string]string, token, command string) (string, error) {
	if endpoint == nil || token == "" || command == "" {
		return "", errors.New("local Execd command configuration is incomplete")
	}
	target := *endpoint
	target.Path = strings.TrimRight(target.Path, "/") + "/command"
	target.RawPath = ""
	body, err := json.Marshal(struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		Background bool   `json:"background"`
		Timeout    int64  `json:"timeout"`
	}{Command: command, Cwd: "/workspace", Timeout: int64(30 * time.Second / time.Millisecond)})
	if err != nil {
		return "", errors.New("encode local Execd command")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("build local Execd command request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-EXECD-ACCESS-TOKEN", token)
	for key, value := range extraHeaders {
		if strings.EqualFold(key, "Host") || strings.HasPrefix(strings.ToLower(key), "proxy-") {
			return "", errors.New("OpenSandbox returned an unsafe Execd routing header")
		}
		if strings.EqualFold(key, "X-EXECD-ACCESS-TOKEN") {
			if value != token {
				return "", errors.New("OpenSandbox returned a conflicting Execd access token")
			}
			continue
		}
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("connect to loopback Execd endpoint")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local Execd command returned HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var stdout strings.Builder
	var completed bool
	var exitCode *int
	for scanner.Scan() {
		payload := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(payload, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
		} else if !strings.HasPrefix(payload, "{") {
			continue
		}
		var event struct {
			Type          string `json:"type"`
			Text          string `json:"text"`
			ExitCode      *int   `json:"exit_code"`
			ExitCodeCamel *int   `json:"exitCode"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return "", errors.New("decode local Execd stream event")
		}
		if event.Type == "execution_complete" {
			if event.ExitCode == nil {
				event.ExitCode = event.ExitCodeCamel
			}
		}
		switch event.Type {
		case "stdout":
			stdout.WriteString(event.Text)
		case "error":
			return "", errors.New("local Execd command failed")
		case "execution_complete":
			completed, exitCode = true, event.ExitCode
		}
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("read local Execd stream")
	}
	if !completed || (exitCode != nil && *exitCode != 0) {
		return "", errors.New("local Execd command did not report a successful exit")
	}
	return stdout.String(), nil
}

func newTestClient(baseURL string, registry prometheus.Registerer) *Client {
	client := NewClient(config.Profile{
		Name: "container", BaseURL: baseURL, APIKey: "osb-secret", RequestTimeout: time.Second,
		NetworkGuardPath: "/opt/cloudhost/network-policy.sh",
	}, nil, registry)
	client.checkNetworkPolicy = func(context.Context, string, string) error { return nil }
	return client
}
