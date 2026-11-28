package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

// RequeueConversationRouteAfterPredecessor serializes with model dispatch.
// The reservation can be discarded only before dispatch; unknown or in-flight
// calls remain frozen for explicit reconciliation.
func (s *Store) RequeueConversationRouteAfterPredecessor(ctx context.Context, claim app.ConversationRouteClaim,
	snapshot app.ConversationRouteSnapshot) (bool, error) {
	if ctx == nil || !snapshot.PreviousMissing || !snapshot.Context.PreviousMissing || !snapshot.Context.PreviousUnresolved ||
		snapshot.Context.ActorID != claim.Message.ActorID || snapshot.Context.Content != claim.Message.Content ||
		claim.Input.PreviousClientMessageID == "" || claim.Input.PreviousClientMessageID == claim.Message.ClientMessageID {
		return false, domain.InvalidArgument("route_predecessor", "conversation predecessor recovery identity is invalid")
	}
	var requeued bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, claim.Message.ActorID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var arrived bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id AND m.role='user'
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.client_message_id=$5 AND r.message_id<>$6)`, claim.ServiceInstanceID, claim.Message.OwnerID,
			claim.Message.ActorID, claim.Message.ConversationID, claim.Input.PreviousClientMessageID, claim.Message.ID).Scan(&arrived); err != nil {
			return fmt.Errorf("verify arrived conversation predecessor: %w", err)
		}
		if !arrived {
			return nil
		}
		safe, err := releaseConversationRouteModelCallForReclassification(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !safe {
			return nil
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state='pending',route_attempt=route_attempt+1,
lease_owner='',lease_expires_at=NULL,available_after=clock_timestamp(),failure_reason='',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8
 AND route_attempt<9223372036854775807 AND lease_expires_at>clock_timestamp()`,
			claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
			claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt)
		if err != nil {
			return fmt.Errorf("requeue arrived predecessor route: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	return requeued, err
}

// RequeueConversationRouteAfterContextChange retries only before a model call
// was dispatched, so a fresh authorization snapshot cannot duplicate a charge.
func (s *Store) RequeueConversationRouteAfterContextChange(ctx context.Context, claim app.ConversationRouteClaim) (bool, error) {
	if ctx == nil {
		return false, domain.InvalidArgument("route_context", "conversation route context is required")
	}
	var requeued bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, claim.Message.ActorID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		safe, err := releaseConversationRouteModelCallForReclassification(ctx, tx, claim)
		if err != nil || !safe {
			return err
		}
		if err := requeueConversationRouteForReclassification(ctx, tx, claim); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	return requeued, err
}

// HandleConversationRouteRateLimit releases only a model call explicitly
// rejected by the provider, then retries or fails the route in the same transaction.
func (s *Store) HandleConversationRouteRateLimit(ctx context.Context, claim app.ConversationRouteClaim, delay time.Duration) (bool, error) {
	if ctx == nil || claim.Input.ResultReviewID != "" || delay < 0 || delay > 24*time.Hour ||
		(delay > 0 && (delay < time.Second || delay%time.Second != 0)) {
		return false, domain.InvalidArgument("route_rate_limit", "conversation route rate-limit handling is invalid")
	}
	var handled bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, claim.Message.ActorID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var callID, projectID string
		err := tx.QueryRow(ctx, `SELECT call_id,COALESCE(project_id,'') FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND attempt=$3 AND phase IN ('normal','missing_predecessor') AND state='dispatched' FOR UPDATE`,
			claim.Message.ID, claim.Message.ActorID, claim.RouteAttempt).Scan(&callID, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find dispatched rate-limited route call: %w", err)
		}
		var ledgerState string
		var personal bool
		if err := tx.QueryRow(ctx, `SELECT state,personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='route' AND call_id=$2 FOR UPDATE`, claim.Message.ActorID, callID).Scan(&ledgerState, &personal); err != nil || ledgerState != "dispatched" {
			return domain.NewError(domain.CodeInvalidState, "rate-limited route account hold is unavailable")
		}
		if !personal {
			if _, _, err := lockPlatformModelBudget(ctx, tx, claim.Message.ActorID); err != nil {
				return err
			}
		}
		if projectID != "" && !personal {
			if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
				return err
			}
		}
		result, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='rejected',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='route' AND call_id=$2 AND state='dispatched'`, claim.Message.ActorID, callID)
		if err != nil {
			return fmt.Errorf("record provider-rejected route call: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "rate-limited route call changed before rejection was recorded")
		}
		if projectID != "" && !personal {
			result, err = tx.Exec(ctx, `UPDATE project_budget_ledger SET status='rejected',settled_at=clock_timestamp()
WHERE project_id=$1 AND source='route' AND call_id=$2 AND actor_id=$3 AND status='reserved'`, projectID, callID, claim.Message.ActorID)
			if err != nil || result.RowsAffected() != 1 {
				if err != nil {
					return fmt.Errorf("release provider-rejected project model hold: %w", err)
				}
				return domain.NewError(domain.CodeInvalidState, "rate-limited project model hold is unavailable")
			}
		}
		result, err = tx.Exec(ctx, `UPDATE conversation_route_model_calls SET state='rejected',updated_at=clock_timestamp()
WHERE message_id=$1 AND actor_id=$2 AND call_id=$3 AND attempt=$4 AND state='dispatched'`,
			claim.Message.ID, claim.Message.ActorID, callID, claim.RouteAttempt)
		if err != nil || result.RowsAffected() != 1 {
			if err != nil {
				return fmt.Errorf("record rejected conversation route call: %w", err)
			}
			return domain.NewError(domain.CodeConflict, "conversation route call changed before rejection was recorded")
		}
		var rejectedCalls int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state='rejected' AND phase IN ('normal','missing_predecessor')`,
			claim.Message.ID, claim.Message.ActorID).Scan(&rejectedCalls); err != nil {
			return fmt.Errorf("count rejected conversation route calls: %w", err)
		}
		if delay > 0 && rejectedCalls < 3 && claim.RouteAttempt < int64(^uint64(0)>>1) {
			result, err = tx.Exec(ctx, `UPDATE conversation_message_routes SET state='pending',route_attempt=route_attempt+1,
lease_owner='',lease_expires_at=NULL,available_after=clock_timestamp()+($9::bigint * interval '1 second'),
failure_reason='',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8
 AND route_attempt<9223372036854775807 AND lease_expires_at>clock_timestamp()`,
				claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
				claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt, int64(delay/time.Second))
		} else {
			// The durable ladder ends in one of two terminal states. Three
			// rejected calls means the retry budget is spent, which gets
			// the ladder-exhausted marker; an over-horizon provider wait
			// keeps the existing model_unavailable reason (M09-05). The
			// failure_reason column is free text, so no migration.
			failureReason := "model_unavailable"
			if rejectedCalls >= 3 {
				failureReason = app.RouteRetryLadderExhaustedReason
			}
			result, err = tx.Exec(ctx, `UPDATE conversation_message_routes SET state='failed',failure_reason=$9,
lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8 AND lease_expires_at>clock_timestamp()`,
				claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
				claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt, failureReason)
		}
		if err != nil {
			return fmt.Errorf("finish rate-limited conversation route: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
		handled = true
		return nil
	})
	return handled, err
}

// RequeueConversationRouteAfterInvalidOutput permits one separately metered
// repair only after the current model call settled a structurally invalid result.
func (s *Store) RequeueConversationRouteAfterInvalidOutput(ctx context.Context, claim app.ConversationRouteClaim) (bool, error) {
	if ctx == nil || claim.Input.ResultReviewID != "" {
		return false, domain.InvalidArgument("route_output", "invalid route output recovery identity is invalid")
	}
	var requeued bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, claim.Message.ActorID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var invalidCount, currentInvalidCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE decision->>'invalid_output'='true'),
count(*) FILTER (WHERE attempt=$3 AND decision->>'invalid_output'='true')
FROM conversation_route_model_calls WHERE message_id=$1 AND actor_id=$2 AND state='settled'
 AND phase IN ('normal','missing_predecessor')`, claim.Message.ID, claim.Message.ActorID, claim.RouteAttempt).
			Scan(&invalidCount, &currentInvalidCount); err != nil {
			return fmt.Errorf("count invalid conversation route outputs: %w", err)
		}
		if invalidCount != 1 || currentInvalidCount != 1 {
			return nil
		}
		if claim.RouteAttempt >= int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "conversation route attempt limit reached")
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state='pending',route_attempt=route_attempt+1,
lease_owner='',lease_expires_at=NULL,available_after=clock_timestamp(),failure_reason='',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8
 AND route_attempt<9223372036854775807 AND lease_expires_at>clock_timestamp()`,
			claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
			claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt)
		if err != nil {
			return fmt.Errorf("requeue invalid conversation route output: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	return requeued, err
}

// IsConversationRouteRepairAttempt binds the repair prompt to the immediately
// preceding durable invalid result rather than unrelated route requeues.
func (s *Store) IsConversationRouteRepairAttempt(ctx context.Context, claim app.ConversationRouteClaim) (bool, error) {
	if ctx == nil || claim.Input.ResultReviewID != "" {
		return false, domain.InvalidArgument("route_output", "invalid route repair identity is invalid")
	}
	if claim.RouteAttempt < 1 {
		return false, nil
	}
	var repair bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state='settled' AND attempt=$3
 AND phase IN ('normal','missing_predecessor') AND decision->>'invalid_output'='true')`,
			claim.Message.ID, claim.Message.ActorID, claim.RouteAttempt-1).Scan(&repair); err != nil {
			return fmt.Errorf("read conversation route repair state: %w", err)
		}
		return nil
	})
	return repair, err
}

func releaseConversationRouteModelCallForReclassification(ctx context.Context, tx pgx.Tx,
	claim app.ConversationRouteClaim) (bool, error) {
	attempt := claim.RouteAttempt
	if claim.Input.ResultReviewID != "" {
		attempt = 0
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND (state IN ('reserved','dispatched') OR (state='unknown' AND attempt=$3))
ORDER BY (state IN ('reserved','dispatched')) DESC,updated_at DESC LIMIT 1 FOR UPDATE`,
		claim.Message.ID, claim.Message.ActorID, attempt).Scan(&state)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock route model call for reclassification: %w", err)
	}
	if state == "dispatched" || state == "unknown" {
		return false, nil
	}
	if state == "reserved" {
		if err := releaseRouteReservationInTx(ctx, tx, claim.Message.ActorID, claim.Message.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}
