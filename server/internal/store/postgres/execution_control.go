package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/task"
)

type executionGate struct {
	state execution.State
	epoch int64
}

func (s *Store) GetExecutionControl(ctx context.Context, ownerID string) (execution.Control, error) {
	var result execution.Control
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state, epoch, updated_at FROM owner_execution_controls WHERE owner_id=$1`, ownerID).
			Scan(&state, &result.Epoch, &result.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
			return missingExecutionGateError()
		} else if err != nil {
			return fmt.Errorf("read owner execution control: %w", err)
		}
		result.OwnerID = ownerID
		result.State = execution.State(state)
		if !result.State.Valid() || result.Epoch < 1 {
			return missingExecutionGateError()
		}
		if err := readPendingExecutionTaskCount(ctx, tx, ownerID, &result.PendingTaskCount); err != nil {
			return err
		}
		return readExecutionRuntimeSummary(ctx, tx, ownerID, &result.Runtime)
	})
	return result, err
}

func (s *Store) StopAllExecution(ctx context.Context, record app.IdempotencyRecord, expectedEpoch int64) (execution.Control, error) {
	return s.setExecutionControl(ctx, record, expectedEpoch, execution.StateDisabled)
}

func (s *Store) EnableExecution(ctx context.Context, record app.IdempotencyRecord, expectedEpoch int64) (execution.Control, error) {
	return s.setExecutionControl(ctx, record, expectedEpoch, execution.StateEnabled)
}

func (s *Store) setExecutionControl(ctx context.Context, record app.IdempotencyRecord, expectedEpoch int64, target execution.State) (execution.Control, error) {
	stopAll := record.Operation == "execution.stop_all"
	if !domain.ValidUserID(record.OwnerID) || expectedEpoch < 1 || !target.Valid() || target == execution.StateStopping ||
		record.ResourceID != record.OwnerID || record.RelatedID != "pending" ||
		(!stopAll && record.Operation != "execution.enable") || (stopAll && target != execution.StateDisabled) || (!stopAll && target != execution.StateEnabled) {
		return execution.Control{}, domain.InvalidArgument("execution_control", "execution control request is invalid")
	}
	var result execution.Control
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, record.OwnerID, true)
		if err != nil {
			return err
		}
		stored, replayed, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replayed {
			result, err = executionSnapshotFromIdempotency(record.OwnerID, stored.RelatedID)
			if err != nil {
				return err
			}
			var updatedAt time.Time
			if err := tx.QueryRow(ctx, `SELECT updated_at FROM owner_execution_controls WHERE owner_id=$1`, record.OwnerID).Scan(&updatedAt); err != nil {
				return missingExecutionGateError()
			}
			result.UpdatedAt = updatedAt
			if !result.Runtime.Valid() {
				return readExecutionRuntimeSummary(ctx, tx, record.OwnerID, &result.Runtime)
			}
			return nil
		}
		if gate.epoch != expectedEpoch {
			conflict := domain.NewError(domain.CodeConflict, "owner execution epoch changed")
			conflict.Details["expected_epoch"] = expectedEpoch
			conflict.Details["actual_epoch"] = gate.epoch
			return conflict
		}
		if stopAll {
			switch gate.state {
			case execution.StateEnabled:
				gate, err = transitionOwnerExecutionControl(ctx, tx, record.OwnerID, gate, execution.StateStopping)
				if err != nil {
					return err
				}
			case execution.StateDisabled:
				var pending int64
				if err := readActiveExecutionTaskCount(ctx, tx, record.OwnerID, &pending); err != nil {
					return err
				}
				var pendingStops int64
				if err := readPendingTaskStopRequestCount(ctx, tx, record.OwnerID, &pendingStops); err != nil {
					return err
				}
				runtimeStopped, err := accountAgentRuntimeStopConfirmed(ctx, tx, record.OwnerID)
				if err != nil {
					return err
				}
				if pending > 0 || pendingStops > 0 || !runtimeStopped {
					gate, err = transitionOwnerExecutionControl(ctx, tx, record.OwnerID, gate, execution.StateStopping)
					if err != nil {
						return err
					}
				}
			}
			if gate.state == execution.StateStopping {
				if err := requestOwnerAccountAgentRuntimeStop(ctx, tx, record.OwnerID); err != nil {
					return err
				}
				if err := disableOwnerSchedules(ctx, tx, record.OwnerID); err != nil {
					return err
				}
				if err := stopAllOwnerTasks(ctx, tx, record.OwnerID); err != nil {
					return err
				}
				if err := requestOwnerMCPRuntimeCancellations(ctx, tx, record.OwnerID); err != nil {
					return err
				}
				var pending int64
				if err := readActiveExecutionTaskCount(ctx, tx, record.OwnerID, &pending); err != nil {
					return err
				}
				var pendingStops int64
				if err := readPendingTaskStopRequestCount(ctx, tx, record.OwnerID, &pendingStops); err != nil {
					return err
				}
				runtimeStopped, err := accountAgentRuntimeStopConfirmed(ctx, tx, record.OwnerID)
				if err != nil {
					return err
				}
				if pending == 0 && pendingStops == 0 && runtimeStopped {
					gate, err = transitionOwnerExecutionControl(ctx, tx, record.OwnerID, gate, execution.StateDisabled)
					if err != nil {
						return err
					}
				}
			}
		} else {
			if gate.state == execution.StateStopping {
				return domain.NewError(domain.CodeConflict, "owner execution is still stopping")
			}
			if gate.state == execution.StateDisabled {
				runtimeStopped, err := accountAgentRuntimeStopConfirmed(ctx, tx, record.OwnerID)
				if err != nil {
					return err
				}
				if !runtimeStopped {
					return domain.NewError(domain.CodeConflict, "account Runtime stop is unconfirmed")
				}
				gate, err = transitionOwnerExecutionControl(ctx, tx, record.OwnerID, gate, execution.StateEnabled)
				if err != nil {
					return err
				}
			}
		}
		result = execution.Control{OwnerID: record.OwnerID, State: gate.state, Epoch: gate.epoch}
		if err := readPendingExecutionTaskCount(ctx, tx, record.OwnerID, &result.PendingTaskCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT updated_at FROM owner_execution_controls WHERE owner_id=$1`, record.OwnerID).Scan(&result.UpdatedAt); err != nil {
			return missingExecutionGateError()
		}
		if err := readExecutionRuntimeSummary(ctx, tx, record.OwnerID, &result.Runtime); err != nil {
			return err
		}
		encoded := encodeExecutionSnapshot(result)
		if _, _, err := reserveIdempotency(ctx, tx, record, record.OwnerID, encoded); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func disableOwnerSchedules(ctx context.Context, tx pgx.Tx, ownerID string) error {
	if _, err := tx.Exec(ctx, `UPDATE schedules SET status='disabled',version=version+1,
next_occurrence_key='',next_run_at=NULL,next_check_at=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND status='enabled'`, ownerID); err != nil {
		return fmt.Errorf("disable owner schedules: %w", err)
	}
	return nil
}

// ReconcileStoppingExecution finalizes owner gates after the durable stop
// worker has settled every task. It is safe to run on every worker poll.
func (s *Store) ReconcileStoppingExecution(ctx context.Context) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT owner_id, epoch FROM owner_execution_controls WHERE state='stopping' ORDER BY owner_id FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("list stopping execution controls: %w", err)
		}
		type ownerEpoch struct {
			owner string
			epoch int64
		}
		owners := make([]ownerEpoch, 0)
		for rows.Next() {
			var item ownerEpoch
			if err := rows.Scan(&item.owner, &item.epoch); err != nil {
				rows.Close()
				return fmt.Errorf("read stopping execution control: %w", err)
			}
			owners = append(owners, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate stopping execution controls: %w", err)
		}
		rows.Close()
		for _, item := range owners {
			var pending int64
			if err := readActiveExecutionTaskCount(ctx, tx, item.owner, &pending); err != nil {
				return err
			}
			var pendingStops int64
			if err := readPendingTaskStopRequestCount(ctx, tx, item.owner, &pendingStops); err != nil {
				return err
			}
			runtimeStopped, err := accountAgentRuntimeStopConfirmed(ctx, tx, item.owner)
			if err != nil {
				return err
			}
			if pending == 0 && pendingStops == 0 && runtimeStopped {
				if item.epoch == math.MaxInt64 {
					return domain.NewError(domain.CodeInvalidState, "owner execution epoch is exhausted")
				}
				command, err := tx.Exec(ctx, `UPDATE owner_execution_controls SET state='disabled', epoch=epoch+1, updated_at=clock_timestamp()
WHERE owner_id=$1 AND state='stopping' AND epoch=$2`, item.owner, item.epoch)
				if err != nil {
					return fmt.Errorf("finalize owner execution stop: %w", err)
				}
				if command.RowsAffected() != 1 {
					return domain.NewError(domain.CodeConflict, "owner execution control changed while finalizing stop")
				}
			}
		}
		return nil
	})
}

func readExecutionRuntimeSummary(ctx context.Context, tx pgx.Tx, ownerID string, target *execution.Runtime) error {
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	runtime, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, ownerID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		*target = execution.Runtime{State: execution.RuntimeStateNotStarted}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read execution account Runtime summary: %w", err)
	}
	state := execution.RuntimeState(runtime.State)
	if state != execution.RuntimeStateStarting && state != execution.RuntimeStateRunning && state != execution.RuntimeStateStopping &&
		state != execution.RuntimeStateStopped && state != execution.RuntimeStateFailed {
		return domain.NewError(domain.CodeInternal, "account Runtime state is unavailable")
	}
	var activeTaskCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_agent_instances
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND account_runtime_id=$3 AND account_runtime_generation=$4 AND state='active'`,
		runtime.Fence.ServiceInstanceID, ownerID, runtime.Fence.RuntimeID, runtime.Fence.RuntimeGeneration).Scan(&activeTaskCount); err != nil {
		return fmt.Errorf("count active account Runtime tasks: %w", err)
	}
	*target = execution.Runtime{State: state, RuntimeID: runtime.Fence.RuntimeID,
		RuntimeGeneration: runtime.Fence.RuntimeGeneration, MaxConcurrentTasks: runtime.MaxConcurrentTasks,
		ActiveTaskCount: activeTaskCount}
	return nil
}

func accountAgentRuntimeStopConfirmed(ctx context.Context, tx pgx.Tx, ownerID string) (bool, error) {
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return false, err
	}
	runtime, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, ownerID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock account Runtime stop proof: %w", err)
	}
	if runtime.State != app.AccountAgentRuntimeStopped || !isLowerHexRuntimeNonce(runtime.NonceSHA256) {
		return false, nil
	}
	var attestation, digest []byte
	if err := tx.QueryRow(ctx, `SELECT guardian_stop_attestation,guardian_stop_attestation_digest
FROM account_agent_runtimes WHERE service_instance_id=$1::uuid AND owner_id=$2`, serviceInstanceID, ownerID).
		Scan(&attestation, &digest); err != nil {
		return false, fmt.Errorf("read Guardian Runtime stop proof: %w", err)
	}
	if len(attestation) == 0 || len(attestation) > 16384 || len(digest) != sha256.Size {
		return false, nil
	}
	actualDigest := sha256.Sum256(attestation)
	return bytes.Equal(actualDigest[:], digest), nil
}

func readPendingTaskStopRequestCount(ctx context.Context, tx pgx.Tx, ownerID string, target *int64) error {
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_stop_requests sr JOIN tasks t ON t.id=sr.task_id
WHERE t.owner_id=$1 AND sr.status='pending'`, ownerID).Scan(target); err != nil {
		return fmt.Errorf("count pending owner Task stop requests: %w", err)
	}
	return nil
}

func lockOwnerExecutionControl(ctx context.Context, tx pgx.Tx, ownerID string, exclusive bool) (executionGate, error) {
	lock := "FOR SHARE"
	if exclusive {
		lock = "FOR UPDATE"
	}
	var gate executionGate
	err := tx.QueryRow(ctx, `SELECT state, epoch FROM owner_execution_controls WHERE owner_id=$1 `+lock, ownerID).
		Scan(&gate.state, &gate.epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return executionGate{}, missingExecutionGateError()
	}
	if err != nil {
		return executionGate{}, fmt.Errorf("lock owner execution control: %w", err)
	}
	if !gate.state.Valid() || gate.epoch < 1 {
		return executionGate{}, missingExecutionGateError()
	}
	return gate, nil
}

func requireOwnerExecutionEnabled(ctx context.Context, tx pgx.Tx, ownerID string, expectedEpoch *int64) (executionGate, error) {
	gate, err := lockOwnerExecutionControl(ctx, tx, ownerID, false)
	if err != nil {
		return executionGate{}, err
	}
	if gate.state != execution.StateEnabled || (expectedEpoch != nil && gate.epoch != *expectedEpoch) {
		return executionGate{}, ownerExecutionDisabledError(gate)
	}
	return gate, nil
}

func transitionOwnerExecutionControl(ctx context.Context, tx pgx.Tx, ownerID string, current executionGate, state execution.State) (executionGate, error) {
	if current.epoch == math.MaxInt64 {
		return executionGate{}, domain.NewError(domain.CodeInvalidState, "owner execution epoch is exhausted")
	}
	if _, err := tx.Exec(ctx, `UPDATE owner_execution_controls SET state=$2, epoch=$3, updated_at=clock_timestamp()
WHERE owner_id=$1 AND epoch=$4`, ownerID, string(state), current.epoch+1, current.epoch); err != nil {
		return executionGate{}, fmt.Errorf("advance owner execution control: %w", err)
	}
	return executionGate{state: state, epoch: current.epoch + 1}, nil
}

func stopAllOwnerTasks(ctx context.Context, tx pgx.Tx, ownerID string) error {
	type activeTask struct {
		id        string
		status    task.Status
		leaseVer  int64
		safeAfter time.Time
	}
	rows, err := tx.Query(ctx, `SELECT t.id, t.status, t.lease_version,
GREATEST(COALESCE(t.runtime_lease_safe_after, '-infinity'::timestamptz), COALESCE(t.lease_expires_at, '-infinity'::timestamptz),
COALESCE((SELECT max(sr.safe_after) FROM task_stop_requests sr WHERE sr.task_id=t.id AND sr.status='pending'), '-infinity'::timestamptz), clock_timestamp())
FROM tasks t WHERE t.owner_id=$1 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
ORDER BY t.id FOR UPDATE OF t`, ownerID)
	if err != nil {
		return fmt.Errorf("lock active owner tasks for stop-all: %w", err)
	}
	values := make([]activeTask, 0)
	for rows.Next() {
		var item activeTask
		if err := rows.Scan(&item.id, &item.status, &item.leaseVer, &item.safeAfter); err != nil {
			rows.Close()
			return fmt.Errorf("read active owner task: %w", err)
		}
		values = append(values, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate active owner tasks: %w", err)
	}
	rows.Close()
	for _, item := range values {
		var pendingCancelID string
		err := tx.QueryRow(ctx, `SELECT id FROM task_stop_requests WHERE task_id=$1 AND action='cancel' AND status='pending' FOR UPDATE`, item.id).Scan(&pendingCancelID)
		if err == nil && item.status == task.StatusCancelling {
			if err := cancelPendingTaskRequest(ctx, tx, item.id); err != nil {
				return err
			}
			continue
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find pending owner task cancellation: %w", err)
		}
		if item.leaseVer == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_stop_requests SET status='completed', lease_owner='', lease_expires_at=NULL,
completed_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE task_id=$1 AND action='pause' AND status='pending'`, item.id); err != nil {
			return fmt.Errorf("replace pending task pause with global cancellation: %w", err)
		}
		if err := cancelPendingTaskRequest(ctx, tx, item.id); err != nil {
			return err
		}
		requestID, err := domain.NewTaskActionID()
		if err != nil {
			return fmt.Errorf("generate global task stop id: %w", err)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='cancelling', lease_version=lease_version+1, lease_owner='', lease_expires_at=NULL,
waiting_reason='', finished_at=NULL, event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 RETURNING event_seq`, item.id).Scan(&sequence)
		if err != nil {
			return fmt.Errorf("fence task for global cancellation: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,'cancel','pending',$3)`, requestID, item.id, item.safeAfter); err != nil {
			return fmt.Errorf("persist global task cancellation: %w", err)
		}
		if err := insertTaskStatusEvent(ctx, tx, item.id, sequence, task.StatusCancelling, task.ActionCancel, requestID); err != nil {
			return err
		}
	}
	return nil
}

func readActiveExecutionTaskCount(ctx context.Context, tx pgx.Tx, ownerID string, target *int64) error {
	if err := tx.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM tasks WHERE owner_id=$1 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')) +
  (SELECT count(*) FROM mcp_runtime_sessions s JOIN mcp_runtime_operations o
     ON o.owner_id=s.owner_id AND o.api_key_id=s.api_key_id AND o.session_id=s.session_id AND o.host_id=s.host_id AND o.operation_id=s.active_operation_id
   WHERE s.owner_id=$1 AND o.status IN ('prepared','dispatched','unknown'))`, ownerID).Scan(target); err != nil {
		return fmt.Errorf("count active owner tasks: %w", err)
	}
	return nil
}

func requestOwnerMCPRuntimeCancellations(ctx context.Context, tx pgx.Tx, ownerID string) error {
	if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_operations o
SET cancel_requested_at=COALESCE(o.cancel_requested_at, clock_timestamp()), updated_at=clock_timestamp()
FROM mcp_runtime_sessions s
WHERE s.owner_id=o.owner_id AND s.api_key_id=o.api_key_id AND s.session_id=o.session_id AND s.host_id=o.host_id
  AND s.active_operation_id=o.operation_id AND o.owner_id=$1
  AND o.status IN ('prepared','dispatched','unknown')`, ownerID); err != nil {
		return fmt.Errorf("persist owner MCP runtime cancellation requests: %w", err)
	}
	return nil
}

func readPendingExecutionTaskCount(ctx context.Context, tx pgx.Tx, ownerID string, target *int64) error {
	if err := tx.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM tasks WHERE owner_id=$1 AND status IN ('pausing','cancelling')) +
  (SELECT count(*) FROM mcp_runtime_sessions s JOIN mcp_runtime_operations o
     ON o.owner_id=s.owner_id AND o.api_key_id=s.api_key_id AND o.session_id=s.session_id AND o.host_id=s.host_id AND o.operation_id=s.active_operation_id
   WHERE s.owner_id=$1 AND o.status IN ('prepared','dispatched','unknown'))`, ownerID).Scan(target); err != nil {
		return fmt.Errorf("count owner tasks awaiting stop: %w", err)
	}
	return nil
}

func encodeExecutionSnapshot(value execution.Control) string {
	encoded, _ := json.Marshal(struct {
		State            execution.State   `json:"state"`
		Epoch            int64             `json:"epoch"`
		PendingTaskCount int64             `json:"pending_task_count"`
		Runtime          execution.Runtime `json:"runtime"`
	}{State: value.State, Epoch: value.Epoch, PendingTaskCount: value.PendingTaskCount, Runtime: value.Runtime})
	return "v2:" + base64.RawURLEncoding.EncodeToString(encoded)
}

func executionSnapshotFromIdempotency(ownerID, encoded string) (execution.Control, error) {
	if strings.HasPrefix(encoded, "v2:") {
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "v2:"))
		if err != nil {
			return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		var value struct {
			State            execution.State   `json:"state"`
			Epoch            int64             `json:"epoch"`
			PendingTaskCount int64             `json:"pending_task_count"`
			Runtime          execution.Runtime `json:"runtime"`
		}
		if err := json.Unmarshal(payload, &value); err != nil {
			return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		result := execution.Control{OwnerID: ownerID, State: value.State, Epoch: value.Epoch,
			PendingTaskCount: value.PendingTaskCount, Runtime: value.Runtime}
		if !result.State.Valid() || result.Epoch < 1 || result.PendingTaskCount < 0 || !result.Runtime.Valid() {
			return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		return result, nil
	}
	parts := strings.Split(encoded, ":")
	if len(parts) != 3 {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	var value execution.Control
	value.OwnerID = ownerID
	value.State = execution.State(parts[0])
	var err error
	if value.Epoch, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if value.PendingTaskCount, err = strconv.ParseInt(parts[2], 10, 64); err != nil {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if !value.State.Valid() || value.Epoch < 1 || value.PendingTaskCount < 0 {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return value, nil
}

func missingExecutionGateError() error {
	return domain.NewError(domain.CodeInternal, "owner execution control is unavailable")
}

func ownerExecutionDisabledError(gate executionGate) error {
	err := domain.NewError(domain.CodeConflict, "owner execution is not enabled")
	err.Details["state"] = string(gate.state)
	err.Details["epoch"] = gate.epoch
	return err
}
