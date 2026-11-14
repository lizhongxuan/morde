package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) GetWorkspaceVolumeRegistry(ctx context.Context, workspaceID string) (app.WorkspaceVolumeRegistry, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return app.WorkspaceVolumeRegistry{}, domain.NewError(domain.CodeNotFound, "workspace volume not found")
	}
	var value app.WorkspaceVolumeRegistry
	var volumeName, legacyHostID *string
	if err := s.pool.QueryRow(ctx, `
SELECT workspace_id, owner_id, provisioned_owner_id, state, volume_name, legacy_host_id
FROM workspace_volumes WHERE workspace_id=$1`, workspaceID).Scan(
		&value.WorkspaceID, &value.OwnerID, &value.ProvisionedOwnerID, &value.State, &volumeName, &legacyHostID,
	); err != nil {
		return app.WorkspaceVolumeRegistry{}, storeError("get workspace volume registry", "workspace volume", err)
	}
	if volumeName != nil {
		value.VolumeName = *volumeName
	}
	if legacyHostID != nil {
		value.LegacyHostID = *legacyHostID
	}
	return value, nil
}

func (s *Store) ListLegacyHostWorkspaceVolumes(ctx context.Context, hostID string) ([]app.WorkspaceVolumeRegistry, error) {
	if !domain.ValidHostID(hostID) {
		return nil, domain.InvalidArgument("host_id", "host id is invalid")
	}
	rows, err := s.pool.Query(ctx, `
SELECT workspace_id, owner_id, provisioned_owner_id, state, volume_name, legacy_host_id
FROM workspace_volumes
WHERE state='legacy' AND legacy_host_id=$1
ORDER BY workspace_id`, hostID)
	if err != nil {
		return nil, fmt.Errorf("list legacy Host workspace volume references: %w", err)
	}
	defer rows.Close()
	values := make([]app.WorkspaceVolumeRegistry, 0)
	for rows.Next() {
		var value app.WorkspaceVolumeRegistry
		var provisionedOwnerID, volumeName, legacyHostID *string
		if err := rows.Scan(&value.WorkspaceID, &value.OwnerID, &provisionedOwnerID, &value.State, &volumeName, &legacyHostID); err != nil {
			return nil, fmt.Errorf("read legacy Host workspace volume reference: %w", err)
		}
		if provisionedOwnerID != nil {
			value.ProvisionedOwnerID = *provisionedOwnerID
		}
		if volumeName != nil {
			value.VolumeName = *volumeName
		}
		if legacyHostID != nil {
			value.LegacyHostID = *legacyHostID
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy Host workspace volume references: %w", err)
	}
	return values, nil
}

func (s *Store) MarkWorkspaceVolumeReady(ctx context.Context, ownerID, workspaceID, volumeName string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || volumeName != app.WorkspaceNamedVolumeName(workspaceID) {
		return domain.InvalidArgument("workspace_volume", "workspace volume registration is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentOwner, state string
		var currentName *string
		if err := tx.QueryRow(ctx, `
SELECT owner_id, state, volume_name FROM workspace_volumes
WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&currentOwner, &state, &currentName); err != nil {
			return storeError("lock workspace volume registry", "workspace volume", err)
		}
		if currentOwner != ownerID {
			return domain.NewError(domain.CodeNotFound, "workspace volume not found")
		}
		if state == string(app.WorkspaceVolumeReady) && currentName != nil && *currentName == volumeName {
			return nil
		}
		if state != string(app.WorkspaceVolumePending) || currentName == nil || *currentName != volumeName {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace volume registry changed before provisioning completed")
		}
		result, err := tx.Exec(ctx, `
UPDATE workspace_volumes SET state='ready', ready_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE workspace_id=$1 AND owner_id=$2 AND state='pending' AND volume_name=$3`, workspaceID, ownerID, volumeName)
		if err != nil {
			return fmt.Errorf("register ready workspace volume: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace volume registry changed before provisioning completed")
		}
		return nil
	})
}
