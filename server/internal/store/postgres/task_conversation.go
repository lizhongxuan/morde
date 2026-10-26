package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
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
INSERT INTO conversations (id, owner_id, host_id, title)
VALUES ($1, $2, $3, $4)`, value.ID, value.OwnerID, value.HostID, value.Title)
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
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.MessagePage{}, err
	}
	rows, err := s.q.ListMessages(ctx, sqlcgen.ListMessagesParams{
		OwnerID: ownerID, ConversationID: conversationID, Column3: cursorAt, ID: cursorID, Limit: limit + 1,
	})
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list conversation messages: %w", err)
	}
	items := mapRows(rows, messageDomain)
	pageResult := app.MessagePage{Items: items}
	pageResult.NextCursor = nextPageCursor(items, limit, func(value conversation.Message) (string, time.Time) {
		return value.ID, value.CreatedAt
	})
	if int32(len(items)) > limit {
		pageResult.Items = items[:limit]
	}
	return pageResult, nil
}

func (s *Store) SubmitTask(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
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
		if submission.Conversation.ID != submission.Task.ConversationID || submission.Conversation.OwnerID != submission.Task.OwnerID ||
			submission.Conversation.HostID != submission.Task.HostID {
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
		if err := lockAIHost(ctx, tx, submission.Task.HostID, submission.Task.OwnerID); err != nil {
			return err
		}
		if err := requireRunningTaskApplication(ctx, tx, submission.Task); err != nil {
			return err
		}
		if submission.Conversation != nil {
			_, err := tx.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, title) VALUES ($1, $2, $3, $4)`,
				submission.Conversation.ID, submission.Conversation.OwnerID, submission.Conversation.HostID, submission.Conversation.Title)
			if err != nil {
				return fmt.Errorf("create task conversation: %w", err)
			}
		} else {
			var hostID string
			err := tx.QueryRow(ctx, `SELECT host_id FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
				submission.Task.ConversationID, submission.Task.OwnerID).Scan(&hostID)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation not found")
			}
			if err != nil {
				return fmt.Errorf("lock task conversation: %w", err)
			}
			if hostID != submission.Task.HostID {
				return domain.NewError(domain.CodeInvalidState, "task host does not match conversation host")
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
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`,
			submission.Task.ConversationID, submission.Task.OwnerID)
		if err != nil {
			return fmt.Errorf("touch task conversation: %w", err)
		}
		result, err = loadSubmissionResult(ctx, tx, submission.Task.OwnerID, submission.Task.ID, message.ID)
		return err
	})
	return result, err
}

func (s *Store) AppendMessage(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
	message := submission.Message
	if !domain.ValidConversationID(message.ConversationID) || !domain.ValidUserID(message.OwnerID) ||
		message.Role != conversation.RoleUser || message.Content == "" || !domain.ValidMessageID(message.ID) {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("message", "message fields are invalid")
	}
	if submission.Task.OwnerID != message.OwnerID || submission.Task.ConversationID != message.ConversationID ||
		!domain.ValidHostID(submission.Task.HostID) || submission.Task.ModelID == "" {
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
lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id
FROM tasks WHERE conversation_id=$1 AND owner_id=$2 AND status IN ('queued','running','waiting_user','pausing','cancelling') FOR UPDATE`,
			message.ConversationID, message.OwnerID).Scan(
			&active.ID, &active.OwnerID, &active.ConversationID, &active.HostID, &active.ModelID, &active.Objective, &active.Status,
			&active.AuthKind, &active.AuthTokenID, &active.AuthScopes, &active.AuthClientInstallationID, &active.BudgetActiveSeconds, &active.BudgetToolCalls,
			&active.BudgetModelTokens, &active.UsageActiveSeconds, &active.UsageToolCalls, &active.UsageModelTokens,
			&active.LeaseOwner, &active.LeaseVersion, &active.LeaseExpiresAt, &active.EventSeq, &active.CurrentStep,
			&active.Result, &active.WaitingReason, &active.CreatedAt, &active.UpdatedAt, &active.FinishedAt, &active.ApplicationID)
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
			targetTask = candidate
		} else {
			targetTask = taskDomain(active)
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
				targetTask.ApplicationID != submission.Task.ApplicationID || conversationRow.HostID != targetTask.HostID {
				return domain.NewError(domain.CodeInvalidState, "message host, model and application must match the active task")
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
		if err := lockAIHost(ctx, tx, targetTask.HostID, targetTask.OwnerID); err != nil {
			return err
		}
		if err := requireRunningTaskApplication(ctx, tx, targetTask); err != nil {
			return err
		}
		if newTask {
			if targetTask.HostID != conversationRow.HostID {
				return domain.NewError(domain.CodeInvalidState, "task host does not match conversation host")
			}
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
			err = tx.QueryRow(ctx, `UPDATE tasks SET status=$2, event_seq=event_seq+1, updated_at=clock_timestamp() WHERE id=$1 RETURNING event_seq`,
				targetTask.ID, status).Scan(&sequence)
			if err != nil {
				return fmt.Errorf("advance task after message: %w", err)
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: targetTask.ID, Sequence: sequence, Type: "task.message", Payload: payload}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, message.ConversationID, message.OwnerID)
		if err != nil {
			return fmt.Errorf("touch conversation after message: %w", err)
		}
		result, err = loadSubmissionResult(ctx, tx, message.OwnerID, targetTask.ID, message.ID)
		return err
	})
	return result, err
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

// The Host row lock taken by lockAIHost serializes this check with application
// launch updates, which take the same Host lock before changing launch state.
func requireRunningTaskApplication(ctx context.Context, tx pgx.Tx, value task.Task) error {
	if value.ApplicationID == "" {
		return nil
	}
	if !app.ValidHostApplicationID(value.ApplicationID) {
		return domain.InvalidArgument("application_id", "application is not supported")
	}
	var running bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM host_application_launches AS a
  JOIN hosts AS h ON h.id=a.host_id AND h.owner_id=a.owner_id
  WHERE a.host_id=$1 AND a.owner_id=$2 AND a.application_id=$3 AND a.status='running'
    AND a.runtime_generation=h.runtime_generation AND a.runtime_instance_id=h.runtime_instance_id
    AND a.control_version=h.control_version
)`, value.HostID, value.OwnerID, value.ApplicationID).Scan(&running)
	if err != nil {
		return fmt.Errorf("check task application launch: %w", err)
	}
	if !running {
		return domain.NewError(domain.CodeInvalidState, "target application is not running on this Host")
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
	if executionEpoch < 1 {
		return missingExecutionGateError()
	}
	_, err := tx.Exec(ctx, `
INSERT INTO tasks (
  id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes,
  budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls,
  usage_model_tokens, lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, finished_at, execution_epoch, application_id,
  auth_client_installation_id
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::text[], '{}'::text[]),$11,$12,$13,$14,$15,$16,'',0,NULL,$17,$18,$19,$20,$21,$22,$23,$24
)`, value.ID, value.OwnerID, value.ConversationID, value.HostID, value.ModelID, value.Objective,
		string(value.Status), value.Authorization.Kind, value.Authorization.TokenID, value.Authorization.Scopes,
		value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens,
		value.Usage.ActiveSeconds, value.Usage.ToolCalls, taskUsageTokens(value.Usage.ModelTokens),
		eventSequence, value.CurrentStep, value.Result, value.WaitingReason, nullableTimestamp(value.FinishedAt), executionEpoch, value.ApplicationID,
		nullableText(value.Authorization.ClientInstallationID))
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func insertMessage(ctx context.Context, tx pgx.Tx, value conversation.Message) error {
	_, err := tx.Exec(ctx, `INSERT INTO conversation_messages (id, conversation_id, owner_id, role, content, task_id) VALUES ($1,$2,$3,$4,$5,$6)`,
		value.ID, value.ConversationID, value.OwnerID, string(value.Role), value.Content, value.TaskID)
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
