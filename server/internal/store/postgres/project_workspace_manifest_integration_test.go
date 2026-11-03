package postgres

import (
	"context"
	"sort"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
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
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "project-file-view")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID})
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`, hostValue.ID, workspaceValue.ID); err != nil {
		t.Fatal(err)
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
		syncTestFile(".ssh/id_rsa", []byte("private")),
		syncTestFile("docs/visible.md", []byte("visible")),
		syncTestFile("private/.env", []byte("secret")),
		syncTestFile("notes/readme.md", []byte("read me")),
	}
	session := newTestSyncSession(t, workspaceValue, owner.ID, files, files, nil)
	session.EmptyDirectories = []string{".aws", "docs/empty"}
	session.UploadEmptyDirectories = []string{".aws", "docs/empty"}
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(files),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "project-files-view:create", session.ID)); err != nil {
		t.Fatalf("create Workspace snapshot: %v", err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "project-files-view:commit", session.ID)); err != nil {
		t.Fatalf("commit Workspace snapshot: %v", err)
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
	want := []string{"docs/empty", "docs/visible.md", "notes/readme.md"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("project file paths = %v, want %v", paths, want)
	}
	if strings.Contains(strings.Join(paths, ","), ".ssh") || strings.Contains(strings.Join(paths, ","), ".env") {
		t.Fatalf("project file paths contain restricted paths: %v", paths)
	}
}
