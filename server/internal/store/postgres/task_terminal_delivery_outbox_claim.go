package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

const (
	TaskTerminalDeliverySourceFailure      = "terminal_failure"
	TaskTerminalDeliverySourceCancellation = "terminal_cancellation"
	TaskTerminalDeliverySourceWaiting      = "waiting_unknown"
	TaskTerminalDeliverySourceValidation   = "late_validation"

	TaskTerminalDeliveryStatePending     = "pending"
	TaskTerminalDeliveryStateClaimed     = "claimed"
	TaskTerminalDeliveryStateReplyReady  = "reply_ready"
	TaskTerminalDeliveryStateUnknown     = "unknown"
	TaskTerminalDeliveryStatePublished   = "published"
	TaskTerminalDeliveryStateInvalidated = "invalidated"

	// Model replies are bounded before encryption. Keep stored ciphertext
	// bounded as well, including the AEAD overhead.
	MaxTaskTerminalDeliveryResponseCiphertext = 1 << 20
)

// TaskTerminalDeliveryClaim is the durable source identity and its current
// claim fence. Reply-ready rows have no lease by the 00240 schema; publication
// must use MarkTaskTerminalDeliveryPublishedInTx inside the message transaction.
type TaskTerminalDeliveryClaim struct {
	ID                        int64
	TaskID                    string
	OwnerID                   string
	ConversationID            string
	InitiatorID               string
	TaskRevision              int64
	AuthorizationVersion      int64
	SourceKind                string
	SourceEventSequence       int64
	SourceEventType           string
	SourceSHA256              []byte
	ResultSHA256              []byte
	ValidationClaimGeneration *int64
	VerdictSHA256             []byte
	State                     string
	ClaimGeneration           int64
	LeaseOwner                string
	LeaseExpiresAt            time.Time
	ModelCallID               string
	ResponseKind              string
	ResponseCiphertext        []byte
	ResponseSHA256            []byte
	PublishedMessageID        string
}

const taskTerminalDeliverySourceCurrentPredicate = `EXISTS (
  SELECT 1
  FROM tasks task
  JOIN task_events event
    ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
  WHERE task.id=delivery.task_id
    AND task.owner_id=delivery.owner_id
    AND task.conversation_id=delivery.conversation_id
    AND task.initiator_id=delivery.initiator_id
    AND task.execution_location='server'
    AND task.current_revision=delivery.task_revision
    AND task.desired_revision=delivery.task_revision
    AND task.event_seq=delivery.source_event_sequence
    AND task.auth_version=delivery.authorization_version
    AND (event.task_revision IS NULL OR event.task_revision=delivery.task_revision)
    AND event.event_type=delivery.source_event_type
    AND sha256(convert_to(event.payload::text,'UTF8'))=delivery.source_sha256
    AND (
      (delivery.source_kind='terminal_failure' AND task.status='failed' AND event.event_type='task.result')
      OR (delivery.source_kind='terminal_cancellation' AND task.status='cancelled' AND event.event_type='task.status')
      OR (delivery.source_kind='waiting_unknown' AND task.status='waiting_user' AND event.event_type='task.waiting_user'
        AND NOT EXISTS (SELECT 1 FROM task_pending_requests pending
          WHERE pending.task_id=task.id AND pending.status='pending')
        AND NOT EXISTS (SELECT 1 FROM task_clarification_requests clarification
          WHERE clarification.task_id=task.id AND clarification.status='pending'))
      OR (delivery.source_kind='late_validation' AND task.status='completed' AND event.event_type='task.result'
        AND delivery.result_sha256 IS NOT NULL
        AND event.result_content_sha256=delivery.result_sha256
        AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=delivery.result_sha256
        AND delivery.validation_claim_generation IS NOT NULL
        AND delivery.verdict_sha256 IS NOT NULL
        AND EXISTS (SELECT 1 FROM task_validation_reviews review
          WHERE review.task_id=delivery.task_id AND review.owner_id=delivery.owner_id
            AND review.task_revision=delivery.task_revision
            AND review.result_event_sequence=delivery.source_event_sequence
            AND review.result_sha256=delivery.result_sha256
            AND review.claim_generation=delivery.validation_claim_generation
            AND review.state='reviewed'
            AND review.validation_state IN ('pending','PASS','PASS+NOTES','FAIL')))
    )
)`

const taskTerminalDeliverySelectColumns = `delivery.id,delivery.task_id,delivery.owner_id,delivery.conversation_id,delivery.initiator_id,
delivery.task_revision,delivery.authorization_version,delivery.source_kind,delivery.source_event_sequence,delivery.source_event_type,
delivery.source_sha256,delivery.result_sha256,delivery.validation_claim_generation,delivery.verdict_sha256,delivery.state,
delivery.claim_generation,COALESCE(delivery.lease_owner,''),delivery.lease_expires_at,COALESCE(delivery.model_call_id,''),
COALESCE(delivery.response_kind,''),delivery.response_ciphertext,delivery.response_sha256,COALESCE(delivery.published_message_id,'')`

// ClaimTaskTerminalDeliveryOutbox leases current source rows. Reclaiming an
// expired or unknown row increments its generation but preserves its stable
// model call ID, so an ambiguous provider call is reconciled under the same ID.
func (s *Store) ClaimTaskTerminalDeliveryOutbox(ctx context.Context, workerID string,
	lease time.Duration, limit int32) ([]TaskTerminalDeliveryClaim, error) {
	if ctx == nil || strings.TrimSpace(workerID) != workerID || workerID == "" || len(workerID) > 128 ||
		lease < time.Second || lease > 5*time.Minute || lease%time.Second != 0 || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("task_terminal_delivery_claim", "terminal delivery claim is invalid")
	}
	claims := make([]TaskTerminalDeliveryClaim, 0, limit)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := invalidateStaleTaskTerminalDeliveriesInTx(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH claimable AS (
  SELECT delivery.id
  FROM task_terminal_delivery_outbox delivery
  JOIN tasks task ON task.id=delivery.task_id AND task.owner_id=delivery.owner_id
  JOIN task_events event ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
  WHERE (delivery.state IN ('pending','unknown')
      OR (delivery.state='claimed' AND delivery.lease_expires_at<=clock_timestamp()))
    AND delivery.claim_generation<9223372036854775807
    AND delivery.response_ciphertext IS NULL AND delivery.response_sha256 IS NULL
    AND `+taskTerminalDeliverySourceCurrentPredicate+`
  ORDER BY delivery.created_at,delivery.id
  FOR UPDATE OF delivery,task,event SKIP LOCKED LIMIT $3
)
UPDATE task_terminal_delivery_outbox delivery
SET state='claimed',claim_generation=delivery.claim_generation+1,lease_owner=$1,
    lease_expires_at=clock_timestamp()+($2::bigint*interval '1 second'),
    model_call_id=COALESCE(delivery.model_call_id,'terminal_delivery_'||delivery.id::text),
    updated_at=clock_timestamp()
FROM claimable
WHERE delivery.id=claimable.id
RETURNING `+taskTerminalDeliverySelectColumns,
			workerID, int64(lease/time.Second), limit)
		if err != nil {
			return fmt.Errorf("claim Task terminal delivery outbox: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			claim, _, err := scanTaskTerminalDeliveryClaim(rows, false)
			if err != nil {
				return fmt.Errorf("read claimed Task terminal delivery: %w", err)
			}
			if !validTaskTerminalDeliveryClaim(claim) || claim.State != TaskTerminalDeliveryStateClaimed {
				return domain.NewError(domain.CodeInvalidState, "stored Task terminal delivery claim is invalid")
			}
			claims = append(claims, claim)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate claimed Task terminal deliveries: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// ListTaskTerminalDeliveryRepliesReady returns durable candidates for crash
// recovery. The migration does not permit a lease in reply_ready; publication
// must lock the row and transition it in the same transaction as message insert.
func (s *Store) ListTaskTerminalDeliveryRepliesReady(ctx context.Context, limit int32) ([]TaskTerminalDeliveryClaim, error) {
	if ctx == nil || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("task_terminal_delivery_claim", "reply-ready query is invalid")
	}
	claims := make([]TaskTerminalDeliveryClaim, 0, limit)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := invalidateStaleTaskTerminalDeliveriesInTx(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+taskTerminalDeliverySelectColumns+`
FROM task_terminal_delivery_outbox delivery
JOIN tasks task ON task.id=delivery.task_id AND task.owner_id=delivery.owner_id
JOIN task_events event ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
WHERE delivery.state='reply_ready' AND delivery.response_ciphertext IS NOT NULL
  AND delivery.response_sha256 IS NOT NULL AND `+taskTerminalDeliverySourceCurrentPredicate+`
ORDER BY delivery.created_at,delivery.id
FOR UPDATE OF delivery,task,event SKIP LOCKED LIMIT $1`, limit)
		if err != nil {
			return fmt.Errorf("list reply-ready Task terminal deliveries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			claim, _, err := scanTaskTerminalDeliveryClaim(rows, false)
			if err != nil {
				return fmt.Errorf("read reply-ready Task terminal delivery: %w", err)
			}
			if !validTaskTerminalDeliveryClaim(claim) || claim.State != TaskTerminalDeliveryStateReplyReady {
				return domain.NewError(domain.CodeInvalidState, "stored reply-ready Task terminal delivery is invalid")
			}
			claims = append(claims, claim)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate reply-ready Task terminal deliveries: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Store) MarkTaskTerminalDeliveryUnknown(ctx context.Context, claim TaskTerminalDeliveryClaim) (bool, error) {
	if ctx == nil || !validTaskTerminalDeliveryClaim(claim) || claim.State != TaskTerminalDeliveryStateClaimed {
		return false, domain.InvalidArgument("task_terminal_delivery_claim", "terminal delivery claim is invalid")
	}
	updated := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockCurrentTaskTerminalDeliveryClaimInTx(ctx, tx, claim)
		if err != nil || !current {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='unknown',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND state='claimed' AND claim_generation=$2 AND lease_owner=$3
  AND lease_expires_at>clock_timestamp() AND model_call_id=$4
  AND response_ciphertext IS NULL AND response_sha256 IS NULL`,
			claim.ID, claim.ClaimGeneration, claim.LeaseOwner, claim.ModelCallID)
		if err != nil {
			return fmt.Errorf("mark Task terminal delivery unknown: %w", err)
		}
		updated = result.RowsAffected() == 1
		return nil
	})
	return updated, err
}

// MarkTaskTerminalDeliveryReplyReady persists an encrypted response candidate
// before publication and releases the processing lease. The SHA is over the
// exact ciphertext bytes so recovery can reject a changed candidate.
func (s *Store) MarkTaskTerminalDeliveryReplyReady(ctx context.Context, claim TaskTerminalDeliveryClaim,
	ciphertext []byte) (TaskTerminalDeliveryClaim, bool, error) {
	if ctx == nil || !validTaskTerminalDeliveryClaim(claim) || claim.State != TaskTerminalDeliveryStateClaimed ||
		len(ciphertext) == 0 || len(ciphertext) > MaxTaskTerminalDeliveryResponseCiphertext {
		return TaskTerminalDeliveryClaim{}, false, domain.InvalidArgument("task_terminal_delivery_claim", "terminal delivery response is invalid")
	}
	digest := sha256.Sum256(ciphertext)
	updatedClaim := TaskTerminalDeliveryClaim{}
	updated := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockCurrentTaskTerminalDeliveryClaimInTx(ctx, tx, claim)
		if err != nil || !current {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='reply_ready',lease_owner=NULL,lease_expires_at=NULL,
    response_kind='system_state',response_ciphertext=$4,response_sha256=$5,updated_at=clock_timestamp()
WHERE id=$1 AND state='claimed' AND claim_generation=$2 AND lease_owner=$3
  AND lease_expires_at>clock_timestamp() AND model_call_id=$6
  AND response_ciphertext IS NULL AND response_sha256 IS NULL`,
			claim.ID, claim.ClaimGeneration, claim.LeaseOwner, ciphertext, digest[:], claim.ModelCallID)
		if err != nil {
			return fmt.Errorf("persist reply-ready Task terminal delivery: %w", err)
		}
		updated = result.RowsAffected() == 1
		if updated {
			updatedClaim = claim
			updatedClaim.State = TaskTerminalDeliveryStateReplyReady
			updatedClaim.LeaseOwner = ""
			updatedClaim.LeaseExpiresAt = time.Time{}
			updatedClaim.ResponseKind = app.TaskTerminalDeliveryResponseSystem
			updatedClaim.ResponseCiphertext = append([]byte(nil), ciphertext...)
			updatedClaim.ResponseSHA256 = append([]byte(nil), digest[:]...)
		}
		return nil
	})
	return updatedClaim, updated, err
}

func (s *Store) InvalidateTaskTerminalDelivery(ctx context.Context, claim TaskTerminalDeliveryClaim) (bool, error) {
	if ctx == nil || !validTaskTerminalDeliveryClaim(claim) || claim.State != TaskTerminalDeliveryStateClaimed {
		return false, domain.InvalidArgument("task_terminal_delivery_claim", "terminal delivery claim is invalid")
	}
	updated := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockCurrentTaskTerminalDeliveryClaimInTx(ctx, tx, claim)
		if err != nil || !current {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='invalidated',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND state='claimed' AND claim_generation=$2 AND lease_owner=$3
  AND lease_expires_at>clock_timestamp()`,
			claim.ID, claim.ClaimGeneration, claim.LeaseOwner)
		if err != nil {
			return fmt.Errorf("invalidate Task terminal delivery: %w", err)
		}
		updated = result.RowsAffected() == 1
		return nil
	})
	return updated, err
}

// MarkTaskTerminalDeliveryPublishedInTx completes a reply-ready source in the
// same transaction that inserts its conversation message. It is generation-
// and response-fenced; reply_ready itself has no lease in migration 00240.
func (s *Store) MarkTaskTerminalDeliveryPublishedInTx(ctx context.Context, tx pgx.Tx,
	ready TaskTerminalDeliveryClaim, messageID string) (bool, error) {
	if ctx == nil || tx == nil || !validTaskTerminalDeliveryClaim(ready) ||
		ready.State != TaskTerminalDeliveryStateReplyReady || !domain.ValidMessageID(messageID) {
		return false, domain.InvalidArgument("task_terminal_delivery_claim", "reply-ready Task terminal delivery is invalid")
	}
	stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, ready.ID)
	if err != nil {
		return false, err
	}
	if !found || !sameTaskTerminalDeliverySource(stored, ready) {
		return false, nil
	}
	if stored.State == TaskTerminalDeliveryStatePublished {
		if stored.PublishedMessageID != messageID ||
			stored.ResponseKind != ready.ResponseKind ||
			!bytes.Equal(stored.ResponseSHA256, ready.ResponseSHA256) ||
			!bytes.Equal(stored.ResponseCiphertext, ready.ResponseCiphertext) {
			return false, domain.NewError(domain.CodeIdempotencyConflict, "Task terminal delivery was published with another result")
		}
		return false, nil
	}
	if stored.State != TaskTerminalDeliveryStateReplyReady || leaseActive || stored.LeaseOwner != "" ||
		stored.ResponseKind != ready.ResponseKind ||
		!bytes.Equal(stored.ResponseSHA256, ready.ResponseSHA256) ||
		!bytes.Equal(stored.ResponseCiphertext, ready.ResponseCiphertext) {
		return false, nil
	}
	current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
	if err != nil {
		return false, err
	}
	if !current {
		if _, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='invalidated',updated_at=clock_timestamp()
WHERE id=$1 AND state='reply_ready' AND claim_generation=$2`, stored.ID, stored.ClaimGeneration); err != nil {
			return false, fmt.Errorf("invalidate stale reply-ready Task terminal delivery: %w", err)
		}
		return false, nil
	}
	result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='published',published_message_id=$4,updated_at=clock_timestamp()
WHERE id=$1 AND state='reply_ready' AND claim_generation=$2
  AND response_kind=$3 AND response_sha256=$5 AND response_ciphertext=$6 AND published_message_id IS NULL`,
		ready.ID, ready.ClaimGeneration, ready.ResponseKind, messageID, ready.ResponseSHA256, ready.ResponseCiphertext)
	if err != nil {
		return false, fmt.Errorf("mark Task terminal delivery published: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func lockCurrentTaskTerminalDeliveryClaimInTx(ctx context.Context, tx pgx.Tx,
	claim TaskTerminalDeliveryClaim) (bool, error) {
	stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, claim.ID)
	if err != nil || !found {
		return false, err
	}
	if !sameTaskTerminalDeliverySource(stored, claim) {
		return false, domain.NewError(domain.CodeConflict, "Task terminal delivery claim identity changed")
	}
	if stored.State != TaskTerminalDeliveryStateClaimed || !leaseActive ||
		stored.LeaseOwner != claim.LeaseOwner || stored.ClaimGeneration != claim.ClaimGeneration ||
		stored.ModelCallID != claim.ModelCallID || !stored.LeaseExpiresAt.Equal(claim.LeaseExpiresAt) {
		return false, nil
	}
	current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
	if err != nil || current {
		return current, err
	}
	if _, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='invalidated',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND state='claimed' AND claim_generation=$2 AND lease_owner=$3
  AND lease_expires_at>clock_timestamp()`,
		stored.ID, stored.ClaimGeneration, stored.LeaseOwner); err != nil {
		return false, fmt.Errorf("invalidate stale claimed Task terminal delivery: %w", err)
	}
	return false, nil
}

func loadTaskTerminalDeliveryClaimInTx(ctx context.Context, tx pgx.Tx, id int64) (
	TaskTerminalDeliveryClaim, bool, bool, error) {
	query := `SELECT ` + taskTerminalDeliverySelectColumns + `,COALESCE(delivery.lease_expires_at>clock_timestamp(),false)
FROM task_terminal_delivery_outbox delivery
WHERE delivery.id=$1
FOR UPDATE OF delivery`
	claim, leaseActive, err := scanTaskTerminalDeliveryClaim(tx.QueryRow(ctx, query, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskTerminalDeliveryClaim{}, false, false, nil
	}
	if err != nil {
		return TaskTerminalDeliveryClaim{}, false, false, fmt.Errorf("read Task terminal delivery claim: %w", err)
	}
	return claim, leaseActive, true, nil
}

func taskTerminalDeliverySourceCurrentInTx(ctx context.Context, tx pgx.Tx,
	delivery TaskTerminalDeliveryClaim) (bool, error) {
	var ownerID, conversationID, initiatorID, executionLocation, status, waitingReason, result string
	var currentRevision, desiredRevision pgtype.Int8
	var authorizationVersion, eventSequence int64
	err := tx.QueryRow(ctx, `SELECT owner_id,conversation_id,initiator_id,execution_location,status,waiting_reason,
       current_revision,desired_revision,auth_version,event_seq,result
FROM tasks WHERE id=$1 FOR UPDATE`, delivery.TaskID).Scan(
		&ownerID, &conversationID, &initiatorID, &executionLocation, &status, &waitingReason,
		&currentRevision, &desiredRevision, &authorizationVersion, &eventSequence, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock Task for terminal delivery fence: %w", err)
	}
	if ownerID != delivery.OwnerID || conversationID != delivery.ConversationID || initiatorID != delivery.InitiatorID ||
		executionLocation != "server" || !currentRevision.Valid || !desiredRevision.Valid ||
		currentRevision.Int64 != delivery.TaskRevision || desiredRevision.Int64 != delivery.TaskRevision ||
		authorizationVersion != delivery.AuthorizationVersion || eventSequence != delivery.SourceEventSequence {
		return false, nil
	}
	var eventType string
	var eventRevision pgtype.Int8
	var eventDigest, eventResultDigest []byte
	err = tx.QueryRow(ctx, `SELECT event_type,task_revision,sha256(convert_to(payload::text,'UTF8')),result_content_sha256
FROM task_events WHERE task_id=$1 AND sequence=$2 FOR SHARE`,
		delivery.TaskID, delivery.SourceEventSequence).Scan(&eventType, &eventRevision, &eventDigest, &eventResultDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock Task terminal delivery source event: %w", err)
	}
	if eventType != delivery.SourceEventType || (eventRevision.Valid && eventRevision.Int64 != delivery.TaskRevision) ||
		!bytes.Equal(eventDigest, delivery.SourceSHA256) {
		return false, nil
	}
	if delivery.SourceKind == TaskTerminalDeliverySourceValidation && !eventRevision.Valid {
		return false, nil
	}
	switch delivery.SourceKind {
	case TaskTerminalDeliverySourceFailure:
		return status == "failed" && eventType == "task.result", nil
	case TaskTerminalDeliverySourceCancellation:
		return status == "cancelled" && eventType == "task.status", nil
	case TaskTerminalDeliverySourceWaiting:
		if status != "waiting_user" || eventType != "task.waiting_user" {
			return false, nil
		}
		var hasCard bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_pending_requests WHERE task_id=$1 AND status='pending'
  UNION ALL SELECT 1 FROM task_clarification_requests WHERE task_id=$1 AND status='pending'
)`, delivery.TaskID).Scan(&hasCard); err != nil {
			return false, fmt.Errorf("check Task terminal delivery waiting cards: %w", err)
		}
		return !hasCard, nil
	case TaskTerminalDeliverySourceValidation:
		if status != "completed" || eventType != "task.result" || delivery.ValidationClaimGeneration == nil ||
			len(delivery.ResultSHA256) != sha256.Size || len(delivery.VerdictSHA256) != sha256.Size ||
			!bytes.Equal(eventResultDigest, delivery.ResultSHA256) {
			return false, nil
		}
		resultDigest := sha256.Sum256([]byte(result))
		if result == "" || !bytes.Equal(resultDigest[:], delivery.ResultSHA256) {
			return false, nil
		}
		var reviewState, validationState string
		var revision, resultSequence, claimGeneration int64
		var reviewResultDigest []byte
		err := tx.QueryRow(ctx, `SELECT state,validation_state,task_revision,result_event_sequence,
       claim_generation,result_sha256
FROM task_validation_reviews
WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3 FOR SHARE`,
			delivery.TaskID, delivery.OwnerID, delivery.TaskRevision).Scan(&reviewState, &validationState, &revision,
			&resultSequence, &claimGeneration, &reviewResultDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("lock Task validation source for terminal delivery: %w", err)
		}
		return reviewState == "reviewed" && revision == delivery.TaskRevision &&
			resultSequence == delivery.SourceEventSequence && claimGeneration == *delivery.ValidationClaimGeneration &&
			bytes.Equal(reviewResultDigest, delivery.ResultSHA256) &&
			(validationState == "pending" || validationState == "PASS" || validationState == "PASS+NOTES" || validationState == "FAIL"), nil
	default:
		return false, nil
	}
}

func scanTaskTerminalDeliveryClaim(scanner interface{ Scan(...any) error },
	includeLeaseActive bool) (TaskTerminalDeliveryClaim, bool, error) {
	var claim TaskTerminalDeliveryClaim
	var validationGeneration pgtype.Int8
	var leaseExpiresAt pgtype.Timestamptz
	arguments := []any{
		&claim.ID, &claim.TaskID, &claim.OwnerID, &claim.ConversationID, &claim.InitiatorID,
		&claim.TaskRevision, &claim.AuthorizationVersion, &claim.SourceKind, &claim.SourceEventSequence, &claim.SourceEventType,
		&claim.SourceSHA256, &claim.ResultSHA256, &validationGeneration, &claim.VerdictSHA256, &claim.State,
		&claim.ClaimGeneration, &claim.LeaseOwner, &leaseExpiresAt, &claim.ModelCallID, &claim.ResponseKind,
		&claim.ResponseCiphertext, &claim.ResponseSHA256, &claim.PublishedMessageID,
	}
	var leaseActive bool
	if includeLeaseActive {
		arguments = append(arguments, &leaseActive)
	}
	if err := scanner.Scan(arguments...); err != nil {
		return TaskTerminalDeliveryClaim{}, false, err
	}
	if validationGeneration.Valid {
		generation := validationGeneration.Int64
		claim.ValidationClaimGeneration = &generation
	}
	if leaseExpiresAt.Valid {
		claim.LeaseExpiresAt = leaseExpiresAt.Time
	}
	return claim, leaseActive, nil
}

func validTaskTerminalDeliveryClaim(claim TaskTerminalDeliveryClaim) bool {
	if claim.ID < 1 || !domain.ValidTaskID(claim.TaskID) || !domain.ValidUserID(claim.OwnerID) ||
		!domain.ValidConversationID(claim.ConversationID) || !domain.ValidUserID(claim.InitiatorID) ||
		claim.TaskRevision < 0 || claim.AuthorizationVersion < 1 || claim.SourceEventSequence < 1 ||
		!validTaskEventType(claim.SourceEventType) || len(claim.SourceSHA256) != sha256.Size ||
		claim.ClaimGeneration < 1 || !strings.HasPrefix(claim.ModelCallID, "terminal_delivery_") ||
		strings.TrimSpace(claim.ModelCallID) != claim.ModelCallID || len(claim.ModelCallID) > 128 {
		return false
	}
	switch claim.SourceKind {
	case TaskTerminalDeliverySourceFailure, TaskTerminalDeliverySourceCancellation,
		TaskTerminalDeliverySourceWaiting:
		if claim.ResultSHA256 != nil || claim.ValidationClaimGeneration != nil || claim.VerdictSHA256 != nil {
			return false
		}
	case TaskTerminalDeliverySourceValidation:
		if len(claim.ResultSHA256) != sha256.Size || claim.ValidationClaimGeneration == nil ||
			*claim.ValidationClaimGeneration < 1 || len(claim.VerdictSHA256) != sha256.Size {
			return false
		}
	default:
		return false
	}
	switch claim.State {
	case TaskTerminalDeliveryStateClaimed:
		return claim.LeaseOwner != "" && strings.TrimSpace(claim.LeaseOwner) == claim.LeaseOwner &&
			len(claim.LeaseOwner) <= 128 && !claim.LeaseExpiresAt.IsZero() &&
			claim.ResponseKind == "" && claim.ResponseCiphertext == nil && claim.ResponseSHA256 == nil && claim.PublishedMessageID == ""
	case TaskTerminalDeliveryStateReplyReady:
		if claim.LeaseOwner != "" || !claim.LeaseExpiresAt.IsZero() || claim.PublishedMessageID != "" ||
			!validPostgresTaskTerminalDeliveryResponseKind(claim.ResponseKind) ||
			len(claim.ResponseCiphertext) == 0 || len(claim.ResponseCiphertext) > MaxTaskTerminalDeliveryResponseCiphertext ||
			len(claim.ResponseSHA256) != sha256.Size {
			return false
		}
		digest := sha256.Sum256(claim.ResponseCiphertext)
		return bytes.Equal(digest[:], claim.ResponseSHA256)
	default:
		return false
	}
}

func validPostgresTaskTerminalDeliveryResponseKind(value string) bool {
	switch value {
	case app.TaskTerminalDeliveryResponseModel, app.TaskTerminalDeliveryResponseSystem, app.TaskTerminalDeliveryResponseLegacy:
		return true
	default:
		return false
	}
}

func sameTaskTerminalDeliverySource(stored, requested TaskTerminalDeliveryClaim) bool {
	return stored.ID == requested.ID && stored.TaskID == requested.TaskID &&
		stored.OwnerID == requested.OwnerID && stored.ConversationID == requested.ConversationID &&
		stored.InitiatorID == requested.InitiatorID && stored.TaskRevision == requested.TaskRevision &&
		stored.AuthorizationVersion == requested.AuthorizationVersion && stored.SourceKind == requested.SourceKind &&
		stored.SourceEventSequence == requested.SourceEventSequence && stored.SourceEventType == requested.SourceEventType &&
		bytes.Equal(stored.SourceSHA256, requested.SourceSHA256) && bytes.Equal(stored.ResultSHA256, requested.ResultSHA256) &&
		nullableInt64Equal(stored.ValidationClaimGeneration, requested.ValidationClaimGeneration) &&
		bytes.Equal(stored.VerdictSHA256, requested.VerdictSHA256) &&
		stored.ClaimGeneration == requested.ClaimGeneration && stored.ModelCallID == requested.ModelCallID
}

func nullableInt64Equal(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func invalidateStaleTaskTerminalDeliveriesInTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox delivery
SET state='invalidated',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE (delivery.state IN ('pending','reply_ready','unknown')
    OR (delivery.state='claimed' AND delivery.lease_expires_at<=clock_timestamp()))
  AND NOT `+taskTerminalDeliverySourceCurrentPredicate)
	if err != nil {
		return fmt.Errorf("invalidate stale Task terminal delivery sources: %w", err)
	}
	return nil
}
