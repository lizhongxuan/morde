package postgres

import (
	"bytes"
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

// AcceptConversationMessage serializes receipt with the conversation and project
// membership locks, so message order and idempotency commit in one transaction.
func (s *Store) AcceptConversationMessage(ctx context.Context, receipt app.ConversationMessageReceipt) (conversation.Message, bool, error) {
	message := receipt.Message
	if !domain.ValidMessageID(message.ID) || !domain.ValidConversationID(message.ConversationID) ||
		!domain.ValidUserID(message.OwnerID) || !domain.ValidUserID(message.ActorID) ||
		message.Role != conversation.RoleUser || message.TaskID != "" || len(receipt.RequestHash) != 32 || !domain.ValidServiceInstanceID(receipt.ServiceInstanceID) ||
		receipt.ClientID == "" || receipt.Authorization.Kind == "" || receipt.Authorization.Version < 1 {
		return conversation.Message{}, false, domain.InvalidArgument("message", "conversation receipt fields are invalid")
	}
	metadata, err := json.Marshal(receipt.Input)
	if err != nil {
		return conversation.Message{}, false, fmt.Errorf("encode conversation routing input: %w", err)
	}
	authorization, err := json.Marshal(receipt.Authorization)
	if err != nil {
		return conversation.Message{}, false, fmt.Errorf("encode conversation authorization: %w", err)
	}
	var result conversation.Message
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.acceptConversationMessageInTx(ctx, tx, receipt, metadata, authorization, &result, &replayed)
	})
	return result, replayed, err
}

func (s *Store) acceptConversationMessageInTx(ctx context.Context, tx pgx.Tx, receipt app.ConversationMessageReceipt,
	metadata, authorization []byte, result *conversation.Message, replayed *bool) error {
	message := receipt.Message
	if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
		return err
	}
	value, requestHash, err := loadConversationReceipt(ctx, tx, message.ActorID, message.ConversationID, receipt.ClientID)
	if err == nil {
		if !bytes.Equal(requestHash, receipt.RequestHash) {
			return domain.NewError(domain.CodeIdempotencyConflict, "client message id was used for a different request")
		}
		*result, *replayed = value, true
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	message.ModelID = receipt.Input.ModelID
	message.SourceKind = "conversation"
	message.DeliveryStatus = conversation.DeliveryPending
	if err := insertMessage(ctx, tx, message); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO conversation_message_routes
(service_instance_id,owner_id,actor_id,conversation_id,message_id,client_message_id,
 previous_client_message_id,reference_task_id,request_hash,request_metadata,authorization_snapshot)
SELECT instance_id,$1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9::jsonb,$10::jsonb
FROM service_identity WHERE singleton=true`, message.OwnerID, message.ActorID, message.ConversationID,
		message.ID, receipt.ClientID, receipt.Input.PreviousClientMessageID, receipt.Input.ReferenceTaskID,
		receipt.RequestHash, metadata, authorization)
	if err != nil {
		return fmt.Errorf("insert conversation routing receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`,
		message.ConversationID, message.OwnerID); err != nil {
		return fmt.Errorf("touch conversation after receipt: %w", err)
	}
	if err := recordAccountObjectChanges(ctx, tx,
		accountObjectChange{OwnerID: message.OwnerID, ObjectType: "conversation", ObjectID: message.ConversationID, Operation: "upsert", MutationID: receipt.ClientID},
		accountObjectChange{OwnerID: message.OwnerID, ObjectType: "message", ObjectID: message.ID, Operation: "upsert", MutationID: receipt.ClientID},
	); err != nil {
		return err
	}
	*result, _, err = loadConversationReceipt(ctx, tx, message.ActorID, message.ConversationID, receipt.ClientID)
	return err
}

func lockConversationReceiptScopeInTx(ctx context.Context, tx pgx.Tx, receipt app.ConversationMessageReceipt) error {
	message := receipt.Message
	if err := requireConversationServiceInstance(ctx, tx, receipt.ServiceInstanceID); err != nil {
		return err
	}
	if receipt.Input.ProjectID != "" {
		if _, _, err := lockProjectForActorInTx(ctx, tx, message.ActorID, receipt.Input.ProjectID, project.RoleMember, true); err != nil {
			return err
		}
		value, err := loadProjectConversationForActorInTx(ctx, tx, message.ActorID, receipt.Input.ProjectID, message.ConversationID, true)
		if err != nil {
			return err
		}
		if value.OwnerID != message.OwnerID {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
	} else {
		if message.ActorID != message.OwnerID {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		var lockedID string
		err := tx.QueryRow(ctx, `SELECT id FROM conversations
WHERE id=$1 AND owner_id=$2 AND project_id IS NULL FOR UPDATE`, message.ConversationID, message.OwnerID).Scan(&lockedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		if err != nil {
			return fmt.Errorf("lock conversation for receipt: %w", err)
		}
	}
	return nil
}

func requireConversationServiceInstance(ctx context.Context, tx pgx.Tx, expected string) error {
	if !domain.ValidServiceInstanceID(expected) {
		return domain.InvalidArgument("service_instance_id", "service instance id is invalid")
	}
	var actual string
	if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true FOR SHARE`).Scan(&actual); err != nil {
		return fmt.Errorf("lock service instance identity: %w", err)
	}
	if actual != expected {
		return domain.NewError(domain.CodeConflict, "service instance has changed")
	}
	return nil
}

func loadConversationReceipt(ctx context.Context, tx pgx.Tx, actorID, conversationID, clientID string) (conversation.Message, []byte, error) {
	var value conversation.Message
	var role, delivery, routing, instanceID, routingFailure, projectID string
	var hash []byte
	err := tx.QueryRow(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.actor_id,m.role,m.content,
COALESCE(m.task_id,''),m.model_id,m.sequence,m.delivery_status,m.created_at,r.client_message_id,r.state,r.request_hash,r.service_instance_id::text,r.failure_reason,COALESCE(c.project_id,'')
FROM conversation_message_routes r
JOIN service_identity i ON i.instance_id=r.service_instance_id AND i.singleton=true
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
WHERE r.actor_id=$1 AND r.conversation_id=$2 AND r.client_message_id=$3`, actorID, conversationID, clientID).Scan(
		&value.ID, &value.ConversationID, &value.OwnerID, &value.ActorID, &role, &value.Content, &value.TaskID,
		&value.ModelID, &value.Sequence, &delivery, &value.CreatedAt, &value.ClientMessageID, &routing, &hash, &instanceID, &routingFailure, &projectID)
	if err != nil {
		return conversation.Message{}, nil, err
	}
	value.Role = conversation.Role(role)
	value.DeliveryStatus = conversation.DeliveryStatus(delivery)
	value.RoutingStatus = conversation.RoutingStatus(routing)
	value.RoutingFailureReason = conversation.RoutingFailureReason(routingFailure)
	if projectID != "" {
		value.AttachmentIDs, err = loadProjectMessageAttachmentIDsInTx(ctx, tx, instanceID, projectID, value.OwnerID, value.ConversationID, value.ID)
	} else {
		value.AttachmentIDs, err = loadHistoryMessageAttachmentIDs(ctx, tx, instanceID, value.OwnerID, value.ConversationID, value.ID)
	}
	if err != nil {
		return conversation.Message{}, nil, err
	}
	return value, hash, nil
}

// FindConversationMessageByClientID only reconciles the caller's own message
// and rechecks project membership in the same query as the lookup.
func (s *Store) FindConversationMessageByClientID(ctx context.Context, actorID, projectID, conversationID, clientID, instanceID string) (conversation.Message, bool, error) {
	var value conversation.Message
	var role, delivery, routing, routingFailure string
	var found bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
			return err
		}
		if projectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleMember, false); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.actor_id,m.role,m.content,
COALESCE(m.task_id,''),m.model_id,m.sequence,m.delivery_status,m.created_at,r.client_message_id,r.state,r.failure_reason
FROM conversation_message_routes r
JOIN service_identity i ON i.instance_id=r.service_instance_id AND i.singleton=true
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
JOIN users u ON u.id=$1 AND u.status='approved'
WHERE r.actor_id=$1 AND r.conversation_id=$3 AND r.client_message_id=$4 AND c.archived_at IS NULL
  AND (($2='' AND c.project_id IS NULL AND c.owner_id=$1) OR
       ($2<>'' AND c.project_id=$2 AND EXISTS (SELECT 1 FROM project_members pm WHERE pm.project_id=$2 AND pm.user_id=$1)))`,
			actorID, projectID, conversationID, clientID).Scan(&value.ID, &value.ConversationID, &value.OwnerID,
			&value.ActorID, &role, &value.Content, &value.TaskID, &value.ModelID, &value.Sequence, &delivery,
			&value.CreatedAt, &value.ClientMessageID, &routing, &routingFailure)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find conversation receipt: %w", err)
		}
		if projectID != "" {
			value.AttachmentIDs, err = loadProjectMessageAttachmentIDsInTx(ctx, tx, instanceID, projectID, value.OwnerID, value.ConversationID, value.ID)
		} else {
			value.AttachmentIDs, err = loadHistoryMessageAttachmentIDs(ctx, tx, instanceID, value.OwnerID, value.ConversationID, value.ID)
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return conversation.Message{}, false, err
	}
	if !found {
		return conversation.Message{}, false, nil
	}
	value.Role = conversation.Role(role)
	value.DeliveryStatus = conversation.DeliveryStatus(delivery)
	value.RoutingStatus = conversation.RoutingStatus(routing)
	value.RoutingFailureReason = conversation.RoutingFailureReason(routingFailure)
	return value, found, nil
}
