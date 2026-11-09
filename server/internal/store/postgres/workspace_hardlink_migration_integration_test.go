package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceHardlinkMigrationDownUpAndPreservesIdentity(t *testing.T) {
	t.Run("sync stores hardlink identity in a revision", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 7, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "hardlink-sync")
		anchor := syncTestFile("a.txt", []byte("shared inode"))
		alias := anchor
		alias.Path, alias.HardlinkTo = "z.txt", anchor.Path
		files := []workspace.File{anchor, alias}
		session := newTestSyncSession(t, value, owner.ID, files, files, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(files),
			syncTestIdempotency(owner.ID, "workspace.sync.create", "hardlink-sync", session.ID)); err != nil {
			t.Fatalf("create hardlink Workspace sync session: %v", err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "hardlink-sync", session.ID)); err != nil {
			t.Fatalf("commit hardlink Workspace sync session: %v", err)
		}
		page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 10)
		if err != nil || len(page.Files) != 2 || page.Files[1].HardlinkTo != "a.txt" {
			t.Fatalf("synced hardlink manifest = %#v, err=%v", page.Files, err)
		}
		changes, err := store.GetWorkspaceChangePage(ctx, value.ID, 0, "", 10)
		if err != nil || len(changes.Items) != 2 || changes.Items[1].File == nil || changes.Items[1].File.HardlinkTo != "a.txt" {
			t.Fatalf("synced hardlink changes = %#v, err=%v", changes.Items, err)
		}
	})

	t.Run("copy stages hardlink identity in the target revision", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 1<<30, 1<<30)
		source := createTestWorkspace(t, ctx, store, owner.ID, "hardlink-copy-source")
		anchor := syncTestFile("a.txt", []byte("shared inode"))
		alias := anchor
		alias.Path, alias.HardlinkTo = "z.txt", anchor.Path
		files := []workspace.File{anchor, alias}
		session := newTestSyncSession(t, source, owner.ID, files, files, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(files),
			syncTestIdempotency(owner.ID, "workspace.sync.create", "hardlink-copy-seed", session.ID)); err != nil {
			t.Fatalf("create source Workspace sync session: %v", err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, source.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "hardlink-copy-seed", session.ID)); err != nil {
			t.Fatalf("commit source Workspace sync session: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, source.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=1 WHERE workspace_id=$1`, source.ID); err != nil {
			t.Fatal(err)
		}
		sourcePage, err := store.GetWorkspaceManifestPage(ctx, source.ID, 1, "", "", 10)
		if err != nil {
			t.Fatalf("read complete hardlink source snapshot: %v", err)
		}
		copyKey := syncTestIdempotency(owner.ID, "workspace.copy", "hardlink-copy", source.ID)
		reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, app.WorkspaceCopyReservationRequest{
			ID: "rabc1234", SourceOwnerID: owner.ID, TargetOwnerID: owner.ID, SourceWorkspaceID: source.ID, TargetWorkspaceID: "w7654321",
			TargetName: "hardlink copy target", ServiceInstanceID: "hardlink-copy-test", SourceRevision: 1,
			ReservedBytes: int64(len("shared inode")), MaxOwnerBytes: 1 << 30, TargetQuotaBytes: 1 << 30, Idempotency: copyKey,
		})
		if err != nil || replayed {
			t.Fatalf("create hardlink Workspace copy reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
		}
		if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyReserved, workspace.Copying); err != nil {
			t.Fatalf("start hardlink Workspace copy: %v", err)
		}
		allocation := []app.WorkspaceBlockAllocation{{WorkspaceID: reservation.TargetWorkspaceID, Digest: anchor.Chunks[0].SHA256, AllocatedBytes: anchor.Chunks[0].SizeBytes}}
		if err := store.StageWorkspaceCopy(ctx, reservation.ID, owner.ID, "hardlink copy target", sourcePage.Files, sourcePage.EmptyDirectories,
			sourcePage.Directories, allocation, 1<<30); err != nil {
			t.Fatalf("stage hardlink Workspace copy: %v", err)
		}
		if err := store.CompleteWorkspaceCopy(ctx, reservation.ID, owner.ID, 1<<30); err != nil {
			t.Fatalf("complete hardlink Workspace copy: %v", err)
		}
		page, err := store.GetWorkspaceManifestPage(ctx, reservation.TargetWorkspaceID, 1, "", "", 10)
		if err != nil || len(page.Files) != 2 || page.Files[1].HardlinkTo != "a.txt" {
			t.Fatalf("copied hardlink manifest = %#v, err=%v", page.Files, err)
		}
	})

	t.Run("empty schema round trip", func(t *testing.T) {
		_, pool := newTemporaryStore(t)
		ctx := context.Background()
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 125); err != nil {
			t.Fatalf("roll back empty Workspace hardlink migration: %v", err)
		}
		assertWorkspaceHardlinkMigrationColumn(t, ctx, pool, false)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("reapply Workspace hardlink migration: %v", err)
		}
		assertWorkspaceHardlinkMigrationColumn(t, ctx, pool, true)
	})

	t.Run("revision identity is preserved and blocks rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 6, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "hardlink-down-guard")
		removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
		content := []byte("shared inode")
		digest := sha256.Sum256(content)
		sha := hex.EncodeToString(digest[:])
		chunkData, err := json.Marshal([]workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}})
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range []struct{ path, anchor string }{{"a.txt", ""}, {"z.txt", "a.txt"}} {
			if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,kind,symlink_target,hardlink_to,chunks)
				VALUES ($1,0,$2,$2,$3,$4,420,'file',NULL,$5,$6::jsonb)`, value.ID, file.path, len(content), sha, nullableWorkspaceHardlink(file.anchor), chunkData); err != nil {
				t.Fatalf("insert Workspace revision file %q: %v", file.path, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE workspace_revisions SET file_count=2,total_bytes=$2 WHERE workspace_id=$1 AND revision=0`, value.ID, 2*len(content)); err != nil {
			t.Fatal(err)
		}
		page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 0, "", "", 10)
		if err != nil || len(page.Files) != 2 || page.Files[1].HardlinkTo != "a.txt" {
			t.Fatalf("Workspace hardlink manifest = %#v, err=%v", page.Files, err)
		}
		for _, invalid := range []string{"", "/absolute", "../outside", "a/../outside", "a//b", "a/", "a:b", `a\b`} {
			if _, err := pool.Exec(ctx, `UPDATE workspace_revision_files SET hardlink_to=$3 WHERE workspace_id=$1 AND file_path=$2`, value.ID, "z.txt", invalid); err == nil {
				t.Errorf("stored invalid hardlink target %q", invalid)
			}
		}

		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 125); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace hardlink identity") {
			t.Fatalf("Workspace hardlink migration rollback error = %v", err)
		}
		var storedAnchor string
		if err := pool.QueryRow(ctx, `SELECT hardlink_to FROM workspace_revision_files WHERE workspace_id=$1 AND file_path='z.txt'`, value.ID).Scan(&storedAnchor); err != nil || storedAnchor != "a.txt" {
			t.Fatalf("hardlink target after rejected rollback = %q, err=%v", storedAnchor, err)
		}
	})
}

func assertWorkspaceHardlinkMigrationColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var actual bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='workspace_revision_files' AND column_name='hardlink_to')`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("Workspace hardlink column exists = %t; want %t", actual, expected)
	}
}

func nullableWorkspaceHardlink(value string) any {
	if value == "" {
		return nil
	}
	return value
}
