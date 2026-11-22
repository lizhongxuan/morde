package postgres

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func (s *Store) ReadCurrentConversationMessageChunk(ctx context.Context, current task.Task, messageID string,
	runeOffset int64, maxRunes int) (app.CurrentConversationMessageChunk, error) {
	if ctx == nil || s == nil || s.pool == nil || !domain.ValidTaskID(current.ID) || !domain.ValidMessageID(messageID) ||
		!domain.ValidUserID(current.OwnerID) || !domain.ValidUserID(current.InitiatorID) || !domain.ValidConversationID(current.ConversationID) ||
		current.ExecutionLocation != "server" || current.Status != task.StatusRunning || current.Authorization.Version < 1 ||
		current.Lease.Owner == "" || current.Lease.Version < 1 || current.AgentSessionID == "" || current.ConversationContext == nil ||
		current.ConversationContext.OwnerID != current.OwnerID || current.ConversationContext.InitiatorID != current.InitiatorID ||
		current.ConversationContext.ConversationID != current.ConversationID || current.ConversationContext.ProjectID != current.ProjectID ||
		current.ConversationContext.ThroughSequence < 0 || runeOffset < 0 || runeOffset > app.MaxConversationMessageBytes ||
		maxRunes < 1 || maxRunes > app.MaxConversationMessageChunkRunes {
		return app.CurrentConversationMessageChunk{}, domain.InvalidArgument("message_read", "current conversation message chunk request is invalid")
	}
	var output app.CurrentConversationMessageChunk
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `SELECT m.id,m.task_id,m.sequence,m.role,
 substring(m.content FROM $8::integer FOR $9::integer),char_length(m.content),sha256(convert_to(m.content,'UTF8'))
FROM conversation_messages m
JOIN tasks current_task ON current_task.id=$6 AND current_task.owner_id=$2 AND current_task.initiator_id=$3
 AND current_task.conversation_id=$4 AND COALESCE(current_task.project_id,'')=$7
 AND current_task.execution_location='server' AND current_task.status='running'
 AND current_task.auth_version=$10 AND current_task.agent_generation=$11 AND current_task.agent_session_id=$12
 AND current_task.lease_owner=$13 AND current_task.lease_version=$14 AND current_task.lease_expires_at>clock_timestamp()
WHERE m.id=$1 AND m.owner_id=$2 AND m.actor_id=$3 AND m.conversation_id=$4 AND m.role='user' AND m.task_id=$6
 AND octet_length(m.content)<=$15
 AND m.sequence=$5+1 AND m.delivery_status IN ('pending','applied')`,
			messageID, current.OwnerID, current.InitiatorID, current.ConversationID, current.ConversationContext.ThroughSequence,
			current.ID, current.ProjectID, int(runeOffset)+1, maxRunes, current.Authorization.Version,
			current.AgentGeneration, current.AgentSessionID, current.Lease.Owner, current.Lease.Version, app.MaxConversationMessageBytes).
			Scan(&output.MessageID, &output.TaskID, &output.Sequence, &role, &output.Content, &output.TotalRunes, &output.SHA256)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "current conversation message is not available for this Task")
		}
		if err != nil {
			return fmt.Errorf("read bounded current conversation message chunk: %w", err)
		}
		if len(output.Content) > app.MaxConversationMessageChunkBytes {
			end := app.MaxConversationMessageChunkBytes
			for end > 0 && !utf8.RuneStart(output.Content[end]) {
				end--
			}
			output.Content = output.Content[:end]
		}
		if runeOffset > output.TotalRunes {
			return domain.InvalidArgument("message_read.rune_offset", "message read offset exceeds the source length")
		}
		output.Role = conversation.Role(role)
		output.RuneOffset = runeOffset
		output.NextRuneOffset = runeOffset + int64(utf8.RuneCountInString(output.Content))
		return nil
	})
	return output, err
}
