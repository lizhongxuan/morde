package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/domain/workspace"
)

func TestWorkspaceDirectoryMigrationDownUpAndPreservesRows(t *testing.T) {
	t.Run("empty schema round trip", func(t *testing.T) {
		_, pool := newTemporaryStore(t)
		ctx := context.Background()
		provider, closeProvider, err := newMigrationProvider(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeProvider()
		if _, err := provider.DownTo(ctx, 116); err != nil {
			t.Fatalf("roll back empty Workspace directory metadata migration: %v", err)
		}
		assertWorkspaceDirectoryMigrationTables(t, ctx, pool, false)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("reapply Workspace directory metadata migration: %v", err)
		}
		assertWorkspaceDirectoryMigrationTables(t, ctx, pool, true)
	})

	t.Run("revision metadata", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 3, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "directory-down-revision")
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns)
VALUES ($1,0,'src','src',493,123)`, value.ID); err != nil {
			t.Fatalf("insert directory revision metadata: %v", err)
		}
		assertWorkspaceDirectoryMigrationDownRejected(t, ctx, pool)
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_revision_directories WHERE workspace_id=$1 AND directory_path='src'`, value.ID).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("directory metadata after rejected rollback = %d, err=%v", rows, err)
		}
	})

	t.Run("sync session metadata", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 4, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "directory-down-session")
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_sessions
(id,workspace_id,owner_id,replica_id,direction,policy,state,stage,base_revision,target_revision,pinned_revision,planned_bytes,files,upload_files,download_files)
VALUES ('pabc1234',$1,$2,'replica-directory-test','bidirectional','live','planned','scan',0,0,0,0,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb)`, value.ID, owner.ID); err != nil {
			t.Fatalf("insert directory metadata sync session: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_session_directories
(workspace_id,session_id,directories,upload_directories,download_directories)
VALUES ($1,'pabc1234','[{"path":"src","mode":493,"mod_time_ns":123}]'::jsonb,
'[{"path":"src","mode":493,"mod_time_ns":123}]'::jsonb,'[]'::jsonb)`, value.ID); err != nil {
			t.Fatalf("insert directory metadata sync session payload: %v", err)
		}
		assertWorkspaceDirectoryMigrationDownRejected(t, ctx, pool)
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_session_directories WHERE workspace_id=$1 AND session_id='pabc1234'`, value.ID).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("sync session directory metadata after rejected rollback = %d, err=%v", rows, err)
		}
	})

	t.Run("legacy directory paths remain explicitly unknown", func(t *testing.T) {
		store, pool := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 5, 10_000, 20_000)
		value := createTestWorkspace(t, ctx, store, owner.ID, "directory-legacy-unknown")
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks)
VALUES ($1,0,'src/main.go','src/main.go',0,$2,420,'[]'::jsonb)`, value.ID, strings.Repeat("0", 64)); err != nil {
			t.Fatalf("insert legacy revision file: %v", err)
		}
		page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 0, "", "", 100)
		if err != nil || len(page.Files) != 1 || len(page.Directories) != 2 ||
			!workspace.SameDirectoryMetadata(page.Directories[0], workspace.Directory{Path: "."}) ||
			!workspace.SameDirectoryMetadata(page.Directories[1], workspace.Directory{Path: "src"}) {
			t.Fatalf("legacy manifest directories = %#v, err=%v; want explicit unknown src", page.Directories, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns)
VALUES ($1,0,'src','src',488,123)`, value.ID); err != nil {
			t.Fatalf("insert directory metadata after legacy revision: %v", err)
		}
		page, err = store.GetWorkspaceManifestPage(ctx, value.ID, 0, "", "", 100)
		if err != nil || len(page.Directories) != 2 || !workspace.SameDirectoryMetadata(page.Directories[0], workspace.Directory{Path: "."}) ||
			page.Directories[1].Mode == nil || *page.Directories[1].Mode != 0o750 ||
			page.Directories[1].ModTimeNS == nil || *page.Directories[1].ModTimeNS != 123 {
			t.Fatalf("known manifest directory metadata = %#v, err=%v", page.Directories, err)
		}
	})
}

func assertWorkspaceDirectoryMigrationDownRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 116); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace directory metadata") {
		t.Fatalf("Workspace directory metadata rollback error = %v", err)
	}
	assertWorkspaceDirectoryMigrationTables(t, ctx, pool, true)
}

func assertWorkspaceDirectoryMigrationTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var revisionTable, sessionTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workspace_revision_directories') IS NOT NULL,
to_regclass('workspace_sync_session_directories') IS NOT NULL`).Scan(&revisionTable, &sessionTable); err != nil {
		t.Fatal(err)
	}
	if revisionTable != expected || sessionTable != expected {
		t.Fatalf("Workspace directory metadata tables = revision:%t session:%t, want %t", revisionTable, sessionTable, expected)
	}
}
