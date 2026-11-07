package postgres

import (
	"context"
	"errors"
	"fmt"

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
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state IN ('reserved','dispatched','unknown') FOR UPDATE`,
			claim.Message.ID, claim.Message.ActorID).Scan(&state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock predecessor route model call: %w", err)
		}
		if state == "dispatched" || state == "unknown" {
			return nil
		}
		if state == "reserved" {
			if err := releaseRouteReservationInTx(ctx, tx, claim.Message.ActorID, claim.Message.ID); err != nil {
				return err
			}
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state='pending',lease_owner='',lease_expires_at=NULL,
available_after=clock_timestamp(),failure_reason='',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='processing' AND lease_owner=$2 AND lease_version=$3 AND lease_expires_at>clock_timestamp()`,
			claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion)
		if err != nil {
			return fmt.Errorf("requeue arrived predecessor route: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
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
