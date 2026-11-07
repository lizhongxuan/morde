package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestTaskOperationWorkspaceAdmissionMigrationBackfillsTaskInputRevision(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "operation-revision-migration")
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "operation-revision-migration", "read the pinned Workspace")
	submission.Task.WorkspaceID, submission.Task.InputRevision = workspaceValue.ID, &revision
	submission.Task.ContextDigest = hashBytes("operation revision migration manifest")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "migration-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Workspace task: claimed=%t task=%s err=%v", found, claimed.ID, err)
	}
	// This schema downgrade fixture has no provisioned data volumes.
	if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='legacy',volume_name=NULL,ready_at=NULL WHERE state <> 'legacy'`); err != nil {
		t.Fatalf("prepare legacy Workspace volume fixture: %v", err)
	}
	// This migration backfills sync versions for every existing account object.
	// The downgrade guard requires an empty sync ledger before returning to the
	// pre-sync schema used by this fixture.
	if _, err := pool.Exec(ctx, `
DELETE FROM personal_mutation_receipts;
DELETE FROM personal_objects;
DELETE FROM account_change_log;
DELETE FROM account_object_versions;
DELETE FROM account_change_counters;`); err != nil {
		t.Fatalf("prepare legacy account sync fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_revision_directories WHERE directory_path='.' AND valid_from_revision=0`); err != nil {
		t.Fatalf("prepare legacy Workspace directory fixture: %v", err)
	}
	if _, err := provider.DownTo(ctx, 71); err != nil {
		t.Fatalf("prepare pre-admission schema: %v", err)
	}
	operationID := mustID(t, domain.NewOperationID)
	if _, err := pool.Exec(ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, workspace_id, binding_version, runtime_generation,
 runtime_instance_id, control_version, lease_version, runtime_lease_version, status)
VALUES ($1,$2,'file.read:migrated',$3,$4,$5,$6,$7,'migration-runtime',$8,$9,$9,'succeeded')`,
		operationID, claimed.ID, hashBytes("legacy Workspace operation"), hostValue.ID, workspaceValue.ID,
		binding.BindingVersion, hostValue.RuntimeGeneration, hostValue.ControlVersion, claimed.Lease.Version); err != nil {
		t.Fatalf("insert pre-migration operation: %v", err)
	}
	if _, err := provider.UpTo(ctx, 72); err != nil {
		t.Fatalf("apply Workspace operation admission migration: %v", err)
	}
	var migratedRevision int64
	if err := pool.QueryRow(ctx, `SELECT input_revision FROM task_operations WHERE id=$1`, operationID).Scan(&migratedRevision); err != nil || migratedRevision != revision {
		t.Fatalf("migrated operation input revision = %d, want %d; err=%v", migratedRevision, revision, err)
	}
}
