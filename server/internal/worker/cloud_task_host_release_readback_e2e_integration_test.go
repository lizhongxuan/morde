package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	mordesync "morde-sync/client"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	domainTemplate "morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

func TestF01HostReleaseKeepsCommittedWorkspaceReadback(t *testing.T) {
	timeout := time.Minute
	if os.Getenv(f01RealOpenSandboxEnv) == "1" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cacheRoot := t.TempDir()
	t.Setenv("HOME", filepath.Join(cacheRoot, "home-before-release"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cacheRoot, "cache"))
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, _, replicaCredential := createF01Host(t, ctx, store, sealer, owner.ID)
	aioKeyEnc, err := sealer.Seal([]byte("f01-release-readback-aio-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET aio_key_enc=$2 WHERE id=$1`, hostValue.ID, aioKeyEnc); err != nil {
		clear(aioKeyEnc)
		t.Fatalf("seal Host AIO key for restore fixture: %v", err)
	}
	clear(aioKeyEnc)
	sandbox := &f01ReleaseSandboxClient{value: app.Sandbox{
		ID: hostValue.SandboxID, State: app.SandboxRunning,
		Metadata: map[string]string{
			"ch-env": "f01-release-readback-test", "ch-host": hostValue.ID,
			"ch-generation": fmt.Sprint(hostValue.RuntimeGeneration),
		},
	}}
	localOpenSandbox, useRealOpenSandbox := newF01LocalOpenSandboxFixture(t, hostValue.ID, owner.ID)
	instance := "f01-release-readback-test"
	template := f01ReleaseTemplate()
	volumes := &f01ReleaseVolumeRecorder{}
	if useRealOpenSandbox {
		instance = localOpenSandbox.instance
		template = localOpenSandbox.runtimeTemplate(template)
		initialSandbox := localOpenSandbox.createInitial(ctx, template, hostValue.RuntimeGeneration)
		sandbox.value = initialSandbox
		sandbox.delegate = localOpenSandbox.client
		sandbox.onCreate = localOpenSandbox.recordCreate
		hostValue.SandboxID = initialSandbox.ID
		result, err := pool.Exec(ctx, `UPDATE hosts SET sandbox_id=$2 WHERE id=$1`, hostValue.ID, initialSandbox.ID)
		if err != nil || result.RowsAffected() != 1 {
			t.Fatalf("bind actual local OpenSandbox ID to Host fixture: rows=%d err=%v", result.RowsAffected(), err)
		}
		volumes.delegate = localOpenSandbox.volumes
	}
	sandboxes := f01ReleaseSandboxRouter{client: sandbox}
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
		Templates:         map[string]domainTemplate.Template{"f01-local-template": template},
		DefaultTemplateID: "f01-local-template", Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
		Instance: instance, Scheme: "https", PortalURL: "https://portal.example.test", HostSuffix: "hosts.example.test",
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

	// Keep one file only on the persistent Host Workspace root. Stop the first
	// sync process while disconnected so release cannot mistake it for a commit.
	syncCredential, syncBinding := f01BindingCredential(t, client, portal.URL, hostValue, replicaCredential, workspaceValue.ID)
	workspaceDirectory := filepath.Join(cacheRoot, "workspace-root-volume", workspaceValue.ID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	initialSync, err := mordesync.New(portal.URL, syncCredential, client)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := int64(0)
	initialDownload, err := initialSync.Sync(ctx, mordesync.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: workspaceDirectory, Direction: "download", Policy: "fill_missing", BaseRevision: &baseRevision,
	})
	if err != nil || initialDownload.Session.State != "completed" || initialDownload.Session.TargetRevision != workspaceBefore.Revision {
		t.Fatalf("Host Workspace baseline download = %#v, %v", initialDownload, err)
	}
	if err := initialSync.ConfirmDownloadedLiveBaseline(ctx, workspaceValue.ID, workspaceDirectory, initialDownload.Session.TargetRevision); err != nil {
		t.Fatalf("confirm Host Workspace baseline: %v", err)
	}
	if err := f01RecordApplied(client, portal.URL, hostValue, replicaCredential, workspaceValue.ID,
		syncBinding.BindingVersion, initialDownload.Session.ID, initialDownload.Session.TargetRevision); err != nil {
		t.Fatalf("record Host Workspace baseline: %v", err)
	}
	bindingBeforeRelease, err = store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil {
		t.Fatalf("read Host Workspace binding after baseline sync: %v", err)
	}
	var offline atomic.Bool
	refused := make(chan struct{}, 1)
	firstSync, err := mordesync.New(portal.URL, syncCredential, &http.Client{Transport: f01ReleaseNetworkGate{offline: &offline, refused: refused}})
	if err != nil {
		t.Fatal(err)
	}
	firstSyncCtx, stopFirstSync := context.WithCancel(ctx)
	defer stopFirstSync()
	firstWatching, firstDone := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		firstDone <- firstSync.LiveSync(firstSyncCtx, mordesync.LiveSyncOptions{
			WorkspaceID: workspaceValue.ID, Directory: workspaceDirectory, RescanInterval: 5 * time.Second,
			Progress: func(progress mordesync.Progress) {
				if progress.Stage == "watch" {
					select {
					case firstWatching <- struct{}{}:
					default:
					}
				}
			},
		})
	}()
	select {
	case <-firstWatching:
	case err := <-firstDone:
		t.Fatalf("first morde-sync stopped before watching: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out starting first morde-sync")
	}
	offline.Store(true)
	pendingPath := "reports/pending-after-release.txt"
	pendingContent := []byte("preserve and upload after Host restore\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(workspaceDirectory, pendingPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDirectory, pendingPath), pendingContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if useRealOpenSandbox {
		// morde-sync runs against the test-process directory; mirror its pending
		// bytes into the actual named Host volume for the delete/recreate check.
		localOpenSandbox.writeWorkspaceFileBeforeRelease(ctx, pendingPath, pendingContent)
	}
	select {
	case <-refused:
	case err := <-firstDone:
		t.Fatalf("first morde-sync stopped during outage: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first morde-sync did not retry the uncommitted file while offline")
	}
	if got, err := os.ReadFile(filepath.Join(workspaceDirectory, pendingPath)); err != nil || !bytes.Equal(got, pendingContent) {
		t.Fatalf("uncommitted Host file before release = %q, %v", got, err)
	}
	manifestDuringOutage := f01ReleaseReadbackManifest(t, ctx, client, portal.URL, sessionCredential, workspaceValue.ID)
	if manifestDuringOutage.Revision != manifestBefore.Revision || !reflect.DeepEqual(manifestDuringOutage.Files, manifestBefore.Files) {
		t.Fatalf("offline Host file reached Workspace before release: before %#v, during outage %#v", manifestBefore, manifestDuringOutage)
	}
	stopFirstSync()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first morde-sync cancellation error = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first morde-sync did not stop before Host release")
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
	if useRealOpenSandbox {
		localOpenSandbox.writeMarkerAfterRelease(ctx)
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

	// Restore the same Host identity and ensure its workspace-root volume claim
	// is stable before a fresh sync process resumes the uncommitted file.
	f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		"/api/hosts/"+hostValue.ID+"/restore", nil, "", "", http.StatusAccepted)
	if _, err := workerApp.AdvanceCreate(ctx, hostValue.ID); err != nil {
		t.Fatalf("restore Host after release: %v", err)
	}
	restoredHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || restoredHost.State != host.StateRunning || restoredHost.RuntimeGeneration != hostValue.RuntimeGeneration+1 ||
		sandbox.creates != 1 || len(sandbox.createRequests) != 1 || len(volumes.deleted) != 0 {
		t.Fatalf("restored Host/runtime/volume lifecycle = host %#v creates %d requests %#v deleted volumes %v err %v",
			restoredHost, sandbox.creates, sandbox.createRequests, volumes.deleted, err)
	}
	if got := sandbox.createRequests[0].Entrypoint; !reflect.DeepEqual(got, []string{domainTemplate.RuntimeBootstrapEntrypoint}) {
		t.Fatalf("Morde Host restore request entrypoint = %#v; want %q", got, domainTemplate.RuntimeBootstrapEntrypoint)
	}
	wantWorkspaceVolume := domain.WorkspaceRootVolumeName(hostValue.ID)
	var workspaceVolumeFound bool
	for _, mounted := range sandbox.createRequests[0].Volumes {
		if mounted.Name == "workspace-root" && mounted.ClaimName == wantWorkspaceVolume {
			workspaceVolumeFound = true
		}
	}
	if !workspaceVolumeFound {
		t.Fatalf("restored Host did not reuse Workspace volume %q: %#v", wantWorkspaceVolume, sandbox.createRequests[0].Volumes)
	}
	if useRealOpenSandbox {
		localOpenSandbox.verifyMarkerAfterRestore(ctx)
		// Check volume persistence before the test-process sync resumes and commits it.
		localOpenSandbox.verifyWorkspaceFileAfterRestore(ctx, pendingPath, pendingContent)
	}
	if got, err := os.ReadFile(filepath.Join(workspaceDirectory, pendingPath)); err != nil || !bytes.Equal(got, pendingContent) {
		t.Fatalf("uncommitted Host file after restore = %q, %v", got, err)
	}
	newRuntimeCredential, err := sealer.Open(restoredHost.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, newRuntimeCredential)
	_, _ = mac.Write([]byte("morde-host-workspace-replica-v1"))
	newReplicaCredential := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	newSyncCredential, newBinding := f01BindingCredential(t, client, portal.URL, restoredHost, newReplicaCredential, workspaceValue.ID)
	clear(newRuntimeCredential)
	resumedSync, err := mordesync.New(portal.URL, newSyncCredential, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumedSync.ValidateLiveBaseline(ctx, workspaceValue.ID, workspaceDirectory, nil); err != nil {
		t.Fatalf("restored Host could not load the persisted live baseline: %v", err)
	}
	resumedCtx, stopResumed := context.WithCancel(ctx)
	defer stopResumed()
	resumedDone := make(chan error, 1)
	uploaded := make(chan struct{}, 1)
	applyErr := make(chan error, 1)
	go func() {
		resumedDone <- resumedSync.LiveSync(resumedCtx, mordesync.LiveSyncOptions{
			WorkspaceID: workspaceValue.ID, Directory: workspaceDirectory, RescanInterval: 5 * time.Second,
			OnResult: func(result mordesync.Result) {
				if result.Session.State != "completed" || result.Session.TargetRevision <= workspaceBefore.Revision {
					return
				}
				if err := f01RecordApplied(client, portal.URL, restoredHost, newReplicaCredential, workspaceValue.ID,
					newBinding.BindingVersion, result.Session.ID, result.Session.TargetRevision); err != nil {
					select {
					case applyErr <- err:
					default:
					}
					return
				}
				select {
				case uploaded <- struct{}{}:
				default:
				}
			},
		})
	}()
	select {
	case err := <-applyErr:
		stopResumed()
		t.Fatalf("record restored Host sync receipt: %v", err)
	case <-uploaded:
	case err := <-resumedDone:
		t.Fatalf("restored morde-sync stopped before uploading the file: %v", err)
	case <-time.After(15 * time.Second):
		stopResumed()
		t.Fatal("restored morde-sync did not upload the uncommitted Host file")
	}
	stopResumed()
	select {
	case err := <-resumedDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restored morde-sync cancellation error = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("restored morde-sync did not stop")
	}

	manifestFinal := f01ReleaseReadbackManifest(t, ctx, client, portal.URL, sessionCredential, workspaceValue.ID)
	var pendingFile *workspace.File
	for i := range manifestFinal.Files {
		if manifestFinal.Files[i].Path == pendingPath {
			pendingFile = &manifestFinal.Files[i]
			break
		}
	}
	pendingDigest := sha256.Sum256(pendingContent)
	if manifestFinal.Revision <= manifestBefore.Revision || pendingFile == nil || pendingFile.SizeBytes != int64(len(pendingContent)) ||
		pendingFile.SHA256 != hex.EncodeToString(pendingDigest[:]) {
		t.Fatalf("Workspace after restored morde-sync = %#v, pending file %#v", manifestFinal, pendingFile)
	}
	finalDownload, err := json.Marshal(workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	})
	if err != nil {
		t.Fatal(err)
	}
	responseBody = f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodPost,
		"/api/workspaces/"+workspaceValue.ID+"/sync-sessions", finalDownload, "application/json", "f01-release-readback-final-download", http.StatusCreated)
	var finalDownloadSession workspace.SyncSession
	if err := json.Unmarshal(responseBody, &finalDownloadSession); err != nil || finalDownloadSession.ID == "" {
		t.Fatalf("decode final Workspace download session = %#v, err=%v", finalDownloadSession, err)
	}
	readbackPath := fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/blocks/%s", workspaceValue.ID, finalDownloadSession.ID, pendingFile.SHA256)
	pendingReadback := f01ReleaseReadbackRequest(t, ctx, client, portal.URL, sessionCredential, http.MethodGet,
		readbackPath, nil, "", "", http.StatusOK)
	if !bytes.Equal(pendingReadback, pendingContent) || len(volumes.deleted) != 0 {
		t.Fatalf("restored Workspace file readback = %q, want %q; deleted volumes %v", pendingReadback, pendingContent, volumes.deleted)
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
	delegate       app.SandboxClient
	onCreate       func(app.Sandbox, error)
	value          app.Sandbox
	deletes        int
	creates        int
	createRequests []app.SandboxCreateRequest
}

func (c *f01ReleaseSandboxClient) List(ctx context.Context, metadata map[string]string) ([]app.Sandbox, error) {
	if c.delegate != nil {
		return c.delegate.List(ctx, metadata)
	}
	for key, value := range metadata {
		if c.value.Metadata[key] != value {
			return nil, nil
		}
	}
	return []app.Sandbox{c.value}, nil
}
func (c *f01ReleaseSandboxClient) Get(ctx context.Context, id string) (app.Sandbox, error) {
	if c.delegate != nil {
		return c.delegate.Get(ctx, id)
	}
	if id != c.value.ID {
		return app.Sandbox{}, app.ErrSandboxNotFound
	}
	return c.value, nil
}
func (c *f01ReleaseSandboxClient) Delete(ctx context.Context, id string) error {
	if id != c.value.ID {
		if c.delegate == nil {
			return app.ErrSandboxNotFound
		}
	}
	c.deletes++
	if c.delegate != nil {
		return c.delegate.Delete(ctx, id)
	}
	c.value.State = app.SandboxTerminated
	return nil
}

func (c *f01ReleaseSandboxClient) Create(ctx context.Context, request app.SandboxCreateRequest) (app.Sandbox, error) {
	c.creates++
	c.createRequests = append(c.createRequests, request)
	if c.delegate != nil {
		// Keep HostService's Morde bootstrap entrypoint unchanged. Disable the
		// browser and Jupyter prewarm used by the local image smoke setup.
		env := make(map[string]string, len(request.Env)+2)
		for key, value := range request.Env {
			env[key] = value
		}
		env["DISABLE_BROWSER"] = "true"
		env["JUPYTER_POOL_SIZE"] = "0"
		request.Env = env
		value, err := c.delegate.Create(ctx, request)
		if c.onCreate != nil {
			c.onCreate(value, err)
		}
		if err == nil {
			c.value = value
		}
		return value, err
	}
	c.value = app.Sandbox{ID: fmt.Sprintf("f01-release-restore-%d", c.creates), State: app.SandboxRunning, Metadata: request.Metadata}
	return c.value, nil
}

func (c *f01ReleaseSandboxClient) Endpoint(ctx context.Context, id string, port int) (app.Upstream, error) {
	if c.delegate != nil {
		return c.delegate.Endpoint(ctx, id, port)
	}
	return app.Upstream{BaseURL: "https://host.example.test"}, nil
}

type f01ReleaseNetworkGate struct {
	offline *atomic.Bool
	refused chan<- struct{}
}

func (gate f01ReleaseNetworkGate) RoundTrip(request *http.Request) (*http.Response, error) {
	if gate.offline.Load() {
		select {
		case gate.refused <- struct{}{}:
		default:
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return http.DefaultTransport.RoundTrip(request)
}

type f01ReleaseVolumeRecorder struct {
	delegate app.VolumeDeleter
	deleted  []string
}

func (v *f01ReleaseVolumeRecorder) DeleteVolume(ctx context.Context, name string) error {
	v.deleted = append(v.deleted, name)
	if v.delegate != nil {
		return v.delegate.DeleteVolume(ctx, name)
	}
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
