package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceCopyReservationFreezesWorkspaceMutation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "copy-freeze")
	insertWorkspaceCopyReservationForTest(t, ctx, store, owner.ID, value.ID, "w7654321")

	if _, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileRename, SourcePath: "before.txt", TargetPath: "after.txt", ExpectedCurrentRevision: 0, WriteEnabled: true,
	}, syncTestIdempotency(owner.ID, "workspace.file_operation", "copy-freeze", value.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("file mutation during Workspace copy error = %v, want workspace busy", err)
	}
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, host.ControlAI, 0)
	if _, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: value, Directory: value.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: value.Revision,
	}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("binding during Workspace copy error = %v, want workspace busy", err)
	}
}

func TestPostgresWorkspaceCopyReservationBlocksBoundHostStart(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "copy-host-freeze")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
	insertWorkspaceCopyReservationForTest(t, ctx, store, owner.ID, value.ID, "w7654321")

	if err := store.MarkCreateAttempt(ctx, hostValue.ID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("Host create attempt during Workspace copy error = %v, want workspace busy", err)
	}
}

func TestPostgresWorkspaceCopyReservationChecksIdentityAndReplays(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "copy-reservation")
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("copy reservation:" + value.ID))
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.copy", Key: "copy:" + value.ID,
		RequestHash: hash[:], ResourceID: value.ID,
	}
	reservationRequest := app.WorkspaceCopyReservationRequest{
		ID: "rabc1234", SourceOwnerID: owner.ID, TargetOwnerID: owner.ID, SourceWorkspaceID: value.ID,
		TargetWorkspaceID: "w7654321", TargetName: "copy target", ServiceInstanceID: "test-instance",
		SourceRevision: 0, ReservedBytes: 32, MaxOwnerBytes: 1 << 40, TargetQuotaBytes: 1 << 30, Idempotency: record,
	}
	reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, reservationRequest)
	if err != nil || replayed || reservation.State != workspace.CopyReserved || reservation.SourceRevision != value.Revision {
		t.Fatalf("create Workspace copy reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
	}
	request := app.WorkspaceVolumeCopyRequest{
		ReservationID: reservation.ID, ServiceInstanceID: reservation.ServiceInstanceID, SourceOwnerID: owner.ID, TargetOwnerID: owner.ID,
		SourceWorkspaceID: value.ID, TargetWorkspaceID: reservation.TargetWorkspaceID,
	}
	if err := store.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		t.Fatalf("check active copy reservation: %v", err)
	}
	if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyReserved, workspace.Copying); err != nil {
		t.Fatalf("mark copy started: %v", err)
	}
	reservationRequest.TargetWorkspaceID = reservation.TargetWorkspaceID
	reservationRequest.ID = reservation.ID
	replayedReservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, reservationRequest)
	if err != nil || !replayed || replayedReservation.State != workspace.Copying {
		t.Fatalf("replay Workspace copy reservation = %#v, replayed=%t, err=%v", replayedReservation, replayed, err)
	}
	if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.Copying, workspace.CopyUnknown); err != nil {
		t.Fatalf("mark copy outcome unknown: %v", err)
	}
	if err := store.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		t.Fatalf("unknown copy reservation must continue fencing storage: %v", err)
	}
	copyManifest, found, err := store.GetWorkspaceCopyReservationByIdempotency(ctx, record, "copy target", "test-instance", 1<<30)
	if err != nil || !found || copyManifest.ID != reservation.ID || copyManifest.State != workspace.CopyUnknown {
		t.Fatalf("lookup by active copy idempotency = %#v, found=%t, err=%v", copyManifest, found, err)
	}
	page, err := store.GetWorkspaceManifestPageForCopy(ctx, owner.ID, reservation.ID, reservation.SourceRevision, "", "", 100)
	if err != nil || page.WorkspaceID != value.ID || page.Revision != value.Revision || len(page.Files) != 0 {
		t.Fatalf("read checkpoint through active copy reservation = %#v, err=%v", page, err)
	}
	otherOwner := createTestUser(t, store, 4, 10_000, 20_000)
	transferID, err := workspace.NewOwnershipTransferID()
	if err != nil {
		t.Fatal(err)
	}
	transferHash := sha256.Sum256([]byte("transfer during copy:" + value.ID))
	_, _, err = store.CreateWorkspaceOwnershipTransfer(ctx, owner.ID, value.ID, otherOwner.ID, value.Revision, transferID, app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.transfer.create", Key: "transfer-during-copy:" + value.ID,
		RequestHash: transferHash[:], ResourceID: value.ID,
	})
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("ownership transfer during Workspace copy error = %v, want workspace busy", err)
	}
}

func TestPostgresWorkspaceCopyLockSerializesVolumeCapacityChecks(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	first, acquired, err := store.TryWorkspaceCopyLock(ctx, "rabc1234")
	if err != nil || !acquired {
		t.Fatalf("acquire first Workspace copy lock = %t, %v", acquired, err)
	}
	second, acquired, err := store.TryWorkspaceCopyLock(ctx, "rdef5678")
	if err != nil || acquired || second != nil {
		t.Fatalf("acquire concurrent Workspace copy capacity lock = %t, %v, %#v; want unavailable", acquired, err, second)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("release first Workspace copy lock: %v", err)
	}
	second, acquired, err = store.TryWorkspaceCopyLock(ctx, "rdef5678")
	if err != nil || !acquired {
		t.Fatalf("acquire Workspace copy lock after release = %t, %v", acquired, err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("release second Workspace copy lock: %v", err)
	}
}

func TestWorkspaceCopyReservationMigrationDownPreservesRecordedData(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "copy-down-guard")
	insertWorkspaceCopyReservationForTest(t, ctx, store, owner.ID, value.ID, "w7654321")
	removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
	removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, "w7654321")

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 115); err == nil || !strings.Contains(err.Error(), "cannot roll back Workspace copy reservations") {
		t.Fatalf("copy reservation migration rollback error = %v", err)
	}
	var reservationsExist bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workspace_copy_reservations') IS NOT NULL`).Scan(&reservationsExist); err != nil || !reservationsExist {
		t.Fatalf("copy reservations table after rejected rollback = %t, err=%v", reservationsExist, err)
	}
}

func TestWorkspaceFileModificationTimeMigrationDownPreservesRecordedData(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "mtime-down-guard")
	removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks,mod_time_ns)
VALUES ($1,0,'notes.txt','notes.txt',0,$2,384,'[]'::jsonb,123)`, value.ID, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 114); err == nil || !strings.Contains(err.Error(), "cannot roll back Workspace file modification times") {
		t.Fatalf("file modification time migration rollback error = %v", err)
	}
	var modifiedAt int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(mod_time_ns,-1) FROM workspace_revision_files WHERE workspace_id=$1 AND file_path='notes.txt'`, value.ID).Scan(&modifiedAt); err != nil || modifiedAt != 123 {
		t.Fatalf("file modification time after rejected rollback = %v, err=%v", modifiedAt, err)
	}
}

func insertWorkspaceCopyReservationForTest(t *testing.T, ctx context.Context, store *Store, ownerID, sourceWorkspaceID, targetWorkspaceID string) {
	t.Helper()
	targetHash := sha256.Sum256([]byte("copy target:" + targetWorkspaceID))
	if _, _, err := store.CreateWorkspace(ctx, workspace.Workspace{ID: targetWorkspaceID, OwnerID: ownerID, Name: "copy target",
		State: workspace.StateReady, QuotaBytes: 1 << 30}, app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "workspace.create", Key: "copy-target:" + targetWorkspaceID,
		RequestHash: targetHash[:], ResourceID: targetWorkspaceID,
	}); err != nil {
		t.Fatalf("create test Workspace copy target: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='copying' WHERE id=$1`, targetWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_copy_reservations
(id,owner_id,source_owner_id,target_owner_id,source_workspace_id,target_workspace_id,service_instance_id,source_revision,read_only,state)
VALUES ($1,$2,$2,$2,$3,$4,'test-instance',0,false,'copying')`, "rabc1234", ownerID, sourceWorkspaceID, targetWorkspaceID); err != nil {
		t.Fatalf("insert Workspace copy reservation: %v", err)
	}
}
