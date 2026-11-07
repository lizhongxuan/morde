package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

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
