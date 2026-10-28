package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"
	mordesync "morde-sync/client"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

// TestCloudTaskSurvivesClientDisconnectAndCommitsHostWorkspaceArtifact exercises
// task acceptance, durable worker execution, the real Node dsh runtime, Host
// runtime file operations, Host replica sync commits, and artifact readback.
func TestCloudTaskSurvivesClientDisconnectAndCommitsHostWorkspaceArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, runtimeCredential, replicaCredential := createF01Host(t, ctx, store, sealer, owner.ID)
	if _, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: 0,
	}); err != nil {
		t.Fatal(err)
	}

	hostRoot := t.TempDir()
	workspaceDirectory := filepath.Join(hostRoot, workspaceValue.ID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := runtimefiles.OpenWorkspace(hostRoot)
	if err != nil {
		t.Fatal(err)
	}
	approvalDesktop := &f01ApprovalDesktopHandler{}
	runtimeHandlers := append(files.Handlers(), approvalDesktop)
	defer func() {
		if err := files.Close(); err != nil {
			t.Errorf("close Host runtime workspace: %v", err)
		}
	}()
	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()),
		HostID: hostValue.ID, Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: runtimeHandlers,
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
	defer runtimeServer.Close()
	defer func() {
		if err := runtimeManager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close Host runtime: %v", err)
		}
	}()

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace blocks: %v", err)
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
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.UserID != owner.ID || principal.Kind != identity.KindSession {
		t.Fatal(err)
	}
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: runtimeServer.URL}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeControl.Handshake(ctx, hostValue.ID); err != nil {
		t.Fatalf("connect Host runtime before Agent task: %v", err)
	}
	hostBeforeAgent, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || hostBeforeAgent.State != host.StateRunning || hostBeforeAgent.RuntimeGeneration != hostValue.RuntimeGeneration || hostBeforeAgent.RuntimeInstanceID == "" {
		t.Fatalf("Host runtime baseline = state %s generation %d instance %q, %v", hostBeforeAgent.State,
			hostBeforeAgent.RuntimeGeneration, hostBeforeAgent.RuntimeInstanceID, err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	apiRouter, err := api.New(api.Deps{
		Hosts: f01UnusedHostService{}, HostReplicas: hostReplicas, TLSAuthorizer: f01UnusedTLSAuthorizer{},
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

	preparingCredential, preparingBinding := f01BindingCredential(t, httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID)
	preparingClient, err := mordesync.New(portal.URL, preparingCredential, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	zeroRevision := int64(0)
	initialDownload, err := preparingClient.Sync(ctx, mordesync.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: workspaceDirectory, Direction: "download", Policy: "fill_missing", BaseRevision: &zeroRevision,
	})
	if err != nil || initialDownload.Session.State != "completed" || initialDownload.Session.TargetRevision != 0 {
		t.Fatalf("initial Host replica download = %#v, %v", initialDownload.Session, err)
	}
	if err := preparingClient.ConfirmDownloadedLiveBaseline(ctx, workspaceValue.ID, workspaceDirectory, initialDownload.Session.TargetRevision); err != nil {
		t.Fatalf("record initial Host replica baseline: %v", err)
	}
	if err := f01RecordApplied(httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID,
		preparingBinding.BindingVersion, initialDownload.Session.ID, initialDownload.Session.TargetRevision); err != nil {
		t.Fatal(err)
	}
	if err := f01ActivateBinding(httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID, preparingBinding.BindingVersion); err != nil {
		t.Fatal(err)
	}
	activeCredential, activeBinding := f01BindingCredential(t, httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID)
	if activeBinding.State != string(workspace.BindingActive) {
		t.Fatalf("Host Workspace binding after initial receipt = %s", activeBinding.State)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real dsh subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x3d}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-e2e-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	childSessions, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	model := &f01CloudTaskModel{store: store, hostID: hostValue.ID, workspaceID: workspaceValue.ID, approvalFirst: true,
		secondStarted: make(chan struct{}), content: "Morde kept the cloud task running after the client disconnected.\n"}
	supervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-e2e-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Children: childSessions, Models: model, Tools: toolBroker, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	executionErrors := make(chan error, 4)
	recorder := f01RecordingExecutor{inner: executor, errors: executionErrors, supervisor: supervisor, tools: toolBroker}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: recorder, WorkerID: "f01-e2e-worker", LeaseDuration: 30 * time.Second,
		PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	postCtx, cancelPost := context.WithCancel(ctx)
	postBody, err := json.Marshal(map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": int64(0),
		"model_id": "p1234567", "objective": "Click the test desktop target, then write a short note in the attached Workspace.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	postRequest, err := http.NewRequestWithContext(postCtx, http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(postBody))
	if err != nil {
		t.Fatal(err)
	}
	postRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	postRequest.Header.Set("Content-Type", "application/json")
	postRequest.Header.Set("Origin", portal.URL)
	postRequest.Header.Set("Idempotency-Key", "f01-cloud-lifecycle-e2e-001")
	postResponse, err := httpClient.Do(postRequest)
	if err != nil {
		cancelPost()
		t.Fatal(err)
	}
	var accepted struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		WorkspaceID   string `json:"workspace_id"`
		InputRevision *int64 `json:"input_revision"`
	}
	if err := json.NewDecoder(postResponse.Body).Decode(&accepted); err != nil {
		_ = postResponse.Body.Close()
		cancelPost()
		t.Fatal(err)
	}
	_ = postResponse.Body.Close()
	cancelPost()
	if postResponse.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) ||
		accepted.WorkspaceID != workspaceValue.ID || accepted.InputRevision == nil || *accepted.InputRevision != 0 {
		t.Fatalf("POST /api/tasks acceptance = status %d, response %#v", postResponse.StatusCode, accepted)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(workerCtx) }()
	defer func() {
		cancelWorker()
		select {
		case runErr := <-workerDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Errorf("TaskWorker.Run() = %v, want context canceled during cleanup", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()

	liveClient, err := mordesync.New(portal.URL, activeCredential, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	liveCtx, cancelLive := context.WithCancel(context.Background())
	liveReceipts := make(chan int64, 4)
	liveErrors := make(chan error, 4)
	liveDone := make(chan error, 1)
	go func() {
		liveDone <- liveClient.LiveSync(liveCtx, mordesync.LiveSyncOptions{
			WorkspaceID: workspaceValue.ID, Directory: workspaceDirectory, RescanInterval: 5 * time.Second,
			OnResult: func(result mordesync.Result) {
				if result.Session.State != "completed" || len(result.Conflicts) != 0 || len(result.Session.Conflicts) != 0 {
					return
				}
				if err := f01RecordApplied(httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID,
					activeBinding.BindingVersion, result.Session.ID, result.Session.TargetRevision); err != nil {
					select {
					case liveErrors <- err:
					default:
					}
					return
				}
				select {
				case liveReceipts <- result.Session.TargetRevision:
				default:
				}
			},
		})
	}()
	defer func() {
		cancelLive()
		select {
		case liveErr := <-liveDone:
			if !errors.Is(liveErr, context.Canceled) {
				t.Errorf("Host replica live sync stopped unexpectedly: %v", liveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Host replica live sync did not stop")
		}
	}()

	streamCtx, cancelStream := context.WithCancel(ctx)
	streamRequest, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
		portal.URL+"/api/tasks/"+accepted.ID+"/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	streamResponse, err := httpClient.Do(streamRequest)
	if err != nil {
		cancelStream()
		t.Fatalf("open task event stream: %v", err)
	}
	if streamResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResponse.Body)
		_ = streamResponse.Body.Close()
		cancelStream()
		t.Fatalf("task event stream status = %d: %s", streamResponse.StatusCode, body)
	}
	if _, err := bufio.NewReader(streamResponse.Body).ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		_ = streamResponse.Body.Close()
		cancelStream()
		t.Fatalf("read first task event: %v", err)
	}
	cancelStream()
	_ = streamResponse.Body.Close()

	var approvalID string
	var approvalVersion int64
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		if current.Task.Status != task.StatusWaitingUser || current.PendingRequest == nil {
			return false, nil
		}
		approvalID = current.PendingRequest.ID
		approvalVersion = current.PendingRequest.Version
		return true, nil
	}, nil, nil); err != nil {
		t.Fatalf("sensitive Host action did not wait for approval: %v", err)
	}
	if approvalDesktop.calls.Load() != 0 {
		t.Fatalf("Host desktop action ran before approval: calls=%d", approvalDesktop.calls.Load())
	}
	secondClientCredential := createF01Session(t, ctx, store, owner.ID)
	getTask, err := http.NewRequest(http.MethodGet, portal.URL+"/api/tasks/"+accepted.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	getTask.AddCookie(&http.Cookie{Name: "ch_portal", Value: secondClientCredential})
	getResponse, err := httpClient.Do(getTask)
	if err != nil {
		t.Fatal(err)
	}
	var pendingView struct {
		Status         string `json:"status"`
		PendingRequest *struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
			Kind    string `json:"kind"`
		} `json:"pending_request"`
	}
	if err := json.NewDecoder(getResponse.Body).Decode(&pendingView); err != nil {
		_ = getResponse.Body.Close()
		t.Fatal(err)
	}
	_ = getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK || pendingView.Status != string(task.StatusWaitingUser) ||
		pendingView.PendingRequest == nil || pendingView.PendingRequest.ID != approvalID ||
		pendingView.PendingRequest.Version != approvalVersion || pendingView.PendingRequest.Kind != "approval" {
		t.Fatalf("GET /api/tasks pending approval = status %d, %#v", getResponse.StatusCode, pendingView)
	}
	approvalBody, err := json.Marshal(map[string]any{"version": approvalVersion, "decision": "approve"})
	if err != nil {
		t.Fatal(err)
	}
	approvalCredentials := []string{secondClientCredential, sessionCredential}
	for attempt, credential := range approvalCredentials {
		approvalRequest, err := http.NewRequest(http.MethodPost,
			portal.URL+"/api/tasks/"+accepted.ID+"/approvals/"+approvalID+"/responses", bytes.NewReader(approvalBody))
		if err != nil {
			t.Fatal(err)
		}
		approvalRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
		approvalRequest.Header.Set("Content-Type", "application/json")
		approvalRequest.Header.Set("Origin", portal.URL)
		approvalRequest.Header.Set("Idempotency-Key", "f01-cloud-approval-e2e-001")
		approvalResponse, err := httpClient.Do(approvalRequest)
		if err != nil {
			t.Fatal(err)
		}
		if approvalResponse.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(approvalResponse.Body)
			_ = approvalResponse.Body.Close()
			t.Fatalf("POST task approval attempt %d = status %d: %s", attempt+1, approvalResponse.StatusCode, body)
		}
		_ = approvalResponse.Body.Close()
	}

	secondTurn := time.NewTicker(5 * time.Second)
	defer secondTurn.Stop()
	secondTurnReached := false
	for !secondTurnReached {
		select {
		case <-model.secondStarted:
			secondTurnReached = true
		case executionErr := <-executionErrors:
			t.Fatalf("real CloudDSHExecutor stopped before its second model call: %v", executionErr)
		case <-secondTurn.C:
			current, getErr := store.GetTask(ctx, accepted.ID, owner.ID)
			t.Logf("waiting for dsh second model turn: err=%v status=%s agent=%s/%d checkpoint=%d reason=%s model_calls=%d steps=%d",
				getErr, current.Task.Status, current.Task.AgentInstanceID, current.Task.AgentGeneration,
				current.Task.CheckpointThroughSequence, current.Task.WaitingReason, model.callCount(), len(current.Steps))
		case <-ctx.Done():
			t.Fatalf("dsh did not continue to its next model turn after the client stream closed: %v", ctx.Err())
		}
	}
	details, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusRunning {
		t.Fatalf("task after client stream disconnect = %s, %v; want running", details.Task.Status, err)
	}
	if approvalDesktop.calls.Load() != 1 {
		t.Fatalf("approved Host desktop operation ran %d times, want exactly once", approvalDesktop.calls.Load())
	}
	fileOnHost, err := os.ReadFile(filepath.Join(workspaceDirectory, "task-note.txt"))
	if err != nil || string(fileOnHost) != model.content {
		current, taskErr := store.GetTask(ctx, accepted.ID, owner.ID)
		operation, operationErr := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
		var stepSummary []string
		for _, step := range current.Steps {
			stepSummary = append(stepSummary, fmt.Sprintf("%s:%s:%s", step.Kind, step.Status, step.Summary))
		}
		t.Fatalf("real Host runtime file after dsh write: read_error=%v bytes=%d task_error=%v task_status=%s tool_calls=%d steps=%v operation_status=%s operation_error=%v model_calls=%d",
			err, len(fileOnHost), taskErr, current.Task.Status, current.Task.Usage.ToolCalls, stepSummary, operation.Status, operationErr, model.callCount())
	}

	if err := waitF01(t, ctx, func() (bool, error) {
		binding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
		if err != nil {
			return false, err
		}
		return binding.Workspace.Revision == 1 && binding.DesiredRevision == 1 && binding.AppliedRevision == 1, nil
	}, liveErrors, liveReceipts); err != nil {
		t.Fatal(err)
	}

	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		return current.Task.Status == task.StatusCompleted, nil
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	details, err = store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	approvalOperation, approvalOperationErr := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-desktop-click-001"))
	if details.PendingRequest == nil || details.PendingRequest.Status != task.PendingRequestApproved ||
		details.PendingRequest.Decision != task.PendingDecisionApprove || approvalOperationErr != nil ||
		approvalOperation.Status != task.OperationSucceeded || approvalDesktop.calls.Load() != 1 || model.callCount() != 3 {
		t.Fatalf("approval recovery = pending=%#v operation=%#v operation_error=%v Host_calls=%d model_calls=%d",
			details.PendingRequest, approvalOperation, approvalOperationErr, approvalDesktop.calls.Load(), model.callCount())
	}
	digest := sha256.Sum256([]byte(model.content))
	wantDigest := hex.EncodeToString(digest[:])
	if details.Task.Status != task.StatusCompleted || details.Task.WorkspaceID != workspaceValue.ID ||
		details.Task.InputRevision == nil || *details.Task.InputRevision != 0 || len(details.Artifacts) != 1 {
		t.Fatalf("completed Task or artifact count = status %s, Workspace %s, input revision %#v, artifacts %#v",
			details.Task.Status, details.Task.WorkspaceID, details.Task.InputRevision, details.Artifacts)
	}
	artifact := details.Artifacts[0]
	if artifact.WorkspaceID != workspaceValue.ID || artifact.Revision != 1 || artifact.Path != "task-note.txt" ||
		artifact.SizeBytes != int64(len(model.content)) || artifact.SHA256 != wantDigest || artifact.MediaType != "text/plain; charset=utf-8" {
		t.Fatalf("persisted artifact metadata = %#v", artifact)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, artifact.Revision, "", "", 10)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != artifact.Path ||
		manifest.Files[0].SizeBytes != artifact.SizeBytes || manifest.Files[0].SHA256 != artifact.SHA256 {
		t.Fatalf("committed Workspace manifest = %#v, %v", manifest, err)
	}

	readCredential, readBinding := f01BindingCredential(t, httpClient, portal.URL, hostValue, replicaCredential, workspaceValue.ID)
	readClient, err := mordesync.New(portal.URL, readCredential, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	readbackDirectory := filepath.Join(t.TempDir(), "downloaded-workspace")
	if err := os.MkdirAll(readbackDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	readback, err := readClient.Sync(ctx, mordesync.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: readbackDirectory, Direction: "download", Policy: "fill_missing", BaseRevision: &zeroRevision,
	})
	if err != nil || readback.Session.State != "completed" || readback.Session.TargetRevision != readBinding.DesiredRevision {
		t.Fatalf("read committed Workspace through Host replica blocks = %#v, %v", readback.Session, err)
	}
	readbackBytes, err := os.ReadFile(filepath.Join(readbackDirectory, artifact.Path))
	if err != nil || string(readbackBytes) != model.content {
		t.Fatalf("downloaded Workspace artifact = %q, %v", readbackBytes, err)
	}
	var agentState, agentInstanceID string
	var agentGeneration int64
	if err := pool.QueryRow(ctx, `SELECT state,agent_instance_id,agent_generation FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 ORDER BY agent_generation DESC LIMIT 1`, accepted.ID, owner.ID).
		Scan(&agentState, &agentInstanceID, &agentGeneration); err != nil || agentState != "stopped" || agentInstanceID == hostValue.ID {
		t.Fatalf("completed Task Agent = state %s instance %q generation %d, %v", agentState, agentInstanceID, agentGeneration, err)
	}
	hostAfterAgent, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || hostAfterAgent.State != host.StateRunning || hostAfterAgent.RuntimeGeneration != hostBeforeAgent.RuntimeGeneration ||
		hostAfterAgent.RuntimeInstanceID != hostBeforeAgent.RuntimeInstanceID {
		t.Fatalf("Host changed when its Agent stopped: before=%s/%d/%s after=%s/%d/%s err=%v", hostBeforeAgent.State,
			hostBeforeAgent.RuntimeGeneration, hostBeforeAgent.RuntimeInstanceID, hostAfterAgent.State,
			hostAfterAgent.RuntimeGeneration, hostAfterAgent.RuntimeInstanceID, err)
	}
	if err := runtimeControl.Handshake(ctx, hostValue.ID); err != nil {
		t.Fatalf("reconnect Host runtime after Agent stopped: %v", err)
	}
	hostAfterReconnect, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || hostAfterReconnect.State != host.StateRunning || hostAfterReconnect.RuntimeGeneration != hostBeforeAgent.RuntimeGeneration ||
		hostAfterReconnect.RuntimeInstanceID != hostBeforeAgent.RuntimeInstanceID {
		t.Fatalf("Host did not survive Agent stop: baseline=%d/%s reconnected=%s/%d/%s err=%v", hostBeforeAgent.RuntimeGeneration,
			hostBeforeAgent.RuntimeInstanceID, hostAfterReconnect.State, hostAfterReconnect.RuntimeGeneration,
			hostAfterReconnect.RuntimeInstanceID, err)
	}

	model.enableBackgroundChild()
	backgroundRequest, err := json.Marshal(map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": int64(1),
		"model_id": "p1234567", "objective": "Read the existing Workspace note in a background job, then write a completion note.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 20, "model_tokens": 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	backgroundPost, err := http.NewRequest(http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(backgroundRequest))
	if err != nil {
		t.Fatal(err)
	}
	backgroundPost.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	backgroundPost.Header.Set("Content-Type", "application/json")
	backgroundPost.Header.Set("Origin", portal.URL)
	backgroundPost.Header.Set("Idempotency-Key", "f01-cloud-background-e2e-001")
	backgroundResponse, err := httpClient.Do(backgroundPost)
	if err != nil {
		t.Fatal(err)
	}
	var backgroundAccepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(backgroundResponse.Body).Decode(&backgroundAccepted); err != nil {
		_ = backgroundResponse.Body.Close()
		t.Fatal(err)
	}
	_ = backgroundResponse.Body.Close()
	if backgroundResponse.StatusCode != http.StatusAccepted || backgroundAccepted.ID == "" || backgroundAccepted.Status != string(task.StatusQueued) {
		t.Fatalf("background POST /api/tasks acceptance = status %d, response %#v", backgroundResponse.StatusCode, backgroundAccepted)
	}
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, backgroundAccepted.ID, owner.ID)
		return err == nil && current.Task.Status == task.StatusCompleted, err
	}, executionErrors, nil); err != nil {
		t.Fatalf("background child Task did not complete: %v", err)
	}
	backgroundDetails, err := store.GetTask(ctx, backgroundAccepted.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var childSessionID string
	var allowedTools []byte
	if err := pool.QueryRow(ctx, `SELECT session_id, allowed_tools FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND parent_session_id IS NOT NULL AND is_background`, backgroundAccepted.ID, owner.ID).
		Scan(&childSessionID, &allowedTools); err != nil {
		t.Fatalf("load persisted background child: %v", err)
	}
	var persistedTools []string
	if err := json.Unmarshal(allowedTools, &persistedTools); err != nil || !sameWorkerToolNames(persistedTools, []string{"file_read"}) {
		t.Fatalf("background child permissions = %s, %v", allowedTools, err)
	}
	var childSucceeded bool
	for _, step := range backgroundDetails.Steps {
		if step.Kind == "subagent" && step.Status == task.StepSucceeded {
			childSucceeded = true
		}
	}
	if !childSucceeded || !model.backgroundOutputWasVerified() {
		t.Fatalf("background job result was not durably read and completed: child_succeeded=%t output_verified=%t steps=%#v",
			childSucceeded, model.backgroundOutputWasVerified(), backgroundDetails.Steps)
	}
	backgroundReadCall := cloudDSHNamespacedToolCallID(backgroundAccepted.ID, childSessionID, "f01-background-read-001")
	backgroundReadOperation, backgroundReadErr := store.GetOperation(ctx, owner.ID, backgroundAccepted.ID, f01OperationKey(backgroundReadCall))
	if backgroundReadErr != nil || backgroundReadOperation.Status != task.OperationSucceeded {
		t.Fatalf("background Host Runtime read operation = %#v, %v", backgroundReadOperation, backgroundReadErr)
	}
	backgroundWrite, err := os.ReadFile(filepath.Join(workspaceDirectory, "background-result.txt"))
	if err != nil || string(backgroundWrite) != "Background job completed after reading task-note.txt.\n" {
		t.Fatalf("background Host Runtime output = %q, %v", backgroundWrite, err)
	}
}

func f01OperationKey(callID string) string {
	digest := sha256.Sum256([]byte(callID))
	return "agent:" + hex.EncodeToString(digest[:])
}

type f01CloudTaskModel struct {
	mu                       sync.Mutex
	calls                    int
	secondStarted            chan struct{}
	startOnce                sync.Once
	approvalFirst            bool
	store                    *storepostgres.Store
	hostID                   string
	workspaceID              string
	content                  string
	backgroundChild          bool
	backgroundRootCalls      int
	backgroundChildCalls     int
	backgroundJobID          string
	backgroundLastGetCallID  string
	backgroundOutputCallID   string
	backgroundOutputVerified bool
}

type f01RecordingExecutor struct {
	inner      TaskExecutor
	errors     chan<- error
	supervisor *f01CapturingSupervisor
	tools      interface {
		Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error)
	}
}

func (e f01RecordingExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	err := e.inner.ExecuteTask(ctx, value)
	if err != nil {
		definitions, definitionsErr := e.tools.Definitions(context.WithoutCancel(ctx), value)
		if definitionsErr != nil {
			definitions = nil
		}
		err = fmt.Errorf("%w; %s", err, f01DSHModelDiagnostic([]byte(e.supervisor.output()), value.ModelID, definitions, definitionsErr))
		select {
		case e.errors <- err:
		default:
		}
	}
	return err
}

// f01DSHModelDiagnostic reports only request shape and schema comparisons. It
// deliberately omits prompts, message text, tool arguments, credentials, and
// frame identifiers so failures remain useful without leaking task content.
func f01DSHModelDiagnostic(output []byte, expectedModel string, definitions []app.AgentToolDefinition, definitionsErr error) string {
	type requestMessage struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	type requestShape struct {
		Provider    string            `json:"provider"`
		Model       string            `json:"model"`
		Tools       []json.RawMessage `json:"tools"`
		Messages    []requestMessage  `json:"messages"`
		MaxTokens   *int              `json:"max_tokens"`
		Temperature *float64          `json:"temperature"`
	}
	type envelope struct {
		Type    string          `json:"type"`
		Request json.RawMessage `json:"request"`
	}
	var rawRequest json.RawMessage
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var frame envelope
		if json.Unmarshal(line, &frame) == nil && frame.Type == "model" {
			rawRequest = frame.Request
			break
		}
	}
	if len(rawRequest) == 0 {
		return fmt.Sprintf("dsh model diagnostic: model_frame=missing stdout_bytes=%d definitions_error=%v", len(output), definitionsErr)
	}
	var shape requestShape
	if err := json.Unmarshal(rawRequest, &shape); err != nil {
		return fmt.Sprintf("dsh model diagnostic: request_bytes=%d request_json_error=%v", len(rawRequest), err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(rawRequest, &fields)
	requestKeys := make([]string, 0, len(fields))
	for key := range fields {
		requestKeys = append(requestKeys, key)
	}
	sort.Strings(requestKeys)
	toolSummary := make([]string, 0, len(shape.Tools))
	for index, rawTool := range shape.Tools {
		var offered struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		_ = json.Unmarshal(rawTool, &offered)
		comparison := "unexpected"
		if index < len(definitions) {
			want, wantOK := canonicalCloudDSHJSON(definitions[index].Parameters)
			got, gotOK := canonicalCloudDSHJSON(offered.Parameters)
			comparison = fmt.Sprintf("name=%t description=%t schema=%t", offered.Name == definitions[index].Name,
				offered.Description == definitions[index].Description, wantOK && gotOK && bytes.Equal(want, got))
		}
		toolSummary = append(toolSummary, fmt.Sprintf("%s(%s)", offered.Name, comparison))
	}
	messageSummary := make([]string, 0, len(shape.Messages))
	for _, message := range shape.Messages {
		blockTypes := make([]string, 0, len(message.Content))
		blockKeys := make([]string, 0)
		for _, rawBlock := range message.Content {
			var block struct {
				Type string `json:"type"`
			}
			var blockFields map[string]json.RawMessage
			_ = json.Unmarshal(rawBlock, &block)
			_ = json.Unmarshal(rawBlock, &blockFields)
			blockTypes = append(blockTypes, block.Type)
			for key := range blockFields {
				blockKeys = append(blockKeys, key)
			}
		}
		sort.Strings(blockTypes)
		sort.Strings(blockKeys)
		messageSummary = append(messageSummary, fmt.Sprintf("%s/types=%v/block_keys=%v", message.Role, blockTypes, blockKeys))
	}
	var strict struct {
		Provider string            `json:"provider"`
		Model    string            `json:"model"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Source  json.RawMessage `json:"source"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			IsError    bool   `json:"isError"`
		} `json:"messages"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
	}
	strictDecoder := json.NewDecoder(bytes.NewReader(rawRequest))
	strictDecoder.DisallowUnknownFields()
	strictErr := strictDecoder.Decode(&strict)
	if strictErr == nil && (strictDecoder.Decode(new(any)) != io.EOF || strict.Provider != "morde" || strict.Model != expectedModel ||
		strict.Tools == nil || len(strict.Tools) != len(definitions) || len(strict.Messages) == 0) {
		strictErr = errors.New("top-level model/provider/tool-count/message-count check failed")
	}
	valid := validCloudDSHModelRequest(rawRequest, expectedModel, definitions)
	return fmt.Sprintf("dsh model diagnostic: request_bytes=%d request_keys=%v expected_model=%s provider=%s model=%s model_matches=%t max_tokens=%v temperature_present=%t tools=%d expected_tools=%d tool_shapes=%v messages=%d message_shapes=%v strict_check=%v valid=%t definitions_error=%v",
		len(rawRequest), requestKeys, expectedModel, shape.Provider, shape.Model, shape.Model == expectedModel, shape.MaxTokens,
		shape.Temperature != nil, len(shape.Tools), len(definitions), toolSummary, len(shape.Messages), messageSummary, strictErr, valid, definitionsErr)
}

type f01CapturingSupervisor struct {
	child *cloudDSHTestSupervisor
	mu    sync.Mutex
	bytes bytes.Buffer
}

func (s *f01CapturingSupervisor) Start(ctx context.Context, launch CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error) {
	input, output, err := s.child.Start(ctx, launch)
	if err != nil {
		return nil, nil, err
	}
	return input, f01CaptureReader{ReadCloser: output, capture: s}, nil
}

func (s *f01CapturingSupervisor) Renew(ctx context.Context, fence app.AgentInstanceFence, expiry time.Time) error {
	return s.child.Renew(ctx, fence, expiry)
}

func (s *f01CapturingSupervisor) Stop(ctx context.Context, fence app.AgentInstanceFence, nonce []byte) (app.AgentStopReceipt, error) {
	return s.child.Stop(ctx, fence, nonce)
}

func (s *f01CapturingSupervisor) append(value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.bytes.Write(value)
}

func (s *f01CapturingSupervisor) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes.String()
}

type f01CaptureReader struct {
	io.ReadCloser
	capture *f01CapturingSupervisor
}

func (r f01CaptureReader) Read(value []byte) (int, error) {
	n, err := r.ReadCloser.Read(value)
	if n > 0 {
		r.capture.append(value[:n])
	}
	return n, err
}

func (m *f01CloudTaskModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *f01CloudTaskModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	callNumber := m.calls
	background := m.backgroundChild
	if background && call.Task != nil && call.Task.AgentSessionID != call.Task.AgentRootSessionID {
		m.backgroundChildCalls++
		childCall := m.backgroundChildCalls
		m.mu.Unlock()
		if childCall == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "f01-background-read-001", Name: "file_read",
				Arguments: fmt.Sprintf(`{"workspace_id":%q,"path":"task-note.txt"}`, m.workspaceID)}},
				StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}}, nil
		}
		return app.ModelResponse{Text: "Background read verified the existing task note.", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}}, nil
	}
	if background {
		m.backgroundRootCalls++
		rootCall := m.backgroundRootCalls
		m.mu.Unlock()
		return m.completeBackground(ctx, call, rootCall)
	}
	m.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}
	if callNumber == 1 {
		if m.approvalFirst {
			return app.ModelResponse{
				ToolCalls: []app.ModelToolCall{{
					CallID: "f01-desktop-click-001", Name: "desktop_click",
					Arguments: `{"window_id":"0x1","x":1,"y":2,"button":"left"}`,
				}}, StopReason: "tool_calls", Usage: usage,
			}, nil
		}
		return app.ModelResponse{
			ToolCalls: []app.ModelToolCall{{
				CallID: "f01-write-note-001", Name: "file_write",
				Arguments: fmt.Sprintf(`{"workspace_id":%q,"path":"task-note.txt","content":%q}`, m.workspaceID, m.content),
			}}, StopReason: "tool_calls", Usage: usage,
		}, nil
	}
	if m.approvalFirst && callNumber == 2 {
		return app.ModelResponse{
			ToolCalls: []app.ModelToolCall{{
				CallID: "f01-write-note-001", Name: "file_write",
				Arguments: fmt.Sprintf(`{"workspace_id":%q,"path":"task-note.txt","content":%q}`, m.workspaceID, m.content),
			}}, StopReason: "tool_calls", Usage: usage,
		}, nil
	}
	completionCall := 2
	if m.approvalFirst {
		completionCall = 3
	}
	if callNumber != completionCall {
		return app.ModelResponse{}, fmt.Errorf("unexpected model call %d", callNumber)
	}
	m.startOnce.Do(func() { close(m.secondStarted) })
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		binding, err := m.store.GetWorkspaceBinding(ctx, m.hostID, m.workspaceID)
		if err == nil && binding.Workspace.Revision >= 1 && binding.AppliedRevision >= 1 {
			return app.ModelResponse{Text: "The Workspace file was committed.", StopReason: "stop", Usage: usage}, nil
		}
		select {
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *f01CloudTaskModel) enableBackgroundChild() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backgroundChild = true
	m.backgroundRootCalls = 0
	m.backgroundChildCalls = 0
	m.backgroundJobID = ""
	m.backgroundLastGetCallID = ""
	m.backgroundOutputCallID = ""
	m.backgroundOutputVerified = false
}

func (m *f01CloudTaskModel) backgroundOutputWasVerified() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backgroundOutputVerified
}

func (m *f01CloudTaskModel) completeBackground(ctx context.Context, call app.ModelCall, callNumber int) (app.ModelResponse, error) {
	usage := app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 1}
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("background model call is missing its Task fence")
	}
	responseWithTool := func(id, name, arguments string) (app.ModelResponse, error) {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: id, Name: name, Arguments: arguments}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	switch callNumber {
	case 1:
		return responseWithTool("f01-background-delegate-001", cloudDSHDelegateToolName,
			`{"title":"Read the existing Workspace note","objective":"Read task-note.txt and report what it says.","tool_names":["file_read"],"run_in_background":true}`)
	case 2:
		return responseWithTool("f01-background-list-001", cloudDSHJobListToolName, `{}`)
	case 3:
		output, ok := f01ToolResult(call.RawRequest, "f01-background-list-001")
		var jobs []cloudDSHJobView
		if !ok || json.Unmarshal([]byte(output), &jobs) != nil || len(jobs) != 1 || jobs[0].JobID == "" {
			return app.ModelResponse{}, errors.New("background job list is missing the persisted child session")
		}
		m.mu.Lock()
		m.backgroundJobID = jobs[0].JobID
		m.backgroundLastGetCallID = "f01-background-get-001"
		m.mu.Unlock()
		return responseWithTool("f01-background-get-001", cloudDSHJobGetToolName,
			fmt.Sprintf(`{"job_id":%q}`, jobs[0].JobID))
	default:
		m.mu.Lock()
		jobID, getCallID, outputCallID, outputVerified := m.backgroundJobID, m.backgroundLastGetCallID,
			m.backgroundOutputCallID, m.backgroundOutputVerified
		m.mu.Unlock()
		if outputVerified {
			return m.waitForBackgroundWorkspaceCommit(ctx, usage)
		}
		if getCallID != "" {
			output, ok := f01ToolResult(call.RawRequest, getCallID)
			var job cloudDSHJobView
			if !ok || json.Unmarshal([]byte(output), &job) != nil || job.JobID != jobID {
				return app.ModelResponse{}, errors.New("background job status is not durably readable")
			}
			if job.Status == "succeeded" && job.OutputCursor >= 0 {
				m.mu.Lock()
				m.backgroundLastGetCallID = ""
				m.backgroundOutputCallID = "f01-background-output-001"
				m.mu.Unlock()
				return responseWithTool("f01-background-output-001", cloudDSHJobOutputToolName,
					fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, jobID))
			}
			if job.Status != "running" && job.Status != "queued" && job.Status != "cancelling" {
				return app.ModelResponse{}, fmt.Errorf("background job ended in unexpected state %q", job.Status)
			}
			m.mu.Lock()
			getCallID = fmt.Sprintf("f01-background-get-%03d", callNumber-2)
			m.backgroundLastGetCallID = getCallID
			m.mu.Unlock()
			return responseWithTool(getCallID, cloudDSHJobGetToolName, fmt.Sprintf(`{"job_id":%q}`, jobID))
		}
		if outputCallID != "" {
			output, ok := f01ToolResult(call.RawRequest, outputCallID)
			var result struct {
				JobID      string `json:"job_id"`
				Status     string `json:"status"`
				NextCursor int64  `json:"next_cursor"`
				Output     string `json:"output"`
			}
			if !ok || json.Unmarshal([]byte(output), &result) != nil || result.JobID != jobID || result.Status != "succeeded" ||
				result.NextCursor < 0 || result.Output != "Background read verified the existing task note." {
				return app.ModelResponse{}, errors.New("background job output or durable cursor is invalid")
			}
			m.mu.Lock()
			m.backgroundOutputCallID = ""
			m.backgroundOutputVerified = true
			m.mu.Unlock()
			return responseWithTool("f01-background-write-001", "file_write", fmt.Sprintf(
				`{"workspace_id":%q,"path":"background-result.txt","content":%q}`, m.workspaceID,
				"Background job completed after reading task-note.txt.\n"))
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected background model call %d", callNumber)
	}
}

func (m *f01CloudTaskModel) waitForBackgroundWorkspaceCommit(ctx context.Context, usage app.ModelUsage) (app.ModelResponse, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		binding, err := m.store.GetWorkspaceBinding(ctx, m.hostID, m.workspaceID)
		if err == nil && binding.Workspace.Revision >= 2 && binding.AppliedRevision >= 2 {
			return app.ModelResponse{Text: "The background job completed and its Workspace output was committed.", StopReason: "stop", Usage: usage}, nil
		}
		select {
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func f01ToolResult(raw json.RawMessage, callID string) (string, bool) {
	var request struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return "", false
	}
	for _, message := range request.Messages {
		if message.Role == "tool" && message.ToolCallID == callID && len(message.Content) == 1 {
			return message.Content[0].Text, true
		}
	}
	return "", false
}

type f01ApprovalDesktopHandler struct{ calls atomic.Int32 }

func (*f01ApprovalDesktopHandler) Capability() string { return "desktop.input" }
func (*f01ApprovalDesktopHandler) Action() string     { return "click" }
func (h *f01ApprovalDesktopHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.calls.Add(1)
	return json.RawMessage(`{"window_id":"0x1","x":1,"y":2,"button":"left","clicked":true}`), nil
}
func (*f01ApprovalDesktopHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

type f01ConfiguredModels struct{}

func (f01ConfiguredModels) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

type f01DefaultHosts struct{}

func (f01DefaultHosts) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return host.Host{}, false, nil
}

type f01UnusedHostService struct{ app.HostService }
type f01UnusedTLSAuthorizer struct{ app.HostTLSAuthorizer }
type f01UnusedAuth struct{ api.AuthService }

type f01SandboxRouter struct {
	app.SandboxRouter
	endpoint string
}

func (r f01SandboxRouter) For(profile string) (app.SandboxClient, error) {
	if profile != "f01-local" {
		return nil, app.ErrSandboxUnavailable
	}
	return f01SandboxClient{endpoint: r.endpoint}, nil
}

func (f01SandboxRouter) Profiles() []string { return []string{"f01-local"} }

type f01SandboxClient struct {
	app.SandboxClient
	endpoint string
}

func (c f01SandboxClient) Endpoint(_ context.Context, sandboxID string, port int) (app.Upstream, error) {
	if sandboxID != "f01-local-host" || port != 8090 {
		return app.Upstream{}, app.ErrSandboxUnavailable
	}
	return app.Upstream{BaseURL: c.endpoint}, nil
}

type f01ReplicaBinding struct {
	WorkspaceID     string `json:"workspace_id"`
	SyncCredential  string `json:"sync_credential"`
	State           string `json:"state"`
	BindingVersion  int64  `json:"binding_version"`
	DesiredRevision int64  `json:"desired_revision"`
	AppliedRevision int64  `json:"applied_revision"`
}

func newF01TemporaryStore(t *testing.T) (*storepostgres.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL end-to-end test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse CH_TEST_DATABASE_URL: %v", err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig.Copy())
	if err != nil {
		t.Fatalf("connect to local test PostgreSQL: %v", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_worker_e2e_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL database: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("connect to isolated PostgreSQL database: %v", err)
	}
	if err := storepostgres.Migrate(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("migrate isolated PostgreSQL database: %v", err)
	}
	t.Cleanup(func() {
		testPool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop isolated PostgreSQL database: %v", err)
		}
		adminPool.Close()
	})
	return storepostgres.New(testPool), testPool
}

func createF01User(t *testing.T, ctx context.Context, store *storepostgres.Store) identity.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.CreateUser(ctx, identity.User{
		ID: id, Name: "f01-e2e-" + id, PasswordHash: "test-only-password-hash", Role: identity.RoleUser,
		Status: identity.StatusPending, QuotaHosts: 3, QuotaCPUMilli: 100_000, QuotaMemMiB: 200_000,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	value.Status = identity.StatusApproved
	value, err = store.UpdateUser(ctx, "system", identity.StatusPending, value)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func createF01Workspace(t *testing.T, ctx context.Context, store *storepostgres.Store, ownerID string) workspace.Workspace {
	t.Helper()
	id, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("f01-workspace:" + id))
	value, replayed, err := store.CreateWorkspace(ctx, workspace.Workspace{
		ID: id, OwnerID: ownerID, Name: "F01 cloud task integration", State: workspace.StateReady, QuotaBytes: 1 << 30,
	}, app.IdempotencyRecord{OwnerID: ownerID, Operation: "workspace.create", Key: "f01:" + id, RequestHash: hash[:], ResourceID: id})
	if err != nil || replayed {
		t.Fatalf("create Workspace fixture = %#v, replayed=%t, err=%v", value, replayed, err)
	}
	return value
}

func createF01Host(t *testing.T, ctx context.Context, store *storepostgres.Store, sealer *app.Sealer, ownerID string) (host.Host, string, string) {
	t.Helper()
	hostID, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
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
	value, _, err := store.CreateHost(ctx, app.CreateHostParams{
		Host: host.Host{
			ID: hostID, OwnerID: ownerID, Name: "F01 local Host runtime", TemplateID: "f01-local-template",
			RuntimeProfile: "f01-local", State: host.StateCreating, CPUMilli: 1000, MemMiB: 2048,
			CPULimitMilli: 2000, MemLimitMiB: 4096, AIOKeyEnc: []byte("test-only-encrypted-aio-key"),
			RuntimeCredentialEnc: sealedCredential, RuntimeGeneration: 1,
		}, Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
	})
	if err != nil {
		t.Fatalf("create Host fixture: %v", err)
	}
	if err := store.MarkCreateAttempt(ctx, value.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "f01-local-host"
	if err := store.SetSandboxID(ctx, value.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	value, err = store.TransitionHost(ctx, value.ID, host.StateCreating, host.StateRunning, "")
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(runtimeCredential))
	_, _ = mac.Write([]byte("morde-host-workspace-replica-v1"))
	return value, runtimeCredential, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func createF01Session(t *testing.T, ctx context.Context, store *storepostgres.Store, ownerID string) string {
	t.Helper()
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	credential, secretHash, err := identity.NewCredential(identity.KindSession, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: ownerID, Kind: identity.KindSession, Name: "F01 local integration session",
		SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func f01BindingCredential(t *testing.T, httpClient *http.Client, baseURL string, hostValue host.Host,
	rootCredential, workspaceID string) (string, f01ReplicaBinding) {
	t.Helper()
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspace-bindings", hostValue.ID, hostValue.RuntimeGeneration)
	response, err := f01ReplicaRequest(httpClient, baseURL, http.MethodGet, path, rootCredential, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close Host replica binding response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("list Host replica bindings status=%d body=%s", response.StatusCode, body)
	}
	var result struct {
		Items []f01ReplicaBinding `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	for _, binding := range result.Items {
		if binding.WorkspaceID == workspaceID {
			if binding.SyncCredential == "" {
				t.Fatal("Host replica API omitted the scoped Workspace credential")
			}
			return binding.SyncCredential, binding
		}
	}
	t.Fatalf("Host replica API omitted Workspace %s", workspaceID)
	return "", f01ReplicaBinding{}
}

func f01RecordApplied(httpClient *http.Client, baseURL string, hostValue host.Host, rootCredential, workspaceID string,
	bindingVersion int64, sessionID string, revision int64) (retErr error) {
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "applied_revision": revision})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspaces/%s/bindings/%d/applied",
		hostValue.ID, hostValue.RuntimeGeneration, workspaceID, bindingVersion)
	response, err := f01ReplicaRequest(httpClient, baseURL, http.MethodPost, path, rootCredential, body)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("record Host applied revision status=%d body=%s", response.StatusCode, body)
	}
	return nil
}

func f01ActivateBinding(httpClient *http.Client, baseURL string, hostValue host.Host, rootCredential, workspaceID string, bindingVersion int64) (retErr error) {
	path := fmt.Sprintf("/api/host-replicas/%s/generations/%d/workspaces/%s/bindings/%d/activate",
		hostValue.ID, hostValue.RuntimeGeneration, workspaceID, bindingVersion)
	response, err := f01ReplicaRequest(httpClient, baseURL, http.MethodPost, path, rootCredential, nil)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("activate Host Workspace binding status=%d body=%s", response.StatusCode, body)
	}
	return nil
}

func f01ReplicaRequest(httpClient *http.Client, baseURL, method, path, credential string, body []byte) (*http.Response, error) {
	request, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(request)
}

func waitF01(t *testing.T, ctx context.Context, check func() (bool, error), errors <-chan error, receipts <-chan int64) error {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case err := <-errors:
			return fmt.Errorf("Host replica live-sync receipt: %w", err)
		case <-receipts:
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
