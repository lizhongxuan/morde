package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type workspaceSyncDeviceAccess struct {
	ownerID        string
	workspaceID    string
	tokenID        string
	installationID string
}

func (s *Store) CheckWorkspaceSyncDeviceAccess(ctx context.Context, principal identity.Principal, workspaceID string) error {
	if principal.Kind != identity.KindDevice || principal.Role != identity.RoleUser ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) || !domain.ValidWorkspaceID(workspaceID) {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		access, err := lockWorkspaceSyncDeviceCredentials(ctx, tx, principal.UserID, principal.ClientInstallationID, principal.TokenID, workspaceID)
		if err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		return requireActiveWorkspaceSyncDeviceBinding(ctx, tx, access)
	})
}

func lockWorkspaceSyncDeviceCredentials(ctx context.Context, tx pgx.Tx, ownerID, installationID, tokenID, workspaceID string) (*workspaceSyncDeviceAccess, error) {
	if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, false); err != nil {
		return nil, err
	}
	if err := requireApprovedWorkspaceSyncOwner(ctx, tx, ownerID); err != nil {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT platform='desktop' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID, ownerID).Scan(&active)
	if err != nil || !active {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	err = tx.QueryRow(ctx, `SELECT kind='device' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM tokens WHERE id=$1 AND owner_id=$2 AND client_installation_id=$3 FOR SHARE`, tokenID, ownerID, installationID).Scan(&active)
	if err != nil || !active {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return &workspaceSyncDeviceAccess{ownerID: ownerID, workspaceID: workspaceID, tokenID: tokenID, installationID: installationID}, nil
}

func prepareWorkspaceSyncDeviceSessionMutation(ctx context.Context, tx pgx.Tx, workspaceID, sessionID string) (*workspaceSyncDeviceAccess, error) {
	var ownerID, replicaID string
	err := tx.QueryRow(ctx, `SELECT owner_id,replica_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`, workspaceID, sessionID).
		Scan(&ownerID, &replicaID)
	if err != nil {
		return nil, storeError("read workspace sync session authorization", "workspace sync session", err)
	}
	if !strings.HasPrefix(replicaID, "device:") {
		return nil, nil
	}
	tokenID := strings.TrimPrefix(replicaID, "device:")
	if !domain.ValidTokenID(tokenID) {
		return nil, domain.NewError(domain.CodeInternal, "stored workspace sync device replica is invalid")
	}
	var installationID string
	err = tx.QueryRow(ctx, `SELECT client_installation_id FROM tokens WHERE id=$1 AND owner_id=$2`, tokenID, ownerID).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !domain.ValidClientInstallationID(installationID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if err != nil {
		return nil, fmt.Errorf("read Workspace sync device installation: %w", err)
	}
	return lockWorkspaceSyncDeviceCredentials(ctx, tx, ownerID, installationID, tokenID, workspaceID)
}
func requireActiveWorkspaceSyncDeviceSession(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess, ownerID, workspaceID, replicaID string) error {
	if access == nil {
		return nil
	}
	if access.ownerID != ownerID || access.workspaceID != workspaceID || replicaID != "device:"+access.tokenID {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return requireActiveWorkspaceSyncDeviceBinding(ctx, tx, access)
}

func requireActiveWorkspaceSyncDeviceBinding(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1
  FROM workspace_sync_device_bindings b
  JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id
  JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id
  JOIN workspaces w ON w.id=b.workspace_id AND w.owner_id=b.owner_id
  WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.client_installation_id=$3
    AND b.state='active' AND i.revoked_at IS NULL AND f.state='active'
    AND w.state='ready' AND w.deleted_at IS NULL
)`, access.ownerID, access.workspaceID, access.installationID).Scan(&active)
	if err != nil {
		return fmt.Errorf("check Workspace sync device binding: %w", err)
	}
	if !active {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return nil
}

func prepareWorkspaceSyncDeviceReplica(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, replicaID string) (*workspaceSyncDeviceAccess, error) {
	if !strings.HasPrefix(replicaID, "device:") {
		return nil, nil
	}
	tokenID := strings.TrimPrefix(replicaID, "device:")
	if !domain.ValidTokenID(tokenID) {
		return nil, domain.NewError(domain.CodeInternal, "stored workspace sync device replica is invalid")
	}
	var installationID string
	err := tx.QueryRow(ctx, `SELECT client_installation_id FROM tokens WHERE id=$1 AND owner_id=$2`, tokenID, ownerID).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !domain.ValidClientInstallationID(installationID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if err != nil {
		return nil, fmt.Errorf("read Workspace sync device installation: %w", err)
	}
	return lockWorkspaceSyncDeviceCredentials(ctx, tx, ownerID, installationID, tokenID, workspaceID)
}

