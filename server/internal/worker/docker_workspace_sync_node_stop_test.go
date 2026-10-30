package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"morde/server/internal/dockerapi"
)

const (
	workspaceSyncStopTestOwner = "owner-stop-test"
	workspaceSyncStopTestImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type workspaceSyncStopDockerAPI struct {
	mu           sync.Mutex
	ownerHash    string
	containerID  string
	container    string
	network      string
	image        string
	managed      string
	labelOwner   string
	imageLabel   string
	networkMode  string
	networkOwner string
	exists       bool
	running      bool
	restarting   bool
	paused       bool
	leaveActive  bool
	stopStatus   int
	stopCalls    int
}

func newWorkspaceSyncStopDockerAPI(ownerID, image string) *workspaceSyncStopDockerAPI {
	ownerHash := sha256.Sum256([]byte(ownerID))
	ownerHashHex := hex.EncodeToString(ownerHash[:])
	return &workspaceSyncStopDockerAPI{
		ownerHash: ownerHashHex, containerID: strings.Repeat("a", 64),
		container: "morde-sync-" + ownerHashHex[:32], network: "morde-sync-net-" + ownerHashHex[:32],
		image: image, managed: "true", labelOwner: ownerHashHex, imageLabel: image,
		networkMode: "morde-sync-net-" + ownerHashHex[:32], networkOwner: ownerHashHex,
		exists: true, running: true, stopStatus: http.StatusNoContent,
	}
}

func (f *workspaceSyncStopDockerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/version" {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.44", "MinAPIVersion": "1.44"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.44")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		if !f.exists || (key != f.container && key != f.containerID) {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.containerInspect())
	case r.Method == http.MethodGet && path == "/networks/"+f.network:
		_ = json.NewEncoder(w).Encode(f.networkInspect())
	case r.Method == http.MethodPost && path == "/containers/"+f.containerID+"/stop":
		f.stopCalls++
		if !f.leaveActive {
			f.running, f.restarting, f.paused = false, false, false
		}
		status := f.stopStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	default:
		http.NotFound(w, r)
	}
}

func (f *workspaceSyncStopDockerAPI) containerInspect() map[string]any {
	return map[string]any{
		"Id": f.containerID, "Name": "/" + f.container,
		"Config": map[string]any{"Image": f.image, "Labels": map[string]string{
			dockerWorkspaceSyncLabelManaged: f.managed,
			dockerWorkspaceSyncLabelOwner:   f.labelOwner,
			dockerWorkspaceSyncLabelImage:   f.imageLabel,
		}},
		"HostConfig": map[string]any{"NetworkMode": f.networkMode},
		"State":      map[string]any{"Running": f.running, "Restarting": f.restarting, "Paused": f.paused},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			f.network: map[string]any{"Aliases": []string{dockerWorkspaceSyncContainerAlias}, "IPAddress": "172.18.0.2"},
		}},
	}
}

func (f *workspaceSyncStopDockerAPI) networkInspect() map[string]any {
	return map[string]any{
		"Id": strings.Repeat("b", 64), "Name": f.network, "Scope": "local", "Driver": "bridge",
		"Internal": false, "EnableIPv6": false, "Attachable": false, "Ingress": false, "ConfigOnly": false,
		"Options": map[string]string{
			"com.docker.network.bridge.name":                 dockerWorkspaceSyncBridgeName(f.ownerHash),
			"com.docker.network.bridge.enable_ip_masquerade": "true",
		},
		"IPAM":       map[string]any{"Driver": "default", "Options": map[string]string{}},
		"Labels":     map[string]string{dockerWorkspaceSyncLabelManaged: "true", dockerWorkspaceSyncLabelOwner: f.networkOwner},
		"Containers": map[string]any{f.containerID: map[string]string{"Name": f.container}},
	}
}

type workspaceSyncStopDockerTransport struct {
	base *url.URL
	next http.RoundTripper
}

func (t workspaceSyncStopDockerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	target := *request.URL
	target.Scheme, target.Host = t.base.Scheme, t.base.Host
	cloned.URL = &target
	cloned.Host = t.base.Host
	return t.next.RoundTrip(cloned)
}

func newWorkspaceSyncStopSupervisor(t *testing.T, fake *workspaceSyncStopDockerAPI) (*DockerWorkspaceSyncNodeSupervisor, func()) {
	t.Helper()
	server := httptest.NewServer(fake)
	base, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client := &http.Client{Transport: workspaceSyncStopDockerTransport{base: base, next: server.Client().Transport}}
	return &DockerWorkspaceSyncNodeSupervisor{
		options: DockerWorkspaceSyncNodeSupervisorOptions{ImageDigest: workspaceSyncStopTestImage, RelayContainerName: "morde-sync-relay"},
		client:  client, api: dockerapi.NewEndpointResolver(client),
	}, server.Close
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerStopsAndIsIdempotent(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err != nil {
		t.Fatalf("StopOwner() error = %v", err)
	}
	fake.mu.Lock()
	stopped, stopCalls := !fake.running && !fake.restarting && !fake.paused, fake.stopCalls
	fake.mu.Unlock()
	if !stopped || stopCalls != 1 {
		t.Fatalf("after StopOwner(): stopped = %v, stop calls = %d; want true, 1", stopped, stopCalls)
	}
	if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err != nil {
		t.Fatalf("second StopOwner() error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 1 {
		t.Fatalf("second StopOwner() issued another stop: calls = %d", fake.stopCalls)
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerMissingContainerIsIdempotent(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	fake.exists = false
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err != nil {
		t.Fatalf("StopOwner() for a missing container error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 0 {
		t.Fatalf("StopOwner() issued %d stop calls for a missing container", fake.stopCalls)
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerAlreadyStoppedIsIdempotent(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	fake.running = false
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err != nil {
		t.Fatalf("StopOwner() for an already stopped container error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 0 {
		t.Fatalf("StopOwner() issued %d stop calls for an already stopped container", fake.stopCalls)
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerFailsClosedOnIdentityMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workspaceSyncStopDockerAPI)
	}{
		{name: "managed label", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.managed = "false" }},
		{name: "owner label", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.labelOwner = strings.Repeat("c", 64) }},
		{name: "image label", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.imageLabel = "sha256:" + strings.Repeat("c", 64) }},
		{name: "container image", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.image = "sha256:" + strings.Repeat("c", 64) }},
		{name: "network mode", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.networkMode = "bridge" }},
		{name: "network owner", mutate: func(fake *workspaceSyncStopDockerAPI) { fake.networkOwner = strings.Repeat("c", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
			test.mutate(fake)
			supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
			defer cleanup()

			if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err == nil {
				t.Fatal("StopOwner() error = nil, want identity mismatch")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.stopCalls != 0 {
				t.Fatalf("StopOwner() issued %d stop calls for a mismatched node", fake.stopCalls)
			}
		})
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerConfirmsStoppedState(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	fake.leaveActive = true
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner)
	if err == nil {
		t.Fatal("StopOwner() error = nil when Docker acknowledged stop but node remains active")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", fake.stopCalls)
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerAcceptsStoppedAfterStopResponseError(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	fake.stopStatus = http.StatusNotModified
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	if err := supervisor.StopOwner(context.Background(), workspaceSyncStopTestOwner); err != nil {
		t.Fatalf("StopOwner() error = %v after inspect confirms stopped state", err)
	}
}

func TestDockerWorkspaceSyncNodeSupervisorStopOwnerRejectsInvalidOwner(t *testing.T) {
	fake := newWorkspaceSyncStopDockerAPI(workspaceSyncStopTestOwner, workspaceSyncStopTestImage)
	supervisor, cleanup := newWorkspaceSyncStopSupervisor(t, fake)
	defer cleanup()

	for _, ownerID := range []string{"", " owner", "owner\ninvalid", strings.Repeat("x", 257)} {
		if err := supervisor.StopOwner(context.Background(), ownerID); err == nil {
			t.Errorf("StopOwner(%q) error = nil, want invalid owner", ownerID)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 0 {
		t.Fatalf("StopOwner() issued %d stop calls for invalid owners", fake.stopCalls)
	}
}
