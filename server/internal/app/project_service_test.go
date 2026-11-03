package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type projectServiceStoreStub struct {
	ProjectStore
	members           map[string]project.Member
	memberCalls       int
	invitation        project.Invitation
	inviteCalls       int
	inviteVersion     int64
	inviteKey         string
	revokeCalls       int
	revokeID          string
	revokeVersion     int64
	revokeKey         string
	content           project.Content
	contextItems      []project.Content
	contextOmitted    bool
	contextPrincipal  identity.Principal
	contextTask       task.Task
	contextLimit      int
	contextBodyLimit  int
	contentCalls      int
	contentBase       int64
	contentDelete     bool
	contentKey        string
	transferCalls     int
	transferTarget    string
	searchResults     []project.ContentSearchResult
	searchCalls       int
	searchActor       string
	searchQuery       string
	searchLimit       int
	comments          []project.ContentComment
	comment           project.ContentComment
	commentBase       int64
	commentDeleted    bool
	commentKey        string
	commentListCalls  int
	commentWriteCalls int
}

func (s *projectServiceStoreStub) GetProjectMember(_ context.Context, projectID, userID string) (project.Member, error) {
	s.memberCalls++
	value, ok := s.members[userID]
	if !ok || value.ProjectID != projectID {
		return project.Member{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	return value, nil
}

func (s *projectServiceStoreStub) ListProjectAgentContext(_ context.Context, principal identity.Principal, taskValue task.Task, limit, maxBodyBytes int) ([]project.Content, bool, error) {
	s.contextPrincipal, s.contextTask, s.contextLimit, s.contextBodyLimit = principal, taskValue, limit, maxBodyBytes
	return append([]project.Content(nil), s.contextItems...), s.contextOmitted, nil
}

func (s *projectServiceStoreStub) CreateProjectInvitation(_ context.Context, _ string, value project.Invitation, expectedVersion int64, key string) (project.Invitation, bool, error) {
	s.inviteCalls++
	s.invitation, s.inviteVersion, s.inviteKey = value, expectedVersion, key
	return value, false, nil
}

func (s *projectServiceStoreStub) RevokeProjectInvitation(_ context.Context, _, _, invitationID string, expectedVersion int64, key string) error {
	s.revokeCalls++
	s.revokeID, s.revokeVersion, s.revokeKey = invitationID, expectedVersion, key
	return nil
}

func (s *projectServiceStoreStub) ApplyProjectContentMutation(_ context.Context, _ string, value project.Content, baseRevision int64, delete bool, key string) (project.Content, bool, error) {
	s.contentCalls++
	s.content, s.contentBase, s.contentDelete, s.contentKey = value, baseRevision, delete, key
	return value, false, nil
}

func (s *projectServiceStoreStub) TransferProjectOwner(_ context.Context, _, projectID, targetID string, _ int64, _ string) (project.Project, error) {
	s.transferCalls++
	s.transferTarget = targetID
	return project.Project{ID: projectID, OwnerID: targetID, Name: "Shared", Version: 3}, nil
}

func (s *projectServiceStoreStub) SearchProjectContent(_ context.Context, actorID, query string, limit int) ([]project.ContentSearchResult, error) {
	s.searchCalls++
	s.searchActor, s.searchQuery, s.searchLimit = actorID, query, limit
	return append([]project.ContentSearchResult(nil), s.searchResults...), nil
}

func (s *projectServiceStoreStub) ListProjectContentComments(_ context.Context, actorID, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID string, limit int) ([]project.ContentComment, error) {
	s.commentListCalls++
	return append([]project.ContentComment(nil), s.comments...), nil
}

func (s *projectServiceStoreStub) ApplyProjectContentCommentMutation(_ context.Context, actorID string, value project.ContentComment, baseVersion int64, deleted bool, key string) (project.ContentComment, bool, error) {
	s.commentWriteCalls++
	s.comment, s.commentBase, s.commentDeleted, s.commentKey = value, baseVersion, deleted, key
	return value, false, nil
}

func TestProjectServiceRequireAccessReadsCurrentMembershipAndRole(t *testing.T) {
	store := &projectServiceStoreStub{members: map[string]project.Member{
		"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleMember, Version: 1},
	}}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := projectPrincipal("u1234567")
	if _, err := service.RequireAccess(context.Background(), principal, "j1234567", project.RoleViewer); err != nil {
		t.Fatalf("member read access = %v", err)
	}
	if store.memberCalls != 1 {
		t.Fatalf("membership query count = %d", store.memberCalls)
	}
	store.members[principal.UserID] = project.Member{ProjectID: "j1234567", UserID: principal.UserID, Role: project.RoleViewer, Version: 2}
	if _, err := service.RequireAccess(context.Background(), principal, "j1234567", project.RoleMember); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revoked write access error = %v", err)
	}
	if store.memberCalls != 2 {
		t.Fatalf("membership was not reloaded for the next request: calls=%d", store.memberCalls)
	}
	if _, err := service.RequireAccess(context.Background(), principal, "j7654321", project.RoleViewer); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-project access error = %v", err)
	}
	apiKey := principal
	apiKey.Kind = identity.KindAPIKey
	if _, err := service.RequireAccess(context.Background(), apiKey, "j1234567", project.RoleViewer); domain.CodeOf(err) != domain.CodeForbidden || store.memberCalls != 3 {
		t.Fatalf("API key project access error=%v membership calls=%d", err, store.memberCalls)
	}
}

func TestProjectServiceAgentContextRequiresCurrentViewerMembershipAndIsBounded(t *testing.T) {
	principal := projectPrincipal("u1234567")
	contextTask := projectContextTask(principal.UserID)
	store := &projectServiceStoreStub{
		members: map[string]project.Member{
			"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
		},
		contextItems:   []project.Content{{ProjectID: "j1234567", Kind: project.ContentMemory, ID: "global", Revision: 3}},
		contextOmitted: true,
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	items, omitted, err := service.AgentContext(context.Background(), principal, contextTask)
	if err != nil || !omitted || len(items) != 1 || store.contextPrincipal.UserID != principal.UserID ||
		store.contextPrincipal.TokenID != principal.TokenID || store.contextTask.ID != contextTask.ID ||
		store.contextLimit != projectAgentContextItems || store.contextBodyLimit != projectAgentContextObjectSize {
		t.Fatalf("AgentContext() = %#v omitted=%t err=%v store=%+v", items, omitted, err, store)
	}
	if _, _, err := service.AgentContext(context.Background(), projectPrincipal("u2234567"), contextTask); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("nonmember AgentContext() error = %v", err)
	}
}

func projectContextTask(initiatorID string) task.Task {
	expiresAt := time.Now().Add(time.Hour)
	return task.Task{
		ID: "q1234567", OwnerID: "u1234567", ProjectID: "j1234567", InitiatorID: initiatorID, ConversationID: "c1234567", ExecutionLocation: "server",
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
			Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}, ExpiresAt: &expiresAt},
		ConversationContext: &task.ConversationContext{OwnerID: "u1234567", ProjectID: "j1234567", InitiatorID: initiatorID,
			ConversationID: "c1234567", Scope: "project"},
	}
}

func TestProjectServiceSearchFiltersRevokedAndUnauthorizedProjects(t *testing.T) {
	store := &projectServiceStoreStub{
		members: map[string]project.Member{
			"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
		},
		searchResults: []project.ContentSearchResult{
			{ProjectID: "j1234567", Kind: project.ContentBrief, ContentID: "brief-1", Revision: 2, Title: "Visible", Excerpt: "visible excerpt"},
			{ProjectID: "j7654321", Kind: project.ContentBrief, ContentID: "brief-hidden", Revision: 1, Title: "Hidden", Excerpt: "must never return"},
		},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := service.SearchContent(context.Background(), projectPrincipal("u1234567"), "visible", 7)
	if err != nil || len(results) != 1 || results[0].ProjectID != "j1234567" || store.searchCalls != 1 ||
		store.searchActor != "u1234567" || store.searchQuery != "visible" || store.searchLimit != 7 {
		t.Fatalf("SearchContent() = %#v, err=%v, store=%+v", results, err, store)
	}
	if _, err := service.SearchContent(context.Background(), projectPrincipal("u1234567"), " ", 7); domain.CodeOf(err) != domain.CodeInvalidArgument || store.searchCalls != 1 {
		t.Fatalf("invalid search was dispatched: err=%v calls=%d", err, store.searchCalls)
	}
	if _, err := service.SearchContent(context.Background(), projectPrincipal("u1234567"), "visible", project.MaxContentSearchResults+1); domain.CodeOf(err) != domain.CodeInvalidArgument || store.searchCalls != 1 {
		t.Fatalf("unbounded search was dispatched: err=%v calls=%d", err, store.searchCalls)
	}
}

func TestProjectServiceContentCommentsPinRevisionAndRequireMemberToWrite(t *testing.T) {
	store := &projectServiceStoreStub{
		members: map[string]project.Member{
			"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleMember, Version: 1},
		},
	}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := projectPrincipal("u1234567")
	value, replay, err := service.MutateContentComment(context.Background(), principal, "j1234567", MutateProjectContentCommentCommand{
		Kind: project.ContentBrief, ContentID: "brief-1", ContentRevision: 4, RowID: "section-2", BaseVersion: 0,
		Body: "Check the owner and acceptance scope.", IdempotencyKey: "comment-create-01",
	})
	if err != nil || replay || value.ContentRevision != 4 || value.Revision != 1 || !project.ValidCommentID(value.ID) ||
		store.commentWriteCalls != 1 || store.commentBase != 0 || store.comment.ID != value.ID {
		t.Fatalf("MutateContentComment(create) = %#v replay=%t err=%v; store=%+v", value, replay, err, store)
	}
	_, _, err = service.MutateContentComment(context.Background(), principal, "j1234567", MutateProjectContentCommentCommand{
		Kind: project.ContentBrief, ContentID: "brief-1", ContentRevision: 4, RowID: "section-2", CommentID: value.ID,
		BaseVersion: 1, Body: "Updated annotation.", IdempotencyKey: "comment-update-01",
	})
	if err != nil || store.commentBase != 1 || store.comment.Revision != 2 || store.comment.ContentRevision != 4 {
		t.Fatalf("MutateContentComment(update) = %+v err=%v; store=%+v", store.comment, err, store)
	}
	store.members[principal.UserID] = project.Member{ProjectID: "j1234567", UserID: principal.UserID, Role: project.RoleViewer, Version: 2}
	if _, _, err := service.MutateContentComment(context.Background(), principal, "j1234567", MutateProjectContentCommentCommand{
		Kind: project.ContentBrief, ContentID: "brief-1", ContentRevision: 4, RowID: "section-2", CommentID: value.ID,
		BaseVersion: 2, Body: "No longer writable.", IdempotencyKey: "comment-update-02",
	}); domain.CodeOf(err) != domain.CodeForbidden || store.commentWriteCalls != 2 {
		t.Fatalf("viewer comment write = %v, calls=%d", err, store.commentWriteCalls)
	}
}

func TestProjectServiceInvitationAndContentMutationsUseCurrentOwnerAndMember(t *testing.T) {
	store := &projectServiceStoreStub{members: map[string]project.Member{
		"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleOwner, Version: 1},
	}}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := projectPrincipal("u1234567")
	invitation, replay, err := service.Invite(context.Background(), principal, "j1234567", InviteProjectMemberCommand{
		UserID: "u7654321", Role: project.RoleViewer, ExpectedVersion: 7, IdempotencyKey: "invite-key-01",
	})
	if err != nil || replay || invitation.State != project.InvitationPending || invitation.InviteeID != "u7654321" ||
		store.inviteCalls != 1 || store.inviteVersion != 7 || store.inviteKey != "invite-key-01" {
		t.Fatalf("Invite() = %+v, replay=%t, err=%v; store=%+v", invitation, replay, err, store)
	}

	store.members[principal.UserID] = project.Member{ProjectID: "j1234567", UserID: principal.UserID, Role: project.RoleMember, Version: 2}
	if _, _, err := service.Invite(context.Background(), principal, "j1234567", InviteProjectMemberCommand{
		UserID: "u7654321", Role: project.RoleViewer, ExpectedVersion: 7, IdempotencyKey: "invite-key-02",
	}); domain.CodeOf(err) != domain.CodeForbidden || store.inviteCalls != 1 {
		t.Fatalf("member invitation error=%v calls=%d", err, store.inviteCalls)
	}

	content, replay, err := service.MutateContent(context.Background(), principal, "j1234567", MutateProjectContentCommand{
		Kind: project.ContentMemory, ContentID: "m_0123abcd", BaseRevision: 0,
		Body: json.RawMessage(`{"content":"Shared context"}`), IdempotencyKey: "content-key-01",
	})
	if err != nil || replay || content.UpdatedBy != principal.UserID || store.contentCalls != 1 || store.contentBase != 0 ||
		store.contentDelete || store.contentKey != "content-key-01" {
		t.Fatalf("MutateContent() = %+v, replay=%t, err=%v; store=%+v", content, replay, err, store)
	}
	if _, _, err := service.MutateContent(context.Background(), principal, "j1234567", MutateProjectContentCommand{
		Kind: project.ContentMemory, ContentID: "m_0123abcd", BaseRevision: 1,
		Body: json.RawMessage(`{"content":"Shared","connector_secret":"secret"}`), IdempotencyKey: "content-key-02",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument || store.contentCalls != 1 {
		t.Fatalf("secret content error=%v calls=%d", err, store.contentCalls)
	}
	if _, _, err := service.MutateContent(context.Background(), principal, "j1234567", MutateProjectContentCommand{
		Kind: project.ContentDocument, ContentID: "brief-1", BaseRevision: 0,
		Body: json.RawMessage(`{"title":"Brief","content":"` + strings.Repeat("x", 1<<20) + `"}`), IdempotencyKey: "content-key-03",
	}); domain.CodeOf(err) != domain.CodeRequestTooLarge || store.contentCalls != 1 {
		t.Fatalf("oversize content error=%v calls=%d", err, store.contentCalls)
	}
}

func TestProjectServiceOwnerTransferRequiresAnAcceptedMember(t *testing.T) {
	store := &projectServiceStoreStub{members: map[string]project.Member{
		"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleOwner, Version: 2},
		"u7654321": {ProjectID: "j1234567", UserID: "u7654321", Role: project.RoleViewer, Version: 1},
	}}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	value, err := service.TransferOwner(context.Background(), projectPrincipal("u1234567"), "j1234567", "u7654321", 2, "transfer-key-01")
	if err != nil || value.OwnerID != "u7654321" || store.transferCalls != 1 || store.transferTarget != "u7654321" {
		t.Fatalf("TransferOwner() = %+v, %v; calls=%d target=%q", value, err, store.transferCalls, store.transferTarget)
	}
	if _, err := service.TransferOwner(context.Background(), projectPrincipal("u1234567"), "j1234567", "u1111111", 2, "transfer-key-02"); domain.CodeOf(err) != domain.CodeNotFound || store.transferCalls != 1 {
		t.Fatalf("transfer to non-member error=%v calls=%d", err, store.transferCalls)
	}
}

func TestProjectServiceRevokeInvitationRequiresCurrentOwnerAndVersion(t *testing.T) {
	store := &projectServiceStoreStub{members: map[string]project.Member{
		"u1234567": {ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleOwner, Version: 1},
	}}
	service, err := NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := projectPrincipal("u1234567")
	if err := service.RevokeInvitation(context.Background(), principal, "j1234567", "r1234567", 2, "revoke-key-01"); err != nil ||
		store.revokeCalls != 1 || store.revokeID != "r1234567" || store.revokeVersion != 2 || store.revokeKey != "revoke-key-01" {
		t.Fatalf("RevokeInvitation() = %v; store=%+v", err, store)
	}
	store.members[principal.UserID] = project.Member{ProjectID: "j1234567", UserID: principal.UserID, Role: project.RoleMember, Version: 2}
	if err := service.RevokeInvitation(context.Background(), principal, "j1234567", "r1234567", 2, "revoke-key-02"); domain.CodeOf(err) != domain.CodeForbidden || store.revokeCalls != 1 {
		t.Fatalf("member RevokeInvitation() = %v calls=%d", err, store.revokeCalls)
	}
	store.members[principal.UserID] = project.Member{ProjectID: "j1234567", UserID: principal.UserID, Role: project.RoleOwner, Version: 3}
	if err := service.RevokeInvitation(context.Background(), principal, "j1234567", "bad", 0, "revoke-key-03"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.revokeCalls != 1 {
		t.Fatalf("invalid RevokeInvitation() = %v calls=%d", err, store.revokeCalls)
	}
}

func projectPrincipal(userID string) identity.Principal {
	return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}
