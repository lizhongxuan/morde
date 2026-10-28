package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateConversation(ctx context.Context, value conversation.Conversation, key app.IdempotencyRecord) (conversation.Conversation, bool, error) {
	if err := validateConversation(value); err != nil {
		return conversation.Conversation{}, false, err
	}
	if value.OwnerID != key.OwnerID {
		return conversation.Conversation{}, false, domain.InvalidArgument("owner_id", "conversation owner does not match idempotency owner")
	}
	var result conversation.Conversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, key, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			row, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: stored.ResourceID, OwnerID: key.OwnerID})
			if err != nil {
				return fmt.Errorf("load replayed conversation: %w", err)
			}
			result, replayed = conversationDomain(row), true
			return nil
		}
		_, err = tx.Exec(ctx, `
INSERT INTO conversations (id, owner_id, host_id, title, model_id)
VALUES ($1, $2, $3, $4, $5)`, value.ID, value.OwnerID, nullableText(value.HostID), value.Title, value.ModelID)
		if err != nil {
			return fmt.Errorf("create conversation: %w", err)
		}
		created, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return fmt.Errorf("read created conversation: %w", err)
		}
		result = conversationDomain(created)
		return nil
	})
	return result, replayed, err
}

// GetLegacyInstantChatReplay returns the canonical conversation and message
// pair copied by the instant-chat migration. It fails closed if the key exists
// but its canonical history is missing or inconsistent.
func (s *Store) GetLegacyInstantChatReplay(ctx context.Context, ownerID, key string, requestHash []byte) (app.LegacyInstantChatReplay, bool, error) {
	var storedHash []byte
	var userMessageID, assistantMessageID string
	err := s.pool.QueryRow(ctx, `SELECT request_hash, resource_id, related_id
FROM idempotency_keys WHERE owner_id=$1 AND operation='instant_chat.send' AND key=$2`, ownerID, key).
		Scan(&storedHash, &userMessageID, &assistantMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.LegacyInstantChatReplay{}, false, nil
	}
	if err != nil {
		return app.LegacyInstantChatReplay{}, false, fmt.Errorf("load legacy instant-chat idempotency record: %w", err)
	}
	if !bytes.Equal(storedHash, requestHash) {
		return app.LegacyInstantChatReplay{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
	}

	var result app.LegacyInstantChatReplay
	var userRole, assistantRole string
	err = s.pool.QueryRow(ctx, `SELECT
  c.id, c.owner_id, c.model_id, c.title, c.created_at, c.updated_at,
  u.id, u.conversation_id, u.owner_id, u.role, u.content, u.model_id, u.sequence,
  u.source_kind, u.source_exchange_id, u.requested_conversation_id, u.created_at,
  a.id, a.conversation_id, a.owner_id, a.role, a.content, a.model_id, a.sequence,
  a.source_kind, a.source_exchange_id, a.requested_conversation_id, a.created_at
FROM idempotency_keys k
JOIN conversation_messages u ON u.id=k.resource_id AND u.owner_id=k.owner_id
JOIN conversation_messages a ON a.id=k.related_id AND a.owner_id=k.owner_id
JOIN conversations c ON c.id=u.conversation_id AND c.owner_id=u.owner_id
WHERE k.owner_id=$1 AND k.operation='instant_chat.send' AND k.key=$2
  AND u.id=$3 AND a.id=$4 AND u.conversation_id=a.conversation_id AND c.host_id IS NULL
  AND c.source_kind='legacy_instant' AND u.task_id IS NULL AND a.task_id IS NULL
  AND u.role='user' AND a.role='assistant'
  AND u.source_kind='legacy_instant' AND a.source_kind='legacy_instant'
  AND u.source_exchange_id<>'' AND u.source_exchange_id=a.source_exchange_id
  AND u.requested_conversation_id=a.requested_conversation_id
  AND u.model_id=c.model_id AND a.model_id=c.model_id`, ownerID, key, userMessageID, assistantMessageID).Scan(
		&result.Conversation.ID, &result.Conversation.OwnerID, &result.Conversation.ModelID, &result.Conversation.Title,
		&result.Conversation.CreatedAt, &result.Conversation.UpdatedAt,
		&result.UserMessage.ID, &result.UserMessage.ConversationID, &result.UserMessage.OwnerID, &userRole,
		&result.UserMessage.Content, &result.UserMessage.ModelID, &result.UserMessage.Sequence,
		&result.UserMessage.SourceKind, &result.UserMessage.SourceExchangeID, &result.UserMessage.RequestedConversationID, &result.UserMessage.CreatedAt,
		&result.AssistantMessage.ID, &result.AssistantMessage.ConversationID, &result.AssistantMessage.OwnerID, &assistantRole,
		&result.AssistantMessage.Content, &result.AssistantMessage.ModelID, &result.AssistantMessage.Sequence,
		&result.AssistantMessage.SourceKind, &result.AssistantMessage.SourceExchangeID, &result.AssistantMessage.RequestedConversationID, &result.AssistantMessage.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.LegacyInstantChatReplay{}, false, domain.NewError(domain.CodeInternal, "legacy instant-chat history is unavailable")
	}
	if err != nil {
		return app.LegacyInstantChatReplay{}, false, fmt.Errorf("load legacy instant-chat history: %w", err)
	}
	result.UserMessage.Role = conversation.Role(userRole)
	result.AssistantMessage.Role = conversation.Role(assistantRole)
	return result, true, nil
}

func (s *Store) GetConversation(ctx context.Context, id, ownerID string) (conversation.Conversation, error) {
	row, err := s.q.GetConversation(ctx, sqlcgen.GetConversationParams{ID: id, OwnerID: ownerID})
	if err != nil {
		return conversation.Conversation{}, storeError("get conversation", "conversation", err)
	}
	return conversationDomain(row), nil
}

func (s *Store) ListConversations(ctx context.Context, ownerID string, hostIDs []string, page app.PageRequest) (app.ConversationPage, error) {
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ConversationPage{}, err
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.ConversationPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	rows, err := s.q.ListConversations(ctx, sqlcgen.ListConversationsParams{
		OwnerID: ownerID, Column2: hostIDs, Column3: cursorAt, ID: cursorID, Limit: limit + 1,
	})
	if err != nil {
		return app.ConversationPage{}, fmt.Errorf("list conversations: %w", err)
	}
	items := mapRows(rows, conversationDomain)
	pageResult := app.ConversationPage{Items: items}
	pageResult.NextCursor = nextPageCursor(items, limit, func(value conversation.Conversation) (string, time.Time) {
		return value.ID, value.CreatedAt
	})
	if int32(len(items)) > limit {
		pageResult.Items = items[:limit]
	}
	return pageResult, nil
}

func (s *Store) ListMessages(ctx context.Context, ownerID, conversationID string, page app.PageRequest) (app.MessagePage, error) {
	if _, err := s.GetConversation(ctx, conversationID, ownerID); err != nil {
		return app.MessagePage{}, err
	}
	limit, _, _, err := pagination(page.Limit, "")
	if err != nil {
		return app.MessagePage{}, err
	}
	var before int64
	if page.Cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(page.Cursor)
		if decodeErr != nil {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
		before, decodeErr = strconv.ParseInt(string(decoded), 10, 64)
		if decodeErr != nil || before < 1 {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
	}
	rows, err := s.q.ListMessages(ctx, sqlcgen.ListMessagesParams{
		OwnerID: ownerID, ConversationID: conversationID, Column3: before, Limit: limit + 1,
	})
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list conversation messages: %w", err)
	}
	items := mapRows(rows, messageDomain)
	pageResult := app.MessagePage{Items: items}
	if int32(len(items)) > limit {
		pageResult.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(items[limit-1].Sequence, 10)))
		pageResult.Items = items[:limit]
	}
	return pageResult, nil
}

func (s *Store) SubmitTask(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
	submission.Task = normalizeNewTask(submission.Task)
	newConversation := submission.Conversation != nil
	if submission.Task.ConversationID == "" {
		switch {
		case submission.Message.ConversationID != "":
			submission.Task.ConversationID = submission.Message.ConversationID
		case submission.Conversation != nil && submission.Conversation.ID != "":
			submission.Task.ConversationID = submission.Conversation.ID
		default:
			id, err := domain.NewConversationID()
			if err != nil {
				return app.TaskSubmissionResult{}, fmt.Errorf("generate task conversation id: %w", err)
			}
			submission.Task.ConversationID = id
			newConversation = true
		}
	}
	if newConversation && submission.Conversation == nil {
		submission.Conversation = &conversation.Conversation{ID: submission.Task.ConversationID, OwnerID: submission.Task.OwnerID, HostID: submission.Task.HostID}
	}
	if submission.Conversation != nil {
		if submission.Conversation.ID == "" {
			submission.Conversation.ID = submission.Task.ConversationID
		}
		if submission.Conversation.HostID == "" {
			submission.Conversation.HostID = submission.Task.HostID
		}
		if submission.Conversation.ModelID == "" {
			submission.Conversation.ModelID = submission.Task.ModelID
		}
	}
	if submission.Message.ConversationID == "" {
		submission.Message.ConversationID = submission.Task.ConversationID
	}
	if err := validateTask(submission.Task); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	message := submission.Message
	message.TaskID = submission.Task.ID
	if message.Role != conversation.RoleUser {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("message.role", "new task submissions must start with a user message")
	}
	if err := validateMessage(message); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if submission.Task.OwnerID != message.OwnerID || submission.Task.ConversationID != message.ConversationID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("task", "task and message associations do not match")
	}
	if submission.Conversation != nil {
		if err := validateConversation(*submission.Conversation); err != nil {
			return app.TaskSubmissionResult{}, err
		}
		if submission.Conversation.ID != submission.Task.ConversationID || submission.Conversation.OwnerID != submission.Task.OwnerID {
			return app.TaskSubmissionResult{}, domain.InvalidArgument("conversation", "conversation and task associations do not match")
		}
	}
	eventPayload, err := validateInitialTaskEvent(submission.InitialEvent.Type, submission.InitialEvent.Payload)
	if err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateBatchItems(submission.Task.ID, submission.BatchItems); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if submission.Idempotency.OwnerID != submission.Task.OwnerID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("owner_id", "task owner does not match idempotency owner")
	}

	var result app.TaskSubmissionResult
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, submission.Idempotency, submission.Task.ID, message.ID)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, submission.Idempotency.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, submission.Task.OwnerID, nil)
		if err != nil {
			return err
		}
		if submission.Task.HostID != "" {
			if err := lockTaskTargetHost(ctx, tx, submission.Task.HostID, submission.Task.OwnerID); err != nil {
				return err
			}
		}
		if err := lockTaskWorkspaceInput(ctx, tx, submission.Task); err != nil {
			return err
		}
		if submission.Conversation != nil {
			_, err := tx.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, title, model_id) VALUES ($1, $2, $3, $4, $5)`,
				submission.Conversation.ID, submission.Conversation.OwnerID, nullableText(submission.Conversation.HostID),
				submission.Conversation.Title, submission.Conversation.ModelID)
			if err != nil {
				return fmt.Errorf("create task conversation: %w", err)
			}
		} else {
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
				submission.Task.ConversationID, submission.Task.OwnerID).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation not found")
			}
			if err != nil {
				return fmt.Errorf("lock task conversation: %w", err)
			}
		}
		var activeTaskID string
		err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE conversation_id=$1 AND status IN ('queued','running','waiting_user','pausing','cancelling') FOR UPDATE`,
			submission.Task.ConversationID).Scan(&activeTaskID)
		if err == nil {
			return domain.NewError(domain.CodeInvalidState, "conversation already has an active task")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check active conversation task: %w", err)
		}
		if err := insertTask(ctx, tx, submission.Task, 1, gate.epoch); err != nil {
			return err
		}
		if err := insertTaskMCPConnectorGrants(ctx, tx, submission.Task.OwnerID, submission.Task.ID, submission.MCPConnectorIDs); err != nil {
			return err
		}
		if err := insertMessage(ctx, tx, message); err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{
			TaskID: submission.Task.ID, Sequence: 1, Type: submission.InitialEvent.Type, Payload: eventPayload,
		}); err != nil {
			return err
		}
		if err := insertBatchItems(ctx, tx, submission.Task.ID, submission.BatchItems); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp(), model_id=$3, host_id=$4 WHERE id=$1 AND owner_id=$2`,
			submission.Task.ConversationID, submission.Task.OwnerID, submission.Task.ModelID, nullableText(submission.Task.HostID))
		if err != nil {
			return fmt.Errorf("touch task conversation: %w", err)
		}
		result, err = loadSubmissionResult(ctx, tx, submission.Task.OwnerID, submission.Task.ID, message.ID)
		return err
	})
	return result, err
}

func (s *Store) AppendMessage(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
	submission.Task = normalizeNewTask(submission.Task)
	message := submission.Message
	if !domain.ValidConversationID(message.ConversationID) || !domain.ValidUserID(message.OwnerID) ||
		message.Role != conversation.RoleUser || message.Content == "" || !domain.ValidMessageID(message.ID) {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("message", "message fields are invalid")
	}
	if submission.Task.OwnerID != message.OwnerID || submission.Task.ConversationID != message.ConversationID ||
		(submission.Task.HostID != "" && !domain.ValidHostID(submission.Task.HostID)) || submission.Task.ModelID == "" {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("task", "task routing fields do not match message")
	}
	if submission.Idempotency.OwnerID != message.OwnerID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("owner_id", "message owner does not match idempotency owner")
	}
	var result app.TaskSubmissionResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var conversationRow sqlcgen.Conversation
		err := tx.QueryRow(ctx, `SELECT id, owner_id, host_id, title, created_at, updated_at FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
			message.ConversationID, message.OwnerID).Scan(&conversationRow.ID, &conversationRow.OwnerID, &conversationRow.HostID,
			&conversationRow.Title, &conversationRow.CreatedAt, &conversationRow.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		if err != nil {
			return fmt.Errorf("lock message conversation: %w", err)
		}
		stored, replay, err := findIdempotency(ctx, tx, submission.Idempotency)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, message.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, message.OwnerID, nil)
		if err != nil {
			return err
		}
		var active sqlcgen.Task
		err = tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens,
lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id,
workspace_id, input_revision, context_digest, file_delivery_intent
FROM tasks WHERE conversation_id=$1 AND owner_id=$2 AND status IN ('queued','running','waiting_user','pausing','cancelling') FOR UPDATE`,
			message.ConversationID, message.OwnerID).Scan(
			&active.ID, &active.OwnerID, &active.ConversationID, &active.HostID, &active.ModelID, &active.Objective, &active.Status,
			&active.AuthKind, &active.AuthTokenID, &active.AuthScopes, &active.AuthClientInstallationID, &active.BudgetActiveSeconds, &active.BudgetToolCalls,
			&active.BudgetModelTokens, &active.UsageActiveSeconds, &active.UsageToolCalls, &active.UsageModelTokens,
			&active.LeaseOwner, &active.LeaseVersion, &active.LeaseExpiresAt, &active.EventSeq, &active.CurrentStep,
			&active.Result, &active.WaitingReason, &active.CreatedAt, &active.UpdatedAt, &active.FinishedAt, &active.ApplicationID,
			&active.WorkspaceID, &active.InputRevision, &active.ContextDigest, &active.FileDeliveryIntent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find active conversation task: %w", err)
		}
		var targetTask task.Task
		newTask := errors.Is(err, pgx.ErrNoRows)
		if newTask {
			candidate := submission.Task
			if err := validateTask(candidate); err != nil {
				return err
			}
			if err := validateNormalizedTaskMCPConnectorIDs(submission.MCPConnectorIDs); err != nil {
				return err
			}
			targetTask = candidate
		} else {
			targetTask = taskDomain(active)
			if err := validateNormalizedTaskMCPConnectorIDs(submission.MCPConnectorIDs); err != nil {
				return err
			}
			if len(submission.MCPConnectorIDs) > 0 {
				if err := requireTaskMCPConnectorGrantSet(ctx, tx, targetTask.OwnerID, targetTask.ID, submission.MCPConnectorIDs); err != nil {
					return err
				}
			}
			var taskEpoch int64
			if err := tx.QueryRow(ctx, `SELECT execution_epoch FROM tasks WHERE id=$1 AND owner_id=$2`, targetTask.ID, targetTask.OwnerID).Scan(&taskEpoch); err != nil {
				return fmt.Errorf("read active task execution epoch: %w", err)
			}
			if taskEpoch != gate.epoch {
				return ownerExecutionDisabledError(gate)
			}
			if targetTask.Status == task.StatusPausing || targetTask.Status == task.StatusCancelling {
				return domain.NewError(domain.CodeInvalidState, "conversation task is stopping")
			}
			if targetTask.HostID != submission.Task.HostID || targetTask.ModelID != submission.Task.ModelID ||
				targetTask.ApplicationID != submission.Task.ApplicationID {
				return domain.NewError(domain.CodeInvalidState, "message host, model and application must match the active task")
			}
			if submission.Task.WorkspaceID != "" && (targetTask.WorkspaceID != submission.Task.WorkspaceID ||
				!sameTaskInputRevision(targetTask.InputRevision, submission.Task.InputRevision) ||
				!bytes.Equal(targetTask.ContextDigest, submission.Task.ContextDigest)) {
				return domain.NewError(domain.CodeInvalidState, "message workspace input must match the active task")
			}
		}
		if targetTask.HostID != "" {
			if err := lockTaskTargetHost(ctx, tx, targetTask.HostID, targetTask.OwnerID); err != nil {
				return err
			}
		}
		if newTask {
			if err := lockTaskWorkspaceInput(ctx, tx, targetTask); err != nil {
				return err
			}
		}
		message.TaskID = targetTask.ID
		if err := validateMessage(message); err != nil {
			return err
		}
		stored, replay, err = reserveIdempotency(ctx, tx, submission.Idempotency, targetTask.ID, message.ID)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, message.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		if newTask {
			payload, err := validateInitialTaskEvent(submission.InitialEvent.Type, submission.InitialEvent.Payload)
			if err != nil {
				return err
			}
			if err := validateBatchItems(targetTask.ID, submission.BatchItems); err != nil {
				return err
			}
			if err := insertTask(ctx, tx, targetTask, 1, gate.epoch); err != nil {
				return err
			}
			if err := insertTaskMCPConnectorGrants(ctx, tx, targetTask.OwnerID, targetTask.ID, submission.MCPConnectorIDs); err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: targetTask.ID, Sequence: 1, Type: submission.InitialEvent.Type, Payload: payload}); err != nil {
				return err
			}
			if err := insertBatchItems(ctx, tx, targetTask.ID, submission.BatchItems); err != nil {
				return err
			}
		} else {
			payload, err := json.Marshal(map[string]string{"message_id": message.ID, "role": string(message.Role), "content": message.Content})
			if err != nil {
				return fmt.Errorf("encode task message event: %w", err)
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			status := string(targetTask.Status)
			if targetTask.Status == task.StatusWaitingUser {
				status = string(task.StatusQueued)
			}
			var sequence int64
			err = tx.QueryRow(ctx, `UPDATE tasks SET status=$2,
file_delivery_intent=CASE
  WHEN file_delivery_intent='required' OR $3='required' THEN 'required'
  WHEN file_delivery_intent='unclear' OR $3='unclear' THEN 'unclear'
  ELSE 'none'
END,
event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 RETURNING event_seq`, targetTask.ID, status, string(submission.Task.FileDeliveryIntent)).Scan(&sequence)
			if err != nil {
				return fmt.Errorf("advance task after message: %w", err)
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: targetTask.ID, Sequence: sequence, Type: "task.message", Payload: payload}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp(), model_id=$3, host_id=$4 WHERE id=$1 AND owner_id=$2`,
			message.ConversationID, message.OwnerID, targetTask.ModelID, nullableText(targetTask.HostID))
		if err != nil {
			return fmt.Errorf("touch conversation after message: %w", err)
		}
		result, err = loadSubmissionResult(ctx, tx, message.OwnerID, targetTask.ID, message.ID)
		return err
	})
	return result, err
}

// Hold the Host lock so deletion cannot race a task target check.
func lockTaskTargetHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
SELECT state<>'deleted' AND deleted_at IS NULL
FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`, hostID, ownerID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !exists) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock task target Host: %w", err)
	}
	return nil
}

func lockTaskWorkspaceInput(ctx context.Context, tx pgx.Tx, value task.Task) error {
	if value.WorkspaceID == "" {
		return nil
	}
	if value.HostID == "" || value.InputRevision == nil || len(value.ContextDigest) != 32 {
		return domain.InvalidArgument("workspace_id", "workspace input snapshot is incomplete")
	}
	var ownerID, state string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).
		Scan(&ownerID, &state, &revision)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != value.OwnerID || state != string(workspace.StateReady))) {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err != nil {
		return fmt.Errorf("lock task input workspace: %w", err)
	}
	if revision != *value.InputRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed before task submission")
	}
	var bindingOwner, bindingState, syncState string
	var desiredRevision, appliedRevision int64
	err = tx.QueryRow(ctx, `SELECT owner_id, state, sync_state, desired_revision, applied_revision
FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, value.HostID, value.WorkspaceID).
		Scan(&bindingOwner, &bindingState, &syncState, &desiredRevision, &appliedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if err != nil {
		return fmt.Errorf("lock task input workspace binding: %w", err)
	}
	if bindingOwner != value.OwnerID || bindingState != string(workspace.BindingActive) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if syncState != string(workspace.BindingSyncHealthy) || desiredRevision != revision || appliedRevision != revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Host has not applied the committed workspace input")
	}
	return nil
}

func sameTaskInputRevision(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// lockAIHost holds a shared Host row lock until the surrounding transaction
// ends. Control transitions take an exclusive Host lock before changing the
// fence, so task writes linearize before or after the AI-control change.
func lockAIHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) error {
	var allowed bool
	err := tx.QueryRow(ctx, `
SELECT state='running' AND control_state='ai' AND deleted_at IS NULL
FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`, hostID, ownerID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock Host AI control fence: %w", err)
	}
	if !allowed {
		return domain.NewError(domain.CodeInvalidState, "host is not available for AI tasks")
	}
	return nil
}

func loadSubmissionResult(ctx context.Context, tx pgx.Tx, ownerID, taskID, messageID string) (app.TaskSubmissionResult, error) {
	queries := sqlcgen.New(tx)
	taskRow, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted task", "task", err)
	}
	messageRow, err := queries.GetMessage(ctx, sqlcgen.GetMessageParams{ID: messageID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted message", "message", err)
	}
	conversationRow, err := queries.GetConversation(ctx, sqlcgen.GetConversationParams{ID: taskRow.ConversationID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted conversation", "conversation", err)
	}
	return app.TaskSubmissionResult{
		Task: taskDomain(taskRow), Message: messageDomain(messageRow), Conversation: conversationDomain(conversationRow),
	}, nil
}

func insertTask(ctx context.Context, tx pgx.Tx, value task.Task, eventSequence int64, executionEpoch int64) error {
	value = normalizeNewTask(value)
	if executionEpoch < 1 {
		return missingExecutionGateError()
	}
	_, err := tx.Exec(ctx, `
INSERT INTO tasks (
  id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes,
  budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls,
  usage_model_tokens, lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, finished_at, execution_epoch, application_id,
  auth_client_installation_id, execution_location, workspace_id, input_revision, context_digest, auth_version, auth_expires_at,
  file_delivery_intent
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::text[], '{}'::text[]),$11,$12,$13,$14,$15,$16,'',0,NULL,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31
)`, value.ID, value.OwnerID, value.ConversationID, nullableText(value.HostID), value.ModelID, value.Objective,
		string(value.Status), value.Authorization.Kind, value.Authorization.TokenID, value.Authorization.Scopes,
		value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens,
		value.Usage.ActiveSeconds, value.Usage.ToolCalls, int64(0),
		eventSequence, value.CurrentStep, value.Result, value.WaitingReason, nullableTimestamp(value.FinishedAt), executionEpoch, value.ApplicationID,
		nullableText(value.Authorization.ClientInstallationID), value.ExecutionLocation, nullableText(value.WorkspaceID),
		value.InputRevision, value.ContextDigest, value.Authorization.Version, value.Authorization.ExpiresAt, string(value.FileDeliveryIntent))
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func insertMessage(ctx context.Context, tx pgx.Tx, value conversation.Message) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE conversations SET message_seq=message_seq+1 WHERE id=$1 AND owner_id=$2 RETURNING message_seq`,
		value.ConversationID, value.OwnerID).Scan(&sequence); err != nil {
		return storeError("reserve message sequence", "conversation", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO conversation_messages
(id, conversation_id, owner_id, role, content, task_id, model_id, source_kind, source_exchange_id, requested_conversation_id, sequence)
VALUES ($1,$2,$3,$4,$5,$6,COALESCE(NULLIF($7, ''), (SELECT model_id FROM tasks WHERE id=$6), ''),$8,$9,$10,$11)`,
		value.ID, value.ConversationID, value.OwnerID, string(value.Role), value.Content, nullableText(value.TaskID), value.ModelID,
		stringOrDefault(value.SourceKind, "task"), value.SourceExchangeID, value.RequestedConversationID, sequence)
	if err != nil {
		return fmt.Errorf("insert conversation message: %w", err)
	}
	return nil
}

func insertTaskEvent(ctx context.Context, tx pgx.Tx, value task.Event) error {
	if !domain.ValidTaskID(value.TaskID) || value.Sequence < 1 || !validTaskEventType(value.Type) {
		return domain.InvalidArgument("event", "task event fields are invalid")
	}
	payload, err := eventPayload(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_events (task_id, sequence, event_type, payload) VALUES ($1,$2,$3,$4)`,
		value.TaskID, value.Sequence, value.Type, payload)
	if err != nil {
		return fmt.Errorf("insert task event: %w", err)
	}
	return nil
}

func validateBatchItems(taskID string, values []task.BatchItem) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !domain.ValidBatchItemID(value.ID) || (value.TaskID != "" && value.TaskID != taskID) || value.Key == "" || len(value.Key) > 255 || value.Input == "" || !validBatchStatus(value.Status) {
			return domain.InvalidArgument("batch_items", "batch item fields are invalid")
		}
		if _, exists := seen[value.Key]; exists {
			return domain.InvalidArgument("batch_items", "batch item keys must be unique")
		}
		seen[value.Key] = struct{}{}
	}
	return nil
}

func validBatchStatus(value task.BatchItemStatus) bool {
	switch value {
	case "", task.BatchItemQueued:
		return true
	default:
		return false
	}
}

func insertBatchItems(ctx context.Context, tx pgx.Tx, taskID string, values []task.BatchItem) error {
	for _, value := range values {
		_, err := tx.Exec(ctx, `INSERT INTO task_batch_items (id, task_id, item_key, input, status, result, error) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			value.ID, taskID, value.Key, value.Input, stringOrDefault(string(value.Status), string(task.BatchItemQueued)), value.Result, value.Error)
		if err != nil {
			return fmt.Errorf("insert task batch item: %w", err)
		}
	}
	return nil
}

func stringOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
