package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

const (
	stopLeaseMinMillis = int64(1)
	stopLeaseMaxMillis = int64((24 * time.Hour) / time.Millisecond)
)

// RequestTaskAction atomically fences the current task worker and persists the
// user's pause, resume, or cancel request. Pause and cancel remain transitional
// until a stop worker has settled every runtime operation and safe_after passes.
func (s *Store) RequestTaskAction(ctx context.Context, record app.IdempotencyRecord, taskID string, action task.Action, expectedStatus task.Status, requestID string) (app.TaskActionResult, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(requestID) || !validRequestedAction(action, expectedStatus) {
		return app.TaskActionResult{}, domain.InvalidArgument("action", "task action fields are invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return app.TaskActionResult{}, err
	}
	if record.Operation != "task.action."+string(action) || record.ResourceID != requestID || record.RelatedID != taskID {
		return app.TaskActionResult{}, domain.InvalidArgument("idempotency", "task action idempotency identity is invalid")
	}
	if record.OwnerID == "" {
		return app.TaskActionResult{}, domain.InvalidArgument("idempotency", "task action owner is required")
	}

	var result app.TaskActionResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var executionGate executionGate
		if action == task.ActionResume {
			_, replayed, err := findIdempotency(ctx, tx, record)
			if err != nil {
				return err
			}
			if !replayed {
				executionGate, err = requireOwnerExecutionEnabled(ctx, tx, record.OwnerID, nil)
				if err != nil {
					return err
				}
			}
		}
		var status, workspaceID, waitingReason string
		var leaseVersion, inputRevision int64
		var taskEpoch int64
		err := tx.QueryRow(ctx, `SELECT status, lease_version, execution_epoch, COALESCE(workspace_id,''),
COALESCE(input_revision,-1), waiting_reason
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, record.OwnerID).
			Scan(&status, &leaseVersion, &taskEpoch, &workspaceID, &inputRevision, &waitingReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock task for action: %w", err)
		}
		if action == task.ActionResume && executionGate.epoch > 0 && taskEpoch != executionGate.epoch {
			return ownerExecutionDisabledError(executionGate)
		}

		stored, replayed, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		requestIDToUse := requestID
		if replayed {
			if stored.RelatedID != taskID || !domain.ValidTaskActionID(stored.ResourceID) {
				return idempotencyConflict()
			}
			requestIDToUse = stored.ResourceID
		} else {
			if action == task.ActionResume && workspaceID != "" {
				var currentRevision int64
				var workspaceState string
				err := tx.QueryRow(ctx, `SELECT revision, state FROM workspaces WHERE id=$1 AND owner_id=$2`, workspaceID, record.OwnerID).
					Scan(&currentRevision, &workspaceState)
				if errors.Is(err, pgx.ErrNoRows) || (err == nil && (workspaceState != "ready" || currentRevision != inputRevision || waitingReason == "workspace_input_changed")) {
					return domain.NewError(domain.CodeWorkspaceConflict, "Workspace input changed; submit a new task with the current files")
				}
				if err != nil {
					return fmt.Errorf("check Workspace input before task resume: %w", err)
				}
			}
			if status != string(expectedStatus) {
				conflict := domain.NewError(domain.CodeConflict, "task status does not match expected_status")
				conflict.Details["status"] = status
				return conflict
			}
			if !actionAllowedFrom(action, task.Status(status)) {
				conflict := domain.NewError(domain.CodeConflict, "task action is not allowed in its current status")
				conflict.Details["status"] = status
				return conflict
			}
			stored, replayed, err = reserveIdempotency(ctx, tx, record, requestID, taskID)
			if err != nil {
				return err
			}
			if replayed {
				if stored.RelatedID != taskID || !domain.ValidTaskActionID(stored.ResourceID) {
					return idempotencyConflict()
				}
				requestIDToUse = stored.ResourceID
			}
		}

		if !replayed {
			if leaseVersion == math.MaxInt64 {
				return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
			}
			switch action {
			case task.ActionPause, task.ActionCancel:
				if action == task.ActionCancel && status == string(task.StatusPaused) {
					// Keep cancellation recoverable if a prior incomplete pause row is
					// present; normal finalized pauses already mark it completed.
					if _, err := tx.Exec(ctx, `UPDATE task_stop_requests SET status='completed', lease_owner='', lease_expires_at=NULL,
completed_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE task_id=$1 AND action='pause' AND status='pending'`, taskID); err != nil {
						return fmt.Errorf("complete paused stop request before cancellation: %w", err)
					}
				}
				var safeAfter time.Time
				err := tx.QueryRow(ctx, `SELECT GREATEST(
COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz),
COALESCE(lease_expires_at, '-infinity'::timestamptz), clock_timestamp())
FROM tasks WHERE id=$1`, taskID).Scan(&safeAfter)
				if err != nil {
					return fmt.Errorf("compute task stop safe_after: %w", err)
				}
				targetStatus := task.StatusPausing
				if action == task.ActionCancel {
					targetStatus = task.StatusCancelling
					if err := cancelPendingTaskRequest(ctx, tx, taskID); err != nil {
						return err
					}
				}
				var sequence int64
				err = tx.QueryRow(ctx, `UPDATE tasks SET status=$3, lease_version=lease_version+1, lease_owner='', lease_expires_at=NULL,
waiting_reason='', finished_at=NULL, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status=$4 RETURNING event_seq`,
					taskID, record.OwnerID, string(targetStatus), status).Scan(&sequence)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeConflict, "task state changed while applying the action")
				}
				if err != nil {
					return fmt.Errorf("begin task stop transition: %w", err)
				}
				// Fence any in-flight MCP result before the stop request becomes
				// visible. The Task row is already locked, matching MCP result
				// settlement's Task-before-operation lock order.
				if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET cancel_requested=true, updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, taskID); err != nil {
					return fmt.Errorf("request cancellation of in-flight MCP operations during task stop: %w", err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,$3,'pending',$4)`, requestIDToUse, taskID, string(action), safeAfter); err != nil {
					return fmt.Errorf("persist task stop request: %w", err)
				}
				if err := insertTaskStatusEvent(ctx, tx, taskID, sequence, targetStatus, action, requestIDToUse); err != nil {
					return err
				}
			case task.ActionResume:
				var sequence int64
				err := tx.QueryRow(ctx, `UPDATE tasks SET status='queued', lease_version=lease_version+1, lease_owner='', lease_expires_at=NULL,
waiting_reason='', finished_at=NULL, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='paused' RETURNING event_seq`, taskID, record.OwnerID).Scan(&sequence)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeConflict, "task state changed while resuming")
				}
				if err != nil {
					return fmt.Errorf("resume paused task: %w", err)
				}
				if err := insertTaskStatusEvent(ctx, tx, taskID, sequence, task.StatusQueued, action, requestIDToUse); err != nil {
					return err
				}
			}
		}

		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: record.OwnerID})
		if err != nil {
			return fmt.Errorf("read task after action: %w", err)
		}
		var unsettled []string
		if taskRow.Status == string(task.StatusPausing) || taskRow.Status == string(task.StatusCancelling) {
			stop, operationIDs, err := loadTaskStopState(ctx, tx, taskID)
			if err != nil {
				return err
			}
			if stop != nil && stop.ID == requestIDToUse && stop.Status == task.StopRequestPending {
				unsettled = operationIDs
			}
		}
		result = app.TaskActionResult{Task: rowTaskDomain(taskRow), Action: action, UnsettledOperationIDs: unsettled}
		return nil
	})
	return result, err
}

func (s *Store) ClaimNextTaskStop(ctx context.Context, worker string, leaseDuration time.Duration) (app.TaskStopClaim, bool, error) {
	if worker == "" || len(worker) > 255 || leaseDuration <= 0 {
		return app.TaskStopClaim{}, false, domain.InvalidArgument("lease", "worker and positive lease duration are required")
	}
	leaseMillis := leaseDuration.Milliseconds()
	if leaseMillis < stopLeaseMinMillis {
		leaseMillis = stopLeaseMinMillis
	}
	if leaseMillis > stopLeaseMaxMillis {
		return app.TaskStopClaim{}, false, domain.InvalidArgument("lease", "stop lease duration is too large")
	}
	var claim app.TaskStopClaim
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskID, ownerID, requestID string
		var oldVersion int64
		err := tx.QueryRow(ctx, `SELECT t.id, t.owner_id, sr.id, sr.lease_version
FROM tasks AS t JOIN task_stop_requests AS sr ON sr.task_id=t.id
WHERE t.status IN ('pausing','cancelling') AND sr.status='pending'
  AND sr.next_attempt_at <= clock_timestamp()
  AND (sr.lease_expires_at IS NULL OR sr.lease_expires_at <= clock_timestamp())
ORDER BY sr.next_attempt_at, sr.created_at, sr.id
LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).Scan(&taskID, &ownerID, &requestID, &oldVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim next task stop request: %w", err)
		}
		if oldVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "task stop lease version is exhausted")
		}
		var row task.StopRequest
		var leaseExpiresAt *time.Time
		err = tx.QueryRow(ctx, `UPDATE task_stop_requests SET lease_owner=$2, lease_version=lease_version+1,
lease_expires_at=clock_timestamp()+($3::bigint * interval '1 millisecond'), attempts=attempts+1, updated_at=clock_timestamp()
WHERE id=$1 AND status='pending' AND (lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp())
RETURNING id, task_id, action, status, safe_after, lease_owner, lease_version, lease_expires_at, created_at, updated_at`,
			requestID, worker, leaseMillis).Scan(&row.ID, &row.TaskID, &row.Action, &row.Status, &row.SafeAfter,
			&row.LeaseOwner, &row.LeaseVersion, &leaseExpiresAt, &row.CreatedAt, &row.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("persist task stop worker lease: %w", err)
		}
		row.LeaseExpiresAt = leaseExpiresAt
		// A prepared connector call has not crossed the external side-effect
		// boundary. Cancel it while holding the Task lock so a stale worker cannot
		// dispatch it after this stop request has fenced the Task lease.
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET status='cancelled', updated_at=clock_timestamp()
WHERE task_id=$1 AND status='prepared'`, taskID); err != nil {
			return fmt.Errorf("cancel prepared MCP connector operations during task stop: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET cancel_requested=true, updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, taskID); err != nil {
			return fmt.Errorf("request cancellation of dispatched MCP connector operations during stop recovery: %w", err)
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read task for stop worker: %w", err)
		}
		operations, err := listStopOperations(ctx, tx, taskID)
		if err != nil {
			return err
		}
		claim = app.TaskStopClaim{Task: rowTaskDomain(taskRow), Request: row, Operations: operations}
		found = true
		return nil
	})
	return claim, found, err
}

func (s *Store) RenewTaskStopClaim(ctx context.Context, taskID, stopID, worker string, version int64, leaseDuration time.Duration) error {
	leaseMillis, err := validStopClaimLease(taskID, stopID, worker, version, leaseDuration)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockStopClaimTask(ctx, tx, taskID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE task_stop_requests SET lease_expires_at=clock_timestamp()+($5::bigint * interval '1 millisecond'),
updated_at=clock_timestamp()
WHERE task_id=$1 AND id=$2 AND status='pending' AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at > clock_timestamp()
AND EXISTS (SELECT 1 FROM tasks WHERE id=$1 AND status IN ('pausing','cancelling'))`, taskID, stopID, worker, version, leaseMillis)
		if err != nil {
			return fmt.Errorf("renew task stop worker lease: %w", err)
		}
		if command.RowsAffected() != 1 {
			return stopClaimConflict()
		}
		return nil
	})
}

func (s *Store) SettleTaskStopOperation(ctx context.Context, taskID, stopID, worker string, version int64, operationID string, status task.OperationStatus, result string, requireSafeAfter bool) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(stopID) || worker == "" || len(worker) > 255 || version < 1 || !domain.ValidOperationID(operationID) ||
		(status != task.OperationCancelled && status != task.OperationSucceeded && status != task.OperationFailed) || len(result) > 8192 {
		return domain.InvalidArgument("operation", "task stop operation settlement fields are invalid")
	}
	if requireSafeAfter && status != task.OperationCancelled {
		return domain.InvalidArgument("operation", "safe-after confirmation only applies to cancellation")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockStopClaimTask(ctx, tx, taskID); err != nil {
			return err
		}
		if err := requireStopClaim(ctx, tx, taskID, stopID, worker, version); err != nil {
			return err
		}
		var operation task.Operation
		operation, err := loadOperationByID(ctx, tx, taskID, operationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task operation not found")
		}
		if err != nil {
			return fmt.Errorf("read operation for stop settlement: %w", err)
		}
		if requireSafeAfter {
			var safeAfterReached bool
			err := tx.QueryRow(ctx, `SELECT safe_after <= clock_timestamp() FROM task_stop_requests
WHERE task_id=$1 AND id=$2 AND status='pending' AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at > clock_timestamp()
FOR SHARE`, taskID, stopID, worker, version).Scan(&safeAfterReached)
			if errors.Is(err, pgx.ErrNoRows) {
				return stopClaimConflict()
			}
			if err != nil {
				return fmt.Errorf("check database safe_after before operation cancellation: %w", err)
			}
			if !safeAfterReached {
				return domain.NewError(domain.CodeConflict, "runtime operation not-found cannot be accepted before safe_after")
			}
		}
		if operation.Status == status {
			if operation.Result == result {
				return nil
			}
			return domain.NewError(domain.CodeConflict, "operation settlement result changed")
		}
		if operation.Status != task.OperationPrepared && operation.Status != task.OperationDispatched && operation.Status != task.OperationUnknown {
			return domain.NewError(domain.CodeConflict, "task operation is already settled")
		}
		if operation.Status == task.OperationPrepared && status != task.OperationCancelled {
			return domain.NewError(domain.CodeConflict, "a prepared operation can only be cancelled")
		}
		if !task.CanTransitionOperation(operation.Status, status) {
			return domain.NewError(domain.CodeConflict, "task operation cannot be settled to the requested status")
		}
		command, err := tx.Exec(ctx, `UPDATE task_operations SET status=$3, result=$4, updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND status=$5`, operation.ID, taskID, string(status), result, string(operation.Status))
		if err != nil {
			return fmt.Errorf("persist stopped operation settlement: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "task operation changed while settling")
		}
		payload, err := json.Marshal(map[string]any{"operation_id": operation.ID, "operation_key": operation.Key, "status": string(status), "stop_request_id": stopID})
		if err != nil {
			return fmt.Errorf("encode stopped operation event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, taskID)
		if err != nil {
			return err
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.operation", Payload: payload})
	})
}

func (s *Store) CompleteTaskStop(ctx context.Context, taskID, stopID, worker string, version int64) (task.Task, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(stopID) || worker == "" || len(worker) > 255 || version < 1 {
		return task.Task{}, domain.InvalidArgument("stop_request", "task stop completion fields are invalid")
	}
	var result task.Task
	var deferredConflict error
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockStopClaimTask(ctx, tx, taskID); err != nil {
			return err
		}
		var action task.Action
		var safeAfter time.Time
		if err := tx.QueryRow(ctx, `SELECT action, safe_after FROM task_stop_requests
WHERE task_id=$1 AND id=$2 AND status='pending' AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at > clock_timestamp()
FOR UPDATE`, taskID, stopID, worker, version).Scan(&action, &safeAfter); errors.Is(err, pgx.ErrNoRows) {
			return stopClaimConflict()
		} else if err != nil {
			return fmt.Errorf("lock task stop request for completion: %w", err)
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("read database time for task stop: %w", err)
		}
		if now.Before(safeAfter) {
			conflict := domain.NewError(domain.CodeConflict, "task stop is not yet safe to finalize")
			conflict.Details["safe_after"] = safeAfter
			return conflict
		}
		// At safe_after, a dispatched connector call can no longer be proven to
		// have a live call manager. Preserve its uncertain outcome and reject any
		// late result instead of claiming safe cancellation or replaying the call.
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET status='unknown',
    result=CASE WHEN result='' THEN 'MCP call outcome could not be confirmed during task stop recovery' ELSE result END,
    updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched'`, taskID); err != nil {
			return fmt.Errorf("mark dispatched MCP connector operations unknown during task stop: %w", err)
		}
		var activeCount int64
		if err := countUnsettledTaskOperations(ctx, tx, taskID, &activeCount); err != nil {
			return fmt.Errorf("count unsettled task operations: %w", err)
		}
		if activeCount != 0 {
			conflict := domain.NewError(domain.CodeConflict, "task stop is not yet safe to finalize")
			conflict.Details["unsettled_operations"] = activeCount
			conflict.Details["safe_after"] = safeAfter
			deferredConflict = conflict
			return nil
		}
		var agentStopUnconfirmed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
	SELECT 1 FROM task_agent_instances WHERE task_id=$1 AND state <> 'stopped'
) OR EXISTS (
	SELECT 1 FROM tasks t
	LEFT JOIN task_agent_instances i ON i.task_id=t.id AND i.agent_generation=t.agent_generation AND i.agent_instance_id=t.agent_instance_id
	WHERE t.id=$1 AND t.agent_instance_id <> '' AND (i.task_id IS NULL OR i.state <> 'stopped')
)`, taskID).Scan(&agentStopUnconfirmed); err != nil {
			return fmt.Errorf("check active task agent before stop finalization: %w", err)
		}
		if agentStopUnconfirmed {
			deferredConflict = domain.NewError(domain.CodeConflict, "task agent stop is unconfirmed")
			return nil
		}
		var status, ownerID, hostID, workspaceID, waitingReason string
		var inputRevision int64
		if err := tx.QueryRow(ctx, `SELECT status, owner_id, COALESCE(host_id,''), COALESCE(workspace_id,''), COALESCE(input_revision,-1), waiting_reason
FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&status, &ownerID, &hostID, &workspaceID, &inputRevision, &waitingReason); err != nil {
			return fmt.Errorf("read task status for stop completion: %w", err)
		}
		workspaceInputChanged := false
		if action == task.ActionPause && workspaceID != "" && inputRevision >= 0 {
			changed, err := taskWorkspaceInputChanged(ctx, tx, taskID, ownerID, hostID, workspaceID, inputRevision,
				waitingReason != "workspace_input_changed")
			if err != nil {
				return err
			}
			workspaceInputChanged = changed || waitingReason == "workspace_input_changed"
		}
		target := task.StatusPaused
		if action == task.ActionCancel {
			target = task.StatusCancelled
		}
		expected := task.StatusPausing
		if action == task.ActionCancel {
			expected = task.StatusCancelling
		}
		if status != string(expected) {
			return domain.NewError(domain.CodeConflict, "task status changed before stop finalization")
		}
		var sequence int64
		err := tx.QueryRow(ctx, `UPDATE tasks SET status=$2, lease_owner='', lease_expires_at=NULL,
waiting_reason=CASE WHEN $4 THEN 'workspace_input_changed' ELSE waiting_reason END,
finished_at=CASE WHEN $2='cancelled' THEN clock_timestamp() ELSE NULL END,
event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND status=$3 RETURNING event_seq`, taskID, string(target), string(expected), workspaceInputChanged).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task status changed before stop finalization")
		}
		if err != nil {
			return fmt.Errorf("finalize task stop state: %w", err)
		}
		command, err := tx.Exec(ctx, `UPDATE task_stop_requests SET status='completed', lease_owner='', lease_expires_at=NULL,
completed_at=clock_timestamp(),
next_attempt_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND status='pending'`, stopID, taskID)
		if err != nil {
			return fmt.Errorf("persist completed task stop request: %w", err)
		}
		if command.RowsAffected() != 1 {
			return stopClaimConflict()
		}
		if err := insertTaskStatusEvent(ctx, tx, taskID, sequence, target, action, stopID); err != nil {
			return err
		}
		if action == task.ActionPause && workspaceInputChanged {
			if err := insertWorkspaceInputChangedEvent(ctx, tx, taskID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM tasks WHERE id=$1`, taskID).Scan(&ownerID); err != nil {
			return fmt.Errorf("read task owner after stop: %w", err)
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("read finalized task: %w", err)
		}
		result = rowTaskDomain(row)
		return nil
	})
	if err != nil {
		return task.Task{}, err
	}
	if deferredConflict != nil {
		return task.Task{}, deferredConflict
	}
	return result, nil
}

// ReconcileStaleWorkspaceTasks fences tasks that were submitted against an
// older Workspace revision and routes them through the existing safe-stop flow.
func (s *Store) ReconcileStaleWorkspaceTasks(ctx context.Context) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id, t.owner_id, t.host_id, t.workspace_id, t.input_revision,
t.status, t.waiting_reason
FROM tasks t JOIN workspaces w ON w.id=t.workspace_id
WHERE t.input_revision IS NOT NULL AND (w.state <> 'ready' OR w.revision <> t.input_revision)
  AND t.status IN ('queued','running','waiting_user','paused')
  AND NOT (t.status='paused' AND t.waiting_reason='workspace_input_changed')
ORDER BY t.updated_at, t.id LIMIT 100 FOR UPDATE OF t SKIP LOCKED`)
		if err != nil {
			return fmt.Errorf("list tasks with stale Workspace inputs: %w", err)
		}
		type staleTask struct {
			id, ownerID, hostID, workspaceID, status, waitingReason string
			inputRevision                                           int64
		}
		stale := make([]staleTask, 0)
		for rows.Next() {
			var item staleTask
			if err := rows.Scan(&item.id, &item.ownerID, &item.hostID, &item.workspaceID, &item.inputRevision,
				&item.status, &item.waitingReason); err != nil {
				rows.Close()
				return fmt.Errorf("read task with stale Workspace input: %w", err)
			}
			stale = append(stale, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate tasks with stale Workspace inputs: %w", err)
		}
		rows.Close()

		for _, item := range stale {
			allowOwnWrites := item.status == string(task.StatusRunning) && item.waitingReason != "workspace_input_changed"
			changed, err := taskWorkspaceInputChanged(ctx, tx, item.id, item.ownerID, item.hostID,
				item.workspaceID, item.inputRevision, allowOwnWrites)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			if item.status == string(task.StatusPaused) {
				var sequence int64
				err := tx.QueryRow(ctx, `UPDATE tasks SET waiting_reason='workspace_input_changed', event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND status='paused' RETURNING event_seq`, item.id).Scan(&sequence)
				if err != nil {
					return fmt.Errorf("mark paused task as requiring a new Workspace input: %w", err)
				}
				if err := insertTaskStatusEvent(ctx, tx, item.id, sequence, task.StatusPaused, task.ActionPause, ""); err != nil {
					return err
				}
				if err := insertWorkspaceInputChangedEvent(ctx, tx, item.id); err != nil {
					return err
				}
				continue
			}
			if item.status == string(task.StatusWaitingUser) {
				if err := cancelPendingTaskRequest(ctx, tx, item.id); err != nil {
					return err
				}
			}
			var safeAfter time.Time
			if err := tx.QueryRow(ctx, `SELECT GREATEST(
COALESCE(runtime_lease_safe_after, '-infinity'::timestamptz),
COALESCE(lease_expires_at, '-infinity'::timestamptz), clock_timestamp())
FROM tasks WHERE id=$1`, item.id).Scan(&safeAfter); err != nil {
				return fmt.Errorf("compute stale Workspace task stop safe_after: %w", err)
			}
			var sequence int64
			err = tx.QueryRow(ctx, `UPDATE tasks SET status='pausing', lease_version=lease_version+1, lease_owner='', lease_expires_at=NULL,
waiting_reason='workspace_input_changed', finished_at=NULL, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND status=$2 RETURNING event_seq`, item.id, item.status).Scan(&sequence)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("fence task with stale Workspace input: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET cancel_requested=true, updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, item.id); err != nil {
				return fmt.Errorf("cancel task MCP operations after Workspace input changed: %w", err)
			}
			stopID, err := domain.NewTaskActionID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,'pause','pending',$3)`, stopID, item.id, safeAfter); err != nil {
				return fmt.Errorf("persist stale Workspace task stop: %w", err)
			}
			if err := insertTaskStatusEvent(ctx, tx, item.id, sequence, task.StatusPausing, task.ActionPause, stopID); err != nil {
				return err
			}
		}
		return nil
	})
}

func taskWorkspaceInputChanged(ctx context.Context, tx pgx.Tx, taskID, ownerID, hostID, workspaceID string, inputRevision int64, allowOwnWrites bool) (bool, error) {
	var currentRevision int64
	var state string
	err := tx.QueryRow(ctx, `SELECT revision, state FROM workspaces WHERE id=$1 AND owner_id=$2`, workspaceID, ownerID).
		Scan(&currentRevision, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read current Workspace revision for task: %w", err)
	}
	if state == "ready" && currentRevision == inputRevision {
		return false, nil
	}
	if allowOwnWrites && state == "ready" && currentRevision > inputRevision {
		matches, err := workspaceChangesMatchTaskWrites(ctx, tx, taskID, hostID, workspaceID, inputRevision, currentRevision)
		if err != nil {
			return false, err
		}
		if matches {
			return false, nil
		}
	}
	return true, nil
}

func workspaceChangesMatchTaskWrites(ctx context.Context, tx pgx.Tx, taskID, hostID, workspaceID string, inputRevision, currentRevision int64) (bool, error) {
	type writeEvidence struct {
		Path         string `json:"path"`
		BytesWritten int64  `json:"bytes_written"`
		SHA256       string `json:"sha256"`
	}
	type fileIdentity struct {
		path, sha256 string
		size         int64
	}
	evidence := make(map[fileIdentity]int)
	// The operation result is durable before its Task progress event, so it is
	// the recovery proof when Workspace sync commits between those transactions.
	rows, err := tx.Query(ctx, `SELECT o.result FROM task_operations o
WHERE o.task_id=$1 AND o.host_id=$2 AND o.workspace_id=$3 AND o.status='succeeded'`, taskID, hostID, workspaceID)
	if err != nil {
		return false, fmt.Errorf("read successful task file writes: %w", err)
	}
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			rows.Close()
			return false, fmt.Errorf("read task file write result: %w", err)
		}
		var write writeEvidence
		if json.Unmarshal([]byte(result), &write) == nil && write.Path != "" && write.BytesWritten >= 0 && write.SHA256 != "" {
			evidence[fileIdentity{path: write.Path, size: write.BytesWritten, sha256: write.SHA256}]++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("iterate successful task file writes: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT file_path, action, file FROM workspace_changes
WHERE workspace_id=$1 AND revision>$2 AND revision<=$3 ORDER BY revision, file_path`, workspaceID, inputRevision, currentRevision)
	if err != nil {
		return false, fmt.Errorf("read Workspace changes for stale task: %w", err)
	}
	changeCount := 0
	for rows.Next() {
		var path, action string
		var fileJSON []byte
		if err := rows.Scan(&path, &action, &fileJSON); err != nil {
			rows.Close()
			return false, fmt.Errorf("read Workspace change for stale task: %w", err)
		}
		changeCount++
		if action != "upsert" {
			rows.Close()
			return false, nil
		}
		var file struct {
			Path      string `json:"path"`
			SizeBytes int64  `json:"size_bytes"`
			SHA256    string `json:"sha256"`
		}
		if json.Unmarshal(fileJSON, &file) != nil || file.Path != path {
			rows.Close()
			return false, nil
		}
		key := fileIdentity{path: file.Path, size: file.SizeBytes, sha256: file.SHA256}
		if evidence[key] == 0 {
			rows.Close()
			return false, nil
		}
		evidence[key]--
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("iterate Workspace changes for stale task: %w", err)
	}
	rows.Close()
	return changeCount > 0, nil
}

func insertWorkspaceInputChangedEvent(ctx context.Context, tx pgx.Tx, taskID string) error {
	sequence, err := incrementTaskEventSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"reason": "workspace_input_changed"})
	if err != nil {
		return fmt.Errorf("encode Workspace input change event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.waiting_user", Payload: payload})
}

func (s *Store) ReleaseTaskStopClaim(ctx context.Context, taskID, stopID, worker string, version int64, retryAfter time.Duration) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(stopID) || worker == "" || len(worker) > 255 || version < 1 || retryAfter < 0 || retryAfter > 24*time.Hour {
		return domain.InvalidArgument("retry_after", "task stop retry delay is out of range")
	}
	retryMillis := retryAfter.Milliseconds()
	if retryMillis < 1 {
		retryMillis = 1
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockStopClaimTask(ctx, tx, taskID); err != nil {
			return err
		}
		var safeAfter time.Time
		if err := tx.QueryRow(ctx, `SELECT safe_after FROM task_stop_requests WHERE task_id=$1 AND id=$2 AND status='pending'
AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at > clock_timestamp() FOR UPDATE`, taskID, stopID, worker, version).Scan(&safeAfter); errors.Is(err, pgx.ErrNoRows) {
			return stopClaimConflict()
		} else if err != nil {
			return fmt.Errorf("lock task stop request for release: %w", err)
		}
		var activeCount int64
		if err := countUnsettledTaskOperations(ctx, tx, taskID, &activeCount); err != nil {
			return fmt.Errorf("count unsettled task operations before release: %w", err)
		}
		command, err := tx.Exec(ctx, `UPDATE task_stop_requests SET lease_owner='', lease_expires_at=NULL,
next_attempt_at=GREATEST(clock_timestamp()+($5::bigint * interval '1 millisecond'), CASE WHEN $6=0 THEN $7 ELSE clock_timestamp() END),
updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND status='pending' AND lease_owner=$3 AND lease_version=$4`,
			stopID, taskID, worker, version, retryMillis, activeCount, safeAfter)
		if err != nil {
			return fmt.Errorf("release task stop worker lease: %w", err)
		}
		if command.RowsAffected() != 1 {
			return stopClaimConflict()
		}
		return nil
	})
}

func loadTaskStopState(ctx context.Context, tx pgx.Tx, taskID string) (*task.StopRequest, []string, error) {
	var request task.StopRequest
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `SELECT id, task_id, action, status, safe_after, lease_owner, lease_version,
lease_expires_at, created_at, updated_at FROM task_stop_requests WHERE task_id=$1
ORDER BY created_at DESC, id DESC LIMIT 1`, taskID).Scan(
		&request.ID, &request.TaskID, &request.Action, &request.Status, &request.SafeAfter,
		&request.LeaseOwner, &request.LeaseVersion, &expiresAt, &request.CreatedAt, &request.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read latest task stop request: %w", err)
	}
	request.LeaseExpiresAt = expiresAt
	if request.Status == task.StopRequestCompleted {
		return &request, []string{}, nil
	}
	ids, err := listUnsettledOperationIDs(ctx, tx, taskID)
	if err != nil {
		return nil, nil, err
	}
	return &request, ids, nil
}

func listUnsettledOperationIDs(ctx context.Context, tx pgx.Tx, taskID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM (
  SELECT id, operation_key FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')
  UNION ALL
  SELECT id, operation_key FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched')
) unsettled ORDER BY operation_key, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list unsettled task operations: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read unsettled task operation id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unsettled task operations: %w", err)
	}
	return ids, nil
}

func countUnsettledTaskOperations(ctx context.Context, tx pgx.Tx, taskID string, count *int64) error {
	return tx.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')) +
  (SELECT count(*) FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched'))`, taskID).Scan(count)
}

func listStopOperations(ctx context.Context, tx pgx.Tx, taskID string) ([]task.Operation, error) {
	rows, err := tx.Query(ctx, `SELECT id, task_id, operation_key, request_hash, host_id, workspace_id, binding_version, input_revision, runtime_generation,
runtime_instance_id, control_version, lease_version, runtime_lease_version, status, external_id, result, created_at, updated_at
FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown') ORDER BY operation_key`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task operations for stop: %w", err)
	}
	defer rows.Close()
	operations := make([]task.Operation, 0)
	for rows.Next() {
		var row sqlcgen.TaskOperation
		if err := rows.Scan(&row.ID, &row.TaskID, &row.OperationKey, &row.RequestHash, &row.HostID,
			&row.WorkspaceID, &row.BindingVersion, &row.InputRevision, &row.RuntimeGeneration, &row.RuntimeInstanceID, &row.ControlVersion, &row.LeaseVersion,
			&row.RuntimeLeaseVersion, &row.Status, &row.ExternalID, &row.Result, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read task operation for stop: %w", err)
		}
		operations = append(operations, taskOperationDomain(row))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task operations for stop: %w", err)
	}
	return operations, nil
}

func loadOperationByID(ctx context.Context, tx pgx.Tx, taskID, operationID string) (task.Operation, error) {
	var row sqlcgen.TaskOperation
	err := tx.QueryRow(ctx, `SELECT id, task_id, operation_key, request_hash, host_id, workspace_id, binding_version, input_revision, runtime_generation, runtime_instance_id,
control_version, lease_version, runtime_lease_version, status, external_id, result, created_at, updated_at
FROM task_operations WHERE task_id=$1 AND id=$2`, taskID, operationID).Scan(
		&row.ID, &row.TaskID, &row.OperationKey, &row.RequestHash, &row.HostID, &row.WorkspaceID, &row.BindingVersion,
		&row.InputRevision, &row.RuntimeGeneration, &row.RuntimeInstanceID, &row.ControlVersion, &row.LeaseVersion, &row.RuntimeLeaseVersion,
		&row.Status, &row.ExternalID, &row.Result, &row.CreatedAt, &row.UpdatedAt)
	return taskOperationDomain(row), err
}

func validRequestedAction(action task.Action, expected task.Status) bool {
	if !validTaskStatus(expected) {
		return false
	}
	switch action {
	case task.ActionPause:
		return expected == task.StatusQueued || expected == task.StatusRunning
	case task.ActionResume:
		return expected == task.StatusPaused
	case task.ActionCancel:
		return expected == task.StatusQueued || expected == task.StatusRunning || expected == task.StatusWaitingUser || expected == task.StatusPaused
	default:
		return false
	}
}

func actionAllowedFrom(action task.Action, status task.Status) bool {
	switch action {
	case task.ActionPause:
		return status == task.StatusQueued || status == task.StatusRunning
	case task.ActionResume:
		return status == task.StatusPaused
	case task.ActionCancel:
		return status == task.StatusQueued || status == task.StatusRunning || status == task.StatusWaitingUser || status == task.StatusPaused
	default:
		return false
	}
}

func insertTaskStatusEvent(ctx context.Context, tx pgx.Tx, taskID string, sequence int64, status task.Status, action task.Action, requestID string) error {
	payload, err := json.Marshal(map[string]string{"status": string(status), "action": string(action), "request_id": requestID})
	if err != nil {
		return fmt.Errorf("encode task action event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.status", Payload: payload})
}

func validStopClaimLease(taskID, stopID, worker string, version int64, duration time.Duration) (int64, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidTaskActionID(stopID) || worker == "" || len(worker) > 255 || version < 1 || duration <= 0 {
		return 0, domain.InvalidArgument("lease", "task, stop request, worker, version, and positive lease duration are required")
	}
	millis := duration.Milliseconds()
	if millis < stopLeaseMinMillis {
		millis = stopLeaseMinMillis
	}
	if millis > stopLeaseMaxMillis {
		return 0, domain.InvalidArgument("lease", "stop lease duration is too large")
	}
	return millis, nil
}

func lockStopClaimTask(ctx context.Context, tx pgx.Tx, taskID string) error {
	var locked string
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock task for stop worker action: %w", err)
	}
	return nil
}

func requireStopClaim(ctx context.Context, tx pgx.Tx, taskID, stopID, worker string, version int64) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_stop_requests sr JOIN tasks t ON t.id=sr.task_id
WHERE sr.task_id=$1 AND sr.id=$2 AND sr.status='pending' AND sr.lease_owner=$3 AND sr.lease_version=$4
AND sr.lease_expires_at > clock_timestamp() AND t.status IN ('pausing','cancelling'))`, taskID, stopID, worker, version).Scan(&valid)
	if err != nil {
		return fmt.Errorf("verify task stop worker lease: %w", err)
	}
	if !valid {
		return stopClaimConflict()
	}
	return nil
}

func stopClaimConflict() error {
	return domain.NewError(domain.CodeConflict, "task stop worker lease is stale")
}

var _ app.TaskActionStore = (*Store)(nil)
