package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	taskReadScope    identity.Scope = identity.ScopeTaskRead
	taskSubmitScope  identity.Scope = identity.ScopeTaskSubmit
	taskControlScope identity.Scope = identity.ScopeTaskControl
)

type ConfiguredModelCatalog interface {
	IsConfiguredModel(context.Context, string, string) (bool, error)
}

type TaskExecutionAuthorizer interface {
	AuthorizeTaskExecution(context.Context, identity.Principal, string, identity.Scope) (task.Authorization, error)
}

type SubmitTaskCommand struct {
	HostID         string
	ApplicationID  string
	ModelID        string
	ConversationID string
	Objective      string
	Budget         task.Budget
	BatchItems     []BatchItemInput
	IdempotencyKey string
}

type AppendMessageCommand struct {
	HostID         string
	ApplicationID  string
	ModelID        string
	Content        string
	Budget         task.Budget
	IdempotencyKey string
}

type BatchItemInput struct {
	Key   string
	Input string
}

type TaskService struct {
	store               TaskStore
	authorizer          TaskAuthorizer
	executionAuthorizer TaskExecutionAuthorizer
	models              ConfiguredModelCatalog
	actions             TaskActionRequester
}

func NewTaskService(store TaskStore, authorizer TaskAuthorizer, models ConfiguredModelCatalog) (*TaskService, error) {
	executionAuthorizer, ok := authorizer.(TaskExecutionAuthorizer)
	if store == nil || !ok || models == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "task service dependencies are required")
	}
	actions, _ := store.(TaskActionRequester)
	return &TaskService{store: store, authorizer: authorizer, executionAuthorizer: executionAuthorizer, models: models, actions: actions}, nil
}

func (s *TaskService) Submit(ctx context.Context, principal identity.Principal, command SubmitTaskCommand) (TaskSubmissionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskSubmissionResult{}, err
	}
	if command.HostID != "" && !domain.ValidHostID(command.HostID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	if command.ApplicationID != "" && !ValidHostApplicationID(command.ApplicationID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("application_id", "application is not supported")
	}
	command.ModelID = strings.TrimSpace(command.ModelID)
	if command.ModelID == "" || len(command.ModelID) > 256 {
		return TaskSubmissionResult{}, domain.InvalidArgument("model_id", "model id is invalid")
	}
	if !validBoundedText(command.Objective, 32768) {
		return TaskSubmissionResult{}, domain.InvalidArgument("objective", "objective is required and must be at most 32768 characters")
	}
	if command.ConversationID != "" && !domain.ValidConversationID(command.ConversationID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("conversation_id", "conversation id is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return TaskSubmissionResult{}, err
	}
	budget, err := normalizedBudget(command.Budget)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	command.Budget = budget
	batchItems, err := newBatchItems(command.BatchItems)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	authorization, err := s.authorizeTask(ctx, principal, command.HostID, taskSubmitScope)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	if err := s.validateModel(ctx, principal.UserID, command.ModelID); err != nil {
		return TaskSubmissionResult{}, err
	}

	conversationID := command.ConversationID
	var newConversation *conversation.Conversation
	if conversationID == "" {
		conversationID, err = domain.NewConversationID()
		if err != nil {
			return TaskSubmissionResult{}, fmt.Errorf("generate task conversation id: %w", err)
		}
		newConversation = &conversation.Conversation{
			ID: conversationID, OwnerID: principal.UserID, HostID: command.HostID,
			Title: truncateRunes(strings.TrimSpace(command.Objective), 120),
		}
	} else {
		_, getErr := s.store.GetConversation(ctx, conversationID, principal.UserID)
		if getErr != nil {
			return TaskSubmissionResult{}, getErr
		}
	}

	taskID, err := domain.NewTaskID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task id: %w", err)
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task message id: %w", err)
	}
	requestHash, err := hashIdempotencyRequest(struct {
		HostID         string           `json:"host_id"`
		ApplicationID  string           `json:"application_id,omitempty"`
		ModelID        string           `json:"model_id"`
		ConversationID string           `json:"conversation_id,omitempty"`
		Objective      string           `json:"objective"`
		Budget         task.Budget      `json:"budget"`
		BatchItems     []BatchItemInput `json:"batch_items"`
	}{command.HostID, command.ApplicationID, command.ModelID, command.ConversationID, command.Objective, command.Budget, normalizedBatchInputs(command.BatchItems)})
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	initialEvent, err := json.Marshal(map[string]string{"status": string(task.StatusQueued)})
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("encode initial task event: %w", err)
	}
	result, err := s.store.SubmitTask(ctx, TaskSubmission{
		Conversation: newConversation,
		Task: task.Task{
			ID: taskID, OwnerID: principal.UserID, ConversationID: conversationID,
			HostID: command.HostID, ApplicationID: command.ApplicationID, ModelID: command.ModelID, Objective: command.Objective,
			ExecutionLocation: "server", Status: task.StatusQueued, Authorization: authorization, Budget: command.Budget,
		},
		Message: conversation.Message{
			ID: messageID, ConversationID: conversationID, OwnerID: principal.UserID,
			Role: conversation.RoleUser, Content: command.Objective,
		},
		InitialEvent: task.Event{TaskID: taskID, Type: "task.status", Payload: initialEvent},
		BatchItems:   batchItems,
		Idempotency: IdempotencyRecord{
			OwnerID: principal.UserID, Operation: "task.submit", Key: command.IdempotencyKey,
			RequestHash: requestHash, ResourceID: taskID, RelatedID: messageID,
		},
	})
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	return result, nil
}

func (s *TaskService) AppendMessage(ctx context.Context, principal identity.Principal, conversationID string, command AppendMessageCommand) (TaskSubmissionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskSubmissionResult{}, err
	}
	if !domain.ValidConversationID(conversationID) {
		return TaskSubmissionResult{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if command.HostID != "" && !domain.ValidHostID(command.HostID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	if command.ApplicationID != "" && !ValidHostApplicationID(command.ApplicationID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("application_id", "application is not supported")
	}
	command.ModelID = strings.TrimSpace(command.ModelID)
	if command.ModelID == "" || len(command.ModelID) > 256 {
		return TaskSubmissionResult{}, domain.InvalidArgument("model_id", "model id is invalid")
	}
	if !validBoundedText(command.Content, 32768) {
		return TaskSubmissionResult{}, domain.InvalidArgument("content", "message is required and must be at most 32768 characters")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return TaskSubmissionResult{}, err
	}
	budget, err := normalizedBudget(command.Budget)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	command.Budget = budget
	if _, err := s.store.GetConversation(ctx, conversationID, principal.UserID); err != nil {
		return TaskSubmissionResult{}, err
	}
	authorization, err := s.authorizeTask(ctx, principal, command.HostID, taskSubmitScope)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	if err := s.validateModel(ctx, principal.UserID, command.ModelID); err != nil {
		return TaskSubmissionResult{}, err
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task id: %w", err)
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task message id: %w", err)
	}
	requestHash, err := hashIdempotencyRequest(struct {
		ConversationID string      `json:"conversation_id"`
		HostID         string      `json:"host_id"`
		ApplicationID  string      `json:"application_id,omitempty"`
		ModelID        string      `json:"model_id"`
		Content        string      `json:"content"`
		Budget         task.Budget `json:"budget"`
	}{conversationID, command.HostID, command.ApplicationID, command.ModelID, command.Content, command.Budget})
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	initialEvent, err := json.Marshal(map[string]string{"status": string(task.StatusQueued)})
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("encode initial task event: %w", err)
	}
	return s.store.AppendMessage(ctx, TaskSubmission{
		Task: task.Task{
			ID: taskID, OwnerID: principal.UserID, ConversationID: conversationID,
			HostID: command.HostID, ApplicationID: command.ApplicationID, ModelID: command.ModelID, Objective: command.Content,
			ExecutionLocation: "server", Status: task.StatusQueued, Authorization: authorization, Budget: command.Budget,
		},
		Message: conversation.Message{
			ID: messageID, ConversationID: conversationID, OwnerID: principal.UserID,
			Role: conversation.RoleUser, Content: command.Content,
		},
		InitialEvent: task.Event{TaskID: taskID, Type: "task.status", Payload: initialEvent},
		Idempotency: IdempotencyRecord{
			OwnerID: principal.UserID, Operation: "conversation.append_message", Key: command.IdempotencyKey,
			RequestHash: requestHash, ResourceID: taskID, RelatedID: messageID,
		},
	})
}

func (s *TaskService) List(ctx context.Context, principal identity.Principal, filter TaskListRequest) (TaskPage, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskPage{}, err
	}
	if err := validatePageRequest(filter.Page); err != nil {
		return TaskPage{}, err
	}
	if filter.Page.Limit == 0 {
		filter.Page.Limit = defaultPageSize
	}
	if filter.Status != "" && !validTaskStatus(filter.Status) {
		return TaskPage{}, domain.InvalidArgument("status", "task status is invalid")
	}
	hostIDs, err := authorizedTaskHostIDs(ctx, s.authorizer, principal, taskReadScope)
	if err != nil {
		return TaskPage{}, err
	}
	return s.store.ListTasks(ctx, principal.UserID, TaskListRequest{HostIDs: hostIDs, Status: filter.Status, Page: filter.Page})
}

func (s *TaskService) Get(ctx context.Context, principal identity.Principal, id string) (TaskDetails, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskDetails{}, err
	}
	if !domain.ValidTaskID(id) {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	details, err := s.store.GetTask(ctx, id, principal.UserID)
	if err != nil {
		return TaskDetails{}, err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskReadScope); err != nil {
		return TaskDetails{}, hideTaskAccessError(err)
	}
	return details, nil
}

// RetryBatchItem resets one failed batch item and queues its task for execution.
// The store applies the idempotency record and state transition atomically.
func (s *TaskService) RetryBatchItem(ctx context.Context, principal identity.Principal, taskID, batchItemID, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidBatchItemID(batchItemID) {
		return domain.NewError(domain.CodeNotFound, "task or batch item not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.batch_item.retry", idempotencyKey, struct {
		Action      string `json:"action"`
		TaskID      string `json:"task_id"`
		BatchItemID string `json:"batch_item_id"`
	}{"retry_batch_item", taskID, batchItemID}, taskID, batchItemID)
	if err != nil {
		return err
	}
	return s.store.RetryFailedBatchItem(ctx, record, taskID, batchItemID)
}

// RespondToPendingRequest records one decision against an immutable approval
// request version. The store performs the idempotent state transition and
// queues the task atomically.
func (s *TaskService) RespondToPendingRequest(ctx context.Context, principal identity.Principal, taskID, pendingID string, version int64, decision task.PendingDecision, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingID) {
		return domain.NewError(domain.CodeNotFound, "task or pending request not found")
	}
	if version < 1 || (decision != task.PendingDecisionApprove && decision != task.PendingDecisionReject) {
		return domain.InvalidArgument("pending_request", "pending request version or decision is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.pending.respond", idempotencyKey, struct {
		Action   string               `json:"action"`
		TaskID   string               `json:"task_id"`
		ID       string               `json:"pending_id"`
		Version  int64                `json:"pending_version"`
		Decision task.PendingDecision `json:"decision"`
	}{"respond", taskID, pendingID, version, decision}, taskID, pendingID)
	if err != nil {
		return err
	}
	return s.store.RespondToPendingRequest(ctx, record, taskID, pendingID, version, decision)
}

// AnswerClarificationRequest records a free-form answer against the exact
// active Agent question and atomically queues the waiting task.
func (s *TaskService) AnswerClarificationRequest(ctx context.Context, principal identity.Principal, taskID, clarificationID string, version int64, answer, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(clarificationID) {
		return domain.NewError(domain.CodeNotFound, "task or clarification request not found")
	}
	if version < 1 || !validBoundedText(answer, 16384) || strings.ContainsRune(answer, '\x00') {
		return domain.InvalidArgument("clarification", "clarification version or answer is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.clarification.answer", idempotencyKey, struct {
		Action  string `json:"action"`
		TaskID  string `json:"task_id"`
		ID      string `json:"clarification_id"`
		Version int64  `json:"clarification_version"`
		Answer  string `json:"answer"`
	}{"answer_clarification", taskID, clarificationID, version, answer}, taskID, clarificationID)
	if err != nil {
		return err
	}
	return s.store.AnswerClarificationRequest(ctx, record, taskID, clarificationID, version, answer)
}

func (s *TaskService) ListEvents(ctx context.Context, principal identity.Principal, id string, after int64, limit int32) (TaskEventPage, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskEventPage{}, err
	}
	if !domain.ValidTaskID(id) {
		return TaskEventPage{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if after < 0 || limit < 1 || limit > maxEventPage {
		return TaskEventPage{}, domain.InvalidArgument("after", "event cursor or limit is out of range")
	}
	page, err := s.store.GetTaskEventPage(ctx, principal.UserID, id, after, limit)
	if err != nil && domain.CodeOf(err) != domain.CodeEventCursorExpired {
		return TaskEventPage{}, err
	}
	if _, err := s.authorizeTask(ctx, principal, page.HostID, taskReadScope); err != nil {
		return TaskEventPage{}, hideTaskAccessError(err)
	}
	if err != nil {
		return page, err
	}
	previous := after
	for _, event := range page.Items {
		if event.TaskID != id || event.Sequence < 1 || event.Sequence != previous+1 ||
			!validTaskEventType(event.Type) || !json.Valid(event.Payload) || event.CreatedAt.IsZero() {
			return TaskEventPage{}, domain.NewError(domain.CodeInternal, "stored task events are invalid")
		}
		previous = event.Sequence
	}
	return page, nil
}

func (s *TaskService) validateModel(ctx context.Context, ownerID, modelID string) error {
	configured, err := s.models.IsConfiguredModel(ctx, ownerID, modelID)
	if err != nil {
		return fmt.Errorf("check configured model: %w", err)
	}
	if !configured {
		return domain.InvalidArgument("model_id", "model is not configured for this user")
	}
	return nil
}

func (s *TaskService) authorizeTask(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if s.executionAuthorizer == nil {
		return task.Authorization{}, domain.NewError(domain.CodeUnsupported, "task authorization is unavailable")
	}
	return s.executionAuthorizer.AuthorizeTaskExecution(ctx, principal, hostID, scope)
}

func authorizedTaskHostIDs(ctx context.Context, authorizer TaskAuthorizer, principal identity.Principal, scope identity.Scope) ([]string, error) {
	if principal.Kind == identity.KindSession || principal.Kind == identity.KindClient {
		// The store still filters by owner_id. NULL host_ids means all of this user's Hosts.
		return nil, nil
	}
	if !principal.HasScope(scope) {
		return nil, domain.NewError(domain.CodeForbidden, "task scope is required")
	}
	ids := append([]string{}, principal.HostIDs...)
	if len(ids) == 0 {
		return ids, nil
	}
	allowed := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !domain.ValidHostID(id) {
			return nil, domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		_, err := authorizeTaskHost(ctx, authorizer, principal, id, scope)
		if domain.CodeOf(err) == domain.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, id)
	}
	sort.Strings(allowed)
	return allowed, nil
}

func normalizedBudget(value task.Budget) (task.Budget, error) {
	if value == (task.Budget{}) {
		value = task.Budget{ActiveSeconds: task.MaxActiveSeconds, ToolCalls: task.MaxToolCalls, ModelTokens: task.MaxModelTokens}
	}
	if err := value.Validate(); err != nil {
		return task.Budget{}, domain.InvalidArgument("budget", err.Error())
	}
	return value, nil
}

func newBatchItems(values []BatchItemInput) ([]task.BatchItem, error) {
	if len(values) > 100 {
		return nil, domain.InvalidArgument("batch_items", "at most 100 batch items are allowed")
	}
	items := make([]task.BatchItem, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validBoundedText(value.Key, 128) || !validBoundedText(value.Input, 16384) {
			return nil, domain.InvalidArgument("batch_items", "batch item key or input is invalid")
		}
		if _, exists := seen[value.Key]; exists {
			return nil, domain.InvalidArgument("batch_items", "batch item keys must be unique")
		}
		seen[value.Key] = struct{}{}
		id, err := domain.NewBatchItemID()
		if err != nil {
			return nil, fmt.Errorf("generate batch item id: %w", err)
		}
		items = append(items, task.BatchItem{ID: id, Key: value.Key, Input: value.Input, Status: task.BatchItemQueued})
	}
	return items, nil
}

func normalizedBatchInputs(values []BatchItemInput) []BatchItemInput {
	if values == nil {
		return []BatchItemInput{}
	}
	return values
}

func truncateRunes(value string, maxRunes int) string {
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	return string([]rune(value)[:maxRunes])
}

func validTaskStatus(status task.Status) bool {
	switch status {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusPaused,
		task.StatusCancelling, task.StatusCompleted, task.StatusFailed, task.StatusCancelled:
		return true
	default:
		return false
	}
}

func validTaskEventType(value string) bool {
	switch value {
	case "task.status", "task.step", "task.operation", "task.message", "task.batch_item", "task.artifact", "task.waiting_user", "task.response", "task.result":
		return true
	default:
		return false
	}
}
