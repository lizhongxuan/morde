package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

// ClaimConversationRoute locks and leases one pending or expired routing row.
func (s *Store) ClaimConversationRoute(ctx context.Context, workerID string, duration time.Duration) (app.ConversationRouteClaim, bool, error) {
	if strings.TrimSpace(workerID) != workerID || workerID == "" || len(workerID) > 128 ||
		duration < time.Second || duration > 5*time.Minute || duration%time.Second != 0 {
		return app.ConversationRouteClaim{}, false, domain.InvalidArgument("route_lease", "routing lease is invalid")
	}
	var claim app.ConversationRouteClaim
	var found bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var metadata, authorization []byte
		var role, delivery, sourceKind string
		err := tx.QueryRow(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.actor_id,m.role,m.content,
COALESCE(m.task_id,''),m.model_id,m.sequence,m.delivery_status,m.created_at,
m.source_kind,r.service_instance_id::text,r.client_message_id,r.request_metadata,r.authorization_snapshot
FROM conversation_message_routes r
JOIN service_identity i ON i.instance_id=r.service_instance_id AND i.singleton=true
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
WHERE (r.state='pending' OR (r.state='processing' AND r.lease_expires_at<=clock_timestamp()))
  AND r.available_after<=clock_timestamp()
  AND NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements_text(COALESCE(r.request_metadata->'client_attachment_ids','[]'::jsonb)) AS expected(client_id)
    WHERE NOT (
      (COALESCE(r.request_metadata->>'project_id','')='' AND EXISTS (
        SELECT 1 FROM conversation_attachment_slots slot
        WHERE slot.message_id=r.message_id AND slot.client_attachment_id=expected.client_id
          AND slot.service_instance_id=r.service_instance_id AND slot.owner_id=r.owner_id
      )) OR
      (COALESCE(r.request_metadata->>'project_id','')<>'' AND EXISTS (
        SELECT 1 FROM project_conversation_attachments attachment
        WHERE attachment.message_id=r.message_id AND attachment.client_attachment_id=expected.client_id
          AND attachment.service_instance_id=r.service_instance_id AND attachment.project_id=r.request_metadata->>'project_id'
          AND attachment.owner_id=r.owner_id AND attachment.actor_id=r.actor_id
          AND attachment.conversation_id=r.conversation_id AND attachment.state='committed'
          AND attachment.derivative_state IN ('none','completed','skipped')
      ))
    )
  )
ORDER BY r.created_at,r.message_id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(
			&claim.Message.ID, &claim.Message.ConversationID, &claim.Message.OwnerID, &claim.Message.ActorID, &role,
			&claim.Message.Content, &claim.Message.TaskID, &claim.Message.ModelID, &claim.Message.Sequence,
			&delivery, &claim.Message.CreatedAt, &sourceKind, &claim.ServiceInstanceID, &claim.Message.ClientMessageID, &metadata, &authorization)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim conversation route: %w", err)
		}
		if err := json.Unmarshal(metadata, &claim.Input); err != nil {
			return fmt.Errorf("decode conversation routing input: %w", err)
		}
		if err := json.Unmarshal(authorization, &claim.Authorization); err != nil {
			return fmt.Errorf("decode conversation routing authorization: %w", err)
		}
		err = tx.QueryRow(ctx, `UPDATE conversation_message_routes
SET state='processing',lease_owner=$2,lease_version=lease_version+1,
    lease_expires_at=clock_timestamp()+($3::bigint * interval '1 second'),updated_at=clock_timestamp()
WHERE message_id=$1 RETURNING lease_version,lease_expires_at`, claim.Message.ID, workerID, int64(duration/time.Second)).
			Scan(&claim.LeaseVersion, &claim.LeaseExpiresAt)
		if err != nil {
			return fmt.Errorf("lease conversation route: %w", err)
		}
		claim.LeaseOwner = workerID
		claim.Message.Role = conversation.Role(role)
		claim.Message.SourceKind = sourceKind
		claim.Message.DeliveryStatus = conversation.DeliveryStatus(delivery)
		claim.Message.RoutingStatus = conversation.RoutingProcessing
		found = true
		return nil
	})
	return claim, found, err
}

// ReleaseConversationRoute cannot release a lease that another worker claimed.
func (s *Store) ReleaseConversationRoute(ctx context.Context, claim app.ConversationRouteClaim) error {
	if !domain.ValidMessageID(claim.Message.ID) || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) ||
		claim.LeaseOwner == "" || claim.LeaseVersion < 1 {
		return domain.InvalidArgument("route_lease", "routing lease is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE conversation_message_routes
SET state='pending',lease_owner='',lease_expires_at=NULL,available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND state='processing'
  AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at>clock_timestamp()
  AND owner_id=$5 AND actor_id=$6 AND conversation_id=$7
  AND EXISTS (SELECT 1 FROM service_identity i WHERE i.singleton=true AND i.instance_id=service_instance_id)`,
		claim.ServiceInstanceID, claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion,
		claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return fmt.Errorf("release conversation route: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "routing lease is no longer current")
	}
	return nil
}

// DeferConversationRoute releases only the current lease and gives other
// messages a chance to route while a predecessor is not yet available.
func (s *Store) DeferConversationRoute(ctx context.Context, claim app.ConversationRouteClaim, delay time.Duration) error {
	if !domain.ValidMessageID(claim.Message.ID) || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) ||
		claim.LeaseOwner == "" || claim.LeaseVersion < 1 || delay < time.Second || delay > 5*time.Minute || delay%time.Second != 0 {
		return domain.InvalidArgument("route_lease", "routing deferral is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE conversation_message_routes
SET state='pending',lease_owner='',lease_expires_at=NULL,
    available_after=clock_timestamp()+($5::bigint * interval '1 second'),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND state='processing'
  AND lease_owner=$3 AND lease_version=$4 AND lease_expires_at>clock_timestamp()
  AND owner_id=$6 AND actor_id=$7 AND conversation_id=$8
  AND EXISTS (SELECT 1 FROM service_identity i WHERE i.singleton=true AND i.instance_id=service_instance_id)`,
		claim.ServiceInstanceID, claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion, int64(delay/time.Second),
		claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return fmt.Errorf("defer conversation route: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "routing lease is no longer current")
	}
	return nil
}
