package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetWorkspaceBindingsByHost(ctx context.Context, hostID string) ([]workspace.Binding, error) {
	rows, err := s.q.ListWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("list workspace bindings by host: %w", err)
	}
	return mapRows(rows, workspaceBindingDomainFromList), nil
}

func (s *Store) GetWorkspaceBindingsByWorkspace(ctx context.Context, workspaceID string) ([]workspace.Binding, error) {
	rows, err := s.q.ListWorkspaceBindingsByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace bindings by workspace: %w", err)
	}
	return mapRows(rows, workspaceBindingDomainFromWorkspaceIDs), nil
}

func (s *Store) GetWorkspaceBinding(ctx context.Context, hostID, workspaceID string) (workspace.Binding, error) {
	row, err := s.q.GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
	if err != nil {
		return workspace.Binding{}, storeError("get workspace binding", "workspace binding", err)
	}
	return workspaceBindingDomain(row), nil
}

func (s *Store) CreateWorkspaceBinding(ctx context.Context, value workspace.Binding) (workspace.Binding, error) {
	if !domain.ValidHostID(value.HostID) || !domain.ValidWorkspaceID(value.Workspace.ID) ||
		!domain.ValidUserID(value.OwnerID) || value.Workspace.OwnerID != value.OwnerID ||
		value.Workspace.State != workspace.StateReady || value.Directory != value.Workspace.ID ||
		value.State != workspace.BindingPreparing || value.BindingVersion != 1 ||
		value.DesiredRevision < 0 || value.AppliedRevision != 0 || value.Workspace.Revision != value.DesiredRevision {
		return workspace.Binding{}, domain.InvalidArgument("workspace_binding", "workspace binding fields are invalid")
	}
	if value.ID == "" {
		value.ID = bindingID(value.HostID, value.Workspace.ID)
	}

	var created workspace.Binding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, hostState string
		err := tx.QueryRow(ctx, `SELECT owner_id, state FROM hosts WHERE id=$1 FOR UPDATE`, value.HostID).Scan(&ownerID, &hostState)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("lock host for workspace binding: %w", err)
		}
		if ownerID != value.OwnerID || hostState == "deleting" || hostState == "deleted" {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		// Portal and Runtime snapshots include detached bindings, so cap persisted rows too.
		var bindingCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_bindings WHERE host_id=$1`, value.HostID).Scan(&bindingCount); err != nil {
			return fmt.Errorf("count host workspace bindings: %w", err)
		}
		if bindingCount >= workspace.MaxBindingsPerHost {
			return domain.NewError(domain.CodeCapacityExceeded, "host has reached the workspace binding limit")
		}
		var workspaceOwner, workspaceState string
		var revision int64
		err = tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, value.Workspace.ID).Scan(&workspaceOwner, &workspaceState, &revision)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && workspaceOwner != value.OwnerID) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err != nil {
			return fmt.Errorf("lock workspace for binding: %w", err)
		}
		if workspaceState != string(workspace.StateReady) || revision != value.DesiredRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed before binding")
		}
		_, err = sqlcgen.New(tx).CreateWorkspaceBinding(ctx, sqlcgen.CreateWorkspaceBindingParams{
			ID: value.ID, OwnerID: value.OwnerID, HostID: value.HostID, WorkspaceID: value.Workspace.ID,
			DirectoryName: value.Directory, State: string(value.State), BindingVersion: value.BindingVersion,
			DesiredRevision: value.DesiredRevision, AppliedRevision: value.AppliedRevision,
		})
		if err != nil {
			return workspaceBindingInsertError(err)
		}
		row, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: value.HostID, WorkspaceID: value.Workspace.ID})
		if err != nil {
			return fmt.Errorf("read created workspace binding: %w", err)
		}
		created = workspaceBindingDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Binding{}, err
	}
	return created, nil
}

func (s *Store) TransitionWorkspaceBinding(
	ctx context.Context,
	hostID, workspaceID string,
	expectedVersion int64,
	from, to workspace.BindingState,
	desiredRevision, appliedRevision int64,
) (workspace.Binding, error) {
	return s.transitionWorkspaceBinding(ctx, 0, hostID, workspaceID, expectedVersion, from, to, desiredRevision, appliedRevision)
}

func (s *Store) ActivateWorkspaceBindingForHost(ctx context.Context, hostID string, expectedGeneration int64,
	workspaceID string, expectedVersion, desiredRevision, appliedRevision int64) (workspace.Binding, error) {
	if expectedGeneration < 1 {
		return workspace.Binding{}, domain.InvalidArgument("workspace_binding", "Host generation is invalid")
	}
	return s.transitionWorkspaceBinding(ctx, expectedGeneration, hostID, workspaceID, expectedVersion,
		workspace.BindingPreparing, workspace.BindingActive, desiredRevision, appliedRevision)
}

func (s *Store) transitionWorkspaceBinding(ctx context.Context, expectedGeneration int64,
	hostID, workspaceID string, expectedVersion int64, from, to workspace.BindingState,
	desiredRevision, appliedRevision int64) (workspace.Binding, error) {
	if err := workspace.ValidateBindingTransition(from, to); err != nil {
		return workspace.Binding{}, err
	}
	if !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) || expectedVersion <= 0 ||
		desiredRevision < 0 || appliedRevision < 0 || appliedRevision > desiredRevision {
		return workspace.Binding{}, domain.InvalidArgument("workspace_binding", "workspace binding transition is invalid")
	}

	var updated workspace.Binding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var hostState string
		var hostGeneration int64
		if err := tx.QueryRow(ctx, `SELECT state, runtime_generation FROM hosts WHERE id=$1 FOR UPDATE`, hostID).
			Scan(&hostState, &hostGeneration); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		} else if err != nil {
			return fmt.Errorf("lock host before binding transition: %w", err)
		}
		if expectedGeneration > 0 && hostGeneration != expectedGeneration {
			return domain.NewError(domain.CodeUnauthenticated, "host replica generation is no longer current")
		}
		if expectedGeneration > 0 && hostState != string(host.StateRunning) {
			return domain.NewError(domain.CodeWorkspaceBusy, "host must be running to activate a workspace binding")
		}
		if to == workspace.BindingPreparing && hostState != "running" {
			return domain.NewError(domain.CodeWorkspaceBusy, "host must be running to bind a workspace")
		}
		var currentWorkspaceState string
		var currentWorkspaceRevision int64
		if err := tx.QueryRow(ctx, `SELECT state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&currentWorkspaceState, &currentWorkspaceRevision); err != nil {
			return fmt.Errorf("lock workspace before binding transition: %w", err)
		}
		var bindingID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, hostID, workspaceID).Scan(&bindingID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace binding not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace binding before transition: %w", err)
		}
		current, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read locked workspace binding before transition: %w", err)
		}
		if currentWorkspaceState != string(workspace.StateReady) && to != workspace.BindingDetached {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for binding changes")
		}
		if current.BindingVersion != expectedVersion || current.BindingState != string(from) {
			return bindingStateConflict(ctx, tx, hostID, workspaceID)
		}
		if desiredRevision != current.DesiredRevision || appliedRevision != current.AppliedRevision || desiredRevision > currentWorkspaceRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace binding revision is stale")
		}
		if to == workspace.BindingActive && (appliedRevision != desiredRevision || desiredRevision != currentWorkspaceRevision) {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace binding has not applied the current workspace revision")
		}
		if to == workspace.BindingDetached && (appliedRevision != desiredRevision || desiredRevision != currentWorkspaceRevision) {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace binding has not reached a verified checkpoint")
		}
		if to == workspace.BindingDetached {
			if err := rejectUnfinishedHostWorkspaceSync(ctx, tx, hostID, workspaceID); err != nil {
				return err
			}
		}
		targetDesiredRevision := desiredRevision
		if to == workspace.BindingPreparing && (from == workspace.BindingDetached || from == workspace.BindingFailed) {
			targetDesiredRevision = currentWorkspaceRevision
		}
		result, err := tx.Exec(ctx, `
UPDATE workspace_bindings
SET state=$5, binding_version=binding_version+1, desired_revision=$8, updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state=$4
  AND desired_revision=$6 AND applied_revision=$7`,
			hostID, workspaceID, expectedVersion, string(from), string(to), desiredRevision, appliedRevision, targetDesiredRevision)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeWorkspaceBusy, "workspace is already bound to a host")
			}
			return fmt.Errorf("transition workspace binding: %w", err)
		}
		if result.RowsAffected() != 1 {
			return bindingStateConflict(ctx, tx, hostID, workspaceID)
		}
		row, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read transitioned workspace binding: %w", err)
		}
		updated = workspaceBindingDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Binding{}, err
	}
	return updated, nil
}

func (s *Store) CompleteWorkspaceBindingUnbind(ctx context.Context, hostID, workspaceID string, expectedVersion, expectedGeneration, checkpointRevision int64) (workspace.Binding, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) || expectedVersion <= 0 || expectedGeneration < 1 || checkpointRevision < 0 {
		return workspace.Binding{}, domain.InvalidArgument("workspace_binding", "workspace unbind checkpoint is invalid")
	}
	var updated workspace.Binding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var hostGeneration int64
		if err := tx.QueryRow(ctx, `SELECT runtime_generation FROM hosts WHERE id=$1 FOR UPDATE`, hostID).Scan(&hostGeneration); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		} else if err != nil {
			return fmt.Errorf("lock host before completing workspace unbind: %w", err)
		}
		if hostGeneration != expectedGeneration {
			return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
		}
		var workspaceState string
		var workspaceRevision int64
		if err := tx.QueryRow(ctx, `SELECT state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&workspaceState, &workspaceRevision); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace before completing unbind: %w", err)
		}
		var bindingID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, hostID, workspaceID).Scan(&bindingID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace binding not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace binding before completing unbind: %w", err)
		}
		q := sqlcgen.New(tx)
		current, err := q.GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read locked workspace binding before completing unbind: %w", err)
		}
		if current.BindingVersion != expectedVersion || current.BindingState != string(workspace.BindingUnbinding) {
			return bindingStateConflict(ctx, tx, hostID, workspaceID)
		}
		if workspaceState != string(workspace.StateReady) || checkpointRevision != workspaceRevision || checkpointRevision != current.DesiredRevision || checkpointRevision != current.AppliedRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed after the Host checkpoint")
		}
		if err := rejectUnfinishedHostWorkspaceSync(ctx, tx, hostID, workspaceID); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `
UPDATE workspace_bindings
SET state='detached', binding_version=binding_version+1,
    desired_revision=$4, updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state='unbinding'
  AND desired_revision=$4 AND applied_revision=$4`, hostID, workspaceID, expectedVersion, checkpointRevision)
		if err != nil {
			return fmt.Errorf("complete workspace unbind: %w", err)
		}
		if result.RowsAffected() != 1 {
			return bindingStateConflict(ctx, tx, hostID, workspaceID)
		}
		row, err := q.GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read completed workspace unbind: %w", err)
		}
		updated = workspaceBindingDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Binding{}, err
	}
	return updated, nil
}

func rejectUnfinishedHostWorkspaceSync(ctx context.Context, tx pgx.Tx, hostID, workspaceID string) error {
	pattern := fmt.Sprintf("host:%s:g%%:%s:b%%", hostID, workspaceID)
	var unfinished bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM workspace_sync_sessions
  WHERE workspace_id=$1 AND replica_id LIKE $2
    AND state IN ('planned','transferring','verifying','applying')
)`, workspaceID, pattern).Scan(&unfinished); err != nil {
		return fmt.Errorf("check unfinished Host workspace sync sessions: %w", err)
	}
	if unfinished {
		return domain.NewError(domain.CodeWorkspaceBusy, "Host workspace synchronization is still in progress")
	}
	return nil
}

func (s *Store) AdvanceWorkspaceBindingAppliedRevision(ctx context.Context, hostID string, hostGeneration int64, workspaceID string, expectedVersion int64, sessionID string, revision int64) (workspace.Binding, error) {
	if !domain.ValidHostID(hostID) || hostGeneration < 1 || !domain.ValidWorkspaceID(workspaceID) || expectedVersion <= 0 ||
		!workspace.ValidSessionID(sessionID) || revision < 0 {
		return workspace.Binding{}, domain.InvalidArgument("workspace_binding", "workspace applied revision receipt is invalid")
	}
	var updated workspace.Binding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var hostOwner string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, runtime_generation FROM hosts WHERE id=$1 FOR SHARE`, hostID).
			Scan(&hostOwner, &currentGeneration); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		} else if err != nil {
			return fmt.Errorf("lock Host before applied revision receipt: %w", err)
		}
		if currentGeneration != hostGeneration {
			return domain.NewError(domain.CodeUnauthenticated, "host replica generation is no longer current")
		}
		var workspaceState string
		var workspaceRevision int64
		if err := tx.QueryRow(ctx, `SELECT state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&workspaceState, &workspaceRevision); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace before applied revision receipt: %w", err)
		}
		var bindingID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, hostID, workspaceID).Scan(&bindingID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace binding not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace binding before applied revision receipt: %w", err)
		}
		current, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read locked workspace binding before applied revision receipt: %w", err)
		}
		if current.BindingVersion != expectedVersion || (current.BindingState != string(workspace.BindingPreparing) &&
			current.BindingState != string(workspace.BindingActive) && current.BindingState != string(workspace.BindingUnbinding)) {
			return bindingStateConflict(ctx, tx, hostID, workspaceID)
		}
		if workspaceState != string(workspace.StateReady) || revision > workspaceRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace applied revision is not available")
		}
		row, err := sqlcgen.New(tx).LockWorkspaceSyncSession(ctx, sqlcgen.LockWorkspaceSyncSessionParams{
			WorkspaceID: workspaceID, ID: sessionID,
		})
		if err != nil {
			return storeError("lock Host workspace applied receipt session", "workspace sync session", err)
		}
		expectedReplicaID := fmt.Sprintf("host:%s:g%d:%s:b%d", hostID, hostGeneration, workspaceID, expectedVersion)
		if row.OwnerID != current.OwnerID || row.OwnerID != hostOwner || row.ReplicaID != expectedReplicaID {
			return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
		}
		validDirection := row.Direction == string(workspace.SyncDownload) || row.Direction == string(workspace.SyncBidirectional)
		validPolicy := row.Policy == string(workspace.SyncFillMissing) || row.Policy == string(workspace.SyncLive)
		if row.State != string(workspace.SyncCompleted) || !validDirection || !validPolicy || row.TargetRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace revision was not fully applied by a completed download session")
		}
		var conflicts []workspace.Conflict
		if err := json.Unmarshal(row.Conflicts, &conflicts); err != nil {
			return fmt.Errorf("decode Host workspace applied receipt conflicts: %w", err)
		}
		if len(conflicts) != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace revision was not fully applied by a conflict-free download session")
		}
		if revision > current.AppliedRevision {
			result, err := tx.Exec(ctx, `UPDATE workspace_bindings SET applied_revision=$4, updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND applied_revision < $4`, hostID, workspaceID, expectedVersion, revision)
			if err != nil {
				return fmt.Errorf("advance workspace binding applied revision: %w", err)
			}
			if result.RowsAffected() != 1 {
				return bindingStateConflict(ctx, tx, hostID, workspaceID)
			}
			current, err = sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
			if err != nil {
				return fmt.Errorf("read advanced workspace binding receipt: %w", err)
			}
		}
		updated = workspaceBindingDomain(current)
		return nil
	})
	if err != nil {
		return workspace.Binding{}, err
	}
	return updated, nil
}

func (s *Store) SetWorkspaceBindingSyncStatus(ctx context.Context, hostID, workspaceID string, expectedVersion int64, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	return s.setWorkspaceBindingSyncStatus(ctx, hostID, 0, workspaceID, expectedVersion, state, conflicts)
}

func (s *Store) SetWorkspaceBindingSyncStatusForHost(ctx context.Context, hostID string, expectedGeneration int64,
	workspaceID string, expectedVersion int64, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	if expectedGeneration < 1 {
		return workspace.Binding{}, domain.InvalidArgument("sync_status", "Host generation is invalid")
	}
	return s.setWorkspaceBindingSyncStatus(ctx, hostID, expectedGeneration, workspaceID, expectedVersion, state, conflicts)
}

func (s *Store) setWorkspaceBindingSyncStatus(ctx context.Context, hostID string, expectedGeneration int64,
	workspaceID string, expectedVersion int64, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) || expectedVersion < 1 ||
		workspace.ValidateBindingSyncStatus(state, conflicts) != nil {
		return workspace.Binding{}, domain.InvalidArgument("sync_status", "workspace sync status is invalid")
	}
	if conflicts == nil {
		conflicts = []string{}
	}
	var updated workspace.Binding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var hostState string
		var hostGeneration int64
		if err := tx.QueryRow(ctx, `SELECT state, runtime_generation FROM hosts WHERE id=$1 FOR UPDATE`, hostID).
			Scan(&hostState, &hostGeneration); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		} else if err != nil {
			return fmt.Errorf("lock host before workspace sync status: %w", err)
		}
		if expectedGeneration > 0 && hostGeneration != expectedGeneration {
			return domain.NewError(domain.CodeUnauthenticated, "host replica generation is no longer current")
		}
		if hostState != string(host.StateRunning) {
			return domain.NewError(domain.CodeWorkspaceBusy, "host must be running to report workspace sync status")
		}
		var workspaceState string
		if err := tx.QueryRow(ctx, `SELECT state FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&workspaceState); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace before sync status: %w", err)
		}
		if workspaceState != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is unavailable for sync status updates")
		}
		var bindingID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, hostID, workspaceID).Scan(&bindingID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace binding not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace binding before sync status: %w", err)
		}
		current, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read workspace binding before sync status: %w", err)
		}
		if current.BindingVersion != expectedVersion {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace binding version changed; refresh and retry")
		}
		if current.BindingState != string(workspace.BindingActive) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace binding must be active to report sync status")
		}
		if current.SyncState == string(state) && equalStringSlices(current.SyncConflicts, conflicts) {
			updated = workspaceBindingDomain(current)
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_bindings SET sync_state=$4, sync_conflicts=$5, updated_at=now()
WHERE host_id=$1 AND workspace_id=$2 AND binding_version=$3 AND state='active'`,
			hostID, workspaceID, expectedVersion, string(state), conflicts); err != nil {
			return fmt.Errorf("update workspace binding sync status: %w", err)
		}
		row, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("read updated workspace binding sync status: %w", err)
		}
		updated = workspaceBindingDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Binding{}, err
	}
	return updated, nil
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func bindingStateConflict(ctx context.Context, tx pgx.Tx, hostID, workspaceID string) error {
	current, err := sqlcgen.New(tx).GetWorkspaceBinding(ctx, sqlcgen.GetWorkspaceBindingParams{HostID: hostID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if err != nil {
		return fmt.Errorf("check workspace binding after conflict: %w", err)
	}
	conflict := domain.NewError(domain.CodeInvalidState, "workspace binding changed")
	conflict.Details["state"] = current.BindingState
	conflict.Details["binding_version"] = current.BindingVersion
	return conflict
}

func (s *Store) ClaimWorkspaceForDelete(ctx context.Context, workspaceID string, expectedRevision int64) (workspace.Workspace, error) {
	if !domain.ValidWorkspaceID(workspaceID) || expectedRevision < 0 {
		return workspace.Workspace{}, domain.InvalidArgument("workspace", "workspace deletion identity is invalid")
	}
	var claimed workspace.Workspace
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock workspace before deletion: %w", err)
		}
		row, err := sqlcgen.New(tx).GetWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("read workspace before deletion: %w", err)
		}
		if row.Revision != expectedRevision {
			return workspaceRevisionConflict(row.Revision, expectedRevision)
		}
		if row.State != string(workspace.StateReady) {
			return workspaceStateConflict(workspaceDomain(row))
		}
		var bound bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM workspace_bindings WHERE workspace_id=$1 AND state <> 'detached')
    OR EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying'))
    OR EXISTS (SELECT 1 FROM workspace_sync_folders WHERE workspace_id=$1 AND state IN ('pending_node','active','revoking'))
    OR EXISTS (SELECT 1 FROM workspace_sync_receive_reservations WHERE workspace_id=$1 AND state IN ('reserved','aborting'))
    OR EXISTS (
      SELECT 1 FROM workspace_sync_folders f
      JOIN workspace_sync_node_revisions r ON r.owner_id=f.owner_id
      WHERE f.workspace_id=$1 AND r.desired_revision>r.applied_revision
    )`, workspaceID).Scan(&bound); err != nil {
			return fmt.Errorf("check workspace bindings before deletion: %w", err)
		}
		if bound {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace has an active binding or synchronization session")
		}
		result, err := tx.Exec(ctx, `
UPDATE workspaces SET state='deleting', updated_at=now()
WHERE id=$1 AND state='ready' AND revision=$2`, workspaceID, expectedRevision)
		if err != nil {
			return fmt.Errorf("claim workspace for deletion: %w", err)
		}
		if result.RowsAffected() != 1 {
			return workspaceStateConflict(workspaceDomain(row))
		}
		updated, err := sqlcgen.New(tx).GetWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("read claimed workspace: %w", err)
		}
		claimed = workspaceDomain(updated)
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, err
	}
	return claimed, nil
}

func (s *Store) CompleteWorkspaceDelete(ctx context.Context, workspaceID string, expectedRevision int64) (workspace.Workspace, error) {
	if !domain.ValidWorkspaceID(workspaceID) || expectedRevision < 0 {
		return workspace.Workspace{}, domain.InvalidArgument("workspace", "workspace deletion identity is invalid")
	}
	var deleted workspace.Workspace
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, state string
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&ownerID, &state, &revision); err != nil {
			return storeError("lock workspace to complete deletion", "workspace", err)
		}
		if state == string(workspace.StateDeleted) {
			row, err := sqlcgen.New(tx).GetWorkspace(ctx, workspaceID)
			if err != nil {
				return fmt.Errorf("read deleted workspace: %w", err)
			}
			deleted = workspaceDomain(row)
			return nil
		}
		if state != string(workspace.StateDeleting) || revision != expectedRevision {
			row, err := sqlcgen.New(tx).GetWorkspace(ctx, workspaceID)
			if err != nil {
				return fmt.Errorf("read workspace after delete conflict: %w", err)
			}
			return workspaceStateConflict(workspaceDomain(row))
		}
		var allocatedBytes, reservedBytes int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(sum(allocated_bytes),0), COALESCE(sum(reserved_bytes),0)
FROM workspace_block_usage WHERE workspace_id=$1`, workspaceID).Scan(&allocatedBytes, &reservedBytes); err != nil {
			return fmt.Errorf("sum workspace block usage before deletion: %w", err)
		}
		ownerAllocated, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		if ownerAllocated < allocatedBytes || ownerReserved < reservedBytes {
			return domain.NewError(domain.CodeInternal, "workspace owner block usage counters are inconsistent")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_block_usage WHERE workspace_id=$1`, workspaceID); err != nil {
			return fmt.Errorf("remove workspace block usage after deletion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspace_owner_block_usage SET allocated_bytes=$2, reserved_bytes=$3, updated_at=now()
WHERE owner_id=$1`, ownerID, ownerAllocated-allocatedBytes, ownerReserved-reservedBytes); err != nil {
			return fmt.Errorf("debit workspace owner usage after deletion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspaces SET state='deleted', deleted_at=now(), updated_at=now()
WHERE id=$1 AND state='deleting' AND revision=$2`, workspaceID, expectedRevision); err != nil {
			return fmt.Errorf("complete workspace deletion: %w", err)
		}
		row, err := sqlcgen.New(tx).GetWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("read completed workspace deletion: %w", err)
		}
		deleted = workspaceDomain(row)
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, err
	}
	return deleted, nil
}

func workspaceRevisionConflict(current, expected int64) error {
	conflict := domain.NewError(domain.CodeWorkspaceConflict, "workspace revision changed")
	conflict.Details["current_revision"] = current
	conflict.Details["expected_revision"] = expected
	return conflict
}

func (s *Store) SetWorkspaceError(ctx context.Context, workspaceID string, expected workspace.State, message string) error {
	result, err := s.pool.Exec(ctx, `UPDATE workspaces SET last_error=$3, updated_at=now() WHERE id=$1 AND state=$2`, workspaceID, string(expected), message)
	if err != nil {
		return fmt.Errorf("set workspace error: %w", err)
	}
	if result.RowsAffected() != 0 {
		return nil
	}
	current, err := s.GetWorkspace(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err != nil {
		return fmt.Errorf("check workspace error state: %w", err)
	}
	return workspaceStateConflict(current)
}

var _ app.WorkspaceStore = (*Store)(nil)
var _ app.HostReplicaWorkspaceStore = (*Store)(nil)
