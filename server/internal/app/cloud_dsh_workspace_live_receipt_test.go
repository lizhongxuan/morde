package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type cloudDSHLiveReceiptFaultStore struct {
	*cloudDSHT08WorkspaceStore
	commitCalls                      int
	commitErrorAfterApply            error
	rejectCommitAsConflict           bool
	findErrorsAfterCommit            int
	findError                        error
	pendingReceiptLookupsAfterCommit int
}

func (s *cloudDSHLiveReceiptFaultStore) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand,
	files []workspace.File, emptyDirectories []string, directories []workspace.Directory, manifestSHA string) (TaskWorkspacePublishReceipt, error) {
	s.commitCalls++
	if s.rejectCommitAsConflict {
		if err := s.RejectTaskWorkspacePublishConflict(ctx, value); err != nil {
			return TaskWorkspacePublishReceipt{}, err
		}
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace revision changed before commit")
	}
	receipt, err := s.cloudDSHT08WorkspaceStore.CommitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA)
	if err != nil {
		return receipt, err
	}
	if s.commitErrorAfterApply != nil {
		return TaskWorkspacePublishReceipt{}, s.commitErrorAfterApply
	}
	return receipt, nil
}

func (s *cloudDSHLiveReceiptFaultStore) FindTaskWorkspacePublishReceipt(ctx context.Context, ownerID, taskID, workspaceID, operationKey string) (TaskWorkspacePublishReceipt, error) {
	if s.commitCalls > 0 && s.findErrorsAfterCommit > 0 {
		s.findErrorsAfterCommit--
		return TaskWorkspacePublishReceipt{}, s.findError
	}
	receipt, err := s.cloudDSHT08WorkspaceStore.FindTaskWorkspacePublishReceipt(ctx, ownerID, taskID, workspaceID, operationKey)
	if err == nil && s.commitCalls > 0 && s.pendingReceiptLookupsAfterCommit > 0 {
		s.pendingReceiptLookupsAfterCommit--
		receipt.State = TaskWorkspacePublishPending
	}
	return receipt, err
}

type cloudDSHLiveCountingDirectory struct {
	cloudDSHLiveWorkspaceDirectory
	writes               int
	writeErrorAfterApply error
}

func (d *cloudDSHLiveCountingDirectory) WriteRegularFile(ctx context.Context, ownerID, workspaceID, relative string, content []byte, mode uint32, expectedSHA string) error {
	d.writes++
	if err := d.cloudDSHLiveWorkspaceDirectory.WriteRegularFile(ctx, ownerID, workspaceID, relative, content, mode, expectedSHA); err != nil {
		return err
	}
	return d.writeErrorAfterApply
}

type cloudDSHLiveSnapshotFailDirectory struct {
	*cloudDSHLiveCountingDirectory
	failAfterWrite bool
}

func (d *cloudDSHLiveSnapshotFailDirectory) ReadDirectory(ownerID, workspaceID, relative string) ([]workspace.DirectoryEntry, error) {
	if d.failAfterWrite && d.writes > 0 {
		return nil, errors.New("injected Workspace snapshot read failure")
	}
	return d.cloudDSHLiveCountingDirectory.ReadDirectory(ownerID, workspaceID, relative)
}

func configureCloudDSHLiveReceiptTest(t *testing.T, fixture *runtimeToolExecutorFixture, base *cloudDSHT08WorkspaceStore,
	root string, fault *cloudDSHLiveReceiptFaultStore) *cloudDSHLiveCountingDirectory {
	t.Helper()
	directory := &cloudDSHLiveCountingDirectory{cloudDSHLiveWorkspaceDirectory: cloudDSHLiveWorkspaceDirectory{root: root}}
	fixture.executor.workspaces.currentDirectory = directory
	fixture.executor.workspaces.liveDirectorySource = true
	fixture.executor.workspaces.store = fault
	return directory
}

func TestCloudDSHLiveWorkspaceWriteLostCommitResponseAndReceiptReadStaysUnknown(t *testing.T) {
	fixture, store, broker, base, current := newCloudDSHT08WorkspaceBroker(t)
	fault := &cloudDSHLiveReceiptFaultStore{
		cloudDSHT08WorkspaceStore: base,
		commitErrorAfterApply:     errors.New("commit response was lost"),
		findErrorsAfterCommit:     1,
		findError:                 errors.New("receipt lookup is temporarily unavailable"),
	}
	root := t.TempDir()
	directory := configureCloudDSHLiveReceiptTest(t, fixture, base, root, fault)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write-lost-commit-response", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/live-receipt-recovery.txt", Content: "committed live write with a lost response",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial live Workspace write = %#v, err=%v; want approval", first, err)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), resumed, call, nil)
	content, readErr := os.ReadFile(filepath.Join(root, "reports", "live-receipt-recovery.txt"))
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_result_unknown" || readErr != nil ||
		string(content) != "committed live write with a lost response" || directory.writes != 1 ||
		fault.commitCalls != 1 || base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("live Workspace lost result = %#v, err=%v; task=%#v file=%q readErr=%v writes=%d commits=%d/%d revision=%d",
			unknown, err, store.details.Task, content, readErr, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}

	// A retry with the same pinned call must consume the durable receipt and not
	// repeat the live file side effect or publish another revision.
	recovered := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), recovered, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || directory.writes != 1 || fault.commitCalls != 1 ||
		base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("live Workspace receipt replay = %#v, err=%v; writes=%d commit calls=%d commits=%d revision=%d",
			result, err, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}
}

func TestCloudDSHLiveWorkspaceWritePendingReceiptStaysUnknownUntilReceiptRecovers(t *testing.T) {
	fixture, store, broker, base, current := newCloudDSHT08WorkspaceBroker(t)
	fault := &cloudDSHLiveReceiptFaultStore{
		cloudDSHT08WorkspaceStore:        base,
		commitErrorAfterApply:            errors.New("commit response was lost"),
		pendingReceiptLookupsAfterCommit: 1,
	}
	root := t.TempDir()
	directory := configureCloudDSHLiveReceiptTest(t, fixture, base, root, fault)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write-pending-receipt", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/live-pending-receipt.txt", Content: "live file already written",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial live Workspace write = %#v, err=%v; want approval", first, err)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_result_unknown" || directory.writes != 1 || fault.commitCalls != 1 ||
		base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("pending live Workspace receipt = %#v, err=%v; task=%#v writes=%d commit calls=%d commits=%d revision=%d",
			unknown, err, store.details.Task, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}

	recovered := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), recovered, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || directory.writes != 1 || fault.commitCalls != 1 ||
		base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("recovered live Workspace pending receipt = %#v, err=%v; writes=%d commit calls=%d commits=%d revision=%d",
			result, err, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}
}

func TestCloudDSHLiveWorkspaceWriteSnapshotFailureAfterWriteStaysUnknownAndReplayDoesNotRewrite(t *testing.T) {
	fixture, store, broker, base, current := newCloudDSHT08WorkspaceBroker(t)
	fault := &cloudDSHLiveReceiptFaultStore{cloudDSHT08WorkspaceStore: base}
	root := t.TempDir()
	directory := configureCloudDSHLiveReceiptTest(t, fixture, base, root, fault)
	snapshotDirectory := &cloudDSHLiveSnapshotFailDirectory{cloudDSHLiveCountingDirectory: directory, failAfterWrite: true}
	fixture.executor.workspaces.currentDirectory = snapshotDirectory
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write-snapshot-failure", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/live-snapshot-recovery.txt", Content: "written before snapshot failed",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial live Workspace write = %#v, err=%v; want approval", first, err)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), resumed, call, nil)
	content, readErr := os.ReadFile(filepath.Join(root, "reports", "live-snapshot-recovery.txt"))
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_result_unknown" || readErr != nil || string(content) != "written before snapshot failed" ||
		directory.writes != 1 || fault.commitCalls != 0 || base.commits != 0 || base.workspace.Revision != 0 {
		t.Fatalf("post-write snapshot failure = %#v, err=%v; task=%#v file=%q readErr=%v writes=%d commit calls=%d commits=%d revision=%d",
			unknown, err, store.details.Task, content, readErr, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}

	// If the next reconciliation still cannot inspect the pending live file,
	// it must remain unknown instead of returning a retryable failure to Agent.
	stillUnknownTask := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	stillUnknown, err := broker.Reconcile(context.Background(), stillUnknownTask, call, nil)
	if err != nil || stillUnknown.Status != AgentToolUnknown || directory.writes != 1 || fault.commitCalls != 0 || base.commits != 0 {
		t.Fatalf("unreadable live Workspace replay = %#v, err=%v; writes=%d commit calls=%d commits=%d",
			stillUnknown, err, directory.writes, fault.commitCalls, base.commits)
	}

	snapshotDirectory.failAfterWrite = false
	recovered := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), recovered, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || directory.writes != 1 || fault.commitCalls != 1 ||
		base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("recovered live Workspace snapshot = %#v, err=%v; writes=%d commit calls=%d commits=%d revision=%d",
			result, err, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}
}

func TestCloudDSHLiveWorkspaceWriteAdapterErrorAfterApplyStaysUnknownAndReplayDoesNotRewrite(t *testing.T) {
	fixture, store, broker, base, current := newCloudDSHT08WorkspaceBroker(t)
	fault := &cloudDSHLiveReceiptFaultStore{cloudDSHT08WorkspaceStore: base}
	root := t.TempDir()
	directory := configureCloudDSHLiveReceiptTest(t, fixture, base, root, fault)
	directory.writeErrorAfterApply = errors.New("parent directory fsync failed after rename")
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write-adapter-error-after-apply", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/live-adapter-recovery.txt", Content: "adapter reported error after atomic replacement",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial live Workspace write = %#v, err=%v; want approval", first, err)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), resumed, call, nil)
	content, readErr := os.ReadFile(filepath.Join(root, "reports", "live-adapter-recovery.txt"))
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_result_unknown" || readErr != nil ||
		string(content) != "adapter reported error after atomic replacement" || directory.writes != 1 || fault.commitCalls != 0 || base.commits != 0 {
		t.Fatalf("post-write adapter error = %#v, err=%v; task=%#v file=%q readErr=%v writes=%d commit calls=%d commits=%d",
			unknown, err, store.details.Task, content, readErr, directory.writes, fault.commitCalls, base.commits)
	}

	recovered := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), recovered, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || directory.writes != 1 || fault.commitCalls != 1 ||
		base.commits != 1 || base.workspace.Revision != 1 {
		t.Fatalf("recovered live Workspace adapter error = %#v, err=%v; writes=%d commit calls=%d commits=%d revision=%d",
			result, err, directory.writes, fault.commitCalls, base.commits, base.workspace.Revision)
	}
}

func TestCloudDSHLiveWorkspaceWriteReturnsTerminalConflictToAgent(t *testing.T) {
	fixture, store, broker, base, current := newCloudDSHT08WorkspaceBroker(t)
	fault := &cloudDSHLiveReceiptFaultStore{
		cloudDSHT08WorkspaceStore: base,
		rejectCommitAsConflict:    true,
	}
	configureCloudDSHLiveReceiptTest(t, fixture, base, t.TempDir(), fault)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "live-write-terminal-conflict", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/live-conflict.txt", Content: "approved conflict handling",
	})
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial live Workspace write = %#v, err=%v; want approval", first, err)
	}

	resumed := resumeCloudDSHT08WorkspaceApproval(store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || result.Status != AgentToolFailed || fault.commitCalls != 1 || base.commits != 0 || base.workspace.Revision != 0 ||
		store.details.Task.Status == task.StatusWaitingUser && store.details.Task.WaitingReason == "tool_result_unknown" {
		t.Fatalf("terminal live Workspace conflict = %#v, err=%v; task=%#v commit calls=%d commits=%d revision=%d",
			result, err, store.details.Task, fault.commitCalls, base.commits, base.workspace.Revision)
	}
}
