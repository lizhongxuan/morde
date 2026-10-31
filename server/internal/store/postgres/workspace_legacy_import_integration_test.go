package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceLegacyImportSessionLockIsExclusivePerWorkspace(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	firstWorkspace := createPendingLegacyWorkspace(t, ctx, store, "lock-first")
	secondWorkspace := createPendingLegacyWorkspace(t, ctx, store, "lock-second")

	first, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, firstWorkspace.WorkspaceID)
	if err != nil || !acquired {
		t.Fatalf("acquire first import lock = (%v, %v)", acquired, err)
	}
	second, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, secondWorkspace.WorkspaceID)
	if err != nil || !acquired {
		t.Fatalf("acquire independent import lock = (%v, %v)", acquired, err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("release independent import lock: %v", err)
	}
	if duplicate, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, firstWorkspace.WorkspaceID); err != nil || acquired || duplicate != nil {
		t.Fatalf("duplicate import lock = (%v, %v), want busy", acquired, err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("release first import lock: %v", err)
	}
	if duplicate, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, firstWorkspace.WorkspaceID); err != nil || !acquired {
		t.Fatalf("reacquire released import lock = (%v, %v)", acquired, err)
	} else if err := duplicate.Release(ctx); err != nil {
		t.Fatalf("release reacquired import lock: %v", err)
	}
}

func TestWorkspaceLegacyImportCommitsFilesAndEmptyVolume(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()

	emptySnapshot := createPendingLegacyWorkspace(t, ctx, store, "empty-import")
	empty, err := commitWorkspaceLegacyVolumeImport(ctx, store, emptySnapshot, []workspace.File{})
	if err != nil || empty.Revision != 0 || empty.UsedBytes != 0 {
		t.Fatalf("import empty legacy volume = %#v, err=%v", empty, err)
	}
	assertLegacyImportState(t, ctx, store, emptySnapshot.WorkspaceID, "imported")
	if _, err := store.GetPendingWorkspaceLegacyVolume(ctx, emptySnapshot.WorkspaceID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("get imported legacy volume error = %v, want not found", err)
	}

	snapshot := createPendingLegacyWorkspace(t, ctx, store, "file-import")
	files := []workspace.File{
		syncTestFile("src/main.go", []byte("package main\n")),
		syncTestFile("empty.txt", nil),
	}
	imported, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, files)
	if err != nil || imported.Revision != 1 || imported.UsedBytes != files[0].SizeBytes {
		t.Fatalf("import legacy files = %#v, err=%v", imported, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, snapshot.WorkspaceID, 1, "", "", 100)
	if err != nil || len(page.Files) != 2 {
		t.Fatalf("imported manifest = %#v, err=%v", page, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, snapshot.WorkspaceID, 0, "", 100)
	if err != nil || len(changes.Items) != 2 {
		t.Fatalf("imported changes = %#v, err=%v", changes, err)
	}
	assertLegacyImportState(t, ctx, store, snapshot.WorkspaceID, "imported")

	directorySnapshot := createPendingLegacyWorkspace(t, ctx, store, "directory-import")
	directoryImport, err := commitWorkspaceLegacyVolumeImportWithDirectories(ctx, store, directorySnapshot, nil, []string{"empty/nested"})
	if err != nil || directoryImport.Revision != 1 {
		t.Fatalf("import legacy empty directory = %#v, err=%v", directoryImport, err)
	}
	var directoryPath string
	if err := store.pool.QueryRow(ctx, `SELECT directory_path FROM workspace_revision_empty_directories WHERE workspace_id=$1 AND valid_from_revision=1`, directorySnapshot.WorkspaceID).Scan(&directoryPath); err != nil || directoryPath != "empty/nested" {
		t.Fatalf("imported empty directory = %q, err=%v", directoryPath, err)
	}

	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, files); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("repeat legacy import error = %v, want workspace conflict", err)
	}
	unchanged, err := store.GetWorkspace(ctx, snapshot.WorkspaceID)
	if err != nil || unchanged.Revision != 1 || unchanged.UsedBytes != files[0].SizeBytes {
		t.Fatalf("workspace after repeated import = %#v, err=%v", unchanged, err)
	}
}

func TestWorkspaceLegacyImportEnforcesOwnerQuota(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	used := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-used")
	usedFile := syncTestFile("used.txt", []byte("sixsix"))
	usedSession := newTestSyncSession(t, used, owner.ID, []workspace.File{usedFile}, []workspace.File{usedFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, usedSession, syncTestBlocks(usedSession.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "legacy-owner-quota:"+usedSession.ID, usedSession.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, used.ID, usedSession.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "legacy-owner-quota:"+usedSession.ID, usedSession.ID)); err != nil {
		t.Fatal(err)
	}
	legacyWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-import")
	volumeName := "legacy-" + legacyWorkspace.ID
	if _, err := store.pool.Exec(ctx, `
INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state)
VALUES ($1, $2, 'pending')`, legacyWorkspace.ID, volumeName); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPendingWorkspaceLegacyVolume(ctx, legacyWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	files := []workspace.File{syncTestFile("imported.txt", []byte("12345"))}
	if err := store.ReserveWorkspaceBlockUsage(ctx, legacyWorkspace.ID, []app.WorkspaceBlockReservation{{
		WorkspaceID: legacyWorkspace.ID, Digest: files[0].Chunks[0].SHA256, ReservedBytes: files[0].SizeBytes,
	}}, 10); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("legacy import reservation beyond owner quota error = %v, want quota exceeded", err)
	}
	assertLegacyImportState(t, ctx, store, legacyWorkspace.ID, "pending")
}

func TestWorkspaceLegacyImportConcurrentCommitHasOneWinner(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "concurrent-import")
	files := []workspace.File{syncTestFile("file.txt", []byte("content"))}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, files)
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errs)

	succeeded, conflicted := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			conflicted++
		} else {
			t.Errorf("concurrent import error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent import outcomes: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	imported, err := store.GetWorkspace(ctx, snapshot.WorkspaceID)
	if err != nil || imported.Revision != 1 || imported.UsedBytes != files[0].SizeBytes {
		t.Fatalf("workspace after concurrent import = %#v, err=%v", imported, err)
	}
}

func TestPendingLegacyImportBlockGCReleasesAbandonedReservations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "pending-import-gc-reservations")
	reservations := []app.WorkspaceBlockReservation{
		{WorkspaceID: snapshot.WorkspaceID, Digest: strings.Repeat("a", 64), ReservedBytes: 11},
		{WorkspaceID: snapshot.WorkspaceID, Digest: strings.Repeat("b", 64), ReservedBytes: 13},
	}
	if err := store.ReserveWorkspaceBlockUsage(ctx, snapshot.WorkspaceID, reservations, 20_000); err != nil {
		t.Fatal(err)
	}
	workspaceValue, err := store.GetWorkspace(ctx, snapshot.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := store.WithPendingLegacyImportWorkspaceBlockGC(ctx, snapshot.WorkspaceID, func(references []workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error) {
		called = true
		if len(references) != 0 {
			t.Fatalf("initial pending Workspace protected references = %#v", references)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var rows, reservedBytes, ownerReserved int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(reserved_bytes),0) FROM workspace_block_usage WHERE workspace_id=$1`, snapshot.WorkspaceID).Scan(&rows, &reservedBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, workspaceValue.OwnerID).Scan(&ownerReserved); err != nil {
		t.Fatal(err)
	}
	if !called || rows != 0 || reservedBytes != 0 || ownerReserved != 0 {
		t.Fatalf("recovered importer reservations: callback=%t rows=%d workspace_reserved=%d owner_reserved=%d", called, rows, reservedBytes, ownerReserved)
	}
}

func TestPendingLegacyImportBlockGCFailsClosedWithActiveSync(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "pending-import-gc-active-sync")
	value, err := store.GetWorkspace(ctx, snapshot.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	session := newTestSyncSession(t, value, value.OwnerID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, []workspace.TransferBlock{},
		syncTestIdempotency(value.OwnerID, "workspace.sync.create", "legacy-gc-active:"+session.ID, session.ID)); err != nil {
		t.Fatal(err)
	}
	called := false
	err = store.WithPendingLegacyImportWorkspaceBlockGC(ctx, snapshot.WorkspaceID, func([]workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error) {
		called = true
		return nil, nil
	})
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy || called {
		t.Fatalf("cleanup with active sync = %v callback=%t; want busy without entering callback", err, called)
	}
}

func TestWorkspaceLegacyImportRejectsActiveSyncAndWorkspaceStateConflict(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()

	owner := createTestUser(t, store, 4, 10_000, 20_000)
	activeWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "active-sync-import")
	activeSession := newTestSyncSession(t, activeWorkspace, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, activeSession, []workspace.TransferBlock{}, syncTestIdempotency(owner.ID, "workspace.sync.create", "legacy-active:"+activeSession.ID, activeSession.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`, activeWorkspace.ID, "legacy-"+activeWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	activeSnapshot, err := store.GetPendingWorkspaceLegacyVolume(ctx, activeWorkspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspaceLegacyImportReady(ctx, activeSnapshot.WorkspaceID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("legacy import preflight with active sync = %v, want workspace busy", err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, activeSnapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("import with active sync error = %v, want workspace busy", err)
	}
	assertLegacyImportState(t, ctx, store, activeSnapshot.WorkspaceID, "pending")

	pendingSnapshot := createPendingLegacyWorkspace(t, ctx, store, "sync-session-blocked-during-import")
	pendingWorkspace, err := store.GetWorkspace(ctx, pendingSnapshot.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	pendingSession := newTestSyncSession(t, pendingWorkspace, pendingWorkspace.OwnerID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, pendingSession, []workspace.TransferBlock{}, syncTestIdempotency(pendingWorkspace.OwnerID, "workspace.sync.create", "legacy-pending:"+pendingSession.ID, pendingSession.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("create sync session during pending legacy import = %v, want workspace busy", err)
	}

	stateSnapshot := createPendingLegacyWorkspace(t, ctx, store, "state-conflict-import")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state = 'deleting' WHERE id = $1`, stateSnapshot.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, stateSnapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("import while workspace is deleting error = %v, want workspace busy", err)
	}
	assertLegacyImportState(t, ctx, store, stateSnapshot.WorkspaceID, "pending")
}

func TestWorkspaceLegacyImportWaitsForTaskOperationAdmission(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "legacy-import-task-admission")
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`,
		workspaceValue.ID, "legacy-"+workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_legacy_import_admission' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "legacy-import-admission", "read the Workspace")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("legacy import admission manifest")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Workspace task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "legacy-import-admission-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Workspace task: %#v, found=%t, err=%v", claimed, found, err)
	}

	admissionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admissionTx.Rollback(context.Background()) }()
	if err := admissionTx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR SHARE`, workspaceValue.ID).Scan(new(string)); err != nil {
		t.Fatalf("lock Workspace for Task admission: %v", err)
	}
	if _, err := admissionTx.Exec(ctx, `INSERT INTO task_operations (
	id, task_id, operation_key, request_hash, host_id, runtime_generation, control_version, lease_version,
	status, workspace_id, binding_version, input_revision, workspace_checkpoint_revision
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$10,$11,$11)`,
		mustID(t, domain.NewOperationID), created.Task.ID, "legacy-import-admitted-operation", hashBytes("admitted operation"),
		hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, claimed.Lease.Version,
		workspaceValue.ID, binding.BindingVersion, revision); err != nil {
		t.Fatalf("insert admitted Task operation: %v", err)
	}

	importReady := make(chan error, 1)
	go func() { importReady <- store.EnsureWorkspaceLegacyImportReady(ctx, workspaceValue.ID) }()
	waitForPostgresLockWait(t, ctx, pool, "%FOR UPDATE OF w, v%", 1)
	if err := admissionTx.Commit(ctx); err != nil {
		t.Fatalf("commit Task admission: %v", err)
	}
	select {
	case err := <-importReady:
		if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
			t.Fatalf("legacy import preflight after Task admission = %v; want workspace busy", err)
		}
	case <-ctx.Done():
		t.Fatalf("legacy import preflight did not resume after Task admission committed: %v", ctx.Err())
	}
}

func TestWorkspaceLegacyImportSerializesDirectReceive(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()

	owner := createTestUser(t, store, 3, 10_000, 20_000)
	pendingWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "pending-import-rejects-receive")
	authorizeTestWorkspaceSyncReceive(t, ctx, store, owner.ID, pendingWorkspace.ID)
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, pendingWorkspace.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`, pendingWorkspace.ID, "legacy-"+pendingWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	request := receiveTestRequest(owner.ID, pendingWorkspace.ID, "from-phone.txt", 1, "pending-import")
	if _, err := reserveTestWorkspaceReceiveErr(t, ctx, store, request); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve direct receive during pending legacy import = %v; want workspace busy", err)
	}

	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "receive-before-pending-import")
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, workspaceValue.ID, 0); err != nil {
		t.Fatal(err)
	}
	activeRequest := receiveTestRequest(owner.ID, workspaceValue.ID, "from-phone.txt", 1, "receive-before-import")
	if reservation := reserveTestWorkspaceReceive(t, ctx, store, activeRequest); reservation.State != "reserved" {
		t.Fatalf("reserve direct receive before pending import = %#v", reservation)
	}
	volumeName := "legacy-" + workspaceValue.ID
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`, workspaceValue.ID, volumeName); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetPendingWorkspaceLegacyVolume(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspaceLegacyImportReady(ctx, workspaceValue.ID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("legacy import preflight during active direct receive = %v; want workspace busy", err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit legacy import during active direct receive = %v; want workspace busy", err)
	}
}

func TestWorkspaceLegacyImportQuotaFailureAndTransactionRollback(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()

	quotaSnapshot := createPendingLegacyWorkspace(t, ctx, store, "quota-import")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET quota_bytes = 1 WHERE id = $1`, quotaSnapshot.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	quotaSnapshot, err := store.GetPendingWorkspaceLegacyVolume(ctx, quotaSnapshot.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, quotaSnapshot, []workspace.File{syncTestFile("large.txt", []byte("too large"))}); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota import error = %v, want quota exceeded", err)
	}
	assertWorkspaceStillPending(t, ctx, store, quotaSnapshot.WorkspaceID)

	rollbackSnapshot := createPendingLegacyWorkspace(t, ctx, store, "rollback-import")
	_, err = store.pool.Exec(ctx, `
CREATE FUNCTION reject_legacy_import_for_test() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
  IF NEW.import_state = 'imported' THEN
    RAISE EXCEPTION 'forced integration test rollback';
  END IF;
  RETURN NEW;
END
$function$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `CREATE TRIGGER reject_legacy_import_for_test
BEFORE UPDATE OF import_state ON workspace_legacy_volumes
FOR EACH ROW EXECUTE FUNCTION reject_legacy_import_for_test()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_legacy_import_for_test ON workspace_legacy_volumes`)
		_, _ = store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_legacy_import_for_test()`)
	})

	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, rollbackSnapshot, []workspace.File{syncTestFile("rollback.txt", []byte("content"))}); err == nil {
		t.Fatal("import with injected final-state failure unexpectedly succeeded")
	}
	assertWorkspaceStillPending(t, ctx, store, rollbackSnapshot.WorkspaceID)
	var changes int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_changes WHERE workspace_id = $1`, rollbackSnapshot.WorkspaceID).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("workspace changes after rollback = %d, want 0", changes)
	}
}

func createPendingLegacyWorkspace(t *testing.T, ctx context.Context, store *Store, name string) app.LegacyWorkspaceVolume {
	t.Helper()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, name)
	volumeName := "legacy-" + value.ID
	if _, err := store.pool.Exec(ctx, `
INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state)
VALUES ($1, $2, 'pending')`, value.ID, volumeName); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetPendingWorkspaceLegacyVolume(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertLegacyImportState(t *testing.T, ctx context.Context, store *Store, workspaceID, expected string) {
	t.Helper()
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT import_state FROM workspace_legacy_volumes WHERE workspace_id = $1`, workspaceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != expected {
		t.Fatalf("legacy import state = %q, want %q", state, expected)
	}
}

func commitWorkspaceLegacyVolumeImport(ctx context.Context, store *Store, value app.LegacyWorkspaceVolume, files []workspace.File) (workspace.Workspace, error) {
	return commitWorkspaceLegacyVolumeImportWithDirectories(ctx, store, value, files, nil)
}

func commitWorkspaceLegacyVolumeImportWithDirectories(ctx context.Context, store *Store, value app.LegacyWorkspaceVolume, files []workspace.File, emptyDirectories []string) (workspace.Workspace, error) {
	if _, err := validateLegacyWorkspaceImport(value, files, emptyDirectories); err != nil {
		return workspace.Workspace{}, err
	}
	seen := make(map[string]struct{})
	for _, file := range files {
		for _, chunk := range file.Chunks {
			if _, ok := seen[chunk.SHA256]; ok {
				continue
			}
			if err := seedTestStoredWorkspaceBlockUsage(ctx, store, value.WorkspaceID, chunk); err != nil {
				return workspace.Workspace{}, err
			}
			seen[chunk.SHA256] = struct{}{}
		}
	}
	return store.CommitWorkspaceLegacyVolumeImport(ctx, value, files, emptyDirectories)
}

func assertWorkspaceStillPending(t *testing.T, ctx context.Context, store *Store, workspaceID string) {
	t.Helper()
	assertLegacyImportState(t, ctx, store, workspaceID, "pending")
	value, err := store.GetWorkspace(ctx, workspaceID)
	if err != nil || value.Revision != 0 || value.UsedBytes != 0 {
		t.Fatalf("workspace after rejected import = %#v, err=%v", value, err)
	}
}

func TestListPendingWorkspaceLegacyVolumes(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	first := createPendingLegacyWorkspace(t, ctx, store, "pending-first")
	second := createPendingLegacyWorkspace(t, ctx, store, "pending-second")
	deleting := createPendingLegacyWorkspace(t, ctx, store, "pending-deleting")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state = 'deleting' WHERE id = $1`, deleting.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, first, []workspace.File{}); err != nil {
		t.Fatal(err)
	}
	values, err := store.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil || len(values) != 1 || values[0].WorkspaceID != second.WorkspaceID || values[0].VolumeName != second.VolumeName {
		t.Fatalf("pending legacy volumes = %#v, err=%v", values, err)
	}
	if _, err := store.GetPendingWorkspaceLegacyVolume(ctx, deleting.WorkspaceID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("get pending legacy volume for deleting workspace error = %v, want not found", err)
	}
}

func TestMarkLegacyWorkspaceNeedsCleanupPersistsUntilImport(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "cleanup-required")
	reason := `legacy workspace file "large.bin" exceeds the 300 MiB file limit; cleanup is required`
	if err := store.MarkWorkspaceLegacyVolumeNeedsCleanup(ctx, snapshot.WorkspaceID, reason); err != nil {
		t.Fatal(err)
	}
	values, err := store.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil || len(values) != 1 || values[0].ImportError != reason {
		t.Fatalf("cleanup-required pending volume = %#v, err=%v", values, err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, []workspace.File{}); err != nil {
		t.Fatal(err)
	}
	values, err = store.ListPendingWorkspaceLegacyVolumes(ctx)
	if err != nil || len(values) != 0 {
		t.Fatalf("pending volumes after import = %#v, err=%v", values, err)
	}
}

func TestWorkspaceLegacyImportRejectsCaseFoldDuplicatePaths(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "casefold-import")
	files := []workspace.File{
		syncTestFile("Readme.md", []byte("one")),
		syncTestFile("README.md", []byte("two")),
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, files); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("case-fold duplicate import error = %v, want invalid argument", err)
	}
	assertWorkspaceStillPending(t, ctx, store, snapshot.WorkspaceID)
}

func TestValidateLegacyWorkspaceImportEnforcesFileCountLimit(t *testing.T) {
	files := make([]workspace.File, workspace.MaxSyncFiles+1)
	_, err := validateLegacyWorkspaceImport(app.LegacyWorkspaceVolume{
		WorkspaceID: "w123abcd", VolumeName: "legacy-w123abcd", QuotaBytes: 1,
	}, files, nil)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("file count limit error = %v, want quota exceeded", err)
	}
}

func TestValidateLegacyWorkspaceImportEnforcesPerFileLimit(t *testing.T) {
	remaining := workspace.MaxSyncFileBytes + 1
	file := workspace.File{Path: "large.bin", SizeBytes: remaining, SHA256: strings.Repeat("a", 64), Mode: 0644}
	for remaining > 0 {
		size := min(remaining, workspace.MaxSyncBlockBytes)
		file.Chunks = append(file.Chunks, workspace.Chunk{SHA256: strings.Repeat("b", 64), SizeBytes: size})
		remaining -= size
	}
	_, err := validateLegacyWorkspaceImport(app.LegacyWorkspaceVolume{
		WorkspaceID: "w123abcd", VolumeName: "legacy-w123abcd", QuotaBytes: workspace.MaxSyncFileBytes + 1,
	}, []workspace.File{file}, nil)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("single-file limit error = %v, want quota exceeded", err)
	}
}

func TestWorkspaceLegacyImportSnapshotConflict(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	snapshot := createPendingLegacyWorkspace(t, ctx, store, "snapshot-import")
	snapshot.QuotaBytes--
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, snapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale quota snapshot error = %v, want workspace conflict", err)
	}
	assertWorkspaceStillPending(t, ctx, store, snapshot.WorkspaceID)
}
