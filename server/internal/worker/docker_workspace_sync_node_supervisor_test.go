package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	workspaceSyncTestOwnerA     = "account-a"
	workspaceSyncTestOwnerB     = "account-b"
	workspaceSyncTestKeyA       = "api-key-for-account-a-0123456789"
	workspaceSyncTestKeyB       = "api-key-for-account-b-9876543210"
	workspaceSyncTestRelayToken = "ssssssssssssssssssssssssssssssss"
)

type workspaceSyncFakeNetwork struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Scope      string            `json:"Scope"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Options    map[string]string `json:"Options"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Attachable bool              `json:"Attachable"`
	Labels     map[string]string `json:"Labels"`
	IPAM       struct {
		Driver string `json:"Driver"`
	} `json:"IPAM"`
	Containers map[string]workspaceSyncEndpoint `json:"Containers"`
}

type workspaceSyncEndpoint struct {
	Name string `json:"Name"`
}

type workspaceSyncFakeContainer struct {
	ID        string
	Name      string
	Payload   map[string]any
	Running   bool
	IPAddress string
}

type workspaceSyncFakeEngine struct {
	mu                  sync.Mutex
	image               string
	relayImage          string
	relayName           string
	relayID             string
	relayStateRoot      string
	relayToken          string
	relayPublicAddress  string
	relayHostIP         string
	relayHostPort       string
	networks            map[string]*workspaceSyncFakeNetwork
	networksByID        map[string]*workspaceSyncFakeNetwork
	containers          map[string]*workspaceSyncFakeContainer
	containersByID      map[string]*workspaceSyncFakeContainer
	networkCreates      int
	containerCreates    int
	starts              int
	missingRelay        bool
	publicRelay         bool
	createErrorBody     string
	createErrorCode     int
	lastNetworkPayload  map[string]any
	lastContainerByName map[string]map[string]any
}

func newWorkspaceSyncFakeEngine(image, relayImage, relayName string) *workspaceSyncFakeEngine {
	return &workspaceSyncFakeEngine{
		image: image, relayImage: relayImage, relayName: relayName, relayID: strings.Repeat("d", 64),
		relayToken: workspaceSyncTestRelayToken, relayPublicAddress: "8.8.8.8:22067", relayHostIP: "172.18.141.222", relayHostPort: "22067",
		networks: make(map[string]*workspaceSyncFakeNetwork), networksByID: make(map[string]*workspaceSyncFakeNetwork),
		containers: make(map[string]*workspaceSyncFakeContainer), containersByID: make(map[string]*workspaceSyncFakeContainer),
		lastContainerByName: make(map[string]map[string]any),
	}
}

func (f *workspaceSyncFakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/version" {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.44", "MinAPIVersion": "1.44"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.44")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		image := f.image
		if path == "/images/"+f.relayImage+"/json" {
			image = f.relayImage
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": image, "RepoDigests": []string{image},
			"Config": map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}, "Volumes": nil},
		})
	case r.Method == http.MethodGet && path == "/containers/"+f.relayName+"/json":
		if f.missingRelay {
			http.NotFound(w, r)
			return
		}
		relayCmd := []string{
			"-listen", ":22067", "-keys", "/var/lib/morde-relay", "-token", f.relayToken, "-status-srv=", "-pools=",
			"-ext-address", f.relayPublicAddress, "-provided-by", "Morde",
		}
		if f.publicRelay {
			relayCmd = append(relayCmd, "-pools=relays.syncthing.net")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": f.relayID, "Image": f.relayImage, "Name": "/" + f.relayName,
			"Config": map[string]any{
				"Image": f.relayImage, "User": "65532:65532", "Cmd": relayCmd,
				"Volumes": nil, "ExposedPorts": map[string]any{"22067/tcp": map[string]any{}},
			},
			"Mounts": []map[string]any{{"Type": "bind", "Source": f.relayStateRoot, "Destination": "/var/lib/morde-relay", "RW": true}},
			"HostConfig": map[string]any{
				"NetworkMode": "morde_workspace_sync_relay", "Privileged": false, "ReadonlyRootfs": true,
				"CapAdd": []string{}, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
				"Memory": 256 << 20, "NanoCpus": 250_000_000, "PidsLimit": 64,
				"RestartPolicy": map[string]string{"Name": "unless-stopped"},
				"PortBindings":  map[string]any{"22067/tcp": []any{map[string]string{"HostIp": f.relayHostIP, "HostPort": f.relayHostPort}}},
			},
			"State":           map[string]any{"Running": true, "Restarting": false, "Paused": false},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}},
		})
	case r.Method == http.MethodPost && path == "/networks/create":
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name := payload["Name"].(string)
		id := strings.Repeat("c", 64)
		if f.networkCreates > 0 {
			id = strings.Repeat("f", 64)
		}
		network := &workspaceSyncFakeNetwork{
			ID: id, Name: name, Scope: "local", Driver: payload["Driver"].(string), Internal: payload["Internal"].(bool),
			EnableIPv6: payload["EnableIPv6"].(bool), Attachable: payload["Attachable"].(bool), Options: stringMap(payload["Options"]),
			Labels: stringMap(payload["Labels"]), Containers: make(map[string]workspaceSyncEndpoint),
		}
		network.IPAM.Driver = "default"
		f.networks[name], f.networksByID[id] = network, network
		f.networkCreates++
		f.lastNetworkPayload = payload
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/networks/"):
		ref := strings.TrimPrefix(path, "/networks/")
		network := f.networks[ref]
		if network == nil {
			network = f.networksByID[ref]
		}
		if network == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(network)
	case r.Method == http.MethodPost && path == "/containers/create":
		if f.createErrorCode != 0 {
			w.WriteHeader(f.createErrorCode)
			_, _ = io.WriteString(w, f.createErrorBody)
			return
		}
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name := r.URL.Query().Get("name")
		id := strings.Repeat("b", 64)
		if f.containerCreates > 0 {
			id = strings.Repeat("e", 64)
		}
		container := &workspaceSyncFakeContainer{ID: id, Name: name, Payload: payload, IPAddress: "172.30.0.2"}
		f.containers[name], f.containersByID[id] = container, container
		f.containerCreates++
		f.lastContainerByName[name] = cloneJSONMap(payload)
		f.attachNodeToNetwork(container)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		container := f.containers[ref]
		if container == nil {
			container = f.containersByID[ref]
		}
		if container == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.containerInspect(container))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/start"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/start")
		container := f.containersByID[ref]
		if container == nil {
			http.NotFound(w, r)
			return
		}
		container.Running = true
		f.starts++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *workspaceSyncFakeEngine) attachNodeToNetwork(container *workspaceSyncFakeContainer) {
	networking := container.Payload["NetworkingConfig"].(map[string]any)
	endpoints := networking["EndpointsConfig"].(map[string]any)
	for networkName := range endpoints {
		network := f.networks[networkName]
		network.Containers[container.ID] = workspaceSyncEndpoint{Name: container.Name}
	}
}

func (f *workspaceSyncFakeEngine) containerInspect(container *workspaceSyncFakeContainer) map[string]any {
	payload := container.Payload
	host := cloneMap(payload["HostConfig"].(map[string]any))
	imageEnv := []string{"PATH=/usr/bin:/bin"}
	for _, value := range payload["Env"].([]any) {
		imageEnv = append(imageEnv, value.(string))
	}
	config := map[string]any{
		"Image": payload["Image"], "User": payload["User"],
		"Env":    imageEnv,
		"Labels": payload["Labels"], "ExposedPorts": payload["ExposedPorts"],
	}
	mountSpecs := host["Mounts"].([]any)
	mounts := make([]map[string]any, 0, len(mountSpecs))
	for _, raw := range mountSpecs {
		spec := raw.(map[string]any)
		mounts = append(mounts, map[string]any{
			"Type": spec["Type"], "Source": spec["Source"], "Destination": spec["Target"], "RW": true,
		})
	}
	ports := map[string][]map[string]string{dockerWorkspaceSyncAPIPort: nil}
	state := map[string]any{"Running": container.Running, "Restarting": false, "Paused": false, "Pid": 4321}
	networks := make(map[string]any)
	endpoints := container.Payload["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	for networkName, network := range f.networks {
		if _, ok := network.Containers[container.ID]; ok {
			endpointConfig := endpoints[networkName].(map[string]any)
			networks[networkName] = map[string]any{"Aliases": endpointConfig["Aliases"], "IPAddress": container.IPAddress}
		}
	}
	return map[string]any{
		"Id": container.ID, "Name": "/" + container.Name,
		"Config": config, "HostConfig": host, "State": state,
		"Mounts":          mounts,
		"NetworkSettings": map[string]any{"Ports": ports, "Networks": networks},
	}
}

func (f *workspaceSyncFakeEngine) stopNode() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.containers {
		container.Running = false
	}
}

func (f *workspaceSyncFakeEngine) tamperOwnerLabel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.containers {
		labels := container.Payload["Labels"].(map[string]any)
		labels[dockerWorkspaceSyncLabelOwner] = strings.Repeat("0", 64)
	}
}

func (f *workspaceSyncFakeEngine) setCreateError(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErrorCode, f.createErrorBody = status, body
}

func (f *workspaceSyncFakeEngine) setMissingRelay(missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missingRelay = missing
}

func (f *workspaceSyncFakeEngine) setRelayImage(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relayImage = image
}

func stringMap(value any) map[string]string {
	result := make(map[string]string)
	for key, raw := range value.(map[string]any) {
		result[key] = raw.(string)
	}
	return result
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneJSONMap(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

type workspaceSyncFixture struct {
	root       string
	socket     string
	image      string
	relayName  string
	stateRoot  string
	workRoot   string
	engine     *workspaceSyncFakeEngine
	server     *httptest.Server
	options    DockerWorkspaceSyncNodeSupervisorOptions
	apiKeyBase string
}

func newWorkspaceSyncFixture(t *testing.T) *workspaceSyncFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "morde-workspace-sync-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	image := "local/morde-syncthing@sha256:" + strings.Repeat("a", 64)
	relayImage := "local/morde-relay@sha256:" + strings.Repeat("9", 64)
	relay := "morde-syncthing-relay"
	engine := newWorkspaceSyncFakeEngine(image, relayImage, relay)
	relayStateRoot := filepath.Join(root, "relay-state")
	engine.relayStateRoot = relayStateRoot
	server := httptest.NewUnstartedServer(engine)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	options := DockerWorkspaceSyncNodeSupervisorOptions{
		ImageDigest: image, RelayImageDigest: relayImage, DockerSocket: socket,
		RelayPublicAddress: "8.8.8.8:22067", RelayBindAddress: "172.18.141.222", RelayToken: workspaceSyncTestRelayToken,
		RelayStateRoot: relayStateRoot,
		StateRoot:      filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "current-workspaces"),
		RelayContainerName: relay,
		Resources:          DockerWorkspaceSyncNodeResourceLimits{MemoryBytes: 256 << 20, NanoCPUs: 250_000_000, PIDsLimit: 64},
	}
	return &workspaceSyncFixture{root: root, socket: socket, image: image, relayName: relay,
		stateRoot: options.StateRoot, workRoot: options.WorkspaceRoot, engine: engine, server: server,
		options: options, apiKeyBase: "api-key-secret-not-for-errors"}
}

func (f *workspaceSyncFixture) supervisor(t *testing.T) *DockerWorkspaceSyncNodeSupervisor {
	t.Helper()
	supervisor, err := NewDockerWorkspaceSyncNodeSupervisor(f.options)
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

func TestDockerWorkspaceSyncNodeEnsureCreatesIsolatedNode(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	got, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerID != strings.Repeat("b", 64) || got.APIURL != "http://172.30.0.2:8384" {
		t.Fatalf("Ensure result = %#v", got)
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 1 || f.engine.containerCreates != 1 || f.engine.starts != 1 {
		t.Fatalf("create counts = network %d, container %d, start %d", f.engine.networkCreates, f.engine.containerCreates, f.engine.starts)
	}
	ownerHashSum := sha256.Sum256([]byte(workspaceSyncTestOwnerA))
	ownerHash := fmt.Sprintf("%x", ownerHashSum[:])
	name := "morde-sync-" + ownerHash[:32]
	created := f.engine.lastContainerByName[name]
	if created == nil {
		t.Fatal("container create request was not captured")
	}
	if created["Image"] != f.image || created["User"] == "0" || created["User"] == "0:0" {
		t.Fatalf("image or user = %v / %v", created["Image"], created["User"])
	}
	env := created["Env"].([]any)
	if !slicesContainsString(env, "STGUIAPIKEY="+workspaceSyncTestKeyA) || slicesContainsString(env, "STGUIAPIKEY="+workspaceSyncTestKeyB) {
		t.Fatalf("per-owner API key environment = %#v", env)
	}
	host := created["HostConfig"].(map[string]any)
	if host["RestartPolicy"].(map[string]any)["Name"] != "unless-stopped" {
		t.Fatalf("revoked node may restart automatically after Docker restart: %#v", host["RestartPolicy"])
	}
	if host["NetworkMode"] != "morde-sync-net-"+ownerHash[:32] || host["ReadonlyRootfs"] != true || host["Privileged"] != false ||
		host["Memory"] != float64(256<<20) || host["MemorySwap"] != float64(256<<20) || host["NanoCpus"] != float64(250_000_000) ||
		host["PidsLimit"] != float64(64) || host["AutoRemove"] != false {
		t.Fatalf("host isolation config = %#v", host)
	}
	if !slicesContainsString(host["CapDrop"].([]any), "ALL") || !slicesContainsString(host["SecurityOpt"].([]any), "no-new-privileges:true") {
		t.Fatalf("capabilities/security options = %#v / %#v", host["CapDrop"], host["SecurityOpt"])
	}
	mounts := host["Mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("mounts = %#v", mounts)
	}
	wantSources := map[string]string{
		dockerWorkspaceSyncConfigPath: filepath.Join(mustEvalSymlinks(t, f.stateRoot), ownerHash),
		dockerWorkspaceSyncFilesPath:  filepath.Join(mustEvalSymlinks(t, f.workRoot), ownerHash),
	}
	for _, raw := range mounts {
		mount := raw.(map[string]any)
		if mount["Type"] != "bind" || wantSources[mount["Target"].(string)] != mount["Source"] || mount["ReadOnly"] != false {
			t.Fatalf("mount config = %#v", mount)
		}
		delete(wantSources, mount["Target"].(string))
	}
	if len(wantSources) != 0 {
		t.Fatalf("missing owner-scoped mounts: %#v", wantSources)
	}
	if _, ok := host["PortBindings"]; ok {
		t.Fatalf("container unexpectedly publishes host ports: %#v", host["PortBindings"])
	}
	if f.engine.lastNetworkPayload["Internal"] != false || f.engine.lastNetworkPayload["Driver"] != "bridge" || f.engine.lastNetworkPayload["Attachable"] != false {
		t.Fatalf("network config = %#v", f.engine.lastNetworkPayload)
	}
	options := stringMap(f.engine.lastNetworkPayload["Options"])
	if options["com.docker.network.bridge.name"] != dockerWorkspaceSyncBridgeName(ownerHash) ||
		options["com.docker.network.bridge.enable_ip_masquerade"] != "true" {
		t.Fatalf("network bridge options = %#v", options)
	}
	for _, network := range f.engine.networks {
		if _, ok := network.Containers[f.engine.relayID]; ok {
			t.Fatal("shared relay must not be attached to an account network")
		}
	}
}

func TestWorkspaceSyncNodeNetworkRequiresDedicatedMasqueradedBridge(t *testing.T) {
	ownerHash := strings.Repeat("b", 64)
	bridgeName := dockerWorkspaceSyncBridgeName(ownerHash)
	value := dockerWorkspaceSyncNetwork{
		ID: strings.Repeat("a", 64), Name: "morde-sync-net-test", Scope: "local", Driver: "bridge", Internal: false,
		Labels: map[string]string{dockerWorkspaceSyncLabelManaged: "true", dockerWorkspaceSyncLabelOwner: strings.Repeat("b", 64)},
	}
	value.IPAM.Driver = "default"
	value.Options = map[string]string{
		"com.docker.network.bridge.name":                 bridgeName,
		"com.docker.network.bridge.enable_ip_masquerade": "true",
	}
	if !validDockerWorkspaceSyncNetwork(value, value.Name, ownerHash, bridgeName) {
		t.Fatal("expected account bridge configuration was rejected")
	}
	for _, mutate := range []func(){
		func() { value.Internal = true },
		func() { value.Options["com.docker.network.bridge.enable_ip_masquerade"] = "false" },
		func() { value.Options["com.docker.network.bridge.name"] = "untrusted0" },
	} {
		value.Internal = false
		value.Options = map[string]string{
			"com.docker.network.bridge.name":                 bridgeName,
			"com.docker.network.bridge.enable_ip_masquerade": "true",
		}
		mutate()
		if validDockerWorkspaceSyncNetwork(value, value.Name, ownerHash, bridgeName) {
			t.Error("unsafe account network configuration was accepted")
		}
	}
}

func TestDockerWorkspaceSyncNodeUsesAccountPrivateAPIAddress(t *testing.T) {
	const networkName = "morde-sync-net-test"
	for _, address := range []string{"10.0.0.2", "172.19.0.2", "192.168.1.2"} {
		value := dockerWorkspaceSyncInspect{}
		value.NetworkSettings.Networks = map[string]dockerWorkspaceSyncNetworkEndpoint{
			networkName: {IPAddress: address},
		}
		if got, ok := dockerWorkspaceSyncPrivateIPv4(value, networkName); !ok || got != address {
			t.Errorf("private API address = %q, %v; want %q", got, ok, address)
		}
	}
	for _, address := range []string{"", "8.8.8.8", "127.0.0.1", "169.254.1.2", "224.0.0.1", "::1"} {
		value := dockerWorkspaceSyncInspect{}
		value.NetworkSettings.Networks = map[string]dockerWorkspaceSyncNetworkEndpoint{
			networkName: {IPAddress: address},
		}
		if got, ok := dockerWorkspaceSyncPrivateIPv4(value, networkName); ok {
			t.Errorf("unsafe API address %q was accepted as %q", address, got)
		}
	}
	if !dockerWorkspaceSyncHasNoPublishedPorts(nil, nil) ||
		!dockerWorkspaceSyncHasNoPublishedPorts(nil, map[string][]dockerWorkspaceSyncPort{dockerWorkspaceSyncAPIPort: nil}) {
		t.Fatal("unpublished exposed API port was rejected")
	}
	if dockerWorkspaceSyncHasNoPublishedPorts(
		map[string][]dockerWorkspaceSyncPort{dockerWorkspaceSyncAPIPort: nil}, nil,
	) || dockerWorkspaceSyncHasNoPublishedPorts(nil, map[string][]dockerWorkspaceSyncPort{
		dockerWorkspaceSyncAPIPort: {{HostIP: "127.0.0.1", HostPort: "49152"}},
	}) {
		t.Fatal("published API port was accepted")
	}
}

func TestWorkspaceSyncNodeContainerAllowsOnlyPinnedImageLabels(t *testing.T) {
	imageLabels := map[string]string{"org.opencontainers.image.licenses": "MPL-2.0"}
	labels := map[string]string{
		"org.opencontainers.image.licenses": "MPL-2.0",
		dockerWorkspaceSyncLabelManaged:     "true",
		dockerWorkspaceSyncLabelOwner:       "owner-hash",
		dockerWorkspaceSyncLabelImage:       "sha256:image",
	}
	if !validDockerWorkspaceSyncContainerLabels(labels, imageLabels, "owner-hash", "sha256:image") {
		t.Fatal("pinned image labels were rejected")
	}
	labels["unexpected"] = "value"
	if validDockerWorkspaceSyncContainerLabels(labels, imageLabels, "owner-hash", "sha256:image") {
		t.Fatal("unexpected container label was accepted")
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDockerWorkspaceSyncNodeEnsureReusesPersistentContainerAfterRestart(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	first, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent Ensure = %#v, first %#v", second, first)
	}
	f.engine.stopNode()
	third, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("restarted Ensure = %#v, first %#v", third, first)
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.containerCreates != 1 || f.engine.starts != 2 {
		t.Fatalf("create/start counts = %d/%d", f.engine.containerCreates, f.engine.starts)
	}
}

func TestDockerWorkspaceSyncNodeEnsureRejectsWrongOwnerContainerAndKey(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	supervisor := f.supervisor(t)
	if _, err := supervisor.Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyB); err == nil || strings.Contains(err.Error(), workspaceSyncTestKeyA) || strings.Contains(err.Error(), workspaceSyncTestKeyB) {
		t.Fatalf("wrong API key error = %v", err)
	}
	f.engine.tamperOwnerLabel()
	if _, err := supervisor.Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("wrong-owner container was accepted")
	}
	other, err := supervisor.Ensure(context.Background(), workspaceSyncTestOwnerB, workspaceSyncTestKeyB)
	if err != nil {
		t.Fatal(err)
	}
	if other.APIURL != "http://172.30.0.2:8384" {
		t.Fatalf("second owner API URL = %q", other.APIURL)
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.containerCreates != 2 || len(f.engine.networks) != 2 {
		t.Fatalf("owner isolation counts = containers %d, networks %d", f.engine.containerCreates, len(f.engine.networks))
	}
}

func TestDockerWorkspaceSyncNodeMissingRelayFailsClosed(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.setMissingRelay(true)
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("missing relay was accepted")
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
		t.Fatalf("created without relay: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
	}
}

func TestDockerWorkspaceSyncNodeRejectsPublicPoolRelay(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.publicRelay = true
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("relay configured for the public pool was accepted")
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
		t.Fatalf("created with an unsafe relay: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
	}
}

func TestDockerWorkspaceSyncNodeRejectsRelayWithWrongToken(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.relayToken = "another-token-that-does-not-match-configured-secret"
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("relay using a different shared token was accepted")
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
		t.Fatalf("created despite relay token mismatch: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
	}
}

func TestDockerWorkspaceSyncNodeRejectsRelayWithWrongPublicAddress(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.relayPublicAddress = "8.8.4.4:22067"
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("relay advertising a different public address was accepted")
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
		t.Fatalf("created despite relay public-address mismatch: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
	}
}

func TestDockerWorkspaceSyncNodeRejectsRelayHostBindingMismatch(t *testing.T) {
	for _, test := range []struct {
		name     string
		hostIP   string
		hostPort string
	}{
		{name: "wrong port", hostIP: "172.18.141.222", hostPort: "22068"},
		{name: "unexpected loopback bind", hostIP: "127.0.0.1", hostPort: "22067"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWorkspaceSyncFixture(t)
			f.engine.relayHostIP = test.hostIP
			f.engine.relayHostPort = test.hostPort
			if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
				t.Fatal("relay with a mismatched advertised endpoint was accepted")
			}
			f.engine.mu.Lock()
			defer f.engine.mu.Unlock()
			if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
				t.Fatalf("created with a mismatched relay endpoint: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
			}
		})
	}
}

func TestWorkspaceSyncRelayHostBindingMatchesAdvertisedAddress(t *testing.T) {
	for _, test := range []struct {
		name        string
		binding     dockerPortBinding
		address     string
		bindAddress string
		want        bool
	}{
		{name: "wildcard bind", binding: dockerPortBinding{HostIP: "0.0.0.0", HostPort: "22067"}, address: "8.8.8.8:22067", bindAddress: "0.0.0.0", want: false},
		{name: "NAT to private host IPv4", binding: dockerPortBinding{HostIP: "172.18.141.222", HostPort: "22067"}, address: "120.25.178.93:22067", bindAddress: "172.18.141.222", want: true},
		{name: "private bind without NAT advertisement", binding: dockerPortBinding{HostIP: "172.18.141.222", HostPort: "22067"}, address: "relay.example.test:22067", bindAddress: "172.18.141.222", want: false},
		{name: "unexpected loopback bind", binding: dockerPortBinding{HostIP: "127.0.0.1", HostPort: "22067"}, address: "192.0.2.8:22067", bindAddress: "127.0.0.1", want: false},
		{name: "wrong port", binding: dockerPortBinding{HostIP: "172.18.141.222", HostPort: "22068"}, address: "8.8.8.8:22067", bindAddress: "172.18.141.222", want: false},
		{name: "wrong explicit IPv4", binding: dockerPortBinding{HostIP: "172.18.141.223", HostPort: "22067"}, address: "8.8.8.8:22067", bindAddress: "172.18.141.222", want: false},
		{name: "unexpected configured bind address", binding: dockerPortBinding{HostIP: "0.0.0.0", HostPort: "22067"}, address: "8.8.8.8:22067", bindAddress: "0.0.0.0", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validWorkspaceSyncRelayHostBinding(test.binding, test.address, test.bindAddress); got != test.want {
				t.Fatalf("validWorkspaceSyncRelayHostBinding(%#v, %q, %q) = %t; want %t", test.binding, test.address, test.bindAddress, got, test.want)
			}
		})
	}
}

func TestDockerWorkspaceSyncNodeRequiresRelayToken(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.options.RelayToken = ""
	if _, err := NewDockerWorkspaceSyncNodeSupervisor(f.options); err == nil {
		t.Fatal("missing relay token was accepted")
	}
}

func TestDockerWorkspaceSyncNodeRequiresRelayBindAddress(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.options.RelayBindAddress = ""
	if _, err := NewDockerWorkspaceSyncNodeSupervisor(f.options); err == nil {
		t.Fatal("missing relay bind address was accepted")
	}
}

func TestDockerWorkspaceSyncNodeRejectsUnpinnedRelay(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.setRelayImage("local/morde-relay:latest")
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("relay with a mutable image reference was accepted")
	}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	if f.engine.networkCreates != 0 || f.engine.containerCreates != 0 {
		t.Fatalf("created despite unpinned relay: networks %d, containers %d", f.engine.networkCreates, f.engine.containerCreates)
	}
}

func TestDockerWorkspaceSyncNodeAcceptsLocalContentPinnedImages(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.image = "sha256:" + strings.Repeat("a", 64)
	f.engine.image = f.image
	f.engine.relayImage = "sha256:" + strings.Repeat("9", 64)
	f.options.ImageDigest = f.image
	f.options.RelayImageDigest = f.engine.relayImage
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err != nil {
		t.Fatalf("Ensure() with local content-pinned images: %v", err)
	}
}

func TestDockerWorkspaceSyncNodeErrorsDoNotExposeDockerBodiesOrKeys(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	f.engine.setCreateError(http.StatusInternalServerError, "response body contains "+workspaceSyncTestKeyA)
	_, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA)
	if err == nil || strings.Contains(err.Error(), workspaceSyncTestKeyA) || strings.Contains(err.Error(), "response body contains") {
		t.Fatalf("unsanitized Docker error = %v", err)
	}
}

func TestNewDockerWorkspaceSyncNodeSupervisorValidatesOptions(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	invalid := f.options
	invalid.WorkspaceRoot = filepath.Join(invalid.StateRoot, "nested")
	if _, err := NewDockerWorkspaceSyncNodeSupervisor(invalid); err == nil {
		t.Fatal("overlapping state and workspace roots were accepted")
	}
	invalid = f.options
	invalid.Resources.MemoryBytes = 2 << 30
	if _, err := NewDockerWorkspaceSyncNodeSupervisor(invalid); err == nil {
		t.Fatal("unbounded resource values were accepted")
	}
}

func slicesContainsString(values any, expected string) bool {
	switch typed := values.(type) {
	case []string:
		for _, value := range typed {
			if value == expected {
				return true
			}
		}
	case []any:
		for _, value := range typed {
			if value == expected {
				return true
			}
		}
	}
	return false
}

func TestDockerWorkspaceSyncNodeContainerWrongMountIsRejected(t *testing.T) {
	f := newWorkspaceSyncFixture(t)
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err != nil {
		t.Fatal(err)
	}
	f.engine.mu.Lock()
	container := f.engine.containersByID[strings.Repeat("b", 64)]
	mounts := container.Payload["HostConfig"].(map[string]any)["Mounts"].([]any)
	mounts[1].(map[string]any)["Source"] = filepath.Join(f.workRoot, "other-owner")
	f.engine.mu.Unlock()
	if _, err := f.supervisor(t).Ensure(context.Background(), workspaceSyncTestOwnerA, workspaceSyncTestKeyA); err == nil {
		t.Fatal("container with another owner's workspace mount was accepted")
	}
}
