package postgres

import (
	"context"
	"testing"
)

func TestEmptyWorkspaceRootMigrationOnlyRepairsUnmaterializedInitialRevisions(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 10_000, 20_000)
	empty := createTestWorkspace(t, ctx, store, owner.ID, "empty-pending")
	knownUnknown := createTestWorkspace(t, ctx, store, owner.ID, "explicit-unknown")
	applied := createTestWorkspace(t, ctx, store, owner.ID, "already-materialized")
	nonempty := createTestWorkspace(t, ctx, store, owner.ID, "has-file")
	legacy := createTestWorkspace(t, ctx, store, owner.ID, "legacy-volume")
	legacyImport := createTestWorkspace(t, ctx, store, owner.ID, "pending-legacy-import")
	readyVolume := createTestWorkspace(t, ctx, store, owner.ID, "ready-volume")
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 135); err != nil {
		t.Fatal(err)
	}
	workspaceIDs := []string{empty.ID, knownUnknown.ID, applied.ID, nonempty.ID, legacy.ID, legacyImport.ID, readyVolume.ID}
	if _, err := store.pool.Exec(ctx, `DELETE FROM workspace_revision_directories WHERE workspace_id=ANY($1::text[])`, workspaceIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_directories(workspace_id,valid_from_revision,directory_path,folded_path,metadata_known)
VALUES ($1,0,'.','.',false)`, knownUnknown.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_files(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks)
VALUES ($1,0,'existing','existing',0,repeat('0',64),384,'[]'::jsonb)`, nonempty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_revision_directories SET metadata_known=false WHERE workspace_id=$1 AND directory_path='.'`, knownUnknown.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, applied.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='legacy',volume_name=NULL WHERE workspace_id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes(workspace_id,volume_name,import_state) VALUES ($1,$2,'pending')`, legacyImport.ID, "legacy-"+legacyImport.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, readyVolume.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 136); err != nil {
		t.Fatal(err)
	}
	var known bool
	if err := store.pool.QueryRow(ctx, `SELECT metadata_known FROM workspace_revision_directories WHERE workspace_id=$1 AND directory_path='.'`, empty.ID).Scan(&known); err != nil || !known {
		t.Fatalf("empty pending Workspace root metadata = %v, %v", known, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT metadata_known FROM workspace_revision_directories WHERE workspace_id=$1 AND directory_path='.'`, knownUnknown.ID).Scan(&known); err != nil || known {
		t.Fatalf("explicit unknown metadata was changed: %v, %v", known, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_revision_directories WHERE workspace_id=$1`, applied.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("already-materialized Workspace was modified: %d, %v", count, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_revision_directories WHERE workspace_id=$1`, nonempty.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Workspace with file data was modified: %d, %v", count, err)
	}
	for _, id := range []string{legacy.ID, legacyImport.ID, readyVolume.ID} {
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_revision_directories WHERE workspace_id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("Workspace with an existing or legacy volume was modified: %d, %v", count, err)
		}
	}
	if _, err := provider.UpTo(ctx, 143); err != nil {
		t.Fatal(err)
	}
	created := createTestWorkspace(t, ctx, store, owner.ID, "new-workspace")
	if err := store.pool.QueryRow(ctx, `SELECT metadata_known FROM workspace_revision_directories WHERE workspace_id=$1 AND directory_path='.'`, created.ID).Scan(&known); err != nil || !known {
		t.Fatalf("new Workspace root metadata = %v, %v", known, err)
	}
}
