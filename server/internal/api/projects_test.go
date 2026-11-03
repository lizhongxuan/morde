package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectRoutesStub struct {
	projectRoutesService
	principal     identity.Principal
	create        app.CreateProjectCommand
	created       project.Project
	createReplay  bool
	createCalls   int
	invite        app.InviteProjectMemberCommand
	inviteCalls   int
	revokeID      string
	revokeVersion int64
	revokeKey     string
	revokeCalls   int
	page          app.PageRequest
	listCalls     int
	pageResult    app.ProjectPage
	manifest      app.ProjectWorkspaceFilePage
	manifestArgs  struct {
		projectID   string
		workspaceID string
		revision    int64
		prefix      string
		cursor      string
		limit       int32
	}
	manifestCalls int
}

func (s *projectRoutesStub) Create(_ context.Context, principal identity.Principal, command app.CreateProjectCommand) (project.Project, bool, error) {
	s.createCalls++
	s.principal, s.create = principal, command
	return s.created, s.createReplay, nil
}

func (s *projectRoutesStub) List(_ context.Context, principal identity.Principal, page app.PageRequest) (app.ProjectPage, error) {
	s.listCalls++
	s.principal, s.page = principal, page
	return s.pageResult, nil
}

func (s *projectRoutesStub) GetWorkspaceManifest(_ context.Context, principal identity.Principal, projectID, workspaceID string, revision int64, prefix, cursor string, limit int32) (app.ProjectWorkspaceFilePage, error) {
	s.principal = principal
	s.manifestArgs.projectID, s.manifestArgs.workspaceID = projectID, workspaceID
	s.manifestArgs.revision, s.manifestArgs.prefix, s.manifestArgs.cursor, s.manifestArgs.limit = revision, prefix, cursor, limit
	s.manifestCalls++
	return s.manifest, nil
}

func (s *projectRoutesStub) Invite(_ context.Context, principal identity.Principal, _ string, command app.InviteProjectMemberCommand) (project.Invitation, bool, error) {
	s.inviteCalls++
	s.principal, s.invite = principal, command
	return project.Invitation{ID: "r1234567", ProjectID: "j1234567", InviterID: principal.UserID, InviteeID: command.UserID, Role: command.Role, State: project.InvitationPending, Version: 1}, false, nil
}

func (s *projectRoutesStub) RevokeInvitation(_ context.Context, principal identity.Principal, _, invitationID string, expectedVersion int64, key string) error {
	s.principal, s.revokeID, s.revokeVersion, s.revokeKey = principal, invitationID, expectedVersion, key
	s.revokeCalls++
	return nil
}

func projectAPIRouter(principal *identity.Principal, service projectRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectRoutes(router.Group("/api"), service)
	return router
}

func projectAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestProjectRoutesListAndCreateForwardPrincipalAndIdempotency(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectRoutesStub{
		pageResult: app.ProjectPage{Items: []project.Project{}},
		created:    project.Project{ID: "j1234567", OwnerID: principal.UserID, Name: "Shared", Version: 1},
	}
	router := projectAPIRouter(&principal, service)
	listed := projectAPIRequest(router, http.MethodGet, "/api/projects?limit=4&cursor=before", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"items":[]`) || service.page != (app.PageRequest{Limit: 4, Cursor: "before"}) ||
		!reflect.DeepEqual(service.principal, principal) || service.listCalls != 1 {
		t.Fatalf("list = %d %s; page=%+v principal=%+v calls=%d", listed.Code, listed.Body, service.page, service.principal, service.listCalls)
	}
	created := projectAPIRequest(router, http.MethodPost, "/api/projects", `{"name":"Shared","description":"Team background"}`, "create-key-01")
	if created.Code != http.StatusCreated || service.createCalls != 1 || !reflect.DeepEqual(service.principal, principal) ||
		service.create != (app.CreateProjectCommand{Name: "Shared", Description: "Team background", IdempotencyKey: "create-key-01"}) ||
		!strings.Contains(created.Body.String(), `"owner_id":"u1234567"`) {
		t.Fatalf("create = %d %s; command=%+v principal=%+v calls=%d", created.Code, created.Body, service.create, service.principal, service.createCalls)
	}
}

func TestProjectRoutesRequireIdentityAndVersionForInvitations(t *testing.T) {
	service := &projectRoutesStub{}
	if response := projectAPIRequest(projectAPIRouter(nil, service), http.MethodGet, "/api/projects", "", ""); response.Code != http.StatusUnauthorized || service.listCalls != 0 {
		t.Fatalf("missing principal = %d %s calls=%d", response.Code, response.Body, service.listCalls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectAPIRouter(&principal, service)
	missingVersion := projectAPIRequest(router, http.MethodPost, "/api/projects/j1234567/members", `{"user_id":"u7654321","role":"member"}`, "invite-key-01")
	if missingVersion.Code != http.StatusBadRequest || service.inviteCalls != 0 {
		t.Fatalf("missing version = %d %s calls=%d", missingVersion.Code, missingVersion.Body, service.inviteCalls)
	}
	created := projectAPIRequest(router, http.MethodPost, "/api/projects/j1234567/members", `{"user_id":"u7654321","role":"member","expected_version":3}`, "invite-key-02")
	if created.Code != http.StatusCreated || service.inviteCalls != 1 || service.invite.ExpectedVersion != 3 ||
		service.invite.IdempotencyKey != "invite-key-02" || !reflect.DeepEqual(service.principal, principal) {
		t.Fatalf("invite = %d %s command=%+v principal=%+v calls=%d", created.Code, created.Body, service.invite, service.principal, service.inviteCalls)
	}
}

func TestProjectInvitationRevokeRouteRequiresVersionAndIdempotency(t *testing.T) {
	service := &projectRoutesStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectAPIRouter(&principal, service)
	missingVersion := projectAPIRequest(router, http.MethodDelete, "/api/projects/j1234567/invitations/r1234567", `{}`, "revoke-key-01")
	if missingVersion.Code != http.StatusBadRequest || service.revokeCalls != 0 {
		t.Fatalf("missing invitation version = %d %s calls=%d", missingVersion.Code, missingVersion.Body, service.revokeCalls)
	}
	missingKey := projectAPIRequest(router, http.MethodDelete, "/api/projects/j1234567/invitations/r1234567", `{"expected_version":2}`, "")
	if missingKey.Code != http.StatusBadRequest || service.revokeCalls != 0 {
		t.Fatalf("missing idempotency key = %d %s calls=%d", missingKey.Code, missingKey.Body, service.revokeCalls)
	}
	revoked := projectAPIRequest(router, http.MethodDelete, "/api/projects/j1234567/invitations/r1234567", `{"expected_version":2}`, "revoke-key-02")
	if revoked.Code != http.StatusNoContent || service.revokeCalls != 1 || service.revokeID != "r1234567" || service.revokeVersion != 2 ||
		service.revokeKey != "revoke-key-02" || !reflect.DeepEqual(service.principal, principal) {
		t.Fatalf("revoke = %d %s; service=%+v", revoked.Code, revoked.Body, service)
	}
}

func TestProjectRoutesNilServiceReturnsUnavailable(t *testing.T) {
	response := projectAPIRequest(projectAPIRouter(nil, nil), http.MethodGet, "/api/projects", "", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil service = %d %s", response.Code, response.Body)
	}
}

func TestProjectWorkspaceManifestRequiresIdentityAndForwardsBoundedQuery(t *testing.T) {
	service := &projectRoutesStub{}
	path := "/api/projects/j1234567/workspaces/w1234567/manifest?revision=3&prefix=src&cursor=next&limit=25"
	if response := projectAPIRequest(projectAPIRouter(nil, service), http.MethodGet, path, "", ""); response.Code != http.StatusUnauthorized || service.manifestCalls != 0 {
		t.Fatalf("missing principal = %d %s calls=%d", response.Code, response.Body, service.manifestCalls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service.manifest = app.ProjectWorkspaceFilePage{
		ProjectID: "j1234567", WorkspaceID: "w1234567", Revision: 3,
		Files:            []app.ProjectWorkspaceFile{{Path: "src/main.go", Kind: "file", SizeBytes: 12, Mode: 0644}},
		EmptyDirectories: []string{},
	}
	response := projectAPIRequest(projectAPIRouter(&principal, service), http.MethodGet, path, "", "")
	if response.Code != http.StatusOK || service.manifestCalls != 1 || !reflect.DeepEqual(service.principal, principal) ||
		service.manifestArgs.projectID != "j1234567" || service.manifestArgs.workspaceID != "w1234567" ||
		service.manifestArgs.revision != 3 || service.manifestArgs.prefix != "src" || service.manifestArgs.cursor != "next" || service.manifestArgs.limit != 25 ||
		!strings.Contains(response.Body.String(), `"path":"src/main.go"`) || strings.Contains(response.Body.String(), "sha256") || strings.Contains(response.Body.String(), "chunks") {
		t.Fatalf("manifest = %d %s args=%+v principal=%+v calls=%d", response.Code, response.Body, service.manifestArgs, service.principal, service.manifestCalls)
	}
}

func TestProjectWorkspaceManifestRejectsInvalidIDsAndQuery(t *testing.T) {
	service := &projectRoutesStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectAPIRouter(&principal, service)
	for _, path := range []string{
		"/api/projects/bad/workspaces/w1234567/manifest",
		"/api/projects/j1234567/workspaces/bad/manifest",
		"/api/projects/j1234567/workspaces/w1234567/manifest?revision=-1",
		"/api/projects/j1234567/workspaces/w1234567/manifest?limit=1001",
	} {
		response := projectAPIRequest(router, http.MethodGet, path, "", "")
		if response.Code != http.StatusBadRequest || service.manifestCalls != 0 {
			t.Fatalf("path %q = %d %s calls=%d", path, response.Code, response.Body, service.manifestCalls)
		}
	}
}
