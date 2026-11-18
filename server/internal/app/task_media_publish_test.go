package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type taskMediaApprovedSaveStoreStub struct{ object TaskMediaStagedObject }

func (s taskMediaApprovedSaveStoreStub) GetTaskMediaForApprovedSave(context.Context, string, string, string, int64, string) (TaskMediaStagedObject, error) {
	return s.object, nil
}

type taskMediaPublishFailStore struct {
	*syncServiceStore
	failNext bool
}

func (s *taskMediaPublishFailStore) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File,
	emptyDirectories []string, directories []workspace.Directory, manifestSHA string) (TaskWorkspacePublishReceipt, error) {
	if s.failNext {
		s.failNext = false
		return TaskWorkspacePublishReceipt{}, errors.New("simulated commit interruption")
	}
	return s.syncServiceStore.CommitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA)
}

type taskMediaArtifactTaskStore struct{ *runtimeToolTaskStore }

func (s *taskMediaArtifactTaskStore) RecordValidatedArtifact(_ context.Context, ownerID string, artifact task.Artifact, leaseOwner string, leaseVersion int64) (task.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ownerID != s.details.Task.OwnerID || leaseOwner != s.details.Task.Lease.Owner || leaseVersion != s.details.Task.Lease.Version {
		return task.Artifact{}, domain.NewError(domain.CodeInvalidState, "task lease changed")
	}
	s.details.Artifacts = append(s.details.Artifacts, artifact)
	return artifact, nil
}

type countingLiveWorkspaceDirectory struct {
	cloudDSHLiveWorkspaceDirectory
	writes int
}

func (d *countingLiveWorkspaceDirectory) WriteRegularFileFrom(ctx context.Context, ownerID, workspaceID, relative string, source io.Reader, size int64,
	digest string, mode uint32, expectedSHA string) error {
	d.writes++
	return d.cloudDSHLiveWorkspaceDirectory.WriteRegularFileFrom(ctx, ownerID, workspaceID, relative, source, size, digest, mode, expectedSHA)
}

func TestTaskMediaSaveIdentityAndPath(t *testing.T) {
	const operationID = "o1234567"
	if got := taskMediaSaveStepID(operationID); got != "s1234567" {
		t.Fatalf("taskMediaSaveStepID() = %q", got)
	}
	if got := taskMediaSaveStepID("invalid"); got != "" {
		t.Fatalf("taskMediaSaveStepID(invalid) = %q", got)
	}
	if got, err := taskMediaArtifactPath(operationID, "video/mp4"); err != nil || got != "morde-media-o1234567.mp4" {
		t.Fatalf("taskMediaArtifactPath() = %q, %v", got, err)
	}
	if _, err := taskMediaArtifactPath(operationID, "application/octet-stream"); err == nil {
		t.Fatal("taskMediaArtifactPath() accepted an unsupported media type")
	}
}

func TestTaskMediaApprovalBindingSeparatesLedgerIDAndBindsToolOperation(t *testing.T) {
	current := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567"}
	requestHash := sha256.Sum256([]byte("tool request"))
	pending := task.PendingRequest{TaskID: current.ID, CallID: "tool-call-1", MediaPreviewOperationID: "o1234567", RequestHash: requestHash[:]}
	staged := TaskMediaStagedObject{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		OperationID: pending.MediaPreviewOperationID, RuntimeCallID: "model-call-1",
		OperationKey: agentOperationKey(pending.CallID), RequestHash: requestHash[:]}
	if staged.RuntimeCallID == pending.CallID {
		t.Fatal("test requires distinct model ledger and provider tool-call IDs")
	}
	if !taskMediaApprovalBindingMatches(current, pending, staged) {
		t.Fatal("Task media operation with a separate settled model call ID did not match its approval")
	}
	staged.OperationKey = agentOperationKey("different-tool-call")
	if taskMediaApprovalBindingMatches(current, pending, staged) {
		t.Fatal("Task media operation matched a different tool call")
	}
	staged.OperationKey = agentOperationKey(pending.CallID)
	differentHash := sha256.Sum256([]byte("different request"))
	staged.RequestHash = differentHash[:]
	if taskMediaApprovalBindingMatches(current, pending, staged) {
		t.Fatal("Task media operation matched a different tool request")
	}
	staged.RequestHash = requestHash[:]
	pending.MediaPreviewOperationID = "o8765432"
	if taskMediaApprovalBindingMatches(current, pending, staged) {
		t.Fatal("Task media operation matched an approval for a different operation")
	}
}

func TestApprovedTaskMediaWorkspaceConflictIsVisibleAndDiscardsPreview(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store

	requestHash := sha256.Sum256([]byte("synthetic approved media request"))
	mediaDigest := sha256.Sum256([]byte("png"))
	pending := task.PendingRequest{
		ID: "r1234567", TaskID: store.details.Task.ID, Version: 1, Kind: "approval",
		CallID: "synthetic-media-call", MediaPreviewOperationID: "o1234567", ToolName: "morde_generate_image",
		RequestHash: requestHash[:], Status: task.PendingRequestApproved, Decision: task.PendingDecisionApprove,
	}
	store.details.PendingRequest = &pending
	inputTokens, outputTokens := int64(1), int64(0)
	staged := TaskMediaStagedObject{
		TaskID: store.details.Task.ID, OwnerID: store.details.Task.OwnerID, InitiatorID: store.details.Task.InitiatorID,
		OperationID: pending.MediaPreviewOperationID, RuntimeCallID: "model-call-1",
		OperationKey: agentOperationKey(pending.CallID), RequestHash: append([]byte(nil), requestHash[:]...),
		OperationStatus: string(task.OperationSucceeded), TokenState: "settled", ReservedTokens: 1,
		InputTokens: &inputTokens, OutputTokens: &outputTokens, MediaType: "image/png", SizeBytes: 3,
		SHA256: mediaDigest[:], State: TaskMediaPreviewSaving, CleanupState: TaskMediaCleanupPending,
	}
	stagedStore := &taskMediaPublishStagedStoreStub{object: staged}
	receiptStore := &taskMediaPublishReceiptStoreStub{receipt: TaskWorkspacePublishReceipt{State: TaskWorkspacePublishConflict}}
	fixture.executor.workspaces = &WorkspaceSyncService{store: receiptStore}
	fixture.executor.mediaStore = stagedStore
	fixture.executor.mediaSave = &TaskMediaSaveService{}

	broker := &CloudDSHToolBroker{store: store, runtime: fixture.executor}
	current := store.details.Task
	result, err := broker.executeApprovedTaskMediaSave(context.Background(), current,
		ModelToolCall{CallID: pending.CallID, Name: pending.ToolName}, store.details, pending, nil)
	if err != nil || result.Status != AgentToolFailed ||
		result.Output != "The approved media could not be saved because the Task Workspace changed or its storage quota was reached. Submit a new task after resolving the Workspace issue." {
		t.Fatalf("approved media conflict result = %#v, %v", result, err)
	}
	if len(store.events) != 2 || len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepFailed ||
		stagedStore.object.State != TaskMediaPreviewDiscard || receiptStore.calls != 1 {
		t.Fatalf("media failure was not durably visible and discarded: events=%d steps=%+v staged=%+v receipt calls=%d",
			len(store.events), store.details.Steps, stagedStore.object, receiptStore.calls)
	}
}

func TestApprovedTaskMediaWritesLiveWorkspaceAndReplaysAfterCommitInterruption(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	current := fixture.store.details.Task
	stepID := "s1234567"
	operationID := "o1234567"
	requestHash := sha256.Sum256([]byte("approved media request"))
	media := []byte("png")
	mediaDigest := sha256.Sum256(media)
	runtimeSession, err := fixture.executor.runtime.Connect(context.Background(), current.HostID, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeFence := AgentHostFence{RuntimeGeneration: runtimeSession.Fence.RuntimeGeneration,
		RuntimeInstanceID: runtimeSession.Fence.RuntimeInstanceID, ControlVersion: runtimeSession.Fence.ControlVersion, AIAllowed: true}
	runtimeSession.Close()
	fixture.fence, fixture.fences.fence = runtimeFence, runtimeFence
	pending := task.PendingRequest{ID: "r1234567", TaskID: current.ID, Version: 1, Kind: "approval",
		CallID: "approved-media-call", MediaPreviewOperationID: operationID, ToolName: "morde_generate_image", RequestHash: requestHash[:],
		RuntimeGeneration: runtimeFence.RuntimeGeneration, RuntimeInstanceID: runtimeFence.RuntimeInstanceID,
		ControlVersion: runtimeFence.ControlVersion, Status: task.PendingRequestApproved, Decision: task.PendingDecisionApprove}
	fixture.store.details.PendingRequest = &pending
	fixture.store.details.Steps = []task.Step{{ID: stepID, TaskID: current.ID, Status: task.StepRunning}}
	staged := TaskMediaStagedObject{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		OperationID: operationID, OperationKey: agentOperationKey(pending.CallID), RequestHash: requestHash[:],
		OperationStatus: string(task.OperationSucceeded), TokenState: "settled", ReservedTokens: 1,
		InputTokens: int64Pointer(1), OutputTokens: int64Pointer(0), MediaType: "image/png", SizeBytes: int64(len(media)),
		SHA256: mediaDigest[:], State: TaskMediaPreviewSaving, CleanupState: TaskMediaCleanupPending}
	stagedStore := &taskMediaPublishStagedStoreStub{object: staged}
	service := fixture.executor.workspaces
	baseStore, ok := service.store.(*syncServiceStore)
	if !ok {
		t.Fatalf("Workspace sync test store has type %T", service.store)
	}
	service.store = &taskMediaPublishFailStore{syncServiceStore: baseStore, failNext: true}
	service.liveDirectorySource = true
	liveDirectory := &countingLiveWorkspaceDirectory{cloudDSHLiveWorkspaceDirectory: cloudDSHLiveWorkspaceDirectory{root: t.TempDir()}}
	if err := service.SetWorkspaceCurrentDirectory(liveDirectory); err != nil {
		t.Fatal(err)
	}
	fixture.executor.store = &taskMediaArtifactTaskStore{runtimeToolTaskStore: fixture.store}
	content := taskMediaPreviewContentStub{open: func(_ context.Context, ownerID, taskID, gotOperationID string, expected TaskMediaStagedContent) (io.ReadCloser, error) {
		if ownerID != current.OwnerID || taskID != current.ID || gotOperationID != operationID || expected.SizeBytes != int64(len(media)) ||
			hex.EncodeToString(expected.SHA256) != hex.EncodeToString(mediaDigest[:]) {
			t.Fatalf("approved media content request mismatch: owner=%q task=%q operation=%q expected=%#v", ownerID, taskID, gotOperationID, expected)
		}
		return io.NopCloser(bytes.NewReader(media)), nil
	}}
	mediaSave, err := NewTaskMediaSaveService(taskMediaApprovedSaveStoreStub{object: staged}, content)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.executor.SetTaskMediaSaveService(mediaSave, stagedStore); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.executor.SaveApprovedTaskMedia(context.Background(), current, pending, stepID); err == nil || err.Error() != "simulated commit interruption" {
		t.Fatalf("first save error = %v; want simulated commit interruption", err)
	}
	filePath := filepath.Join(liveDirectory.root, "morde-media-o1234567.png")
	if got, err := os.ReadFile(filePath); err != nil || !bytes.Equal(got, media) {
		t.Fatalf("live media after interrupted commit = %q, %v", got, err)
	}
	saved, err := fixture.executor.SaveApprovedTaskMedia(context.Background(), current, pending, stepID)
	if err != nil || saved.Path != "morde-media-o1234567.png" || saved.Revision != 1 || saved.SHA256 != hex.EncodeToString(mediaDigest[:]) {
		t.Fatalf("replayed approved media save = %#v, %v", saved, err)
	}
	if liveDirectory.writes != 1 {
		t.Fatalf("live media writes = %d, want one write across retry", liveDirectory.writes)
	}
	if stagedStore.object.State != TaskMediaPreviewSaved || len(fixture.store.details.Artifacts) != 1 {
		t.Fatalf("saved media state=%q, artifacts=%d; want durable Artifact and saved staged object", stagedStore.object.State, len(fixture.store.details.Artifacts))
	}
}

type taskMediaPublishReceiptStoreStub struct {
	WorkspaceSyncStore
	receipt TaskWorkspacePublishReceipt
	calls   int
}

func (s *taskMediaPublishReceiptStoreStub) FindTaskWorkspacePublishReceipt(context.Context, string, string, string, string) (TaskWorkspacePublishReceipt, error) {
	s.calls++
	return s.receipt, nil
}

type taskMediaPublishStagedStoreStub struct {
	TaskMediaStagedObjectStore
	object TaskMediaStagedObject
}

func (s *taskMediaPublishStagedStoreStub) GetTaskMediaOperationDetails(context.Context, string, string) (TaskMediaStagedObject, error) {
	return s.object, nil
}

func (s *taskMediaPublishStagedStoreStub) SetTaskMediaStagedObjectState(_ context.Context, taskID, operationID string,
	expected, next TaskMediaStagedObjectState) (TaskMediaStagedObject, error) {
	if taskID != s.object.TaskID || operationID != s.object.OperationID || s.object.State != expected {
		return TaskMediaStagedObject{}, domain.NewError(domain.CodeConflict, "staged Task media state changed")
	}
	s.object.State = next
	return s.object, nil
}

func TestWriteTaskMediaChunksVerifiesAndSpoolsContent(t *testing.T) {
	content := bytes.Repeat([]byte("media-block-"), int(workspace.MaxSyncBlockBytes/12)+2)
	digest := sha256.Sum256(content)
	file, locations, gotDigest, spool := writeTestTaskMedia(t, context.Background(), content, int64(len(content)), digest[:])
	if file.SizeBytes != int64(len(content)) || file.SHA256 != hex.EncodeToString(digest[:]) || gotDigest != file.SHA256 || len(file.Chunks) != 2 {
		t.Fatalf("spooled media metadata = %#v, digest=%q", file, gotDigest)
	}
	if len(locations) == 0 {
		t.Fatal("spooled media has no block locations")
	}
	var reconstructed []byte
	for _, chunk := range file.Chunks {
		location, ok := locations[chunk.SHA256]
		if !ok || location.size != chunk.SizeBytes {
			t.Fatalf("missing block location for %#v: %#v", chunk, location)
		}
		block := make([]byte, location.size)
		if _, err := spool.ReadAt(block, location.offset); err != nil {
			t.Fatalf("read spooled block: %v", err)
		}
		blockDigest := sha256.Sum256(block)
		if hex.EncodeToString(blockDigest[:]) != chunk.SHA256 {
			t.Fatalf("spooled block digest does not match manifest chunk %q", chunk.SHA256)
		}
		reconstructed = append(reconstructed, block...)
	}
	if !bytes.Equal(reconstructed, content) {
		t.Fatal("spooled blocks do not reconstruct the approved media bytes")
	}
}

func TestWriteTaskMediaChunksRejectsSizeAndDigestChanges(t *testing.T) {
	content := []byte("approved-media")
	digest := sha256.Sum256(content)
	wrongDigest := sha256.Sum256([]byte("different-media"))
	for _, test := range []struct {
		name     string
		input    []byte
		size     int64
		expected []byte
	}{
		{name: "short", input: content[:len(content)-1], size: int64(len(content)), expected: digest[:]},
		{name: "trailing", input: append(append([]byte(nil), content...), 'x'), size: int64(len(content)), expected: digest[:]},
		{name: "digest", input: content, size: int64(len(content)), expected: wrongDigest[:]},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "task-media-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Errorf("close test spool: %v", err)
				}
			})
			if _, _, _, err := writeTaskMediaChunks(context.Background(), file, bytes.NewReader(test.input), test.size, test.expected); err == nil {
				t.Fatal("writeTaskMediaChunks() accepted changed content")
			}
		})
	}
}

func writeTestTaskMedia(t *testing.T, ctx context.Context, content []byte, size int64, digest []byte) (workspace.File, map[string]taskMediaBlockLocation, string, *os.File) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "task-media-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	manifestFile, locations, gotDigest, err := writeTaskMediaChunks(ctx, file, bytes.NewReader(content), size, digest)
	if err != nil {
		t.Fatalf("writeTaskMediaChunks() error = %v", err)
	}
	return manifestFile, locations, gotDigest, file
}
