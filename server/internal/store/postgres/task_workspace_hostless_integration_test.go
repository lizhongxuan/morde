package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/storage/workspacefs"
)

func TestHostBoundAgentWorkspaceTaskPublishRecordsDirectDirectorySnapshot(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil {
		t.Fatalf("create Agent Workspace: %v", err)
	}
	if err := store.MarkAgentWorkspaceDirectoryInitialized(ctx, owner.ID, workspaceValue.ID); err != nil {
		t.Fatalf("mark Agent Workspace directory initialized: %v", err)
	}
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, taskWorkspacePublishRuntimeID); err != nil {
		t.Fatal(err)
	}
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatalf("bind Host to Agent Workspace: %v", err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatalf("activate Agent Workspace Host binding: %v", err)
	}
	manifest, err := store.GetWorkspaceManifestPageForOwner(ctx, owner.ID, workspaceValue.ID, workspaceValue.Revision, "", "", 100)
	if err != nil {
		t.Fatalf("read Agent Workspace manifest: %v", err)
	}
	inputManifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(manifest.Files, manifest.EmptyDirectories, manifest.Directories)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(inputManifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	inputRevision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "agent-workspace-direct-publish", "write a report")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &inputRevision
	submission.Task.ContextDigest = contextDigest
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Host-bound Agent Workspace task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "agent-workspace-direct-publish-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Host-bound Agent Workspace task: %#v found=%t err=%v", claimed, found, err)
	}
	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	directory, err := workspacecurrent.Open(filepath.Join(t.TempDir(), "current"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	service, err := app.NewWorkspaceSyncServiceWithOptions(store, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes,
		app.WorkspaceSyncOptions{LiveDirectorySourceEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentDirectory(directory); err != nil {
		t.Fatal(err)
	}
	content := []byte("shared Agent Workspace report")
	file := syncTestFile("report.txt", content)
	files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	emptyDirectories := []string{}
	requestHash := sha256.Sum256([]byte("direct Agent Workspace publish"))
	command := app.TaskWorkspacePublishCommand{
		OwnerID: owner.ID, TaskID: claimed.ID, WorkspaceID: workspaceValue.ID, HostID: hostValue.ID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, BindingVersion: binding.BindingVersion,
		InputRevision: inputRevision, BaseRevision: inputRevision,
		HostFence: app.AgentHostFence{RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: taskWorkspacePublishRuntimeID,
			ControlVersion: hostValue.ControlVersion, AIAllowed: true},
		ViewID: "v" + strings.Repeat("a", 64), InputManifestSHA256: inputManifestSHA,
		OperationKey: "agent:direct-directory-publish", RequestHash: requestHash[:], DirectDirectorySource: true,
	}
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatalf("reserve direct Host-bound Agent Workspace publish: %v", err)
	}
	receipt, err := service.CommitTaskWorkspacePublishDirect(ctx, command, files, emptyDirectories, directories, manifestSHA,
		func(_ context.Context, digest string) ([]byte, error) {
			if digest != file.SHA256 {
				return nil, domain.NewError(domain.CodeNotFound, "unexpected Agent Workspace block")
			}
			return append([]byte(nil), content...), nil
		})
	if err != nil || receipt.State != app.TaskWorkspacePublishSucceeded || !receipt.DirectDirectorySource ||
		receipt.PublishedRevision != inputRevision+1 {
		t.Fatalf("commit direct Host-bound Agent Workspace snapshot = %#v, %v", receipt, err)
	}
	updatedBinding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil || updatedBinding.DesiredRevision != receipt.PublishedRevision || updatedBinding.AppliedRevision != receipt.PublishedRevision ||
		updatedBinding.State != workspace.BindingActive || updatedBinding.SyncState != workspace.BindingSyncHealthy {
		t.Fatalf("Host binding after direct Workspace snapshot = %#v, %v; want revision %d", updatedBinding, err, receipt.PublishedRevision)
	}
	updatedTask, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || updatedTask.Task.WorkspaceCheckpointRevision == nil || *updatedTask.Task.WorkspaceCheckpointRevision != receipt.PublishedRevision {
		t.Fatalf("Task checkpoint after direct Workspace snapshot = %#v, %v", updatedTask.Task.WorkspaceCheckpointRevision, err)
	}
}

func TestHostlessWorkspaceTaskSubmitPublishReceiptAndRecovery(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "hostless-task-workspace")
	initialPage, err := store.GetWorkspaceManifestPageForOwner(ctx, owner.ID, workspaceValue.ID, workspaceValue.Revision, "", "", 100)
	if err != nil {
		t.Fatalf("read initial owner-scoped Workspace manifest: %v", err)
	}
	inputDigest, err := workspace.ManifestTreeWithMetadataSHA256(initialPage.Files, initialPage.EmptyDirectories, initialPage.Directories)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(inputDigest)
	if err != nil {
		t.Fatal(err)
	}
	inputRevision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, "", "hostless-workspace-submit-receipt", "write a short workspace report")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &inputRevision
	submission.Task.ContextDigest = contextDigest
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Hostless Workspace task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hostless-workspace-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.HostID != "" || claimed.WorkspaceID != workspaceValue.ID {
		t.Fatalf("claim Hostless Workspace task = %#v found=%t err=%v", claimed, found, err)
	}
	secondSubmission := newTestTaskSubmission(t, owner.ID, "", "hostless-workspace-second-submit", "write another report")
	secondSubmission.Task.WorkspaceID = workspaceValue.ID
	secondSubmission.Task.InputRevision = &inputRevision
	secondSubmission.Task.ContextDigest = append([]byte(nil), contextDigest...)
	secondCreated, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatalf("submit second Hostless Workspace task: %v", err)
	}
	secondClaimed, found, err := store.ClaimNextTask(ctx, "hostless-workspace-worker-2", time.Minute)
	if err != nil || !found || secondClaimed.ID != secondCreated.Task.ID || secondClaimed.WorkspaceID != workspaceValue.ID {
		t.Fatalf("claim second Hostless Workspace task = %#v found=%t err=%v", secondClaimed, found, err)
	}
	if claimed.WorkspaceCheckpointRevision == nil || *claimed.WorkspaceCheckpointRevision != inputRevision {
		t.Fatalf("claimed task checkpoint = %#v, want %d", claimed.WorkspaceCheckpointRevision, inputRevision)
	}
	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	service, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("Hostless Workspace result")
	fileDigest := sha256.Sum256(content)
	fileSHA := hex.EncodeToString(fileDigest[:])
	rootMode := uint32(0o755)
	rootModTime := int64(1)
	copyDirectories := []workspace.Directory{{Path: ".", Mode: &rootMode, ModTimeNS: &rootModTime, MetadataKnown: true}}
	files := append([]workspace.File(nil), initialPage.Files...)
	files = append(files, workspace.File{Kind: workspace.FileKindRegular, Path: "report.txt", SizeBytes: int64(len(content)),
		SHA256: fileSHA, Mode: 0644, MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: fileSHA, SizeBytes: int64(len(content))}}})
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, initialPage.EmptyDirectories, copyDirectories)
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("workspace write call:report.txt"))
	command := app.TaskWorkspacePublishCommand{
		OwnerID: owner.ID, TaskID: claimed.ID, WorkspaceID: workspaceValue.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, InputRevision: inputRevision, BaseRevision: inputRevision,
		InputManifestSHA256: inputDigest, OperationKey: "agent:hostless-write-001", RequestHash: requestHash[:],
	}
	reserved, err := service.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil || reserved.State != app.TaskWorkspacePublishPending {
		t.Fatalf("reserve Hostless Workspace receipt = %#v, %v", reserved, err)
	}
	committed, err := service.CommitTaskWorkspacePublish(ctx, command, files, initialPage.EmptyDirectories, copyDirectories, manifestSHA,
		func(_ context.Context, digest string) ([]byte, error) {
			if digest != fileSHA {
				return nil, domain.NewError(domain.CodeNotFound, "unexpected Workspace block")
			}
			return append([]byte(nil), content...), nil
		})
	if err != nil || committed.State != app.TaskWorkspacePublishSucceeded || committed.PublishedRevision != inputRevision+1 ||
		committed.ManifestSHA256 != manifestSHA || committed.RequestHash != hex.EncodeToString(requestHash[:]) {
		t.Fatalf("commit Hostless Workspace revision = %#v, %v", committed, err)
	}
	updated, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || updated.Task.WorkspaceCheckpointRevision == nil || *updated.Task.WorkspaceCheckpointRevision != committed.PublishedRevision {
		t.Fatalf("Task checkpoint after Workspace commit = %#v, %v", updated.Task.WorkspaceCheckpointRevision, err)
	}
	replayed, err := service.CommitTaskWorkspacePublish(ctx, command, files, initialPage.EmptyDirectories, copyDirectories, manifestSHA,
		func(context.Context, string) ([]byte, error) {
			t.Fatal("receipt replay requested block bytes")
			return nil, nil
		})
	if err != nil || replayed != committed {
		t.Fatalf("restart receipt replay = %#v, %v; original=%#v", replayed, err, committed)
	}

	directContent := []byte("shared directory result")
	directDigest := sha256.Sum256(directContent)
	directSHA := hex.EncodeToString(directDigest[:])
	directFiles := append([]workspace.File(nil), files...)
	directFiles = append(directFiles, workspace.File{Kind: workspace.FileKindRegular, Path: "shared.txt", SizeBytes: int64(len(directContent)),
		SHA256: directSHA, Mode: 0644, MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: directSHA, SizeBytes: int64(len(directContent))}}})
	directDirectories := append([]workspace.Directory(nil), copyDirectories...)
	directManifest, err := workspace.ManifestTreeWithMetadataSHA256(directFiles, initialPage.EmptyDirectories, directDirectories)
	if err != nil {
		t.Fatal(err)
	}
	directHash := sha256.Sum256([]byte("direct Workspace write call"))
	directCommand := command
	directCommand.BaseRevision = committed.PublishedRevision
	directCommand.OperationKey = "agent:hostless-direct-write-001"
	directCommand.RequestHash = directHash[:]
	directCommand.DirectDirectorySource = true
	directReserved, err := service.ReserveTaskWorkspacePublish(ctx, directCommand)
	if err != nil || directReserved.State != app.TaskWorkspacePublishPending {
		t.Fatalf("reserve direct Workspace write receipt = %#v, %v", directReserved, err)
	}
	directCommitted, err := service.CommitTaskWorkspacePublishDirect(ctx, directCommand, directFiles, initialPage.EmptyDirectories, directDirectories, directManifest,
		func(_ context.Context, digest string) ([]byte, error) {
			if digest == directSHA {
				return append([]byte(nil), directContent...), nil
			}
			return nil, domain.NewError(domain.CodeNotFound, "unexpected direct Workspace block")
		})
	if err != nil || directCommitted.State != app.TaskWorkspacePublishSucceeded || !directCommitted.DirectDirectorySource ||
		directCommitted.PublishedRevision != committed.PublishedRevision+1 {
		t.Fatalf("commit direct Workspace snapshot = %#v, %v", directCommitted, err)
	}
	var materializedRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceValue.ID).Scan(&materializedRevision); err != nil || materializedRevision != committed.PublishedRevision {
		t.Fatalf("direct Workspace write queued materialization revision = %d, err=%v; want previous revision %d", materializedRevision, err, committed.PublishedRevision)
	}
	secondHash := sha256.Sum256([]byte("second Hostless Workspace write call"))
	secondCommand := app.TaskWorkspacePublishCommand{
		OwnerID: owner.ID, TaskID: secondClaimed.ID, WorkspaceID: workspaceValue.ID, LeaseOwner: secondClaimed.Lease.Owner,
		LeaseVersion: secondClaimed.Lease.Version, InputRevision: inputRevision, BaseRevision: inputRevision,
		InputManifestSHA256: inputDigest, OperationKey: "agent:hostless-write-002", RequestHash: secondHash[:],
	}
	if _, err := service.ReserveTaskWorkspacePublish(ctx, secondCommand); err != nil {
		t.Fatalf("reserve second task at old checkpoint: %v", err)
	}
	if _, err := service.CommitTaskWorkspacePublish(ctx, secondCommand, files, initialPage.EmptyDirectories, copyDirectories, manifestSHA,
		func(_ context.Context, digest string) ([]byte, error) {
			if digest != fileSHA {
				return nil, domain.NewError(domain.CodeNotFound, "unexpected Workspace block")
			}
			return append([]byte(nil), content...), nil
		}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("second task publish after Workspace advanced error = %v; want strict CAS conflict", err)
	}
	conflictReceipt, err := store.FindTaskWorkspacePublishReceipt(ctx, owner.ID, secondClaimed.ID, workspaceValue.ID, secondCommand.OperationKey)
	if err != nil || conflictReceipt.State != app.TaskWorkspacePublishConflict || conflictReceipt.PublishedRevision != 0 {
		t.Fatalf("stale publish receipt = %#v, %v; want durable conflict", conflictReceipt, err)
	}
	differentRequest := command
	differentRequest.RequestHash = hostlessWorkspaceSHA256Bytes("different content under same operation key")
	if _, err := service.ReserveTaskWorkspacePublish(ctx, differentRequest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("operation key with different request hash error = %v; want conflict", err)
	}
	stale := command
	stale.OperationKey = "agent:hostless-stale-write"
	stale.RequestHash = hostlessWorkspaceSHA256Bytes("stale write")
	if _, err := service.ReserveTaskWorkspacePublish(ctx, stale); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale base revision error = %v; want workspace conflict", err)
	}
	wrongWorkspace := command
	otherWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "hostless-other-workspace")
	wrongWorkspace.WorkspaceID = otherWorkspace.ID
	wrongWorkspace.OperationKey = "agent:hostless-cross-workspace"
	wrongWorkspace.RequestHash = hostlessWorkspaceSHA256Bytes("cross workspace")
	if _, err := service.ReserveTaskWorkspacePublish(ctx, wrongWorkspace); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("Task Workspace receipt outside the task binding error = %v; want workspace conflict", err)
	}
}

func TestHostlessWorkspaceWriteApprovalPersistsWithoutHostFence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "hostless-workspace-approval")
	manifest, err := store.GetWorkspaceManifestPageForOwner(ctx, owner.ID, workspaceValue.ID, workspaceValue.Revision, "", "", 100)
	if err != nil {
		t.Fatalf("read Workspace manifest: %v", err)
	}
	digest, err := workspace.ManifestTreeWithMetadataSHA256(manifest.Files, manifest.EmptyDirectories, manifest.Directories)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(digest)
	if err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, "", "hostless-workspace-approval-submit", "write a workspace file")
	submission.Task.ExecutionLocation = "server"
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = contextDigest
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Hostless Workspace task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hostless-workspace-approval-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Hostless Workspace task = %#v found=%t err=%v", claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: 1, Kind: "approval", Title: "Approve Workspace save",
		Object: "Save a file to your Workspace", Content: "Path: report.txt · SHA-256: test digest",
		Impact: "The file is saved only after you approve.", CallID: "workspace-write-call-1",
		ToolName: app.CloudDSHWorkspaceWriteToolName, RequestHash: hashBytes("exact workspace write call"), Status: task.PendingRequestPending,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
			Title: request.Title, Summary: request.Content}},
		Event:          task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
		PendingRequest: request,
	}); err != nil {
		t.Fatalf("persist Hostless Workspace approval: %v", err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.PendingRequest == nil || details.PendingRequest.ID != pendingID || details.PendingRequest.RuntimeInstanceID != "" ||
		details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("read Hostless Workspace approval = %#v err=%v", details, err)
	}
	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.pending.respond", Key: "hostless-workspace-approval-response",
		RequestHash: hashBytes("approve:" + pendingID + ":1"), ResourceID: claimed.ID, RelatedID: pendingID}
	if err := store.RespondToPendingRequest(ctx, record, claimed.ID, pendingID, 1, task.PendingDecisionApprove); err != nil {
		t.Fatalf("approve Hostless Workspace write: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.PendingRequest == nil ||
		details.PendingRequest.Status != task.PendingRequestApproved || details.PendingRequest.Decision != task.PendingDecisionApprove {
		t.Fatalf("Hostless Workspace approval response = %#v err=%v", details, err)
	}
}

func hostlessWorkspaceSHA256Bytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
