package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const expiredWorkspaceSyncInstallationNeedsCleanup = `(
  EXISTS (
    SELECT 1 FROM workspace_sync_device_bindings b
    WHERE b.owner_id=i.owner_id AND b.client_installation_id=i.id
      AND b.state IN ('active','pending_node')
  ) OR EXISTS (
    SELECT 1 FROM workspace_sync_device_identities d
    WHERE d.owner_id=i.owner_id AND d.client_installation_id=i.id AND d.revoked_at IS NULL
  ) OR EXISTS (
    SELECT 1 FROM workspace_sync_device_challenges c
    WHERE c.owner_id=i.owner_id AND c.client_installation_id=i.id
  ) OR EXISTS (
    SELECT 1 FROM workspace_sync_sessions s
    JOIN tokens t ON s.replica_id='device:' || t.id
    WHERE s.owner_id=i.owner_id AND t.owner_id=i.owner_id
      AND t.client_installation_id=i.id AND t.kind='device'
      AND s.state IN ('planned','transferring','verifying','applying')
  )
)`

func (s *Store) RevokeExpiredWorkspaceSyncDevices(ctx context.Context, disableRelay func(context.Context, string) error) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT i.owner_id
FROM client_installations i
WHERE i.platform='desktop' AND i.revoked_at IS NULL
  AND (i.expires_at IS NULL OR i.expires_at<=clock_timestamp())
  AND `+expiredWorkspaceSyncInstallationNeedsCleanup+`
ORDER BY i.owner_id`)
	if err != nil {
		return fmt.Errorf("list owners with expired Workspace sync installations: %w", err)
	}
	owners := make([]string, 0)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			rows.Close()
			return fmt.Errorf("read owner with expired Workspace sync installation: %w", err)
		}
		owners = append(owners, ownerID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read owners with expired Workspace sync installations: %w", err)
	}

	var failures []error
	for _, ownerID := range owners {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.withWorkspaceSyncOwnerTx(ctx, ownerID, func(tx pgx.Tx) error {
			if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, true); err != nil {
				return err
			}
			installations, err := tx.Query(ctx, `SELECT i.id
FROM client_installations i
WHERE i.owner_id=$1 AND i.platform='desktop' AND i.revoked_at IS NULL
  AND (i.expires_at IS NULL OR i.expires_at<=clock_timestamp())
  AND `+expiredWorkspaceSyncInstallationNeedsCleanup+`
ORDER BY i.expires_at NULLS FIRST,i.id FOR UPDATE OF i`, ownerID)
			if err != nil {
				return fmt.Errorf("lock expired Workspace sync installations: %w", err)
			}
			installationIDs := make([]string, 0)
			for installations.Next() {
				var installationID string
				if err := installations.Scan(&installationID); err != nil {
					installations.Close()
					return fmt.Errorf("read expired Workspace sync installation: %w", err)
				}
				installationIDs = append(installationIDs, installationID)
			}
			installations.Close()
			if err := installations.Err(); err != nil {
				return fmt.Errorf("read expired Workspace sync installations: %w", err)
			}
			if len(installationIDs) > 0 && disableRelay != nil {
				if err := disableRelay(ctx, ownerID); err != nil {
					return fmt.Errorf("disable Workspace sync relay before expired installation revocation: %w", err)
				}
			}
			for _, installationID := range installationIDs {
				if err := revokeWorkspaceSyncDevicesForInstallation(ctx, tx, ownerID, installationID); err != nil {
					return err
				}
			}
			return nil
		}, nil, nil)
		if err != nil {
			failures = append(failures, fmt.Errorf("revoke expired Workspace sync devices for owner %s: %w", ownerID, err))
		}
	}
	return errors.Join(failures...)
}
