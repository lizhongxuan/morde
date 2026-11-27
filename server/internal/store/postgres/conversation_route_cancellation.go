package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
)

func (s *Store) CancelConversationRoute(ctx context.Context, actorID, projectID, conversationID, messageID,
	serviceInstanceID string) (conversation.Message, bool, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) ||
		!domain.ValidServiceInstanceID(serviceInstanceID) || (projectID != "" && !project.ValidID(projectID)) {
		return conversation.Message{}, false, domain.InvalidArgument("conversation_message", "conversation message identity is invalid")
	}
	var value conversation.Message
	var changed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, role, actualProjectID string
		if err := tx.QueryRow(ctx, `SELECT m.owner_id,m.role,COALESCE(c.project_id,'')
FROM conversation_messages m JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
WHERE m.id=$1 AND m.conversation_id=$2 AND m.actor_id=$3`, messageID, conversationID, actorID).
			Scan(&ownerID, &role, &actualProjectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation message not found")
			}
			return fmt.Errorf("read conversation message for cancellation: %w", err)
		}
		if role != string(conversation.RoleUser) || actualProjectID != projectID || (projectID == "" && ownerID != actorID) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		receipt := app.ConversationMessageReceipt{Message: conversation.Message{ID: messageID, OwnerID: ownerID,
			ActorID: actorID, ConversationID: conversationID, Role: conversation.RoleUser}, ServiceInstanceID: serviceInstanceID,
			Input: app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		var state, clientMessageID string
		var metadata []byte
		var cancelRequested bool
		if err := tx.QueryRow(ctx, `SELECT state,client_message_id,request_metadata,cancel_requested_at IS NOT NULL
FROM conversation_message_routes WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND message_id=$4 FOR UPDATE`,
			serviceInstanceID, actorID, conversationID, messageID).Scan(&state, &clientMessageID, &metadata, &cancelRequested); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation message not found")
			}
			return fmt.Errorf("lock conversation route for cancellation: %w", err)
		}
		var input app.ConversationRoutingInput
		if json.Unmarshal(metadata, &input) != nil || input.ResponseMode != app.ConversationResponseLongForm {
			return domain.NewError(domain.CodeConflict, "only explicit long-form replies can be cancelled")
		}
		switch state {
		case "pending":
			result, err := tx.Exec(ctx, `UPDATE conversation_message_routes
SET state='cancelled',aggregation_state='cancelled',lease_owner='',lease_expires_at=NULL,cancel_requested_at=clock_timestamp(),
failure_reason='',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND message_id=$4 AND state='pending'`,
				serviceInstanceID, actorID, conversationID, messageID)
			if err != nil || result.RowsAffected() != 1 {
				if err != nil {
					return fmt.Errorf("cancel pending long-form route: %w", err)
				}
				return domain.NewError(domain.CodeConflict, "conversation route changed before cancellation")
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied'
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user' AND task_id IS NULL`,
				messageID, ownerID, actorID, conversationID); err != nil {
				return fmt.Errorf("settle cancelled conversation receipt: %w", err)
			}
			changed = true
		case "processing":
			if !cancelRequested {
				result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET cancel_requested_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND message_id=$4 AND state='processing' AND cancel_requested_at IS NULL`,
					serviceInstanceID, actorID, conversationID, messageID)
				if err != nil || result.RowsAffected() != 1 {
					if err != nil {
						return fmt.Errorf("request long-form route cancellation: %w", err)
					}
					return domain.NewError(domain.CodeConflict, "conversation route changed before cancellation")
				}
				changed = true
			}
		case "cancelled":
			// An identical cancellation retry returns the current message.
		default:
			return domain.NewError(domain.CodeConflict, "long-form reply is no longer cancellable")
		}
		if changed {
			if err := syncConversationDuplicateMessages(ctx, tx, messageID); err != nil {
				return err
			}
			if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: ownerID,
				ObjectType: "message", ObjectID: messageID, Operation: "upsert", MutationID: clientMessageID}); err != nil {
				return err
			}
		}
		var err error
		value, _, err = loadConversationReceipt(ctx, tx, actorID, conversationID, clientMessageID)
		return err
	})
	if err != nil {
		return conversation.Message{}, false, err
	}
	return value, changed, nil
}

func (s *Store) ConversationRouteCancellationRequested(ctx context.Context, claim app.ConversationRouteClaim) (bool, error) {
	var state, leaseOwner string
	var leaseVersion int64
	var requested bool
	err := s.pool.QueryRow(ctx, `SELECT state,lease_owner,lease_version,cancel_requested_at IS NOT NULL
FROM conversation_message_routes WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND message_id=$5`,
		claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID, claim.Message.ID).
		Scan(&state, &leaseOwner, &leaseVersion, &requested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.NewError(domain.CodeConflict, "conversation route is no longer available")
	}
	if err != nil {
		return false, fmt.Errorf("check persisted conversation route cancellation: %w", err)
	}
	if state == "cancelled" {
		return true, nil
	}
	if state != "processing" || leaseOwner != claim.LeaseOwner || leaseVersion != claim.LeaseVersion {
		return false, domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
	}
	return requested, nil
}
