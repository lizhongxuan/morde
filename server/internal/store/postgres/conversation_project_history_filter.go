package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

const maxProjectConversationHistoryFilterIDs = 800

func (s *Store) FilterTaskProjectConversationHistory(ctx context.Context, current task.Task, messageIDs []string) (map[string]struct{}, error) {
	if ctx == nil || s == nil || s.pool == nil || !domain.ValidTaskID(current.ID) || !domain.ValidUserID(current.OwnerID) ||
		!domain.ValidUserID(current.InitiatorID) || !project.ValidID(current.ProjectID) || !domain.ValidConversationID(current.ConversationID) ||
		current.ExecutionLocation != "server" || current.Status != task.StatusRunning || current.Authorization.Version < 1 ||
		current.Lease.Owner == "" || current.Lease.Version < 1 || current.AgentSessionID == "" || current.ConversationContext == nil ||
		current.ConversationContext.OwnerID != current.OwnerID || current.ConversationContext.ProjectID != current.ProjectID ||
		current.ConversationContext.InitiatorID != current.InitiatorID || current.ConversationContext.ConversationID != current.ConversationID ||
		current.ConversationContext.Scope != "project" || current.ConversationContext.ThroughSequence < 0 ||
		len(messageIDs) > maxProjectConversationHistoryFilterIDs {
		return nil, domain.NewError(domain.CodeForbidden, "project conversation history is unavailable for this Task")
	}
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if !domain.ValidMessageID(id) {
			return nil, domain.InvalidArgument("message_ids", "project conversation history message IDs are invalid")
		}
		if _, exists := seen[id]; exists {
			return nil, domain.InvalidArgument("message_ids", "project conversation history message IDs must be unique")
		}
		seen[id] = struct{}{}
	}

	allowed := make(map[string]struct{}, len(messageIDs))
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, current.InitiatorID, current.ProjectID, project.RoleMember, false); err != nil {
			return err
		}
		var taskID string
		err := tx.QueryRow(ctx, `SELECT id FROM tasks
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 AND project_id=$5
  AND execution_location='server' AND status='running' AND auth_kind=$6 AND auth_token_id=$7
  AND COALESCE(auth_scopes,'{}')=COALESCE($8::text[],'{}')
  AND COALESCE(auth_client_installation_id,'')=$9 AND auth_version=$10
  AND auth_expires_at IS NOT DISTINCT FROM $11::timestamptz
  AND lease_owner=$12 AND lease_version=$13 AND lease_expires_at>clock_timestamp()
  AND agent_generation=$14 AND agent_session_id=$15
FOR SHARE`, current.ID, current.OwnerID, current.InitiatorID, current.ConversationID, current.ProjectID,
			current.Authorization.Kind, current.Authorization.TokenID, current.Authorization.Scopes,
			current.Authorization.ClientInstallationID, current.Authorization.Version, current.Authorization.ExpiresAt,
			current.Lease.Owner, current.Lease.Version, current.AgentGeneration, current.AgentSessionID).Scan(&taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeForbidden, "project conversation history authorization changed")
		}
		if err != nil {
			return fmt.Errorf("verify project history Task authorization: %w", err)
		}
		var conversationID string
		err = tx.QueryRow(ctx, `SELECT id FROM conversations
WHERE id=$1 AND owner_id=$2 AND project_id=$3 AND archived_at IS NULL FOR SHARE`,
			current.ConversationID, current.OwnerID, current.ProjectID).Scan(&conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeForbidden, "project conversation history authorization changed")
		}
		if err != nil {
			return fmt.Errorf("verify project history conversation scope: %w", err)
		}
		if len(messageIDs) == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT id FROM conversation_messages
WHERE id=ANY($1::text[]) AND owner_id=$2 AND conversation_id=$3 AND sequence<=$4`,
			messageIDs, current.OwnerID, current.ConversationID, current.ConversationContext.ThroughSequence)
		if err != nil {
			return fmt.Errorf("bound project history message IDs to Task snapshot: %w", err)
		}
		candidates := make([]task.ConversationContextMessage, 0, len(messageIDs))
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan project history message ID: %w", err)
			}
			candidates = append(candidates, task.ConversationContextMessage{ID: id})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read project history message IDs: %w", err)
		}
		rows.Close()
		filtered, err := filterConversationRouteTaskHistory(ctx, tx, current, candidates)
		if err != nil {
			return err
		}
		for _, message := range filtered {
			allowed[message.ID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return allowed, nil
}
