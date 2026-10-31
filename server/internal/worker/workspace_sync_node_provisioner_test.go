package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/domain/workspace"
)

type workspaceSyncQuotaSocketManagerStub struct{ err error }

func (s workspaceSyncQuotaSocketManagerStub) EnsureOwner(context.Context, string) error { return s.err }
func (workspaceSyncQuotaSocketManagerStub) Close() error                                { return nil }

func workspaceSyncNoopStartGuard() WorkspaceSyncNodeStartGuard {
	return WorkspaceSyncNodeStartGuardFunc(func(context.Context, string) error { return nil })
}

func TestWorkspaceSyncNodeProvisionerObserveDoesNotEnsureOrTouchRelayGate(t *testing.T) {
	relayRoot := t.TempDir()
	supervisor := &workspaceSyncNodeSupervisorStub{
		observation: workspace.SyncNodeObservation{
			Present: true, Running: true,
			ConnectedDeviceIDs: []string{testWorkspaceSyncDevice},
			Folders: []workspace.SyncNodeFolderObservation{{
				FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "syncing", NeedBytes: 11, NeedFiles: 2,
			}},
		},
		workspaceSyncRelayConfig: workspaceSyncRelayConfig{
			StateRoot: relayRoot, PublicAddress: "8.8.8.8:22067", BindAddress: "172.18.141.222", Token: workspaceSyncTestRelayToken,
		},
	}
	gate := &workspaceSyncRelayGateStub{}
	provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: &workspaceSyncNodeStoreStub{}, RelayGate: gate,
		QuotaSockets: workspaceSyncQuotaSocketManagerStub{}, StartGuard: workspaceSyncNoopStartGuard(), DataKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provisioner.ObserveWorkspaceSyncNode(context.Background(), testWorkspaceSyncOwner)
	if err != nil {
		t.Fatalf("ObserveWorkspaceSyncNode() error = %v", err)
	}
	if got.Present != supervisor.observation.Present || got.Running != supervisor.observation.Running ||
		len(got.ConnectedDeviceIDs) != 1 || len(got.Folders) != 1 || supervisor.observeCalls != 1 ||
		supervisor.observeOwnerID != testWorkspaceSyncOwner || supervisor.observeAPIKey == "" || supervisor.ensureCalls != 0 || supervisor.stopCalls != 0 {
		t.Fatalf("observation=%+v supervisor=%+v", got, supervisor)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("ObserveWorkspaceSyncNode() changed relay gate: %v", gate.calls)
	}
}

const (
	testWorkspaceSyncOwner    = "u1234567"
	testWorkspaceSyncDevice   = "AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	testWorkspaceSyncNodeID   = "BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB"
	testWorkspaceSyncRelayID  = "CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC"
	testWorkspaceSyncRelayURI = "relay://8.8.8.8:22067/?id=" + testWorkspaceSyncRelayID + "&token=" + workspaceSyncTestRelayToken
)

type workspaceSyncNodeSupervisorStub struct {
	ownerID          string
	apiKey           string
	apiURL           string
	ensureCalls      int
	observeCalls     int
	observeOwnerID   string
	observeAPIKey    string
	observation      workspace.SyncNodeObservation
	observeError     error
	ensureError      error
	stopError        error
	stopCalls        int
	stopOwnerID      string
	stopContextError error
	workspaceSyncRelayConfig
}

type workspaceSyncRelayGateStub struct {
	mu           sync.Mutex
	calls        []string
	enableError  error
	disableError error
}

func (s *workspaceSyncRelayGateStub) Enable(_ context.Context, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "enable:"+ownerID)
	return s.enableError
}

func (s *workspaceSyncRelayGateStub) Disable(_ context.Context, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "disable:"+ownerID)
	return s.disableError
}

func (s *workspaceSyncNodeSupervisorStub) Ensure(_ context.Context, ownerID, apiKey string) (DockerWorkspaceSyncNode, error) {
	s.ensureCalls++
	s.ownerID, s.apiKey = ownerID, apiKey
	if s.ensureError != nil {
		return DockerWorkspaceSyncNode{}, s.ensureError
	}
	return DockerWorkspaceSyncNode{ContainerID: strings.Repeat("a", 64), APIURL: s.apiURL}, nil
}

func (s *workspaceSyncNodeSupervisorStub) Observe(_ context.Context, ownerID, apiKey string) (workspace.SyncNodeObservation, error) {
	s.observeCalls++
	s.observeOwnerID, s.observeAPIKey = ownerID, apiKey
	return s.observation, s.observeError
}

func (s *workspaceSyncNodeSupervisorStub) StopOwner(ctx context.Context, ownerID string) error {
	s.stopCalls++
	s.stopOwnerID = ownerID
	s.stopContextError = ctx.Err()
	return s.stopError
}

func (s *workspaceSyncNodeSupervisorStub) relayConfig() workspaceSyncRelayConfig {
	return s.workspaceSyncRelayConfig
}

type workspaceSyncNodeStoreStub struct {
	owners        []string
	pendingOwners []string
	state         workspace.SyncNodeDesiredState
	reconcile     int
	applyError    error
	expiryScan    bool
}

func (s *workspaceSyncNodeStoreStub) RevokeExpiredWorkspaceSyncDevices(context.Context, func(context.Context, string) error) error {
	s.expiryScan = true
	return nil
}

func (s *workspaceSyncNodeStoreStub) ListWorkspaceSyncNodeOwnersToReconcile(context.Context) ([]string, error) {
	if !s.expiryScan {
		return nil, fmt.Errorf("expired Workspace sync devices were not scanned before listing owners")
	}
	if s.pendingOwners != nil {
		return append([]string(nil), s.pendingOwners...), nil
	}
	return append([]string(nil), s.owners...), nil
}

func (s *workspaceSyncNodeStoreStub) ListWorkspaceSyncNodeOwners(context.Context) ([]string, error) {
	return append([]string(nil), s.owners...), nil
}

func (s *workspaceSyncNodeStoreStub) ReconcileWorkspaceSyncNodeWithRelayGate(ctx context.Context, ownerID string,
	apply func(context.Context, workspace.SyncNodeDesiredState) error,
	enableRelay func(context.Context, workspace.SyncNodeDesiredState) error,
	disableRelay func(context.Context) error) error {
	s.reconcile++
	if err := disableRelay(ctx); err != nil {
		return err
	}
	if s.applyError != nil {
		return s.applyError
	}
	state := s.state
	state.OwnerID = ownerID
	if err := apply(ctx, state); err != nil {
		return err
	}
	if len(state.Folders) > 0 {
		return enableRelay(ctx, state)
	}
	return nil
}

type workspaceSyncNodeTestAPI struct {
	mu                   sync.Mutex
	folders              map[string]map[string]any
	devices              map[string]map[string]any
	pausedDevices        map[string]bool
	connectedDevices     map[string]bool
	statusFailures       int
	statusCalls          atomic.Int32
	apiKey               string
	nodeID               string
	optionsError         bool
	listenAddresses      []string
	events               []string
	omitConnectionDevice string
}

func (s *workspaceSyncNodeTestAPI) handler(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-API-Key"); got != s.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
		if int(s.statusCalls.Add(1)) <= s.statusFailures {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		nodeID := s.nodeID
		if nodeID == "" {
			nodeID = testWorkspaceSyncNodeID
		}
		_, _ = fmt.Fprintf(w, `{"myID":%q}`, nodeID)
	case r.Method == http.MethodPatch && r.URL.Path == "/rest/config/options":
		if s.optionsError {
			http.Error(w, "options update failed", http.StatusInternalServerError)
			return
		}
		var patch struct {
			ListenAddresses []string `json:"listenAddresses"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "bad patch", http.StatusBadRequest)
			return
		}
		s.listenAddresses = patch.ListenAddresses
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && r.URL.Path == "/rest/config/defaults/device":
		_, _ = w.Write([]byte(`{"deviceID":"","name":"","addresses":["dynamic"],"autoAcceptFolders":false,"introducer":false,"paused":false}`))
	case r.Method == http.MethodGet && r.URL.Path == "/rest/config/defaults/folder":
		_, _ = w.Write([]byte(`{"id":"","label":"","path":"","type":"sendreceive","paused":false,"devices":[]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/rest/config/devices":
		s.ensureLocalDevice()
		writeConfigMapList(w, s.devices)
	case r.Method == http.MethodGet && r.URL.Path == "/rest/config/folders":
		s.ensureLocalDevice()
		for _, folder := range s.folders {
			devices, _ := folder["devices"].([]any)
			hasLocal := false
			for _, item := range devices {
				device, _ := item.(map[string]any)
				if device["deviceID"] == testWorkspaceSyncNodeID {
					hasLocal = true
				}
			}
			if !hasLocal {
				folder["devices"] = append(devices, map[string]any{"deviceID": testWorkspaceSyncNodeID})
			}
		}
		writeConfigMapList(w, s.folders)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/config/devices/"):
		writeConfigItem(w, s.devices, strings.TrimPrefix(r.URL.Path, "/rest/config/devices/"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/config/folders/"):
		writeConfigItem(w, s.folders, strings.TrimPrefix(r.URL.Path, "/rest/config/folders/"))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rest/config/devices/"):
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			http.Error(w, "bad device", http.StatusBadRequest)
			return
		}
		id, _ := value["deviceID"].(string)
		s.devices[id] = value
		s.events = append(s.events, "put-device:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rest/config/folders/"):
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			http.Error(w, "bad folder", http.StatusBadRequest)
			return
		}
		id, _ := value["id"].(string)
		s.folders[id] = value
		s.events = append(s.events, "put-folder:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/rest/config/folders/"):
		id := strings.TrimPrefix(r.URL.Path, "/rest/config/folders/")
		var patch struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || s.folders[id] == nil {
			http.Error(w, "bad folder pause", http.StatusBadRequest)
			return
		}
		s.folders[id]["paused"] = patch.Paused
		s.events = append(s.events, "pause-folder:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rest/config/folders/"):
		id := strings.TrimPrefix(r.URL.Path, "/rest/config/folders/")
		delete(s.folders, id)
		s.events = append(s.events, "delete-folder:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rest/config/devices/"):
		id := strings.TrimPrefix(r.URL.Path, "/rest/config/devices/")
		delete(s.devices, id)
		s.events = append(s.events, "delete-device:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && r.URL.Path == "/rest/system/pause":
		id := r.URL.Query().Get("device")
		s.pausedDevices[id] = true
		s.events = append(s.events, "pause-device:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && r.URL.Path == "/rest/system/resume":
		id := r.URL.Query().Get("device")
		delete(s.pausedDevices, id)
		s.events = append(s.events, "resume-device:"+id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && r.URL.Path == "/rest/system/connections":
		connections := make(map[string]any, len(s.devices))
		for id := range s.devices {
			if id == s.omitConnectionDevice {
				continue
			}
			connections[id] = map[string]any{"connected": s.connectedDevices[id], "paused": s.pausedDevices[id]}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": connections})
	default:
		http.Error(w, "unexpected Syncthing request", http.StatusNotFound)
	}
}

func (s *workspaceSyncNodeTestAPI) ensureLocalDevice() {
	if s.devices[testWorkspaceSyncNodeID] == nil {
		s.devices[testWorkspaceSyncNodeID] = map[string]any{"deviceID": testWorkspaceSyncNodeID, "name": "server", "paused": false}
	}
}

func writeConfigMapList(w http.ResponseWriter, values map[string]map[string]any) {
	items := make([]map[string]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	_ = json.NewEncoder(w).Encode(items)
}

func writeConfigItem(w http.ResponseWriter, values map[string]map[string]any, id string) {
	value, ok := values[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func TestWorkspaceSyncNodeProvisionerAppliesSnapshotAndReturnsPinnedRelay(t *testing.T) {
	relayRoot := t.TempDir()
	relayID := writeWorkspaceSyncRelayCertificate(t, relayRoot)
	publicRelayURI := "relay://8.8.8.8:22067/?id=" + relayID + "&token=" + workspaceSyncTestRelayToken
	nodeRelayURI := "relay://172.18.141.222:22067/?id=" + relayID + "&token=" + workspaceSyncTestRelayToken
	api := &workspaceSyncNodeTestAPI{
		folders: make(map[string]map[string]any), devices: make(map[string]map[string]any),
		pausedDevices: make(map[string]bool), statusFailures: 1,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	api.apiKey = ""
	supervisor := &workspaceSyncNodeSupervisorStub{apiURL: server.URL, workspaceSyncRelayConfig: workspaceSyncRelayConfig{
		StateRoot: relayRoot, PublicAddress: "8.8.8.8:22067", BindAddress: "172.18.141.222", Token: workspaceSyncTestRelayToken,
	}}
	store := &workspaceSyncNodeStoreStub{state: workspace.SyncNodeDesiredState{
		Revision: 1,
		Folders:  []workspace.SyncNodeFolder{{WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceIDs: []string{testWorkspaceSyncDevice}}},
	}}
	dataKey := []byte(strings.Repeat("k", 32))
	provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: store, RelayGate: &workspaceSyncRelayGateStub{}, QuotaSockets: workspaceSyncQuotaSocketManagerStub{},
		StartGuard: workspaceSyncNoopStartGuard(), DataKey: dataKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	api.apiKey = provisioner.apiKey(testWorkspaceSyncOwner)
	connection, err := provisioner.EnsureWorkspaceSyncNode(context.Background(), testWorkspaceSyncOwner)
	if err != nil {
		t.Fatal(err)
	}
	if connection.DeviceID != testWorkspaceSyncNodeID || connection.RelayAddress != publicRelayURI ||
		supervisor.ownerID != testWorkspaceSyncOwner || supervisor.apiKey != api.apiKey || store.reconcile != 1 || api.statusCalls.Load() != 3 || supervisor.stopCalls != 0 {
		t.Fatalf("connection=%+v owner=%q key_match=%t reconciles=%d status_calls=%d", connection, supervisor.ownerID,
			supervisor.apiKey == api.apiKey, store.reconcile, api.statusCalls.Load())
	}
	if calls := strings.Join(provisioner.relayGate.(*workspaceSyncRelayGateStub).calls, ","); calls != "disable:"+testWorkspaceSyncOwner+",enable:"+testWorkspaceSyncOwner {
		t.Fatalf("relay gate calls = %q", calls)
	}
	if len(api.listenAddresses) != 1 || api.listenAddresses[0] != nodeRelayURI {
		t.Fatalf("node listen addresses = %v", api.listenAddresses)
	}
	if len(api.folders) != 1 || len(api.devices) != 2 {
		t.Fatalf("node config folders=%d devices=%d", len(api.folders), len(api.devices))
	}
	if api.devices[testWorkspaceSyncNodeID] == nil || slices.Contains(api.events, "pause-device:"+testWorkspaceSyncNodeID) ||
		slices.Contains(api.events, "delete-device:"+testWorkspaceSyncNodeID) {
		t.Fatalf("local Syncthing device was treated as a remote device: devices=%v events=%v", api.devices, api.events)
	}
	folder := api.folders["morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
	if folder["path"] != "/workspaces/w1234567" || folder["paused"] != false || len(folder["devices"].([]any)) != 2 {
		t.Fatalf("folder config = %#v", folder)
	}
	device := api.devices[testWorkspaceSyncDevice]
	addresses, ok := device["addresses"].([]any)
	if device["autoAcceptFolders"] != false || device["paused"] != false || !ok || !slices.Equal(addresses, []any{nodeRelayURI}) {
		t.Fatalf("device config = %#v", device)
	}
}

func TestWorkspaceSyncNodeProvisionerStopsOwnerOnProvisioningFailures(t *testing.T) {
	storeFailure := errors.New("desired state store failed")
	tests := []struct {
		name             string
		apiURL           string
		ensureError      error
		statusFailures   int
		nodeID           string
		invalidRelayCert bool
		optionsError     bool
		storeError       error
		gateEnableError  error
		cancelRequestCtx bool
	}{
		{name: "supervisor ensure", ensureError: errors.New("supervisor failed")},
		{name: "invalid API configuration", apiURL: "not a URL"},
		{name: "API readiness", statusFailures: 1, cancelRequestCtx: true},
		{name: "node identity", nodeID: "invalid device id"},
		{name: "relay certificate", invalidRelayCert: true},
		{name: "private relay configuration", optionsError: true},
		{name: "store reconciliation", storeError: storeFailure},
		{name: "relay gate activation", gateEnableError: errors.New("nftables gate failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relayRoot := t.TempDir()
			if test.invalidRelayCert {
				if err := os.WriteFile(filepath.Join(relayRoot, "cert.pem"), []byte("invalid certificate"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = writeWorkspaceSyncRelayCertificate(t, relayRoot)
			}
			api := &workspaceSyncNodeTestAPI{
				folders: make(map[string]map[string]any), devices: make(map[string]map[string]any),
				pausedDevices: make(map[string]bool), statusFailures: test.statusFailures,
				nodeID: test.nodeID, optionsError: test.optionsError,
			}
			server := httptest.NewServer(http.HandlerFunc(api.handler))
			defer server.Close()
			apiURL := server.URL
			if test.apiURL != "" {
				apiURL = test.apiURL
			}
			supervisor := &workspaceSyncNodeSupervisorStub{
				apiURL: apiURL, ensureError: test.ensureError,
				workspaceSyncRelayConfig: workspaceSyncRelayConfig{
					StateRoot: relayRoot, PublicAddress: "8.8.8.8:22067", BindAddress: "172.18.141.222", Token: workspaceSyncTestRelayToken,
				},
			}
			store := &workspaceSyncNodeStoreStub{applyError: test.storeError}
			if test.gateEnableError != nil {
				store.owners = []string{testWorkspaceSyncOwner}
				store.pendingOwners = []string{}
				store.state = workspace.SyncNodeDesiredState{Folders: []workspace.SyncNodeFolder{{
					WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceIDs: []string{testWorkspaceSyncDevice},
				}}}
			}
			gate := &workspaceSyncRelayGateStub{enableError: test.gateEnableError}
			provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
				Supervisor: supervisor, Store: store, RelayGate: gate, QuotaSockets: workspaceSyncQuotaSocketManagerStub{},
				StartGuard: workspaceSyncNoopStartGuard(), DataKey: []byte(strings.Repeat("k", 32)),
			})
			if err != nil {
				t.Fatal(err)
			}
			api.apiKey = provisioner.apiKey(testWorkspaceSyncOwner)

			ctx := context.Background()
			if test.cancelRequestCtx {
				cancelledCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelledCtx
			}
			if test.gateEnableError != nil {
				err = provisioner.ReconcileWorkspaceSyncNodesOnStartup(ctx)
			} else {
				_, err = provisioner.EnsureWorkspaceSyncNode(ctx, testWorkspaceSyncOwner)
			}
			if err == nil {
				t.Fatal("provisioning failure was accepted")
			}
			if supervisor.stopCalls != 1 || supervisor.stopOwnerID != testWorkspaceSyncOwner {
				t.Fatalf("stop calls=%d owner=%q", supervisor.stopCalls, supervisor.stopOwnerID)
			}
			if supervisor.stopContextError != nil {
				t.Fatalf("StopOwner received canceled context: %v", supervisor.stopContextError)
			}
			if test.gateEnableError != nil && !slices.Equal(gate.calls, []string{
				"disable:" + testWorkspaceSyncOwner, "enable:" + testWorkspaceSyncOwner,
				"disable:" + testWorkspaceSyncOwner,
			}) {
				t.Fatalf("relay gate calls after activation failure = %v", gate.calls)
			}
			if test.gateEnableError != nil {
				gate.enableError = nil
				if err := provisioner.ReconcilePendingWorkspaceSyncNodes(context.Background()); err != nil {
					t.Fatalf("retry startup owner after transient gate failure: %v", err)
				}
				if store.reconcile != 2 || !slices.Equal(gate.calls[len(gate.calls)-2:], []string{
					"disable:" + testWorkspaceSyncOwner,
					"enable:" + testWorkspaceSyncOwner,
				}) {
					t.Fatalf("startup relay retry reconcile=%d gate calls=%v", store.reconcile, gate.calls)
				}
			}
		})
	}
}

func TestWorkspaceSyncNodeProvisionerJoinsStopErrorAndRedactsSecrets(t *testing.T) {
	relayRoot := t.TempDir()
	_ = writeWorkspaceSyncRelayCertificate(t, relayRoot)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	cleanup := errors.New("cleanup failure includes relay token " + workspaceSyncTestRelayToken)
	supervisor := &workspaceSyncNodeSupervisorStub{
		apiURL: server.URL, stopError: cleanup,
		workspaceSyncRelayConfig: workspaceSyncRelayConfig{
			StateRoot: relayRoot, PublicAddress: "8.8.8.8:22067", BindAddress: "172.18.141.222", Token: workspaceSyncTestRelayToken,
		},
	}
	provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: &workspaceSyncNodeStoreStub{}, RelayGate: &workspaceSyncRelayGateStub{},
		QuotaSockets: workspaceSyncQuotaSocketManagerStub{}, StartGuard: workspaceSyncNoopStartGuard(), DataKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	apiKey := provisioner.apiKey(testWorkspaceSyncOwner)
	primary := errors.New("primary failure includes api key " + apiKey)
	supervisor.ensureError = primary
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provisioner.EnsureWorkspaceSyncNode(ctx, testWorkspaceSyncOwner)
	if err == nil || !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("error=%v, want primary and cleanup errors joined", err)
	}
	if strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), workspaceSyncTestRelayToken) {
		t.Fatalf("provisioning error leaked a secret: %v", err)
	}
	if supervisor.stopCalls != 1 || supervisor.stopContextError != nil {
		t.Fatalf("stop calls=%d context error=%v", supervisor.stopCalls, supervisor.stopContextError)
	}
}

func TestWorkspaceSyncNodeApplyRevokesOneFolderButKeepsOtherWorkspaceDevice(t *testing.T) {
	removedFolder := "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keptFolder := "morde-cccccccccccccccccccccccccccccccc"
	api := &workspaceSyncNodeTestAPI{
		folders: map[string]map[string]any{
			removedFolder: {"id": removedFolder, "label": "Morde Workspace", "path": "/workspaces/w7654321", "type": "sendreceive", "paused": false,
				"devices": []any{map[string]any{"deviceID": testWorkspaceSyncDevice}}},
			keptFolder: {"id": keptFolder, "label": "Morde Workspace", "path": "/workspaces/w1234567", "type": "sendreceive", "paused": false,
				"devices": []any{map[string]any{"deviceID": testWorkspaceSyncDevice}}},
		},
		devices: map[string]map[string]any{testWorkspaceSyncDevice: {"deviceID": testWorkspaceSyncDevice, "name": "Morde",
			"addresses": []any{"dynamic"}, "autoAcceptFolders": false, "introducer": false, "paused": false}},
		pausedDevices: make(map[string]bool),
		apiKey:        testSyncthingAPIKey,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	state := workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2,
		Folders: []workspace.SyncNodeFolder{{WorkspaceID: "w1234567", FolderID: keptFolder, DeviceIDs: []string{testWorkspaceSyncDevice}}}}
	if err := applyWorkspaceSyncNodeDesiredState(context.Background(), client, state, testWorkspaceSyncNodeID, testWorkspaceSyncRelayURI); err != nil {
		t.Fatal(err)
	}
	if _, exists := api.folders[removedFolder]; exists || api.folders[keptFolder] == nil || api.devices[testWorkspaceSyncDevice] == nil {
		t.Fatalf("revocation removed the wrong configuration: folders=%v devices=%v", api.folders, api.devices)
	}
	pauseIndex, deleteIndex, resumeIndex := -1, -1, -1
	for index, event := range api.events {
		switch event {
		case "pause-device:" + testWorkspaceSyncDevice:
			pauseIndex = index
		case "delete-folder:" + removedFolder:
			deleteIndex = index
		case "resume-device:" + testWorkspaceSyncDevice:
			resumeIndex = index
		}
	}
	if pauseIndex < 0 || deleteIndex <= pauseIndex || resumeIndex <= deleteIndex {
		t.Fatalf("revocation order did not pause, remove folder, then resume device: %v", api.events)
	}
	if slices.Contains(api.events, "pause-device:"+testWorkspaceSyncNodeID) || slices.Contains(api.events, "delete-device:"+testWorkspaceSyncNodeID) {
		t.Fatalf("local Syncthing device was changed during revocation: %v", api.events)
	}
}

func TestWorkspaceSyncNodeApplyRepairsManagedFolderTypeAndLabel(t *testing.T) {
	folderID := "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	api := &workspaceSyncNodeTestAPI{
		folders: map[string]map[string]any{folderID: {"id": folderID, "label": "Wrong label", "path": "/workspaces/w1234567", "type": "sendonly", "paused": false,
			"devices": []any{map[string]any{"deviceID": testWorkspaceSyncDevice}}}},
		devices: map[string]map[string]any{testWorkspaceSyncDevice: {"deviceID": testWorkspaceSyncDevice, "name": "Morde",
			"addresses": []any{"dynamic"}, "autoAcceptFolders": false, "introducer": false, "paused": false}},
		pausedDevices: make(map[string]bool), apiKey: testSyncthingAPIKey,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	state := workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2,
		Folders: []workspace.SyncNodeFolder{{WorkspaceID: "w1234567", FolderID: folderID, DeviceIDs: []string{testWorkspaceSyncDevice}}}}
	if err := applyWorkspaceSyncNodeDesiredState(context.Background(), client, state, testWorkspaceSyncNodeID, testWorkspaceSyncRelayURI); err != nil {
		t.Fatal(err)
	}
	folder := api.folders[folderID]
	if folder["label"] != "Morde Workspace" || folder["type"] != "sendreceive" {
		t.Fatalf("managed folder fields = %#v", folder)
	}
}

func TestWorkspaceSyncNodeApplyFailsClosedWhenPauseStateOmitsConfiguredDevice(t *testing.T) {
	folderID := "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	api := &workspaceSyncNodeTestAPI{
		folders: map[string]map[string]any{folderID: {"id": folderID, "label": "Morde Workspace", "path": "/workspaces/w1234567", "type": "sendreceive", "paused": false,
			"devices": []any{map[string]any{"deviceID": testWorkspaceSyncDevice}}}},
		devices: map[string]map[string]any{testWorkspaceSyncDevice: {"deviceID": testWorkspaceSyncDevice, "name": "Morde",
			"addresses": []any{"dynamic"}, "autoAcceptFolders": false, "introducer": false, "paused": false}},
		pausedDevices: make(map[string]bool), omitConnectionDevice: testWorkspaceSyncDevice, apiKey: testSyncthingAPIKey,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyWorkspaceSyncNodeDesiredState(context.Background(), client,
		workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2}, testWorkspaceSyncNodeID, testWorkspaceSyncRelayURI); err == nil {
		t.Fatal("missing configured-device connection state was treated as a completed pause")
	}
	if api.folders[folderID] == nil || api.devices[testWorkspaceSyncDevice] == nil {
		t.Fatalf("revocation removed configuration without proving disconnection: folders=%v devices=%v", api.folders, api.devices)
	}
	if slices.Contains(api.events, "delete-folder:"+folderID) || slices.Contains(api.events, "delete-device:"+testWorkspaceSyncDevice) {
		t.Fatalf("revocation continued after connection-state omission: %v", api.events)
	}
}

func TestWorkspaceSyncNodeApplyWaitsForActiveConnectionToClose(t *testing.T) {
	folderID := "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	api := &workspaceSyncNodeTestAPI{
		folders: map[string]map[string]any{folderID: {"id": folderID, "label": "Morde Workspace", "path": "/workspaces/w1234567", "type": "sendreceive", "paused": false,
			"devices": []any{map[string]any{"deviceID": testWorkspaceSyncDevice}}}},
		devices: map[string]map[string]any{testWorkspaceSyncDevice: {"deviceID": testWorkspaceSyncDevice, "name": "Morde",
			"addresses": []any{"dynamic"}, "autoAcceptFolders": false, "introducer": false, "paused": false}},
		pausedDevices: make(map[string]bool), connectedDevices: map[string]bool{testWorkspaceSyncDevice: true}, apiKey: testSyncthingAPIKey,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := applyWorkspaceSyncNodeDesiredState(ctx, client,
		workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2}, testWorkspaceSyncNodeID, testWorkspaceSyncRelayURI); err == nil {
		t.Fatal("active Syncthing connection was treated as disconnected")
	}
	if api.folders[folderID] == nil || api.devices[testWorkspaceSyncDevice] == nil {
		t.Fatalf("revocation removed configuration before active connection closed: folders=%v devices=%v", api.folders, api.devices)
	}
	if slices.Contains(api.events, "delete-folder:"+folderID) || slices.Contains(api.events, "delete-device:"+testWorkspaceSyncDevice) {
		t.Fatalf("revocation proceeded while device was connected: %v", api.events)
	}
}

func TestWorkspaceSyncNodeProvisionerRejectsInvalidInputsAndReconcilesPendingOwners(t *testing.T) {
	api := &workspaceSyncNodeTestAPI{folders: map[string]map[string]any{}, devices: map[string]map[string]any{}, pausedDevices: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	relayRoot := t.TempDir()
	_ = writeWorkspaceSyncRelayCertificate(t, relayRoot)
	supervisor := &workspaceSyncNodeSupervisorStub{apiURL: server.URL, workspaceSyncRelayConfig: workspaceSyncRelayConfig{
		StateRoot: relayRoot, PublicAddress: "8.8.8.8:22067", BindAddress: "172.18.141.222", Token: workspaceSyncTestRelayToken,
	}}
	store := &workspaceSyncNodeStoreStub{owners: []string{testWorkspaceSyncOwner}, state: workspace.SyncNodeDesiredState{Revision: 1}}
	options := WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, Store: store, RelayGate: &workspaceSyncRelayGateStub{},
		QuotaSockets: workspaceSyncQuotaSocketManagerStub{}, StartGuard: workspaceSyncNoopStartGuard(), DataKey: []byte(strings.Repeat("k", 32))}
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, DataKey: options.DataKey}); err == nil {
		t.Fatal("missing durable reconciliation store was accepted")
	}
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, Store: store, RelayGate: options.RelayGate, DataKey: []byte("short")}); err == nil {
		t.Fatal("short data key was accepted")
	}
	invalidRelaySupervisor := *supervisor
	invalidRelaySupervisor.PublicAddress = "https://relay.example.test:22067/path"
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: &invalidRelaySupervisor, Store: store, RelayGate: options.RelayGate, DataKey: options.DataKey}); err == nil {
		t.Fatal("invalid relay address was accepted")
	}
	invalidRelaySupervisor = *supervisor
	invalidRelaySupervisor.Token = ""
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: &invalidRelaySupervisor, Store: store, RelayGate: options.RelayGate, DataKey: options.DataKey}); err == nil {
		t.Fatal("missing relay token was accepted")
	}
	invalidRelaySupervisor = *supervisor
	invalidRelaySupervisor.BindAddress = "0.0.0.0"
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: &invalidRelaySupervisor, Store: store, RelayGate: options.RelayGate, DataKey: options.DataKey}); err == nil {
		t.Fatal("unspecified relay bind address was accepted")
	}
	provisioner, err := NewWorkspaceSyncNodeProvisioner(options)
	if err != nil {
		t.Fatal(err)
	}
	api.apiKey = provisioner.apiKey(testWorkspaceSyncOwner)
	if err := provisioner.ReconcilePendingWorkspaceSyncNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.expiryScan || store.reconcile != 1 || supervisor.ownerID != testWorkspaceSyncOwner {
		t.Fatalf("pending reconciliation expiry_scan=%t calls=%d owner=%q", store.expiryScan, store.reconcile, supervisor.ownerID)
	}
	if _, err := provisioner.EnsureWorkspaceSyncNode(context.Background(), "../other"); err == nil {
		t.Fatal("invalid owner ID was accepted")
	}
}

func TestWorkspaceSyncRelayHostRequiresPublicIPv4AndPort22067(t *testing.T) {
	for _, address := range []string{"8.8.8.8:22067"} {
		if !validWorkspaceSyncRelayHost(address) {
			t.Errorf("valid relay address %q was rejected", address)
		}
	}
	for _, address := range []string{"relay.example.test:22067", "10.0.0.1:22067", "127.0.0.1:22067", "8.8.8.8:22068", "[2001:db8::1]:22067"} {
		if validWorkspaceSyncRelayHost(address) {
			t.Errorf("unsafe relay address %q was accepted", address)
		}
	}
}

func TestWorkspaceSyncRelayNodeURIAllowsOnlyRoutableIPv4AndPort22067(t *testing.T) {
	for _, address := range []string{"172.18.141.222:22067", "8.8.8.8:22067"} {
		if !validWorkspaceSyncRelayURIHost(address) {
			t.Errorf("valid relay URI host %q was rejected", address)
		}
	}
	for _, address := range []string{"127.0.0.1:22067", "169.254.1.1:22067", "0.0.0.0:22067", "relay.example.test:22067", "172.18.141.222:22068", "[::ffff:8.8.8.8]:22067"} {
		if validWorkspaceSyncRelayURIHost(address) {
			t.Errorf("invalid relay URI host %q was accepted", address)
		}
	}
}

func writeWorkspaceSyncRelayCertificate(t *testing.T, root string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Morde relay"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		SignatureAlgorithm: x509.ECDSAWithSHA256}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := workspace.SyncthingDeviceIDFromCertificateDER(der)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
