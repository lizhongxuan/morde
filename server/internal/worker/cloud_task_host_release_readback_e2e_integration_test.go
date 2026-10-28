package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	domainTemplate "morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

func TestF01HostReleaseKeepsCommittedWorkspaceReadback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, _ := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, _, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	sandbox := &f01ReleaseSandboxClient{value: app.Sandbox{
		ID: hostValue.SandboxID, State: app.SandboxRunning,
		Metadata: map[string]string{
			"ch-env": "f01-release-readback-test", "ch-host": hostValue.ID,
			"ch-generation": fmt.Sprint(hostValue.RuntimeGeneration),
		},
	}}
	sandboxes := f01ReleaseSandboxRouter{client: sandbox}
	volumes := &f01ReleaseVolumeRecorder{}
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, binding.DesiredRevision, binding.AppliedRevision)
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)

	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close test Workspace blocks: %v", err)
		}
	}()
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	hostService, err := app.NewHostService(app.HostServiceOptions{
		Hosts: store, Users: store, Workspaces: store, Tokens: store,
		Sandboxes: sandboxes,
		Volumes:   volumes, DiskCapacity: f01ReleaseNoopCapacity{},
		Probe: f01ReleaseNoopProbe{}, Runtime: f01ReleaseNoopRuntime{}, Audit: auditService,
		CreateLocks: store, Sealer: sealer,
		Templates:         map[string]domainTemplate.Template{"f01-local-template": f01ReleaseTemplate()},
		DefaultTemplateID: "f01-local-template", Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
		Instance: "f01-release-readback-test", Scheme: "https", PortalURL: "https://portal.example.test", HostSuffix: "hosts.example.test",
		TicketTTL: time.Minute, ReadyTimeout: time.Second, ReadyPollInterval: time.Millisecond,
		NotifyCreate: func(context.Context, string) error { return nil }, ObserveHostReady: func(string, time.Duration) {},
		CountAuditFailure: func() {}, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.NewRuntime(store, sandboxes, sealer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workerApp, err := app.NewWorkerApp(hostService, runtime, store, store, store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	apiRouter, err := api.New(api.Deps{
		Hosts: hostService, HostReplicas: hostReplicas, TLSAuthorizer: hostService,
		Verifier: credentials, Auth: f01UnusedAuth{}, Tasks: taskService, WorkspaceSync: syncService, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "hosts.example.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(apiRouter)
	defer portal.Close()
	client := portal.Client()

	content := []byte("This committed Workspace file remains readable after Host release.\n")
	chunkDigest := sha256.Sum256(content)
	digest := hex.EncodeToString(chunkDigest[:])
	file := workspace.File{
		Path: "reports/release-readback.txt", SizeBytes: int64(len(content)), SHA256: digest, Mode: 0o644,
		Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: int64(len(content))}},
	}
	createUpload, err := json.Marshal(workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	var uploadSession workspace.SyncSession
	responseBody := f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		"/api/workspaces/"+workspaceValue.ID+"/sync-sessions", createUpload, "application/json", "f01-release-readback-upload", http.StatusCreated)
	if err := json.Unmarshal(responseBody, &uploadSession); err != nil || uploadSession.ID == "" {
		t.Fatalf("decode Workspace upload session = %#v, err=%v", uploadSession, err)
	}
	f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPut,
		fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/blocks/%s", workspaceValue.ID, uploadSession.ID, digest),
		content, "application/octet-stream", "", http.StatusNoContent)
	responseBody = f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/commit", workspaceValue.ID, uploadSession.ID),
		nil, "", "f01-release-readback-commit", http.StatusOK)
	var committed workspace.SyncSession
	if err := json.Unmarshal(responseBody, &committed); err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("Workspace upload commit = state %q, err=%v", committed.State, err)
	}
	workspaceBefore, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceBefore.Revision <= 0 || workspaceBefore.State != workspace.StateReady {
		t.Fatalf("Workspace after committed upload = state %q revision %d, err=%v", workspaceBefore.State, workspaceBefore.Revision, err)
	}
	bindingBeforeRelease, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil || bindingBeforeRelease.State != workspace.BindingActive || bindingBeforeRelease.DesiredRevision != workspaceBefore.Revision {
		t.Fatalf("Host Workspace binding before release = %#v, err=%v; Workspace revision %d", bindingBeforeRelease, err, workspaceBefore.Revision)
	}

	// Create a download transfer for the committed revision so the Morde block
	// readback uses the same manifest and binary-block Portal API as a replica.
	createDownload, err := json.Marshal(workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var downloadSession workspace.SyncSession
	responseBody = f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		"/api/workspaces/"+workspaceValue.ID+"/sync-sessions", createDownload, "application/json", "f01-release-readback-download", http.StatusCreated)
	if err := json.Unmarshal(responseBody, &downloadSession); err != nil || downloadSession.ID == "" {
		t.Fatalf("decode Workspace download session = %#v, err=%v", downloadSession, err)
	}

	manifestBefore := f01ReleaseReadbackManifest(t, ctx, client, portal.URL, sessionCredential, workspaceValue.ID)
	if manifestBefore.Revision != workspaceBefore.Revision || len(manifestBefore.Files) != 1 || !reflect.DeepEqual(manifestBefore.Files[0], file) {
		t.Fatalf("Workspace manifest before Host release = %#v; expected revision %d with committed file %#v", manifestBefore, workspaceBefore.Revision, file)
	}
	blockPath := fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/blocks/%s", workspaceValue.ID, downloadSession.ID, digest)
	blockBefore := f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodGet,
		blockPath, nil, "", "", http.StatusOK)
	if !bytes.Equal(blockBefore, content) {
		t.Fatalf("Workspace block before Host release = %q, want %q", blockBefore, content)
	}

	releaseBody := []byte(`{"confirm_process_termination":true}`)
	releaseResponse := f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		"/api/hosts/"+hostValue.ID+"/release", releaseBody, "application/json", "", http.StatusAccepted)
	var released struct {
		ID    string     `json:"id"`
		State host.State `json:"state"`
	}
	if err := json.Unmarshal(releaseResponse, &released); err != nil || released.ID != hostValue.ID || released.State != host.StateStopping {
		t.Fatalf("Host release API response = %#v, err=%v; want Host %s stopping", released, err, hostValue.ID)
	}
	actions, err := workerApp.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile Host release: %v", err)
	}
	stoppedHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || stoppedHost.State != host.StateStopped || actions["release"] != 1 ||
		sandbox.deletes != 1 || len(volumes.deleted) != 0 {
		t.Fatalf("Host release = host %#v, actions %#v, sandbox deletes %d, volume deletes %v, err %v",
			stoppedHost, actions, sandbox.deletes, volumes.deleted, err)
	}

	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.State != workspaceBefore.State || workspaceAfter.Revision != workspaceBefore.Revision {
		t.Fatalf("Workspace after successful Host release = state %q revision %d, err=%v; before state %q revision %d",
			workspaceAfter.State, workspaceAfter.Revision, err, workspaceBefore.State, workspaceBefore.Revision)
	}
	bindingAfter, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil || bindingAfter.State != bindingBeforeRelease.State || bindingAfter.DesiredRevision != bindingBeforeRelease.DesiredRevision ||
		bindingAfter.AppliedRevision != bindingBeforeRelease.AppliedRevision {
		t.Fatalf("Host Workspace binding after release = %#v, err=%v; before release %#v", bindingAfter, err, bindingBeforeRelease)
	}
	manifestAfter := f01ReleaseReadbackManifest(t, ctx, client, portal.URL, sessionCredential, workspaceValue.ID)
	if manifestAfter.Revision != manifestBefore.Revision || !reflect.DeepEqual(manifestAfter.Files, manifestBefore.Files) {
		t.Fatalf("Workspace manifest changed after Host release: before %#v, after %#v", manifestBefore, manifestAfter)
	}
	blockAfter := f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodGet,
		blockPath, nil, "", "", http.StatusOK)
	if !bytes.Equal(blockAfter, blockBefore) || !bytes.Equal(blockAfter, content) {
		t.Fatalf("Workspace block after Host release = %q; before %q, committed content %q", blockAfter, blockBefore, content)
	}
}

type f01ReleaseSandboxRouter struct{ client *f01ReleaseSandboxClient }

func (r f01ReleaseSandboxRouter) Profiles() []string { return []string{"f01-local"} }
func (r f01ReleaseSandboxRouter) For(profile string) (app.SandboxClient, error) {
	if profile != "f01-local" {
		return nil, app.ErrSandboxUnavailable
	}
	return r.client, nil
}

type f01ReleaseSandboxClient struct {
	app.SandboxClient
	value   app.Sandbox
	deletes int
}

func (c *f01ReleaseSandboxClient) List(_ context.Context, metadata map[string]string) ([]app.Sandbox, error) {
	for key, value := range metadata {
		if c.value.Metadata[key] != value {
			return nil, nil
		}
	}
	return []app.Sandbox{c.value}, nil
}
func (c *f01ReleaseSandboxClient) Get(_ context.Context, id string) (app.Sandbox, error) {
	if id != c.value.ID {
		return app.Sandbox{}, app.ErrSandboxNotFound
	}
	return c.value, nil
}
func (c *f01ReleaseSandboxClient) Delete(_ context.Context, id string) error {
	if id != c.value.ID {
		return app.ErrSandboxNotFound
	}
	c.deletes++
	c.value.State = app.SandboxTerminated
	return nil
}

type f01ReleaseVolumeRecorder struct{ deleted []string }

func (v *f01ReleaseVolumeRecorder) DeleteVolume(_ context.Context, name string) error {
	v.deleted = append(v.deleted, name)
	return nil
}

func f01ReleaseReadbackManifest(t *testing.T, ctx context.Context, client *http.Client, baseURL, credential, workspaceID string) workspace.ManifestPage {
	t.Helper()
	body := f01ReleaseReadbackRequest(t, ctx, client, baseURL, credential, http.MethodGet,
		"/api/workspaces/"+workspaceID+"/manifest", nil, "", "", http.StatusOK)
	var manifest workspace.ManifestPage
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode Workspace manifest: %v", err)
	}
	return manifest
}

func f01ReleaseReadbackRequest(t *testing.T, ctx context.Context, client *http.Client, baseURL, credential, method, path string,
	body []byte, contentType, idempotencyKey string, wantStatus int) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
	request.Header.Set("Origin", baseURL)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close %s %s response body: %v", method, path, err)
		}
	}()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s = status %d body %s; want %d", method, path, response.StatusCode, responseBody, wantStatus)
	}
	return responseBody
}
