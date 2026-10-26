package postgres

import (
	"context"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ListAdminWorkspaces(ctx context.Context, page app.PageRequest) (workspace.AdminPage, error) {
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return workspace.AdminPage{}, err
	}
	rows, err := s.q.ListAdminWorkspaces(ctx, sqlcgen.ListAdminWorkspacesParams{
		Column1: cursorAt.Valid, CreatedAt: cursorAt, ID: cursorID, Limit: limit + 1,
	})
	if err != nil {
		return workspace.AdminPage{}, fmt.Errorf("list admin workspaces: %w", err)
	}
	values := make([]workspace.AdminSummary, 0, len(rows))
	for _, row := range rows {
		values = append(values, adminWorkspaceDomain(row))
	}
	result := workspace.AdminPage{Items: values}
	result.NextCursor = nextPageCursor(values, limit, func(value workspace.AdminSummary) (string, time.Time) {
		return value.ID, value.CreatedAt
	})
	if int32(len(result.Items)) > limit {
		result.Items = result.Items[:limit]
	}
	if err := s.populateAdminWorkspaceBindings(ctx, result.Items); err != nil {
		return workspace.AdminPage{}, err
	}
	return result, nil
}

func (s *Store) GetAdminWorkspace(ctx context.Context, id string) (workspace.AdminSummary, error) {
	if !domain.ValidWorkspaceID(id) {
		return workspace.AdminSummary{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	row, err := s.q.GetAdminWorkspace(ctx, id)
	if err != nil {
		return workspace.AdminSummary{}, storeError("get admin workspace", "workspace", err)
	}
	values := []workspace.AdminSummary{adminWorkspaceDomain(sqlcgen.ListAdminWorkspacesRow(row))}
	if err := s.populateAdminWorkspaceBindings(ctx, values); err != nil {
		return workspace.AdminSummary{}, err
	}
	return values[0], nil
}

func (s *Store) populateAdminWorkspaceBindings(ctx context.Context, values []workspace.AdminSummary) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]string, 0, len(values))
	byID := make(map[string]int, len(values))
	for i := range values {
		ids = append(ids, values[i].ID)
		byID[values[i].ID] = i
		values[i].Bindings = make([]workspace.AdminBinding, 0)
	}
	rows, err := s.q.ListAdminWorkspaceBindings(ctx, ids)
	if err != nil {
		return fmt.Errorf("list admin workspace bindings: %w", err)
	}
	for _, row := range rows {
		if i, ok := byID[row.WorkspaceID]; ok {
			values[i].Bindings = append(values[i].Bindings, workspace.AdminBinding{
				HostID: row.HostID, HostName: row.HostName, State: workspace.BindingState(row.State),
				SyncState: workspace.BindingSyncState(row.SyncState), DesiredRevision: row.DesiredRevision,
				AppliedRevision: row.AppliedRevision,
			})
		}
	}
	return nil
}

func adminWorkspaceDomain(row sqlcgen.ListAdminWorkspacesRow) workspace.AdminSummary {
	return workspace.AdminSummary{
		ID: row.ID, OwnerID: row.OwnerID, OwnerName: row.OwnerName, Name: row.Name,
		State: workspace.State(row.State), Revision: row.Revision, UsedBytes: row.UsedBytes,
		QuotaBytes: row.QuotaBytes, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
		Bindings: make([]workspace.AdminBinding, 0),
	}
}

var _ app.AdminWorkspaceStore = (*Store)(nil)
