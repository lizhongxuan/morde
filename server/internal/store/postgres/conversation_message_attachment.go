package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *Store) AssociateConversationMessageAttachment(ctx context.Context, principal identity.Principal, conversationID, messageID, clientAttachmentID, attachmentID, instanceID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
			return err
		}
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		var declared bool
		var state string
		err := tx.QueryRow(ctx, `SELECT COALESCE(r.request_metadata->'client_attachment_ids','[]'::jsonb) ? $5, r.state
FROM conversation_message_routes r
JOIN conversations c ON c.id=r.conversation_id AND c.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.actor_id=$2 AND r.owner_id=$2 AND r.conversation_id=$3
  AND r.message_id=$4 AND c.project_id IS NULL AND c.archived_at IS NULL
FOR UPDATE OF r,c`, instanceID, principal.UserID, conversationID, messageID, clientAttachmentID).Scan(&declared, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		if err != nil {
			return fmt.Errorf("lock conversation attachment slot: %w", err)
		}
		if !declared {
			return domain.NewError(domain.CodeNotFound, "conversation attachment slot not found")
		}
		var current string
		err = tx.QueryRow(ctx, `SELECT attachment_id FROM conversation_attachment_slots WHERE message_id=$1 AND client_attachment_id=$2`, messageID, clientAttachmentID).Scan(&current)
		if err == nil {
			if current != attachmentID {
				return domain.NewError(domain.CodeIdempotencyConflict, "conversation attachment slot already has different content")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read conversation attachment slot: %w", err)
		}
		if state != "pending" {
			return domain.NewError(domain.CodeConflict, "message is already being processed")
		}
		uploads, err := lockAndValidateHistoryAttachments(ctx, tx, instanceID, principal.UserID, []string{attachmentID})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_message_attachments
(service_instance_id,owner_id,conversation_id,message_id,upload_id,attachment_id)
VALUES ($1::uuid,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, instanceID, principal.UserID, conversationID, messageID, uploads[attachmentID], attachmentID)
		if err != nil {
			return fmt.Errorf("associate conversation attachment: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_attachment_slots
(message_id,client_attachment_id,service_instance_id,owner_id,attachment_id) VALUES ($1,$2,$3::uuid,$4,$5)`,
			messageID, clientAttachmentID, instanceID, principal.UserID, attachmentID)
		if err != nil {
			return fmt.Errorf("record conversation attachment slot: %w", err)
		}
		return nil
	})
}
