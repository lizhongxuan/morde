package app

import (
	"context"
	"errors"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ProjectMCPConnector struct {
	Grant             project.MCPConnectorGrant
	Name              string
	Enabled           bool
	ConnectorRevision int64
}

type ProjectMCPConnectorStore interface {
	ListProjectMCPConnectors(context.Context, string, string) ([]ProjectMCPConnector, error)
	GrantProjectMCPConnector(context.Context, string, string, string, int64) (ProjectMCPConnector, error)
	RevokeProjectMCPConnector(context.Context, string, string, string, int64) (ProjectMCPConnector, error)
	AuthorizeProjectMCPConnector(context.Context, string, string, string, string, int64) error
}

// ProjectMCPConnectorAuthorizationStore is consulted by the Task broker and
// the durable operation store. Implementations must check current membership,
// grant version, and the connector owner in the same query/transaction.
type ProjectMCPConnectorAuthorizationStore interface {
	AuthorizeProjectMCPConnector(context.Context, string, string, string, string, int64) error
}

type ProjectMCPConnectorService struct {
	store    ProjectMCPConnectorStore
	projects ProjectAccessChecker
}

func NewProjectMCPConnectorService(store ProjectMCPConnectorStore, projects ProjectAccessChecker) (*ProjectMCPConnectorService, error) {
	if store == nil || projects == nil {
		return nil, errors.New("project connector store and access checker are required")
	}
	return &ProjectMCPConnectorService{store: store, projects: projects}, nil
}

func (s *ProjectMCPConnectorService) List(ctx context.Context, principal identity.Principal, projectID string) ([]ProjectMCPConnector, error) {
	if _, err := s.projects.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return nil, err
	}
	items, err := s.store.ListProjectMCPConnectors(ctx, principal.UserID, projectID)
	if err != nil {
		return nil, err
	}
	if len(items) > maxProjectMCPConnectors {
		return nil, domain.NewError(domain.CodeInternal, "project connector store returned too many grants")
	}
	for _, item := range items {
		if err := validateProjectMCPConnector(item, projectID); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "project connector store returned an invalid grant")
		}
	}
	return items, nil
}

func (s *ProjectMCPConnectorService) Grant(ctx context.Context, principal identity.Principal, projectID, connectorID string, expectedVersion int64) (ProjectMCPConnector, error) {
	if _, err := s.projects.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectMCPConnector{}, err
	}
	if !domain.ValidMCPConnectorID(connectorID) || expectedVersion < 0 {
		return ProjectMCPConnector{}, domain.InvalidArgument("grant", "connector ID and non-negative expected version are required")
	}
	value, err := s.store.GrantProjectMCPConnector(ctx, principal.UserID, projectID, connectorID, expectedVersion)
	if err != nil {
		return ProjectMCPConnector{}, err
	}
	if err := validateProjectMCPConnector(value, projectID); err != nil {
		return ProjectMCPConnector{}, domain.NewError(domain.CodeInternal, "project connector store returned an invalid grant")
	}
	if value.Grant.ConnectorOwnerID != principal.UserID {
		return ProjectMCPConnector{}, domain.NewError(domain.CodeInternal, "project connector store returned a grant for another owner")
	}
	return value, nil
}

func (s *ProjectMCPConnectorService) Revoke(ctx context.Context, principal identity.Principal, projectID, connectorID string, expectedVersion int64) (ProjectMCPConnector, error) {
	// A connector owner must still be able to withdraw their authorization after
	// leaving the project; the Store separately permits only that owner or the
	// current project owner to revoke it.
	if err := validateProjectPrincipal(principal); err != nil {
		return ProjectMCPConnector{}, err
	}
	if !project.ValidID(projectID) {
		return ProjectMCPConnector{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if !domain.ValidMCPConnectorID(connectorID) || expectedVersion < 1 {
		return ProjectMCPConnector{}, domain.InvalidArgument("grant", "connector ID and positive expected version are required")
	}
	value, err := s.store.RevokeProjectMCPConnector(ctx, principal.UserID, projectID, connectorID, expectedVersion)
	if err != nil {
		return ProjectMCPConnector{}, err
	}
	if err := validateProjectMCPConnector(value, projectID); err != nil || value.Grant.Active {
		return ProjectMCPConnector{}, domain.NewError(domain.CodeInternal, "project connector store returned an invalid revoked grant")
	}
	return value, nil
}

func validateProjectMCPConnector(value ProjectMCPConnector, projectID string) error {
	if err := value.Grant.Validate(); err != nil {
		return err
	}
	if value.Grant.ProjectID != projectID {
		return errors.New("project connector view fields are invalid")
	}
	if value.Grant.Active && (value.Name == "" || len(value.Name) > 80 || value.ConnectorRevision < 1) {
		return errors.New("active project connector view fields are invalid")
	}
	if !value.Grant.Active && (len(value.Name) > 80 || value.ConnectorRevision < 0) {
		return errors.New("project connector view fields are invalid")
	}
	return nil
}

const maxProjectMCPConnectors = 16
