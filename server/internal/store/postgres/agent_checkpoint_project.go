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
)

// The calling transaction has locked the active Task and its project member.
// Each declared slot must belong to this Task's actual initiator and be fully
// committed before it can enter a pending message or a root checkpoint.
func loadProjectCheckpointAttachmentIDs(ctx context.Context, tx pgx.Tx, instanceID, projectID, ownerID, actorID, conversationID, messageID string) ([]string, error) {
	var personalLinks bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_message_attachments WHERE message_id=$1)`, messageID).Scan(&personalLinks); err != nil {
		return nil, fmt.Errorf("check project message attachment scope: %w", err)
	}
	if personalLinks {
		return nil, domain.NewError(domain.CodeInvalidState, "project message has an attachment outside its project")
	}
	var metadata []byte
	err := tx.QueryRow(ctx, `SELECT request_metadata FROM conversation_message_routes
WHERE message_id=$1 AND service_instance_id=$2::uuid AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5`,
		messageID, instanceID, ownerID, actorID, conversationID).Scan(&metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		var hasForeignScope bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_message_routes WHERE message_id=$1)
OR EXISTS (SELECT 1 FROM project_conversation_attachments WHERE message_id=$1)`, messageID).Scan(&hasForeignScope); err != nil {
			return nil, fmt.Errorf("check unbound project attachments: %w", err)
		}
		if hasForeignScope {
			return nil, domain.NewError(domain.CodeInvalidState, "project attachment message is not routed in this scope")
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read project message attachment declaration: %w", err)
	}
	var input app.ConversationRoutingInput
	if json.Unmarshal(metadata, &input) != nil || input.ProjectID != projectID || len(input.ClientAttachmentIDs) > conversation.MaxHistoryImportAttachments {
		return nil, domain.NewError(domain.CodeInvalidState, "project attachment declaration is invalid")
	}
	expected := make(map[string]bool, len(input.ClientAttachmentIDs))
	for _, clientID := range input.ClientAttachmentIDs {
		if clientID == "" || expected[clientID] {
			return nil, domain.NewError(domain.CodeInvalidState, "project attachment declaration is invalid")
		}
		expected[clientID] = true
	}
	rows, err := tx.Query(ctx, `SELECT service_instance_id::text,project_id,owner_id,actor_id,conversation_id,
client_attachment_id,attachment_id,state FROM project_conversation_attachments
WHERE message_id=$1 ORDER BY attachment_id FOR SHARE`, messageID)
	if err != nil {
		return nil, fmt.Errorf("query project message attachments: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0, len(expected))
	for rows.Next() {
		var instance, project, owner, actor, conversationIDRow, clientID, attachmentID, state string
		if err := rows.Scan(&instance, &project, &owner, &actor, &conversationIDRow, &clientID, &attachmentID, &state); err != nil {
			return nil, fmt.Errorf("scan project message attachment: %w", err)
		}
		if instance != instanceID || project != projectID || owner != ownerID || actor != actorID || conversationIDRow != conversationID ||
			!expected[clientID] || state != "committed" || !app.ValidProjectConversationAttachmentID(attachmentID) ||
			len(ids) >= conversation.MaxHistoryImportAttachments || len(ids) > 0 && ids[len(ids)-1] >= attachmentID {
			return nil, domain.NewError(domain.CodeInvalidState, "project message attachment is not committed in the authorized scope")
		}
		delete(expected, clientID)
		ids = append(ids, attachmentID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project message attachments: %w", err)
	}
	if len(expected) != 0 {
		return nil, domain.NewError(domain.CodeInvalidState, "project message attachments are not fully committed")
	}
	return ids, nil
}
