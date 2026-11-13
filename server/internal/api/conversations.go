package api

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type ConversationService interface {
	Create(context.Context, identity.Principal, app.CreateConversationCommand) (conversation.Conversation, bool, error)
	List(context.Context, identity.Principal, app.PageRequest) (app.ConversationPage, error)
	Get(context.Context, identity.Principal, string) (conversation.Conversation, error)
	ListMessages(context.Context, identity.Principal, string, app.PageRequest) (app.MessagePage, error)
	GetMain(context.Context, identity.Principal) (conversation.MainConversation, error)
	EnsureMain(context.Context, identity.Principal, app.EnsureMainConversationCommand) (conversation.MainConversation, bool, error)
	SetMain(context.Context, identity.Principal, app.SetMainConversationCommand) (conversation.MainConversation, bool, error)
}

type conversationResponse struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	IsMain    bool      `json:"is_main"`
	HostID    string    `json:"host_id,omitempty"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type conversationPageResponse struct {
	Items      []conversationResponse `json:"items"`
	NextCursor *string                `json:"next_cursor"`
}

type createConversationRequest struct {
	HostID string `json:"host_id"`
	Title  string `json:"title"`
}

type setMainConversationRequest struct {
	ConversationID  string `json:"conversation_id"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type mainConversationResponse struct {
	Status         conversation.MainConversationStatus `json:"status"`
	ConversationID string                              `json:"conversation_id"`
	Version        int64                               `json:"version"`
}

type messageResponse struct {
	ID                        string                            `json:"id"`
	ConversationID            string                            `json:"conversation_id"`
	Sequence                  int64                             `json:"sequence"`
	Role                      conversation.Role                 `json:"role"`
	Content                   string                            `json:"content"`
	TaskID                    string                            `json:"task_id,omitempty"`
	DuplicateOfMessageID      string                            `json:"duplicate_of_message_id,omitempty"`
	RelatedTaskID             string                            `json:"related_task_id,omitempty"`
	AttachmentIDs             []string                          `json:"attachment_ids,omitempty"`
	ClientMessageID           string                            `json:"client_message_id,omitempty"`
	RoutingStatus             conversation.RoutingStatus        `json:"routing_status,omitempty"`
	RoutingFailureReason      conversation.RoutingFailureReason `json:"routing_failure_reason,omitempty"`
	Sources                   []conversation.MessageSource      `json:"sources,omitempty"`
	DeliveryStatus            conversation.DeliveryStatus       `json:"delivery_status"`
	OutputStatus              conversation.OutputStatus         `json:"output_status,omitempty"`
	OutputTaskRevision        *int64                            `json:"output_task_revision,omitempty"`
	OutputResultEventSequence *int64                            `json:"output_result_event_sequence,omitempty"`
	CreatedAt                 time.Time                         `json:"created_at"`
}

type messagePageResponse struct {
	Items      []messageResponse `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

type conversationMessageRequest struct {
	ClientMessageID         string             `json:"client_message_id"`
	PreviousClientMessageID string             `json:"previous_client_message_id,omitempty"`
	ReferenceTaskID         string             `json:"reference_task_id,omitempty"`
	HostID                  string             `json:"host_id,omitempty"`
	WorkspaceID             string             `json:"workspace_id,omitempty"`
	ChangeTaskTarget        bool               `json:"change_task_target,omitempty"`
	MCPConnectorIDs         []string           `json:"mcp_connector_ids,omitempty"`
	ClientAttachmentIDs     []string           `json:"client_attachment_ids,omitempty"`
	ApplicationID           string             `json:"application_id,omitempty"`
	ModelID                 string             `json:"model_id,omitempty"`
	TimeZone                string             `json:"time_zone,omitempty"`
	Content                 string             `json:"content"`
	Budget                  *taskBudgetRequest `json:"budget,omitempty"`
}

type conversationMessageResponseEnvelope struct {
	Message messageResponse `json:"message"`
}

type acceptedConversationMessageResponse struct {
	Message  messageResponse `json:"message"`
	Replayed bool            `json:"replayed"`
}

func requiredMessageServiceInstance(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("X-Morde-Service-Instance-ID")
	if len(values) != 1 || !domain.ValidServiceInstanceID(values[0]) {
		writeError(c, domain.InvalidArgument("service_instance_id", "a valid service instance header is required"))
		return "", false
	}
	return values[0], true
}

// A persisted outbox must only be submitted under the account that created it.
// The authenticated principal is resolved for this request, so a cookie change
// in another tab cannot redirect a pending message into a different account.
func requiredMessageActor(c *gin.Context, principal identity.Principal) bool {
	values := c.Request.Header.Values("X-Morde-Actor-ID")
	if len(values) != 1 || !domain.ValidUserID(values[0]) {
		writeError(c, domain.InvalidArgument("actor_id", "a valid actor header is required"))
		return false
	}
	if values[0] != principal.UserID {
		writeError(c, domain.NewError(domain.CodeForbidden, "pending message belongs to a different account"))
		return false
	}
	return true
}

func optionalMessageActor(c *gin.Context, principal identity.Principal) bool {
	if len(c.Request.Header.Values("X-Morde-Actor-ID")) == 0 && len(c.Request.Header.Values("X-Morde-Service-Instance-ID")) == 0 {
		return true
	}
	if !requiredMessageActor(c, principal) {
		return false
	}
	instanceID, ok := requiredMessageServiceInstance(c)
	if !ok {
		return false
	}
	c.Request = c.Request.WithContext(app.WithExpectedServiceInstance(c.Request.Context(), instanceID))
	return true
}

func optionalOutboxCreationScope(c *gin.Context, principal identity.Principal) (string, bool) {
	if len(c.Request.Header.Values("X-Morde-Actor-ID")) == 0 && len(c.Request.Header.Values("X-Morde-Service-Instance-ID")) == 0 {
		return "", true
	}
	if !requiredMessageActor(c, principal) {
		return "", false
	}
	return requiredMessageServiceInstance(c)
}

type conversationActiveTaskResponse struct {
	taskSummaryResponse
	EventSequence int64 `json:"event_sequence"`
}

type conversationActiveTaskPageResponse struct {
	Items        []conversationActiveTaskResponse `json:"items"`
	NextCursor   *string                          `json:"next_cursor"`
	PendingCount int64                            `json:"pending_count"`
}

func (h *api) registerConversations(group *gin.RouterGroup, conversations ConversationService, tasks TaskService) {
	if conversations != nil {
		group.GET("/conversations", h.listConversations(conversations))
		group.POST("/conversations", h.createConversation(conversations))
		group.GET("/conversations/:id", h.getConversation(conversations))
		group.GET("/conversations/:id/messages", h.listConversationMessages(conversations))
		group.GET("/me/main-conversation", h.getMainConversation(conversations))
		group.POST("/me/main-conversation/ensure", h.ensureMainConversation(conversations))
		group.PUT("/me/main-conversation", h.setMainConversation(conversations))
	}
	if tasks != nil {
		group.POST("/conversations/:id/messages", h.acceptConversationMessage(tasks))
		group.PUT("/conversations/:id/messages/:message_id/attachments/:client_attachment_id", h.associateConversationMessageAttachment(tasks))
		group.POST("/conversations/:id/messages/:message_id/retry-processing", h.retryConversationMessageProcessing(tasks))
		group.POST("/projects/:id/conversations/:conversation_id/messages/:message_id/retry-processing", h.retryConversationMessageProcessing(tasks))
		group.GET("/conversations/:id/tasks", h.listConversationActiveTasks(tasks))
		registerConversationMessageUploads(group, tasks)
	}
}

func (h *api) getMainConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		value, err := service.GetMain(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toMainConversationResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) ensureMainConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if c.Request.ContentLength != 0 {
			var request struct{}
			if !decodeStrictTaskJSON(c, &request) {
				return
			}
		}
		value, _, err := service.EnsureMain(c.Request.Context(), principal, app.EnsureMainConversationCommand{IdempotencyKey: key})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toMainConversationResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) setMainConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		var request setMainConversationRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "expected_version is required"))
			return
		}
		value, _, err := service.SetMain(c.Request.Context(), principal, app.SetMainConversationCommand{
			ConversationID: request.ConversationID, ExpectedVersion: *request.ExpectedVersion, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toMainConversationResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) createConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		instanceID, ok := optionalOutboxCreationScope(c, principal)
		if !ok {
			return
		}
		var request createConversationRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		value, replayed, err := service.Create(c.Request.Context(), principal, app.CreateConversationCommand{
			HostID: request.HostID, Title: request.Title, IdempotencyKey: c.GetHeader("Idempotency-Key"), ServiceInstanceID: instanceID,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		mainValue, err := service.GetMain(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toConversationResponse(value, mainValue)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		c.JSON(status, response)
	}
}

func (h *api) listConversations(service ConversationService) gin.HandlerFunc {
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
		page.Search = c.Query("q")
		result, err := service.List(c.Request.Context(), principal, page)
		if err != nil {
			writeError(c, err)
			return
		}
		mainValue, err := service.GetMain(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response := conversationPageResponse{Items: make([]conversationResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			view, err := toConversationResponse(item, mainValue)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, view)
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) getConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidConversationID)
		if !ok {
			return
		}
		value, err := service.Get(c.Request.Context(), principal, id)
		if err != nil {
			writeError(c, err)
			return
		}
		mainValue, err := service.GetMain(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toConversationResponse(value, mainValue)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) listConversationMessages(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidConversationID)
		if !ok {
			return
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		var result app.MessagePage
		var err error
		if clientID := c.Query("client_message_id"); clientID != "" {
			if !requiredMessageActor(c, principal) {
				return
			}
			instanceID, ok := requiredMessageServiceInstance(c)
			if !ok {
				return
			}
			lookup, ok := service.(interface {
				FindMessageByClientID(context.Context, identity.Principal, string, string, string) (app.MessagePage, error)
			})
			if !ok {
				serviceUnavailable(c)
				return
			}
			result, err = lookup.FindMessageByClientID(c.Request.Context(), principal, id, clientID, instanceID)
		} else {
			result, err = service.ListMessages(c.Request.Context(), principal, id, page)
		}
		if err != nil {
			writeError(c, err)
			return
		}
		response := messagePageResponse{Items: make([]messageResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			view, err := toMessageResponse(item)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, view)
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) listConversationActiveTasks(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidConversationID)
		if !ok {
			return
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		view := app.ConversationTaskView(c.DefaultQuery("view", string(app.ConversationTaskViewActive)))
		if view != app.ConversationTaskViewActive && view != app.ConversationTaskViewRecent {
			writeError(c, domain.InvalidArgument("view", "conversation task view is invalid"))
			return
		}
		result, err := service.ListConversationTasks(c.Request.Context(), principal, "", id, view, page)
		if err != nil {
			writeError(c, err)
			return
		}
		writeConversationActiveTasks(c, result)
	}
}

func writeConversationActiveTasks(c *gin.Context, page app.TaskPage) {
	response := conversationActiveTaskPageResponse{Items: make([]conversationActiveTaskResponse, 0, len(page.Items)), NextCursor: cursorPointer(page.NextCursor), PendingCount: page.PendingCount}
	for _, value := range page.Items {
		summary, err := toTaskSummaryResponseWithK2(value, page.K2Projections[value.ID])
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, conversationActiveTaskResponse{taskSummaryResponse: summary, EventSequence: value.EventSequence})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) acceptConversationMessage(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !requiredMessageActor(c, principal) {
			return
		}
		conversationID, ok := safeID(c, "id", domain.ValidConversationID)
		if !ok {
			return
		}
		var request conversationMessageRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		instanceID, ok := requiredMessageServiceInstance(c)
		if !ok {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if request.ClientMessageID == "" || request.ClientMessageID != key || !utf8.ValidString(request.Content) ||
			strings.ContainsRune(request.Content, '\x00') || utf8.RuneCountInString(request.Content) > 32768 ||
			(strings.TrimSpace(request.Content) == "" && len(request.ClientAttachmentIDs) == 0) {
			writeError(c, domain.InvalidArgument("message", "message id and text or attachment are required"))
			return
		}
		budget, ok := requestBudget(c, request.Budget)
		if !ok {
			return
		}
		c.Request = c.Request.WithContext(app.WithExpectedServiceInstance(c.Request.Context(), instanceID))
		message, replayed, err := service.AcceptConversationMessage(c.Request.Context(), principal, conversationID, app.AcceptConversationMessageCommand{
			ServiceInstanceID: instanceID, ClientMessageID: request.ClientMessageID,
			PreviousClientMessageID: request.PreviousClientMessageID, ReferenceTaskID: request.ReferenceTaskID,
			HostID: request.HostID, WorkspaceID: request.WorkspaceID, ChangeTaskTarget: request.ChangeTaskTarget,
			MCPConnectorIDs:     request.MCPConnectorIDs,
			ClientAttachmentIDs: request.ClientAttachmentIDs, ApplicationID: request.ApplicationID,
			ModelID: request.ModelID, TimeZone: request.TimeZone, Content: request.Content, Budget: budget, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		if message.ConversationID != conversationID || message.Role != conversation.RoleUser ||
			message.ClientMessageID != request.ClientMessageID || message.Content != request.Content {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		view, err := toMessageResponse(message)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, acceptedConversationMessageResponse{Message: view, Replayed: replayed})
	}
}

func toConversationResponse(value conversation.Conversation, main conversation.MainConversation) (conversationResponse, error) {
	if !domain.ValidConversationID(value.ID) || (value.HostID != "" && !domain.ValidHostID(value.HostID)) || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return conversationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if err := main.Validate(); err != nil {
		return conversationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return conversationResponse{ID: value.ID, Scope: "personal", IsMain: main.Status == conversation.MainConversationReady && main.ConversationID == value.ID,
		HostID: value.HostID, Title: value.Title, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}, nil
}

func toMainConversationResponse(value conversation.MainConversation) (mainConversationResponse, error) {
	if err := value.Validate(); err != nil {
		return mainConversationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return mainConversationResponse{Status: value.Status, ConversationID: value.ConversationID, Version: value.Version}, nil
}

func toMessageResponse(value conversation.Message) (messageResponse, error) {
	return toScopedMessageResponse(value, false)
}

func toScopedMessageResponse(value conversation.Message, projectScope bool) (messageResponse, error) {
	deliveryStatus := value.DeliveryStatus
	if deliveryStatus == "" {
		deliveryStatus = conversation.DeliveryApplied
	}
	if !domain.ValidMessageID(value.ID) || !domain.ValidConversationID(value.ConversationID) ||
		(value.TaskID != "" && !domain.ValidTaskID(value.TaskID)) ||
		(value.DuplicateOfMessageID != "" && (!domain.ValidMessageID(value.DuplicateOfMessageID) || value.DuplicateOfMessageID == value.ID ||
			value.Role != conversation.RoleUser || value.TaskID != "")) ||
		(value.RelatedTaskID != "" && (value.DuplicateOfMessageID == "" || !domain.ValidTaskID(value.RelatedTaskID))) ||
		(value.Role != conversation.RoleUser && value.Role != conversation.RoleAssistant) || value.Sequence < 1 || value.CreatedAt.IsZero() ||
		!deliveryStatus.Valid() || (value.RoutingStatus != "" && !value.RoutingStatus.Valid()) ||
		(value.RoutingFailureReason != "" && !value.RoutingFailureReason.Valid()) {
		return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if value.OutputStatus != "" {
		if !value.OutputStatus.Valid() || value.Role != conversation.RoleAssistant || value.TaskID == "" ||
			value.OutputTaskRevision == nil || *value.OutputTaskRevision < 0 ||
			(value.OutputStatus == conversation.OutputCompleted) != (value.OutputResultEventSequence != nil) ||
			(value.OutputResultEventSequence != nil && *value.OutputResultEventSequence < 1) {
			return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
	} else if value.OutputTaskRevision != nil || value.OutputResultEventSequence != nil {
		return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	for _, attachmentID := range value.AttachmentIDs {
		if (!projectScope && !personal.ValidPersonalAttachmentID(attachmentID)) ||
			(projectScope && !app.ValidProjectConversationAttachmentID(attachmentID)) {
			return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
	}
	if len(value.Sources) > 64 || (len(value.Sources) > 0 && value.Role != conversation.RoleAssistant) {
		return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	for _, source := range value.Sources {
		if !domain.ValidMessageID(source.UserMessageID) || !domain.ValidTaskID(source.TaskID) ||
			source.TaskRevision < 0 || source.EventSequence < 1 || strings.TrimSpace(source.Content) == "" ||
			len(source.Content) > app.MaxAssistantMessageBytes || !utf8.ValidString(source.Content) || strings.ContainsRune(source.Content, '\x00') {
			return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
	}
	return messageResponse{
		ID: value.ID, ConversationID: value.ConversationID, Sequence: value.Sequence, Role: value.Role, Content: value.Content,
		TaskID: value.TaskID, DuplicateOfMessageID: value.DuplicateOfMessageID, RelatedTaskID: value.RelatedTaskID,
		AttachmentIDs:   append([]string(nil), value.AttachmentIDs...),
		ClientMessageID: value.ClientMessageID, RoutingStatus: value.RoutingStatus, RoutingFailureReason: value.RoutingFailureReason,
		Sources:        append([]conversation.MessageSource(nil), value.Sources...),
		DeliveryStatus: deliveryStatus, OutputStatus: value.OutputStatus, OutputTaskRevision: value.OutputTaskRevision,
		OutputResultEventSequence: value.OutputResultEventSequence, CreatedAt: value.CreatedAt,
	}, nil
}
