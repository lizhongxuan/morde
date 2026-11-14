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

func (s *Store) CheckWorkspaceSyncProjectFolderAccess(ctx context.Context, principal identity.Principal, workspaceID, folderID string) error {
	if principal.Kind != identity.KindDevice || principal.Role != identity.RoleUser ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) || !domain.ValidWorkspaceID(workspaceID) ||
		!workspace.ValidSyncFolderID(folderID) || !principal.HasScope(identity.ScopeWorkspaceSync) || !principal.AllowsWorkspace(workspaceID) {
		return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockWorkspaceSyncDeviceCredentials(ctx, tx, principal.UserID, principal.ClientInstallationID, principal.TokenID, workspaceID); err != nil {
			return err
		}
		if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, principal.UserID, workspaceID, folderID); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		var available bool
		if err := tx.QueryRow(ctx, `SELECT true FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3 AND folder_kind='project' AND state IN ('pending_node','active') FOR SHARE`,
			workspaceID, principal.UserID, folderID).Scan(&available); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
		} else if err != nil {
			return fmt.Errorf("check Workspace project upload authorization: %w", err)
		}
		return requireWorkspaceSyncProjectHostBinding(ctx, tx, principal.UserID, workspaceID, folderID)
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
		if err := lockWorkspaceSyncHostSessionAccess(ctx, tx, principal.UserID, workspaceID, sessionID, replica, false, false); err != nil {
			return err
		}
		return write()
	})
}

func lockWorkspaceSyncHostSessionAccess(ctx context.Context, tx pgx.Tx, ownerID, workspaceID, sessionID string, replica hostSyncReplicaID, forUpdate, allowUnbindingVersionAdvance bool) error {
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
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
		return err
	}
	var bindingOwner, bindingState string
	var bindingVersion int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id, state, binding_version FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 %s`, lockClause),
		replica.hostID, workspaceID).Scan(&bindingOwner, &bindingState, &bindingVersion); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	} else if err != nil {
		return fmt.Errorf("lock Workspace binding for Host sync access: %w", err)
	}
	versionMatches := bindingVersion == replica.bindingVersion
	if allowUnbindingVersionAdvance && bindingState == string(workspace.BindingUnbinding) &&
		bindingVersion > replica.bindingVersion && bindingVersion-replica.bindingVersion == 1 {
		versionMatches = true
	}
	if bindingOwner != ownerID || !versionMatches ||
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
		var preflightFolderID pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 AND owner_id=$3`,
			workspaceID, sessionID, principal.UserID).Scan(&preflightFolderID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		} else if err != nil {
			return fmt.Errorf("read project folder before session sync access: %w", err)
		}
		if preflightFolderID.Valid {
			if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, principal.UserID, workspaceID, preflightFolderID.String); err != nil {
				return err
			}
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
		if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
			return err
		}
		var sessionOwner, replicaID, sessionState string
		var folderID pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT owner_id, replica_id, state, folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 FOR SHARE`,
			workspaceID, sessionID).Scan(&sessionOwner, &replicaID, &sessionState, &folderID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		} else if err != nil {
			return fmt.Errorf("lock session Workspace sync session: %w", err)
		}
		if sessionOwner != principal.UserID || replicaID != "session:"+principal.UserID ||
			folderID.Valid != preflightFolderID.Valid || folderID.Valid && folderID.String != preflightFolderID.String {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if sessionState == string(workspace.SyncCancelled) || sessionState == string(workspace.SyncExpired) || sessionState == string(workspace.SyncFailed) {
			return domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
		}
		if folderID.Valid {
			if err := requireWorkspaceSyncProjectHostBinding(ctx, tx, sessionOwner, workspaceID, folderID.String); err != nil {
				return err
			}
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
		var preflightOwner, preflightReplicaID string
		var preflightFolderID pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT owner_id,replica_id,folder_id FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2`,
			workspaceID, sessionID).Scan(&preflightOwner, &preflightReplicaID, &preflightFolderID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		} else if err != nil {
			return fmt.Errorf("read project folder before device sync access: %w", err)
		}
		if preflightOwner != principal.UserID || preflightReplicaID != "device:"+principal.TokenID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if preflightFolderID.Valid {
			if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, principal.UserID, workspaceID, preflightFolderID.String); err != nil {
				return err
			}
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		var ownerID, replicaID, state string
		var folderID pgtype.Text
		err = tx.QueryRow(ctx, `SELECT owner_id,replica_id,state,folder_id FROM workspace_sync_sessions
WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, sessionID).Scan(&ownerID, &replicaID, &state, &folderID)
		if err != nil {
			return storeError("lock Workspace sync download session", "workspace sync session", err)
		}
		if ownerID != principal.UserID || replicaID != "device:"+principal.TokenID ||
			folderID.Valid != preflightFolderID.Valid || folderID.Valid && folderID.String != preflightFolderID.String {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		if state == string(workspace.SyncCancelled) || state == string(workspace.SyncExpired) || state == string(workspace.SyncFailed) {
			return domain.NewError(domain.CodeTransferExpired, "workspace sync session is no longer active")
		}
		if err := requireActiveWorkspaceSyncDeviceSession(ctx, tx, access, ownerID, workspaceID, replicaID, folderID.String); err != nil {
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
func requireActiveWorkspaceSyncDeviceSession(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess, ownerID, workspaceID, replicaID string, folderIDs ...string) error {
	if access == nil {
		return nil
	}
	if access.ownerID != ownerID || access.workspaceID != workspaceID || replicaID != "device:"+access.tokenID {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	folderID := ""
	if len(folderIDs) > 0 {
		folderID = folderIDs[0]
	}
	if folderID != "" {
		return requireActiveWorkspaceSyncProjectFolder(ctx, tx, access, folderID)
	}
	return requireActiveWorkspaceSyncDeviceBinding(ctx, tx, access)
}

func requireActiveWorkspaceSyncProjectFolder(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess, folderID string) error {
	if access == nil || !workspace.ValidSyncFolderID(folderID) {
		return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM workspace_sync_folders f
  JOIN workspaces w ON w.id=f.workspace_id AND w.owner_id=f.owner_id
  WHERE f.owner_id=$1 AND f.workspace_id=$2 AND f.folder_id=$3 AND f.folder_kind='project'
    AND f.state IN ('pending_node','active') AND w.state='ready' AND w.deleted_at IS NULL
)`, access.ownerID, access.workspaceID, folderID).Scan(&active); err != nil {
		return fmt.Errorf("check Workspace project upload target: %w", err)
	}
	if !active {
		return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	return requireWorkspaceSyncProjectHostBinding(ctx, tx, access.ownerID, access.workspaceID, folderID)
}

func requireActiveWorkspaceSyncDeviceBinding(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1
  FROM workspace_sync_device_bindings b
  JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id
  JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id
  JOIN workspaces w ON w.id=b.workspace_id AND w.owner_id=b.owner_id
  WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.client_installation_id=$3
    AND b.state='active' AND i.revoked_at IS NULL AND f.state='active' AND f.folder_kind='workspace_root'
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

func requireActiveWorkspaceSyncDeviceWorkspaceAccess(ctx context.Context, tx pgx.Tx, access *workspaceSyncDeviceAccess) error {
	if access == nil {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err := requireActiveWorkspaceSyncDeviceBinding(ctx, tx, access); err == nil {
		return nil
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT f.folder_id
FROM workspace_sync_device_bindings b
JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id
JOIN workspaces w ON w.id=b.workspace_id AND w.owner_id=b.owner_id
WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.client_installation_id=$3
  AND b.state='active' AND i.revoked_at IS NULL AND f.folder_kind='project'
  AND f.state IN ('pending_node','active') AND w.state='ready' AND w.deleted_at IS NULL
ORDER BY f.folder_id`, access.ownerID, access.workspaceID, access.installationID)
	if err != nil {
		return fmt.Errorf("list active Workspace project device bindings: %w", err)
	}
	folderIDs := make([]string, 0)
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			rows.Close()
			return fmt.Errorf("read Workspace project device binding: %w", err)
		}
		folderIDs = append(folderIDs, folderID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Workspace project device bindings: %w", err)
	}
	rows.Close()
	for _, folderID := range folderIDs {
		if err := requireWorkspaceSyncProjectHostBinding(ctx, tx, access.ownerID, access.workspaceID, folderID); err == nil {
			return nil
		} else if domain.CodeOf(err) != domain.CodeWorkspaceUnavailable && domain.CodeOf(err) != domain.CodeNotFound {
			return err
		}
	}
	return domain.NewError(domain.CodeNotFound, "workspace not found")
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
