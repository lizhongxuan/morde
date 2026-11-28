package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// taskTerminalDeliveryPublicWaitingReason is the explicit allowlist of
// persisted internal reasons that mean an external operation's outcome is
// unresolved. Only the bounded category is ever returned to the model worker.
func taskTerminalDeliveryPublicWaitingReason(reason string) (string, bool) {
	switch reason {
	case "mcp_result_unknown", "hub_action_unknown", "tool_result_unknown", "target_result_unknown", "dsh_recovery_unresolved":
		if reason == "dsh_recovery_unresolved" {
			return "reconciliation_required", true
		}
		return "unknown_outcome", true
	default:
		return "", false
	}
}

// enqueueTaskTerminalDeliveryInTx records an immutable source for a server Task
// failure, cancellation, or unresolved-operation reply. The caller must have
// committed the matching Task state and event in the same transaction. Pauses,
// ordinary waits, non-server Tasks, and internal route Tasks do not produce
// conversation delivery work.
func enqueueTaskTerminalDeliveryInTx(ctx context.Context, tx pgx.Tx, taskID string, sequence int64, eventType string) error {
	if !domain.ValidTaskID(taskID) || sequence < 1 || !validTaskEventType(eventType) {
		return domain.InvalidArgument("task_terminal_delivery", "terminal delivery source is invalid")
	}
	var status, executionLocation string
	var internalTool bool
	err := tx.QueryRow(ctx, `SELECT status,execution_location,EXISTS (
  SELECT 1 FROM conversation_route_internal_tool_calls route WHERE route.task_id=tasks.id
)
FROM tasks WHERE id=$1`, taskID).Scan(&status, &executionLocation, &internalTool)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("read Task before enqueueing terminal delivery: %w", err)
	}
	if executionLocation != "server" || internalTool || (status != string(task.StatusFailed) && status != string(task.StatusCancelled)) {
		return nil
	}

	var deliveryID int64
	err = tx.QueryRow(ctx, `INSERT INTO task_terminal_delivery_outbox
(task_id,owner_id,conversation_id,initiator_id,task_revision,authorization_version,source_kind,
 source_event_sequence,source_event_type,source_sha256)
SELECT task.id,task.owner_id,task.conversation_id,task.initiator_id,task.current_revision,task.auth_version,
       CASE task.status WHEN 'failed' THEN 'terminal_failure' ELSE 'terminal_cancellation' END,
       event.sequence,event.event_type,sha256(convert_to(event.payload::text,'UTF8'))
FROM tasks task JOIN task_events event ON event.task_id=task.id AND event.sequence=$2 AND event.event_type=$3
WHERE task.id=$1 AND task.execution_location='server' AND task.status IN ('failed','cancelled')
ON CONFLICT DO NOTHING
RETURNING id`, taskID, sequence, eventType).Scan(&deliveryID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("enqueue Task terminal delivery source: %w", err)
	}

	// ON CONFLICT is the idempotent replay path. A zero-row insert without the
	// same source identity means the caller did not persist the claimed event.
	var sourceExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_terminal_delivery_outbox
  WHERE task_id=$1 AND source_event_sequence=$2 AND source_event_type=$3
)`, taskID, sequence, eventType).Scan(&sourceExists); err != nil {
		return fmt.Errorf("verify Task terminal delivery replay: %w", err)
	}
	if sourceExists {
		return nil
	}
	return fmt.Errorf("terminal delivery source event %s/%d/%s was not persisted", taskID, sequence, eventType)
}

// enqueueTaskWaitingUnknownDeliveryInTx records selected durable waits only
// when the Task is waiting without an active approval or clarification card.
// The caller must have committed the matching Task state and event in the same
// transaction.
func enqueueTaskWaitingUnknownDeliveryInTx(ctx context.Context, tx pgx.Tx, taskID string, sequence int64, eventType string) error {
	if !domain.ValidTaskID(taskID) || sequence < 1 || !validTaskEventType(eventType) {
		return domain.InvalidArgument("task_terminal_delivery", "waiting delivery source is invalid")
	}
	if eventType != "task.waiting_user" {
		return nil
	}
	var status, executionLocation, waitingReason string
	var internalTool, pendingApproval, pendingClarification bool
	err := tx.QueryRow(ctx, `SELECT task.status,task.execution_location,task.waiting_reason,EXISTS (
  SELECT 1 FROM conversation_route_internal_tool_calls route WHERE route.task_id=task.id
),EXISTS (
  SELECT 1 FROM task_pending_requests pending WHERE pending.task_id=task.id AND pending.status='pending'
),EXISTS (
  SELECT 1 FROM task_clarification_requests clarification WHERE clarification.task_id=task.id AND clarification.status='pending'
)
FROM tasks task WHERE task.id=$1`, taskID).Scan(
		&status, &executionLocation, &waitingReason, &internalTool, &pendingApproval, &pendingClarification)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("read Task before enqueueing waiting delivery: %w", err)
	}
	if status != string(task.StatusWaitingUser) || executionLocation != "server" || internalTool ||
		pendingApproval || pendingClarification {
		return nil
	}
	if _, ok := taskTerminalDeliveryPublicWaitingReason(waitingReason); !ok {
		return nil
	}

	var deliveryID int64
	err = tx.QueryRow(ctx, `INSERT INTO task_terminal_delivery_outbox
(task_id,owner_id,conversation_id,initiator_id,task_revision,authorization_version,source_kind,
 source_event_sequence,source_event_type,source_sha256)
SELECT task.id,task.owner_id,task.conversation_id,task.initiator_id,task.current_revision,task.auth_version,
       'waiting_unknown',event.sequence,event.event_type,sha256(convert_to(event.payload::text,'UTF8'))
FROM tasks task JOIN task_events event ON event.task_id=task.id AND event.sequence=$2 AND event.event_type=$3
WHERE task.id=$1 AND task.execution_location='server' AND task.status='waiting_user'
  AND task.waiting_reason IN ('mcp_result_unknown','hub_action_unknown','tool_result_unknown',
    'target_result_unknown','dsh_recovery_unresolved')
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls route WHERE route.task_id=task.id)
  AND NOT EXISTS (SELECT 1 FROM task_pending_requests pending WHERE pending.task_id=task.id AND pending.status='pending')
  AND NOT EXISTS (SELECT 1 FROM task_clarification_requests clarification WHERE clarification.task_id=task.id AND clarification.status='pending')
ON CONFLICT DO NOTHING
RETURNING id`, taskID, sequence, eventType).Scan(&deliveryID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("enqueue Task waiting delivery source: %w", err)
	}

	var sourceExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_terminal_delivery_outbox
  WHERE task_id=$1 AND source_event_sequence=$2 AND source_event_type=$3 AND source_kind='waiting_unknown'
)`, taskID, sequence, eventType).Scan(&sourceExists); err != nil {
		return fmt.Errorf("verify Task waiting delivery replay: %w", err)
	}
	if sourceExists {
		return nil
	}
	return fmt.Errorf("waiting delivery source event %s/%d/%s was not persisted", taskID, sequence, eventType)
}
