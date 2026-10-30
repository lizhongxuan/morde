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

const (
	testWorkspaceSyncOwner   = "u1234567"
	testWorkspaceSyncDevice  = "AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	testWorkspaceSyncNodeID  = "BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB"
	testWorkspaceSyncRelayID = "CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC"
)

type workspaceSyncNodeSupervisorStub struct {
	ownerID string
	apiKey  string
	apiURL  string
	workspaceSyncRelayConfig
}

func (s *workspaceSyncNodeSupervisorStub) Ensure(_ context.Context, ownerID, apiKey string) (DockerWorkspaceSyncNode, error) {
	s.ownerID, s.apiKey = ownerID, apiKey
	return DockerWorkspaceSyncNode{ContainerID: strings.Repeat("a", 64), APIURL: s.apiURL}, nil
}

func (s *workspaceSyncNodeSupervisorStub) relayConfig() workspaceSyncRelayConfig {
	return s.workspaceSyncRelayConfig
}

type workspaceSyncNodeStoreStub struct {
	owners     []string
	state      workspace.SyncNodeDesiredState
	reconcile  int
	applyError error
}

func (s *workspaceSyncNodeStoreStub) ListWorkspaceSyncNodeOwnersToReconcile(context.Context) ([]string, error) {
	return append([]string(nil), s.owners...), nil
}

func (s *workspaceSyncNodeStoreStub) ReconcileWorkspaceSyncNode(ctx context.Context, ownerID string,
	apply func(context.Context, workspace.SyncNodeDesiredState) error) error {
	s.reconcile++
	if s.applyError != nil {
		return s.applyError
	}
	state := s.state
	state.OwnerID = ownerID
	return apply(ctx, state)
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
		_, _ = fmt.Fprintf(w, `{"myID":%q}`, testWorkspaceSyncNodeID)
	case r.Method == http.MethodPatch && r.URL.Path == "/rest/config/options":
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
	api := &workspaceSyncNodeTestAPI{
		folders: make(map[string]map[string]any), devices: make(map[string]map[string]any),
		pausedDevices: make(map[string]bool), statusFailures: 1,
	}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	api.apiKey = ""
	supervisor := &workspaceSyncNodeSupervisorStub{apiURL: server.URL, workspaceSyncRelayConfig: workspaceSyncRelayConfig{
		StateRoot: relayRoot, PublicAddress: "relay.example.test:22067", Token: workspaceSyncTestRelayToken,
	}}
	store := &workspaceSyncNodeStoreStub{state: workspace.SyncNodeDesiredState{
		Revision: 1,
		Folders:  []workspace.SyncNodeFolder{{WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceIDs: []string{testWorkspaceSyncDevice}}},
	}}
	dataKey := []byte(strings.Repeat("k", 32))
	provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: store, DataKey: dataKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	api.apiKey = provisioner.apiKey(testWorkspaceSyncOwner)
	connection, err := provisioner.EnsureWorkspaceSyncNode(context.Background(), testWorkspaceSyncOwner)
	if err != nil {
		t.Fatal(err)
	}
	if connection.DeviceID != testWorkspaceSyncNodeID || connection.RelayAddress != "relay://relay.example.test:22067/?id="+relayID+"&token="+workspaceSyncTestRelayToken ||
		supervisor.ownerID != testWorkspaceSyncOwner || supervisor.apiKey != api.apiKey || store.reconcile != 1 || api.statusCalls.Load() != 3 {
		t.Fatalf("connection=%+v owner=%q key_match=%t reconciles=%d status_calls=%d", connection, supervisor.ownerID,
			supervisor.apiKey == api.apiKey, store.reconcile, api.statusCalls.Load())
	}
	if len(api.listenAddresses) != 1 || api.listenAddresses[0] != "relay://morde-relay:22067/?id="+relayID+"&token="+workspaceSyncTestRelayToken {
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
	if device["autoAcceptFolders"] != false || device["paused"] != false {
		t.Fatalf("device config = %#v", device)
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
	if err := applyWorkspaceSyncNodeDesiredState(context.Background(), client, state, testWorkspaceSyncNodeID); err != nil {
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
	if err := applyWorkspaceSyncNodeDesiredState(context.Background(), client, state, testWorkspaceSyncNodeID); err != nil {
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
		workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2}, testWorkspaceSyncNodeID); err == nil {
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
		workspace.SyncNodeDesiredState{OwnerID: testWorkspaceSyncOwner, Revision: 2}, testWorkspaceSyncNodeID); err == nil {
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
		StateRoot: relayRoot, PublicAddress: "relay.example.test:22067", Token: workspaceSyncTestRelayToken,
	}}
	store := &workspaceSyncNodeStoreStub{owners: []string{testWorkspaceSyncOwner}, state: workspace.SyncNodeDesiredState{Revision: 1}}
	options := WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, Store: store, DataKey: []byte(strings.Repeat("k", 32))}
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, DataKey: options.DataKey}); err == nil {
		t.Fatal("missing durable reconciliation store was accepted")
	}
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: supervisor, Store: store, DataKey: []byte("short")}); err == nil {
		t.Fatal("short data key was accepted")
	}
	invalidRelaySupervisor := *supervisor
	invalidRelaySupervisor.workspaceSyncRelayConfig.PublicAddress = "https://relay.example.test:22067/path"
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: &invalidRelaySupervisor, Store: store, DataKey: options.DataKey}); err == nil {
		t.Fatal("invalid relay address was accepted")
	}
	invalidRelaySupervisor = *supervisor
	invalidRelaySupervisor.workspaceSyncRelayConfig.Token = ""
	if _, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{Supervisor: &invalidRelaySupervisor, Store: store, DataKey: options.DataKey}); err == nil {
		t.Fatal("missing relay token was accepted")
	}
	provisioner, err := NewWorkspaceSyncNodeProvisioner(options)
	if err != nil {
		t.Fatal(err)
	}
	api.apiKey = provisioner.apiKey(testWorkspaceSyncOwner)
	if err := provisioner.ReconcilePendingWorkspaceSyncNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.reconcile != 1 || supervisor.ownerID != testWorkspaceSyncOwner {
		t.Fatalf("pending reconciliation calls=%d owner=%q", store.reconcile, supervisor.ownerID)
	}
	if _, err := provisioner.EnsureWorkspaceSyncNode(context.Background(), "../other"); err == nil {
		t.Fatal("invalid owner ID was accepted")
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
