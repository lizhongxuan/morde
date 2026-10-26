package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestDeleteVolumeUsesDockerUnixSocketAPI(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		wantErr  error
		wantText string
	}{
		{name: "deleted", status: http.StatusNoContent},
		{name: "already absent", status: http.StatusNotFound},
		{name: "in use", status: http.StatusConflict, body: `{"message":"volume is in use"}`, wantErr: app.ErrVolumeInUse},
		{name: "engine failure", status: http.StatusInternalServerError, body: `{"message":"daemon failed"}`, wantText: "HTTP 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			type observedRequest struct {
				method string
				path   string
				query  string
			}
			observed := make(chan observedRequest, 1)
			client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- observedRequest{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery}
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			}))

			err := client.DeleteVolume(context.Background(), "ws-w8f2k1q0")
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("DeleteVolume() error = %v, want errors.Is(_, %v)", err, test.wantErr)
				}
			} else if test.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantText) || !strings.Contains(err.Error(), "daemon failed") {
					t.Fatalf("DeleteVolume() error = %v, want status and bounded daemon message", err)
				}
			} else if err != nil {
				t.Fatalf("DeleteVolume() error = %v, want nil", err)
			}
			request := <-observed
			if request.method != http.MethodDelete || request.path != "/v1.43/volumes/ws-w8f2k1q0" || request.query != "" {
				t.Fatalf("Docker request = %s %s?%s, want DELETE /v1.43/volumes/ws-w8f2k1q0", request.method, request.path, request.query)
			}
		})
	}
}

func TestDeleteVolumeRejectsInvalidNameWithoutRequest(t *testing.T) {
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	for _, name := range []string{"", "../other", "a/b", "a?b", "a b", "-starts-with-dash"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			if err := client.DeleteVolume(context.Background(), name); err == nil {
				t.Fatalf("DeleteVolume(%q) error = nil, want validation error", name)
			}
		})
	}
}

func TestNewDockerValidatesUnixSocketURL(t *testing.T) {
	for _, host := range []string{
		"",
		"tcp://127.0.0.1:2375",
		"unix://relative/docker.sock",
		"unix:///var/run/docker.sock?query=1",
		"unix:///var/run/docker.sock#fragment",
		"unix:///",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := NewDocker(host, t.TempDir(), 1); err == nil {
				t.Fatalf("NewDocker(%q) error = nil, want validation error", host)
			}
		})
	}
	if _, err := NewDocker("unix:///var/run/docker.sock", "relative", 1); err == nil {
		t.Fatal("NewDocker(relative data directory) error = nil, want validation error")
	}
	if _, err := NewDocker("unix:///var/run/docker.sock", t.TempDir(), 0); err == nil {
		t.Fatal("NewDocker(zero reserve) error = nil, want validation error")
	}
}

func TestCheckCapacityVerifiesDockerRootAndFreeSpace(t *testing.T) {
	dockerRoot := t.TempDir()
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.43/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"DockerRootDir": dockerRoot})
	}))
	client.dataDir = dockerRoot
	if err := client.CheckCapacity(context.Background()); err != nil {
		t.Fatalf("CheckCapacity() = %v, want nil", err)
	}

	client.minFreeBytes = 1 << 62
	if err := client.CheckCapacity(context.Background()); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("CheckCapacity(low reserve) = %v, want capacity_exceeded", err)
	}

	client.minFreeBytes = 1
	client.dataDir = t.TempDir()
	if err := client.CheckCapacity(context.Background()); err == nil || domain.CodeOf(err) == domain.CodeCapacityExceeded {
		t.Fatalf("CheckCapacity(root mismatch) = %v, want configuration error", err)
	}
}

func TestCheckCapacityRejectsDockerEngineErrors(t *testing.T) {
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	if err := client.CheckCapacity(context.Background()); err == nil {
		t.Fatal("CheckCapacity() error = nil, want engine error")
	}
}

func TestDeleteVolumeHonorsCanceledContext(t *testing.T) {
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.DeleteVolume(ctx, "ws-w8f2k1q0")
	if err == nil {
		t.Fatal("DeleteVolume() error = nil, want canceled request error")
	}
}

func TestOpenLegacyVolumeReturnsReadOnlyRootForUnattachedLocalVolume(t *testing.T) {
	dockerRoot := t.TempDir()
	dockerRoot, err := filepath.EvalSymlinks(dockerRoot)
	if err != nil {
		t.Fatal(err)
	}
	volumeName := "ws-w8f2k1q0"
	dataDir := filepath.Join(dockerRoot, "volumes", volumeName, "_data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "README.md"), []byte("legacy workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newLegacyVolumeDockerForTest(t, dockerRoot, volumeName, dataDir, "local", nil)
	filesystem, closer, err := client.OpenLegacyVolume(context.Background(), volumeName)
	if err != nil {
		t.Fatalf("OpenLegacyVolume() = %v, want nil", err)
	}
	defer func() { _ = closer.Close() }()
	got, err := fs.ReadFile(filesystem, "README.md")
	if err != nil || string(got) != "legacy workspace" {
		t.Fatalf("fs.ReadFile() = %q, %v; want legacy workspace", got, err)
	}
	if _, err := fs.ReadFile(filesystem, "../outside"); err == nil {
		t.Fatal("fs.ReadFile(parent path) error = nil, want root escape rejection")
	}
}

func TestOpenLegacyVolumeRejectsUnsafeOrReferencedVolumes(t *testing.T) {
	for _, test := range []struct {
		name            string
		volumeName      string
		mountpoint      func(string, string) string
		driver          string
		volumeHTTPCode  int
		containerJSON   any
		inspectJSON     any
		makeEscapeLink  bool
		mismatchDataDir bool
	}{
		{name: "missing volume", volumeHTTPCode: http.StatusNotFound},
		{name: "name mismatch", volumeName: "another-volume"},
		{name: "non-local driver", driver: "nfs"},
		{name: "mountpoint outside data root", mountpoint: func(_, _ string) string { return "/outside/volume" }},
		{name: "mountpoint symlink escape", makeEscapeLink: true},
		{name: "referenced by stopped container mount", containerJSON: []any{map[string]string{"Id": "container-1"}}, inspectJSON: map[string]any{"Mounts": []any{map[string]string{"Type": "volume", "Name": "ws-w8f2k1q0"}}}},
		{name: "referenced in host config", containerJSON: []any{map[string]string{"Id": "container-1"}}, inspectJSON: map[string]any{"HostConfig": map[string]any{"Mounts": []any{map[string]string{"Type": "volume", "Source": "ws-w8f2k1q0"}}}}},
		{name: "referenced by legacy bind", containerJSON: []any{map[string]string{"Id": "container-1"}}, inspectJSON: map[string]any{"HostConfig": map[string]any{"Binds": []string{"ws-w8f2k1q0:/workspace:rw"}}}},
		{name: "docker root mismatch", mismatchDataDir: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dockerRoot := t.TempDir()
			dockerRoot, err := filepath.EvalSymlinks(dockerRoot)
			if err != nil {
				t.Fatal(err)
			}
			volumeName := "ws-w8f2k1q0"
			dataDir := filepath.Join(dockerRoot, "volumes", volumeName, "_data")
			if err := os.MkdirAll(filepath.Dir(dataDir), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if test.makeEscapeLink {
				if err := os.Symlink(outside, dataDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			inspectName := test.volumeName
			if inspectName == "" {
				inspectName = volumeName
			}
			driver := test.driver
			if driver == "" {
				driver = "local"
			}
			mountpoint := dataDir
			if test.mountpoint != nil {
				mountpoint = test.mountpoint(dockerRoot, dataDir)
			}
			client := newLegacyVolumeDockerWithResponses(t, dockerRoot, inspectName, mountpoint, driver, test.volumeHTTPCode, test.containerJSON, test.inspectJSON)
			if test.mismatchDataDir {
				client.dataDir = t.TempDir()
			}
			filesystem, closer, err := client.OpenLegacyVolume(context.Background(), volumeName)
			if err == nil {
				_ = closer.Close()
				t.Fatalf("OpenLegacyVolume() = %v, want rejection", filesystem)
			}
		})
	}
}

func TestOpenLegacyVolumeRejectsInvalidNameAndCanceledContext(t *testing.T) {
	requests := 0
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	for _, name := range []string{"", "../other", "a/b", "a?b", "-starts-with-dash"} {
		if _, _, err := client.OpenLegacyVolume(context.Background(), name); err == nil {
			t.Errorf("OpenLegacyVolume(%q) error = nil, want input validation error", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.OpenLegacyVolume(ctx, "ws-w8f2k1q0"); err == nil {
		t.Fatal("OpenLegacyVolume(canceled context) error = nil, want context error")
	}
	if requests != 0 {
		t.Fatalf("Docker requests = %d, want none for invalid inputs and canceled context", requests)
	}
}

func TestOpenLegacyVolumePropagatesCancellationDuringEngineRequest(t *testing.T) {
	started := make(chan struct{})
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := client.OpenLegacyVolume(ctx, "ws-w8f2k1q0")
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("OpenLegacyVolume() error = nil, want cancellation error")
	}
}

func newLegacyVolumeDockerForTest(t *testing.T, dockerRoot, name, mountpoint, driver string, containers []any) *Docker {
	return newLegacyVolumeDockerWithResponses(t, dockerRoot, name, mountpoint, driver, http.StatusOK, containers, nil)
}

func newLegacyVolumeDockerWithResponses(t *testing.T, dockerRoot, name, mountpoint, driver string, volumeStatus int, containers, inspections any) *Docker {
	t.Helper()
	if volumeStatus == 0 {
		volumeStatus = http.StatusOK
	}
	ids := []any{}
	if containers != nil {
		ids = containers.([]any)
	}
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/info":
			_ = json.NewEncoder(w).Encode(map[string]string{"DockerRootDir": dockerRoot})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/volumes/ws-w8f2k1q0":
			if volumeStatus != http.StatusOK {
				http.Error(w, "missing", volumeStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Name": name, "Driver": driver, "Mountpoint": mountpoint})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/containers/json":
			if r.URL.Query().Get("all") != "1" {
				http.Error(w, "all containers must be requested", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(ids)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.43/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			if inspections == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode(inspections)
		default:
			http.NotFound(w, r)
		}
	}))
	client.dataDir = dockerRoot
	return client
}

func newUnixDockerForTest(t *testing.T, handler http.Handler) *Docker {
	t.Helper()
	dataDir := t.TempDir()
	shortDir, err := os.MkdirTemp("/tmp", "ch-")
	if err != nil {
		t.Fatalf("make short Unix socket directory: %v", err)
	}
	socket := filepath.Join(shortDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.RemoveAll(shortDir)
		t.Fatalf("listen on Unix socket: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = os.RemoveAll(shortDir)
	})
	client, err := NewDocker((&url.URL{Scheme: "unix", Path: socket}).String(), dataDir, 1)
	if err != nil {
		t.Fatalf("NewDocker(): %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
