package postgres

import (
	"context"
	"fmt"
	"hash/fnv"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListWorkspaceCurrentMaterializations(ctx context.Context, afterWorkspaceID string, limit int32) ([]app.WorkspaceCurrentMaterialization, error) {
	if (afterWorkspaceID != "" && !domain.ValidWorkspaceID(afterWorkspaceID)) || limit < 1 || limit > 1000 {
		return nil, domain.InvalidArgument("limit", "workspace current reconciliation limit is out of range")
	}
	rows, err := s.pool.Query(ctx, `
SELECT m.workspace_id, w.owner_id, m.desired_revision, m.applied_revision
FROM workspace_current_materializations m
JOIN workspaces w ON w.id=m.workspace_id
WHERE m.workspace_id > $1 AND w.state='ready' AND w.deleted_at IS NULL
ORDER BY m.workspace_id
LIMIT $2`, afterWorkspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Workspace current materializations: %w", err)
	}
	defer rows.Close()
	items := make([]app.WorkspaceCurrentMaterialization, 0)
	for rows.Next() {
		var item app.WorkspaceCurrentMaterialization
		if err := rows.Scan(&item.WorkspaceID, &item.OwnerID, &item.DesiredRevision, &item.AppliedRevision); err != nil {
			return nil, fmt.Errorf("scan Workspace current materialization: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Workspace current materializations: %w", err)
	}
	return items, nil
}

func (s *Store) MarkWorkspaceCurrentMaterialized(ctx context.Context, workspaceID string, revision int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || revision < 0 {
		return domain.InvalidArgument("workspace_current", "Workspace current revision is invalid")
	}
	return s.markWorkspaceCurrentMaterialized(ctx, workspaceID, revision, "")
}

func (s *Store) MarkWorkspaceCurrentMaterializedForReceive(ctx context.Context, workspaceID string, revision int64, reservationID string) error {
	if !domain.ValidWorkspaceID(workspaceID) || revision < 0 || !domain.ValidPendingRequestID(reservationID) {
		return domain.InvalidArgument("workspace_current", "Workspace current revision or receive reservation is invalid")
	}
	return s.markWorkspaceCurrentMaterialized(ctx, workspaceID, revision, reservationID)
}

func (s *Store) BeginWorkspaceCurrentMaterialization(ctx context.Context, workspaceID string, revision int64, allowedReservationID string) error {
	if !domain.ValidWorkspaceID(workspaceID) || revision < 0 || (allowedReservationID != "" && !domain.ValidPendingRequestID(allowedReservationID)) {
		return domain.InvalidArgument("workspace_current", "Workspace current materialization identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, state string
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&ownerID, &state, &currentRevision); err != nil {
			return storeError("lock Workspace before current materialization", "workspace", err)
		}
		if state != string(workspace.StateReady) {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available for current materialization")
		}
		if currentRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace revision changed before current materialization")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, allowedReservationID); err != nil {
			return err
		}
		if allowedReservationID != "" {
			reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, allowedReservationID, true)
			if err != nil {
				return err
			}
			if !found || reservation.State != "aborting" {
				return domain.NewError(domain.CodeWorkspaceConflict, "only an aborting receive reservation may recover its Workspace directory")
			}
		}
		var desiredRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, workspaceID).
			Scan(&desiredRevision); err != nil {
			return storeError("lock Workspace current materialization state", "Workspace current materialization", err)
		}
		if desiredRevision != currentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current materialization target is not its latest revision")
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=-1, updated_at=now()
WHERE workspace_id=$1 AND desired_revision=$2`, workspaceID, currentRevision)
		if err != nil {
			return fmt.Errorf("invalidate Workspace current revision before materialization: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current materialization target changed before it began")
		}
		return nil
	})
}

func (s *Store) markWorkspaceCurrentMaterialized(ctx context.Context, workspaceID string, revision int64, allowedReservationID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, state string
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).
			Scan(&ownerID, &state, &currentRevision); err != nil {
			return storeError("lock Workspace before marking current materialization", "workspace", err)
		}
		if state != string(workspace.StateReady) || currentRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace revision changed during current materialization")
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, workspaceID, allowedReservationID); err != nil {
			return err
		}
		if allowedReservationID != "" {
			reservation, found, err := getWorkspaceSyncReceiveReservationByID(ctx, tx, ownerID, workspaceID, allowedReservationID, true)
			if err != nil {
				return err
			}
			if !found || reservation.State != "aborting" {
				return domain.NewError(domain.CodeWorkspaceConflict, "only an aborting receive reservation may mark its Workspace directory recovered")
			}
		}
		var desiredRevision, appliedRevision int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1 FOR UPDATE`, workspaceID).
			Scan(&desiredRevision, &appliedRevision); err != nil {
			return storeError("read Workspace current materialization state", "Workspace current materialization", err)
		}
		if desiredRevision != revision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current revision is no longer the materialization target")
		}
		if appliedRevision == revision {
			return nil
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=$2, updated_at=now()
WHERE workspace_id=$1 AND desired_revision=$2 AND applied_revision=-1`, workspaceID, revision)
		if err != nil {
			return fmt.Errorf("mark Workspace current revision applied: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current revision could not be marked applied")
		}
		return nil
	})
}

func (s *Store) TryWorkspaceCurrentMaterializationLock(ctx context.Context, workspaceID string) (app.LeaderLock, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, false, domain.InvalidArgument("workspace_id", "Workspace ID is invalid")
	}
	if s.pool.Config().MaxConns < 2 {
		return nil, false, fmt.Errorf("workspace current materialization lock requires at least two database connections")
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("morde:workspace-current-materialization:" + workspaceID))
	key := int64(hasher.Sum64())
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire Workspace current materialization lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&acquired); err != nil {
		_ = discardLeaderConnection(conn)
		return nil, false, fmt.Errorf("try Workspace current materialization advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &sessionLeaderLock{conn: conn, locks: []advisoryLock{{key: key}}}, true, nil
}

func enqueueWorkspaceCurrentMaterialization(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) error {
	tag, err := tx.Exec(ctx, `
UPDATE workspace_current_materializations
SET desired_revision=GREATEST(desired_revision,$2), updated_at=now()
WHERE workspace_id=$1`, workspaceID, revision)
	if err != nil {
		return fmt.Errorf("enqueue Workspace current materialization: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInternal, "Workspace current materialization state is missing")
	}
	return nil
}

var _ app.WorkspaceCurrentMaterializationBarrierStore = (*Store)(nil)
var _ app.WorkspaceCurrentMaterializationLockStore = (*Store)(nil)
