package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceSymlinkMigrationDownUpAndPreservesIdentity(t *testing.T) {
	t.Run("empty schema round trip", func(t *testing.T) {
		_, pool := newTemporaryStore(t)
		ctx := context.Background()
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 118); err != nil {
			t.Fatalf("roll back empty Workspace symlink migration: %v", err)
		}
		assertWorkspaceSymlinkMigrationColumns(t, ctx, pool, false)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("reapply Workspace symlink migration: %v", err)
		}
		assertWorkspaceSymlinkMigrationColumns(t, ctx, pool, true)
	})

	t.Run("symlink revision is preserved and blocks rollback", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 4, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "symlink-down-guard")
		removeInitialWorkspaceRootForLegacyTest(t, ctx, pool, value.ID)
		target := "../README.md"
		digest := sha256.Sum256([]byte(target))
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,kind,symlink_target,chunks)
VALUES ($1,0,'src/current','src/current',$2,$3,0,'symlink',$4,'[]'::jsonb)`, value.ID, len([]byte(target)), hex.EncodeToString(digest[:]), target); err != nil {
			t.Fatalf("insert Workspace symlink revision: %v", err)
		}
		for _, test := range []struct{ path, target string }{
			{path: "src/invalid-colon", target: "name:stream"},
			{path: "src/invalid-length", target: strings.Repeat("a", 4097)},
		} {
			unsafeDigest := sha256.Sum256([]byte(test.target))
			_, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,kind,symlink_target,chunks)
			VALUES ($1,0,$2,$2,$3,$4,0,'symlink',$5,'[]'::jsonb)`, value.ID, test.path, len([]byte(test.target)), hex.EncodeToString(unsafeDigest[:]), test.target)
			if err == nil {
				t.Fatalf("database accepted unsafe symlink target %q", test.target)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE workspace_revisions SET file_count=1,total_bytes=$2 WHERE workspace_id=$1 AND revision=0`, value.ID, len([]byte(target))); err != nil {
			t.Fatal(err)
		}
		page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 0, "", "", 10)
		if err != nil || len(page.Files) != 1 || page.Files[0].Kind != "symlink" || page.Files[0].SymlinkTarget != target {
			t.Fatalf("Workspace symlink manifest = %#v, err=%v", page.Files, err)
		}
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 118); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace symlink identity") {
			t.Fatalf("Workspace symlink migration rollback error = %v", err)
		}
		var storedTarget string
		if err := pool.QueryRow(ctx, `SELECT symlink_target FROM workspace_revision_files WHERE workspace_id=$1 AND file_path='src/current'`, value.ID).Scan(&storedTarget); err != nil || storedTarget != target {
			t.Fatalf("symlink target after rejected rollback = %q, err=%v", storedTarget, err)
		}
	})
}

func assertWorkspaceSymlinkMigrationColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var kindColumn, targetColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_revision_files' AND column_name='kind'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_revision_files' AND column_name='symlink_target')`).Scan(&kindColumn, &targetColumn); err != nil {
		t.Fatal(err)
	}
	if kindColumn != expected || targetColumn != expected {
		t.Fatalf("Workspace symlink columns = %t/%t; want %t", kindColumn, targetColumn, expected)
	}
}
