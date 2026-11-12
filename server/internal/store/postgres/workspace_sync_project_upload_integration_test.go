package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceProjectUploadUsesInstallationCredentialWithoutSyncthingBinding(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-manual")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)
	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-folder-key")
	if err != nil {
		t.Fatalf("register project folder metadata: %v", err)
	}

	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	var nodeBindings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE workspace_id=$1 AND folder_id=$2`,
		workspaceValue.ID, folder.FolderID).Scan(&nodeBindings); err != nil || nodeBindings != 0 {
		t.Fatalf("project Syncthing bindings before upload = %d, err=%v; want zero", nodeBindings, err)
	}
	if err := store.CheckWorkspaceSyncProjectFolderAccess(ctx, device, workspaceValue.ID, folder.FolderID); err != nil {
		t.Fatalf("authorize installation-bound project upload without a Syncthing binding: %v", err)
	}

	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := workspace.CreateSyncSession{
		FolderID: folder.FolderID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		Files: []workspace.File{}, EmptyDirectories: []string{"empty"},
	}
	first, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-create-key")
	if err != nil {
		t.Fatalf("create manual project upload session: %v", err)
	}
	if first.FolderBaselineRevision != nil || first.BaseRevision != workspaceValue.Revision || len(first.TransferBlocks) != 0 {
		t.Fatalf("first project upload snapshot = %#v", first)
	}
	competingRequest := request
	competingRequest.EmptyDirectories = []string{"other"}
	competing, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, competingRequest, "project-upload-competing-create-key")
	if err != nil {
		t.Fatalf("create competing project upload before baseline advances: %v", err)
	}
	if competing.FolderBaselineRevision != nil || len(competing.TransferBlocks) != 0 {
		t.Fatalf("competing project upload snapshot = %#v", competing)
	}
	committed, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, first.ID, "project-upload-commit-key")
	if err != nil {
		t.Fatalf("commit metadata-only project upload: %v", err)
	}
	if committed.State != workspace.SyncCompleted || committed.TargetRevision <= workspaceValue.Revision {
		t.Fatalf("committed project upload = %#v", committed)
	}
	folderAfter, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfter.LastUploadRevision == nil || *folderAfter.LastUploadRevision != committed.TargetRevision {
		t.Fatalf("project upload baseline = %#v, err=%v; want revision %d", folderAfter.LastUploadRevision, err, committed.TargetRevision)
	}
	competingResult, err := syncService.CommitSyncSession(ctx, device, workspaceValue.ID, competing.ID, "project-upload-competing-commit-key")
	if err != nil || competingResult.State != workspace.SyncConflict {
		t.Fatalf("competing project upload result = %#v, err=%v; want conflict", competingResult, err)
	}
	folderAfterConflict, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfterConflict.LastUploadRevision == nil || *folderAfterConflict.LastUploadRevision != committed.TargetRevision {
		t.Fatalf("baseline after competing upload = %#v, err=%v; want unchanged revision %d", folderAfterConflict.LastUploadRevision, err, committed.TargetRevision)
	}
	replayed, err := syncService.CreateSyncSession(ctx, device, workspaceValue.ID, request, "project-upload-create-key")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("same project upload request after baseline advance = session %q, err=%v; want original %q", replayed.ID, err, first.ID)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_bindings WHERE workspace_id=$1 AND folder_id=$2`,
		workspaceValue.ID, folder.FolderID).Scan(&nodeBindings); err != nil || nodeBindings != 0 {
		t.Fatalf("project Syncthing bindings after upload = %d, err=%v; want zero", nodeBindings, err)
	}
}

func TestFirstProjectUploadConflictsWithExistingDestinationWithoutTransfer(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-upload-existing-destination")
	hostValue := workspaceSyncHostForFolderTest(t, ctx, store, owner.ID, workspaceValue.ID)
	portal := workspaceSyncPortalPrincipal(t, store, owner.ID)

	seedService, err := app.NewWorkspaceSyncService(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := seedService.CreateSyncSession(ctx, portal, workspaceValue.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		EmptyDirectories: []string{"project/demo/existing"},
	}, "project-upload-existing-destination-seed")
	if err != nil {
		t.Fatalf("create metadata-only destination seed: %v", err)
	}
	if len(seed.TransferBlocks) != 0 {
		t.Fatalf("destination seed unexpectedly planned content blocks: %#v", seed.TransferBlocks)
	}
	if _, err := seedService.CommitSyncSession(ctx, portal, workspaceValue.ID, seed.ID, "project-upload-existing-destination-seed-commit"); err != nil {
		t.Fatalf("commit metadata-only destination seed: %v", err)
	}
	workspaceBefore, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}

	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, portal, hostValue.ID, "demo", "project-upload-existing-destination-folder")
	if err != nil {
		t.Fatalf("register project folder over existing destination: %v", err)
	}
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: syncToken.ID,
		ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	uploadService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := uploadService.CreateSyncSession(ctx, device, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
	}, "project-upload-existing-destination-create")
	if err != nil {
		t.Fatalf("create first upload against existing destination: %v", err)
	}
	if planned.FolderBaselineRevision != nil || len(planned.TransferBlocks) != 0 || planned.PlannedBytes != 0 ||
		len(planned.Conflicts) != 1 || planned.Conflicts[0].Path != "project/demo/existing" || planned.Conflicts[0].Reason != "destination_exists" {
		t.Fatalf("first upload plan for existing destination = %#v; want metadata-only destination conflict", planned)
	}
	result, err := uploadService.CommitSyncSession(ctx, device, workspaceValue.ID, planned.ID, "project-upload-existing-destination-commit")
	if err != nil || result.State != workspace.SyncConflict {
		t.Fatalf("first upload result = %#v, err=%v; want conflict", result, err)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.Revision != workspaceBefore.Revision {
		t.Fatalf("workspace revision after first-upload conflict = %d, err=%v; want unchanged revision %d", workspaceAfter.Revision, err, workspaceBefore.Revision)
	}
	folderAfter, err := store.GetWorkspaceSyncFolder(ctx, device, workspaceValue.ID, folder.FolderID)
	if err != nil || folderAfter.LastUploadRevision != nil {
		t.Fatalf("project baseline after first-upload conflict = %#v, err=%v; want unset", folderAfter.LastUploadRevision, err)
	}
}
