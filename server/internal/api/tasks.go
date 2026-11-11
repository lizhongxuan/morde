package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type TaskService interface {
	Submit(context.Context, identity.Principal, app.SubmitTaskCommand) (app.TaskSubmissionResult, error)
	AcceptConversationMessage(context.Context, identity.Principal, string, app.AcceptConversationMessageCommand) (conversation.Message, bool, error)
	InitializeConversationMessageUpload(context.Context, identity.Principal, string, app.InitializeConversationMessageUploadCommand) (app.ConversationMessageUpload, error)
	PutConversationMessageChunk(context.Context, identity.Principal, string, string, string, string, int, []byte, string) error
	GetConversationMessageUpload(context.Context, identity.Principal, string, string, string, string) (app.ConversationMessageUpload, error)
	CommitConversationMessageUpload(context.Context, identity.Principal, string, string, string, string, string) (conversation.Message, error)
	AssociateConversationMessageAttachment(context.Context, identity.Principal, string, string, string, string, string) error
	RetryConversationMessageProcessing(context.Context, identity.Principal, string, string, string, string, string) error
	List(context.Context, identity.Principal, app.TaskListRequest) (app.TaskPage, error)
	ListConversationActiveTasks(context.Context, identity.Principal, string, string, app.PageRequest) (app.TaskPage, error)
	Get(context.Context, identity.Principal, string) (app.TaskDetails, error)
	RequestAction(context.Context, identity.Principal, string, task.Action, task.Status, string) (app.TaskActionResult, error)
	RetryBatchItem(context.Context, identity.Principal, string, string, string) error
	RespondToPendingRequest(context.Context, identity.Principal, string, string, int64, task.PendingDecision, string) error
	AnswerClarificationRequest(context.Context, identity.Principal, string, string, int64, string, string) error
	ListEvents(context.Context, identity.Principal, string, int64, int32) (app.TaskEventPage, error)
}

type TaskFileFetchQueryService interface {
	GetTaskFileFetch(context.Context, identity.Principal, string, string) (app.TaskFileFetch, error)
}

type TaskRevisionFencedActionService interface {
	RequestActionWithRevisionFence(context.Context, identity.Principal, string, task.Action, task.Status, *app.TaskActionRevisionFence, string) (app.TaskActionResult, error)
}

type TaskWorkbenchObservationTicketService interface {
	IssueWorkbenchObservationTicket(context.Context, identity.Principal, string) (app.TaskWorkbenchObservationTicket, error)
}

type TaskWorkspaceViewDeletionService interface {
	RequestWorkspaceViewDeletion(context.Context, identity.Principal, string, bool, string) (app.TaskWorkspaceViewDeletion, error)
	GetWorkspaceViewDeletion(context.Context, identity.Principal, string) (app.TaskWorkspaceViewDeletion, error)
}

type TaskMCPConnectorGrantService interface {
	RevokeMCPConnectorGrant(context.Context, identity.Principal, string, string, int64) (app.TaskMCPConnectorGrant, error)
}

type taskPageResponse struct {
	Items      []taskSummaryResponse `json:"items"`
	NextCursor *string               `json:"next_cursor"`
}

type taskSummaryResponse struct {
	ID                          string                    `json:"id"`
	OwnerID                     string                    `json:"owner_id"`
	InitiatorID                 string                    `json:"initiator_id"`
	HostID                      string                    `json:"host_id,omitempty"`
	ExecutionLocation           string                    `json:"execution_location"`
	WorkspaceID                 string                    `json:"workspace_id,omitempty"`
	InputRevision               *int64                    `json:"input_revision,omitempty"`
	CurrentRevision             *int64                    `json:"current_revision,omitempty"`
	DesiredRevision             *int64                    `json:"desired_revision,omitempty"`
	WorkspaceCheckpointRevision *int64                    `json:"workspace_checkpoint_revision,omitempty"`
	ApplicationID               string                    `json:"application_id,omitempty"`
	ConversationID              string                    `json:"conversation_id"`
	ModelID                     string                    `json:"model_id"`
	Objective                   string                    `json:"objective"`
	Status                      task.Status               `json:"status"`
	CurrentStep                 *string                   `json:"current_step"`
	StepCount                   int64                     `json:"step_count"`
	CompletedStepCount          int64                     `json:"completed_step_count"`
	Budget                      taskBudgetResponse        `json:"budget"`
	Usage                       taskUsageResponse         `json:"usage"`
	Dependencies                []taskDependencyResponse  `json:"dependencies"`
	ChildJobs                   []taskChildJobResponse    `json:"child_jobs"`
	ResultReview                *taskResultReviewResponse `json:"result_review"`
	CreatedAt                   time.Time                 `json:"created_at"`
	UpdatedAt                   time.Time                 `json:"updated_at"`
}

type taskDependencyResponse struct {
	TaskID           string      `json:"task_id"`
	RequiredRevision int64       `json:"required_revision"`
	TaskStatus       task.Status `json:"task_status"`
	Satisfied        bool        `json:"satisfied"`
}

type taskChildJobResponse struct {
	JobID        string          `json:"job_id"`
	TaskStepID   string          `json:"task_step_id"`
	Title        string          `json:"title"`
	TaskRevision int64           `json:"task_revision"`
	Status       task.StepStatus `json:"status"`
}

type taskResultReviewResponse struct {
	State               string  `json:"state"`
	SourceTaskID        string  `json:"source_task_id"`
	SourceTaskRevision  int64   `json:"source_task_revision"`
	SourceEventSequence int64   `json:"source_event_sequence"`
	ReplyMessageID      *string `json:"reply_message_id"`
}

type taskResponse struct {
	taskSummaryResponse
	EventSequence          int64                             `json:"event_sequence"`
	Result                 *string                           `json:"result"`
	CurrentRevisionStarted *bool                             `json:"current_revision_started,omitempty"`
	ResultMessagePending   bool                              `json:"result_message_pending,omitempty"`
	ResultMessagePublished bool                              `json:"result_message_published,omitempty"`
	WaitingReason          *string                           `json:"waiting_reason"`
	Steps                  []taskStepResponse                `json:"steps"`
	BatchItems             []taskBatchItemResponse           `json:"batch_items"`
	Artifacts              []taskArtifactResponse            `json:"artifacts"`
	PendingRequest         *taskPendingRequestResponse       `json:"pending_request"`
	ClarificationRequest   *taskClarificationRequestResponse `json:"clarification_request"`
	StopRequest            *taskStopRequestResponse          `json:"stop_request"`
	AssistantMessage       *taskAssistantMessageResponse     `json:"assistant_message"`
	FinishedAt             *time.Time                        `json:"finished_at"`
}

type taskFileFetchResponse struct {
	FetchID              string                     `json:"fetch_id"`
	TaskID               string                     `json:"task_id"`
	SourceInstallationID string                     `json:"source_installation_id"`
	SourceDeviceName     string                     `json:"source_device_name"`
	SourceDirectoryName  *string                    `json:"source_directory_name"`
	RelativePath         string                     `json:"relative_path"`
	TaskRevision         int64                      `json:"task_revision"`
	SourceVersion        *string                    `json:"source_version"`
	SourceSHA256         *string                    `json:"source_sha256"`
	MediaType            *string                    `json:"media_type"`
	SizeBytes            *int64                     `json:"size_bytes"`
	ConfirmedBytes       int64                      `json:"confirmed_bytes"`
	Status               task.FileFetchStatus       `json:"status"`
	WaitingReason        *string                    `json:"waiting_reason"`
	FailureReason        *string                    `json:"failure_reason"`
	ContentID            *string                    `json:"content_id"`
	ExpiresAt            *time.Time                 `json:"expires_at"`
	ContentState         task.FileFetchContentState `json:"content_state"`
	ParseStatus          task.FileFetchParseStatus  `json:"parse_status"`
	ParseError           *string                    `json:"parse_error"`
	DeletedAt            *time.Time                 `json:"deleted_at"`
}

type taskFileFetchListResponse struct {
	Items []taskFileFetchResponse `json:"items"`
}

type taskStopRequestResponse struct {
	ID                    string                 `json:"id"`
	Action                task.Action            `json:"action"`
	Status                task.StopRequestStatus `json:"status"`
	SafeAfter             time.Time              `json:"safe_after"`
	UnsettledOperationIDs []string               `json:"unsettled_operation_ids"`
}

type taskAssistantMessageResponse struct {
	ID                  string            `json:"id"`
	ConversationID      string            `json:"conversation_id"`
	Sequence            *int64            `json:"sequence,omitempty"`
	TaskID              string            `json:"task_id"`
	TaskRevision        *int64            `json:"task_revision,omitempty"`
	ResultEventSequence *int64            `json:"result_event_sequence,omitempty"`
	Role                conversation.Role `json:"role"`
	Content             string            `json:"content"`
	Status              string            `json:"status"`
	CreatedAt           time.Time         `json:"created_at"`
}

type taskActionResponse struct {
	TaskID                string      `json:"task_id"`
	Action                string      `json:"action"`
	Status                task.Status `json:"status"`
	UnsettledOperationIDs []string    `json:"unsettled_operation_ids"`
}

type taskWorkspaceViewDeletionRequest struct {
	ConfirmDelete bool `json:"confirm_delete"`
}

type taskWorkspaceViewDeletionResponse struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
}

type taskPendingRequestResponse struct {
	ID                      string `json:"id"`
	Version                 int64  `json:"version"`
	Kind                    string `json:"kind"`
	Title                   string `json:"title"`
	Object                  string `json:"object"`
	Content                 string `json:"content"`
	Impact                  string `json:"impact"`
	MediaPreviewOperationID string `json:"media_preview_operation_id,omitempty"`
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
	ID                string    `json:"id"`
	StepID            string    `json:"step_id"`
	Label             string    `json:"label"`
	Path              string    `json:"path"`
	MediaType         string    `json:"media_type"`
	WorkspaceID       *string   `json:"workspace_id"`
	WorkspaceRevision *int64    `json:"workspace_revision"`
	SizeBytes         *int64    `json:"size_bytes"`
	SHA256            *string   `json:"sha256"`
	Verified          bool      `json:"verified"`
	BatchItemID       *string   `json:"batch_item_id"`
	BatchAttempt      int32     `json:"batch_attempt"`
	CreatedAt         time.Time `json:"created_at"`
}

type submitTaskRequest struct {
	HostID          string             `json:"host_id"`
	WorkspaceID     string             `json:"workspace_id"`
	InputRevision   *int64             `json:"input_revision"`
	MCPConnectorIDs []string           `json:"mcp_connector_ids"`
	ApplicationID   string             `json:"application_id"`
	ModelID         string             `json:"model_id"`
	Objective       string             `json:"objective"`
	Budget          *taskBudgetRequest `json:"budget"`
	BatchItems      []batchItemRequest `json:"batch_items"`
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
	Action                  string          `json:"action"`
	ExpectedStatus          task.Status     `json:"expected_status"`
	ExpectedCurrentRevision json.RawMessage `json:"expected_current_revision"`
	ExpectedDesiredRevision json.RawMessage `json:"expected_desired_revision"`
	BatchItemID             string          `json:"batch_item_id"`
}

func parseOptionalTaskActionRevision(raw json.RawMessage) (*int64, bool) {
	if raw == nil {
		return nil, true
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value < 0 {
		return nil, false
	}
	return &value, true
}

type taskInputRequest struct {
	ClarificationID      string `json:"clarification_id"`
	ClarificationVersion int64  `json:"clarification_version"`
	Answer               string `json:"answer"`
}

type taskApprovalResponseRequest struct {
	Version  int64                `json:"version"`
	Decision task.PendingDecision `json:"decision"`
}

func (h *api) registerTasks(group *gin.RouterGroup, service TaskService) {
	if service == nil {
		return
	}
	group.GET("/tasks", h.listTasks(service))
	group.POST("/tasks", h.submitTask(service))
	group.DELETE("/tasks/:id/mcp-connectors/:connector_id", h.revokeTaskMCPConnectorGrant(service))
	group.GET("/tasks/:id", h.getTask(service))
	if fetchService, ok := service.(TaskFileFetchQueryService); ok {
		group.GET("/tasks/:id/file-fetches/:fetch_id", h.getTaskFileFetch(fetchService))
	}
	group.GET("/tasks/:id/workspace-view-deletions", h.getTaskWorkspaceViewDeletion(service))
	group.POST("/tasks/:id/workspace-view-deletions", h.requestTaskWorkspaceViewDeletion(service))
	group.POST("/tasks/:id/actions", h.taskAction(service))
	group.POST("/tasks/:id/inputs", h.addTaskInput(service))
	group.POST("/tasks/:id/approvals/:approval_id/responses", h.respondToTaskApproval(service))
	group.GET("/tasks/:id/events", h.streamTaskEvents(service))
}

func (h *api) registerTaskWorkbenchObservationTickets(group *gin.RouterGroup, service TaskWorkbenchObservationTicketService) {
	if service == nil {
		return
	}
	group.POST("/tasks/:id/workbench-observation-ticket", h.issueWorkbenchObservationTicket(service))
}

func (h *api) issueWorkbenchObservationTicket(service TaskWorkbenchObservationTicketService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" {
			writeError(c, domain.InvalidArgument("query", "unexpected query parameters"))
			return
		}
		if c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("body", "request body is not allowed"))
			return
		}
		principal, ok := principalFrom(c)
		if !ok || principal.Kind != identity.KindSession {
			unauthenticated(c)
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		value, err := service.IssueWorkbenchObservationTicket(c.Request.Context(), principal, taskID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Header("Referrer-Policy", "no-referrer")
		c.JSON(http.StatusCreated, value)
	}
}

func (h *api) getTaskWorkspaceViewDeletion(service TaskService) gin.HandlerFunc {
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
		deletionService, ok := service.(TaskWorkspaceViewDeletionService)
		if !ok {
			writeError(c, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable"))
			return
		}
		deletion, err := deletionService.GetWorkspaceViewDeletion(c.Request.Context(), principal, taskID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, taskWorkspaceViewDeletionResponse{RequestID: deletion.ID, TaskID: deletion.TaskID, Status: deletion.Status})
	}
}

func (h *api) requestTaskWorkspaceViewDeletion(service TaskService) gin.HandlerFunc {
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
		var request taskWorkspaceViewDeletionRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		deletionService, ok := service.(TaskWorkspaceViewDeletionService)
		if !ok {
			writeError(c, domain.NewError(domain.CodeUnsupported, "task Workspace view deletion is unavailable"))
			return
		}
		deletion, err := deletionService.RequestWorkspaceViewDeletion(c.Request.Context(), principal, taskID, request.ConfirmDelete, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusAccepted
		if deletion.Status == "applied" {
			status = http.StatusOK
		}
		c.JSON(status, taskWorkspaceViewDeletionResponse{RequestID: deletion.ID, TaskID: deletion.TaskID, Status: deletion.Status})
	}
}

func (h *api) revokeTaskMCPConnectorGrant(service TaskService) gin.HandlerFunc {
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
		connectorID, ok := safeIDParam(c, "connector_id", "connector_id", domain.ValidMCPConnectorID)
		if !ok {
			return
		}
		versions := c.QueryArray("expected_grant_version")
		if len(versions) != 1 {
			writeError(c, domain.InvalidArgument("expected_grant_version", "a single positive grant version is required"))
			return
		}
		expectedVersion, err := strconv.ParseInt(versions[0], 10, 64)
		if err != nil || expectedVersion < 1 {
			writeError(c, domain.InvalidArgument("expected_grant_version", "a single positive grant version is required"))
			return
		}
		grantService, ok := service.(TaskMCPConnectorGrantService)
		if !ok {
			writeError(c, domain.NewError(domain.CodeUnsupported, "task MCP connector grants are unavailable"))
			return
		}
		if _, err := grantService.RevokeMCPConnectorGrant(c.Request.Context(), principal, taskID, connectorID, expectedVersion); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
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
		currentRevision, validCurrentRevision := parseOptionalTaskActionRevision(request.ExpectedCurrentRevision)
		desiredRevision, validDesiredRevision := parseOptionalTaskActionRevision(request.ExpectedDesiredRevision)
		if !validCurrentRevision || !validDesiredRevision {
			writeError(c, domain.InvalidArgument("revision", "expected task revisions must be non-negative integers"))
			return
		}
		switch request.Action {
		case "retry_batch_item":
			if !domain.ValidBatchItemID(request.BatchItemID) || request.ExpectedStatus != "" ||
				request.ExpectedCurrentRevision != nil || request.ExpectedDesiredRevision != nil {
				writeError(c, domain.InvalidArgument("action", "retry_batch_item requires only a valid batch_item_id"))
				return
			}
			if err := service.RetryBatchItem(c.Request.Context(), principal, taskID, request.BatchItemID, c.GetHeader("Idempotency-Key")); err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: request.Action, Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
			return
		case "pause", "resume", "cancel":
			action := task.Action(request.Action)
			if request.BatchItemID != "" || !validTaskActionExpectedStatus(action, request.ExpectedStatus) ||
				(request.ExpectedCurrentRevision == nil) != (request.ExpectedDesiredRevision == nil) {
				writeError(c, domain.InvalidArgument("action", "pause, resume, or cancel requires a valid expected_status and no batch_item_id"))
				return
			}
			var revisionFence *app.TaskActionRevisionFence
			if currentRevision != nil {
				revisionFence = &app.TaskActionRevisionFence{
					CurrentRevision: *currentRevision,
					DesiredRevision: *desiredRevision,
				}
			}
			var result app.TaskActionResult
			var err error
			if revisionFence == nil {
				result, err = service.RequestAction(c.Request.Context(), principal, taskID, action, request.ExpectedStatus, c.GetHeader("Idempotency-Key"))
			} else {
				fencedService, ok := service.(TaskRevisionFencedActionService)
				if !ok {
					writeError(c, domain.NewError(domain.CodeUnsupported, "revision-fenced task actions are unavailable"))
					return
				}
				result, err = fencedService.RequestActionWithRevisionFence(c.Request.Context(), principal, taskID, action, request.ExpectedStatus, revisionFence, c.GetHeader("Idempotency-Key"))
			}
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
			writeError(c, domain.InvalidArgument("action", "supported task actions are retry_batch_item, pause, resume, and cancel"))
			return
		}
	}
}

func (h *api) addTaskInput(service TaskService) gin.HandlerFunc {
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
		var request taskInputRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if !domain.ValidPendingRequestID(request.ClarificationID) || request.ClarificationVersion < 1 || !validTaskText(request.Answer, 16384) ||
			strings.ContainsRune(request.Answer, '\x00') || strings.TrimSpace(request.Answer) == "" {
			writeError(c, domain.InvalidArgument("input", "a clarification id, positive version, and answer are required"))
			return
		}
		if err := service.AnswerClarificationRequest(c.Request.Context(), principal, taskID, request.ClarificationID,
			request.ClarificationVersion, request.Answer, c.GetHeader("Idempotency-Key")); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: "answer_clarification", Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
	}
}

func (h *api) respondToTaskApproval(service TaskService) gin.HandlerFunc {
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
		approvalID, ok := safeIDParam(c, "approval_id", "approval_id", domain.ValidPendingRequestID)
		if !ok {
			return
		}
		var request taskApprovalResponseRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if request.Version < 1 || (request.Decision != task.PendingDecisionApprove && request.Decision != task.PendingDecisionReject) {
			writeError(c, domain.InvalidArgument("approval", "a positive version and approve or reject decision are required"))
			return
		}
		if err := service.RespondToPendingRequest(c.Request.Context(), principal, taskID, approvalID, request.Version,
			request.Decision, c.GetHeader("Idempotency-Key")); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, taskActionResponse{TaskID: taskID, Action: "respond", Status: task.StatusQueued, UnsettledOperationIDs: []string{}})
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
		result, err := service.Submit(c.Request.Context(), principal, app.SubmitTaskCommand{
			HostID: request.HostID, WorkspaceID: request.WorkspaceID, InputRevision: request.InputRevision,
			MCPConnectorIDs: request.MCPConnectorIDs,
			ApplicationID:   request.ApplicationID, ModelID: request.ModelID,
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
			view, err := toTaskSummaryResponseWithK2(item, result.K2Projections[item.ID])
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

func (h *api) getTaskFileFetch(service TaskFileFetchQueryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "file fetch status query must not include a query string or body"))
			return
		}
		principal, ok := principalFrom(c)
		if !ok || principal.Kind != identity.KindSession {
			unauthenticated(c)
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		fetchID, ok := safeIDParam(c, "fetch_id", "fetch_id", domain.ValidOperationID)
		if !ok {
			return
		}
		fetch, err := service.GetTaskFileFetch(c.Request.Context(), principal, taskID, fetchID)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toTaskFileFetchResponse(fetch)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusOK, response)
	}
}

func toTaskFileFetchResponse(value app.TaskFileFetch) (taskFileFetchResponse, error) {
	if !domain.ValidOperationID(value.ID) || !domain.ValidTaskID(value.TaskID) || !domain.ValidClientInstallationID(value.SourceInstallationID) ||
		!validTaskText(value.SourceDeviceName, 100) || (value.SourceDirectoryName != "" && !app.ValidTaskFileFetchDirectoryName(value.SourceDirectoryName)) ||
		value.TargetGrantVersion < 1 || value.TaskAuthorizationVersion < 1 ||
		value.TaskRevision < 1 || !validTaskText(value.RelativePath, 1024) || !task.ValidFileFetchStatus(value.Status) ||
		(value.FailureReason != "" && (value.Status != task.FileFetchFailed || !task.ValidFileFetchFailureReason(value.FailureReason))) ||
		!task.ValidFileFetchContentState(value.ContentState) ||
		!task.ValidFileFetchParseStatus(value.ParseStatus) || !app.ValidTaskFileFetchParseResult(value.ParseStatus, value.ParseError) ||
		(value.Status != task.FileFetchCompleted && value.ParseStatus != task.FileFetchParsePending) ||
		value.ConfirmedBytes < 0 || value.SizeBytes < 0 || value.SizeBytes > app.TaskFileFetchMaxFileBytes ||
		value.ConfirmedBytes > value.SizeBytes || (value.SourceVersion != "" && len(value.SourceVersion) != 64) ||
		(value.SourceSHA256 != "" && len(value.SourceSHA256) != 64) ||
		(value.ContentID != "" && !domain.ValidOperationID(value.ContentID)) ||
		((value.ContentState == task.FileFetchContentNone) != (value.ContentID == "")) ||
		((value.ContentState == task.FileFetchContentDeleted) != (value.DeletedAt != nil)) ||
		(value.MediaType != "" && !app.TaskFileFetchMediaTypeValid(value.MediaType)) || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return taskFileFetchResponse{}, domain.NewError(domain.CodeInternal, "Task file fetch status is invalid")
	}
	response := taskFileFetchResponse{
		FetchID: value.ID, TaskID: value.TaskID, SourceInstallationID: value.SourceInstallationID,
		SourceDeviceName: value.SourceDeviceName, SourceDirectoryName: stringPointer(value.SourceDirectoryName),
		RelativePath: value.RelativePath, TaskRevision: value.TaskRevision,
		SourceVersion: stringPointer(value.SourceVersion), SourceSHA256: stringPointer(value.SourceSHA256), MediaType: stringPointer(value.MediaType),
		ConfirmedBytes: value.ConfirmedBytes, Status: value.Status, WaitingReason: stringPointer(value.WaitingReason),
		FailureReason: stringPointer(value.FailureReason),
		ContentID:     stringPointer(value.ContentID), ExpiresAt: value.ExpiresAt, ContentState: value.ContentState, DeletedAt: value.DeletedAt,
		ParseStatus: value.ParseStatus, ParseError: stringPointer(value.ParseError),
	}
	if value.SizeBytes > 0 {
		size := value.SizeBytes
		response.SizeBytes = &size
	}
	return response, nil
}

func toTaskSummaryResponseWithK2(value task.Task, projection app.TaskK2Projection) (taskSummaryResponse, error) {
	initiatorID := value.InitiatorID
	if initiatorID == "" {
		initiatorID = value.OwnerID
	}
	if !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(initiatorID) ||
		(value.HostID != "" && !domain.ValidHostID(value.HostID)) ||
		(value.WorkspaceID != "" && !domain.ValidWorkspaceID(value.WorkspaceID)) ||
		(value.WorkspaceID == "") != (value.InputRevision == nil) ||
		(value.WorkspaceID == "") != (value.WorkspaceCheckpointRevision == nil) ||
		(value.InputRevision != nil && *value.InputRevision < 0) ||
		(value.CurrentRevision == nil) != (value.DesiredRevision == nil) ||
		(value.CurrentRevision != nil && (*value.CurrentRevision < 0 || *value.DesiredRevision < *value.CurrentRevision)) ||
		(value.InputRevision != nil && value.WorkspaceCheckpointRevision != nil && *value.WorkspaceCheckpointRevision < *value.InputRevision) ||
		(value.ExecutionLocation != "server" && value.ExecutionLocation != "legacy") ||
		!domain.ValidConversationID(value.ConversationID) || strings.TrimSpace(value.ModelID) == "" ||
		(value.ApplicationID != "" && !app.ValidHostApplicationID(value.ApplicationID)) || !validTaskStatusResponse(value.Status) ||
		value.StepCount < 0 || value.CompletedStepCount < 0 || value.CompletedStepCount > value.StepCount ||
		value.Budget.Validate() != nil || value.Usage.ActiveSeconds < 0 || value.Usage.ToolCalls < 0 || (value.Usage.ModelTokens != nil && *value.Usage.ModelTokens < 0) ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return taskSummaryResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	response := taskSummaryResponse{
		ID: value.ID, OwnerID: value.OwnerID, InitiatorID: initiatorID, HostID: value.HostID, ExecutionLocation: value.ExecutionLocation,
		WorkspaceID: value.WorkspaceID, InputRevision: value.InputRevision, CurrentRevision: value.CurrentRevision,
		DesiredRevision: value.DesiredRevision, WorkspaceCheckpointRevision: value.WorkspaceCheckpointRevision,
		ApplicationID: value.ApplicationID, ConversationID: value.ConversationID,
		ModelID: value.ModelID, Objective: value.Objective, Status: value.Status, CurrentStep: stringPointer(value.CurrentStep),
		StepCount: value.StepCount, CompletedStepCount: value.CompletedStepCount,
		Budget: taskBudgetResponse(value.Budget), Usage: taskUsageResponse{
			ActiveSeconds: value.Usage.ActiveSeconds, ToolCalls: value.Usage.ToolCalls, ModelTokens: value.Usage.ModelTokens,
		}, Dependencies: make([]taskDependencyResponse, 0), ChildJobs: make([]taskChildJobResponse, 0),
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	for _, dependency := range projection.Dependencies {
		if !domain.ValidTaskID(dependency.TaskID) || dependency.RequiredRevision < 0 || !validTaskStatusResponse(dependency.TaskStatus) {
			return taskSummaryResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.Dependencies = append(response.Dependencies, taskDependencyResponse{
			TaskID: dependency.TaskID, RequiredRevision: dependency.RequiredRevision,
			TaskStatus: dependency.TaskStatus, Satisfied: dependency.Satisfied,
		})
	}
	for _, job := range projection.ChildJobs {
		if !utf8.ValidString(job.JobID) || utf8.RuneCountInString(job.JobID) < 1 || utf8.RuneCountInString(job.JobID) > 128 ||
			!domain.ValidStepID(job.TaskStepID) || !utf8.ValidString(job.Title) || job.TaskRevision < 0 || !validStepStatus(job.Status) {
			return taskSummaryResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.ChildJobs = append(response.ChildJobs, taskChildJobResponse{
			JobID: job.JobID, TaskStepID: job.TaskStepID, Title: job.Title, TaskRevision: job.TaskRevision, Status: job.Status,
		})
	}
	if review := projection.ResultReview; review != nil {
		if !validTaskResultReviewState(review.State) || !domain.ValidTaskID(review.SourceTaskID) || review.SourceTaskRevision < 0 ||
			review.SourceEventSequence < 1 || (review.ReplyMessageID != nil && !domain.ValidMessageID(*review.ReplyMessageID)) {
			return taskSummaryResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.ResultReview = &taskResultReviewResponse{
			State: review.State, SourceTaskID: review.SourceTaskID, SourceTaskRevision: review.SourceTaskRevision,
			SourceEventSequence: review.SourceEventSequence, ReplyMessageID: review.ReplyMessageID,
		}
	}
	return response, nil
}

func toTaskResponse(value app.TaskDetails) (taskResponse, error) {
	taskSummary := value.Task
	taskSummary.StepCount, taskSummary.CompletedStepCount = task.StepCounts(value.Steps)
	summary, err := toTaskSummaryResponseWithK2(taskSummary, app.TaskK2Projection{
		Dependencies: value.Dependencies, ChildJobs: value.ChildJobs, ResultReview: value.ResultReview,
	})
	if err != nil {
		return taskResponse{}, err
	}
	if value.Task.EventSequence < 0 || (value.CurrentRevisionStarted != nil && value.Task.CurrentRevision == nil) {
		return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	response := taskResponse{
		taskSummaryResponse: summary, EventSequence: value.Task.EventSequence,
		Result: stringPointer(value.Task.Result), CurrentRevisionStarted: value.CurrentRevisionStarted, ResultMessagePending: value.ResultMessagePending,
		ResultMessagePublished: value.ResultMessagePublished, WaitingReason: stringPointer(value.Task.WaitingReason),
		Steps: make([]taskStepResponse, 0, len(value.Steps)), BatchItems: make([]taskBatchItemResponse, 0, len(value.BatchItems)),
		Artifacts: make([]taskArtifactResponse, 0, len(value.Artifacts)), FinishedAt: value.Task.FinishedAt,
	}
	if assistant := value.AssistantMessage; assistant != nil {
		message := assistant.Message
		status := string(assistant.Status)
		if !domain.ValidMessageID(message.ID) || !domain.ValidConversationID(message.ConversationID) ||
			message.ConversationID != value.Task.ConversationID || message.TaskID != value.Task.ID ||
			(message.Sequence < 1 && status == "completed") || message.Sequence < 0 || message.Role != conversation.RoleAssistant || len(message.Content) == 0 ||
			len(message.Content) > app.MaxAssistantMessageBytes || !utf8.ValidString(message.Content) ||
			strings.ContainsRune(message.Content, '\x00') || message.CreatedAt.IsZero() ||
			(assistant.TaskRevision != nil && *assistant.TaskRevision < 0) ||
			(assistant.ResultEventSequence != nil && (*assistant.ResultEventSequence < 1 || assistant.TaskRevision == nil)) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		switch status {
		case "streaming", "completed", "interrupted":
		default:
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.AssistantMessage = &taskAssistantMessageResponse{
			ID: message.ID, ConversationID: message.ConversationID, Sequence: positiveInt64Pointer(message.Sequence), TaskID: message.TaskID,
			TaskRevision: assistant.TaskRevision, ResultEventSequence: assistant.ResultEventSequence,
			Role: message.Role, Content: message.Content, Status: status, CreatedAt: message.CreatedAt,
		}
	}
	if pending := value.PendingRequest; pending != nil && pending.Status == task.PendingRequestPending {
		if value.Task.Status != task.StatusWaitingUser || !domain.ValidPendingRequestID(pending.ID) || pending.TaskID != value.Task.ID || pending.Version < 1 || pending.Kind != "approval" ||
			(pending.MediaPreviewOperationID != "" && !domain.ValidOperationID(pending.MediaPreviewOperationID)) ||
			!validTaskText(pending.Title, 512) || !validTaskText(pending.Object, 512) || !validTaskText(pending.Content, 512) || !validTaskText(pending.Impact, 512) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.PendingRequest = &taskPendingRequestResponse{
			ID: pending.ID, Version: pending.Version, Kind: pending.Kind, Title: pending.Title,
			Object: pending.Object, Content: pending.Content, Impact: pending.Impact,
			MediaPreviewOperationID: pending.MediaPreviewOperationID,
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
		proofPresent := artifact.WorkspaceID != "" || artifact.Revision != 0 || artifact.SizeBytes != 0 || artifact.SHA256 != ""
		if proofPresent && (!domain.ValidWorkspaceID(artifact.WorkspaceID) || artifact.WorkspaceID != value.Task.WorkspaceID ||
			artifact.Revision < 0 || artifact.SizeBytes < 0 || !validWorkspaceSyncSHA256(artifact.SHA256)) {
			return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if artifact.BatchItemID != "" {
			if _, exists := batchItemIDs[artifact.BatchItemID]; !exists {
				return taskResponse{}, domain.NewError(domain.CodeInternal, "internal error")
			}
		}
		var workspaceRevision, sizeBytes *int64
		workspaceID, sha256 := stringPointer(artifact.WorkspaceID), stringPointer(artifact.SHA256)
		if proofPresent {
			revision, size := artifact.Revision, artifact.SizeBytes
			workspaceRevision, sizeBytes = &revision, &size
		}
		response.Artifacts = append(response.Artifacts, taskArtifactResponse{
			ID: artifact.ID, StepID: artifact.StepID, Label: artifact.Label, Path: artifact.Path,
			MediaType: artifact.MediaType, WorkspaceID: workspaceID, WorkspaceRevision: workspaceRevision, SizeBytes: sizeBytes,
			SHA256: sha256, Verified: proofPresent,
			BatchItemID: stringPointer(artifact.BatchItemID), BatchAttempt: artifact.BatchAttempt, CreatedAt: artifact.CreatedAt,
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
	return decodeStrictJSON(c, target)
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

func validTaskResultReviewState(value string) bool {
	switch value {
	case "pending", "processing", "published", "failed":
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
	case "plan", "model", "tool", "wait", "clarification", "budget", "verification", "verify", "batch_item", "batch_result", "subagent", "progress", "result":
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
