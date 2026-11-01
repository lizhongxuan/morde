package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
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

func (s *Store) WithWorkspaceSyncHostSessionAccess(ctx context.Context, principal identity.Principal, workspaceID, sessionID string, write func() error) error {
	if principal.Kind != identity.KindHostReplica || !domain.ValidUserID(principal.UserID) ||
		!domain.ValidHostID(principal.HostScope) || principal.HostGeneration < 1 ||
		!domain.ValidWorkspaceID(workspaceID) || principal.ReplicaWorkspaceID != workspaceID || principal.BindingVersion < 1 ||
		principal.TokenID != "" || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 ||
		!workspace.ValidSessionID(sessionID) || write == nil {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	expectedReplicaID := fmt.Sprintf("host:%s:g%d:%s:b%d", principal.HostScope, principal.HostGeneration, workspaceID, principal.BindingVersion)
	return s.withApprovedWorkspaceSyncOwnerReadTx(ctx, principal.UserID, func(tx pgx.Tx) error {
		replica, ok := parseHostSyncReplicaID(expectedReplicaID)
		if !ok {
			return domain.NewError(domain.CodeUnauthenticated, "host replica workspace binding is invalid")
		}
		if err := lockWorkspaceSyncHostSessionAccess(ctx, tx, principal.UserID, workspaceID, sessionID, replica, false); err != nil {
			return err
		}
		return write()
	})
}

func lockWorkspaceSyncHostSessionAccess(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, sessionID string, replica hostSyncReplicaID, forUpdate bool) error {
	lockClause := "FOR SHARE"
	if forUpdate {
		lockClause = "FOR UPDATE"
	}
	var hostOwner, hostState string
	var generation int64
	var deletedAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id, state, runtime_generation, deleted_at FROM hosts WHERE id=$1 %s`, lockClause), replica.hostID).
		Scan(&hostOwner, &hostState, &generation, &deletedAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	} else if err != nil {
		return fmt.Errorf("lock Host workspace access: %w", err)
	}
	if hostOwner != ownerID || generation != replica.generation || deletedAt.Valid ||
		(hostState != string(host.StateCreating) && hostState != string(host.StateRunning)) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	var workspaceOwner, workspaceState string
	var workspaceDeleted pgtype.Timestamptz
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id, state, deleted_at FROM workspaces WHERE id=$1 %s`, lockClause), workspaceID).
		Scan(&workspaceOwner, &workspaceState, &workspaceDeleted); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	} else if err != nil {
		return fmt.Errorf("lock Workspace for Host sync access: %w", err)
	}
	if workspaceOwner != ownerID || workspaceState != string(workspace.StateReady) || workspaceDeleted.Valid {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	var bindingOwner, bindingState string
	var bindingVersion int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id, state, binding_version FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 %s`, lockClause),
		replica.hostID, workspaceID).Scan(&bindingOwner, &bindingState, &bindingVersion); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	} else if err != nil {
		return fmt.Errorf("lock Workspace binding for Host sync access: %w", err)
	}
	if bindingOwner != ownerID || bindingVersion != replica.bindingVersion ||
		(bindingState != string(workspace.BindingPreparing) && bindingState != string(workspace.BindingActive) &&
			bindingState != string(workspace.BindingUnbinding)) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	var sessionOwner, replicaID, sessionState string
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id, replica_id, state FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 %s`, lockClause),
		workspaceID, sessionID).Scan(&sessionOwner, &replicaID, &sessionState); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	} else if err != nil {
		return fmt.Errorf("lock Host workspace sync session: %w", err)
	}
	if sessionOwner != ownerID || replicaID != fmt.Sprintf("host:%s:g%d:%s:b%d", replica.hostID, replica.generation, workspaceID, replica.bindingVersion) {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	if sessionState == string(workspace.SyncCancelled) || sessionState == string(workspace.SyncExpired) || sessionState == string(workspace.SyncFailed) {
		return domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
	}
	return nil
}

func (s *Store) WithWorkspaceSyncSessionAccess(ctx context.Context, principal identity.Principal, workspaceID, sessionID string, write func() error) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 ||
		!domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSessionID(sessionID) || write == nil {
		return domain.NewError(domain.CodeUnauthenticated, "session authentication required")
	}
	return s.withApprovedWorkspaceSyncOwnerReadTx(ctx, principal.UserID, func(tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT kind='session' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, principal.TokenID, principal.UserID).Scan(&active); err != nil || !active {
			return domain.NewError(domain.CodeUnauthenticated, "session is no longer active")
		}
		var workspaceOwner, workspaceState string
		var workspaceDeleted pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, deleted_at FROM workspaces WHERE id=$1 FOR SHARE`, workspaceID).
			Scan(&workspaceOwner, &workspaceState, &workspaceDeleted); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock Workspace for session sync access: %w", err)
		}
		if workspaceOwner != principal.UserID || workspaceState != string(workspace.StateReady) || workspaceDeleted.Valid {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		var sessionOwner, replicaID, sessionState string
		if err := tx.QueryRow(ctx, `SELECT owner_id, replica_id, state FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 FOR SHARE`,
			workspaceID, sessionID).Scan(&sessionOwner, &replicaID, &sessionState); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		} else if err != nil {
			return fmt.Errorf("lock session Workspace sync session: %w", err)
		}
		if sessionOwner != principal.UserID || replicaID != "session:"+principal.UserID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if sessionState == string(workspace.SyncCancelled) || sessionState == string(workspace.SyncExpired) || sessionState == string(workspace.SyncFailed) {
			return domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
		}
		return write()
	})
}

func (s *Store) WithWorkspaceSyncDeviceSessionAccess(ctx context.Context, principal identity.Principal, workspaceID, sessionID string, write func() error) error {
	if principal.Kind != identity.KindDevice || principal.Role != identity.RoleUser ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) || !domain.ValidWorkspaceID(workspaceID) ||
		!workspace.ValidSessionID(sessionID) || write == nil {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		access, err := lockWorkspaceSyncDeviceCredentials(ctx, tx, principal.UserID, principal.ClientInstallationID, principal.TokenID, workspaceID)
		if err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		var ownerID, replicaID, state string
		err = tx.QueryRow(ctx, `SELECT owner_id,replica_id,state FROM workspace_sync_sessions
WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, sessionID).Scan(&ownerID, &replicaID, &state)
		if err != nil {
			return storeError("lock Workspace sync download session", "workspace sync session", err)
		}
		if ownerID != principal.UserID || replicaID != "device:"+principal.TokenID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if state == string(workspace.SyncCancelled) || state == string(workspace.SyncExpired) || state == string(workspace.SyncFailed) {
			return domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, access, ownerID, workspaceID, replicaID); err != nil {
			return err
		}
		return write()
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
