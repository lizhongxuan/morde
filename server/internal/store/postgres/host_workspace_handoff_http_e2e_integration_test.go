package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde-sync/client"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

func TestHostWorkspaceHandoffAtoBThroughHTTPAndPostgres(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "host-handoff-http-e2e")
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	hostA, rootCredentialA := createHostHandoffTestHost(t, ctx, store, sealer, owner.ID, capacity, []string{workspaceValue.ID})
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts:         unusedWorkspaceSyncHostService{},
		HostReplicas:  hostReplicas,
		TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier:      unusedWorkspaceSyncVerifier{},
		Auth:          unusedWorkspaceSyncAuth{},
		WorkspaceSync: syncService,
		Tasks:         unusedWorkspaceSyncTasks{},
		Conversations: unusedWorkspaceSyncConversations{},
		MCPDisabled:   true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20,
		MCPMaxBodyBytes: 16 << 20, SessionTTL: time.Hour, HostScheme: "https",
		PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)
	httpClient := httpServer.Client()

	zeroRevision := int64(0)
	credentialA, bindingA := hostHandoffBindingCredential(t, httpClient, httpServer.URL, hostA, rootCredentialA, workspaceValue.ID)
	directoryA := t.TempDir()
	clientA, err := client.New(httpServer.URL, credentialA, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	initialA, err := clientA.Sync(ctx, client.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: directoryA, Direction: "download", Policy: "fill_missing", BaseRevision: &zeroRevision,
	})
	if err != nil || initialA.Session.State != "completed" || initialA.Session.TargetRevision != 0 {
		t.Fatalf("Host A initial download = %#v, %v", initialA.Session, err)
	}
	hostHandoffRecordApplied(t, httpClient, httpServer.URL, hostA, bindingA.BindingVersion, workspaceValue.ID,
		rootCredentialA, initialA.Session.ID, initialA.Session.TargetRevision)
	hostHandoffActivate(t, httpClient, httpServer.URL, hostA, bindingA.BindingVersion, workspaceValue.ID, rootCredentialA)

	credentialA, bindingA = hostHandoffBindingCredential(t, httpClient, httpServer.URL, hostA, rootCredentialA, workspaceValue.ID)
	clientA, err = client.New(httpServer.URL, credentialA, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	var initialCheckpoint client.SyncSession
	initialRevision, err := clientA.HandoffCheckpoint(ctx, client.LiveSyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: directoryA,
		OnResult: func(result client.Result) { initialCheckpoint = result.Session },
	})
	if err != nil || initialRevision != 0 || initialCheckpoint.ID == "" {
		t.Fatalf("Host A initial common checkpoint = revision %d session %#v, %v", initialRevision, initialCheckpoint, err)
	}
	hostHandoffRecordApplied(t, httpClient, httpServer.URL, hostA, bindingA.BindingVersion, workspaceValue.ID,
		rootCredentialA, initialCheckpoint.ID, initialCheckpoint.TargetRevision)

	writeWorkspaceSyncE2EFile(t, directoryA, "before-leaving.txt", []byte("checkpointed on Host A\n"))
	activeBindingA, err := store.GetWorkspaceBinding(ctx, hostA.ID, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	unbindingA, err := store.TransitionWorkspaceBinding(ctx, hostA.ID, workspaceValue.ID, activeBindingA.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, activeBindingA.DesiredRevision, activeBindingA.AppliedRevision)
	if err != nil || unbindingA.State != workspace.BindingUnbinding {
		t.Fatalf("begin Host A unbinding = %#v, %v", unbindingA, err)
	}
	credentialA, unbindingCredential := hostHandoffBindingCredential(t, httpClient, httpServer.URL, hostA, rootCredentialA, workspaceValue.ID)
	clientA, err = client.New(httpServer.URL, credentialA, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	var finalCheckpoint client.SyncSession
	checkpointRevision, err := clientA.HandoffCheckpoint(ctx, client.LiveSyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: directoryA,
		OnResult: func(result client.Result) { finalCheckpoint = result.Session },
	})
	if err != nil || checkpointRevision != 1 || finalCheckpoint.ID == "" || finalCheckpoint.TargetRevision != checkpointRevision {
		t.Fatalf("Host A final checkpoint = revision %d session %#v, %v", checkpointRevision, finalCheckpoint, err)
	}
	hostHandoffRecordApplied(t, httpClient, httpServer.URL, hostA, unbindingCredential.BindingVersion, workspaceValue.ID,
		rootCredentialA, finalCheckpoint.ID, checkpointRevision)
	hostHandoffDetach(t, httpClient, httpServer.URL, hostA, unbindingCredential.BindingVersion, workspaceValue.ID, rootCredentialA, checkpointRevision)

	staleCreate := []byte(fmt.Sprintf(`{"direction":"bidirectional","policy":"live","base_revision":%d,"files":[]}`, checkpointRevision))
	staleResponse := hostHandoffRequest(t, httpClient, httpServer.URL, http.MethodPost,
		"/api/workspaces/"+workspaceValue.ID+"/sync-sessions", credentialA, "stale-a-after-detach", staleCreate)
	_, _ = io.Copy(io.Discard, staleResponse.Body)
	_ = staleResponse.Body.Close()
	if staleResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("detached Host A stale credential sync status = %d, want %d", staleResponse.StatusCode, http.StatusUnauthorized)
	}
	afterStaleA, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || afterStaleA.Revision != checkpointRevision {
		t.Fatalf("stale Host A advanced revision to %d: %v", afterStaleA.Revision, err)
	}

	hostB, rootCredentialB := createHostHandoffTestHost(t, ctx, store, sealer, owner.ID, capacity, nil)
	workspaceValue, err = store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	preparingB, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostB.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: checkpointRevision,
	})
	if err != nil || preparingB.DesiredRevision != checkpointRevision {
		t.Fatalf("bind Workspace to Host B at checkpoint = %#v, %v", preparingB, err)
	}
	credentialB, bindingB := hostHandoffBindingCredential(t, httpClient, httpServer.URL, hostB, rootCredentialB, workspaceValue.ID)
	directoryB := t.TempDir()
	clientB, err := client.New(httpServer.URL, credentialB, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	initialB, err := clientB.Sync(ctx, client.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: directoryB, Direction: "download", Policy: "fill_missing", BaseRevision: &zeroRevision,
	})
	if err != nil || initialB.Session.State != "completed" || initialB.Session.TargetRevision != checkpointRevision {
		t.Fatalf("Host B restore download = %#v, %v", initialB.Session, err)
	}
	if data, err := os.ReadFile(filepath.Join(directoryB, "before-leaving.txt")); err != nil || string(data) != "checkpointed on Host A\n" {
		t.Fatalf("Host B restored checkpoint data = %q, %v", data, err)
	}
	hostHandoffRecordApplied(t, httpClient, httpServer.URL, hostB, bindingB.BindingVersion, workspaceValue.ID,
		rootCredentialB, initialB.Session.ID, checkpointRevision)
	hostHandoffActivate(t, httpClient, httpServer.URL, hostB, bindingB.BindingVersion, workspaceValue.ID, rootCredentialB)

	activeB, err := store.GetWorkspaceBinding(ctx, hostB.ID, workspaceValue.ID)
	if err != nil || activeB.State != workspace.BindingActive || activeB.AppliedRevision != checkpointRevision ||
		activeB.DesiredRevision != checkpointRevision || activeB.Workspace.Revision != checkpointRevision {
		t.Fatalf("Host B binding after verified restore = %#v, %v", activeB, err)
	}
}

func createHostHandoffTestHost(t *testing.T, ctx context.Context, store *Store, sealer *app.Sealer, ownerID string,
	capacity app.Capacity, workspaceIDs []string) (host.Host, string) {
	t.Helper()
	runtimeBytes := make([]byte, 32)
	if _, err := rand.Read(runtimeBytes); err != nil {
		t.Fatal(err)
	}
	runtimeCredential := base64.RawURLEncoding.EncodeToString(runtimeBytes)
	clear(runtimeBytes)
	sealedCredential, err := sealer.Seal([]byte(runtimeCredential))
	if err != nil {
		t.Fatal(err)
	}
	params := testCreateHostParams(t, ownerID, capacity, workspaceIDs)
	params.Host.RuntimeCredentialEnc = sealedCredential
	created, _, err := store.CreateHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	created, err = store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, "")
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(runtimeCredential))
	_, _ = mac.Write([]byte("morde-host-workspace-replica-v1"))
	return created, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func hostHandoffBindingCredential(t *testing.T, httpClient *http.Client, baseURL string, hostValue host.Host,
	rootCredential, workspaceID string) (string, hostHandoffBinding) {
	t.Helper()
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspace-bindings", hostValue.ID, hostValue.RuntimeGeneration)
	response := hostHandoffRequest(t, httpClient, baseURL, http.MethodGet, path, rootCredential, "", nil)
	defer closeHostHandoffResponse(t, response)
	var result struct {
		Items []hostHandoffBinding `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode Host replica bindings: %v", err)
	}
	for _, binding := range result.Items {
		if binding.WorkspaceID == workspaceID {
			if binding.SyncCredential == "" {
				t.Fatal("Host replica binding omitted its sync credential")
			}
			return binding.SyncCredential, binding
		}
	}
	t.Fatalf("Host %s did not return Workspace %s binding", hostValue.ID, workspaceID)
	return "", hostHandoffBinding{}
}

type hostHandoffBinding struct {
	WorkspaceID     string `json:"workspace_id"`
	SyncCredential  string `json:"sync_credential"`
	State           string `json:"state"`
	BindingVersion  int64  `json:"binding_version"`
	DesiredRevision int64  `json:"desired_revision"`
	AppliedRevision int64  `json:"applied_revision"`
}

func hostHandoffRecordApplied(t *testing.T, httpClient *http.Client, baseURL string, hostValue host.Host,
	bindingVersion int64, workspaceID, credential, sessionID string, revision int64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "applied_revision": revision})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspaces/%s/bindings/%d/applied",
		hostValue.ID, hostValue.RuntimeGeneration, workspaceID, bindingVersion)
	response := hostHandoffRequest(t, httpClient, baseURL, http.MethodPost, path, credential, "", body)
	defer closeHostHandoffResponse(t, response)
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("record Host %s applied revision status=%d body=%s", hostValue.ID, response.StatusCode, contents)
	}
}

func hostHandoffActivate(t *testing.T, httpClient *http.Client, baseURL string, hostValue host.Host,
	bindingVersion int64, workspaceID, credential string) {
	t.Helper()
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspaces/%s/bindings/%d/activate",
		hostValue.ID, hostValue.RuntimeGeneration, workspaceID, bindingVersion)
	response := hostHandoffRequest(t, httpClient, baseURL, http.MethodPost, path, credential, "", nil)
	defer closeHostHandoffResponse(t, response)
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("activate Host %s binding status=%d body=%s", hostValue.ID, response.StatusCode, contents)
	}
}

func hostHandoffDetach(t *testing.T, httpClient *http.Client, baseURL string, hostValue host.Host,
	bindingVersion int64, workspaceID, credential string, checkpointRevision int64) {
	t.Helper()
	body, err := json.Marshal(map[string]int64{"checkpoint_revision": checkpointRevision})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspaces/%s/bindings/%d/detached",
		hostValue.ID, hostValue.RuntimeGeneration, workspaceID, bindingVersion)
	response := hostHandoffRequest(t, httpClient, baseURL, http.MethodPost, path, credential, "", body)
	defer closeHostHandoffResponse(t, response)
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("detach Host %s binding status=%d body=%s", hostValue.ID, response.StatusCode, contents)
	}
}

func hostHandoffRequest(t *testing.T, httpClient *http.Client, baseURL string, method, path, credential, idempotencyKey string,
	body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return response
}

func closeHostHandoffResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Errorf("close Host handoff response: %v", err)
	}
}

type unusedWorkspaceSyncVerifier struct{ app.Verifier }
