package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSyncthingAPIKey = "test-api-key-do-not-leak"

func TestNewSyncthingAPIClientLocalAddressValidation(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:8384", "http://[::1]:8384", "http://localhost:8384",
		"http://10.0.0.2:8384", "http://172.19.0.2:8384", "http://192.168.1.2:8384",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := NewSyncthingAPIClient(endpoint, testSyncthingAPIKey)
			if err != nil {
				t.Fatalf("NewSyncthingAPIClient(%q) error = %v", endpoint, err)
			}
		})
	}

	for _, endpoint := range []string{
		"",
		"/rest/system/status",
		"https://127.0.0.1:8384",
		"http://127.0.0.2:8384",
		"http://8.8.8.8:8384",
		"http://169.254.1.2:8384",
		"http://localhost.evil:8384",
		"http://example.com:8384",
		"http://user:pass@127.0.0.1:8384",
		"http://127.0.0.1:8384/rest",
		"http://127.0.0.1:8384?key=secret",
		"http://127.0.0.1:0",
		"http://127.0.0.1:65536",
	} {
		t.Run("reject "+endpoint, func(t *testing.T) {
			if _, err := NewSyncthingAPIClient(endpoint, testSyncthingAPIKey); !errors.Is(err, errSyncthingAPIURL) {
				t.Fatalf("NewSyncthingAPIClient(%q) error = %v, want invalid URL", endpoint, err)
			}
		})
	}

	for _, apiKey := range []string{"", "   ", "key\r\ninjected: yes"} {
		if _, err := NewSyncthingAPIClient("http://127.0.0.1:8384", apiKey); !errors.Is(err, errSyncthingAPIKey) {
			t.Fatalf("NewSyncthingAPIClient with API key %q error = %v, want invalid key", apiKey, err)
		}
	}
}
func TestSyncthingAPIClientSendsKeyOnlyInHeader(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("X-API-Key"); got != testSyncthingAPIKey {
			t.Errorf("X-API-Key = %q, want key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		if strings.Contains(r.URL.String(), testSyncthingAPIKey) {
			t.Error("API key appeared in request URL")
		}
		if r.Method != http.MethodGet || r.URL.Path != "/rest/config/folders/folder-1" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s, want folder GET without query", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"folder-1","path":"/workspace"}`))
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetFolder(context.Background(), "folder-1")
	if err != nil {
		t.Fatal(err)
	}
	var folder map[string]any
	if err := json.Unmarshal(got, &folder); err != nil || folder["id"] != "folder-1" {
		t.Fatalf("GetFolder() = %s, error = %v", got, err)
	}
	if calls != 1 {
		t.Fatalf("request count = %d, want 1", calls)
	}
}

func TestSyncthingAPIClientConfigAndRevocationEndpoints(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("X-API-Key"); got != testSyncthingAPIKey {
			t.Errorf("X-API-Key = %q, want key", got)
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config/folders/folder-1":
			var body struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID != "folder-1" {
				t.Errorf("folder PUT body = %+v, error = %v", body, err)
			}
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config/devices/device-1":
			var body struct {
				DeviceID string `json:"deviceID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DeviceID != "device-1" {
				t.Errorf("device PUT body = %+v, error = %v", body, err)
			}
		case r.Method == http.MethodPost && (r.URL.Path == "/rest/system/pause" || r.URL.Path == "/rest/system/resume"):
			if got := r.URL.Query().Get("device"); got != "device-1" {
				t.Errorf("device query = %q, want device-1", got)
			}
		case r.Method == http.MethodGet && (r.URL.Path == "/rest/config/folders" || r.URL.Path == "/rest/config/devices"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
			_, _ = w.Write([]byte(`{"myID":"node-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/rest/system/connections":
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected Syncthing request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.PutFolder(ctx, "folder-1", json.RawMessage(`{"id":"folder-1","path":"/workspace"}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.PutDevice(ctx, "device-1", json.RawMessage(`{"deviceID":"device-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.PauseDevice(ctx, "device-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.ResumeDevice(ctx, "device-1"); err != nil {
		t.Fatal(err)
	}
	if folders, err := client.GetFolders(ctx); err != nil || len(folders) != 0 {
		t.Fatalf("GetFolders() = %v, error = %v", folders, err)
	}
	if devices, err := client.GetDevices(ctx); err != nil || len(devices) != 0 {
		t.Fatalf("GetDevices() = %v, error = %v", devices, err)
	}
	if _, err := client.SystemStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Connections(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 8 {
		t.Fatalf("request count = %d, want 8", calls)
	}
}

func TestSyncthingAPIClientReadOnlyWorkspaceObservation(t *testing.T) {
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("X-API-Key") != testSyncthingAPIKey {
			t.Errorf("request = %s %s, want authenticated GET", r.Method, r.URL.String())
		}
		switch {
		case r.URL.Path == "/rest/system/connections":
			_, _ = w.Write([]byte(`{"connections":{"BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB":{"connected":false},"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA":{"connected":true}}}`))
		case r.URL.Path == "/rest/config/folders":
			_, _ = w.Write([]byte(`[{"id":"` + folderID + `"},{"id":"local-other"}]`))
		case r.URL.Path == "/rest/db/status":
			if r.URL.Query().Get("folder") != folderID {
				t.Errorf("folder query = %q, want %q", r.URL.Query().Get("folder"), folderID)
			}
			_, _ = w.Write([]byte(`{"state":"future-state","needBytes":9,"needFiles":3}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := client.ConnectedDeviceIDs(context.Background())
	if err != nil || len(connected) != 1 || connected[0] != "AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA" {
		t.Fatalf("ConnectedDeviceIDs() = %v, error = %v", connected, err)
	}
	folders, err := client.WorkspaceFolderStatuses(context.Background())
	if err != nil || len(folders) != 1 {
		t.Fatalf("WorkspaceFolderStatuses() = %v, error = %v", folders, err)
	}
	if folders[0].FolderID != folderID || folders[0].State != "unknown" || folders[0].NeedBytes != 9 || folders[0].NeedFiles != 3 {
		t.Fatalf("WorkspaceFolderStatuses() = %+v", folders[0])
	}
	if calls.Load() != 3 {
		t.Fatalf("request count = %d, want 3", calls.Load())
	}
}

func TestSyncthingAPIClientRejectsInvalidConfigLists(t *testing.T) {
	for _, response := range []string{"null", "{}", `[null]`, `[{"id":""}]`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetFolders(context.Background()); err == nil {
				t.Fatal("malformed folder configuration list was accepted")
			}
		})
	}
}

func TestSyncthingAPIClientDefaultsOptionsAndFolderLifecycle(t *testing.T) {
	var calls int
	var folderPauseRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config/defaults/folder":
			_, _ = w.Write([]byte(`{"path":"/default-folder"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config/defaults/device":
			_, _ = w.Write([]byte(`{"name":"default-device"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config/options":
			_, _ = w.Write([]byte(`{"maxSendKbps":123}`))
		case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
			_, _ = w.Write([]byte(`{"myID":"NODE-1"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/rest/config/options":
			var patch map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("options PATCH decode error = %v", err)
				return
			}
			if _, exists := patch["maxSendKbps"]; exists {
				t.Error("options PATCH replaced an unrelated option")
			}
			var listen []string
			if err := json.Unmarshal(patch["listenAddresses"], &listen); err != nil || len(listen) != 1 ||
				listen[0] != "relay://private.example:22067/?id="+testWorkspaceSyncRelayID+"&token="+workspaceSyncTestRelayToken {
				t.Errorf("private relay listen addresses = %v, error = %v", listen, err)
			}
			for _, field := range []string{"globalAnnounceServers", "stunServers"} {
				var values []string
				if err := json.Unmarshal(patch[field], &values); err != nil || len(values) != 0 {
					t.Errorf("%s = %v, error = %v, want empty list", field, values, err)
				}
			}
			for field, want := range map[string]bool{
				"globalAnnounceEnabled": false,
				"localAnnounceEnabled":  false,
				"natEnabled":            false,
				"relaysEnabled":         true,
			} {
				var got bool
				if err := json.Unmarshal(patch[field], &got); err != nil || got != want {
					t.Errorf("%s = %v, error = %v, want %v", field, got, err, want)
				}
			}
		case r.Method == http.MethodPatch && r.URL.Path == "/rest/config/folders/folder-1":
			folderPauseRequests++
			var patch struct {
				Paused bool `json:"paused"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("folder PATCH decode error = %v", err)
			}
			want := folderPauseRequests == 1
			if patch.Paused != want {
				t.Errorf("folder paused = %v, want %v", patch.Paused, want)
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/rest/config/folders/folder-1":
		case r.Method == http.MethodDelete && r.URL.Path == "/rest/config/devices/device-1":
		default:
			t.Errorf("unexpected Syncthing request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.GetDefaultFolder(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetDefaultDevice(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetOptions(ctx); err != nil {
		t.Fatal(err)
	}
	if nodeID, err := client.NodeID(ctx); err != nil || nodeID != "NODE-1" {
		t.Fatalf("NodeID() = %q, error = %v", nodeID, err)
	}
	if err := client.ConfigurePrivateRelay(ctx, "relay://private.example:22067/?id="+testWorkspaceSyncRelayID+"&token="+workspaceSyncTestRelayToken); err != nil {
		t.Fatal(err)
	}
	if err := client.PauseFolder(ctx, "folder-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.ResumeFolder(ctx, "folder-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveFolder(ctx, "folder-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveDevice(ctx, "device-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 9 {
		t.Fatalf("request count = %d, want 9", calls)
	}
}

func TestSyncthingAPIClientRejectsInvalidPrivateRelayAndOptionsPatch(t *testing.T) {
	client, err := NewSyncthingAPIClient("http://127.0.0.1:8384", testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{
		"", "dynamic+https://relays.example/endpoint", "relay://user:pass@relay.example:22067", "relay://relay.example:0", "relay://relay.example:",
		"relay://relay.example:22067", "relay://relay.example:22067/?id=" + testWorkspaceSyncRelayID,
		"relay://relay.example/?id=" + testWorkspaceSyncRelayID + "&token=" + workspaceSyncTestRelayToken,
		"relay://relay.example:22067/?id=" + testWorkspaceSyncRelayID + "&token=short",
		"relay://relay.example:22067/?id=" + testWorkspaceSyncRelayID + "&id=" + testWorkspaceSyncRelayID + "&token=" + workspaceSyncTestRelayToken,
		"relay://relay.example:22067/?id=" + testWorkspaceSyncRelayID + "&token=" + workspaceSyncTestRelayToken + "&extra=value",
	} {
		if err := client.ConfigurePrivateRelay(context.Background(), address); err == nil {
			t.Errorf("ConfigurePrivateRelay(%q) error = nil, want invalid relay address", address)
		}
	}
	for _, patch := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{}`), json.RawMessage(`invalid`)} {
		if err := client.PatchOptions(context.Background(), patch); err == nil {
			t.Errorf("PatchOptions(%q) error = nil, want invalid patch", patch)
		}
	}
}

func TestSyncthingAPIClientStatusErrorsAreSanitized(t *testing.T) {
	const responseSecret = "response-body-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, responseSecret+" "+testSyncthingAPIKey, http.StatusForbidden)
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetFolder(context.Background(), "folder-1")
	if err == nil {
		t.Fatal("GetFolder() error = nil, want status error")
	}
	message := err.Error()
	for _, forbidden := range []string{testSyncthingAPIKey, responseSecret, server.URL} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("error %q leaked %q", message, forbidden)
		}
	}
	if !strings.Contains(message, "403") {
		t.Fatalf("error %q does not include HTTP status", message)
	}
}

func TestSyncthingAPIClientResponseBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", syncthingAPIResponseLimit+1)))
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SystemStatus(context.Background())
	if !errors.Is(err, errSyncthingAPIResponseMax) {
		t.Fatalf("SystemStatus() error = %v, want response size limit", err)
	}
}

func TestSyncthingAPIClientRequestBodyLimit(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	config := json.RawMessage(fmt.Sprintf(`{"id":"folder-1","extra":"%s"}`, strings.Repeat("x", syncthingAPIRequestLimit)))
	err = client.PutFolder(context.Background(), "folder-1", config)
	if !errors.Is(err, errSyncthingAPIRequestMax) {
		t.Fatalf("PutFolder() error = %v, want request size limit", err)
	}
	if calls != 0 {
		t.Fatalf("request count = %d, want no request", calls)
	}
}

func TestSyncthingAPIClientCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := NewSyncthingAPIClient(server.URL, testSyncthingAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.SystemStatus(ctx)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach test server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SystemStatus() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not cancel")
	}
}
