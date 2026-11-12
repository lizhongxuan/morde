package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

func TestPostgresHostCopyReservationFencesSourceUntilNewHostBinding(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 100_000, 200_000)
	sourceHost, sourceWorkspace := prepareStoppedHostCopySource(t, ctx, store, pool, owner.ID)
	reservation, record, err := createHostCopyReservation(t, ctx, store, owner.ID, sourceHost.ID, sourceWorkspace.ID, 1, 0)
	if err != nil {
		t.Fatalf("create Host copy reservation: %v", err)
	}
	if reservation.SourceHostID != sourceHost.ID || reservation.SourceBindingVersion != 1 || !reservation.HostCopyPending ||
		reservation.State != workspace.CopyReserved {
		t.Fatalf("Host copy reservation = %#v", reservation)
	}
	stored, found, err := store.GetWorkspaceCopyReservationByIdempotency(ctx, record, "Host copy", "host-copy-test", 2_048<<20)
	if err != nil || !found || stored.ID != reservation.ID || stored.SourceHostID != sourceHost.ID || !stored.HostCopyPending {
		t.Fatalf("Host copy idempotency lookup = %#v, found=%t, err=%v", stored, found, err)
	}
	if err := store.ValidateHostCopyReservationSource(ctx, owner.ID, reservation.ID); err != nil {
		t.Fatalf("validate active Host copy source fence: %v", err)
	}
	for _, state := range []string{"copying", "unknown", "copying"} {
		if _, err := pool.Exec(ctx, `UPDATE workspace_copy_reservations SET state=$2 WHERE id=$1`, reservation.ID, state); err != nil {
			t.Fatal(err)
		}
		if err := store.ValidateHostCopyReservationSource(ctx, owner.ID, reservation.ID); err != nil {
			t.Fatalf("resume Host copy in %s state: %v", state, err)
		}
	}
	if _, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, sourceWorkspace.ID, app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileRename, SourcePath: "before.txt", TargetPath: "after.txt", ExpectedCurrentRevision: 0, WriteEnabled: true,
	}, syncTestIdempotency(owner.ID, "workspace.file_operation", "host-copy-freeze", sourceWorkspace.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("source mutation while Host copy pending error = %v, want workspace busy", err)
	}
	if err := store.MarkCreateAttempt(ctx, sourceHost.ID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("source Host create attempt while copy pending error = %v, want workspace busy", err)
	}
	if _, _, err := store.BeginHostRestore(ctx, app.RestoreHostParams{
		HostID: sourceHost.ID, RuntimeCredentialEnc: []byte("fresh runtime credential"),
		ExpiresAt: time.Now().UTC().Add(time.Hour), Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
	}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("source Host restore while copy pending error = %v, want workspace busy", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, sourceWorkspace.ID)
	}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("source Workspace transfer/unbind guard while copy pending error = %v, want workspace busy", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspaces SET state='ready',revision=1 WHERE id=$1`, reservation.TargetWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes,directory_count)
VALUES ($1,1,0,0,0)`, reservation.TargetWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_current_materializations SET desired_revision=1,applied_revision=1 WHERE workspace_id=$1`, reservation.TargetWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, reservation.TargetWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_copy_reservations SET state='completed' WHERE id=$1`, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, sourceHost.ID); err != nil {
		t.Fatal(err)
	}
	completedReplay, found, err := store.GetWorkspaceCopyReservationByIdempotency(ctx, record, "Host copy", "host-copy-test", 2_048<<20)
	if err != nil || !found || completedReplay.ID != reservation.ID || completedReplay.State != workspace.CopyCompleted {
		t.Fatalf("completed Host copy replay after source restart = %#v, found=%t, err=%v", completedReplay, found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, sourceHost.ID); err != nil {
		t.Fatal(err)
	}
	ordinaryHostParams := testCreateHostParams(t, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{reservation.TargetWorkspaceID})
	if _, _, err := store.CreateHost(ctx, ordinaryHostParams); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("ordinary Host binding to pending copy target error = %v, want workspace busy", err)
	}
	copyHostParams := testCreateHostParams(t, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{reservation.TargetWorkspaceID})
	copyHostParams.WorkspaceCopyReservationID = reservation.ID
	createdHost, replayed, err := store.CreateHost(ctx, copyHostParams)
	if err != nil || replayed || createdHost.State != host.StateCreating || len(createdHost.Workspaces) != 1 ||
		createdHost.Workspaces[0].Workspace.ID != reservation.TargetWorkspaceID {
		t.Fatalf("create independent Host from copy = %#v, replayed=%t, err=%v", createdHost, replayed, err)
	}
	settled, err := store.GetWorkspaceCopyReservationByID(ctx, reservation.ID)
	if err != nil || settled.HostCopyPending || settled.State != workspace.CopyCompleted {
		t.Fatalf("Host copy reservation after Host creation = %#v, err=%v", settled, err)
	}
	unchangedSource, err := store.GetHost(ctx, sourceHost.ID)
	if err != nil || unchangedSource.State != host.StateStopped || len(unchangedSource.Workspaces) != 1 ||
		unchangedSource.Workspaces[0].Workspace.ID != sourceWorkspace.ID || unchangedSource.Workspaces[0].State != workspace.BindingActive {
		t.Fatalf("source Host after copy = %#v, err=%v", unchangedSource, err)
	}
}

func TestPostgresHostCopyReservationRequiresStoppedHostAndExactHealthyBinding(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 100_000, 200_000)
	sourceHost, sourceWorkspace := prepareStoppedHostCopySource(t, ctx, store, pool, owner.ID)
	for _, tc := range []struct {
		name             string
		bindingVersion   int64
		sourceRevision   int64
		hostState        string
		bindingSyncState string
		wantCode         domain.ErrorCode
	}{
		{name: "running Host", bindingVersion: 1, hostState: "running", wantCode: domain.CodeWorkspaceBusy},
		{name: "wrong binding version", bindingVersion: 2, wantCode: domain.CodeWorkspaceConflict},
		{name: "stale Workspace revision", bindingVersion: 1, sourceRevision: 1, wantCode: domain.CodeWorkspaceConflict},
		{name: "sync conflict", bindingVersion: 1, bindingSyncState: "conflict", wantCode: domain.CodeWorkspaceConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, sourceHost.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET sync_state='healthy',sync_conflicts='{}' WHERE host_id=$1`, sourceHost.ID); err != nil {
				t.Fatal(err)
			}
			if tc.hostState != "" {
				if _, err := pool.Exec(ctx, `UPDATE hosts SET state=$2 WHERE id=$1`, sourceHost.ID, tc.hostState); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bindingSyncState != "" {
				if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET sync_state=$2,sync_conflicts=ARRAY['file.txt']::text[] WHERE host_id=$1`,
					sourceHost.ID, tc.bindingSyncState); err != nil {
					t.Fatal(err)
				}
			}
			revision := tc.sourceRevision
			if revision == 0 {
				revision = sourceWorkspace.Revision
			}
			_, _, err := createHostCopyReservation(t, ctx, store, owner.ID, sourceHost.ID, sourceWorkspace.ID, tc.bindingVersion, revision)
			if domain.CodeOf(err) != tc.wantCode {
				t.Fatalf("Host copy reservation error = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

func prepareStoppedHostCopySource(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, ownerID string) (host.Host, workspace.Workspace) {
	t.Helper()
	value := createTestWorkspace(t, ctx, store, ownerID, "host-copy-source")
	created := createTestHost(t, ctx, store, ownerID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
	if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='active',desired_revision=0,applied_revision=0,sync_state='healthy',sync_conflicts='{}'
WHERE host_id=$1 AND workspace_id=$2`, created.ID, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='stopped',sandbox_id=NULL,runtime_instance_id=NULL,control_state='ai' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	return created, value
}

func createHostCopyReservation(t *testing.T, ctx context.Context, store *Store, ownerID, sourceHostID, sourceWorkspaceID string,
	bindingVersion, sourceRevision int64) (workspace.CopyReservation, app.IdempotencyRecord, error) {
	t.Helper()
	targetWorkspaceID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	key := "host-copy-" + reservationID
	hash := sha256.Sum256([]byte(sourceHostID + ":" + sourceWorkspaceID + ":" + key))
	record := app.IdempotencyRecord{OwnerID: ownerID, Operation: "workspace.computer.copy", Key: key,
		RequestHash: hash[:], ResourceID: sourceHostID}
	reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, app.WorkspaceCopyReservationRequest{
		ID: reservationID, SourceOwnerID: ownerID, TargetOwnerID: ownerID, SourceHostID: sourceHostID,
		SourceBindingVersion: bindingVersion, SourceWorkspaceID: sourceWorkspaceID, TargetWorkspaceID: targetWorkspaceID,
		TargetName: "Host copy", ServiceInstanceID: "host-copy-test", SourceRevision: sourceRevision,
		MaxOwnerBytes: 1 << 40, TargetQuotaBytes: 2_048 << 20, Idempotency: record,
	})
	if err == nil && replayed {
		t.Fatal("new Host copy reservation was replayed")
	}
	return reservation, record, err
}
