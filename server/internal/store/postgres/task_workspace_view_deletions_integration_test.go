package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresTaskWorkspaceViewDeletionWaitsForCurrentHostReceipt(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "task-view-delete")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "task-view-delete", "write a workspace file")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace manifest")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	requestID := mustID(t, domain.NewTaskWorkspaceViewDeletionID)
	idempotency := testIdempotency(owner.ID, "task.workspace_view.delete", "delete-view-01")
	idempotency.ResourceID, idempotency.RelatedID = requestID, created.Task.ID
	if _, err := store.RequestTaskWorkspaceViewDeletion(ctx, idempotency, created.Task.ID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("active Task deletion error = %v, want invalid state", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status=$2,finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`,
		created.Task.ID, string(task.StatusCompleted)); err != nil {
		t.Fatal(err)
	}
	deletion, err := store.RequestTaskWorkspaceViewDeletion(ctx, idempotency, created.Task.ID)
	if err != nil || deletion.Status != "pending" || deletion.ID != requestID || deletion.TaskID != created.Task.ID {
		t.Fatalf("request deletion = %#v, err=%v", deletion, err)
	}
	replayed, err := store.RequestTaskWorkspaceViewDeletion(ctx, idempotency, created.Task.ID)
	if err != nil || replayed.ID != deletion.ID || replayed.Status != "pending" {
		t.Fatalf("replay deletion = %#v, err=%v", replayed, err)
	}
	if _, _, err := store.BeginHostRelease(ctx, hostValue.ID, false); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("release with pending deletion error = %v, want conflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state=$2 WHERE id=$1`, hostValue.ID, string(host.StateStopped)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListPendingTaskWorkspaceViewDeletions(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration)
	if err != nil || len(items) != 0 {
		t.Fatalf("offline Host deletion poll = %#v, err=%v; want no claim", items, err)
	}
	state, err := store.GetTaskWorkspaceViewDeletion(ctx, owner.ID, created.Task.ID)
	if err != nil || state.Status != "pending" {
		t.Fatalf("offline deletion state = %#v, err=%v", state, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state=$2 WHERE id=$1`, hostValue.ID, string(host.StateRunning)); err != nil {
		t.Fatal(err)
	}
	items, err = store.ListPendingTaskWorkspaceViewDeletions(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration)
	if err != nil || len(items) != 1 || items[0].ID != requestID {
		t.Fatalf("online Host deletion poll = %#v, err=%v", items, err)
	}
	if _, err := store.CompleteTaskWorkspaceViewDeletion(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration+1, created.Task.ID, requestID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale generation acknowledgement error = %v, want conflict", err)
	}
	completed, err := store.CompleteTaskWorkspaceViewDeletion(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration, created.Task.ID, requestID)
	if err != nil || completed.Status != "applied" || completed.ID != requestID {
		t.Fatalf("complete deletion = %#v, err=%v", completed, err)
	}
}
