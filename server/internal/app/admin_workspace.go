package app

import (
	"context"
	"errors"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type AdminWorkspaceStore interface {
	ListAdminWorkspaces(context.Context, PageRequest) (workspace.AdminPage, error)
	GetAdminWorkspace(context.Context, string) (workspace.AdminSummary, error)
}

type AdminWorkspaceService struct {
	store AdminWorkspaceStore
}

func NewAdminWorkspaceService(store AdminWorkspaceStore) (*AdminWorkspaceService, error) {
	if store == nil {
		return nil, errors.New("admin workspace store is required")
	}
	return &AdminWorkspaceService{store: store}, nil
}

func (s *AdminWorkspaceService) List(ctx context.Context, principal identity.Principal, page PageRequest) (workspace.AdminPage, error) {
	if err := authorizeAdmin(principal); err != nil {
		return workspace.AdminPage{}, err
	}
	if err := validatePageRequest(page); err != nil {
		return workspace.AdminPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	return s.store.ListAdminWorkspaces(ctx, page)
}

func (s *AdminWorkspaceService) Get(ctx context.Context, principal identity.Principal, id string) (workspace.AdminSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return workspace.AdminSummary{}, err
	}
	if !domain.ValidWorkspaceID(id) {
		return workspace.AdminSummary{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.store.GetAdminWorkspace(ctx, id)
}
