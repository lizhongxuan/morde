package api

import (
	"context"
	"encoding/json"
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

type projectContentRoutesStub struct {
	principal        identity.Principal
	projectID        string
	kind             project.ContentKind
	contentID        string
	command          app.MutateProjectContentCommand
	content          project.Content
	replay           bool
	getCalls         int
	mutateCalls      int
	searchCalls      int
	searchQuery      string
	searchLimit      int
	searchItems      []project.ContentSearchResult
	commentCalls     int
	commentListCalls int
	commentCommand   app.MutateProjectContentCommentCommand
	commentItems     []project.ContentComment
}

func (s *projectContentRoutesStub) GetContent(_ context.Context, principal identity.Principal, projectID string, kind project.ContentKind, contentID string) (project.Content, error) {
	s.getCalls++
	s.principal, s.projectID, s.kind, s.contentID = principal, projectID, kind, contentID
	return s.content, nil
}

func (s *projectContentRoutesStub) MutateContent(_ context.Context, principal identity.Principal, projectID string, command app.MutateProjectContentCommand) (project.Content, bool, error) {
	s.mutateCalls++
	s.principal, s.projectID, s.kind, s.contentID, s.command = principal, projectID, command.Kind, command.ContentID, command
	value := s.content
	value.Revision = command.BaseRevision + 1
	if command.Delete {
		value.Body = nil
	} else {
		value.Body = append(json.RawMessage(nil), command.Body...)
	}
	return value, s.replay, nil
}

func (s *projectContentRoutesStub) SearchContent(_ context.Context, _ identity.Principal, query string, limit int) ([]project.ContentSearchResult, error) {
	s.searchCalls++
	s.searchQuery, s.searchLimit = query, limit
	return append([]project.ContentSearchResult(nil), s.searchItems...), nil
}

func (s *projectContentRoutesStub) ListContentComments(_ context.Context, principal identity.Principal, projectID string, kind project.ContentKind, contentID string, revision int64, rowID string) ([]project.ContentComment, error) {
	s.commentListCalls++
	s.principal, s.projectID, s.kind, s.contentID = principal, projectID, kind, contentID
	return append([]project.ContentComment(nil), s.commentItems...), nil
}

func (s *projectContentRoutesStub) MutateContentComment(_ context.Context, principal identity.Principal, projectID string, command app.MutateProjectContentCommentCommand) (project.ContentComment, bool, error) {
	s.commentCalls++
	s.principal, s.projectID, s.kind, s.contentID, s.commentCommand = principal, projectID, command.Kind, command.ContentID, command
	value := project.ContentComment{
		ProjectID: projectID, Kind: command.Kind, ContentID: command.ContentID, ContentRevision: command.ContentRevision,
		RowID: command.RowID, ID: command.CommentID, Revision: command.BaseVersion + 1, Body: command.Body,
		Deleted: command.Delete, UpdatedBy: principal.UserID,
	}
	if value.ID == "" {
		value.ID = "c1234567"
	}
	if command.Delete {
		value.Body = ""
	}
	return value, s.replay, nil
}

func projectContentAPIRouter(principal *identity.Principal, service projectContentRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectContentRoutes(router.Group("/api"), service)
	return router
}

func projectContentAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
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

func TestProjectContentRoutesReadMutateAndReturnDeleteRevision(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectContentRoutesStub{content: project.Content{
		ProjectID: "j1234567", Kind: project.ContentMemory, ID: "global", Revision: 2,
		Body: json.RawMessage(`{"content":"shared"}`), UpdatedBy: principal.UserID,
	}}
	router := projectContentAPIRouter(&principal, service)
	path := "/api/projects/j1234567/content/memory"

	got := projectContentAPIRequest(router, http.MethodGet, path+"?content_id=global", "", "")
	var content projectContentResponse
	if err := json.Unmarshal(got.Body.Bytes(), &content); err != nil || got.Code != http.StatusOK ||
		content.ProjectID != "j1234567" || content.Kind != project.ContentMemory || content.ID != "global" ||
		content.Revision != 2 || string(content.Body) != `{"content":"shared"}` || content.UpdatedBy != principal.UserID ||
		service.getCalls != 1 || !reflect.DeepEqual(service.principal, principal) {
		t.Fatalf("GET content = %d %s; response=%#v service=%#v err=%v", got.Code, got.Body, content, service, err)
	}

	got = projectContentAPIRequest(router, http.MethodPut, path, `{"content_id":"global","base_revision":0,"body":{"content":"shared"}}`, "content-create-0001")
	if got.Code != http.StatusCreated || service.mutateCalls != 1 || service.projectID != "j1234567" ||
		!reflect.DeepEqual(service.command, app.MutateProjectContentCommand{
			Kind: project.ContentMemory, ContentID: "global", BaseRevision: 0,
			Body: json.RawMessage(`{"content":"shared"}`), IdempotencyKey: "content-create-0001",
		}) {
		t.Fatalf("PUT content = %d %s; service=%#v", got.Code, got.Body, service)
	}

	service.replay = true
	got = projectContentAPIRequest(router, http.MethodPut, path, `{"content_id":"global","base_revision":1,"body":{"content":"shared"}}`, "content-update-0001")
	if got.Code != http.StatusOK || service.mutateCalls != 2 {
		t.Fatalf("replayed PUT content = %d %s; calls=%d", got.Code, got.Body, service.mutateCalls)
	}

	service.replay = false
	got = projectContentAPIRequest(router, http.MethodDelete, path, `{"content_id":"global","base_revision":2}`, "content-delete-0001")
	var deleted projectContentDeleteResponse
	if err := json.Unmarshal(got.Body.Bytes(), &deleted); err != nil || got.Code != http.StatusOK || !deleted.Deleted ||
		deleted.Revision != 3 || service.mutateCalls != 3 || !service.command.Delete || service.command.BaseRevision != 2 ||
		service.command.Body != nil || service.command.IdempotencyKey != "content-delete-0001" {
		t.Fatalf("DELETE content = %d %s; response=%#v service=%#v err=%v", got.Code, got.Body, deleted, service, err)
	}
}

func TestProjectContentRoutesRejectInvalidInputBeforeService(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectContentRoutesStub{}
	router := projectContentAPIRouter(&principal, service)
	base := "/api/projects/j1234567/content/memory"
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
		key    string
	}{
		{name: "invalid kind", method: http.MethodGet, path: "/api/projects/j1234567/content/credential?content_id=global"},
		{name: "missing content id", method: http.MethodGet, path: base},
		{name: "duplicate content id", method: http.MethodGet, path: base + "?content_id=global&content_id=other"},
		{name: "missing revision", method: http.MethodPut, path: base, body: `{"content_id":"global","body":{"content":"x"}}`, key: "content-key-0001"},
		{name: "unknown PUT field", method: http.MethodPut, path: base, body: `{"content_id":"global","base_revision":0,"body":{"content":"x"},"owner_id":"u1234567"}`, key: "content-key-0001"},
		{name: "missing idempotency", method: http.MethodPut, path: base, body: `{"content_id":"global","base_revision":0,"body":{"content":"x"}}`},
		{name: "DELETE rejects body field", method: http.MethodDelete, path: base, body: `{"content_id":"global","base_revision":1,"body":{}}`, key: "content-key-0001"},
		{name: "missing delete revision", method: http.MethodDelete, path: base, body: `{}`, key: "content-key-0001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := projectContentAPIRequest(router, test.method, test.path, test.body, test.key)
			if got.Code != http.StatusBadRequest || service.getCalls != 0 || service.mutateCalls != 0 {
				t.Fatalf("invalid request = %d %s; get=%d mutate=%d", got.Code, got.Body, service.getCalls, service.mutateCalls)
			}
		})
	}
	if got := projectContentAPIRequest(projectContentAPIRouter(nil, service), http.MethodGet, base+"?content_id=global", "", ""); got.Code != http.StatusUnauthorized || service.getCalls != 0 {
		t.Fatalf("unauthenticated GET = %d %s calls=%d", got.Code, got.Body, service.getCalls)
	}
	if got := projectContentAPIRequest(projectContentAPIRouter(&principal, nil), http.MethodGet, base+"?content_id=global", "", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing service GET = %d %s", got.Code, got.Body)
	}
}

func TestProjectContentRouteKeepsEncodedSlashInContentID(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectContentRoutesStub{content: project.Content{
		ProjectID: "j1234567", Kind: project.ContentDocument, ID: "brief/one", Revision: 1,
		Body: json.RawMessage(`{"title":"Brief","content":"Context"}`), UpdatedBy: principal.UserID,
	}}
	got := projectContentAPIRequest(projectContentAPIRouter(&principal, service), http.MethodGet,
		"/api/projects/j1234567/content/document?content_id=brief%2Fone", "", "")
	if got.Code != http.StatusOK || service.contentID != "brief/one" || service.getCalls != 1 {
		t.Fatalf("GET encoded content ID = %d %s; contentID=%q calls=%d", got.Code, got.Body, service.contentID, service.getCalls)
	}
}

func TestProjectContentSearchAndCommentRoutesCarryRevisionAndBaseVersion(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectContentRoutesStub{
		searchItems: []project.ContentSearchResult{{ProjectID: "j1234567", Kind: project.ContentBrief, ContentID: "brief-1", Revision: 3, Title: "Weekly brief", Excerpt: "project excerpt"}},
		commentItems: []project.ContentComment{{
			ProjectID: "j1234567", Kind: project.ContentBrief, ContentID: "brief-1", ContentRevision: 3,
			RowID: "section-1", ID: "c1234567", Revision: 1, Body: "Review this row", UpdatedBy: principal.UserID,
		}},
	}
	router := projectContentAPIRouter(&principal, service)

	got := projectContentAPIRequest(router, http.MethodGet, "/api/project-content/search?q=project&limit=4", "", "")
	if got.Code != http.StatusOK || service.searchCalls != 1 || service.searchQuery != "project" || service.searchLimit != 4 || !strings.Contains(got.Body.String(), "project excerpt") {
		t.Fatalf("search = %d %s; service=%+v", got.Code, got.Body, service)
	}

	base := "/api/projects/j1234567/content/brief/comments?content_id=brief-1&content_revision=3&row_id=section-1"
	got = projectContentAPIRequest(router, http.MethodGet, base, "", "")
	if got.Code != http.StatusOK || service.commentListCalls != 1 || !strings.Contains(got.Body.String(), "Review this row") {
		t.Fatalf("list comments = %d %s; service=%+v", got.Code, got.Body, service)
	}

	got = projectContentAPIRequest(router, http.MethodPost, "/api/projects/j1234567/content/brief/comments",
		`{"content_id":"brief-1","content_revision":3,"row_id":"section-1","body":"Add a note"}`, "comment-create-01")
	if got.Code != http.StatusCreated || service.commentCalls != 1 || service.commentCommand.BaseVersion != 0 ||
		service.commentCommand.ContentRevision != 3 || service.commentCommand.RowID != "section-1" || service.commentCommand.Body != "Add a note" {
		t.Fatalf("create comment = %d %s; command=%+v", got.Code, got.Body, service.commentCommand)
	}

	got = projectContentAPIRequest(router, http.MethodPut, "/api/projects/j1234567/content/brief/comments/c1234567?content_id=brief-1&content_revision=3&row_id=section-1",
		`{"base_version":1,"body":"Updated note"}`, "comment-update-01")
	if got.Code != http.StatusOK || service.commentCalls != 2 || service.commentCommand.CommentID != "c1234567" ||
		service.commentCommand.BaseVersion != 1 || service.commentCommand.Body != "Updated note" {
		t.Fatalf("update comment = %d %s; command=%+v", got.Code, got.Body, service.commentCommand)
	}

	got = projectContentAPIRequest(router, http.MethodDelete, "/api/projects/j1234567/content/brief/comments/c1234567?content_id=brief-1&content_revision=3&row_id=section-1",
		`{"base_version":2}`, "comment-delete-01")
	if got.Code != http.StatusOK || service.commentCalls != 3 || !service.commentCommand.Delete || service.commentCommand.BaseVersion != 2 {
		t.Fatalf("delete comment = %d %s; command=%+v", got.Code, got.Body, service.commentCommand)
	}
}

func TestProjectContentSearchAndCommentRoutesRejectUnboundedOrUnpinnedRequests(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectContentRoutesStub{}
	router := projectContentAPIRouter(&principal, service)
	for _, request := range []struct{ method, path, body, key string }{
		{http.MethodGet, "/api/project-content/search?q=project&limit=999", "", ""},
		{http.MethodGet, "/api/project-content/search?q=first&q=second", "", ""},
		{http.MethodGet, "/api/projects/j1234567/content/brief/comments?content_id=brief-1&row_id=section-1", "", ""},
		{http.MethodPost, "/api/projects/j1234567/content/brief/comments", `{"content_id":"brief-1","row_id":"section-1","body":"No pin"}`, "comment-create-02"},
	} {
		got := projectContentAPIRequest(router, request.method, request.path, request.body, request.key)
		if got.Code != http.StatusBadRequest || service.searchCalls != 0 || service.commentCalls != 0 || service.commentListCalls != 0 {
			t.Fatalf("invalid request %s %s = %d %s; service=%+v", request.method, request.path, got.Code, got.Body, service)
		}
	}
}
