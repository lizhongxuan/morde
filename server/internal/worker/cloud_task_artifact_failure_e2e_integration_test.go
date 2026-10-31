package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

var errInjectedCloudArtifactCommit = errors.New("injected transient artifact commit failure")

type artifactCommitFailOnceStore struct {
	app.TaskStore
	fired atomic.Bool
}

func (s *artifactCommitFailOnceStore) CommitTaskProgress(ctx context.Context, progress app.TaskProgress) (task.Event, error) {
	if progress.Status == task.StatusCompleted && len(progress.Artifacts) > 0 && s.fired.CompareAndSwap(false, true) {
		return task.Event{}, errInjectedCloudArtifactCommit
	}
	return s.TaskStore.CommitTaskProgress(ctx, progress)
}

type artifactFailureWriteCounter struct {
	runtimecore.ActionHandler
	calls *atomic.Int32
}

func (h *artifactFailureWriteCounter) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	h.calls.Add(1)
	return h.ActionHandler.Execute(ctx, request)
}

func TestCloudTaskArtifactCommitFailureRecoversWithoutRedispatch(t *testing.T) {
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
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate empty test Workspace binding: %v", err)
	}

	hostRoot := t.TempDir()
	workspaceDirectory := filepath.Join(hostRoot, workspaceValue.ID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "Persist this artifact exactly once.\n"
	if err := os.WriteFile(filepath.Join(workspaceDirectory, "recovery-note.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	taskWorkspace, err := runtimefiles.OpenTaskWorkspace(hostRoot,
		filepath.Join(hostRoot, ".morde-task-views"), uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := taskWorkspace.Close(); err != nil {
			t.Errorf("close Host Task Workspace views: %v", err)
		}
	}()
	var fileWriteCalls atomic.Int32
	runtimeHandlers := taskWorkspace.Handlers()
	for index, handler := range runtimeHandlers {
		if handler.Capability() == "file.write" && handler.Action() == "write" {
			runtimeHandlers[index] = &artifactFailureWriteCounter{ActionHandler: handler, calls: &fileWriteCalls}
		}
	}
	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()),
		HostID: hostValue.ID, Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: runtimeHandlers, TaskWorkspaceViews: taskWorkspace,
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
		t.Fatalf("verify task session: principal=%#v err=%v", principal, err)
	}
	contentDigest := sha256.Sum256([]byte(content))
	contentHash := hex.EncodeToString(contentDigest[:])
	manifestFile := workspace.File{
		Path: "recovery-note.txt", SizeBytes: int64(len(content)), SHA256: contentHash, Mode: 0o600,
		Chunks: []workspace.Chunk{{SHA256: contentHash, SizeBytes: int64(len(content))}},
	}
	initialUpload, err := syncService.CreateSyncSession(ctx, principal, workspaceValue.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{manifestFile},
	}, "f01-artifact-initial-upload")
	if err != nil {
		t.Fatalf("create initial Workspace upload session: %v", err)
	}
	if written, err := syncService.PutSyncBlock(ctx, principal, workspaceValue.ID, initialUpload.ID, contentHash, strings.NewReader(content)); err != nil || written != int64(len(content)) {
		t.Fatalf("upload initial Workspace content block: written=%d err=%v", written, err)
	}
	initialCommitted, err := syncService.CommitSyncSession(ctx, principal, workspaceValue.ID, initialUpload.ID, "f01-artifact-initial-commit")
	if err != nil || initialCommitted.State != workspace.SyncCompleted || initialCommitted.TargetRevision != 1 {
		t.Fatalf("commit initial Workspace file: session=%#v err=%v", initialCommitted, err)
	}
	localWorkspaceFile, err := os.ReadFile(filepath.Join(workspaceDirectory, manifestFile.Path))
	if err != nil || !bytes.Equal(localWorkspaceFile, []byte(content)) {
		t.Fatalf("Host Workspace file before download receipt = %q, err=%v", localWorkspaceFile, err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	replicaPrincipal, err := hostReplicas.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, replicaCredential)
	if err != nil {
		t.Fatal(err)
	}
	replicaPrincipal, err = hostReplicas.ScopeWorkspace(ctx, replicaPrincipal, workspaceValue.ID, binding.BindingVersion)
	if err != nil {
		t.Fatal(err)
	}
	download, err := syncService.CreateSyncSession(ctx, replicaPrincipal, workspaceValue.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{manifestFile},
	}, "f01-artifact-host-download")
	if err != nil {
		t.Fatalf("create Host replica download session: %v", err)
	}
	completedDownload, err := syncService.CommitSyncSession(ctx, replicaPrincipal, workspaceValue.ID, download.ID, "f01-artifact-host-download-commit")
	if err != nil || completedDownload.State != workspace.SyncCompleted || completedDownload.TargetRevision != 1 {
		t.Fatalf("complete Host replica download session: session=%#v err=%v", completedDownload, err)
	}
	if _, err := store.AdvanceWorkspaceBindingAppliedRevision(ctx, hostValue.ID, hostValue.RuntimeGeneration, workspaceValue.ID,
		binding.BindingVersion, completedDownload.ID, completedDownload.TargetRevision); err != nil {
		t.Fatalf("record applied revision from Host download session: %v", err)
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
		t.Fatalf("connect Host runtime before task: %v", err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	decoratedStore := &artifactCommitFailOnceStore{TaskStore: store}
	toolBroker, err := app.NewCloudDSHToolBroker(decoratedStore, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	inputRevision := int64(1)
	submitted, err := taskService.Submit(ctx, principal, app.SubmitTaskCommand{
		HostID: hostValue.ID, WorkspaceID: workspaceValue.ID, InputRevision: &inputRevision,
		ModelID: "p1234567", Objective: "Write and verify a note in the cloud Workspace.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}, IdempotencyKey: "f01-artifact-commit-failure",
	})
	if err != nil {
		t.Fatalf("submit cloud task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "f01-artifact-failure-worker", time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim cloud task: claimed=%t task=%q err=%v", found, claimed.ID, err)
	}

	stopKey := bytes.Repeat([]byte{0x56}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-artifact-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x6a}, 32)
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, AgentInstanceID: "agent_artifact_failure",
		AgentSessionID: "session_artifact_failure", SupervisorID: "f01-artifact-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed || started.Fence.AgentGeneration != 1 {
		t.Fatalf("start persisted dsh identity: lease=%+v replayed=%t err=%v", started, replayed, err)
	}
	checkpointPayload := []byte(`{"header":{"id":"session_artifact_failure"},"from_seq":0,"events":[{"seq":0}]}`)
	checkpointDigest := sha256.Sum256(checkpointPayload)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointFence := app.AgentCheckpointFence{
		TaskID: started.Fence.TaskID, OwnerID: started.Fence.OwnerID,
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AgentSessionID: started.Fence.AgentSessionID, AgentRootSessionID: started.Fence.AgentSessionID,
		AuthorizationVersion: started.Fence.AuthorizationVersion, LeaseOwner: started.Fence.LeaseOwner,
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion,
	}
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{
		Fence: checkpointFence, From: 0, Through: 0, Digest: checkpointDigest[:], Payload: checkpointPayload,
	}); err != nil {
		t.Fatalf("persist agent checkpoint: %v", err)
	}

	callID := "artifact-failure-file-write-001"
	arguments, err := json.Marshal(map[string]string{
		"workspace_id": workspaceValue.ID, "path": "recovery-note.txt", "content": content,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := app.ModelToolCall{CallID: callID, Name: "file_write", Arguments: string(arguments)}
	toolResult, err := toolBroker.Execute(ctx, claimed, call)
	if err != nil || toolResult.Status != app.AgentToolSucceeded {
		t.Fatalf("execute Task Workspace file_write: result=%+v err=%v", toolResult, err)
	}
	operationKey := f01OperationKey(callID)
	writeOperation, err := store.GetOperation(ctx, owner.ID, claimed.ID, operationKey)
	if err != nil || writeOperation.Status != task.OperationSucceeded || writeOperation.ID == "" {
		t.Fatalf("persist successful Task Workspace operation: operation=%#v err=%v", writeOperation, err)
	}
	if fileWriteCalls.Load() != 1 {
		t.Fatalf("Task Workspace file_write side effect count before artifact recovery = %d, want 1", fileWriteCalls.Load())
	}

	completion := app.CloudDSHTaskCompletion{
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AuthorizationVersion: started.Fence.AuthorizationVersion, AgentSessionID: started.Fence.AgentSessionID,
		CheckpointThroughSequence: 0, CheckpointDigest: checkpointDigest[:], Result: "The note was saved and verified.",
		Inputs: []app.ModelInput{
			{Type: "function_call", CallID: callID, Name: call.Name, Arguments: call.Arguments},
			{Type: "function_call_output", CallID: callID, Output: toolResult.Output},
		},
	}
	receipt := app.AgentStopReceipt{Fence: started.Fence, SupervisorNonce: nonce, TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, stopKey)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	if _, err := instances.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm dsh process tree stopped: %v", err)
	}

	if err := toolBroker.Complete(ctx, claimed, completion); !errors.Is(err, errInjectedCloudArtifactCommit) {
		afterFirstCompletion, readErr := store.GetTask(ctx, claimed.ID, owner.ID)
		t.Fatalf("first completion error = %v, want injected transient artifact commit error; task=%s waiting=%s steps=%#v inputs=%#v read_error=%v",
			err, afterFirstCompletion.Task.Status, afterFirstCompletion.Task.WaitingReason, afterFirstCompletion.Steps, completion.Inputs, readErr)
	}
	if !decoratedStore.fired.Load() {
		t.Fatal("completion did not reach the injected artifact commit boundary")
	}
	afterFailure, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || afterFailure.Task.Status != task.StatusRunning || len(afterFailure.Artifacts) != 0 {
		t.Fatalf("task after artifact transaction failure = status %s artifacts=%d err=%v; want running without artifacts",
			afterFailure.Task.Status, len(afterFailure.Artifacts), err)
	}
	firstOperationID := writeOperation.ID
	if err := toolBroker.Complete(ctx, claimed, completion); err != nil {
		t.Fatalf("retry completion after transient artifact failure: %v", err)
	}
	completed, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || len(completed.Artifacts) != 1 ||
		completed.Task.WorkspaceCheckpointRevision == nil || *completed.Task.WorkspaceCheckpointRevision != 2 {
		t.Fatalf("task after completion retry = status %s waiting=%s checkpoint=%#v artifacts=%d steps=%#v err=%v; want completed with one artifact",
			completed.Task.Status, completed.Task.WaitingReason, completed.Task.WorkspaceCheckpointRevision,
			len(completed.Artifacts), completed.Steps, err)
	}
	finalOperation, err := store.GetOperation(ctx, owner.ID, claimed.ID, operationKey)
	if err != nil || finalOperation.ID != firstOperationID || finalOperation.Status != task.OperationSucceeded {
		t.Fatalf("file_write operation changed across recovery: before=%q after=%#v err=%v", firstOperationID, finalOperation, err)
	}
	if fileWriteCalls.Load() != 1 {
		t.Fatalf("Task Workspace file_write side effect count after completion retry = %d, want 1", fileWriteCalls.Load())
	}
	artifact := completed.Artifacts[0]
	if artifact.WorkspaceID != workspaceValue.ID || artifact.Revision != 2 || artifact.Path != manifestFile.Path ||
		artifact.SizeBytes != manifestFile.SizeBytes || artifact.SHA256 != manifestFile.SHA256 {
		t.Fatalf("recovered artifact metadata = %#v", artifact)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, artifact.Revision, "", "", 10)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != artifact.Path || manifest.Files[0].SHA256 != artifact.SHA256 {
		t.Fatalf("recovered committed Workspace manifest = %#v err=%v", manifest, err)
	}
	hostBytes, err := os.ReadFile(filepath.Join(workspaceDirectory, artifact.Path))
	if err != nil || string(hostBytes) != content {
		t.Fatalf("Host file after artifact recovery = %q err=%v", hostBytes, err)
	}
}
