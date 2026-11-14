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
	"github.com/jackc/pgx/v5/pgtype"
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
	requestExpired := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, replayed, err := findIdempotency(ctx, tx, record); err != nil {
			return err
		} else if replayed {
			return nil
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, record.OwnerID, nil)
		if err != nil {
			return err
		}
		var status, workspaceID, initiatorID string
		var leaseVersion, taskEpoch int64
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, record.OwnerID, true, true)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status, lease_version, execution_epoch, COALESCE(workspace_id,''), initiator_id
FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).Scan(&status, &leaseVersion, &taskEpoch, &workspaceID, &initiatorID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock task for pending response: %w", err)
		}
		if err := lockTaskWorkspaceForExecution(ctx, tx, workspaceID, initiatorID); err != nil {
			return err
		}
		if taskEpoch != gate.epoch {
			return ownerExecutionDisabledError(gate)
		}
		if _, replayed, err := findIdempotency(ctx, tx, record); err != nil {
			return err
		} else if replayed {
			return nil
		}
		if status != string(task.StatusWaitingUser) {
			return domain.NewError(domain.CodeConflict, "task is no longer waiting for this response")
		}
		if leaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		var storedVersion int64
		var requestStatus, mediaPreviewOperationID string
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT version, status, COALESCE(media_preview_operation_id,''),
(expires_at IS NOT NULL AND expires_at <= clock_timestamp())
FROM task_pending_requests WHERE id=$1 AND task_id=$2 FOR UPDATE`, pendingID, taskID).
			Scan(&storedVersion, &requestStatus, &mediaPreviewOperationID, &expired); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "pending request is no longer active")
		} else if err != nil {
			return fmt.Errorf("lock pending request: %w", err)
		}
		if storedVersion != version || requestStatus != string(task.PendingRequestPending) {
			return domain.NewError(domain.CodeConflict, "pending request version is stale or already resolved")
		}
		if expired {
			if err := expirePendingRequestTx(ctx, tx, taskID, pendingID, version, leaseVersion); err != nil {
				return err
			}
			requestExpired = true
			return nil
		}
		requestStatus = string(task.PendingRequestApproved)
		if decision == task.PendingDecisionReject {
			requestStatus = string(task.PendingRequestRejected)
		}
		updated, err := tx.Exec(ctx, `UPDATE task_pending_requests SET status=$3, decision=$4, responded_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND status='pending' AND (expires_at IS NULL OR expires_at > clock_timestamp())`,
			pendingID, taskID, requestStatus, string(decision))
		if err != nil {
			return fmt.Errorf("persist pending request decision: %w", err)
		}
		if updated.RowsAffected() != 1 {
			if err := expirePendingRequestTx(ctx, tx, taskID, pendingID, version, leaseVersion); err != nil {
				return err
			}
			requestExpired = true
			return nil
		}
		stored, replayed, err := reserveIdempotency(ctx, tx, record, taskID, pendingID)
		if err != nil {
			return err
		}
		if replayed {
			return domain.NewError(domain.CodeConflict, "pending request was already answered")
		}
		if mediaPreviewOperationID != "" {
			if !domain.ValidOperationID(mediaPreviewOperationID) {
				return domain.NewError(domain.CodeInternal, "pending media preview identity is invalid")
			}
			nextState := "discarded"
			if decision == task.PendingDecisionApprove {
				nextState = "saving"
			}
			updated, err := tx.Exec(ctx, `UPDATE task_media_staged_objects
SET state=$3, updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_id=$2 AND state='preview' AND cleanup_state='pending'
  AND ($3='discarded' OR expires_at > clock_timestamp())`, taskID, mediaPreviewOperationID, nextState)
			if err != nil {
				return fmt.Errorf("transition pending media preview: %w", err)
			}
			if decision == task.PendingDecisionApprove && updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "task media preview is expired or no longer available")
			}
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
RETURNING event_seq`, taskID, taskOwnerID).Scan(&sequence)
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
	if err != nil {
		return err
	}
	if requestExpired {
		return domain.NewError(domain.CodeConflict, "pending request has expired")
	}
	return nil
}

// ExpirePendingRequest rejects an expired approval, records the Task event, and
// requeues only the Task waiting on that exact request version. The database
// clock is authoritative; a stale read or concurrent response is a no-op.
func (s *Store) ExpirePendingRequest(ctx context.Context, actorID, taskID, pendingID string, version int64) (bool, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingID) || version < 1 {
		return false, domain.InvalidArgument("pending_request", "pending request expiry identity is invalid")
	}
	expired := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, actorID, false, true)
		if err != nil {
			return err
		}
		var taskStatus, waitingReason, workspaceID, initiatorID, leaseOwner string
		var leaseVersion int64
		var leaseExpiresAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT status, waiting_reason, lease_version, COALESCE(workspace_id,''), initiator_id,
lease_owner, lease_expires_at FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).
			Scan(&taskStatus, &waitingReason, &leaseVersion, &workspaceID, &initiatorID, &leaseOwner, &leaseExpiresAt); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock Task for pending approval expiry: %w", err)
		}
		if taskStatus != string(task.StatusWaitingUser) || waitingReason != "approval_required" || leaseOwner != "" || leaseExpiresAt.Valid {
			return nil
		}
		if err := lockTaskWorkspaceForExecution(ctx, tx, workspaceID, initiatorID); err != nil {
			return err
		}
		var storedVersion int64
		var requestStatus string
		var due bool
		if err := tx.QueryRow(ctx, `SELECT version, status, expires_at IS NOT NULL AND expires_at <= clock_timestamp()
FROM task_pending_requests WHERE id=$1 AND task_id=$2 FOR UPDATE`, pendingID, taskID).
			Scan(&storedVersion, &requestStatus, &due); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("lock pending approval for expiry: %w", err)
		}
		if storedVersion != version || requestStatus != string(task.PendingRequestPending) || !due {
			return nil
		}
		if leaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		if err := expirePendingRequestTx(ctx, tx, taskID, pendingID, version, leaseVersion); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

// ExpireNextPendingRequest advances one due approval without a client request.
// Lock the Task before its request, matching response and explicit expiry; the
// row lock also prevents multiple service instances from applying it twice.
func (s *Store) ExpireNextPendingRequest(ctx context.Context) (bool, error) {
	expired := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskID, pendingID, workspaceID, initiatorID string
		var version, leaseVersion int64
		err := tx.QueryRow(ctx, `SELECT t.id,p.id,p.version,t.lease_version,COALESCE(t.workspace_id,''),t.initiator_id
FROM task_pending_requests p JOIN tasks t ON t.id=p.task_id
WHERE p.status='pending' AND p.expires_at IS NOT NULL AND p.expires_at<=clock_timestamp()
 AND t.status='waiting_user' AND t.waiting_reason='approval_required'
 AND t.lease_owner='' AND t.lease_expires_at IS NULL AND t.lease_version<9223372036854775807
 AND (t.workspace_id IS NULL OR EXISTS (
   SELECT 1 FROM workspaces w WHERE w.id=t.workspace_id AND w.owner_id=t.initiator_id
    AND NOT EXISTS (SELECT 1 FROM workspace_ownership_transfers transfer WHERE transfer.workspace_id=w.id
      AND transfer.state IN ('pending','accepting','unknown'))
    AND NOT EXISTS (SELECT 1 FROM workspace_copy_reservations copy
      WHERE (copy.source_workspace_id=w.id OR copy.target_workspace_id=w.id)
        AND (copy.state IN ('reserved','copying','unknown') OR copy.host_copy_pending))
 ))
ORDER BY p.expires_at,p.task_id,p.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).
			Scan(&taskID, &pendingID, &version, &leaseVersion, &workspaceID, &initiatorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select expired pending approval: %w", err)
		}
		if err := lockTaskWorkspaceForExecution(ctx, tx, workspaceID, initiatorID); err != nil {
			return err
		}
		var due bool
		err = tx.QueryRow(ctx, `SELECT status='pending' AND expires_at IS NOT NULL AND expires_at<=clock_timestamp()
FROM task_pending_requests WHERE id=$1 AND task_id=$2 AND version=$3 FOR UPDATE`, pendingID, taskID, version).Scan(&due)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !due {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock expired pending approval: %w", err)
		}
		if err := expirePendingRequestTx(ctx, tx, taskID, pendingID, version, leaseVersion); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

func expirePendingRequestTx(ctx context.Context, tx pgx.Tx, taskID, pendingID string, version, leaseVersion int64) error {
	updated, err := tx.Exec(ctx, `UPDATE task_pending_requests SET status='expired', decision='reject', responded_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND version=$3 AND status='pending' AND expires_at IS NOT NULL AND expires_at <= clock_timestamp()`,
		pendingID, taskID, version)
	if err != nil {
		return fmt.Errorf("reject expired pending approval: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "pending request changed while expiring")
	}
	var sequence int64
	err = tx.QueryRow(ctx, `UPDATE tasks SET status='queued', lease_version=lease_version+1,
waiting_reason='', event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND status='waiting_user' AND waiting_reason='approval_required' AND lease_owner='' AND lease_expires_at IS NULL
RETURNING event_seq`, taskID).Scan(&sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "Task changed while expiring its approval")
	}
	if err != nil {
		return fmt.Errorf("requeue Task after approval expiry: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"pending_id": pendingID, "pending_version": version, "decision": string(task.PendingDecisionReject),
		"reason": "approval_expired", "idempotency_replayed": false,
		"status": string(task.StatusQueued), "lease_version": leaseVersion + 1,
	})
	if err != nil {
		return fmt.Errorf("encode expired approval event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.response", Payload: payload})
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
		var status, waitingReason, storedFileIntent, workspaceID, initiatorID string
		var leaseVersion, taskEpoch int64
		taskOwnerID, err := taskOwnerIDForActor(ctx, tx, taskID, record.OwnerID, true, true)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status, waiting_reason, lease_version, execution_epoch, file_delivery_intent,
COALESCE(workspace_id, ''), initiator_id FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).
			Scan(&status, &waitingReason, &leaseVersion, &taskEpoch, &storedFileIntent, &workspaceID, &initiatorID); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock task for clarification answer: %w", err)
		}
		if err := lockTaskWorkspaceForExecution(ctx, tx, workspaceID, initiatorID); err != nil {
			return err
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
RETURNING event_seq`, taskID, taskOwnerID, objective, string(fileIntent)).Scan(&sequence)
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
	var workspaceID string
	var checkpointRevision int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(workspace_id,''), COALESCE(workspace_checkpoint_revision,-1) FROM tasks WHERE id=$1`, taskID).
		Scan(&workspaceID, &checkpointRevision); err != nil {
		return fmt.Errorf("read task Workspace for media cancellation: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT status, COALESCE(media_preview_operation_id,'')
FROM task_pending_requests WHERE task_id=$1 AND status IN ('pending','approved') ORDER BY version FOR UPDATE`, taskID)
	if err != nil {
		return fmt.Errorf("lock pending media decisions for task cancellation: %w", err)
	}
	type mediaDecision struct{ status, operationID string }
	var decisions []mediaDecision
	for rows.Next() {
		var decision mediaDecision
		if err := rows.Scan(&decision.status, &decision.operationID); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending media decision for task cancellation: %w", err)
		}
		if decision.operationID != "" {
			if !domain.ValidOperationID(decision.operationID) {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "pending media preview identity is invalid")
			}
			decisions = append(decisions, decision)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read pending media decisions for task cancellation: %w", err)
	}
	rows.Close()
	for _, decision := range decisions {
		if decision.status == string(task.PendingRequestApproved) {
			var receiptState string
			var publishedRevision pgtype.Int8
			operationKey := ""
			stepID := "s" + decision.operationID[1:]
			if domain.ValidWorkspaceID(workspaceID) {
				operationKey = "task-workspace-publish:" + stepID + ":" + workspaceID
				err := tx.QueryRow(ctx, `SELECT state, published_revision FROM task_workspace_publish_receipts
WHERE task_id=$1 AND operation_key=$2 FOR UPDATE`, taskID, operationKey).Scan(&receiptState, &publishedRevision)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("read Task media publish receipt during cancellation: %w", err)
				}
			}
			if receiptState == string(app.TaskWorkspacePublishSucceeded) {
				if !publishedRevision.Valid || publishedRevision.Int64 != checkpointRevision {
					return domain.NewError(domain.CodeConflict, "published Task media checkpoint changed before cancellation")
				}
				var mediaType string
				var sizeBytes int64
				var sha256Bytes []byte
				if err := tx.QueryRow(ctx, `SELECT media_type, size_bytes, sha256 FROM task_media_staged_objects
WHERE task_id=$1 AND operation_id=$2`, taskID, decision.operationID).Scan(&mediaType, &sizeBytes, &sha256Bytes); err != nil {
					return fmt.Errorf("read published Task media metadata during cancellation: %w", err)
				}
				var artifactRecorded bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_artifacts
  WHERE task_id=$1 AND step_id=$2 AND workspace_id=$3 AND workspace_revision=$4
    AND size_bytes=$5 AND sha256=encode($6::bytea,'hex') AND media_type=$7
    AND path LIKE $8
)`, taskID, stepID, workspaceID, publishedRevision.Int64, sizeBytes, sha256Bytes, mediaType,
					"morde-media-"+decision.operationID+".%").Scan(&artifactRecorded); err != nil {
					return fmt.Errorf("check published Task media Artifact during cancellation: %w", err)
				}
				if !artifactRecorded {
					return domain.NewError(domain.CodeConflict, "Task media was published but its Artifact is still being recorded; retry cancellation")
				}
				if _, err := tx.Exec(ctx, `UPDATE task_media_staged_objects SET state='published', updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_id=$2 AND state='saving' AND cleanup_state='pending'`, taskID, decision.operationID); err != nil {
					return fmt.Errorf("settle published Task media during cancellation: %w", err)
				}
				continue
			}
			if operationKey != "" && receiptState == string(app.TaskWorkspacePublishPending) {
				if _, err := tx.Exec(ctx, `UPDATE task_workspace_publish_receipts SET state='conflict', updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_key=$2 AND state='pending'`, taskID, operationKey); err != nil {
					return fmt.Errorf("reject pending Task media publish during cancellation: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE task_media_staged_objects SET state='discarded', updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_id=$2 AND state='saving' AND cleanup_state='pending'`, taskID, decision.operationID); err != nil {
				return fmt.Errorf("discard approved Task media during cancellation: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE task_media_staged_objects SET state='discarded', updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_id=$2 AND state='preview' AND cleanup_state='pending'`, taskID, decision.operationID); err != nil {
			return fmt.Errorf("discard pending media preview during task cancellation: %w", err)
		}
	}
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

// ExpireTaskMediaPreviewApproval rejects an approval whose preview bytes have
// expired, then requeues the exact waiting Task so its broker can settle the
// declined tool call without replaying generation.
func (s *Store) ExpireTaskMediaPreviewApproval(ctx context.Context, taskID, operationID string) error {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return domain.InvalidArgument("task_media_preview", "expired Task media identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskStatus, waitingReason string
		var leaseVersion int64
		if err := tx.QueryRow(ctx, `SELECT status, waiting_reason, lease_version FROM tasks WHERE id=$1 FOR UPDATE`, taskID).
			Scan(&taskStatus, &waitingReason, &leaseVersion); errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		} else if err != nil {
			return fmt.Errorf("lock Task for expired media approval: %w", err)
		}
		var pendingID string
		var pendingVersion int64
		var pendingStatus string
		if err := tx.QueryRow(ctx, `SELECT id, version, status FROM task_pending_requests
WHERE task_id=$1 AND media_preview_operation_id=$2 FOR UPDATE`, taskID, operationID).
			Scan(&pendingID, &pendingVersion, &pendingStatus); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("lock approval for expired Task media: %w", err)
		}
		if pendingStatus != string(task.PendingRequestPending) {
			return nil
		}
		if taskStatus != string(task.StatusWaitingUser) || waitingReason != "approval_required" || leaseVersion == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeConflict, "expired media approval is not attached to a waiting Task")
		}
		var mediaState string
		if err := tx.QueryRow(ctx, `SELECT state FROM task_media_staged_objects
WHERE task_id=$1 AND operation_id=$2 FOR UPDATE`, taskID, operationID).Scan(&mediaState); err != nil {
			return fmt.Errorf("lock expired Task media preview: %w", err)
		}
		if mediaState != string(app.TaskMediaPreviewExpired) {
			return domain.NewError(domain.CodeConflict, "Task media preview has not expired")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_pending_requests
SET status='rejected', decision='reject', responded_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND version=$3 AND status='pending'`, pendingID, taskID, pendingVersion); err != nil {
			return fmt.Errorf("reject expired Task media approval: %w", err)
		}
		var sequence int64
		err := tx.QueryRow(ctx, `UPDATE tasks SET status='queued', lease_version=lease_version+1,
waiting_reason='', event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 AND status='waiting_user' AND lease_owner='' AND lease_expires_at IS NULL RETURNING event_seq`, taskID).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "Task changed while expiring its media approval")
		}
		if err != nil {
			return fmt.Errorf("requeue Task after media preview expiry: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"pending_id": pendingID, "pending_version": pendingVersion, "decision": string(task.PendingDecisionReject),
			"reason": "media_preview_expired", "idempotency_replayed": false,
			"status": string(task.StatusQueued), "lease_version": leaseVersion + 1,
		})
		if err != nil {
			return fmt.Errorf("encode expired media approval event: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.response", Payload: payload})
	})
}

func (s *Store) CanCleanupTaskMediaStagedObject(ctx context.Context, taskID, operationID string) (bool, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return false, domain.InvalidArgument("task_media_cleanup", "Task media cleanup identity is invalid")
	}
	stepID := "s" + operationID[1:]
	var taskStatus, stepStatus, pendingStatus string
	err := s.pool.QueryRow(ctx, `SELECT t.status, COALESCE(step.status,''), COALESCE(pending.status,'')
FROM tasks t
LEFT JOIN task_steps step ON step.task_id=t.id AND step.id=$3
LEFT JOIN task_pending_requests pending ON pending.task_id=t.id AND pending.media_preview_operation_id=$2
WHERE t.id=$1`, taskID, operationID, stepID).Scan(&taskStatus, &stepStatus, &pendingStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, missingTaskError()
	}
	if err != nil {
		return false, fmt.Errorf("check Task media cleanup readiness: %w", err)
	}
	return taskStatus == string(task.StatusCancelling) || taskStatus == string(task.StatusCancelled) ||
		taskStatus == string(task.StatusFailed) || taskStatus == string(task.StatusCompleted) ||
		stepStatus == string(task.StepFailed) || stepStatus == string(task.StepSucceeded) ||
		(stepStatus == "" && (pendingStatus == string(task.PendingRequestRejected) || pendingStatus == string(task.PendingRequestCancelled))), nil
}
