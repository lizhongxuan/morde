package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceCreationRegistersPendingVolumeAndReadyIsIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	user := createTestUser(t, store, 2, 4000, 8192)
	value := createTestWorkspace(t, ctx, store, user.ID, "volume registry")
	name := app.WorkspaceNamedVolumeName(value.ID)

	registered, err := store.GetWorkspaceVolumeRegistry(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if registered.WorkspaceID != value.ID || registered.OwnerID != user.ID || registered.State != app.WorkspaceVolumePending || registered.VolumeName != name {
		t.Fatalf("new Workspace volume registration = %#v", registered)
	}
	if err := store.MarkWorkspaceVolumeReady(ctx, user.ID, value.ID, name); err != nil {
		t.Fatalf("MarkWorkspaceVolumeReady() = %v", err)
	}
	if err := store.MarkWorkspaceVolumeReady(ctx, user.ID, value.ID, name); err != nil {
		t.Fatalf("idempotent MarkWorkspaceVolumeReady() = %v", err)
	}
	registered, err = store.GetWorkspaceVolumeRegistry(ctx, value.ID)
	if err != nil || registered.State != app.WorkspaceVolumeReady || registered.VolumeName != name {
		t.Fatalf("ready Workspace volume registration = %#v, %v", registered, err)
	}

	if err := store.MarkWorkspaceVolumeReady(ctx, "u8765432", value.ID, name); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("MarkWorkspaceVolumeReady(foreign owner) = %v, want not found", err)
	}
	if err := store.MarkWorkspaceVolumeReady(ctx, user.ID, value.ID, "workspace-w8765432"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("MarkWorkspaceVolumeReady(wrong name) = %v, want invalid argument", err)
	}
	if value.State != workspace.StateReady {
		t.Fatalf("Workspace state changed during volume registration: %q", value.State)
	}
}
