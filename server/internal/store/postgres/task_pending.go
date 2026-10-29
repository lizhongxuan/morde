package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

// RespondToPendingRequest consumes one exact active approval and queues its
// task in the same transaction. Idempotent retries return success without
// applying the decision twice.
func (s *Store) RespondToPendingRequest(ctx context.Context, record app.IdempotencyRecord, taskID, pendingID string, version int64, decision task.PendingDecision) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingID) || version < 1 ||
		(decision != task.PendingDecisionApprove && decision != task.PendingDecisionReject) {
		return domain.InvalidArgument("pending_request", "pending request response fields are invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return err
	}
	if record.Operation != "task.pending.respond" || record.ResourceID != taskID || record.RelatedID != pendingID {
		return domain.InvalidArgument("idempotency", "pending request idempotency identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, replayed, err := findIdempotency(ctx, tx, record); err != nil {
			return err
		} else if replayed {
			return nil
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, record.OwnerID, nil)
		if err != nil {
			return err
		}
		var status string
		var leaseVersion, taskEpoch int64
		if err := tx.QueryRow(ctx, `SELECT status, lease_version, execution_epoch FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, record.OwnerID).Scan(&status, &leaseVersion, &taskEpoch); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock task for pending response: %w", err)
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		stored, replayed, err := reserveIdempotency(ctx, tx, record, taskID, pendingID)
		if err != nil {
			return err
		}
		if replayed {
			return nil
		}
		if status != string(task.StatusWaitingUser) {
			return domain.NewError(domain.CodeConflict, "task is no longer waiting for this response")
		}
		if leaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		var storedVersion int64
		var requestStatus string
		if err := tx.QueryRow(ctx, `SELECT version, status FROM task_pending_requests WHERE id=$1 AND task_id=$2 FOR UPDATE`, pendingID, taskID).
			Scan(&storedVersion, &requestStatus); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "pending request is no longer active")
		} else if err != nil {
			return fmt.Errorf("lock pending request: %w", err)
		}
		if storedVersion != version || requestStatus != string(task.PendingRequestPending) {
			return domain.NewError(domain.CodeConflict, "pending request version is stale or already resolved")
		}
		requestStatus = string(task.PendingRequestApproved)
		if decision == task.PendingDecisionReject {
			requestStatus = string(task.PendingRequestRejected)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_pending_requests SET status=$3, decision=$4, responded_at=clock_timestamp()
WHERE id=$1 AND task_id=$2`, pendingID, taskID, requestStatus, string(decision)); err != nil {
			return fmt.Errorf("persist pending request decision: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"pending_id": pendingID, "pending_version": version, "decision": string(decision),
			"idempotency_replayed": false, "status": string(task.StatusQueued), "lease_version": leaseVersion + 1,
		})
		if err != nil {
			return fmt.Errorf("encode pending response event: %w", err)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='queued', lease_version=lease_version+1, waiting_reason='', event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='waiting_user' AND lease_owner='' AND lease_expires_at IS NULL
RETURNING event_seq`, taskID, record.OwnerID).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task state changed while applying the response")
		}
		if err != nil {
			return fmt.Errorf("queue task after pending response: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.response", Payload: payload}); err != nil {
			return err
		}
		_ = stored
		return nil
	})
}

// AnswerClarificationRequest stores a free-form answer and requeues only the
// task waiting for the exact active clarification version.
func (s *Store) AnswerClarificationRequest(ctx context.Context, record app.IdempotencyRecord, taskID, clarificationID string, version int64, answer string, fileIntent task.FileDeliveryIntent, objective string) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(clarificationID) || version < 1 || !validClarificationText(answer) {
		return domain.InvalidArgument("clarification", "clarification response fields are invalid")
	}
	if (fileIntent == "") != (objective == "") || (fileIntent != "" && (!fileIntent.Valid() || fileIntent == task.FileDeliveryUnclear || strings.TrimSpace(objective) == "")) {
		return domain.InvalidArgument("clarification", "file delivery clarification resolution is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return err
	}
	if record.Operation != "task.clarification.answer" || record.ResourceID != taskID || record.RelatedID != clarificationID {
		return domain.InvalidArgument("idempotency", "clarification idempotency identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, replayed, err := findIdempotency(ctx, tx, record); err != nil {
			return err
		} else if replayed {
			return nil
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, record.OwnerID, nil)
		if err != nil {
			return err
		}
		var status, waitingReason, storedFileIntent, workspaceID string
		var leaseVersion, taskEpoch int64
		if err := tx.QueryRow(ctx, `SELECT status, waiting_reason, lease_version, execution_epoch, file_delivery_intent, COALESCE(workspace_id, '') FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, record.OwnerID).
			Scan(&status, &waitingReason, &leaseVersion, &taskEpoch, &storedFileIntent, &workspaceID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock task for clarification answer: %w", err)
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if _, replayed, err := reserveIdempotency(ctx, tx, record, taskID, clarificationID); err != nil {
			return err
		} else if replayed {
			return nil
		}
		if status != string(task.StatusWaitingUser) || waitingReason != "clarification_required" || leaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeConflict, "task is no longer waiting for this clarification")
		}
		var storedVersion int64
		var requestStatus, callID string
		if err := tx.QueryRow(ctx, `SELECT version, status, call_id FROM task_clarification_requests WHERE id=$1 AND task_id=$2 FOR UPDATE`, clarificationID, taskID).
			Scan(&storedVersion, &requestStatus, &callID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "clarification request is no longer active")
		} else if err != nil {
			return fmt.Errorf("lock task clarification request: %w", err)
		}
		if storedVersion != version || requestStatus != string(task.ClarificationPending) {
			return domain.NewError(domain.CodeConflict, "clarification version is stale or already answered")
		}
		if callID == app.FileDeliveryIntentClarificationCallID {
			if fileIntent == "" || storedFileIntent != string(task.FileDeliveryUnclear) {
				return domain.NewError(domain.CodeConflict, "file delivery clarification resolution is missing or stale")
			}
			if fileIntent == task.FileDeliveryRequired && workspaceID == "" {
				return domain.InvalidArgument("workspace_id", "file delivery requires a Workspace")
			}
		} else if fileIntent != "" {
			return domain.InvalidArgument("clarification", "only file delivery clarifications may change file delivery intent")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_clarification_requests SET status='answered', answer=$3, responded_at=clock_timestamp()
WHERE id=$1 AND task_id=$2`, clarificationID, taskID, answer); err != nil {
			return fmt.Errorf("persist task clarification answer: %w", err)
		}
		payloadValues := map[string]any{
			"clarification_id": clarificationID, "clarification_version": version, "answer": answer,
			"idempotency_replayed": false, "status": string(task.StatusQueued), "lease_version": leaseVersion + 1,
		}
		if fileIntent != "" {
			payloadValues["file_delivery_intent"] = fileIntent
		}
		payload, err := json.Marshal(payloadValues)
		if err != nil {
			return fmt.Errorf("encode clarification answer event: %w", err)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET objective=CASE WHEN $3='' THEN objective ELSE $3 END,
file_delivery_intent=CASE WHEN $4='' THEN file_delivery_intent ELSE $4 END,
status='queued', lease_version=lease_version+1, waiting_reason='', event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='waiting_user' AND waiting_reason='clarification_required' AND lease_owner='' AND lease_expires_at IS NULL
RETURNING event_seq`, taskID, record.OwnerID, objective, string(fileIntent)).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task state changed while applying the clarification")
		}
		if err != nil {
			return fmt.Errorf("queue task after clarification answer: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.response", Payload: payload})
	})
}

// cancelPendingTaskRequest settles an outstanding approval in the same
// transaction that changes its task to cancelling, so a late user response
// cannot requeue work after cancellation began.
func cancelPendingTaskRequest(ctx context.Context, tx pgx.Tx, taskID string) error {
	if _, err := tx.Exec(ctx, `UPDATE task_pending_requests SET status='cancelled', decision='', responded_at=clock_timestamp()
WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		return fmt.Errorf("cancel pending task approval: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_clarification_requests SET status='cancelled', responded_at=clock_timestamp()
WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		return fmt.Errorf("cancel pending task clarification: %w", err)
	}
	return nil
}
