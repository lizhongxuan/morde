package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
)

func loadProjectMessageAttachmentIDsInTx(ctx context.Context, tx pgx.Tx, instanceID, projectID, ownerID, conversationID, messageID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT a.attachment_id FROM project_conversation_attachments a
JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id AND r.actor_id=a.actor_id
WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.conversation_id=$4 AND a.message_id=$5
 AND a.state='committed' ORDER BY a.attachment_id`, instanceID, projectID, ownerID, conversationID, messageID)
	if err != nil {
		return nil, fmt.Errorf("read project message attachments: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project message attachment: %w", err)
		}
		if !app.ValidProjectConversationAttachmentID(id) || len(ids) >= conversation.MaxHistoryImportAttachments {
			return nil, domain.NewError(domain.CodeInvalidState, "project message attachments are invalid")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project message attachments: %w", err)
	}
	return ids, nil
}

func (s *Store) attachProjectConversationMessageAttachmentIDs(ctx context.Context, actorID, projectID, conversationID string, items []conversation.Message) error {
	if len(items) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		value, err := loadProjectConversationForActorInTx(ctx, tx, actorID, projectID, conversationID, false)
		if err != nil {
			return err
		}
		var instanceID string
		if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true FOR SHARE`).Scan(&instanceID); err != nil {
			return fmt.Errorf("read project attachment service instance: %w", err)
		}
		ids := make([]string, 0, len(items))
		byID := make(map[string]int, len(items))
		for index, message := range items {
			if message.OwnerID != value.OwnerID || message.ConversationID != conversationID {
				return domain.NewError(domain.CodeInvalidState, "project message identity changed")
			}
			ids = append(ids, message.ID)
			byID[message.ID] = index
		}
		rows, err := tx.Query(ctx, `SELECT a.message_id,a.attachment_id FROM project_conversation_attachments a
JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id AND r.actor_id=a.actor_id
WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.conversation_id=$4
 AND a.message_id=ANY($5::text[]) AND a.state='committed' ORDER BY a.message_id,a.attachment_id`,
			instanceID, projectID, value.OwnerID, conversationID, ids)
		if err != nil {
			return fmt.Errorf("list project message attachments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var messageID, id string
			if err := rows.Scan(&messageID, &id); err != nil {
				return fmt.Errorf("scan project message attachment: %w", err)
			}
			index, found := byID[messageID]
			if !found || !app.ValidProjectConversationAttachmentID(id) || len(items[index].AttachmentIDs) >= conversation.MaxHistoryImportAttachments {
				return domain.NewError(domain.CodeInvalidState, "project message attachments are invalid")
			}
			items[index].AttachmentIDs = append(items[index].AttachmentIDs, id)
		}
		return rows.Err()
	})
}
