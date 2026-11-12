package postgres

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceSyncProjectUploadMigrationFieldsConstraintsAndRollbackGuards(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-migration")
	folderID := "morde-11111111111111111111111111111111"
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_folders (workspace_id,owner_id,folder_id,state)
VALUES ($1,$2,$3,'active')`, value.ID, owner.ID, folderID); err != nil {
		t.Fatalf("insert Workspace root folder for project upload migration: %v", err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes) VALUES ($1,1,0,0)`, value.ID); err != nil {
		t.Fatalf("create upload baseline revision: %v", err)
	}

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 186); err != nil {
		t.Fatalf("roll back latest migrations before project upload migration: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply project upload and Host copy migrations: %v", err)
	}
	assertWorkspaceSyncProjectUploadMigrationSchema(t, ctx, pool, true)

	if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET last_upload_revision=1 WHERE workspace_id=$1 AND folder_id=$2`, value.ID, folderID); err != nil {
		t.Fatalf("store folder upload revision: %v", err)
	}
	assertWorkspaceSyncProjectUploadFolderRevisionRejected(t, ctx, pool, value.ID, folderID)
	assertWorkspaceSyncProjectUploadSession(t, ctx, pool, "paaaaaaa", value.ID, owner.ID, folderID, 1, "upload", "compare_merge", []string{"."}, 1)

	// Legacy sessions remain valid when both new folder fields are NULL.
	assertWorkspaceSyncProjectUploadSession(t, ctx, pool, "pbbbbbbb", value.ID, owner.ID, nil, nil, "download", "fill_missing", []string{}, 0)
	assertWorkspaceSyncProjectUploadSessionRejected(t, ctx, pool, "pccccccc", value.ID, owner.ID, nil, 1, "download", "fill_missing", []string{}, 0)
	assertWorkspaceSyncProjectUploadSessionRejected(t, ctx, pool, "pddddddd", value.ID, owner.ID, folderID, nil, "download", "compare_merge", []string{"."}, 0)
	assertWorkspaceSyncProjectUploadSessionRejected(t, ctx, pool, "peeeeeee", value.ID, owner.ID, folderID, nil, "upload", "compare_merge", []string{}, 0)
	assertWorkspaceSyncProjectUploadSessionRejected(t, ctx, pool, "pfffffff", value.ID, owner.ID, "morde-ffffffffffffffffffffffffffffffff", nil, "upload", "compare_merge", []string{"."}, 0)
	assertWorkspaceSyncProjectUploadSessionRejected(t, ctx, pool, "pggggggg", value.ID, owner.ID, folderID, 0, "upload", "compare_merge", []string{"."}, 1)

	if _, err := provider.DownTo(ctx, 186); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace project upload sessions or baselines") {
		t.Fatalf("rollback with folder-bound session = %v; want project upload rollback protection", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_sync_sessions WHERE id='paaaaaaa'`); err != nil {
		t.Fatalf("remove folder-bound session for baseline rollback check: %v", err)
	}
	if _, err := provider.DownTo(ctx, 186); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace project upload sessions or baselines") {
		t.Fatalf("rollback with folder upload revision = %v; want project upload rollback protection", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET last_upload_revision=NULL WHERE workspace_id=$1 AND folder_id=$2`, value.ID, folderID); err != nil {
		t.Fatalf("clear folder upload revision for clean rollback: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_sync_sessions WHERE id='pbbbbbbb'`); err != nil {
		t.Fatalf("remove legacy session for clean rollback: %v", err)
	}
	if _, err := provider.DownTo(ctx, 186); err != nil {
		t.Fatalf("roll back empty project upload and Host copy migrations: %v", err)
	}
	assertWorkspaceSyncProjectUploadMigrationSchema(t, ctx, pool, false)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("reapply project upload and Host copy migrations: %v", err)
	}
	assertWorkspaceSyncProjectUploadMigrationSchema(t, ctx, pool, true)
}

func TestWorkspaceSyncHostCopySourceMigrationFieldsConstraintsAndRollbackGuard(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "host-copy-source-migration")
	target := createTestWorkspace(t, ctx, store, owner.ID, "host-copy-target-migration")
	sourceHost := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{source.ID})
	otherOwner := createTestUser(t, store, 3, 10_000, 20_000)
	otherSource := createTestWorkspace(t, ctx, store, otherOwner.ID, "host-copy-other-source-migration")
	otherHost := createTestHost(t, ctx, store, otherOwner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{otherSource.ID})

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 187); err != nil {
		t.Fatalf("roll back Host copy source migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_copy_reservations
(id,owner_id,source_owner_id,target_owner_id,source_workspace_id,target_workspace_id,service_instance_id,source_revision,reserved_bytes,state)
VALUES ('rabc1234',$1,$1,$1,$2,$3,'migration-test',0,0,'reserved')`, owner.ID, source.ID, target.ID); err != nil {
		t.Fatalf("insert pre-migration Workspace copy reservation: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply Host copy source migration: %v", err)
	}
	assertWorkspaceSyncHostCopySourceMigrationSchema(t, ctx, pool, true)

	var hostIsNull, pending bool
	var bindingVersion int64
	if err := pool.QueryRow(ctx, `SELECT source_host_id IS NULL,source_binding_version,host_copy_pending FROM workspace_copy_reservations WHERE id='rabc1234'`).
		Scan(&hostIsNull, &bindingVersion, &pending); err != nil || !hostIsNull || bindingVersion != 0 || pending {
		t.Fatalf("legacy copy source defaults: host_null=%t binding_version=%d pending=%t err=%v", hostIsNull, bindingVersion, pending, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_copy_reservations SET source_host_id=$1,source_binding_version=1,host_copy_pending=true WHERE id='rabc1234'`, sourceHost.ID); err != nil {
		t.Fatalf("record valid source Host and binding version: %v", err)
	}
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET source_binding_version=-1 WHERE id='rabc1234'`)
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET source_binding_version=0 WHERE id='rabc1234'`)
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET source_host_id=NULL,source_binding_version=1 WHERE id='rabc1234'`)
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET source_host_id='missing-host',source_binding_version=1 WHERE id='rabc1234'`)
	if _, err := pool.Exec(ctx, `UPDATE workspace_copy_reservations SET source_host_id=$1,source_binding_version=1 WHERE id='rabc1234'`, otherHost.ID); err == nil {
		t.Fatal("database accepted a source Host owned by a different source owner")
	}
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET host_copy_pending=true,source_host_id=NULL,source_binding_version=0 WHERE id='rabc1234'`)
	assertWorkspaceSyncHostCopySourceUpdateRejected(t, ctx, pool, `UPDATE workspace_copy_reservations SET state='failed' WHERE id='rabc1234'`)

	if _, err := provider.DownTo(ctx, 187); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back workspace copies that record a source host") {
		t.Fatalf("rollback with source Host = %v; want Host copy rollback protection", err)
	}
	var sourceHostID string
	if err := pool.QueryRow(ctx, `SELECT source_host_id FROM workspace_copy_reservations WHERE id='rabc1234'`).Scan(&sourceHostID); err != nil || sourceHostID != sourceHost.ID {
		t.Fatalf("source Host after rejected rollback = %q, err=%v", sourceHostID, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_copy_reservations SET source_host_id=NULL,source_binding_version=0,host_copy_pending=false WHERE id='rabc1234'`); err != nil {
		t.Fatalf("clear source Host metadata for clean rollback: %v", err)
	}
	if _, err := provider.DownTo(ctx, 187); err != nil {
		t.Fatalf("roll back Host copy source migration with no source Host data: %v", err)
	}
	assertWorkspaceSyncHostCopySourceMigrationSchema(t, ctx, pool, false)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("reapply Host copy source migration: %v", err)
	}
	assertWorkspaceSyncHostCopySourceMigrationSchema(t, ctx, pool, true)
	if err := pool.QueryRow(ctx, `SELECT source_host_id IS NULL,source_binding_version,host_copy_pending FROM workspace_copy_reservations WHERE id='rabc1234'`).
		Scan(&hostIsNull, &bindingVersion, &pending); err != nil || !hostIsNull || bindingVersion != 0 || pending {
		t.Fatalf("legacy copy source defaults after reapply: host_null=%t binding_version=%d pending=%t err=%v", hostIsNull, bindingVersion, pending, err)
	}
}

func assertWorkspaceSyncProjectUploadMigrationSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var lastUploadRevision, sessionFolderID, baselineRevision, folderIndex bool
	if err := pool.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_folders' AND column_name='last_upload_revision'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_sessions' AND column_name='folder_id'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_sync_sessions' AND column_name='folder_baseline_revision'),
EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='workspace_sync_sessions_project_folder')`).
		Scan(&lastUploadRevision, &sessionFolderID, &baselineRevision, &folderIndex); err != nil {
		t.Fatal(err)
	}
	if lastUploadRevision != expected || sessionFolderID != expected || baselineRevision != expected || folderIndex != expected {
		t.Fatalf("Workspace project upload schema: folder_revision=%t session_folder=%t baseline=%t index=%t; want %t",
			lastUploadRevision, sessionFolderID, baselineRevision, folderIndex, expected)
	}
	var constraints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid IN ('workspace_sync_folders'::regclass,'workspace_sync_sessions'::regclass)
AND conname IN ('workspace_sync_folder_last_upload_revision_fk','workspace_sync_session_folder_check','workspace_sync_session_folder_fk','workspace_sync_session_folder_baseline_revision_fk')`).Scan(&constraints); err != nil {
		t.Fatal(err)
	}
	wantConstraints := 4
	if !expected {
		wantConstraints = 0
	}
	if constraints != wantConstraints {
		t.Fatalf("Workspace project upload constraints = %d; want %d", constraints, wantConstraints)
	}
}

func assertWorkspaceSyncProjectUploadSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, workspaceID, ownerID string,
	folderID, baselineRevision any, direction, policy string, scope []string, baseRevision int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_sessions
(id,workspace_id,owner_id,replica_id,direction,policy,state,stage,base_revision,target_revision,pinned_revision,planned_bytes,
 files,upload_files,download_files,scope,folder_id,folder_baseline_revision)
VALUES ($1,$2,$3,'migration-replica',$4,$5,'completed','complete',$6,$6,$6,0,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb,$7,$8,$9)`,
		id, workspaceID, ownerID, direction, policy, baseRevision, scope, folderID, baselineRevision); err != nil {
		t.Fatalf("insert Workspace sync session %s: %v", id, err)
	}
}

func assertWorkspaceSyncProjectUploadSessionRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, workspaceID, ownerID string,
	folderID, baselineRevision any, direction, policy string, scope []string, baseRevision int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_sessions
(id,workspace_id,owner_id,replica_id,direction,policy,state,stage,base_revision,target_revision,pinned_revision,planned_bytes,
 files,upload_files,download_files,scope,folder_id,folder_baseline_revision)
VALUES ($1,$2,$3,'migration-replica',$4,$5,'completed','complete',$6,$6,$6,0,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb,$7,$8,$9)`,
		id, workspaceID, ownerID, direction, policy, baseRevision, scope, folderID, baselineRevision); err == nil {
		t.Fatalf("database accepted invalid Workspace sync session %s", id)
	}
}

func assertWorkspaceSyncProjectUploadFolderRevisionRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, folderID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE workspace_sync_folders SET last_upload_revision=999 WHERE workspace_id=$1 AND folder_id=$2`, workspaceID, folderID); err == nil {
		t.Fatal("database accepted an upload baseline that does not exist in the Workspace revision ledger")
	}
}

func assertWorkspaceSyncHostCopySourceMigrationSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var sourceHost, bindingVersion, pending, activeIndex, pendingIndex bool
	if err := pool.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_copy_reservations' AND column_name='source_host_id'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_copy_reservations' AND column_name='source_binding_version'),
EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_copy_reservations' AND column_name='host_copy_pending'),
EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='workspace_copy_reservations_source_host_active'),
EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='workspace_copy_reservations_host_copy_pending')`).
		Scan(&sourceHost, &bindingVersion, &pending, &activeIndex, &pendingIndex); err != nil {
		t.Fatal(err)
	}
	if sourceHost != expected || bindingVersion != expected || pending != expected || activeIndex != expected || pendingIndex != expected {
		t.Fatalf("Workspace Host copy source schema: host=%t binding_version=%t pending=%t active_index=%t pending_index=%t; want %t",
			sourceHost, bindingVersion, pending, activeIndex, pendingIndex, expected)
	}
	var constraints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='workspace_copy_reservations'::regclass
AND conname IN ('workspace_copy_reservations_host_copy_pending_check','workspace_copy_reservations_source_host_owner_fk','workspace_copy_reservations_source_host_binding_check')`).Scan(&constraints); err != nil {
		t.Fatal(err)
	}
	wantConstraints := 3
	if !expected {
		wantConstraints = 0
	}
	if constraints != wantConstraints {
		t.Fatalf("Workspace Host copy source constraints = %d; want %d", constraints, wantConstraints)
	}
}

func assertWorkspaceSyncHostCopySourceUpdateRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := pool.Exec(ctx, query); err == nil {
		t.Fatalf("database accepted invalid Workspace copy source update: %s", query)
	}
}
