package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5"
)

func (s *Store) RequestTaskWorkspaceViewDeletion(ctx context.Context, record app.IdempotencyRecord, taskID string) (app.TaskWorkspaceViewDeletion, error) {
	if !domain.ValidTaskID(taskID) || record.Operation != "task.workspace_view.delete" || !domain.ValidTaskWorkspaceViewDeletionID(record.ResourceID) || record.RelatedID != taskID {
		return app.TaskWorkspaceViewDeletion{}, domain.InvalidArgument("task_workspace_view_deletion", "deletion request identity is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return app.TaskWorkspaceViewDeletion{}, err
	}
	var deletion app.TaskWorkspaceViewDeletion
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentHostID string
		if err := tx.QueryRow(ctx, `SELECT host_id FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, record.OwnerID).Scan(&currentHostID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("find Host before Task Workspace view deletion: %w", err)
		}
		var hostOwnerID, hostState string
		if err := tx.QueryRow(ctx, `SELECT owner_id,state FROM hosts WHERE id=$1 FOR UPDATE`, currentHostID).Scan(&hostOwnerID, &hostState); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "Host not found")
		} else if err != nil {
			return fmt.Errorf("lock Host for Task Workspace view deletion: %w", err)
		} else if hostOwnerID != record.OwnerID {
			return missingTaskError()
		} else if hostState != "running" && hostState != "stopped" && hostState != "failed" {
			return domain.NewError(domain.CodeConflict, "Host is changing state; retry Task Workspace view deletion later")
		}
		var ownerID, hostID, workspaceID, status, leaseOwner string
		var leaseActive bool
		err := tx.QueryRow(ctx, `SELECT owner_id, host_id, COALESCE(workspace_id,''), status, lease_owner,
COALESCE(lease_expires_at > clock_timestamp(), false)
FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&ownerID, &hostID, &workspaceID, &status, &leaseOwner, &leaseActive)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownerID != record.OwnerID) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for Workspace view deletion: %w", err)
		}
		if workspaceID == "" || !domain.ValidHostID(hostID) {
			return domain.NewError(domain.CodeInvalidState, "task has no private Workspace view")
		}
		stored, replayed, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replayed {
			if stored.RelatedID != taskID || !domain.ValidTaskWorkspaceViewDeletionID(stored.ResourceID) {
				return idempotencyConflict()
			}
			deletion, err = getTaskWorkspaceViewDeletion(ctx, tx, stored.ResourceID)
			return err
		}
		if existing, err := getTaskWorkspaceViewDeletionByTask(ctx, tx, taskID); err == nil {
			deletion = existing
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !terminalTaskStatus(status) || leaseOwner != "" || leaseActive {
			return domain.NewError(domain.CodeInvalidState, "task must be stopped before deleting its Workspace view")
		}
		var unsettled bool
		if err := tx.QueryRow(ctx, `SELECT
  EXISTS (SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
  EXISTS (SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
  EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND state='pending') OR
  EXISTS (SELECT 1 FROM task_stop_requests WHERE task_id=$1 AND status='pending')`, taskID).Scan(&unsettled); err != nil {
			return fmt.Errorf("check unsettled Task work before Workspace view deletion: %w", err)
		}
		if unsettled {
			return domain.NewError(domain.CodeConflict, "task has unsettled work; resolve it before deleting its Workspace view")
		}
		stored, replayed, err = reserveIdempotency(ctx, tx, record, record.ResourceID, taskID)
		if err != nil {
			return err
		}
		if replayed {
			if stored.RelatedID != taskID || !domain.ValidTaskWorkspaceViewDeletionID(stored.ResourceID) {
				return idempotencyConflict()
			}
			deletion, err = getTaskWorkspaceViewDeletion(ctx, tx, stored.ResourceID)
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_workspace_view_deletions (request_id, task_id, owner_id, host_id, status)
VALUES ($1,$2,$3,$4,'pending')`, stored.ResourceID, taskID, record.OwnerID, hostID); err != nil {
			return fmt.Errorf("persist Task Workspace view deletion request: %w", err)
		}
		deletion = app.TaskWorkspaceViewDeletion{ID: stored.ResourceID, TaskID: taskID, HostID: hostID, Status: "pending"}
		return nil
	})
	return deletion, err
}

func (s *Store) ListPendingTaskWorkspaceViewDeletions(ctx context.Context, hostID, ownerID string, generation int64) ([]app.TaskWorkspaceViewDeletion, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) || generation < 1 {
		return nil, domain.InvalidArgument("host", "Host generation is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.request_id,d.task_id,d.host_id,d.status
FROM task_workspace_view_deletions d
JOIN hosts h ON h.id=d.host_id AND h.owner_id=d.owner_id
WHERE d.host_id=$1 AND d.owner_id=$2 AND d.status='pending' AND h.runtime_generation=$3 AND h.state='running'
ORDER BY d.requested_at,d.request_id LIMIT 100`, hostID, ownerID, generation)
	if err != nil {
		return nil, fmt.Errorf("list pending Task Workspace view deletions: %w", err)
	}
	defer rows.Close()
	items := make([]app.TaskWorkspaceViewDeletion, 0)
	for rows.Next() {
		var item app.TaskWorkspaceViewDeletion
		if err := rows.Scan(&item.ID, &item.TaskID, &item.HostID, &item.Status); err != nil {
			return nil, fmt.Errorf("scan pending Task Workspace view deletion: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending Task Workspace view deletions: %w", err)
	}
	return items, nil
}

func (s *Store) GetTaskWorkspaceViewDeletion(ctx context.Context, ownerID, taskID string) (app.TaskWorkspaceViewDeletion, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) {
		return app.TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task Workspace view deletion not found")
	}
	var item app.TaskWorkspaceViewDeletion
	err := s.pool.QueryRow(ctx, `SELECT request_id,task_id,host_id,status FROM task_workspace_view_deletions
WHERE owner_id=$1 AND task_id=$2`, ownerID, taskID).Scan(&item.ID, &item.TaskID, &item.HostID, &item.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task Workspace view deletion not found")
	}
	if err != nil {
		return app.TaskWorkspaceViewDeletion{}, fmt.Errorf("get Task Workspace view deletion: %w", err)
	}
	return item, nil
}

func (s *Store) CompleteTaskWorkspaceViewDeletion(ctx context.Context, hostID, ownerID string, generation int64, taskID, requestID string) (app.TaskWorkspaceViewDeletion, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) || generation < 1 || !domain.ValidTaskID(taskID) || !domain.ValidTaskWorkspaceViewDeletionID(requestID) {
		return app.TaskWorkspaceViewDeletion{}, domain.NewError(domain.CodeNotFound, "task Workspace view deletion not found")
	}
	var deletion app.TaskWorkspaceViewDeletion
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentOwnerID, hostState string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT owner_id,runtime_generation,state FROM hosts WHERE id=$1 FOR UPDATE`, hostID).Scan(&currentOwnerID, &currentGeneration, &hostState); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "Host not found")
		} else if err != nil {
			return fmt.Errorf("lock Host before Task Workspace view deletion receipt: %w", err)
		} else if currentOwnerID != ownerID || currentGeneration != generation || hostState != "running" {
			return domain.NewError(domain.CodeConflict, "Host Runtime generation changed")
		}
		var storedOwnerID, storedHostID, status string
		var appliedGeneration *int64
		err := tx.QueryRow(ctx, `SELECT owner_id,host_id,status,applied_generation FROM task_workspace_view_deletions
WHERE request_id=$1 AND task_id=$2 FOR UPDATE`, requestID, taskID).Scan(&storedOwnerID, &storedHostID, &status, &appliedGeneration)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (storedHostID != hostID || storedOwnerID != ownerID)) {
			return domain.NewError(domain.CodeNotFound, "task Workspace view deletion not found")
		}
		if err != nil {
			return fmt.Errorf("lock Task Workspace view deletion receipt: %w", err)
		}
		if status == "applied" {
			if appliedGeneration == nil || *appliedGeneration != generation {
				return domain.NewError(domain.CodeConflict, "Task Workspace view deletion was acknowledged by another Host generation")
			}
			deletion = app.TaskWorkspaceViewDeletion{ID: requestID, TaskID: taskID, HostID: hostID, Status: status}
			return nil
		}
		command, err := tx.Exec(ctx, `UPDATE task_workspace_view_deletions SET status='applied',applied_at=clock_timestamp(),applied_generation=$2
WHERE request_id=$1 AND task_id=$3 AND host_id=$4 AND status='pending'`, requestID, generation, taskID, hostID)
		if err != nil {
			return fmt.Errorf("record Task Workspace view deletion receipt: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task Workspace view deletion request changed")
		}
		deletion = app.TaskWorkspaceViewDeletion{ID: requestID, TaskID: taskID, HostID: hostID, Status: "applied"}
		return nil
	})
	return deletion, err
}

func getTaskWorkspaceViewDeletion(ctx context.Context, tx pgx.Tx, requestID string) (app.TaskWorkspaceViewDeletion, error) {
	var item app.TaskWorkspaceViewDeletion
	err := tx.QueryRow(ctx, `SELECT request_id,task_id,host_id,status FROM task_workspace_view_deletions WHERE request_id=$1`, requestID).
		Scan(&item.ID, &item.TaskID, &item.HostID, &item.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskWorkspaceViewDeletion{}, idempotencyConflict()
	}
	if err != nil {
		return app.TaskWorkspaceViewDeletion{}, fmt.Errorf("read Task Workspace view deletion request: %w", err)
	}
	return item, nil
}

func getTaskWorkspaceViewDeletionByTask(ctx context.Context, tx pgx.Tx, taskID string) (app.TaskWorkspaceViewDeletion, error) {
	var item app.TaskWorkspaceViewDeletion
	err := tx.QueryRow(ctx, `SELECT request_id,task_id,host_id,status FROM task_workspace_view_deletions WHERE task_id=$1`, taskID).
		Scan(&item.ID, &item.TaskID, &item.HostID, &item.Status)
	if err != nil {
		return app.TaskWorkspaceViewDeletion{}, err
	}
	return item, nil
}

func terminalTaskStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}
