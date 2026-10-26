package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type TaskService interface {
	Submit(context.Context, identity.Principal, app.SubmitTaskCommand) (app.TaskSubmissionResult, error)
	AppendMessage(context.Context, identity.Principal, string, app.AppendMessageCommand) (app.TaskSubmissionResult, error)
	List(context.Context, identity.Principal, app.TaskListRequest) (app.TaskPage, error)
	Get(context.Context, identity.Principal, string) (app.TaskDetails, error)
	RequestAction(context.Context, identity.Principal, string, task.Action, task.Status, string) (app.TaskActionResult, error)
	RetryBatchItem(context.Context, identity.Principal, string, string, string) error
	RespondToPendingRequest(context.Context, identity.Principal, string, string, int64, task.PendingDecision, string) error
	AnswerClarificationRequest(context.Context, identity.Principal, string, string, int64, string, string) error
	ListEvents(context.Context, identity.Principal, string, int64, int32) (app.TaskEventPage, error)
}

type taskPageResponse struct {
	Items      []taskSummaryResponse `json:"items"`
	NextCursor *string               `json:"next_cursor"`
}

type taskSummaryResponse struct {
	ID             string             `json:"id"`
	OwnerID        string             `json:"owner_id"`
	HostID         string             `json:"host_id"`
	ApplicationID  string             `json:"application_id,omitempty"`
	ConversationID string             `json:"conversation_id"`
	ModelID        string             `json:"model_id"`
	Objective      string             `json:"objective"`
	Status         task.Status        `json:"status"`
	CurrentStep    *string            `json:"current_step"`
	Budget         taskBudgetResponse `json:"budget"`
	Usage          taskUsageResponse  `json:"usage"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

type taskResponse struct {
	taskSummaryResponse
	Result               *string                           `json:"result"`
	WaitingReason        *string                           `json:"waiting_reason"`
	Steps                []taskStepResponse                `json:"steps"`
	BatchItems           []taskBatchItemResponse           `json:"batch_items"`
	Artifacts            []taskArtifactResponse            `json:"artifacts"`
	PendingRequest       *taskPendingRequestResponse       `json:"pending_request"`
	ClarificationRequest *taskClarificationRequestResponse `json:"clarification_request"`
	StopRequest          *taskStopRequestResponse          `json:"stop_request"`
	FinishedAt           *time.Time                        `json:"finished_at"`
}

type taskStopRequestResponse struct {
	ID                    string                 `json:"id"`
	Action                task.Action            `json:"action"`
	Status                task.StopRequestStatus `json:"status"`
	SafeAfter             time.Time              `json:"safe_after"`
	UnsettledOperationIDs []string               `json:"unsettled_operation_ids"`
}

type taskActionResponse struct {
	TaskID                string      `json:"task_id"`
	Action                string      `json:"action"`
	Status                task.Status `json:"status"`
	UnsettledOperationIDs []string    `json:"unsettled_operation_ids"`
}

type taskPendingRequestResponse struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Object  string `json:"object"`
	Content string `json:"content"`
	Impact  string `json:"impact"`
}

type taskClarificationRequestResponse struct {
	ID       string `json:"id"`
	Version  int64  `json:"version"`
	Question string `json:"question"`
}

func validTaskText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) > 0 && utf8.RuneCountInString(value) <= maxRunes
}

func validTaskActionExpectedStatus(action task.Action, status task.Status) bool {
	switch action {
	case task.ActionPause:
		return status == task.StatusQueued || status == task.StatusRunning
	case task.ActionResume:
		return status == task.StatusPaused
	case task.ActionCancel:
		return status == task.StatusQueued || status == task.StatusRunning || status == task.StatusWaitingUser || status == task.StatusPaused
	default:
		return false
	}
}

type taskBudgetResponse struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

type taskUsageResponse struct {
	ActiveSeconds int64  `json:"active_seconds"`
	ToolCalls     int64  `json:"tool_calls"`
	ModelTokens   *int64 `json:"model_tokens"`
}

type taskStepResponse struct {
	ID        string          `json:"id"`
	Sequence  int64           `json:"sequence"`
	Kind      string          `json:"kind"`
	Status    task.StepStatus `json:"status"`
	Title     string          `json:"title"`
	Summary   string          `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
}

type taskBatchItemResponse struct {
	ID      string               `json:"id"`
	Key     string               `json:"key"`
	Status  task.BatchItemStatus `json:"status"`
	Attempt int32                `json:"attempt"`
	Result  *string              `json:"result"`
	Error   *string              `json:"error"`
}

type taskArtifactResponse struct {
	ID           string    `json:"id"`
	StepID       string    `json:"step_id"`
	Label        string    `json:"label"`
	Path         string    `json:"path"`
	MediaType    string    `json:"media_type"`
	BatchItemID  *string   `json:"batch_item_id"`
	BatchAttempt int32     `json:"batch_attempt"`
	CreatedAt    time.Time `json:"created_at"`
}

type submitTaskRequest struct {
	HostID         string             `json:"host_id"`
	ApplicationID  string             `json:"application_id"`
	ModelID        string             `json:"model_id"`
	ConversationID *string            `json:"conversation_id"`
	Objective      string             `json:"objective"`
	Budget         *taskBudgetRequest `json:"budget"`
	BatchItems     []batchItemRequest `json:"batch_items"`
}

type taskBudgetRequest struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

type batchItemRequest struct {
	Key   string `json:"key"`
	Input string `json:"input"`
}

type taskActionRequest struct {
	Action               string               `json:"action"`
	ExpectedStatus       task.Status          `json:"expected_status"`
	BatchItemID          string               `json:"batch_item_id"`
	PendingID            string               `json:"pending_id"`
	PendingVersion       int64                `json:"pending_version"`
	Decision             task.PendingDecision `json:"decision"`
	ClarificationID      string               `json:"clarification_id"`
	ClarificationVersion int64                `json:"clarification_version"`
	Answer               string               `json:"answer"`
}

func (h *api) registerTasks(group *gin.RouterGroup, service TaskService) {
	if service == nil {
		return
	}
	group.GET("/tasks", h.listTasks(service))
	group.POST("/tasks", h.submitTask(service))
	group.GET("/tasks/:id", h.getTask(service))
	group.POST("/tasks/:id/actions", h.taskAction(service))
	group.GET("/tasks/:id/events", h.streamTaskEvents(service))
}

func (h *api) taskAction(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		var request taskActionRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		switch request.Action {
		case "retry_batch_item":
			if !domain.ValidBatchItemID(request.BatchItemID) || request.PendingID != "" || request.PendingVersion != 0 || request.Decision != "" || request.ExpectedStatus != "" ||
				request.ClarificationID != "" || request.ClarificationVersion != 0 || request.Answer != "" {
				writeError(c, domain.InvalidArgument("action", "retry_batch_item requires only a valid batch_item_id"))
				return
			}
			if err := service.RetryBatchItem(c.Request.Context(), principal, taskID, request.BatchItemID, c.GetHeader("Idempotency-Key")); err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: request.Action, Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
			return
		case "respond":
			if request.BatchItemID != "" || request.ExpectedStatus != "" || request.ClarificationID != "" || request.ClarificationVersion != 0 || request.Answer != "" ||
				!domain.ValidPendingRequestID(request.PendingID) || request.PendingVersion < 1 ||
				(request.Decision != task.PendingDecisionApprove && request.Decision != task.PendingDecisionReject) {
				writeError(c, domain.InvalidArgument("action", "respond requires a pending_id, positive pending_version, and approve or reject decision"))
				return
			}
			if err := service.RespondToPendingRequest(c.Request.Context(), principal, taskID, request.PendingID, request.PendingVersion, request.Decision, c.GetHeader("Idempotency-Key")); err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: request.Action, Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
			return
		case "answer_clarification":
			if request.BatchItemID != "" || request.PendingID != "" || request.PendingVersion != 0 || request.Decision != "" || request.ExpectedStatus != "" ||
				!domain.ValidPendingRequestID(request.ClarificationID) || request.ClarificationVersion < 1 || !validTaskText(request.Answer, 16384) ||
				strings.ContainsRune(request.Answer, '\x00') || strings.TrimSpace(request.Answer) == "" {
				writeError(c, domain.InvalidArgument("action", "answer_clarification requires a clarification_id, positive clarification_version, and answer"))
				return
			}
			if err := service.AnswerClarificationRequest(c.Request.Context(), principal, taskID, request.ClarificationID, request.ClarificationVersion, request.Answer, c.GetHeader("Idempotency-Key")); err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: request.Action, Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
			return
		case "pause", "resume", "cancel":
			action := task.Action(request.Action)
			if request.BatchItemID != "" || request.PendingID != "" || request.PendingVersion != 0 || request.Decision != "" || request.ClarificationID != "" ||
				request.ClarificationVersion != 0 || request.Answer != "" ||
				!validTaskActionExpectedStatus(action, request.ExpectedStatus) {
				writeError(c, domain.InvalidArgument("action", "pause, resume, or cancel requires a valid expected_status and no other action fields"))
				return
			}
			result, err := service.RequestAction(c.Request.Context(), principal, taskID, action, request.ExpectedStatus, c.GetHeader("Idempotency-Key"))
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusAccepted, taskActionResponse{
				TaskID: result.Task.ID, Action: string(result.Action), Status: result.Task.Status,
				UnsettledOperationIDs: append([]string{}, result.UnsettledOperationIDs...),
			})
			return
		default:
			writeError(c, domain.InvalidArgument("action", "supported task actions are retry_batch_item, respond, answer_clarification, pause, resume, and cancel"))
			return
		}
	}
}

func (h *api) submitTask(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request submitTaskRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		budget, ok := requestBudget(c, request.Budget)
		if !ok {
			return
		}
		batchItems := make([]app.BatchItemInput, 0, len(request.BatchItems))
		for _, item := range request.BatchItems {
			batchItems = append(batchItems, app.BatchItemInput{Key: item.Key, Input: item.Input})
		}
		conversationID := ""
		if request.ConversationID != nil {
			if !domain.ValidConversationID(*request.ConversationID) {
				writeError(c, domain.InvalidArgument("conversation_id", "conversation id is invalid"))
				return
			}
			conversationID = *request.ConversationID
		}
		result, err := service.Submit(c.Request.Context(), principal, app.SubmitTaskCommand{
			HostID: request.HostID, ApplicationID: request.ApplicationID, ModelID: request.ModelID, ConversationID: conversationID,
			Objective: request.Objective, Budget: budget, BatchItems: batchItems,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toTaskResponse(app.TaskDetails{Task: result.Task})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, response)
	}
}

func (h *api) listTasks(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		status, ok := singleQuery(c, "status")
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, app.TaskListRequest{Status: task.Status(status), Page: page})
		if err != nil {
			writeError(c, err)
			return
		}
		response := taskPageResponse{Items: make([]taskSummaryResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			view, err := toTaskSummaryResponse(item)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, view)
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) getTask(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		value, err := service.Get(c.Request.Context(), principal, id)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toTaskResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func toTaskSummaryResponse(value task.Task) (taskSummaryResponse, error) {
	if !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidHostID(value.HostID) ||
		!domain.ValidConversationID(value.ConversationID) || strings.TrimSpace(value.ModelID) == "" ||
		(value.ApplicationID != "" && !app.ValidHostApplicationID(value.ApplicationID)) || !validTaskStatusResponse(value.Status) ||
		value.Budget.Validate() != nil || value.Usage.ActiveSeconds < 0 || value.Usage.ToolCalls < 0 || (value.Usage.ModelTokens != nil && *value.Usage.ModelTokens < 0) ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return taskSummaryResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return taskSummaryResponse{
		ID: value.ID, OwnerID: value.OwnerID, HostID: value.HostID, ApplicationID: value.ApplicationID, ConversationID: value.ConversationID,
		ModelID: value.ModelID, Objective: value.Objective, Status: value.Status, CurrentStep: stringPointer(value.CurrentStep),
		Budget: taskBudgetResponse(value.Budget), Usage: taskUsageResponse{
			ActiveSeconds: value.Usage.ActiveSeconds, ToolCalls: value.Usage.ToolCalls, ModelTokens: value.Usage.ModelTokens,
		}, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}, nil
}

func toTaskResponse(value app.TaskDetails) (taskResponse, error) {
	summary, err := toTaskSummaryResponse(value.Task)
	if err != nil {
		return taskResponse{}, err
	}
	response := taskResponse{
		taskSummaryResponse: summary, Result: stringPointer(value.Task.Result), WaitingReason: stringPointer(value.Task.WaitingReason),
		Steps: make([]taskStepResponse, 0, len(value.Steps)), BatchItems: make([]taskBatchItemResponse, 0, len(value.BatchItems)),
		Artifacts: make([]taskArtifactResponse, 0, len(value.Artifacts)), FinishedAt: value.Task.FinishedAt,
	}
	if pending := value.PendingRequest; pending != nil && pending.Status == task.PendingRequestPending {
		if value.Task.Status != task.StatusWaitingUser || !domain.ValidPendingRequestID(pending.ID) || pending.TaskID != value.Task.ID || pending.Version < 1 || pending.Kind != "approval" ||
			!validTaskText(pending.Title, 512) || !validTaskText(pending.Object, 512) || !validTaskText(pending.Content, 512) || !validTaskText(pending.Impact, 512) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.PendingRequest = &taskPendingRequestResponse{
			ID: pending.ID, Version: pending.Version, Kind: pending.Kind, Title: pending.Title,
			Object: pending.Object, Content: pending.Content, Impact: pending.Impact,
		}
	}
	for _, clarification := range value.ClarificationRequests {
		if clarification.Status != task.ClarificationPending {
			continue
		}
		if value.Task.Status != task.StatusWaitingUser || value.Task.WaitingReason != "clarification_required" ||
			!domain.ValidPendingRequestID(clarification.ID) || clarification.TaskID != value.Task.ID || clarification.Version < 1 ||
			!domain.ValidStepID(clarification.ModelStepID) || clarification.CallID == "" || len(clarification.CallID) > 256 ||
			!validTaskText(clarification.Question, 4096) || clarification.Answer != "" || clarification.RespondedAt != nil {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if response.ClarificationRequest != nil {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.ClarificationRequest = &taskClarificationRequestResponse{
			ID: clarification.ID, Version: clarification.Version, Question: clarification.Question,
		}
	}
	if stop := value.StopRequest; stop != nil {
		if !domain.ValidTaskActionID(stop.ID) || stop.TaskID != value.Task.ID ||
			(stop.Action != task.ActionPause && stop.Action != task.ActionCancel) ||
			(stop.Status != task.StopRequestPending && stop.Status != task.StopRequestCompleted) || stop.SafeAfter.IsZero() {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if stop.Status == task.StopRequestPending &&
			((stop.Action == task.ActionPause && value.Task.Status != task.StatusPausing) ||
				(stop.Action == task.ActionCancel && value.Task.Status != task.StatusCancelling)) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if stop.Status == task.StopRequestCompleted && len(value.StopOperationIDs) != 0 {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		unsettled := make([]string, 0, len(value.StopOperationIDs))
		for _, id := range value.StopOperationIDs {
			if !domain.ValidOperationID(id) {
				return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
			}
			unsettled = append(unsettled, id)
		}
		response.StopRequest = &taskStopRequestResponse{
			ID: stop.ID, Action: stop.Action, Status: stop.Status, SafeAfter: stop.SafeAfter,
			UnsettledOperationIDs: unsettled,
		}
	}
	for _, step := range value.Steps {
		if !domain.ValidStepID(step.ID) || step.TaskID != value.Task.ID || step.Sequence < 1 || !validStepStatus(step.Status) ||
			!validStepKind(step.Kind) || utf8.RuneCountInString(step.Summary) > 2000 || step.CreatedAt.IsZero() {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.Steps = append(response.Steps, taskStepResponse{
			ID: step.ID, Sequence: step.Sequence, Kind: step.Kind, Status: step.Status, Title: step.Title,
			Summary: step.Summary, CreatedAt: step.CreatedAt,
		})
	}
	for _, item := range value.BatchItems {
		if !domain.ValidBatchItemID(item.ID) || item.TaskID != value.Task.ID || !validBatchStatusResponse(item.Status) || item.Attempt < 1 {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.BatchItems = append(response.BatchItems, taskBatchItemResponse{
			ID: item.ID, Key: item.Key, Status: item.Status, Attempt: item.Attempt, Result: stringPointer(item.Result), Error: stringPointer(item.Error),
		})
	}
	batchItemIDs := make(map[string]struct{}, len(value.BatchItems))
	for _, item := range value.BatchItems {
		batchItemIDs[item.ID] = struct{}{}
	}
	for _, artifact := range value.Artifacts {
		if !domain.ValidArtifactID(artifact.ID) || !domain.ValidStepID(artifact.StepID) || artifact.TaskID != value.Task.ID || artifact.Label == "" || artifact.Path == "" || artifact.MediaType == "" || artifact.CreatedAt.IsZero() ||
			(artifact.BatchItemID == "" && artifact.BatchAttempt != 0) ||
			(artifact.BatchItemID != "" && (!domain.ValidBatchItemID(artifact.BatchItemID) || artifact.BatchAttempt < 1)) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if artifact.BatchItemID != "" {
			if _, exists := batchItemIDs[artifact.BatchItemID]; !exists {
				return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
			}
		}
		response.Artifacts = append(response.Artifacts, taskArtifactResponse{
			ID: artifact.ID, StepID: artifact.StepID, Label: artifact.Label, Path: artifact.Path,
			MediaType: artifact.MediaType, BatchItemID: stringPointer(artifact.BatchItemID), BatchAttempt: artifact.BatchAttempt, CreatedAt: artifact.CreatedAt,
		})
	}
	return response, nil
}

func requestBudget(c *gin.Context, request *taskBudgetRequest) (task.Budget, bool) {
	if request == nil {
		return task.Budget{}, true
	}
	budget := task.Budget{ActiveSeconds: request.ActiveSeconds, ToolCalls: request.ToolCalls, ModelTokens: request.ModelTokens}
	if err := budget.Validate(); err != nil {
		writeError(c, domain.InvalidArgument("budget", err.Error()))
		return task.Budget{}, false
	}
	return budget, true
}

func decodeStrictTaskJSON(c *gin.Context, target any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeTaskDecodeError(c, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple json values")
		}
		writeTaskDecodeError(c, err)
		return false
	}
	return true
}

func writeTaskDecodeError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
		return
	}
	writeError(c, domain.InvalidArgument("body", "invalid json request"))
}

func requestPage(c *gin.Context) (app.PageRequest, bool) {
	limitValues, exists := c.Request.URL.Query()["limit"]
	limit := int64(50)
	if exists {
		if len(limitValues) != 1 {
			writeError(c, domain.InvalidArgument("limit", "page limit is invalid"))
			return app.PageRequest{}, false
		}
		parsed, err := strconv.ParseInt(limitValues[0], 10, 32)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(c, domain.InvalidArgument("limit", "page limit is invalid"))
			return app.PageRequest{}, false
		}
		limit = parsed
	}
	cursor, ok := singleQuery(c, "cursor")
	if !ok {
		return app.PageRequest{}, false
	}
	if len(cursor) > 1024 {
		writeError(c, domain.InvalidArgument("cursor", "page cursor is invalid"))
		return app.PageRequest{}, false
	}
	return app.PageRequest{Limit: int32(limit), Cursor: cursor}, true
}

func singleQuery(c *gin.Context, key string) (string, bool) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return "", true
	}
	if len(values) != 1 {
		writeError(c, domain.InvalidArgument(key, "query parameter must appear once"))
		return "", false
	}
	return values[0], true
}

func validTaskStatusResponse(value task.Status) bool {
	switch value {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusPaused,
		task.StatusCancelling, task.StatusCompleted, task.StatusFailed, task.StatusCancelled:
		return true
	default:
		return false
	}
}

func validStepStatus(value task.StepStatus) bool {
	switch value {
	case task.StepQueued, task.StepRunning, task.StepWaitingUser, task.StepSucceeded, task.StepFailed, task.StepUnknown:
		return true
	default:
		return false
	}
}

func validStepKind(value string) bool {
	switch value {
	case "plan", "tool", "wait", "result":
		return true
	default:
		return false
	}
}

func validBatchStatusResponse(value task.BatchItemStatus) bool {
	switch value {
	case task.BatchItemQueued, task.BatchItemRunning, task.BatchItemSucceeded, task.BatchItemFailed, task.BatchItemUnknown:
		return true
	default:
		return false
	}
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func cursorPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
