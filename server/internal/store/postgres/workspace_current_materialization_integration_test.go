package postgres

import (
	"context"
	"testing"
)

func TestListWorkspaceCurrentMaterializationsSkipsDeletedWorkspace(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	ready := createTestWorkspace(t, ctx, store, owner.ID, "current-materialization-ready")
	deleted := createTestWorkspace(t, ctx, store, owner.ID, "current-materialization-deleted")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleted',deleted_at=clock_timestamp() WHERE id=$1`, deleted.ID); err != nil {
		t.Fatal(err)
	}
	values, err := store.ListWorkspaceCurrentMaterializations(ctx, "", 100)
	if err != nil || len(values) != 1 || values[0].WorkspaceID != ready.ID || values[0].OwnerID != owner.ID {
		t.Fatalf("ListWorkspaceCurrentMaterializations() = %#v, %v; want only ready Workspace %s", values, err, ready.ID)
	}
}
