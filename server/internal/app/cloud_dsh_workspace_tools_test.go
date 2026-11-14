package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

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
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: store.details.Task.Lease.Version + 1, ExpiresAt: &expires}
	return store.details.Task
}

type cloudDSHT08WorkspaceStore struct {
	*syncServiceStore
	commits int
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
	receipt, err := s.syncServiceStore.FindTaskWorkspacePublishReceipt(ctx, ownerID, taskID, workspaceID, operationKey)
	if err == nil {
		command := s.taskWorkspacePublishCommands[taskWorkspacePublishReceiptKey(taskID, operationKey)]
		receipt.RequestHash = hex.EncodeToString(command.RequestHash)
	}
	return receipt, err
}

func (s *cloudDSHT08WorkspaceStore) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File,
	emptyDirectories []string, directories []workspace.Directory, manifestSHA string) (TaskWorkspacePublishReceipt, error) {
	receipt, err := s.syncServiceStore.CommitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA)
	if err == nil {
		s.commits++
		receipt.RequestHash = hex.EncodeToString(value.RequestHash)
		s.taskWorkspacePublishReceipts[taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)] = receipt
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
		ExecutionLocation: "server", Status: task.StatusRunning, InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
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
	result, err := tools.execute(context.Background(), current, read, false, nil)
	var decoded cloudDSHWorkspaceReadOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &decoded) != nil ||
		decoded.WorkspaceID != current.WorkspaceID || decoded.Revision != 0 || decoded.Content != "untrusted note" || decoded.SHA256 != file.SHA256 {
		t.Fatalf("Workspace read = %#v, %#v, %v", result, decoded, err)
	}
	for _, path := range []string{"../outside", "/etc/passwd", "notes/../../outside", ".env", ".env.local/private.txt", "settings/.env.production/key", ".ssh/id_rsa"} {
		call := workspaceToolCall(t, cloudDSHWorkspaceReadToolName, "bad-read-"+path, cloudDSHWorkspaceReadArguments{ExpectedRevision: 0, Path: path})
		result, err := tools.execute(context.Background(), current, call, false, nil)
		if err != nil || result.Status != AgentToolFailed {
			t.Errorf("read %q = %#v, %v; want rejected tool result", path, result, err)
		}
	}
	unknownWorkspace := `{"expected_revision":0,"path":"notes/readme.txt","workspace_id":"w7654321"}`
	if _, err := tools.execute(context.Background(), current, ModelToolCall{CallID: "cross-workspace", Name: cloudDSHWorkspaceReadToolName, Arguments: unknownWorkspace}, false, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("caller supplied workspace ID error = %v; want invalid argument", err)
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
	result, err := tools.execute(context.Background(), current, call, false, nil)
	var found cloudDSHWorkspaceSearchOutput
	if err != nil || result.Status != AgentToolSucceeded || decodeStrictRuntimeResult([]byte(result.Output), &found) != nil ||
		found.WorkspaceID != current.WorkspaceID || found.Revision != 0 || !found.Complete || found.FilesScanned != 1 ||
		len(found.Matches) != 1 || found.Matches[0].Path != file.Path || found.Matches[0].Line != 2 || found.Matches[0].Text != "AI news from the report" {
		t.Fatalf("Workspace search = %#v, %#v, %v", result, found, err)
	}

	call = workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-workspace-bounded", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 0, Query: "AI",
	})
	result, err = tools.execute(context.Background(), current, call, false, nil)
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
	result, err = tools.execute(context.Background(), current, symlinkPrefix, false, nil)
	if err != nil || result.Status != AgentToolFailed {
		t.Fatalf("search through symlink prefix = %#v, %v; want rejected tool result", result, err)
	}

	stale := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-stale-workspace", cloudDSHWorkspaceSearchArguments{
		ExpectedRevision: 1, Query: "AI",
	})
	result, err = tools.execute(context.Background(), current, stale, false, nil)
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
	checkpoint := dshToolResultText(result)
	replayed, err := broker.Reconcile(context.Background(), resumed, call, &checkpoint)
	if err != nil || replayed.Status != result.Status || replayed.Output != result.Output || workspaceStore.commits != 1 {
		t.Fatalf("approved Workspace write replay = %#v, err=%v; commits=%d", replayed, err, workspaceStore.commits)
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
		result, err := tools.execute(context.Background(), current.task, call, false, nil)
		if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, current.want) {
			t.Fatalf("%s could not read its own Workspace: result=%#v err=%v", current.name, result, err)
		}
		search := workspaceToolCall(t, cloudDSHWorkspaceSearchToolName, "search-"+current.name,
			cloudDSHWorkspaceSearchArguments{ExpectedRevision: 0, Query: "workspace"})
		result, err = tools.execute(context.Background(), current.task, search, false, nil)
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
		result, err := tools.execute(context.Background(), taskA, call, false, nil)
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
		result, err := tools.execute(context.Background(), current, call, false, nil)
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
	result, err := tools.execute(context.Background(), current, call, false, nil)
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
	replayed, err := tools.execute(context.Background(), current, call, true, &checkpoint)
	if err != nil || replayed.Status != result.Status || replayed.Output != result.Output || store.commits != 1 {
		t.Fatalf("restarted write replay = %#v, %v; prior=%#v commits=%d", replayed, err, result, store.commits)
	}
	secondWrite := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-2", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 1, Path: "reports/second.txt", Content: "second receipt-backed result",
	})
	secondResult, err := tools.execute(context.Background(), current, secondWrite, false, nil)
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
	changedResult, err := tools.execute(context.Background(), current, changed, false, nil)
	if err != nil || changedResult.Status != AgentToolFailed || store.commits != 2 {
		t.Fatalf("reused operation key with different body = %#v, %v; commits=%d", changedResult, err, store.commits)
	}
	stale := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-call-stale", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/stale.txt", Content: "must not commit",
	})
	staleResult, err := tools.execute(context.Background(), current, stale, false, nil)
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
	result, err := tools.execute(context.Background(), current, call, false, nil)
	if err != nil || result.Status != AgentToolFailed || store.commits != 0 || len(store.taskWorkspacePublishReceipts) != 0 {
		t.Fatalf("externally stale write = %#v, %v; commits=%d receipts=%d", result, err, store.commits, len(store.taskWorkspacePublishReceipts))
	}
}

func int64PointerForWorkspaceTools(value int64) *int64 { return &value }
