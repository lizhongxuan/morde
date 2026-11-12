package app

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const projectUploadTestSourceBindingID = "12345678-1234-4234-8234-123456789abc"

func TestProjectUploadIsClosedByDefault(t *testing.T) {
	store := newSyncServiceStore()
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.projectFolders[folderID] = workspace.SyncFolder{
		FolderID: folderID, WorkspaceID: store.workspace.ID, OwnerID: store.workspace.OwnerID,
		Kind: workspace.SyncFolderKindProject, ProjectName: "demo", RelativePath: "project/demo", State: "active",
	}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567",
		ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	_, err = service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		FolderID: folderID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
	}, "project-upload-closed")
	if domain.CodeOf(err) != domain.CodeWorkspaceUnavailable || len(store.sessions) != 0 {
		t.Fatalf("default project upload gate = error %v, sessions %d; want closed with no session", err, len(store.sessions))
	}
}

func TestProjectUploadGateBlocksChunkAndCommitForExistingSession(t *testing.T) {
	store := newSyncServiceStore()
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.projectFolders[folderID] = workspace.SyncFolder{
		FolderID: folderID, WorkspaceID: store.workspace.ID, OwnerID: store.workspace.OwnerID,
		Kind: workspace.SyncFolderKindProject, ProjectName: "demo", RelativePath: "project/demo", State: "active",
	}
	service, err := NewWorkspaceSyncServiceWithOptions(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40,
		WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567",
		ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	digest := strings.Repeat("a", 64)
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		FolderID: folderID, SourceBindingID: projectUploadTestSourceBindingID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		Files: []workspace.File{{Path: "main.go", SizeBytes: 1, SHA256: digest, Mode: 0o644,
			Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: 1}}}},
	}, "project-upload-disable-after-create")
	if err != nil || len(session.TransferBlocks) != 1 {
		t.Fatalf("create metadata-only project session = %#v, err=%v", session, err)
	}
	service.projectUploadEnabled = false
	_, err = service.PutSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, digest, strings.NewReader(""))
	if domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("PutSyncBlock() after gate closed = %v, want workspace unavailable", err)
	}
	_, err = service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "project-upload-commit-after-disable")
	if domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("CommitSyncSession() after gate closed = %v, want workspace unavailable", err)
	}
}

func TestProjectUploadCreateReplayUsesOriginalRequestAfterBaselineAdvances(t *testing.T) {
	store := newSyncServiceStore()
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.projectFolders[folderID] = workspace.SyncFolder{
		FolderID: folderID, WorkspaceID: store.workspace.ID, OwnerID: store.workspace.OwnerID,
		Kind: workspace.SyncFolderKindProject, ProjectName: "demo", RelativePath: "project/demo", State: "active",
	}
	service, err := NewWorkspaceSyncServiceWithOptions(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40,
		WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567",
		ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	request := workspace.CreateSyncSession{
		FolderID: folderID, SourceBindingID: projectUploadTestSourceBindingID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		Files: []workspace.File{}, EmptyDirectories: []string{"empty"},
	}
	first, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, request, "project-upload-create-retry")
	if err != nil {
		t.Fatal(err)
	}
	if first.FolderID != folderID || first.BaseRevision != 0 || first.FolderBaselineRevision != nil ||
		len(first.Scope) != 1 || first.Scope[0] != "project/demo" || store.projectFolderAccessChecks == 0 || len(store.nodeBindings) != 0 {
		t.Fatalf("first project upload session = %#v", first)
	}

	// Simulate the durable metadata left by a successful first commit. The
	// replay must still hash the original client request, not derived baseline
	// revision or server-generated deletion paths.
	revision := int64(1)
	folder := store.projectFolders[folderID]
	folder.LastUploadRevision = &revision
	store.projectFolders[folderID] = folder
	store.workspace.Revision = revision
	store.emptyDirectorySnapshots[revision] = []string{"project/demo/empty"}
	store.snapshots[revision] = []workspace.File{}
	store.directorySnapshots[revision] = []workspace.Directory{}

	replayed, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, request, "project-upload-create-retry")
	if err != nil {
		t.Fatalf("retry project upload create after baseline advance: %v", err)
	}
	if replayed.ID != first.ID || replayed.FolderID != first.FolderID || len(store.sessions) != 1 {
		t.Fatalf("project upload retry = %#v; original session %q, stored sessions %d", replayed, first.ID, len(store.sessions))
	}
}

func TestProjectUploadBaselineComesFromHostConfirmedSourceNotWorkspaceRevision(t *testing.T) {
	store := newSyncServiceStore()
	folderID := "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := "project/demo"
	store.projectFolders[folderID] = workspace.SyncFolder{
		FolderID: folderID, WorkspaceID: store.workspace.ID, OwnerID: store.workspace.OwnerID,
		Kind: workspace.SyncFolderKindProject, ProjectName: "demo", RelativePath: root, State: "active",
	}
	store.workspace.Revision = 3
	digestA, digestCloud := strings.Repeat("a", 64), strings.Repeat("c", 64)
	chunkA := workspace.Chunk{SHA256: digestA, SizeBytes: 1}
	baselineFile := workspace.File{Path: root + "/a.go", SHA256: digestA, SizeBytes: 1, Mode: 0o644, Chunks: []workspace.Chunk{chunkA}}
	cloudOnly := workspace.File{Path: root + "/cloud-only.txt", SHA256: digestCloud, SizeBytes: 1, Mode: 0o644,
		Chunks: []workspace.Chunk{{SHA256: digestCloud, SizeBytes: 1}}}
	store.projectUploadBaselines[projectUploadTestSourceBindingID] = workspace.SyncSession{
		ID: "p1234567", WorkspaceID: store.workspace.ID, OwnerID: store.workspace.OwnerID, FolderID: folderID,
		SourceBindingID: projectUploadTestSourceBindingID, Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		State: workspace.SyncCompleted, Scope: []string{root}, Files: []workspace.File{baselineFile},
		Directories: []workspace.Directory{{Path: root}},
	}
	store.snapshots[3] = []workspace.File{baselineFile, cloudOnly}
	store.directorySnapshots[3] = []workspace.Directory{{Path: root}}
	store.emptyDirectorySnapshots[3] = []string{}
	service, err := NewWorkspaceSyncServiceWithOptions(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40,
		WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567",
		ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		FolderID: folderID, SourceBindingID: projectUploadTestSourceBindingID,
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge,
		Files: []workspace.File{{Path: "a.go", SHA256: digestA, SizeBytes: 1, Mode: 0o644, Chunks: []workspace.Chunk{chunkA}}},
	}, "project-upload-keeps-cloud-only-content")
	if err != nil {
		t.Fatal(err)
	}
	if session.BaseRevision != store.workspace.Revision || session.SourceBaselineSessionID != "p1234567" ||
		len(session.DeletedPaths) != 0 || len(session.UploadDeletedPaths) != 0 || len(session.UploadFiles) != 0 {
		t.Fatalf("project upload treated a Workspace-only file as source-owned: %#v", session)
	}
}

func TestProjectDestinationConflictAllowsEmptyRegisteredRoot(t *testing.T) {
	root := "project/demo"
	command := workspace.CreateSyncSession{Scope: []string{root}, Files: []workspace.File{}}
	rootOnly := []workspace.Directory{{Path: "project"}, {Path: root}}
	if hasProjectDestinationContents(root, nil, nil, rootOnly) {
		t.Fatal("an empty registered project root was treated as existing project content")
	}
	plan := projectDestinationConflictPlan(command, nil, nil, rootOnly)
	if len(plan.Conflicts) != 0 {
		t.Fatalf("project parent and empty root conflicts = %#v", plan.Conflicts)
	}

	withNestedDirectory := append(rootOnly, workspace.Directory{Path: root + "/existing"})
	if !hasProjectDestinationContents(root, nil, nil, withNestedDirectory) {
		t.Fatal("existing project subdirectory was not treated as destination content")
	}
	plan = projectDestinationConflictPlan(command, nil, nil, withNestedDirectory)
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != root+"/existing" || plan.Conflicts[0].Reason != "destination_exists" {
		t.Fatalf("existing project subdirectory conflicts = %#v", plan.Conflicts)
	}
}
