package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	domainTemplate "morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

// TestTaskWorkspaceFenceBlocksCommitAndHostReleaseWhileFileWriteIsDispatched
// covers the operation-first ordering with the real Portal, PostgreSQL store,
// TaskWorker, RuntimeToolExecutor, and an in-process Morde Host Runtime.
func TestTaskWorkspaceFenceBlocksCommitAndHostReleaseWhileFileWriteIsDispatched(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, runtimeCredential, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate Host Workspace binding: %v", err)
	}

	hostRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostRoot, workspaceValue.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	taskViewRoot := filepath.Join(t.TempDir(), "task-views")
	taskViews, err := runtimefiles.OpenTaskWorkspace(hostRoot, taskViewRoot, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := taskViews.Close(); err != nil {
			t.Errorf("close Host Task Workspace views: %v", err)
		}
	}()
	var heldWrite runtimecore.ActionHandler
	runtimeHandlers := taskViews.Handlers()
	for index, handler := range runtimeHandlers {
		if handler.Capability() == "file.write" && handler.Action() == "write" {
			heldWrite = &f02FenceHeldFileWriteHandler{inner: handler, entered: make(chan struct{}), release: make(chan struct{})}
			runtimeHandlers[index] = heldWrite
			break
		}
	}
	if heldWrite == nil {
		t.Fatal("Host runtime did not register file.write")
	}
	fileWriteGate := heldWrite.(*f02FenceHeldFileWriteHandler)
	defer fileWriteGate.unblock()

	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: hostValue.ID,
		Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: runtimeHandlers, TaskWorkspaceViews: taskViews,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeHandler, err := runtimehttp.NewHandler(runtimeManager, runtimeCredential)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer := httptest.NewUnstartedServer(runtimeHandler)
	runtimeServer.TLS, err = runtimeidentity.ServerTLSConfig(runtimeCredential, hostValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer.StartTLS()
	defer func() {
		runtimeServer.Close()
		if err := runtimeManager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close Host Runtime: %v", err)
		}
	}()

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace block store: %v", err)
		}
	}()
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
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
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: runtimeServer.URL}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeControl.Handshake(ctx, hostValue.ID); err != nil {
		t.Fatalf("handshake with Host Runtime: %v", err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	hostService, err := app.NewHostService(app.HostServiceOptions{
		Hosts: store, Users: store, Workspaces: store, Tokens: store,
		Sandboxes: f01ReleaseSandboxRouter{client: &f01ReleaseSandboxClient{value: app.Sandbox{
			ID: hostValue.SandboxID, State: app.SandboxRunning,
			Metadata: map[string]string{"ch-env": "f01-local", "ch-host": hostValue.ID,
				"ch-generation": fmt.Sprint(hostValue.RuntimeGeneration)},
		}}},
		Volumes: f01ReleaseNoopVolume{}, DiskCapacity: f01ReleaseNoopCapacity{},
		Probe: f01ReleaseNoopProbe{}, Runtime: f01ReleaseNoopRuntime{}, Audit: auditService,
		CreateLocks: store, Sealer: sealer,
		Templates:         map[string]domainTemplate.Template{"f01-local-template": f01ReleaseTemplate()},
		DefaultTemplateID: "f01-local-template", Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
		Instance: "f02-task-workspace-fence-test", Scheme: "https", PortalURL: "https://portal.example.test",
		HostSuffix: "hosts.example.test", TicketTTL: time.Minute, ReadyTimeout: time.Second, ReadyPollInterval: time.Millisecond,
		NotifyCreate: func(context.Context, string) error { return nil }, ObserveHostReady: func(string, time.Duration) {},
		CountAuditFailure: func() {}, Logger: zap.NewNop(),
	})
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
	httpClient := portal.Client()

	manifestFile := []byte("external revision after the Task write settles\n")
	digest := sha256.Sum256(manifestFile)
	digestText := hex.EncodeToString(digest[:])
	file := workspace.File{
		Path: "external-update.txt", SizeBytes: int64(len(manifestFile)), SHA256: digestText, Mode: 0o644,
		Chunks: []workspace.Chunk{{SHA256: digestText, SizeBytes: int64(len(manifestFile))}},
	}
	createSync, err := json.Marshal(workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	createBody := f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPost,
		"/api/workspaces/"+workspaceValue.ID+"/sync-sessions", createSync, "application/json", "f02-fence-external-upload", http.StatusCreated)
	var upload workspace.SyncSession
	if err := json.Unmarshal(createBody, &upload); err != nil || upload.ID == "" {
		t.Fatalf("decode Workspace upload session = %#v, err=%v", upload, err)
	}
	f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPut,
		fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/blocks/%s", workspaceValue.ID, upload.ID, digestText),
		manifestFile, "application/octet-stream", "", http.StatusNoContent)

	modelClient := &f02FenceAgentModelClient{
		workspaceID: workspaceValue.ID, secondTurn: make(chan struct{}, 1),
	}
	agent, err := app.NewAgent(store, f02FenceModelResolver{}, modelClient, credentials, runtimeControl, runtimeTools)
	if err != nil {
		t.Fatal(err)
	}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: agent, WorkerID: "f02-task-workspace-fence-worker",
		LeaseDuration: 2 * time.Second, PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": int64(0),
		"model_id": "p1234567", "objective": "Inspect the Workspace and summarize its contents.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 10_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	request.Header.Set("Origin", portal.URL)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "f02-task-workspace-fence-001")
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) {
		t.Fatalf("POST /api/tasks = status %d, response %#v", response.StatusCode, accepted)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(workerCtx) }()
	workerStopped := false
	defer func() {
		if !workerStopped {
			stopF01Worker(t, cancelWorker, workerDone, "F02 Workspace fence TaskWorker")
		}
	}()
	select {
	case <-fileWriteGate.entered:
	case <-ctx.Done():
		latest, getErr := store.GetTask(context.Background(), accepted.ID, owner.ID)
		t.Fatalf("Task did not reach the held Runtime file.write: model_calls=%d task_status=%q waiting_reason=%q current_step=%q get_err=%v context_err=%v",
			modelClient.calls.Load(), latest.Task.Status, latest.Task.WaitingReason, latest.Task.CurrentStep, getErr, ctx.Err())
	}
	operation, err := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f02-workspace-fence-write-001"))
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("persisted Runtime file.write while held = %#v, err=%v; want dispatched", operation, err)
	}
	current, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || current.Task.Status != task.StatusRunning || current.Task.InputRevision == nil || *current.Task.InputRevision != 0 {
		t.Fatalf("Task at held file.write = %#v, err=%v", current.Task, err)
	}

	releaseBody, err := json.Marshal(map[string]any{"confirm_process_termination": false})
	if err != nil {
		t.Fatal(err)
	}
	releaseResponse := f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPost,
		"/api/hosts/"+hostValue.ID+"/release", releaseBody, "application/json", "", http.StatusConflict)
	if !bytes.Contains(releaseResponse, []byte(`"code":"conflict"`)) {
		t.Fatalf("Host release rejection did not return conflict: %s", releaseResponse)
	}
	hostDuringWrite, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || hostDuringWrite.State != host.StateRunning {
		t.Fatalf("Host release advanced while Runtime file.write was in flight: state=%q err=%v", hostDuringWrite.State, err)
	}

	commitPath := fmt.Sprintf("/api/workspaces/%s/sync-sessions/%s/commit", workspaceValue.ID, upload.ID)
	commitBody := f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPost,
		commitPath, nil, "", "f02-fence-external-commit", http.StatusConflict)
	if !bytes.Contains(commitBody, []byte(`"code":"workspace_busy"`)) {
		t.Fatalf("Workspace commit while Runtime file.write was dispatched returned %s; want workspace_busy", commitBody)
	}
	before, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || before.Revision != 0 {
		t.Fatalf("Workspace revision crossed active file.write barrier: revision=%d err=%v", before.Revision, err)
	}

	fileWriteGate.unblock()
	if err := waitF01(t, ctx, func() (bool, error) {
		operation, err := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f02-workspace-fence-write-001"))
		if err != nil {
			return false, err
		}
		return operation.Status == task.OperationSucceeded, nil
	}, nil, nil); err != nil {
		t.Fatalf("real Runtime file.write did not settle: %v", err)
	}
	fileWriteGate.mu.Lock()
	viewID := fileWriteGate.workspaceViewID
	fileWriteGate.mu.Unlock()
	if viewID == "" {
		t.Fatal("Runtime file.write did not receive a Task-private Workspace view")
	}
	written, err := os.ReadFile(filepath.Join(taskViewRoot, accepted.ID, viewID, "task-owned.txt"))
	if err != nil || string(written) != "task-owned write\n" || fileWriteGate.effects.Load() != 1 {
		t.Fatalf("settled Task-private file.write content=%q err=%v effects=%d", written, err, fileWriteGate.effects.Load())
	}
	select {
	case <-modelClient.secondTurn:
	case <-ctx.Done():
		t.Fatalf("Agent did not continue after the file.write call: %v", ctx.Err())
	}

	committedBody := f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPost,
		commitPath, nil, "", "f02-fence-external-commit", http.StatusOK)
	var committed workspace.SyncSession
	if err := json.Unmarshal(committedBody, &committed); err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 {
		t.Fatalf("external Workspace commit after operation settlement = %#v, err=%v", committed, err)
	}
	workspaceAfterCommit, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfterCommit.Revision != 1 {
		t.Fatalf("Workspace revision after external commit = %d err=%v", workspaceAfterCommit.Revision, err)
	}

	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store, Executor: runtimeControl, WorkerID: "f02-task-workspace-fence-stop",
		LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond, OperationTimeout: 2 * time.Second,
		RetryAfter: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	stopDone := make(chan error, 1)
	go func() { stopDone <- stopWorker.Run(stopCtx) }()
	defer stopF01Worker(t, cancelStop, stopDone, "F02 Workspace fence TaskStopWorker")
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		return current.Task.Status == task.StatusPausing, nil
	}, nil, nil); err != nil {
		current, getErr := store.GetTask(context.WithoutCancel(ctx), accepted.ID, owner.ID)
		t.Fatalf("TaskStopWorker did not fence the stale Task into pausing: status=%q reason=%q err=%v get_err=%v",
			current.Task.Status, current.Task.WaitingReason, err, getErr)
	}
	pausingReleaseBody := f01ReleaseReadbackRequest(t, ctx, httpClient, portal.URL, sessionCredential, http.MethodPost,
		"/api/hosts/"+hostValue.ID+"/release", releaseBody, "application/json", "", http.StatusConflict)
	if !bytes.Contains(pausingReleaseBody, []byte(`"code":"conflict"`)) {
		t.Fatalf("Host release while Task was pausing did not return conflict: %s", pausingReleaseBody)
	}
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		return current.Task.Status == task.StatusPaused && current.Task.WaitingReason == "workspace_input_changed" &&
			current.StopRequest != nil && current.StopRequest.Status == task.StopRequestCompleted, nil
	}, nil, nil); err != nil {
		current, getErr := store.GetTask(context.WithoutCancel(ctx), accepted.ID, owner.ID)
		t.Fatalf("stale Task did not safely pause for resubmission: status=%q reason=%q stop=%#v err=%v get_err=%v",
			current.Task.Status, current.Task.WaitingReason, current.StopRequest, err, getErr)
	}
	final, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || final.Task.InputRevision == nil || *final.Task.InputRevision != 0 ||
		final.Task.WorkspaceCheckpointRevision == nil || *final.Task.WorkspaceCheckpointRevision != 0 {
		t.Fatalf("paused stale Task no longer records its original input checkpoint: %#v err=%v", final.Task, err)
	}
	hostAfterPause, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || hostAfterPause.State != host.StateRunning {
		t.Fatalf("Host release began before stale Task pause completed: state=%q err=%v", hostAfterPause.State, err)
	}

	stopF01Worker(t, cancelWorker, workerDone, "F02 Workspace fence TaskWorker")
	workerStopped = true
}

type f02FenceHeldFileWriteHandler struct {
	inner           runtimecore.ActionHandler
	entered         chan struct{}
	release         chan struct{}
	enterOnce       sync.Once
	releaseOnce     sync.Once
	mu              sync.Mutex
	workspaceViewID string
	effects         atomic.Int32
}

func (h *f02FenceHeldFileWriteHandler) Capability() string { return "file.write" }
func (h *f02FenceHeldFileWriteHandler) Action() string     { return "write" }

func (h *f02FenceHeldFileWriteHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	h.mu.Lock()
	h.workspaceViewID = request.WorkspaceViewID
	h.mu.Unlock()
	h.enterOnce.Do(func() { close(h.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.release:
	}
	result, err := h.inner.Execute(ctx, request)
	if err == nil {
		h.effects.Add(1)
	}
	return result, err
}

func (h *f02FenceHeldFileWriteHandler) Stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	return h.inner.Stop(ctx, operation)
}

func (h *f02FenceHeldFileWriteHandler) unblock() {
	h.releaseOnce.Do(func() { close(h.release) })
}

type f02FenceModelResolver struct{}

func (f02FenceModelResolver) ResolveEndpoint(context.Context, string, string) (app.ModelEndpoint, error) {
	return app.ModelEndpoint{BaseURL: "https://model.example.test/v1", APIKey: "local-test-key", ModelID: "f02-test-model"}, nil
}

type f02FenceAgentModelClient struct {
	workspaceID string
	secondTurn  chan struct{}
	calls       atomic.Int32
}

func (c *f02FenceAgentModelClient) Complete(ctx context.Context, _ app.ModelRequest) (app.ModelResponse, error) {
	switch c.calls.Add(1) {
	case 1:
		return app.ModelResponse{
			ToolCalls: []app.ModelToolCall{{
				CallID: "f02-workspace-fence-write-001", Name: "file_write",
				Arguments: fmt.Sprintf(`{"workspace_id":%q,"path":"task-owned.txt","content":"task-owned write\n"}`, c.workspaceID),
			}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3},
		}, nil
	case 2:
		select {
		case c.secondTurn <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return app.ModelResponse{}, ctx.Err()
	default:
		return app.ModelResponse{}, errors.New("unexpected extra model call in F02 workspace fence test")
	}
}

func (*f02FenceAgentModelClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true, Tools: true, ModelID: "f02-test-model"}, nil
}
