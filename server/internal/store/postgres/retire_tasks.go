package postgres

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
)

// RetireTaskExecution closes every existing owner's execution gate and records
// durable cancellation requests. The task stop worker must keep running until
// ReconcileStoppingExecution has finalized every gate. This transaction locks
// the gate table so a concurrent submit or schedule change is either included
// in the retirement or rejected by the closed gate afterward.
func (s *Store) RetireTaskExecution(ctx context.Context) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE owner_execution_controls IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("lock owner execution controls for retirement: %w", err)
		}
		var missing int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users AS u
LEFT JOIN owner_execution_controls AS ec ON ec.owner_id=u.id
WHERE ec.owner_id IS NULL`).Scan(&missing); err != nil {
			return fmt.Errorf("check owner execution controls: %w", err)
		}
		if missing != 0 {
			return domain.NewError(domain.CodeInvalidState, "owners without execution controls cannot be retired")
		}
		rows, err := tx.Query(ctx, `SELECT owner_id, state, epoch
FROM owner_execution_controls ORDER BY owner_id FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("list owner execution controls for retirement: %w", err)
		}
		type ownerControl struct {
			ownerID string
			gate    executionGate
		}
		owners := make([]ownerControl, 0)
		for rows.Next() {
			var item ownerControl
			if err := rows.Scan(&item.ownerID, &item.gate.state, &item.gate.epoch); err != nil {
				rows.Close()
				return fmt.Errorf("read owner execution control for retirement: %w", err)
			}
			if !item.gate.state.Valid() || item.gate.epoch < 1 ||
				(item.gate.epoch == math.MaxInt64 && item.gate.state != execution.StateDisabled) {
				rows.Close()
				return domain.NewError(domain.CodeInvalidState, "owner execution control cannot be safely retired")
			}
			owners = append(owners, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate owner execution controls for retirement: %w", err)
		}
		rows.Close()
		for _, item := range owners {
			gate := item.gate
			if gate.state == execution.StateEnabled {
				gate, err = transitionOwnerExecutionControl(ctx, tx, item.ownerID, gate, execution.StateStopping)
				if err != nil {
					return err
				}
			}
			if err := disableOwnerSchedules(ctx, tx, item.ownerID); err != nil {
				return err
			}
			if err := stopAllOwnerTasks(ctx, tx, item.ownerID); err != nil {
				return err
			}
			if err := requestOwnerMCPRuntimeCancellations(ctx, tx, item.ownerID); err != nil {
				return err
			}
			var active, pendingStops int64
			if err := readActiveExecutionTaskCount(ctx, tx, item.ownerID, &active); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_stop_requests AS sr
JOIN tasks AS t ON t.id=sr.task_id
WHERE t.owner_id=$1 AND sr.status='pending'`, item.ownerID).Scan(&pendingStops); err != nil {
				return fmt.Errorf("count pending task stops for retirement: %w", err)
			}
			if active != 0 || pendingStops != 0 {
				if gate.state == execution.StateDisabled {
					if gate.epoch == math.MaxInt64 {
						return domain.NewError(domain.CodeInvalidState, "owner execution control cannot be safely retired")
					}
					if _, err := transitionOwnerExecutionControl(ctx, tx, item.ownerID, gate, execution.StateStopping); err != nil {
						return err
					}
				}
				continue
			}
			if gate.state == execution.StateStopping {
				if _, err := transitionOwnerExecutionControl(ctx, tx, item.ownerID, gate, execution.StateDisabled); err != nil {
					return err
				}
			}
		}
		var undrainable bool
		if err := tx.QueryRow(ctx, `SELECT
  EXISTS (SELECT 1 FROM task_stop_requests AS sr JOIN tasks AS t ON t.id=sr.task_id
    WHERE sr.status='pending' AND t.status NOT IN ('pausing','cancelling'))
  OR EXISTS (SELECT 1 FROM task_operations AS o JOIN tasks AS t ON t.id=o.task_id
    WHERE o.status IN ('prepared','dispatched','unknown') AND t.status IN ('completed','failed','cancelled'))
  OR EXISTS (SELECT 1 FROM mcp_runtime_operations AS o
    LEFT JOIN mcp_runtime_sessions AS s
      ON s.owner_id=o.owner_id AND s.api_key_id=o.api_key_id AND s.session_id=o.session_id AND s.host_id=o.host_id
    WHERE o.status IN ('prepared','dispatched','unknown') AND s.active_operation_id IS DISTINCT FROM o.operation_id)`).Scan(&undrainable); err != nil {
			return fmt.Errorf("check undrainable task execution state: %w", err)
		}
		if undrainable {
			return domain.NewError(domain.CodeInvalidState, "task execution state cannot be safely retired")
		}
		return nil
	})
}
