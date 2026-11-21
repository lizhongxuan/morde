package postgres

import (
	"context"
	"sort"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

func TestPostgresProjectWorkspaceManifestRequiresProjectAndIndependentAccess(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	viewer := createTestUser(t, store, 4, 20_000, 40_000)
	noWorkspaceAccess := createTestUser(t, store, 4, 20_000, 40_000)
	noProjectAccess := createTestUser(t, store, 4, 20_000, 40_000)
	workspaceValue, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil {
		t.Fatalf("create stable Agent Workspace: %v", err)
	}
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID})
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`, hostValue.ID, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	folderService, err := app.NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := folderService.CreateFolder(ctx, workspaceSyncPortalPrincipal(t, store, owner.ID), hostValue.ID,
		"manifest-project", "project-files-view-folder-01")
	if err != nil {
		t.Fatalf("register project folder: %v", err)
	}
	for _, userID := range []string{viewer.ID, noProjectAccess.ID} {
		if _, err := store.UpsertHostMember(ctx, host.Member{
			HostID: hostValue.ID, OwnerID: owner.ID, UserID: userID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead},
		}); err != nil {
			t.Fatalf("grant Workspace read access to %s: %v", userID, err)
		}
	}
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Workspace files", "Project-scoped file view", "project-files-view-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, viewer.ID, projectValue, project.RoleViewer, 1,
		"project-files-view-invite-01", "project-files-view-accept-01"); err != nil {
		t.Fatalf("add project viewer: %v", err)
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, noWorkspaceAccess.ID, projectValue, project.RoleViewer, currentProject.Version,
		"project-files-no-workspace-invite-01", "project-files-no-workspace-accept-01"); err != nil {
		t.Fatalf("add project viewer without Workspace access: %v", err)
	}
	currentProject, err = store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, currentProject.Version, "project-files-view-resource-01"); err != nil {
		t.Fatalf("attach Workspace to project: %v", err)
	}

	files := []workspace.File{
		syncTestFile(".ssh/id_rsa", nil),
		syncTestFile("docs/visible.md", nil),
		syncTestFile("private/.env", nil),
		syncTestFile("notes/readme.md", nil),
	}
	syncService, err := app.NewWorkspaceSyncServiceWithOptions(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30,
		app.WorkspaceSyncOptions{ProjectUploadEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, syncToken); err != nil {
		t.Fatal(err)
	}
	syncClient := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: syncToken.ID, ClientInstallationID: installationID, Scopes: []identity.Scope{identity.ScopeWorkspaceSync}}
	session, err := syncService.CreateSyncSession(ctx, syncClient, workspaceValue.ID, workspace.CreateSyncSession{
		FolderID: folder.FolderID, SourceBindingID: "12345678-1234-4234-8234-123456789abc",
		Direction: workspace.SyncUpload, Policy: workspace.SyncCompareMerge, Files: files,
		EmptyDirectories: []string{".aws", "docs/empty"},
	}, "project-files-view-upload")
	if err != nil {
		t.Fatalf("create project Workspace snapshot: %v", err)
	}
	if _, err := syncService.CommitSyncSession(ctx, syncClient, workspaceValue.ID, session.ID, "project-files-view-commit"); err != nil {
		t.Fatalf("commit project Workspace snapshot: %v", err)
	}

	if _, err := store.GetProjectWorkspaceManifestPage(ctx, noWorkspaceAccess.ID, projectValue.ID, workspaceValue.ID, 0, "", "", 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("project member without Workspace permission read manifest = %v, want not found", err)
	}
	if _, err := store.GetProjectWorkspaceManifestPage(ctx, noProjectAccess.ID, projectValue.ID, workspaceValue.ID, 0, "", "", 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Workspace member without project membership read manifest = %v, want not found", err)
	}

	var paths []string
	page, err := store.GetProjectWorkspaceManifestPage(ctx, viewer.ID, projectValue.ID, workspaceValue.ID, 0, "", "", 1)
	if err != nil {
		t.Fatalf("read first project Workspace manifest page: %v", err)
	}
	if page.Revision != 1 {
		t.Fatalf("project Workspace revision = %d, want 1", page.Revision)
	}
	for {
		for _, file := range page.Files {
			paths = append(paths, file.Path)
		}
		paths = append(paths, page.EmptyDirectories...)
		if page.NextCursor == nil {
			break
		}
		cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
		if err != nil || cursor.Revision != page.Revision || app.WorkspaceFileOperationPathIsSensitive(cursor.After) || strings.Contains(cursor.After, ".env") {
			t.Fatalf("project Workspace cursor = %#v, err=%v; cursor must not expose restricted paths", cursor, err)
		}
		page, err = store.GetProjectWorkspaceManifestPage(ctx, viewer.ID, projectValue.ID, workspaceValue.ID,
			page.Revision, "", cursor.After, 1)
		if err != nil {
			t.Fatalf("read next project Workspace manifest page: %v", err)
		}
	}
	sort.Strings(paths)
	want := []string{"project/manifest-project/docs/empty", "project/manifest-project/docs/visible.md", "project/manifest-project/notes/readme.md"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("project file paths = %v, want %v", paths, want)
	}
	if strings.Contains(strings.Join(paths, ","), ".ssh") || strings.Contains(strings.Join(paths, ","), ".env") {
		t.Fatalf("project file paths contain restricted paths: %v", paths)
	}
}
