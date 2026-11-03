package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

type testHostWorkspaceFileService struct {
	bindings  []workspace.Binding
	page      workspace.ManifestPage
	file      workspace.File
	body      []byte
	listErr   error
	pageErr   error
	stream    func(func(workspace.File, []byte) error) error
	listCall  int
	pageCall  int
	fileCall  int
	actorID   string
	hostID    string
	spaceID   string
	revision  int64
	path      string
	diffErr   error
	diffCalls int
}

func (s *testHostWorkspaceFileService) GetHostDiff(_ context.Context, actorID, hostID, workspaceID string, fromRevision, toRevision int64) (app.WorkspaceDiff, error) {
	s.diffCalls++
	s.actorID, s.hostID, s.spaceID = actorID, hostID, workspaceID
	s.revision = toRevision
	if s.diffErr != nil {
		return app.WorkspaceDiff{}, s.diffErr
	}
	return app.WorkspaceDiff{WorkspaceID: workspaceID, FromRevision: fromRevision, ToRevision: toRevision, Changes: []app.WorkspaceDiffChange{}}, nil
}

func (s *testHostWorkspaceFileService) ListHostWorkspaceBindings(_ context.Context, actorID, hostID string) ([]workspace.Binding, error) {
	s.listCall++
	s.actorID, s.hostID = actorID, hostID
	return s.bindings, s.listErr
}

func (s *testHostWorkspaceFileService) GetHostWorkspaceManifest(_ context.Context, actorID, hostID, workspaceID string, revision int64, _, _ string, _ int32) (workspace.ManifestPage, error) {
	s.pageCall++
	s.actorID, s.hostID, s.spaceID, s.revision = actorID, hostID, workspaceID, revision
	return s.page, s.pageErr
}

func (s *testHostWorkspaceFileService) StreamHostWorkspaceFile(_ context.Context, actorID, hostID, workspaceID string, revision int64, relativePath string, write func(workspace.File, []byte) error) (workspace.File, error) {
	s.fileCall++
	s.actorID, s.hostID, s.spaceID, s.revision, s.path = actorID, hostID, workspaceID, revision, relativePath
	if s.stream != nil {
		return s.file, s.stream(write)
	}
	return s.file, write(s.file, s.body)
}

func TestHostWorkspaceGatewayMemberUsesOnlyExplicitFilePermission(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	files := g.deps.WorkspaceFiles.(*testHostWorkspaceFileService)
	files.bindings = []workspace.Binding{{
		HostID: testHostID, OwnerID: testUserID,
		Workspace: workspace.Workspace{ID: "w1234567", OwnerID: testUserID, Name: "project", State: workspace.StateReady, Revision: 8, UsedBytes: 42, QuotaBytes: 1024},
		State:     workspace.BindingActive, BindingVersion: 3, DesiredRevision: 8, AppliedRevision: 8,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	}}
	files.page = workspace.ManifestPage{WorkspaceID: "w1234567", Revision: 8, Files: []workspace.File{}, EmptyDirectories: []string{}}
	memberCookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	setMemberPermissionGrants(g, host.PermissionWorkspaceFileRead)

	bindings := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces", nil, hostCookieName+"="+memberCookie, "")
	if bindings.Code != http.StatusOK || !strings.Contains(bindings.Body.String(), `"applied_revision":8`) || files.listCall != 1 || files.actorID != testMemberID {
		t.Fatalf("workspace binding response = %d %q calls=%d actor=%s", bindings.Code, bindings.Body.String(), files.listCall, files.actorID)
	}
	manifest := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces/w1234567/manifest?revision=8&limit=100", nil, hostCookieName+"="+memberCookie, "")
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), `"revision":8`) || files.pageCall != 1 || files.spaceID != "w1234567" || files.revision != 8 {
		t.Fatalf("workspace manifest response = %d %q calls=%d", manifest.Code, manifest.Body.String(), files.pageCall)
	}
	workbench := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/?tab=workspace", nil, hostCookieName+"="+memberCookie, "")
	if workbench.Code != http.StatusOK || !strings.Contains(workbench.Body.String(), "workbench") {
		t.Fatalf("workspace-only Workbench entry = %d %q", workbench.Code, workbench.Body.String())
	}
}

func TestHostWorkspaceDiffIsOwnerOnlyAndUsesBoundedReadService(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	files := g.deps.WorkspaceFiles.(*testHostWorkspaceFileService)
	ownerCookie := hostCookie(g, testHostID, testUserID, time.Hour)
	ownerResponse := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces/w1234567/diff?from_revision=2&to_revision=4", nil, hostCookieName+"="+ownerCookie, "")
	if ownerResponse.Code != http.StatusOK || files.diffCalls != 1 || files.actorID != testUserID || files.spaceID != "w1234567" || files.revision != 4 {
		t.Fatalf("owner diff = %d %s calls=%d actor=%s", ownerResponse.Code, ownerResponse.Body, files.diffCalls, files.actorID)
	}
	if !strings.Contains(ownerResponse.Body.String(), `"from_revision":2`) || ownerResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("diff response body/header = %s %#v", ownerResponse.Body, ownerResponse.Header())
	}
	memberCookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	setMemberPermissionGrants(g, host.PermissionWorkspaceFileRead)
	memberResponse := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces/w1234567/diff?from_revision=2&to_revision=4", nil, hostCookieName+"="+memberCookie, "")
	if memberResponse.Code != http.StatusNotFound || files.diffCalls != 1 {
		t.Fatalf("workspace:file:read member diff = %d %s calls=%d, want denied before service", memberResponse.Code, memberResponse.Body, files.diffCalls)
	}
	badQuery := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces/w1234567/diff?from_revision=2&to_revision=4&extra=1", nil, hostCookieName+"="+ownerCookie, "")
	if badQuery.Code != http.StatusBadRequest || files.diffCalls != 1 {
		t.Fatalf("invalid diff query = %d %s calls=%d", badQuery.Code, badQuery.Body, files.diffCalls)
	}
}

func TestHostWorkspaceGatewayRejectsDesktopPermissionWithoutFileGrant(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	files := g.deps.WorkspaceFiles.(*testHostWorkspaceFileService)
	memberCookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	setMemberPermissionGrants(g, host.PermissionDesktopView)

	for _, requestPath := range []string{
		"/__ch/api/workspaces",
		"/__ch/api/workspaces/w1234567/manifest?revision=8",
		"/__ch/api/workspaces/w1234567/files/readme.md?revision=8",
		"/__ch/wb/?tab=workspace",
	} {
		response := request(handler, http.MethodGet, hostAuthority(), requestPath, nil, hostCookieName+"="+memberCookie, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("desktop-only member request %s = %d %q, want not found", requestPath, response.Code, response.Body.String())
		}
	}
	if files.listCall != 0 || files.pageCall != 0 || files.fileCall != 0 {
		t.Fatalf("unauthorized member reached Workspace service: list=%d manifest=%d file=%d", files.listCall, files.pageCall, files.fileCall)
	}
}

func TestHostWorkspaceGatewayStreamsAttachmentAndStopsAfterRevocation(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	files := g.deps.WorkspaceFiles.(*testHostWorkspaceFileService)
	files.file = workspace.File{Path: "docs/readme.txt", SizeBytes: 11}
	files.stream = func(write func(workspace.File, []byte) error) error {
		if err := write(files.file, []byte("first")); err != nil {
			return err
		}
		sharing := g.deps.HostSharing.(*testHostSharingService)
		sharing.mu.Lock()
		sharing.permissions[host.PermissionWorkspaceFileRead] = false
		sharing.mu.Unlock()
		return write(files.file, []byte("second"))
	}
	memberCookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	setMemberPermissionGrants(g, host.PermissionWorkspaceFileRead)
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces/w1234567/files/docs/readme.txt?revision=8", nil, hostCookieName+"="+memberCookie, "")
	if response.Code != http.StatusOK || response.Body.String() != "first" || response.Header().Get("Content-Length") != "11" ||
		response.Header().Get("Content-Disposition") != `attachment; filename=readme.txt` || files.path != "docs/readme.txt" {
		t.Fatalf("revoked file response = %d headers=%v body=%q path=%q", response.Code, response.Header(), response.Body.String(), files.path)
	}
}

func TestWorkspaceMemberGatewayAllowlistRejectsUnknownOrMalformedPaths(t *testing.T) {
	cases := []struct {
		method string
		path   string
		allow  bool
	}{
		{http.MethodGet, "/__ch/api/workspaces", true},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/manifest", false},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/manifest?revision=2&cursor=abc", true},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/files/src/main.go?revision=2", true},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/manifest?revision=2&revision=3", false},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/files/../secret?revision=2", false},
		{http.MethodPost, "/__ch/api/workspaces/w1234567/files/src/main.go?revision=2", false},
		{http.MethodGet, "/__ch/api/workspaces/w1234567/other", false},
	}
	for _, test := range cases {
		request, err := http.NewRequest(test.method, "https://host.example"+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		permissions, allowed := memberGatewayPermissions(request, ParsedHost{})
		if allowed != test.allow {
			t.Errorf("memberGatewayPermissions(%s %s) allowed=%t permissions=%v, want %t", test.method, test.path, allowed, permissions, test.allow)
		}
	}
}

func TestHostWorkspaceGatewayReturnsServiceErrorsBeforeStreaming(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	files := g.deps.WorkspaceFiles.(*testHostWorkspaceFileService)
	files.listErr = errors.New("workspace store unavailable")
	ownerCookie := hostCookie(g, testHostID, testUserID, time.Hour)
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/workspaces", nil, hostCookieName+"="+ownerCookie, "")
	if response.Code != http.StatusInternalServerError || files.listCall != 1 {
		t.Fatalf("Workspace service failure = %d %q calls=%d", response.Code, response.Body.String(), files.listCall)
	}
}
