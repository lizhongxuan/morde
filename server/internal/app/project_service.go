package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type ProjectPage struct {
	Items      []project.Project
	NextCursor string
}

type ProjectMemberPage struct {
	Items      []project.Member
	NextCursor string
}

type ProjectInvitationPage struct {
	Items      []project.Invitation
	NextCursor string
}

type ProjectResourcePage struct {
	Items      []project.Resource
	NextCursor string
}

// ProjectStore must scope reads to accepted current memberships. Mutations
// must recheck actor role, expected version, and idempotency inside the write
// transaction. Ownership transfer promotes an accepted member and demotes the
// previous owner to member atomically. AddProjectResource must verify the actor
// already has access to the resource; ListProjectResources must not reveal a
// resource ID unless the reader independently has access to it.
type ProjectStore interface {
	CreateProject(context.Context, string, project.Project, string) (project.Project, bool, error)
	GetProject(context.Context, string, string) (project.Project, error)
	ListProjects(context.Context, string, PageRequest) (ProjectPage, error)
	// GetProjectMember returns only an accepted, current membership.
	GetProjectMember(context.Context, string, string) (project.Member, error)
	ListProjectMembers(context.Context, string, string, PageRequest) (ProjectMemberPage, error)
	CreateProjectInvitation(context.Context, string, project.Invitation, int64, string) (project.Invitation, bool, error)
	RevokeProjectInvitation(context.Context, string, string, string, int64, string) error
	// ListProjectInvitations returns invitations addressed to the actor only.
	ListProjectInvitations(context.Context, string, PageRequest) (ProjectInvitationPage, error)
	AcceptProjectInvitation(context.Context, string, string, string) (project.Member, bool, error)
	UpdateProjectMemberRole(context.Context, string, string, string, project.Role, int64, string) (project.Member, error)
	RemoveProjectMember(context.Context, string, string, string, int64, string) error
	TransferProjectOwner(context.Context, string, string, string, int64, string) (project.Project, error)
	ListProjectResources(context.Context, string, string, PageRequest) (ProjectResourcePage, error)
	AddProjectResource(context.Context, string, project.Resource, int64, string) (project.Resource, bool, error)
	RemoveProjectResource(context.Context, string, string, project.ResourceKind, string, int64, string) error
	GetProjectContent(context.Context, string, string, project.ContentKind, string) (project.Content, error)
	ListProjectAgentContext(context.Context, identity.Principal, task.Task, int, int) ([]project.Content, bool, error)
	ApplyProjectContentMutation(context.Context, string, project.Content, int64, bool, string) (project.Content, bool, error)
	SearchProjectContent(context.Context, string, string, int) ([]project.ContentSearchResult, error)
	ListProjectContentComments(context.Context, string, string, project.ContentKind, string, int64, string, int) ([]project.ContentComment, error)
	ApplyProjectContentCommentMutation(context.Context, string, project.ContentComment, int64, bool, string) (project.ContentComment, bool, error)
}

const (
	projectAgentContextItems      = 32
	projectAgentContextObjectSize = 24 << 10
)

// ProjectAccessChecker is the shared membership gate for project-scoped app use cases.
type ProjectAccessChecker interface {
	RequireAccess(context.Context, identity.Principal, string, project.Role) (project.Member, error)
}

type ProjectService struct {
	store ProjectStore
}

func NewProjectService(store ProjectStore) (*ProjectService, error) {
	if store == nil {
		return nil, errors.New("project store is required")
	}
	return &ProjectService{store: store}, nil
}

func (s *ProjectService) RequireAccess(ctx context.Context, principal identity.Principal, projectID string, minimumRole project.Role) (project.Member, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return project.Member{}, err
	}
	if !project.ValidID(projectID) {
		return project.Member{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if !project.ValidRole(minimumRole) {
		return project.Member{}, domain.InvalidArgument("minimum_role", "project role is invalid")
	}
	member, err := s.store.GetProjectMember(ctx, projectID, principal.UserID)
	if err != nil {
		return project.Member{}, err
	}
	if member.ProjectID != projectID || member.UserID != principal.UserID || !project.ValidRole(member.Role) || member.Version < 1 {
		return project.Member{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if !project.RoleAtLeast(member.Role, minimumRole) {
		return project.Member{}, domain.NewError(domain.CodeForbidden, "project role is insufficient")
	}
	return member, nil
}

func (s *ProjectService) Create(ctx context.Context, principal identity.Principal, command CreateProjectCommand) (project.Project, bool, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return project.Project{}, false, err
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.Project{}, false, err
	}
	value, err := project.NewID()
	if err != nil {
		return project.Project{}, false, err
	}
	created, err := project.New(value, principal.UserID, command.Name, command.Description)
	if err != nil {
		return project.Project{}, false, domain.InvalidArgument("project", err.Error())
	}
	result, replay, err := s.store.CreateProject(ctx, principal.UserID, created, command.IdempotencyKey)
	return result, replay, err
}

type CreateProjectCommand struct {
	Name           string
	Description    string
	IdempotencyKey string
}

func (s *ProjectService) List(ctx context.Context, principal identity.Principal, page PageRequest) (ProjectPage, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return ProjectPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectPage{}, err
	}
	return s.store.ListProjects(ctx, principal.UserID, page)
}

func (s *ProjectService) Get(ctx context.Context, principal identity.Principal, projectID string) (project.Project, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return project.Project{}, err
	}
	return s.store.GetProject(ctx, principal.UserID, projectID)
}

func (s *ProjectService) ListMembers(ctx context.Context, principal identity.Principal, projectID string, page PageRequest) (ProjectMemberPage, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectMemberPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectMemberPage{}, err
	}
	return s.store.ListProjectMembers(ctx, projectID, principal.UserID, page)
}

type InviteProjectMemberCommand struct {
	UserID          string
	Role            project.Role
	ExpectedVersion int64
	IdempotencyKey  string
}

func (s *ProjectService) Invite(ctx context.Context, principal identity.Principal, projectID string, command InviteProjectMemberCommand) (project.Invitation, bool, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return project.Invitation{}, false, err
	}
	if !domain.ValidUserID(command.UserID) || command.UserID == principal.UserID {
		return project.Invitation{}, false, domain.InvalidArgument("user_id", "invitee is invalid")
	}
	if !project.ValidInvitationRole(command.Role) {
		return project.Invitation{}, false, domain.InvalidArgument("role", "invitation role is invalid")
	}
	if command.ExpectedVersion < 1 {
		return project.Invitation{}, false, domain.InvalidArgument("expected_version", "project version is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.Invitation{}, false, err
	}
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return project.Invitation{}, false, err
	}
	invitation := project.Invitation{
		ID: id, ProjectID: projectID, InviterID: principal.UserID, InviteeID: command.UserID,
		Role: command.Role, State: project.InvitationPending, Version: 1,
	}
	return s.store.CreateProjectInvitation(ctx, principal.UserID, invitation, command.ExpectedVersion, command.IdempotencyKey)
}

func (s *ProjectService) ListInvitations(ctx context.Context, principal identity.Principal, page PageRequest) (ProjectInvitationPage, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return ProjectInvitationPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectInvitationPage{}, err
	}
	return s.store.ListProjectInvitations(ctx, principal.UserID, page)
}

func (s *ProjectService) AcceptInvitation(ctx context.Context, principal identity.Principal, invitationID, idempotencyKey string) (project.Member, bool, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return project.Member{}, false, err
	}
	if !domain.ValidPendingRequestID(invitationID) {
		return project.Member{}, false, domain.NewError(domain.CodeNotFound, "project invitation not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return project.Member{}, false, err
	}
	return s.store.AcceptProjectInvitation(ctx, invitationID, principal.UserID, idempotencyKey)
}

func (s *ProjectService) RevokeInvitation(ctx context.Context, principal identity.Principal, projectID, invitationID string, expectedVersion int64, idempotencyKey string) error {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return err
	}
	if !domain.ValidPendingRequestID(invitationID) || expectedVersion < 1 {
		return domain.InvalidArgument("invitation", "project invitation request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	return s.store.RevokeProjectInvitation(ctx, principal.UserID, projectID, invitationID, expectedVersion, idempotencyKey)
}

type UpdateProjectMemberCommand struct {
	Role            project.Role
	ExpectedVersion int64
	IdempotencyKey  string
}

func (s *ProjectService) UpdateMemberRole(ctx context.Context, principal identity.Principal, projectID, userID string, command UpdateProjectMemberCommand) (project.Member, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return project.Member{}, err
	}
	if !domain.ValidUserID(userID) || userID == principal.UserID {
		return project.Member{}, domain.InvalidArgument("user_id", "project member is invalid")
	}
	if !project.ValidInvitationRole(command.Role) || command.ExpectedVersion < 1 {
		return project.Member{}, domain.InvalidArgument("role", "project member update is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.Member{}, err
	}
	return s.store.UpdateProjectMemberRole(ctx, principal.UserID, projectID, userID, command.Role, command.ExpectedVersion, command.IdempotencyKey)
}

func (s *ProjectService) RemoveMember(ctx context.Context, principal identity.Principal, projectID, userID string, expectedVersion int64, idempotencyKey string) error {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return err
	}
	if !domain.ValidUserID(userID) || userID == principal.UserID || expectedVersion < 1 {
		return domain.InvalidArgument("member", "project member request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	return s.store.RemoveProjectMember(ctx, principal.UserID, projectID, userID, expectedVersion, idempotencyKey)
}

func (s *ProjectService) TransferOwner(ctx context.Context, principal identity.Principal, projectID, userID string, expectedVersion int64, idempotencyKey string) (project.Project, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return project.Project{}, err
	}
	if !domain.ValidUserID(userID) || userID == principal.UserID || expectedVersion < 1 {
		return project.Project{}, domain.InvalidArgument("user_id", "owner transfer request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return project.Project{}, err
	}
	target, err := s.store.GetProjectMember(ctx, projectID, userID)
	if err != nil {
		return project.Project{}, err
	}
	if target.Role == project.RoleOwner {
		return project.Project{}, domain.NewError(domain.CodeConflict, "project ownership already changed")
	}
	return s.store.TransferProjectOwner(ctx, principal.UserID, projectID, userID, expectedVersion, idempotencyKey)
}

func (s *ProjectService) ListResources(ctx context.Context, principal identity.Principal, projectID string, page PageRequest) (ProjectResourcePage, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectResourcePage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectResourcePage{}, err
	}
	return s.store.ListProjectResources(ctx, projectID, principal.UserID, page)
}

func (s *ProjectService) AddResource(ctx context.Context, principal identity.Principal, projectID string, kind project.ResourceKind, resourceID string, expectedVersion int64, idempotencyKey string) (project.Resource, bool, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return project.Resource{}, false, err
	}
	if !validProjectResource(kind, resourceID) || expectedVersion < 1 {
		return project.Resource{}, false, domain.InvalidArgument("resource", "project resource request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return project.Resource{}, false, err
	}
	return s.store.AddProjectResource(ctx, principal.UserID, project.Resource{
		ProjectID: projectID, Kind: kind, ResourceID: resourceID, Version: 1,
	}, expectedVersion, idempotencyKey)
}

func (s *ProjectService) RemoveResource(ctx context.Context, principal identity.Principal, projectID string, kind project.ResourceKind, resourceID string, expectedVersion int64, idempotencyKey string) error {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return err
	}
	if !validProjectResource(kind, resourceID) || expectedVersion < 1 {
		return domain.InvalidArgument("resource", "project resource request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	return s.store.RemoveProjectResource(ctx, principal.UserID, projectID, kind, resourceID, expectedVersion, idempotencyKey)
}

func (s *ProjectService) GetContent(ctx context.Context, principal identity.Principal, projectID string, kind project.ContentKind, contentID string) (project.Content, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return project.Content{}, err
	}
	if !project.ValidContentKind(kind) || !project.ValidContentID(contentID) {
		return project.Content{}, domain.NewError(domain.CodeNotFound, "project content not found")
	}
	value, err := s.store.GetProjectContent(ctx, principal.UserID, projectID, kind, contentID)
	if err != nil {
		return project.Content{}, err
	}
	if value.ProjectID != projectID || value.Kind != kind || value.ID != contentID || value.Revision < 1 || !domain.ValidUserID(value.UpdatedBy) ||
		project.ValidateContentBody(kind, contentID, value.Body) != nil {
		return project.Content{}, domain.NewError(domain.CodeInternal, "project content store returned an invalid record")
	}
	return value, nil
}

// AgentContext returns a bounded snapshot of shared project memory, goals, and documents.
func (s *ProjectService) AgentContext(ctx context.Context, principal identity.Principal, taskValue task.Task) ([]project.Content, bool, error) {
	if taskValue.InitiatorID != principal.UserID || !cloudDSHProjectContextEligible(taskValue) {
		return nil, false, domain.NewError(domain.CodeForbidden, "project context requires the authorized Task initiator")
	}
	if _, err := s.RequireAccess(ctx, principal, taskValue.ProjectID, project.RoleViewer); err != nil {
		return nil, false, err
	}
	return s.store.ListProjectAgentContext(ctx, principal, taskValue, projectAgentContextItems, projectAgentContextObjectSize)
}

type MutateProjectContentCommand struct {
	Kind           project.ContentKind
	ContentID      string
	BaseRevision   int64
	Body           json.RawMessage
	Delete         bool
	IdempotencyKey string
}

func (s *ProjectService) MutateContent(ctx context.Context, principal identity.Principal, projectID string, command MutateProjectContentCommand) (project.Content, bool, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return project.Content{}, false, err
	}
	if !project.ValidContentKind(command.Kind) || !project.ValidContentID(command.ContentID) || command.BaseRevision < 0 ||
		(command.Delete && len(command.Body) != 0) {
		return project.Content{}, false, domain.InvalidArgument("content", "project content mutation is invalid")
	}
	if !command.Delete {
		if err := project.ValidateContentBody(command.Kind, command.ContentID, command.Body); err != nil {
			return project.Content{}, false, err
		}
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.Content{}, false, err
	}
	value := project.Content{
		ProjectID: projectID, Kind: command.Kind, ID: command.ContentID,
		Revision: command.BaseRevision + 1, Body: append(json.RawMessage(nil), command.Body...), UpdatedBy: principal.UserID,
	}
	result, replay, err := s.store.ApplyProjectContentMutation(ctx, principal.UserID, value, command.BaseRevision, command.Delete, command.IdempotencyKey)
	if err != nil {
		return project.Content{}, false, err
	}
	if result.ProjectID != projectID || result.Kind != command.Kind || result.ID != command.ContentID || result.Revision < 1 || !domain.ValidUserID(result.UpdatedBy) ||
		(command.Delete && len(result.Body) != 0) || (!command.Delete && project.ValidateContentBody(command.Kind, command.ContentID, result.Body) != nil) {
		return project.Content{}, false, domain.NewError(domain.CodeInternal, "project content store returned an invalid record")
	}
	return result, replay, nil
}

func (s *ProjectService) SearchContent(ctx context.Context, principal identity.Principal, query string, limit int) ([]project.ContentSearchResult, error) {
	if err := validateProjectPrincipal(principal); err != nil {
		return nil, err
	}
	if err := project.ValidateContentSearchQuery(query); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > project.MaxContentSearchResults {
		return nil, domain.InvalidArgument("limit", "project content search limit is invalid")
	}
	values, err := s.store.SearchProjectContent(ctx, principal.UserID, query, limit)
	if err != nil {
		return nil, err
	}
	results := make([]project.ContentSearchResult, 0, len(values))
	checked := make(map[string]bool)
	for _, value := range values {
		if err := project.ValidateContentSearchResult(value); err != nil {
			return nil, err
		}
		allowed, exists := checked[value.ProjectID]
		if !exists {
			_, accessErr := s.RequireAccess(ctx, principal, value.ProjectID, project.RoleViewer)
			allowed = accessErr == nil
			if accessErr != nil && domain.CodeOf(accessErr) != domain.CodeNotFound && domain.CodeOf(accessErr) != domain.CodeForbidden {
				return nil, accessErr
			}
			checked[value.ProjectID] = allowed
		}
		if allowed {
			results = append(results, value)
		}
	}
	return results, nil
}

func (s *ProjectService) ListContentComments(ctx context.Context, principal identity.Principal, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID string) ([]project.ContentComment, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return nil, err
	}
	if !project.ValidMuseContentKind(kind) || !project.ValidContentID(contentID) || contentRevision < 1 || !project.ValidContentID(rowID) {
		return nil, domain.NewError(domain.CodeNotFound, "project content comments not found")
	}
	values, err := s.store.ListProjectContentComments(ctx, principal.UserID, projectID, kind, contentID, contentRevision, rowID, project.MaxContentCommentsPerRow)
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		if err := project.ValidateContentComment(value); err != nil || value.ProjectID != projectID || value.Kind != kind ||
			value.ContentID != contentID || value.ContentRevision != contentRevision || value.RowID != rowID || value.Deleted {
			return nil, domain.NewError(domain.CodeInternal, "project content comment store returned an invalid record")
		}
	}
	return values, nil
}

type MutateProjectContentCommentCommand struct {
	Kind            project.ContentKind
	ContentID       string
	ContentRevision int64
	RowID           string
	CommentID       string
	BaseVersion     int64
	Body            string
	Delete          bool
	IdempotencyKey  string
}

func (s *ProjectService) MutateContentComment(ctx context.Context, principal identity.Principal, projectID string, command MutateProjectContentCommentCommand) (project.ContentComment, bool, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return project.ContentComment{}, false, err
	}
	if !project.ValidMuseContentKind(command.Kind) || !project.ValidContentID(command.ContentID) || command.ContentRevision < 1 ||
		!project.ValidContentID(command.RowID) || command.BaseVersion < 0 || command.BaseVersion == math.MaxInt64 ||
		(command.CommentID != "" && !project.ValidCommentID(command.CommentID)) ||
		(command.CommentID != "" && command.BaseVersion == 0) ||
		(command.BaseVersion > 0 && command.CommentID == "") || (command.Delete && command.BaseVersion == 0) ||
		(command.Delete && command.Body != "") || (!command.Delete && (strings.TrimSpace(command.Body) != command.Body ||
		len([]rune(command.Body)) < 1 || len([]rune(command.Body)) > project.MaxContentCommentBodyRunes)) {
		return project.ContentComment{}, false, domain.InvalidArgument("comment", "project content comment mutation is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.ContentComment{}, false, err
	}
	commentID := command.CommentID
	if commentID == "" {
		var err error
		commentID, err = project.NewCommentID()
		if err != nil {
			return project.ContentComment{}, false, err
		}
	}
	value := project.ContentComment{
		ProjectID: projectID, Kind: command.Kind, ContentID: command.ContentID, ContentRevision: command.ContentRevision,
		RowID: command.RowID, ID: commentID, Revision: command.BaseVersion + 1, Body: command.Body,
		Deleted: command.Delete, UpdatedBy: principal.UserID,
	}
	result, replay, err := s.store.ApplyProjectContentCommentMutation(ctx, principal.UserID, value, command.BaseVersion, command.Delete, command.IdempotencyKey)
	if err != nil {
		return project.ContentComment{}, false, err
	}
	if err := project.ValidateContentComment(result); err != nil || result.ProjectID != projectID || result.Kind != command.Kind ||
		result.ContentID != command.ContentID || result.ContentRevision != command.ContentRevision || result.RowID != command.RowID {
		return project.ContentComment{}, false, domain.NewError(domain.CodeInternal, "project content comment store returned an invalid record")
	}
	return result, replay, nil
}

func validateProjectPrincipal(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.Role != identity.RoleUser {
		return domain.NewError(domain.CodeForbidden, "project access requires a user account")
	}
	switch principal.Kind {
	case identity.KindSession:
		if principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	case identity.KindClient:
		if !domain.ValidClientInstallationID(principal.ClientInstallationID) || principal.HostScope != "" ||
			len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	default:
		return domain.NewError(domain.CodeForbidden, "project access requires a user session or client installation")
	}
	return nil
}

func normalizeProjectPage(page PageRequest) (PageRequest, error) {
	if err := validatePageRequest(page); err != nil {
		return PageRequest{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	return page, nil
}

func validProjectResource(kind project.ResourceKind, id string) bool {
	switch kind {
	case project.ResourceHost:
		return domain.ValidHostID(id)
	case project.ResourceWorkspace:
		return domain.ValidWorkspaceID(id)
	default:
		return false
	}
}
