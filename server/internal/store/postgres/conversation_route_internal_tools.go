package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetConversationRouteInternalToolCall(ctx context.Context, taskID, ownerID string) (app.ConversationRouteInternalToolCall, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return app.ConversationRouteInternalToolCall{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	var value app.ConversationRouteInternalToolCall
	var arguments []byte
	err := s.pool.QueryRow(ctx, `SELECT task_id,owner_id,message_id,call_id,tool_name,arguments::text,reply_message_id
FROM conversation_route_internal_tool_calls WHERE task_id=$1 AND owner_id=$2`, taskID, ownerID).Scan(
		&value.TaskID, &value.OwnerID, &value.MessageID, &value.CallID, &value.ToolName, &arguments, &value.ReplyMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ConversationRouteInternalToolCall{}, domain.NewError(domain.CodeNotFound, "internal route tool call not found")
	}
	if err != nil {
		return app.ConversationRouteInternalToolCall{}, fmt.Errorf("read internal conversation route tool call: %w", err)
	}
	value.Arguments = string(arguments)
	if !domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidMessageID(value.MessageID) ||
		!domain.ValidMessageID(value.ReplyMessageID) || value.CallID != "morde_route_tool_"+value.MessageID ||
		!app.ValidConversationRouteDirectToolCall(value.ToolName, arguments) {
		return app.ConversationRouteInternalToolCall{}, domain.NewError(domain.CodeInvalidState, "stored internal route tool call is invalid")
	}
	return value, nil
}

func (s *Store) PublishPendingConversationRouteInternalToolReplies(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, domain.InvalidArgument("context", "context is required")
	}
	published := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskID, ownerID, actorID, conversationID, modelID, sourceMessageID, replyMessageID, clientMessageID string
		var status task.Status
		err := tx.QueryRow(ctx, `SELECT internal_tool.task_id,internal_tool.owner_id,t.initiator_id,t.conversation_id,t.model_id,
 internal_tool.message_id,internal_tool.reply_message_id,COALESCE(route.client_message_id,''),t.status
FROM conversation_route_internal_tool_calls internal_tool
JOIN tasks t ON t.id=internal_tool.task_id AND t.owner_id=internal_tool.owner_id
JOIN conversation_message_routes route ON route.message_id=internal_tool.message_id AND route.owner_id=internal_tool.owner_id
WHERE internal_tool.reply_published_at IS NULL AND t.status='waiting_user'
  AND t.waiting_reason IN ('mcp_result_unknown','mcp_connector_revoked','tool_result_unknown')
ORDER BY internal_tool.created_at,internal_tool.task_id
LIMIT 1 FOR UPDATE OF internal_tool,t SKIP LOCKED`).Scan(
			&taskID, &ownerID, &actorID, &conversationID, &modelID, &sourceMessageID, &replyMessageID, &clientMessageID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim pending internal route tool reply: %w", err)
		}
		if status != task.StatusWaitingUser || !domain.ValidMessageID(replyMessageID) {
			return domain.NewError(domain.CodeInvalidState, "pending internal route tool reply identity is invalid")
		}
		content := "网页工具的执行结果目前无法确认，系统已保留操作账本且不会自动重复调用。你可以重新提出查询。"
		if err := insertMessage(ctx, tx, conversation.Message{ID: replyMessageID, ConversationID: conversationID,
			OwnerID: ownerID, ActorID: actorID, Role: conversation.RoleAssistant, Content: content, ModelID: modelID,
			SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_route_internal_tool_calls
SET reply_published_at=clock_timestamp() WHERE task_id=$1 AND owner_id=$2 AND reply_published_at IS NULL`, taskID, ownerID)
		if err != nil {
			return fmt.Errorf("mark internal route tool reply as published: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "internal route tool reply changed before publication")
		}
		if err := recordAccountObjectChanges(ctx, tx,
			accountObjectChange{OwnerID: ownerID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert", MutationID: clientMessageID},
			accountObjectChange{OwnerID: ownerID, ObjectType: "message", ObjectID: replyMessageID, Operation: "upsert", MutationID: clientMessageID},
		); err != nil {
			return err
		}
		published = true
		return nil
	})
	return published, err
}
