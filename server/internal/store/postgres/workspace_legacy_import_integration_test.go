package postgres

import (
	"context"
	"sync"
	"testing"

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

func TestWorkspaceLegacyImportRejectsActiveSyncAndWorkspaceStateConflict(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()

	activeSnapshot := createPendingLegacyWorkspace(t, ctx, store, "active-sync-import")
	owner, err := store.GetWorkspace(ctx, activeSnapshot.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	activeSession := newTestSyncSession(t, owner, owner.OwnerID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, activeSession, []workspace.TransferBlock{}, syncTestIdempotency(owner.OwnerID, "workspace.sync.create", "legacy-active:"+activeSession.ID, activeSession.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, activeSnapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("import with active sync error = %v, want workspace busy", err)
	}
	assertLegacyImportState(t, ctx, store, activeSnapshot.WorkspaceID, "pending")

	stateSnapshot := createPendingLegacyWorkspace(t, ctx, store, "state-conflict-import")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state = 'deleting' WHERE id = $1`, stateSnapshot.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := commitWorkspaceLegacyVolumeImport(ctx, store, stateSnapshot, []workspace.File{}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("import while workspace is deleting error = %v, want workspace busy", err)
	}
	assertLegacyImportState(t, ctx, store, stateSnapshot.WorkspaceID, "pending")
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
	if _, err := validateLegacyWorkspaceImport(value, files); err != nil {
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
	return store.CommitWorkspaceLegacyVolumeImport(ctx, value, files)
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
	}, files)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("file count limit error = %v, want quota exceeded", err)
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
