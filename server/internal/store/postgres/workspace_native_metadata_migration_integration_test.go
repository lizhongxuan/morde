package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceNativeMetadataManifestSessionAndCopyRoundTrip(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 7, 1<<30, 1<<30)
	source := createTestWorkspace(t, ctx, store, owner.ID, "native-metadata-source")

	file := syncTestFile("config/settings.ini", []byte("native metadata payload"))
	file.MetadataKnown = true
	file.Xattrs = []workspace.ExtendedAttribute{{Name: "user.comment", Value: []byte("settings")}}
	file.ACL = &workspace.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{1, 2, 3}}
	files := []workspace.File{file}
	rootMode, rootTime := uint32(0o755), int64(100)
	configMode, configTime := uint32(0o750), int64(200)
	cacheMode, cacheTime := uint32(0o700), int64(300)
	directories := []workspace.Directory{
		{Path: ".", Mode: &rootMode, ModTimeNS: &rootTime, MetadataKnown: true, Xattrs: []workspace.ExtendedAttribute{{Name: "user.root", Value: []byte("kept")}}},
		{Path: "cache", Mode: &cacheMode, ModTimeNS: &cacheTime, MetadataKnown: true},
		{Path: "config", Mode: &configMode, ModTimeNS: &configTime, MetadataKnown: true,
			ACL: &workspace.NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: []byte{4, 5, 6}}},
	}
	session := newTestSyncSession(t, source, owner.ID, files, files, nil)
	session.EmptyDirectories = []string{"cache"}
	session.UploadEmptyDirectories = []string{"cache"}
	session.Directories = directories
	session.UploadDirectories = directories
	session.DownloadDirectories = []workspace.Directory{}
	created, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(files),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "native-metadata-session", session.ID))
	if err != nil {
		t.Fatalf("create metadata Workspace sync session: %v", err)
	}
	if !reflect.DeepEqual(created.UploadDirectories, directories) {
		t.Fatalf("persisted sync session directories = %#v; want %#v", created.UploadDirectories, directories)
	}
	loaded, err := store.GetWorkspaceSyncSession(ctx, source.ID, session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Directories, directories) || !reflect.DeepEqual(loaded.UploadDirectories, directories) {
		t.Fatalf("loaded sync session metadata = %#v, err=%v", loaded, err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, source.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "native-metadata-commit", session.ID)); err != nil {
		t.Fatalf("commit metadata Workspace sync session: %v", err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, source.ID, 1, "", "", 20)
	fileMatches := len(page.Files) == 1 && sameWorkspaceFile(page.Files[0], files[0])
	if err != nil || !fileMatches || !reflect.DeepEqual(page.EmptyDirectories, []string{"cache"}) || !sameWorkspaceDirectorySnapshots(page.Directories, directories) {
		t.Fatalf("metadata Workspace manifest = %#v, err=%v", page, err)
	}
	expectedDigest, err := workspace.ManifestTreeWithMetadataSHA256(files, page.EmptyDirectories, directories)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	storedDigest, storedBytes, err := workspaceRevisionManifestSHA256(ctx, tx, source.ID, 1)
	_ = tx.Rollback(ctx)
	if err != nil || storedDigest != expectedDigest || storedBytes != files[0].SizeBytes {
		t.Fatalf("Site revision manifest digest=%q bytes=%d err=%v; want digest=%q bytes=%d", storedDigest, storedBytes, err, expectedDigest, files[0].SizeBytes)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, source.ID, 0, "", 20)
	if err != nil {
		t.Fatalf("read metadata Workspace change feed: %v", err)
	}
	changeDirectories := make(map[string]workspace.Directory)
	for _, item := range changes.Items {
		if item.Action == workspace.ChangeDirectoryUpsert && item.Directory != nil {
			changeDirectories[item.Path] = *item.Directory
		}
	}
	if len(changeDirectories) != len(directories) {
		t.Fatalf("directory metadata changes = %#v; want %d entries", changeDirectories, len(directories))
	}
	var fileIsNull bool
	var storedDirectory []byte
	if err := pool.QueryRow(ctx, `SELECT file IS NULL,directory FROM workspace_changes
WHERE workspace_id=$1 AND revision=1 AND file_path='.' AND action='directory_upsert'`, source.ID).Scan(&fileIsNull, &storedDirectory); err != nil || !fileIsNull || len(storedDirectory) == 0 {
		t.Fatalf("root directory change columns: file_null=%t directory=%s err=%v", fileIsNull, storedDirectory, err)
	}
	for _, directory := range directories {
		if got, ok := changeDirectories[directory.Path]; !ok || !workspace.SameDirectoryMetadata(got, directory) {
			t.Fatalf("change feed metadata for %q = %#v, found=%t; want %#v", directory.Path, got, ok, directory)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=1 WHERE workspace_id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreWorkspaceRevision(ctx, owner.ID, source.ID, 1, 1, []string{"config"},
		syncTestIdempotency(owner.ID, "workspace.restore", "native-metadata-restore", source.ID))
	if err != nil || restored.Revision != 1 {
		t.Fatalf("restore metadata Workspace revision = %#v, err=%v", restored, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=1 WHERE workspace_id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, app.WorkspaceCopyReservationRequest{
		ID: "rabc1234", SourceOwnerID: owner.ID, TargetOwnerID: owner.ID, SourceWorkspaceID: source.ID, TargetWorkspaceID: "w7654321",
		TargetName: "native copy", ServiceInstanceID: "native-metadata-copy", SourceRevision: 1, ReservedBytes: files[0].SizeBytes,
		MaxOwnerBytes: 1 << 30, TargetQuotaBytes: 1 << 30,
		Idempotency: syncTestIdempotency(owner.ID, "workspace.copy", "native-metadata-copy", source.ID),
	})
	if err != nil || replayed {
		t.Fatalf("create metadata Workspace copy reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
	}
	if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyReserved, workspace.Copying); err != nil {
		t.Fatalf("start metadata Workspace copy: %v", err)
	}
	allocation := []app.WorkspaceBlockAllocation{{WorkspaceID: reservation.TargetWorkspaceID,
		Digest: files[0].Chunks[0].SHA256, AllocatedBytes: files[0].Chunks[0].SizeBytes}}
	if err := store.StageWorkspaceCopy(ctx, reservation.ID, owner.ID, "native copy", page.Files, page.EmptyDirectories, page.Directories,
		allocation, 1<<30); err != nil {
		t.Fatalf("stage metadata Workspace copy: %v", err)
	}
	if err := store.CompleteWorkspaceCopy(ctx, reservation.ID, owner.ID, 1<<30); err != nil {
		t.Fatalf("complete metadata Workspace copy: %v", err)
	}
	copied, err := store.GetWorkspaceManifestPage(ctx, reservation.TargetWorkspaceID, 1, "", "", 20)
	filesMatch := len(copied.Files) == len(page.Files) && len(copied.Files) == 1 && sameWorkspaceFile(copied.Files[0], page.Files[0])
	if err != nil || !filesMatch || !sameWorkspaceDirectorySnapshots(copied.Directories, page.Directories) {
		t.Fatalf("copied native metadata manifest = %#v, err=%v", copied, err)
	}
}

func sameWorkspaceDirectorySnapshots(left, right []workspace.Directory) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !workspace.SameDirectoryMetadata(left[index], right[index]) {
			return false
		}
	}
	return true
}

func TestWorkspaceNativeMetadataMigrationDownUpAndGuard(t *testing.T) {
	t.Run("empty schema round trip", func(t *testing.T) {
		_, pool := newTemporaryStore(t)
		ctx := context.Background()
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 126); err != nil {
			t.Fatalf("roll back empty native metadata migration: %v", err)
		}
		assertWorkspaceNativeMetadataColumns(t, ctx, pool, false)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("reapply native metadata migration: %v", err)
		}
		assertWorkspaceNativeMetadataColumns(t, ctx, pool, true)
	})

	t.Run("native attributes block rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "native-metadata-down-guard")
		removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
		xattrsJSON, aclJSON, err := workspaceNativeMetadataJSON(
			[]workspace.ExtendedAttribute{{Name: "user.guard", Value: []byte("written")}},
			&workspace.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{9, 8, 7}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks,xattrs,acl,metadata_known)
VALUES ($1,0,'guard.txt','guard.txt',0,$2,420,'[]'::jsonb,$3::jsonb,$4::jsonb,true)`, value.ID, strings.Repeat("0", 64), xattrsJSON, aclJSON); err != nil {
			t.Fatalf("insert native metadata revision file: %v", err)
		}
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 126); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace native metadata") {
			t.Fatalf("native metadata rollback error = %v", err)
		}
		assertWorkspaceNativeMetadataColumns(t, ctx, pool, true)
		var metadataPreserved bool
		if err := pool.QueryRow(ctx, `SELECT xattrs=$2::jsonb FROM workspace_revision_files WHERE workspace_id=$1 AND file_path='guard.txt'`, value.ID, xattrsJSON).Scan(&metadataPreserved); err != nil || !metadataPreserved {
			t.Fatalf("native metadata after rejected rollback preserved=%t, err=%v", metadataPreserved, err)
		}
	})

	t.Run("directory change payload blocks rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 9, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "native-directory-down-guard")
		removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_changes (workspace_id,revision,file_path,action,directory)
VALUES ($1,0,'.','directory_upsert','{"path":".","metadata_known":false}'::jsonb)`, value.ID); err != nil {
			t.Fatalf("insert root directory change payload: %v", err)
		}
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 126); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace native metadata") {
			t.Fatalf("directory change rollback error = %v", err)
		}
		assertWorkspaceNativeMetadataColumns(t, ctx, pool, true)
	})
}

func assertWorkspaceNativeMetadataColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND (
  (table_name IN ('workspace_revision_files','workspace_revision_directories') AND column_name IN ('xattrs','acl','metadata_known')) OR
  (table_name='workspace_changes' AND column_name='directory')
)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	want := 7
	if !expected {
		want = 0
	}
	if count != want {
		t.Fatalf("native metadata column count = %d, want %d", count, want)
	}
}
