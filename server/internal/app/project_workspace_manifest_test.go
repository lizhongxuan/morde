package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

type projectWorkspaceManifestTestStore struct {
	ProjectStore
	member      project.Member
	manifest    workspace.ManifestPage
	actorID     string
	projectID   string
	workspaceID string
	revision    int64
	prefix      string
	after       string
	limit       int32
	calls       int
}

func (s *projectWorkspaceManifestTestStore) GetProjectMember(_ context.Context, projectID, userID string) (project.Member, error) {
	if s.member.ProjectID != projectID || s.member.UserID != userID {
		return project.Member{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	return s.member, nil
}

func (s *projectWorkspaceManifestTestStore) GetProjectWorkspaceManifestPage(_ context.Context, actorID, projectID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	s.calls++
	s.actorID, s.projectID, s.workspaceID, s.revision, s.prefix, s.after, s.limit = actorID, projectID, workspaceID, revision, prefix, after, limit
	return s.manifest, nil
}

func TestProjectServiceWorkspaceManifestReturnsSanitizedConfirmedPage(t *testing.T) {
	cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: 3, Prefix: "src", After: "src/a.go"})
	content := []byte("workspace contents not shown")
	fileDigest := sha256.Sum256(content)
	digest := hex.EncodeToString(fileDigest[:])
	store := &projectWorkspaceManifestTestStore{
		member: project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
		manifest: workspace.ManifestPage{
			WorkspaceID: "w1234567", Revision: 3,
			Files: []workspace.File{{Path: "src/b.go", SizeBytes: int64(len(content)), SHA256: digest, Mode: 0640,
				Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: int64(len(content))}}}},
			EmptyDirectories: []string{}, NextCursor: &cursor,
		},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.GetWorkspaceManifest(context.Background(), projectPrincipal("u1234567"), "j1234567", "w1234567", 0, "src/", "", 10)
	if err != nil {
		t.Fatalf("GetWorkspaceManifest() error = %v", err)
	}
	if page.ProjectID != "j1234567" || page.WorkspaceID != "w1234567" || page.Revision != 3 || len(page.Files) != 1 || page.Files[0] != (ProjectWorkspaceFile{Path: "src/b.go", Kind: "file", SizeBytes: int64(len(content)), Mode: 0640}) {
		t.Fatalf("GetWorkspaceManifest() = %#v", page)
	}
	if store.calls != 1 || store.actorID != "u1234567" || store.projectID != "j1234567" || store.workspaceID != "w1234567" || store.revision != 0 || store.prefix != "src" || store.after != "" || store.limit != 10 {
		t.Fatalf("manifest query args = %#v, calls=%d", store, store.calls)
	}
	if page.NextCursor == nil || *page.NextCursor != cursor {
		t.Fatalf("manifest next cursor = %v, want %q", page.NextCursor, cursor)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{digest, string(content), "chunks", "sha256"} {
		if !json.Valid(encoded) || strings.Contains(string(encoded), secret) {
			t.Fatalf("sanitized project file page leaked %q: %s", secret, encoded)
		}
	}
}

func TestProjectServiceWorkspaceManifestShowsSymlinkKindWithoutTarget(t *testing.T) {
	target := "../.env"
	digest := sha256.Sum256([]byte(target))
	store := &projectWorkspaceManifestTestStore{
		member: project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
		manifest: workspace.ManifestPage{WorkspaceID: "w1234567", Revision: 3, Files: []workspace.File{{
			Path: "src/current", Kind: "symlink", SymlinkTarget: target, SizeBytes: int64(len(target)),
			SHA256: hex.EncodeToString(digest[:]), Mode: 0, Chunks: []workspace.Chunk{},
		}}, EmptyDirectories: []string{}},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.GetWorkspaceManifest(context.Background(), projectPrincipal("u1234567"), "j1234567", "w1234567", 3, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0] != (ProjectWorkspaceFile{Path: "src/current", Kind: "symlink", SizeBytes: int64(len(target)), Mode: 0}) {
		t.Fatalf("symlink project manifest = %#v, err=%v", page, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil || strings.Contains(string(encoded), target) {
		t.Fatalf("project symlink manifest leaked target %q: %s err=%v", target, encoded, err)
	}
}

func TestProjectServiceWorkspaceManifestPinsCursorAndRejectsRestrictedInputs(t *testing.T) {
	cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: 3, Prefix: "src", After: "src/a.go"})
	store := &projectWorkspaceManifestTestStore{
		member:   project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleMember, Version: 1},
		manifest: workspace.ManifestPage{WorkspaceID: "w1234567", Revision: 3, Files: []workspace.File{}, EmptyDirectories: []string{}},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetWorkspaceManifest(context.Background(), projectPrincipal("u1234567"), "j1234567", "w1234567", 0, "src", cursor, 5); err != nil {
		t.Fatalf("GetWorkspaceManifest() with cursor error = %v", err)
	}
	if store.revision != 3 || store.after != "src/a.go" {
		t.Fatalf("pinned query revision=%d after=%q", store.revision, store.after)
	}
	for _, test := range []struct {
		name     string
		revision int64
		prefix   string
		cursor   string
	}{
		{name: "revision mismatch", revision: 4, prefix: "src", cursor: cursor},
		{name: "sensitive prefix", prefix: ".ssh"},
		{name: "invalid cursor", prefix: "src", cursor: "bad"},
		{name: "oversized cursor", prefix: "src", cursor: strings.Repeat("a", 513)},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := store.calls
			_, err := service.GetWorkspaceManifest(context.Background(), projectPrincipal("u1234567"), "j1234567", "w1234567", test.revision, test.prefix, test.cursor, 5)
			if domain.CodeOf(err) != domain.CodeInvalidArgument && domain.CodeOf(err) != domain.CodeForbidden {
				t.Fatalf("GetWorkspaceManifest() error = %v", err)
			}
			if store.calls != before {
				t.Fatalf("invalid query reached store: calls %d -> %d", before, store.calls)
			}
		})
	}
}

func TestProjectServiceWorkspaceManifestFailsClosedOnRestrictedStoreData(t *testing.T) {
	store := &projectWorkspaceManifestTestStore{
		member: project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
		manifest: workspace.ManifestPage{WorkspaceID: "w1234567", Revision: 1,
			Files: []workspace.File{{Path: ".ssh/id_rsa", SizeBytes: 10, SHA256: "a", Mode: 0600}}, EmptyDirectories: []string{}},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetWorkspaceManifest(context.Background(), projectPrincipal("u1234567"), "j1234567", "w1234567", 0, "", "", 10); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("GetWorkspaceManifest() restricted store response error = %v", err)
	}

	store.member.Role = project.RoleViewer
	if _, err := service.GetWorkspaceManifest(context.Background(), identity.Principal{}, "j1234567", "w1234567", 0, "", "", 10); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("GetWorkspaceManifest() without an authenticated principal error = %v", err)
	}
}
