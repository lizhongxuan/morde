package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestAdminWorkspaceQueriesPageAndReturnHostMetadata(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	first := createTestWorkspace(t, ctx, store, owner.ID, "admin-list-first")
	hostID := createRunningSiteTestHost(t, ctx, store, owner.ID, first.ID)
	second := createTestWorkspace(t, ctx, store, owner.ID, "admin-list-second")
	otherOwner := createTestUser(t, store, 3, 20_000, 40_000)
	foreign := createTestWorkspace(t, ctx, store, otherOwner.ID, "admin-list-foreign")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleted', deleted_at=now() WHERE id=$1`, foreign.ID); err != nil {
		t.Fatal(err)
	}

	firstPage, err := store.ListAdminWorkspaces(ctx, app.PageRequest{Limit: 1})
	if err != nil || len(firstPage.Items) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first admin workspace page = %#v, %v", firstPage, err)
	}
	secondPage, err := store.ListAdminWorkspaces(ctx, app.PageRequest{Limit: 1, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Items) != 1 || secondPage.NextCursor != "" || firstPage.Items[0].ID == secondPage.Items[0].ID {
		t.Fatalf("second admin workspace page = %#v, %v", secondPage, err)
	}
	all, err := store.ListAdminWorkspaces(ctx, app.PageRequest{Limit: 100})
	if err != nil || len(all.Items) != 2 {
		t.Fatalf("all admin workspaces = %#v, %v", all, err)
	}
	if all.Items[0].ID != first.ID && all.Items[1].ID != first.ID || all.Items[0].ID != second.ID && all.Items[1].ID != second.ID {
		t.Fatalf("admin workspace list missed expected IDs: %#v", all.Items)
	}
	var bound workspace.AdminSummary
	for _, item := range all.Items {
		if item.ID == first.ID {
			bound = item
		}
	}
	if bound.ID != first.ID || bound.OwnerID != owner.ID || bound.OwnerName != owner.Name || len(bound.Bindings) != 1 ||
		bound.Bindings[0].HostID != hostID || bound.Bindings[0].State != workspace.BindingActive {
		t.Fatalf("admin workspace binding metadata = %#v", bound)
	}

	detail, err := store.GetAdminWorkspace(ctx, first.ID)
	if err != nil || detail.ID != first.ID || len(detail.Bindings) != 1 || detail.Bindings[0].HostID != hostID {
		t.Fatalf("admin workspace detail = %#v, %v", detail, err)
	}
	if _, err := store.GetAdminWorkspace(ctx, foreign.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted admin workspace error = %v, want not found", err)
	}
	if _, err := store.GetAdminWorkspace(ctx, "w0000000"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing admin workspace error = %v, want not found", err)
	}
}
