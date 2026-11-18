package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetAgentWorkspace(ctx context.Context, ownerID string) (workspace.Workspace, bool, error) {
	if !domain.ValidUserID(ownerID) {
		return workspace.Workspace{}, false, domain.NewError(domain.CodeNotFound, "Agent Workspace not found")
	}
	var workspaceID string
	var initialized bool
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id,directory_initialized_at IS NOT NULL
FROM user_agent_workspaces WHERE owner_id=$1`, ownerID).Scan(&workspaceID, &initialized); errors.Is(err, pgx.ErrNoRows) {
		return workspace.Workspace{}, false, domain.NewError(domain.CodeNotFound, "Agent Workspace not found")
	} else if err != nil {
		return workspace.Workspace{}, false, fmt.Errorf("read stable Agent Workspace mapping: %w", err)
	}
	row, err := s.q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, false, storeError("read Agent Workspace", "workspace", err)
	}
	value := workspaceDomain(row)
	if value.OwnerID != ownerID {
		return workspace.Workspace{}, false, domain.NewError(domain.CodeWorkspaceConflict, "Agent Workspace ownership mapping is inconsistent")
	}
	return value, initialized, nil
}

// CreateAgentWorkspace serializes first-time resolution on the owner row, so
// concurrent requests create one Workspace and one stable mapping. Existing
// Workspaces are never selected by name or Host binding.
func (s *Store) CreateAgentWorkspace(ctx context.Context, value workspace.Workspace) (workspace.Workspace, bool, error) {
	if !domain.ValidWorkspaceID(value.ID) || !domain.ValidUserID(value.OwnerID) || value.Name != "Agent Workspace" ||
		value.State != workspace.StateReady || value.Revision != 0 || value.UsedBytes != 0 || value.QuotaBytes <= 0 {
		return workspace.Workspace{}, false, domain.InvalidArgument("agent_workspace", "Agent Workspace creation fields are invalid")
	}
	var result workspace.Workspace
	var initialized bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedOwner string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, value.OwnerID).Scan(&lockedOwner); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "Agent Workspace owner not found")
		} else if err != nil {
			return fmt.Errorf("lock owner while ensuring Agent Workspace: %w", err)
		}

		var existingID string
		if err := tx.QueryRow(ctx, `SELECT workspace_id,directory_initialized_at IS NOT NULL
FROM user_agent_workspaces WHERE owner_id=$1`, value.OwnerID).Scan(&existingID, &initialized); err == nil {
			row, err := sqlcgen.New(tx).GetWorkspace(ctx, existingID)
			if err != nil {
				return storeError("read stable Agent Workspace", "workspace", err)
			}
			result = workspaceDomain(row)
			return validateStoredAgentWorkspace(result, value.OwnerID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check stable Agent Workspace mapping: %w", err)
		}

		created, err := createWorkspaceInTx(ctx, tx, value)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_agent_workspaces (owner_id,workspace_id) VALUES ($1,$2)`, value.OwnerID, value.ID); err != nil {
			return fmt.Errorf("record stable Agent Workspace mapping: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_workspace_history (workspace_id,owner_id) VALUES ($1,$2)`, value.ID, value.OwnerID); err != nil {
			return fmt.Errorf("record Agent Workspace history: %w", err)
		}
		result = created
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, false, err
	}
	return result, initialized, nil
}

// AdoptExistingWorkspaceAsAgentWorkspace switches an owner to an explicitly
// selected, already materialized Workspace during offline migration. The
// caller must verify the current directory against the selected revision.
func (s *Store) AdoptExistingWorkspaceAsAgentWorkspace(ctx context.Context, ownerID, workspaceID string, expectedRevision, expectedUsedBytes int64) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || expectedRevision < 0 || expectedUsedBytes < 0 {
		return domain.InvalidArgument("agent_workspace", "Agent Workspace adoption fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedOwner string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, ownerID).Scan(&lockedOwner); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "Agent Workspace owner not found")
		} else if err != nil {
			return fmt.Errorf("lock owner for Agent Workspace adoption: %w", err)
		}

		var sourceOwner, sourceState string
		var sourceRevision int64
		var sourceUsedBytes int64
		var sourceReadOnly bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,used_bytes,is_read_only FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&sourceOwner, &sourceState, &sourceRevision, &sourceUsedBytes, &sourceReadOnly); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "source Workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock source Workspace for Agent Workspace adoption: %w", err)
		}
		if sourceOwner != ownerID || sourceState != string(workspace.StateReady) || sourceReadOnly || sourceRevision != expectedRevision || sourceUsedBytes != expectedUsedBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "source Workspace ownership, state, or revision changed before adoption")
		}
		var sourceVolumeState string
		if err := tx.QueryRow(ctx, `SELECT state FROM workspace_volumes WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&sourceVolumeState); err != nil {
			return fmt.Errorf("lock source Workspace volume inventory: %w", err)
		}
		if sourceVolumeState != "legacy" {
			return domain.NewError(domain.CodeWorkspaceConflict, "source Workspace Host volume must be reconciled before Agent Workspace adoption")
		}
		var legacyImportState string
		if err := tx.QueryRow(ctx, `SELECT import_state FROM workspace_legacy_volumes WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&legacyImportState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeWorkspaceConflict, "source legacy Host volume has no explicit import audit record")
			}
			return fmt.Errorf("lock source legacy Host volume audit: %w", err)
		}
		if legacyImportState != "imported" && legacyImportState != "verified" && legacyImportState != "not_required" {
			return domain.NewError(domain.CodeWorkspaceConflict, "source legacy Host volume must be imported and verified before Agent Workspace adoption")
		}
		if err := requireAgentWorkspaceAdoptionMaterializedRevision(ctx, tx, workspaceID, expectedRevision); err != nil {
			return err
		}
		if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, workspaceID); err != nil {
			return err
		}
		if err := ensureWorkspaceLegacyImportHasNoActiveWork(ctx, tx, workspaceID); err != nil {
			return err
		}
		if err := requireNoActiveWorkspaceTasksForAgentAdoption(ctx, tx, workspaceID); err != nil {
			return err
		}

		// Host lifecycle rows stay locked while the cutover checks their bindings.
		hostRows, err := tx.Query(ctx, `SELECT id,state FROM hosts WHERE owner_id=$1 ORDER BY id FOR UPDATE`, ownerID)
		if err != nil {
			return fmt.Errorf("lock owner Hosts for Agent Workspace adoption: %w", err)
		}
		for hostRows.Next() {
			var hostID, hostState string
			if err := hostRows.Scan(&hostID, &hostState); err != nil {
				hostRows.Close()
				return fmt.Errorf("read owner Host during Agent Workspace adoption: %w", err)
			}
			if hostState != "stopped" && hostState != "deleted" {
				hostRows.Close()
				return domain.NewError(domain.CodeWorkspaceBusy, "all owner Hosts must be stopped before Agent Workspace adoption")
			}
		}
		if err := hostRows.Err(); err != nil {
			hostRows.Close()
			return fmt.Errorf("iterate owner Hosts during Agent Workspace adoption: %w", err)
		}
		hostRows.Close()

		bindingRows, err := tx.Query(ctx, `SELECT b.host_id,b.workspace_id,b.state,h.state,b.sync_state,
cardinality(b.sync_conflicts),b.binding_version,b.desired_revision,b.applied_revision
FROM workspace_bindings b JOIN hosts h ON h.id=b.host_id
WHERE h.owner_id=$1 AND b.state <> 'detached'
ORDER BY b.host_id,b.workspace_id FOR UPDATE OF b`, ownerID)
		if err != nil {
			return fmt.Errorf("lock active Host bindings during Agent Workspace adoption: %w", err)
		}
		activeBindingCount := 0
		var adoptedHostID string
		var adoptedBindingVersion int64
		for bindingRows.Next() {
			var bindingHostID, bindingWorkspaceID, bindingState, bindingHostState, syncState string
			var syncConflictCount int
			var bindingVersion, desiredRevision, appliedRevision int64
			if err := bindingRows.Scan(&bindingHostID, &bindingWorkspaceID, &bindingState, &bindingHostState,
				&syncState, &syncConflictCount, &bindingVersion, &desiredRevision, &appliedRevision); err != nil {
				bindingRows.Close()
				return fmt.Errorf("read active Host binding during Agent Workspace adoption: %w", err)
			}
			activeBindingCount++
			if activeBindingCount > 1 || bindingWorkspaceID != workspaceID || bindingState != "active" || bindingHostState != "stopped" {
				bindingRows.Close()
				return domain.NewError(domain.CodeWorkspaceBusy, "only one active binding to the selected Workspace on a stopped Host may remain during adoption")
			}
			if syncState != "healthy" || syncConflictCount != 0 {
				bindingRows.Close()
				return domain.NewError(domain.CodeWorkspaceConflict, "source Host binding has unresolved Workspace sync conflicts")
			}
			if desiredRevision != appliedRevision {
				bindingRows.Close()
				return domain.NewError(domain.CodeWorkspaceBusy, "source Host Workspace sync is not settled")
			}
			adoptedHostID, adoptedBindingVersion = bindingHostID, bindingVersion
		}
		if err := bindingRows.Err(); err != nil {
			bindingRows.Close()
			return fmt.Errorf("iterate active Host bindings during Agent Workspace adoption: %w", err)
		}
		bindingRows.Close()

		var currentWorkspaceID string
		mappingErr := tx.QueryRow(ctx, `SELECT workspace_id FROM user_agent_workspaces WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&currentWorkspaceID)
		if mappingErr != nil && !errors.Is(mappingErr, pgx.ErrNoRows) {
			return fmt.Errorf("lock current Agent Workspace mapping for adoption: %w", mappingErr)
		}
		if mappingErr == nil && currentWorkspaceID == workspaceID {
			return nil
		}

		var alreadyRetired bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_workspace_history WHERE workspace_id=$1)`, workspaceID).Scan(&alreadyRetired); err != nil {
			return fmt.Errorf("check source Agent Workspace history: %w", err)
		}
		if alreadyRetired {
			return domain.NewError(domain.CodeWorkspaceConflict, "a retired Agent Workspace cannot be adopted again")
		}

		if mappingErr == nil {
			var currentOwner, currentState string
			var currentRevision, currentUsedBytes int64
			var currentReadOnly bool
			if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,used_bytes,is_read_only FROM workspaces WHERE id=$1 FOR UPDATE`, currentWorkspaceID).
				Scan(&currentOwner, &currentState, &currentRevision, &currentUsedBytes, &currentReadOnly); err != nil {
				return storeError("lock current Agent Workspace before adoption", "workspace", err)
			}
			if currentOwner != ownerID || currentState != string(workspace.StateReady) || currentRevision != 0 || currentUsedBytes != 0 || currentReadOnly {
				return domain.NewError(domain.CodeWorkspaceConflict, "current Agent Workspace is not empty and cannot be replaced")
			}
			if err := requireAgentWorkspaceAdoptionMaterializedRevision(ctx, tx, currentWorkspaceID, 0); err != nil {
				return err
			}
			if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, currentWorkspaceID); err != nil {
				return err
			}
			if err := ensureWorkspaceLegacyImportHasNoActiveWork(ctx, tx, currentWorkspaceID); err != nil {
				return err
			}
			if err := requireNoActiveWorkspaceTasksForAgentAdoption(ctx, tx, currentWorkspaceID); err != nil {
				return err
			}
			var currentBindings int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_bindings WHERE workspace_id=$1 AND state <> 'detached'`, currentWorkspaceID).Scan(&currentBindings); err != nil {
				return fmt.Errorf("check current Agent Workspace bindings: %w", err)
			}
			if currentBindings != 0 {
				return domain.NewError(domain.CodeWorkspaceBusy, "current Agent Workspace is still bound to a Host")
			}
		} else if !errors.Is(mappingErr, pgx.ErrNoRows) {
			return mappingErr
		}

		if mappingErr == nil {
			if _, err := tx.Exec(ctx, `UPDATE user_agent_workspaces SET workspace_id=$2,directory_initialized_at=clock_timestamp() WHERE owner_id=$1`, ownerID, workspaceID); err != nil {
				return fmt.Errorf("switch stable Agent Workspace mapping: %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO user_agent_workspaces (owner_id,workspace_id,directory_initialized_at) VALUES ($1,$2,clock_timestamp())`, ownerID, workspaceID); err != nil {
				return fmt.Errorf("adopt source as stable Agent Workspace: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_workspace_history (workspace_id,owner_id) VALUES ($1,$2)`, workspaceID, ownerID); err != nil {
			return fmt.Errorf("record adopted Agent Workspace history: %w", err)
		}
		if adoptedHostID != "" {
			result, err := tx.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=$3,applied_revision=$3,
binding_version=binding_version+1,updated_at=clock_timestamp()
WHERE host_id=$1 AND workspace_id=$2 AND state='active' AND binding_version=$4`,
				adoptedHostID, workspaceID, expectedRevision, adoptedBindingVersion)
			if err != nil {
				return fmt.Errorf("align stopped source Host binding with adopted Workspace revision: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "source Host binding changed during Agent Workspace adoption")
			}
		}
		return nil
	})
}

func requireAgentWorkspaceAdoptionMaterializedRevision(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) error {
	var desiredRevision, appliedRevision int64
	if err := tx.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, workspaceID).
		Scan(&desiredRevision, &appliedRevision); err != nil {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current directory is not ready for adoption")
	}
	if desiredRevision != revision || appliedRevision != revision {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current directory does not match the selected revision")
	}
	return nil
}

func requireNoActiveWorkspaceTasksForAgentAdoption(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM tasks WHERE workspace_id=$1 AND status IN ('queued','running','waiting_user','pausing','cancelling')
)`, workspaceID).Scan(&active); err != nil {
		return fmt.Errorf("check active Workspace Tasks for Agent adoption: %w", err)
	}
	if active {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has active tasks and cannot be adopted")
	}
	return nil
}

func (s *Store) AcquireAgentWorkspaceInitializationLock(ctx context.Context, ownerID string) (func() error, error) {
	if !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("owner_id", "Agent Workspace owner is invalid")
	}
	if s.pool.Config().MaxConns < 3 {
		return nil, errors.New("agent Workspace initialization requires at least three database connections")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire Agent Workspace initialization lock connection: %w", err)
	}
	lockKey := "morde:agent-workspace-init:" + ownerID
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		closeHijackedConnection(conn)
		return nil, fmt.Errorf("lock Agent Workspace initialization: %w", err)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey).Scan(&unlocked); err != nil {
				releaseErr = fmt.Errorf("unlock Agent Workspace initialization: %w", err)
				closeHijackedConnection(conn)
				return
			}
			if !unlocked {
				releaseErr = errors.New("agent Workspace initialization lock was not held")
				closeHijackedConnection(conn)
				return
			}
			conn.Release()
		})
		return releaseErr
	}, nil
}

func (s *Store) IsAgentWorkspaceWorkspace(ctx context.Context, ownerID, workspaceID string) (bool, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return false, domain.InvalidArgument("agent_workspace", "Agent Workspace identity is invalid")
	}
	var found bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM agent_workspace_history WHERE owner_id=$1 AND workspace_id=$2
)`, ownerID, workspaceID).Scan(&found); err != nil {
		return false, fmt.Errorf("check Agent Workspace history: %w", err)
	}
	return found, nil
}

func (s *Store) MarkAgentWorkspaceDirectoryInitialized(ctx context.Context, ownerID, workspaceID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return domain.InvalidArgument("agent_workspace", "Agent Workspace identity is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE user_agent_workspaces
SET directory_initialized_at=COALESCE(directory_initialized_at,clock_timestamp())
WHERE owner_id=$1 AND workspace_id=$2`, ownerID, workspaceID)
	if err != nil {
		return fmt.Errorf("record Agent Workspace directory initialization: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "Agent Workspace mapping changed during directory initialization")
	}
	return nil
}

func validateStoredAgentWorkspace(value workspace.Workspace, ownerID string) error {
	if value.OwnerID != ownerID || value.State == workspace.StateDeleted || value.DeletedAt != nil {
		return domain.NewError(domain.CodeWorkspaceConflict, "stable Agent Workspace is unavailable or has inconsistent ownership")
	}
	if value.State != workspace.StateReady {
		return domain.NewError(domain.CodeWorkspaceBusy, "stable Agent Workspace is not ready")
	}
	return nil
}

func isAgentWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_workspace_history WHERE workspace_id=$1)`, workspaceID).Scan(&found); err != nil {
		return false, fmt.Errorf("check stable Agent Workspace mapping: %w", err)
	}
	return found, nil
}

func rejectAgentWorkspaceMutation(ctx context.Context, tx pgx.Tx, workspaceID, action string) error {
	found, err := isAgentWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if found {
		return domain.NewError(domain.CodeWorkspaceConflict, "stable Agent Workspace cannot be "+action+"; its directory is reserved for the owner")
	}
	return nil
}

var _ app.AgentWorkspaceStore = (*Store)(nil)
var _ app.AgentWorkspaceDeleteStore = (*Store)(nil)
