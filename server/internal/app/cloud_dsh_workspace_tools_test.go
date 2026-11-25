package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

type cloudDSHLiveWorkspaceDirectory struct{ root string }

func (d cloudDSHLiveWorkspaceDirectory) ReadDirectory(ownerID, workspaceID, relative string) ([]workspace.DirectoryEntry, error) {
	if ownerID != "u1234567" || workspaceID != "w1234567" {
		return nil, os.ErrNotExist
	}
	root := d.root
	if relative != "" {
		root = filepath.Join(root, filepath.FromSlash(relative))
	}
	values, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name() < values[j].Name() })
	result := make([]workspace.DirectoryEntry, 0, len(values))
	for _, value := range values {
		info, err := os.Lstat(filepath.Join(root, value.Name()))
		if err != nil {
			return nil, err
		}
		kind := "other"
		if info.Mode().IsRegular() {
			kind = "file"
		} else if info.IsDir() {
			kind = "directory"
		} else if info.Mode()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		result = append(result, workspace.DirectoryEntry{Name: value.Name(), Kind: kind, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	return result, nil
}

func (d cloudDSHLiveWorkspaceDirectory) OpenRegularFile(ownerID, workspaceID, relative string) (*os.File, os.FileInfo, error) {
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return nil, nil, os.ErrNotExist
	}
	path := filepath.Join(d.root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = os.ErrInvalid
		}
		return nil, nil, err
	}
	file, err := os.Open(path)
	return file, info, err
}

func (d cloudDSHLiveWorkspaceDirectory) ReadSymbolicLink(ownerID, workspaceID, relative string) (string, error) {
	if ownerID != "u1234567" || workspaceID != "w1234567" {
		return "", os.ErrNotExist
	}
	return os.Readlink(filepath.Join(d.root, filepath.FromSlash(relative)))
}

func (d cloudDSHLiveWorkspaceDirectory) StatPath(ownerID, workspaceID, relative string) (os.FileInfo, error) {
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return nil, os.ErrNotExist
	}
	return os.Lstat(filepath.Join(d.root, filepath.FromSlash(relative)))
}

func (d cloudDSHLiveWorkspaceDirectory) WriteRegularFile(ctx context.Context, ownerID, workspaceID, relative string, content []byte, mode uint32, expectedSHA string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	path := filepath.Join(d.root, filepath.FromSlash(relative))
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if expectedSHA != "" {
			return workspacecurrent.ErrWriteConflict
		}
	} else if err != nil {
		return err
	} else {
		digest := sha256.Sum256(old)
		if expectedSHA == "" || hex.EncodeToString(digest[:]) != expectedSHA {
			return workspacecurrent.ErrWriteConflict
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".live-write-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(os.FileMode(mode)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (d cloudDSHLiveWorkspaceDirectory) WriteRegularFileFrom(ctx context.Context, ownerID, workspaceID, relative string, source io.Reader, size int64,
	digest string, mode uint32, expectedSHA string) error {
	if source == nil || size < 0 {
		return os.ErrInvalid
	}
	content, err := io.ReadAll(io.LimitReader(source, size+1))
	if err != nil || int64(len(content)) != size || sha256Hex(content) != digest {
		return os.ErrInvalid
	}
	return d.WriteRegularFile(ctx, ownerID, workspaceID, relative, content, mode, expectedSHA)
}

func (d cloudDSHLiveWorkspaceDirectory) CreateDirectory(ctx context.Context, ownerID, workspaceID, relative string, mode uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	return os.MkdirAll(filepath.Join(d.root, filepath.FromSlash(relative)), os.FileMode(mode))
}

func (d cloudDSHLiveWorkspaceDirectory) RemoveEmptyDirectory(ctx context.Context, ownerID, workspaceID, relative string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	return os.Remove(filepath.Join(d.root, filepath.FromSlash(relative)))
}

func (d cloudDSHLiveWorkspaceDirectory) RemoveRegularFile(ctx context.Context, ownerID, workspaceID, relative, expectedSHA string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	path := filepath.Join(d.root, filepath.FromSlash(relative))
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256Hex(content) != expectedSHA {
		return workspacecurrent.ErrWriteConflict
	}
	return os.Remove(path)
}

func (d cloudDSHLiveWorkspaceDirectory) WriteSymbolicLink(ctx context.Context, ownerID, workspaceID, relative, target, expectedTarget string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	path := filepath.Join(d.root, filepath.FromSlash(relative))
	current, err := os.Readlink(path)
	if errors.Is(err, os.ErrNotExist) {
		if expectedTarget != "" {
			return workspacecurrent.ErrWriteConflict
		}
	} else if err != nil || current != expectedTarget {
		if err != nil {
			return err
		}
		return workspacecurrent.ErrWriteConflict
	} else if current == target {
		return nil
	} else if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return os.Symlink(target, path)
}

func (d cloudDSHLiveWorkspaceDirectory) RemoveSymbolicLink(ctx context.Context, ownerID, workspaceID, relative, expectedTarget string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID != "u1234567" || workspaceID != "w1234567" || workspace.ValidateRelativePath(relative) != nil {
		return os.ErrNotExist
	}
	path := filepath.Join(d.root, filepath.FromSlash(relative))
	current, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if current != expectedTarget {
		return workspacecurrent.ErrWriteConflict
	}
	return os.Remove(path)
}

func newCloudDSHT08WorkspaceBroker(t *testing.T) (*runtimeToolExecutorFixture, *cloudDSHBrokerTaskStore,
	*CloudDSHToolBroker, *cloudDSHT08WorkspaceStore, task.Task) {
	t.Helper()
	tools, workspaceStore, _, current := newCloudDSHT08WorkspaceTools(t, nil, nil, nil)
	fixture := newRuntimeToolExecutorFixture(t, nil)
	current.ExecutionLocation = "server"
	current.HostID = ""
	fixture.store.details.Task = current
	fixture.executor.workspaces = tools.workspaces
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, store, broker, workspaceStore, current
}

func resumeCloudDSHT08WorkspaceApproval(store *cloudDSHBrokerTaskStore, status task.PendingRequestStatus, decision task.PendingDecision) task.Task {
	store.mu.Lock()
	defer store.mu.Unlock()
	expires := time.Now().Add(time.Minute).UTC()
	store.details.PendingRequest.Status = status
	store.details.PendingRequest.Decision = decision
	responded := time.Now().UTC()
	store.details.PendingRequest.RespondedAt = &responded
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: store.details.Task.Lease.Version + 1, ExpiresAt: &expires}
	return store.details.Task
}

type cloudDSHT08WorkspaceStore struct {
	*syncServiceStore
	commits                          int
	commitAttempts                   int
	commitReturnError                error
	commitErrorAfterApply            bool
	receiptLookupFailuresAfterCommit int
	receiptLookupErrorAfterCommit    error
	receiptMissingAfterCommitError   bool
	receiptStateOverrideAfterCommit  TaskWorkspacePublishState
}

type cloudDSHT08MultiOwnerWorkspaceStore struct {
	*cloudDSHT08WorkspaceStore
	byWorkspaceID map[string]*cloudDSHT08WorkspaceStore
}

type cloudDSHArtifactCaptureStore struct {
	*cloudDSHBrokerTaskStore
	progress TaskProgress
}

func (s *cloudDSHArtifactCaptureStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	s.progress = progress
	return s.cloudDSHBrokerTaskStore.CommitTaskProgress(ctx, progress)
}

func (s *cloudDSHT08MultiOwnerWorkspaceStore) GetWorkspace(ctx context.Context, workspaceID string) (workspace.Workspace, error) {
	if store := s.byWorkspaceID[workspaceID]; store != nil {
		return store.GetWorkspace(ctx, workspaceID)
	}
	return s.cloudDSHT08WorkspaceStore.GetWorkspace(ctx, workspaceID)
}

func (s *cloudDSHT08MultiOwnerWorkspaceStore) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if store := s.byWorkspaceID[workspaceID]; store != nil {
		return store.GetWorkspaceManifestPageForOwner(ctx, ownerID, workspaceID, revision, prefix, after, limit)
	}
	return s.cloudDSHT08WorkspaceStore.GetWorkspaceManifestPageForOwner(ctx, ownerID, workspaceID, revision, prefix, after, limit)
}

func (s *cloudDSHT08WorkspaceStore) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if ownerID != s.workspace.OwnerID {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, limit)
}

func (s *cloudDSHT08WorkspaceStore) GetWorkspaceChangePageForOwner(context.Context, string, string, int64, string, int32) (workspace.ChangePage, error) {
	return workspace.ChangePage{}, nil
}

func (s *cloudDSHT08WorkspaceStore) ReserveTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	receipt, err := s.syncServiceStore.ReserveTaskWorkspacePublish(ctx, value)
	if err != nil {
		return receipt, err
	}
	receipt.RequestHash = hex.EncodeToString(value.RequestHash)
	s.taskWorkspacePublishReceipts[taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)] = receipt
	return receipt, nil
}

func (s *cloudDSHT08WorkspaceStore) GetTaskWorkspacePublishReceipt(ctx context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	receipt, err := s.syncServiceStore.GetTaskWorkspacePublishReceipt(ctx, value)
	if err == nil {
		receipt.RequestHash = hex.EncodeToString(value.RequestHash)
	}
	return receipt, err
}

func (s *cloudDSHT08WorkspaceStore) FindTaskWorkspacePublishReceipt(ctx context.Context, ownerID, taskID, workspaceID, operationKey string) (TaskWorkspacePublishReceipt, error) {
	if s.commitAttempts > 0 && s.receiptLookupFailuresAfterCommit > 0 {
		s.receiptLookupFailuresAfterCommit--
		return TaskWorkspacePublishReceipt{}, s.receiptLookupErrorAfterCommit
	}
	if s.commitAttempts > 0 && s.receiptMissingAfterCommitError {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeNotFound, "Task Workspace publish receipt not found")
	}
	receipt, err := s.syncServiceStore.FindTaskWorkspacePublishReceipt(ctx, ownerID, taskID, workspaceID, operationKey)
	if err == nil {
		command := s.taskWorkspacePublishCommands[taskWorkspacePublishReceiptKey(taskID, operationKey)]
		receipt.RequestHash = hex.EncodeToString(command.RequestHash)
		if s.receiptStateOverrideAfterCommit != "" {
			receipt.State = s.receiptStateOverrideAfterCommit
		}
	}
	return receipt, err
}

func (s *cloudDSHT08WorkspaceStore) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File,
	emptyDirectories []string, directories []workspace.Directory, manifestSHA string) (TaskWorkspacePublishReceipt, error) {
	s.commitAttempts++
	if s.commitReturnError != nil && !s.commitErrorAfterApply {
		return TaskWorkspacePublishReceipt{}, s.commitReturnError
	}
	receipt, err := s.syncServiceStore.CommitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA)
	if err == nil {
		s.commits++
		receipt.RequestHash = hex.EncodeToString(value.RequestHash)
		s.taskWorkspacePublishReceipts[taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)] = receipt
		if s.commitReturnError != nil {
			return TaskWorkspacePublishReceipt{}, s.commitReturnError
		}
	}
	return receipt, err
}

func newCloudDSHT08WorkspaceTools(t *testing.T, files []workspace.File, emptyDirectories []string, directories []workspace.Directory) (*cloudDSHWorkspaceTools, *cloudDSHT08WorkspaceStore, *syncBlockMemory, task.Task) {
	t.Helper()
	store := &cloudDSHT08WorkspaceStore{syncServiceStore: newSyncServiceStore()}
	store.snapshots[0] = append([]workspace.File(nil), files...)
	store.emptyDirectorySnapshots[0] = append([]string(nil), emptyDirectories...)
	store.directorySnapshots[0] = append([]workspace.Directory(nil), directories...)
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	expires := time.Now().Add(time.Minute).UTC()
	current := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", WorkspaceID: "w1234567",
		ExecutionLocation: "server", Status: task.StatusRunning, FileDeliveryIntent: task.FileDeliveryNone, InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
		CurrentRevision: &revision, DesiredRevision: &revision,
		ContextDigest: contextDigest, Lease: task.Lease{Owner: "worker-1", Version: 1, ExpiresAt: &expires},
		Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}}
	return newCloudDSHWorkspaceTools(service), store, blocks, current
}

func workspaceSymlinkFile(filePath, target string) workspace.File {
	digest := sha256.Sum256([]byte(target))
	return workspace.File{Kind: workspace.FileKindSymlink, SymlinkTarget: target, Path: filePath, SizeBytes: int64(len(target)),
		SHA256: hex.EncodeToString(digest[:]), Mode: 0, Chunks: []workspace.Chunk{}}
}

func workspaceToolCall(t *testing.T, name, callID string, arguments any) ModelToolCall {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return ModelToolCall{CallID: callID, Name: name, Arguments: string(encoded)}
}

func approvedWorkspaceWriteForTest(t *testing.T, current task.Task, call ModelToolCall) *task.PendingRequest {
	t.Helper()
	if current.CurrentRevision == nil {
		t.Fatal("test Task revision is missing")
	}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	revision := *current.CurrentRevision
	responded := time.Now().UTC()
	return &task.PendingRequest{ID: "r1234567", TaskID: current.ID, Version: 1, TaskRevision: &revision,
		Kind: "approval", CallID: call.CallID, ToolName: call.Name, RequestHash: hash[:],
		Status: task.PendingRequestApproved, Decision: task.PendingDecisionApprove, RespondedAt: &responded}
}

func TestCloudDSHWorkspaceToolsReadOwnerSnapshotAndRejectTraversal(t *testing.T) {
	file := syncServiceFile("notes/readme.txt", []byte("untrusted note"))
	tools, _, blocks, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{file}, nil, nil)
	blocks.data[file.SHA256] = []byte("untrusted note")
	if _, err := tools.readSnapshot(context.Background(), current, 0); err != nil {
		t.Fatalf("readSnapshot() error = %v", err)
	}
	read := workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "read-call", cloudDSHWorkspaceReadArguments{
		ExpectedRevision: 0, Path: file.Path,
	})
	result, err := tools.execute(context.Background(), current, read, nil, false, nil)
	var decoded cloudDSHWorkspaceReadOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &decoded) != nil ||
		decoded.WorkspaceID != current.WorkspaceID || decoded.Revision != 0 || decoded.Content != "untrusted note" || decoded.SHA256 != file.SHA256 {
		t.Fatalf("Workspace read = %#v, %#v, %v", result, decoded, err)
	}
	for _, path := range []string{"../outside", "/etc/passwd", "notes/../../outside", ".env", ".env.local/private.txt", "settings/.env.production/key", ".ssh/id_rsa"} {
		call := workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "bad-read-"+path, cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: path})
		result, err := tools.execute(context.Background(), current, call, nil, false, nil)
		if err != nil || result.Status != AgentToolFailed {
			t.Errorf("read %q = %#v, %v; want rejected tool result", path, result, err)
		}
	}
	unknownWorkspace := `{"expected_revision":0,"path":"notes/readme.txt","workspace_id":"w7654321"}`
	if _, err := tools.execute(context.Background(), current, ModelToolCall{CallID: "cross-workspace", Name: cloudDSHWorkspaceReadToolName, Arguments: unknownWorkspace}, nil, false, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("caller supplied workspace ID error = %v; want invalid argument", err)
	}
}

func TestCloudDSHWorkspaceReadDoesNotPersistContentForExternalReview(t *testing.T) {
	const content = "A private Workspace source passage."
	file := syncServiceFile("docs/readme.md", []byte(content))
	tools, _, blocks, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{file}, nil, nil)
	blocks.data[file.SHA256] = []byte(content)
	fixture := newRuntimeToolExecutorFixture(t, nil)
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-workspace", 1, "session-root"
	current.CurrentRevision = int64PointerForWorkspaceTools(0)
	current.Authorization.Version = 1
	fixture.store.details.Task = current
	fixture.executor.workspaces = tools.workspaces
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "workspace-read-evidence", Name: cloudDSHWorkspaceReadToolName,
		Arguments: `{"expected_revision":0,"path":"docs/readme.md"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, sealer)
	result, err := broker.executeWorkspaceTool(ctx, current, call, false, nil)
	if err != nil || result.Status != AgentToolSucceeded || len(store.toolCalls) < 2 {
		t.Fatalf("Workspace tool result = %+v; tool progress = %d; err=%v", result, len(store.toolCalls), err)
	}
	progress := store.toolCalls[len(store.toolCalls)-1]
	if progress.ToolName != cloudDSHWorkspaceReadToolName || progress.DetailState != "available" || len(progress.ContentEvidenceSHA256) != 0 {
		t.Fatalf("Workspace read progress = %+v; file content must not be persisted for external review", progress)
	}
	plain, err := sealer.OpenWithAAD(progress.DetailCiphertext,
		taskStepDetailAAD(current.OwnerID, current.ID, store.details.Steps[len(store.details.Steps)-1].ID, progress.TaskRevision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.ContentEvidence != nil || bytes.Contains(plain, []byte(content)) {
		t.Fatalf("encrypted Workspace detail included file bytes for external review: %+v, err=%v", detail.ContentEvidence, err)
	}
}

func TestCloudDSHWorkspaceToolsReadLiveDirectoryInsteadOfSnapshot(t *testing.T) {
	tools, _, _, current := newCloudDSHT08WorkspaceTools(t, nil, nil, nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "live.txt"), []byte("host wrote this"), 0600); err != nil {
		t.Fatal(err)
	}
	tools.workspaces.currentDirectory = cloudDSHLiveWorkspaceDirectory{root: root}
	tools.workspaces.liveDirectorySource = true
	list, err := tools.list(context.Background(), current, cloudDSHWorkspaceListArguments{ExpectedRevision: 0, Limit: 10})
	var listed cloudDSHWorkspaceListOutput
	if err != nil || list.Status != AgentToolSucceeded || json.Unmarshal([]byte(list.Output), &listed) != nil || len(listed.Entries) != 1 || listed.Entries[0].Path != "live.txt" {
		t.Fatalf("live Workspace list = %#v, err=%v", list, err)
	}
	read, err := tools.read(context.Background(), current, cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: "live.txt"})
	var readValue cloudDSHWorkspaceReadOutput
	if err != nil || read.Status != AgentToolSucceeded || json.Unmarshal([]byte(read.Output), &readValue) != nil || readValue.Content != "host wrote this" {
		t.Fatalf("live Workspace read = %#v, err=%v", read, err)
	}
	search, err := tools.search(context.Background(), current, cloudDSHWorkspaceSearchArguments{ExpectedRevision: 0, Query: "HOST WROTE", MaxResults: 10})
	var searchValue cloudDSHWorkspaceSearchOutput
	if err != nil || search.Status != AgentToolSucceeded || json.Unmarshal([]byte(search.Output), &searchValue) != nil || len(searchValue.Matches) != 1 || searchValue.Matches[0].Path != "live.txt" {
		t.Fatalf("live Workspace search = %#v, err=%v", search, err)
	}
}

func TestCloudDSHWorkspaceWriteUpdatesLiveDirectoryWithoutMaterializingRevision(t *testing.T) {
	tools, store, _, current := newCloudDSHT08WorkspaceTools(t, nil, nil, nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "host-file.txt"), []byte("host data"), 0600); err != nil {
		t.Fatal(err)
	}
	tools.workspaces.currentDirectory = cloudDSHLiveWorkspaceDirectory{root: root}
	tools.workspaces.liveDirectorySource = true
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/summary.md", Content: "agent data",
	})
	approval := approvedWorkspaceWriteForTest(t, current, call)
	result, err := tools.execute(context.Background(), current, call, approval, false, nil)
	var receipt cloudDSHWorkspaceWriteReceiptOutput
	if err != nil || result.Status != AgentToolSucceeded || json.Unmarshal([]byte(result.Output), &receipt) != nil ||
		receipt.PublishedRevision != 1 || store.commits != 1 || store.workspace.Revision != 1 {
		t.Fatalf("live Workspace write = %#v, err=%v, receipt=%#v commits=%d revision=%d",
			result, err, receipt, store.commits, store.workspace.Revision)
	}
	digest, digestErr := workspace.ManifestTreeWithMetadataSHA256(store.snapshots[1], store.emptyDirectorySnapshots[1], store.directorySnapshots[1])
	if digestErr != nil || digest != receipt.ManifestSHA256 {
		t.Fatalf("direct receipt manifest digest = %q, err=%v; want %q", digest, digestErr, receipt.ManifestSHA256)
	}
	command := store.taskWorkspacePublishCommands[taskWorkspacePublishReceiptKey(current.ID, agentOperationKey(call.CallID))]
	if command.TaskRevision != *approval.TaskRevision || command.CallID != call.CallID || command.ToolName != call.Name ||
		command.ApprovalID != approval.ID || command.ApprovalVersion != approval.Version || !bytes.Equal(command.ApprovalRequestHash, approval.RequestHash) {
		t.Fatalf("live Workspace publish command lost approval binding: %+v", command)
	}
	key := taskWorkspacePublishReceiptKey(current.ID, agentOperationKey(call.CallID))
	replayed, err := tools.execute(context.Background(), current, call, approval, true, nil)
	if err != nil || replayed.Status != AgentToolSucceeded || store.commits != 1 {
		t.Fatalf("exact live Workspace approval replay = %#v, err=%v, commits=%d", replayed, err, store.commits)
	}
	missingBinding := store.taskWorkspacePublishReceipts[key]
	missingBinding.ApprovalID = ""
	store.taskWorkspacePublishReceipts[key] = missingBinding
	replayed, err = tools.execute(context.Background(), current, call, approval, true, nil)
	if err != nil || replayed.Status == AgentToolSucceeded || store.commits != 1 {
		t.Fatalf("live Workspace replay without approval binding = %#v, err=%v, commits=%d", replayed, err, store.commits)
	}
	for filePath, want := range map[string]string{"host-file.txt": "host data", "reports/summary.md": "agent data"} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(filePath)))
		if err != nil || string(content) != want {
			t.Fatalf("live Workspace %q = %q, err=%v", filePath, content, err)
		}
	}
}

func TestCloudDSHWorkspaceWritePreservesAHostChangeToTheSameFile(t *testing.T) {
	baselineFile := syncServiceFile("report.txt", []byte("manifest version"))
	tools, store, blocks, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{baselineFile}, nil, nil)
	blocks.data[baselineFile.SHA256] = []byte("manifest version")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, baselineFile.Path), []byte("host version"), 0600); err != nil {
		t.Fatal(err)
	}
	tools.workspaces.currentDirectory = cloudDSHLiveWorkspaceDirectory{root: root}
	tools.workspaces.liveDirectorySource = true
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-conflict", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: baselineFile.Path, Content: "agent version",
	})
	approval := approvedWorkspaceWriteForTest(t, current, call)
	result, err := tools.execute(context.Background(), current, call, approval, false, nil)
	content, readErr := os.ReadFile(filepath.Join(root, baselineFile.Path))
	if err != nil || result.Status != AgentToolFailed || readErr != nil || string(content) != "host version" || store.commits != 0 || store.workspace.Revision != 0 {
		t.Fatalf("conflicting live write = %#v, err=%v, file=%q, readErr=%v, commits=%d revision=%d",
			result, err, content, readErr, store.commits, store.workspace.Revision)
	}
}

func TestCloudDSHWorkspaceSearchIsBoundedRevisionScopedAndOmitsRestrictedFiles(t *testing.T) {
	content := []byte("first line\nAI news from the report\nlast line")
	file := syncServiceFile("notes/report.txt", content)
	large := syncServiceFile("archive/large.txt", bytes.Repeat([]byte("x"), cloudDSHWorkspaceReadMaxBytes+1))
	secret := syncServiceFile(".env", []byte("AI_SECRET=do-not-return"))
	link := workspaceSymlinkFile("linked", "outside")
	tools, _, blocks, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{file, large, secret, link}, nil, nil)
	blocks.data[file.SHA256] = content

	call := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-workspace", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 0, Query: "  aI NEWS ", Prefix: "notes", MaxResults: 10,
	})
	result, err := tools.execute(context.Background(), current, call, nil, false, nil)
	var found cloudDSHWorkspaceSearchOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &found) != nil ||
		found.WorkspaceID != current.WorkspaceID || found.Revision != 0 || !found.Complete || found.FilesScanned != 1 ||
		len(found.Matches) != 1 || found.Matches[0].Path != file.Path || found.Matches[0].Line != 2 || found.Matches[0].Text != "AI news from the report" {
		t.Fatalf("Workspace search = %#v, %#v, %v", result, found, err)
	}

	call = workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-workspace-bounded", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 0, Query: "AI",
	})
	result, err = tools.execute(context.Background(), current, call, nil, false, nil)
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &found) != nil ||
		found.Complete || found.SkippedFiles != 1 || len(found.Matches) != 1 || found.Matches[0].Path != file.Path {
		t.Fatalf("bounded Workspace search = %#v, %#v, %v", result, found, err)
	}
	if strings.Contains(result.Output, ".env") || strings.Contains(result.Output, "do-not-return") {
		t.Fatalf("Workspace search exposed restricted content: %s", result.Output)
	}

	symlinkPrefix := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-symlink-prefix", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 0, Query: "AI", Prefix: "linked",
	})
	result, err = tools.execute(context.Background(), current, symlinkPrefix, nil, false, nil)
	if err != nil || result.Status != AgentToolFailed {
		t.Fatalf("search through symlink prefix = %#v, %v; want rejected tool result", result, err)
	}

	stale := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-stale-workspace", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 1, Query: "AI",
	})
	result, err = tools.execute(context.Background(), current, stale, nil, false, nil)
	if err != nil || result.Status != AgentToolFailed {
		t.Fatalf("search stale revision = %#v, %v; want failed tool result", result, err)
	}
}

func TestCloudDSHWorkspaceSearchIsRegisteredForHostlessTasks(t *testing.T) {
	_, _, broker, _, current := newCloudDSHT08WorkspaceBroker(t)
	definitions, err := broker.Definitions(context.Background(), current)
	if err != nil {
		t.Fatalf("broker tool definitions = %v", err)
	}
	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		seen[definition.Name] = true
		if definition.Name == cloudDSHWorkspaceSearchToolName && !definition.Strict {
			t.Errorf("Hostless Workspace search tool must use strict arguments: %#v", definition)
		}
	}
	if !seen[cloudDSHWorkspaceListToolName] || !seen[cloudDSHWorkspaceReadToolName] ||
		!seen[cloudDSHWorkspaceSearchToolName] || !seen[cloudDSHWorkspaceWriteToolName] {
		t.Fatalf("Hostless Workspace tools missing from broker definitions: %#v", definitions)
	}
	for _, forbidden := range []string{
		runtimeToolShellExec, runtimeToolFileRead, runtimeToolFileWrite, runtimeToolFileList, runtimeToolFileFind,
		runtimeToolBrowserOpenTab, runtimeToolBrowserNavigate, runtimeToolBrowserSnapshot, runtimeToolBrowserClick,
		runtimeToolBrowserType, runtimeToolBrowserPressKey, runtimeToolBrowserObserveField, runtimeToolBrowserScreenshot,
		runtimeToolDesktopScreenshot, runtimeToolWindowList, runtimeToolWindowActivate,
		runtimeToolDesktopClick, runtimeToolDesktopType, runtimeToolDesktopPressKey,
	} {
		if seen[forbidden] {
			t.Errorf("Hostless Workspace task exposed Host/runtime tool %q: %#v", forbidden, definitions)
		}
	}
}

func TestCloudDSHWorkspaceWriteRequiresApprovalAndCommitsOnlyTheApprovedCall(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-requires-approval", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/summary.txt", Content: "draft saved only after confirmation",
	})

	first, err := broker.Execute(context.Background(), current, call)
	pending := store.details.PendingRequest
	if err != nil || first.Status != AgentToolNeedsUser || pending == nil || pending.Status != task.PendingRequestPending ||
		pending.CallID != call.CallID || pending.ToolName != call.Name || pending.RuntimeInstanceID != "" ||
		!strings.Contains(pending.Content, "reports/summary.txt") || workspaceStore.commits != 0 || workspaceStore.workspace.Revision != 0 ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "approval_required" {
		t.Fatalf("unapproved Workspace write = %#v, err=%v; pending=%#v commits=%d revision=%d task=%#v",
			first, err, pending, workspaceStore.commits, workspaceStore.workspace.Revision, store.details.Task)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), resumed, call, nil)
	var receipt cloudDSHWorkspaceWriteReceiptOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &receipt) != nil ||
		receipt.Status != "committed" || receipt.PublishedRevision != 1 || workspaceStore.commits != 1 || workspaceStore.workspace.Revision != 1 {
		t.Fatalf("approved Workspace write = %#v, err=%v; receipt=%#v commits=%d revision=%d",
			result, err, receipt, workspaceStore.commits, workspaceStore.workspace.Revision)
	}
	storedApproval := store.details.PendingRequest
	command := workspaceStore.taskWorkspacePublishCommands[taskWorkspacePublishReceiptKey(current.ID, agentOperationKey(call.CallID))]
	persistedReceipt := workspaceStore.taskWorkspacePublishReceipts[taskWorkspacePublishReceiptKey(current.ID, agentOperationKey(call.CallID))]
	if storedApproval == nil || storedApproval.TaskRevision == nil || command.TaskRevision != *storedApproval.TaskRevision ||
		command.CallID != call.CallID || command.ToolName != call.Name || command.ApprovalID != storedApproval.ID ||
		command.ApprovalVersion != storedApproval.Version || !bytes.Equal(command.ApprovalRequestHash, storedApproval.RequestHash) ||
		!taskWorkspacePublishReceiptMatchesApproval(persistedReceipt, call, storedApproval) {
		t.Fatalf("Workspace publish did not persist the exact approved binding: command=%+v receipt=%+v approval=%+v",
			command, persistedReceipt, storedApproval)
	}
	checkpoint := dshToolResultText(result)
	replayed, err := broker.Reconcile(context.Background(), resumed, call, &checkpoint)
	if err != nil || replayed.Status != result.Status || replayed.Output != result.Output || workspaceStore.commits != 1 {
		t.Fatalf("approved Workspace write replay = %#v, err=%v; commits=%d", replayed, err, workspaceStore.commits)
	}
}

func TestCloudDSHWorkspaceWriteCommitResponseAndReceiptLookupFailureStaysUnknown(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-lost-commit-response", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/receipt-recovery.txt", Content: "durable write with a lost response",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial Workspace write = %#v, err=%v; want approval", first, err)
	}
	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	workspaceStore.commitReturnError = errors.New("commit response was lost")
	workspaceStore.commitErrorAfterApply = true
	workspaceStore.receiptLookupErrorAfterCommit = errors.New("receipt lookup is temporarily unavailable")
	workspaceStore.receiptLookupFailuresAfterCommit = 1

	result, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || result.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_result_unknown" || workspaceStore.commitAttempts != 1 ||
		workspaceStore.commits != 1 || workspaceStore.workspace.Revision != 1 {
		t.Fatalf("lost commit response and receipt read = %#v, err=%v; task=%#v commits=%d attempts=%d revision=%d",
			result, err, store.details.Task, workspaceStore.commits, workspaceStore.commitAttempts, workspaceStore.workspace.Revision)
	}

	// Once receipt reads recover, replaying the pinned call must return the
	// durable result without dispatching a second Workspace write.
	recovered := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	replayed, err := broker.Reconcile(context.Background(), recovered, call, nil)
	if err != nil || replayed.Status != AgentToolSucceeded || workspaceStore.commitAttempts != 1 ||
		workspaceStore.commits != 1 || workspaceStore.workspace.Revision != 1 {
		t.Fatalf("receipt recovery replay = %#v, err=%v; commits=%d attempts=%d revision=%d",
			replayed, err, workspaceStore.commits, workspaceStore.commitAttempts, workspaceStore.workspace.Revision)
	}
}

func TestCloudDSHWorkspaceWriteCommitErrorRequiresReceiptOrKnownPreWriteRejection(t *testing.T) {
	for _, test := range []struct {
		name                            string
		commitError                     error
		hideReceipt                     bool
		receiptStateOverrideAfterCommit TaskWorkspacePublishState
		wantStatus                      AgentToolResultStatus
		wantWaitingReason               string
	}{
		{
			name:        "quota rejection without a receipt is agent recoverable",
			commitError: domain.NewError(domain.CodeQuotaExceeded, "workspace quota rejected the publish before commit"),
			hideReceipt: true,
			wantStatus:  AgentToolFailed,
		},
		{
			name:        "revision conflict without a receipt is agent recoverable",
			commitError: domain.NewError(domain.CodeWorkspaceConflict, "workspace revision changed before commit"),
			hideReceipt: true,
			wantStatus:  AgentToolFailed,
		},
		{
			name:                            "terminal quota receipt is agent recoverable",
			commitError:                     errors.New("commit returned an error after quota rejection"),
			receiptStateOverrideAfterCommit: TaskWorkspacePublishQuotaRejected,
			wantStatus:                      AgentToolFailed,
		},
		{
			name:                            "terminal conflict receipt is agent recoverable",
			commitError:                     errors.New("commit returned an error after revision conflict"),
			receiptStateOverrideAfterCommit: TaskWorkspacePublishConflict,
			wantStatus:                      AgentToolFailed,
		},
		{
			name:              "pending receipt remains unknown even for a business error",
			commitError:       domain.NewError(domain.CodeQuotaExceeded, "workspace quota rejected the publish"),
			wantStatus:        AgentToolUnknown,
			wantWaitingReason: "tool_result_unknown",
		},
		{
			name:              "infrastructure error without receipt remains unknown",
			commitError:       errors.New("workspace database connection failed"),
			hideReceipt:       true,
			wantStatus:        AgentToolUnknown,
			wantWaitingReason: "tool_result_unknown",
		},
		{
			name:              "commit timeout without receipt remains unknown",
			commitError:       context.DeadlineExceeded,
			hideReceipt:       true,
			wantStatus:        AgentToolUnknown,
			wantWaitingReason: "tool_result_unknown",
		},
		{
			name:              "unsupported publish store without receipt remains unknown",
			commitError:       domain.NewError(domain.CodeUnsupported, "publish store is unavailable"),
			hideReceipt:       true,
			wantStatus:        AgentToolUnknown,
			wantWaitingReason: "tool_result_unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
			call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-commit-error", cloudDSHWorkspaceWriteArguments{
				ExpectedRevision: 0, Path: "reports/commit-error.txt", Content: "retry only after a known rejection",
			})
			first, err := broker.Execute(context.Background(), current, call)
			if err != nil || first.Status != AgentToolNeedsUser {
				t.Fatalf("initial Workspace write = %#v, err=%v; want approval", first, err)
			}
			resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
			workspaceStore.commitReturnError = test.commitError
			workspaceStore.receiptMissingAfterCommitError = test.hideReceipt
			workspaceStore.receiptStateOverrideAfterCommit = test.receiptStateOverrideAfterCommit

			result, err := broker.Reconcile(context.Background(), resumed, call, nil)
			if err != nil || result.Status != test.wantStatus || workspaceStore.commitAttempts != 1 ||
				workspaceStore.commits != 0 || workspaceStore.workspace.Revision != 0 {
				t.Fatalf("Workspace commit error = %#v, err=%v; commits=%d attempts=%d revision=%d",
					result, err, workspaceStore.commits, workspaceStore.commitAttempts, workspaceStore.workspace.Revision)
			}
			if test.wantWaitingReason != "" && (store.details.Task.Status != task.StatusWaitingUser ||
				store.details.Task.WaitingReason != test.wantWaitingReason) {
				t.Fatalf("unknown commit result did not pause for reconciliation: task=%#v", store.details.Task)
			}
		})
	}
}

func TestCloudDSHWorkspaceWriteReplayRejectsMissingOrChangedApprovalBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*TaskWorkspacePublishReceipt)
	}{
		{name: "missing approval id", mutate: func(value *TaskWorkspacePublishReceipt) { value.ApprovalID = "" }},
		{name: "changed task revision", mutate: func(value *TaskWorkspacePublishReceipt) { value.TaskRevision++ }},
		{name: "changed call id", mutate: func(value *TaskWorkspacePublishReceipt) { value.CallID += "-changed" }},
		{name: "changed approval version", mutate: func(value *TaskWorkspacePublishReceipt) { value.ApprovalVersion++ }},
		{name: "changed approval hash", mutate: func(value *TaskWorkspacePublishReceipt) {
			digest := sha256.Sum256([]byte("changed"))
			value.ApprovalRequestHash = hex.EncodeToString(digest[:])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
			call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-replay-binding-"+strings.ReplaceAll(test.name, " ", "-"),
				cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: "reports/replay.txt", Content: "approved replay"})
			if result, err := broker.Execute(context.Background(), current, call); err != nil || result.Status != AgentToolNeedsUser {
				t.Fatalf("initial Workspace write = %#v, err=%v; want approval", result, err)
			}
			resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
			result, err := broker.Reconcile(context.Background(), resumed, call, nil)
			if err != nil || result.Status != AgentToolSucceeded || workspaceStore.commits != 1 {
				t.Fatalf("approved Workspace write = %#v, err=%v; commits=%d", result, err, workspaceStore.commits)
			}
			key := taskWorkspacePublishReceiptKey(current.ID, agentOperationKey(call.CallID))
			receipt := workspaceStore.taskWorkspacePublishReceipts[key]
			test.mutate(&receipt)
			workspaceStore.taskWorkspacePublishReceipts[key] = receipt
			replayed, err := broker.Reconcile(context.Background(), resumed, call, nil)
			if err != nil || replayed.Status == AgentToolSucceeded || workspaceStore.commits != 1 || workspaceStore.workspace.Revision != 1 {
				t.Fatalf("replay with %s = %#v, err=%v; commits=%d revision=%d", test.name, replayed, err,
					workspaceStore.commits, workspaceStore.workspace.Revision)
			}
		})
	}
}

func TestCloudDSHHostlessWorkspaceWriteCompletionRegistersVerifiedArtifact(t *testing.T) {
	fixture, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	current.Objective = "Write and save a Gomoku design as Markdown."
	setCloudDSHCompletionFence(&current)
	store.details.Task = current

	content := []byte("# Gomoku design\n\n- Board: 15 x 15\n")
	file := syncServiceFile("gomoku-design.md", content)
	workspaceStore.workspace.Revision = 1
	workspaceStore.snapshots[1] = []workspace.File{file}
	fixture.workspaceBlocks = &proofBlocks{syncBlockMemory: newSyncBlockMemory()}
	fixture.workspaceBlocks.data[file.SHA256] = content
	fixture.executor.workspaces.blocks = fixture.workspaceBlocks
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256([]workspace.File{file}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-gomoku-design",
		cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: file.Path, Content: string(content)})
	requestHash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	operationKey := agentOperationKey(call.CallID)
	receipt := TaskWorkspacePublishReceipt{State: TaskWorkspacePublishSucceeded, WorkspaceID: current.WorkspaceID,
		BaseRevision: 0, PublishedRevision: 1, ManifestSHA256: manifestSHA, RequestHash: hex.EncodeToString(requestHash[:])}
	receiptKey := taskWorkspacePublishReceiptKey(current.ID, operationKey)
	workspaceStore.taskWorkspacePublishReceipts[receiptKey] = receipt
	workspaceStore.taskWorkspacePublishCommands[receiptKey] = TaskWorkspacePublishCommand{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: current.WorkspaceID, BaseRevision: 0, RequestHash: requestHash[:],
	}
	toolResult := cloudDSHWorkspaceReceiptResult(receipt, operationKey)
	inputs := []ModelInput{
		{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments},
		{Type: "function_call_output", CallID: call.CallID, Output: toolResult.Output},
	}
	capture := &cloudDSHArtifactCaptureStore{cloudDSHBrokerTaskStore: store}
	broker.store = capture
	if err := broker.Complete(context.Background(), current, cloudDSHCompletionFor(current, "The Gomoku design is ready.", inputs)); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(capture.progress.Artifacts) != 1 {
		t.Fatalf("completed hostless Task artifacts = %#v", capture.progress.Artifacts)
	}
	artifact := capture.progress.Artifacts[0]
	if artifact.TaskID != current.ID || artifact.StepID != capture.progress.CurrentStep || artifact.WorkspaceID != current.WorkspaceID ||
		artifact.Revision != 1 || artifact.Path != file.Path || artifact.MediaType != "text/markdown" ||
		artifact.SizeBytes != int64(len(content)) || artifact.SHA256 != file.SHA256 || capture.progress.Status != task.StatusCompleted {
		t.Fatalf("completed hostless artifact proof = %#v progress=%#v", artifact, capture.progress)
	}
	reader, err := fixture.executor.workspaces.OpenCommittedArtifact(context.Background(), TaskArtifactProof{
		OwnerID: current.OwnerID, WorkspaceID: artifact.WorkspaceID, Revision: artifact.Revision,
		Path: artifact.Path, SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256,
	})
	if err != nil {
		t.Fatalf("open pinned hostless artifact: %v", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close pinned hostless artifact: %v", err)
		}
	}()
	actual, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("downloaded Markdown = %q, err=%v", actual, err)
	}
}

func TestCloudDSHWorkspaceWriteRejectsWithoutChangingWorkspace(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-rejected", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/rejected.txt", Content: "must not be saved",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser || store.details.PendingRequest == nil {
		t.Fatalf("initial Workspace write = %#v, err=%v; want approval", first, err)
	}
	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestRejected, task.PendingDecisionReject)
	result, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "rejected") ||
		workspaceStore.commits != 0 || workspaceStore.workspace.Revision != 0 || len(workspaceStore.snapshots[0]) != 0 {
		t.Fatalf("rejected Workspace write = %#v, err=%v; commits=%d revision=%d files=%#v",
			result, err, workspaceStore.commits, workspaceStore.workspace.Revision, workspaceStore.snapshots[0])
	}
}

func TestCloudDSHWorkspaceWriteExpiredApprovalDoesNotCommit(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-expired", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/expired.txt", Content: "must not be saved",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser || store.details.PendingRequest == nil {
		t.Fatalf("initial Workspace write = %#v err=%v; want approval", first, err)
	}
	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestExpired, task.PendingDecisionReject)
	expiresAt, respondedAt := time.Now().UTC().Add(-time.Minute), time.Now().UTC()
	store.details.PendingRequest.ExpiresAt, store.details.PendingRequest.RespondedAt = &expiresAt, &respondedAt
	result, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "expired") ||
		workspaceStore.commits != 0 || workspaceStore.workspace.Revision != 0 || len(workspaceStore.snapshots[0]) != 0 {
		t.Fatalf("expired Workspace write = %#v err=%v commits=%d revision=%d files=%#v",
			result, err, workspaceStore.commits, workspaceStore.workspace.Revision, workspaceStore.snapshots[0])
	}
}

func TestCloudDSHWorkspaceWriteApprovalCannotAuthorizeChangedContent(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	original := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-changed", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/approved.txt", Content: "approved content",
	})
	if result, err := broker.Execute(context.Background(), current, original); err != nil || result.Status != AgentToolNeedsUser {
		t.Fatalf("initial Workspace write = %#v, err=%v; want approval", result, err)
	}
	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	changed := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-changed", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/approved.txt", Content: "changed after approval",
	})
	result, err := broker.Reconcile(context.Background(), resumed, changed, nil)
	pending := store.details.PendingRequest
	if err != nil || result.Status != AgentToolNeedsUser || pending == nil || pending.Status != task.PendingRequestPending ||
		pending.Version != 2 || pending.RequestHash == nil || workspaceStore.commits != 0 || workspaceStore.workspace.Revision != 0 {
		t.Fatalf("changed Workspace write after approval = %#v, err=%v; pending=%#v commits=%d revision=%d",
			result, err, pending, workspaceStore.commits, workspaceStore.workspace.Revision)
	}
}

func TestCloudDSHWorkspaceToolsIsolateOwnerDataAcrossOwners(t *testing.T) {
	ownerA, ownerB := "u1234567", "u7654321"
	ownerAContent := []byte("owner A workspace")
	ownerBContent := []byte("owner B private workspace")
	ownerAFile := syncServiceFile("notes/owner-a.txt", ownerAContent)
	ownerBFile := syncServiceFile("notes/owner-b.txt", ownerBContent)

	tools, storeA, _, taskA := newCloudDSHT08WorkspaceTools(t, []workspace.File{ownerAFile}, nil, nil)
	_, storeB, _, taskB := newCloudDSHT08WorkspaceTools(t, []workspace.File{ownerBFile}, nil, nil)
	storeA.workspace.OwnerID = ownerA
	storeB.workspace.ID, storeB.workspace.OwnerID = "w7654321", ownerB
	taskA.OwnerID, taskA.InitiatorID = ownerA, ownerA
	taskB.ID, taskB.OwnerID, taskB.InitiatorID, taskB.WorkspaceID = "q7654321", ownerB, ownerB, storeB.workspace.ID

	globalBlocks := newSyncBlockMemory()
	globalBlocks.data[ownerAFile.SHA256] = append([]byte(nil), ownerAContent...)
	globalBlocks.data[ownerBFile.SHA256] = append([]byte(nil), ownerBContent...)
	globalStore := &cloudDSHT08MultiOwnerWorkspaceStore{
		cloudDSHT08WorkspaceStore: storeA,
		byWorkspaceID: map[string]*cloudDSHT08WorkspaceStore{
			storeA.workspace.ID: storeA,
			storeB.workspace.ID: storeB,
		},
	}
	tools.workspaces.store = globalStore
	tools.workspaces.blocks = globalBlocks

	for _, current := range []struct {
		name string
		task task.Task
		file workspace.File
		want string
	}{{"owner A", taskA, ownerAFile, string(ownerAContent)}, {"owner B", taskB, ownerBFile, string(ownerBContent)}} {
		call := workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "read-"+current.name,
			cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: current.file.Path})
		result, err := tools.execute(context.Background(), current.task, call, nil, false, nil)
		if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, current.want) {
			t.Fatalf("%s could not read its own Workspace: result=%#v err=%v", current.name, result, err)
		}
		search := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-"+current.name,
			cloudDSHWorkspaceSearchArguments{ExpectedRevision: 0, Query: "workspace"})
		result, err = tools.execute(context.Background(), current.task, search, nil, false, nil)
		if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, current.file.Path) ||
			!strings.Contains(result.Output, current.want) {
			t.Fatalf("%s could not search its own Workspace: result=%#v err=%v", current.name, result, err)
		}
	}

	taskA.WorkspaceID = storeB.workspace.ID
	attacks := []ModelToolCall{
		workspaceToolCall(t, cloudDSHWorkspaceListToolName, "owner-a-list-owner-b", cloudDSHWorkspaceListArguments{ExpectedRevision: 0, Limit: 10}),
		workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "owner-a-read-owner-b", cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: ownerBFile.Path}),
		workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "owner-a-search-owner-b", cloudDSHWorkspaceSearchArguments{ExpectedRevision: 0, Query: "private workspace"}),
		workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "owner-a-write-owner-b", cloudDSHWorkspaceWriteArguments{
			ExpectedRevision: 0, Path: ownerBFile.Path, Content: "owner A overwrite",
		}),
	}
	for _, call := range attacks {
		var approval *task.PendingRequest
		if call.Name == cloudDSHWorkspaceWriteToolName {
			approval = approvedWorkspaceWriteForTest(t, taskA, call)
		}
		result, err := tools.execute(context.Background(), taskA, call, approval, false, nil)
		if err != nil || result.Status != AgentToolFailed || strings.Contains(result.Output, string(ownerBContent)) || strings.Contains(result.Output, ownerBFile.Path) {
			t.Errorf("owner A tool %s exposed or accepted owner B data: result=%#v err=%v", call.Name, result, err)
		}
	}

	if storeB.workspace.Revision != 0 || storeB.commits != 0 || len(storeB.snapshots[0]) != 1 ||
		storeB.snapshots[0][0].Path != ownerBFile.Path || storeB.snapshots[0][0].SHA256 != ownerBFile.SHA256 ||
		string(globalBlocks.data[ownerBFile.SHA256]) != string(ownerBContent) || len(storeB.taskWorkspacePublishReceipts) != 0 {
		t.Fatalf("owner A modified owner B Workspace data: workspace=%#v commits=%d files=%#v receipts=%d",
			storeB.workspace, storeB.commits, storeB.snapshots[0], len(storeB.taskWorkspacePublishReceipts))
	}
	if storeA.workspace.Revision != 0 || storeA.commits != 0 || len(storeA.taskWorkspacePublishReceipts) != 0 {
		t.Fatalf("foreign Workspace write changed owner A state: workspace=%#v commits=%d receipts=%d",
			storeA.workspace, storeA.commits, len(storeA.taskWorkspacePublishReceipts))
	}
}

func TestCloudDSHWorkspaceToolsRejectSymlinkTraversalAndWrites(t *testing.T) {
	link := workspaceSymlinkFile("linked", "notes")
	tools, _, _, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{link}, nil, nil)
	for _, call := range []ModelToolCall{
		workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "read-link", cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: "linked"}),
		workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "read-through-link", cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: "linked/private.txt"}),
		workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "read-through-casefold-link", cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: "LINKED/private.txt"}),
		workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-through-link", cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: "linked/new.txt", Content: "blocked"}),
		workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-through-casefold-link", cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: "LINKED/new.txt", Content: "blocked"}),
		workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "replace-link", cloudDSHWorkspaceWriteArguments{ExpectedRevision: 0, Path: "linked", Content: "blocked"}),
	} {
		var approval *task.PendingRequest
		if call.Name == cloudDSHWorkspaceWriteToolName {
			approval = approvedWorkspaceWriteForTest(t, current, call)
		}
		result, err := tools.execute(context.Background(), current, call, approval, false, nil)
		if err != nil || result.Status != AgentToolFailed {
			t.Errorf("symlink call %s = %#v, %v; want rejected", call.CallID, result, err)
		}
	}
}

func TestCloudDSHWorkspaceWriteCommitsCASReceiptAndReplaysAfterRestart(t *testing.T) {
	tools, store, blocks, current := newCloudDSHT08WorkspaceTools(t, nil, nil, nil)
	if _, err := tools.readSnapshot(context.Background(), current, 0); err != nil {
		t.Fatalf("empty readSnapshot() error = %v", err)
	}
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-1", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/result.txt", Content: "receipt-backed result",
	})
	approval := approvedWorkspaceWriteForTest(t, current, call)
	result, err := tools.execute(context.Background(), current, call, approval, false, nil)
	var receipt cloudDSHWorkspaceWriteReceiptOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &receipt) != nil ||
		receipt.Status != "committed" || receipt.WorkspaceID != current.WorkspaceID || receipt.BaseRevision != 0 ||
		receipt.PublishedRevision != 1 || !validSyncDigest(receipt.ManifestSHA256) || !validSyncDigest(receipt.RequestSHA256) ||
		store.workspace.Revision != 1 || store.commits != 1 {
		t.Fatalf("Workspace write = %#v, %#v, %v; commits=%d revision=%d", result, receipt, err, store.commits, store.workspace.Revision)
	}
	current.WorkspaceCheckpointRevision = int64PointerForWorkspaceTools(1)
	current.Lease.Version = 2
	checkpoint := dshToolResultText(result)
	replayed, err := tools.execute(context.Background(), current, call, approval, true, &checkpoint)
	if err != nil || replayed.Status != result.Status || replayed.Output != result.Output || store.commits != 1 {
		t.Fatalf("restarted write replay = %#v, %v; prior=%#v commits=%d", replayed, err, result, store.commits)
	}
	secondWrite := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-2", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 1, Path: "reports/second.txt", Content: "second receipt-backed result",
	})
	secondApproval := approvedWorkspaceWriteForTest(t, current, secondWrite)
	secondResult, err := tools.execute(context.Background(), current, secondWrite, secondApproval, false, nil)
	var secondReceipt cloudDSHWorkspaceWriteReceiptOutput
	if err != nil || secondResult.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(secondResult.Output), &secondReceipt) != nil ||
		secondReceipt.BaseRevision != 1 || secondReceipt.PublishedRevision != 2 || store.commits != 2 || store.workspace.Revision != 2 {
		t.Fatalf("second checkpoint write = %#v, %#v, %v; commits=%d revision=%d", secondResult, secondReceipt, err, store.commits, store.workspace.Revision)
	}
	secondContent := []byte("second receipt-backed result")
	secondDigest := sha256.Sum256(secondContent)
	if string(blocks.data[hex.EncodeToString(secondDigest[:])]) != string(secondContent) {
		t.Fatal("second file bytes were not stored under their verified digest")
	}
	changed := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-1", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 1, Path: "reports/result.txt", Content: "different body",
	})
	changedApproval := approvedWorkspaceWriteForTest(t, current, changed)
	changedResult, err := tools.execute(context.Background(), current, changed, changedApproval, false, nil)
	if err != nil || changedResult.Status != AgentToolFailed || store.commits != 2 {
		t.Fatalf("reused operation key with different body = %#v, %v; commits=%d", changedResult, err, store.commits)
	}
	stale := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-stale", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/stale.txt", Content: "must not commit",
	})
	staleApproval := approvedWorkspaceWriteForTest(t, current, stale)
	staleResult, err := tools.execute(context.Background(), current, stale, staleApproval, false, nil)
	if err != nil || staleResult.Status != AgentToolFailed || store.commits != 2 || store.workspace.Revision != 2 {
		t.Fatalf("stale Workspace write = %#v, %v; commits=%d revision=%d", staleResult, err, store.commits, store.workspace.Revision)
	}
}

func TestCloudDSHWorkspaceWriteRejectsWorkspaceAdvancedPastCheckpoint(t *testing.T) {
	tools, store, _, current := newCloudDSHT08WorkspaceTools(t, nil, nil, nil)
	store.workspace.Revision = 1
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "stale-external", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "stale.txt", Content: "no",
	})
	approval := approvedWorkspaceWriteForTest(t, current, call)
	result, err := tools.execute(context.Background(), current, call, approval, false, nil)
	if err != nil || result.Status != AgentToolFailed || store.commits != 0 || len(store.taskWorkspacePublishReceipts) != 0 {
		t.Fatalf("externally stale write = %#v, %v; commits=%d receipts=%d", result, err, store.commits, len(store.taskWorkspacePublishReceipts))
	}
}

func int64PointerForWorkspaceTools(value int64) *int64 { return &value }
