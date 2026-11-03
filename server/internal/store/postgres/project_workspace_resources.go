package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func lockWorkspaceProjectResourcesForTransfer(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if !domain.ValidWorkspaceID(workspaceID) {
		return domain.InvalidArgument("workspace_id", "Workspace project resource lock is invalid")
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT project_id FROM project_resources
WHERE kind='workspace' AND workspace_id=$1 AND active ORDER BY project_id`, workspaceID)
	if err != nil {
		return fmt.Errorf("list Workspace projects before transfer: %w", err)
	}
	projectIDs := make([]string, 0)
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			rows.Close()
			return fmt.Errorf("scan Workspace project before transfer: %w", err)
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Workspace projects before transfer: %w", err)
	}
	rows.Close()
	for _, projectID := range projectIDs {
		if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
			return err
		}
		resources, err := tx.Query(ctx, `SELECT resource_id FROM project_resources
WHERE project_id=$1 AND kind='workspace' AND workspace_id=$2 AND active ORDER BY resource_id FOR UPDATE`, projectID, workspaceID)
		if err != nil {
			return fmt.Errorf("lock Workspace project resource before transfer: %w", err)
		}
		for resources.Next() {
			var resourceID string
			if err := resources.Scan(&resourceID); err != nil {
				resources.Close()
				return fmt.Errorf("scan Workspace project resource before transfer: %w", err)
			}
			if resourceID != workspaceID {
				resources.Close()
				return domain.NewError(domain.CodeInternal, "Workspace project resource identity is inconsistent")
			}
		}
		if err := resources.Err(); err != nil {
			resources.Close()
			return fmt.Errorf("read Workspace project resources before transfer: %w", err)
		}
		resources.Close()
	}
	return nil
}

func revokeWorkspaceProjectResourcesForTransfer(ctx context.Context, tx pgx.Tx, workspaceID, oldOwnerID, actorID string) error {
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidUserID(oldOwnerID) || !domain.ValidUserID(actorID) {
		return domain.InvalidArgument("workspace_transfer", "Workspace project resource revocation is invalid")
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT project_id FROM project_resources
WHERE kind='workspace' AND workspace_id=$1 AND active ORDER BY project_id`, workspaceID)
	if err != nil {
		return fmt.Errorf("list Workspace project resources to revoke: %w", err)
	}
	projectIDs := make([]string, 0)
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			rows.Close()
			return fmt.Errorf("scan Workspace project resource to revoke: %w", err)
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Workspace project resources to revoke: %w", err)
	}
	rows.Close()
	for _, projectID := range projectIDs {
		resourceRows, err := tx.Query(ctx, `SELECT resource_id,resource_owner_id,version FROM project_resources
WHERE project_id=$1 AND kind='workspace' AND workspace_id=$2 AND active ORDER BY resource_id FOR UPDATE`, projectID, workspaceID)
		if err != nil {
			return fmt.Errorf("lock Workspace project resource for revocation: %w", err)
		}
		versions := make([]int64, 0)
		for resourceRows.Next() {
			var resourceID, resourceOwnerID string
			var version int64
			if err := resourceRows.Scan(&resourceID, &resourceOwnerID, &version); err != nil {
				resourceRows.Close()
				return fmt.Errorf("scan Workspace project resource for revocation: %w", err)
			}
			if resourceID != workspaceID || resourceOwnerID != oldOwnerID {
				resourceRows.Close()
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace project resource owner changed before transfer")
			}
			if version < 1 || version == math.MaxInt64 {
				resourceRows.Close()
				return domain.NewError(domain.CodeInvalidState, "Workspace project resource version cannot be advanced")
			}
			versions = append(versions, version)
		}
		if err := resourceRows.Err(); err != nil {
			resourceRows.Close()
			return fmt.Errorf("read Workspace project resources for revocation: %w", err)
		}
		resourceRows.Close()
		if len(versions) == 0 {
			continue
		}
		for _, version := range versions {
			tag, err := tx.Exec(ctx, `UPDATE project_resources SET active=false,version=version+1,updated_by=$4,updated_at=clock_timestamp()
WHERE project_id=$1 AND kind='workspace' AND workspace_id=$2 AND resource_owner_id=$3 AND active AND version=$5`,
				projectID, workspaceID, oldOwnerID, actorID, version)
			if err != nil {
				return fmt.Errorf("deactivate Workspace project resource: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace project resource changed before transfer")
			}
		}
		var projectVersion int64
		if err := tx.QueryRow(ctx, `SELECT version FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&projectVersion); err != nil {
			return storeError("lock project before Workspace resource revocation", "project", err)
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, projectVersion); err != nil {
			return err
		}
	}
	return nil
}

func requireCurrentProjectWorkspaceResourceInTx(ctx context.Context, tx pgx.Tx, projectID, workspaceID string) error {
	if !project.ValidID(projectID) || !domain.ValidWorkspaceID(workspaceID) {
		return domain.NewError(domain.CodeNotFound, "project resource not found")
	}
	var resourceOwnerID string
	err := tx.QueryRow(ctx, `SELECT resource_owner_id FROM project_resources
WHERE project_id=$1 AND kind='workspace' AND resource_id=$2 AND workspace_id=$2 AND active FOR SHARE`, projectID, workspaceID).
		Scan(&resourceOwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "project resource not found")
	}
	if err != nil {
		return fmt.Errorf("lock project Workspace resource: %w", err)
	}
	var ownerID, state string
	var deleted bool
	err = tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at IS NOT NULL FROM workspaces WHERE id=$1 FOR SHARE`, workspaceID).
		Scan(&ownerID, &state, &deleted)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (state != "ready" || deleted || ownerID != resourceOwnerID) {
		return domain.NewError(domain.CodeNotFound, "project resource not found")
	}
	if err != nil {
		return fmt.Errorf("lock current Workspace for project access: %w", err)
	}
	return requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID)
}
