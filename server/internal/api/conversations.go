package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type ConversationService interface {
	Create(context.Context, identity.Principal, app.CreateConversationCommand) (conversation.Conversation, bool, error)
	List(context.Context, identity.Principal, app.PageRequest) (app.ConversationPage, error)
	Get(context.Context, identity.Principal, string) (conversation.Conversation, error)
	ListMessages(context.Context, identity.Principal, string, app.PageRequest) (app.MessagePage, error)
}

type conversationResponse struct {
	ID        string    `json:"id"`
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

type messageResponse struct {
	ID             string            `json:"id"`
	ConversationID string            `json:"conversation_id"`
	Role           conversation.Role `json:"role"`
	Content        string            `json:"content"`
	TaskID         string            `json:"task_id"`
	CreatedAt      time.Time         `json:"created_at"`
}

type messagePageResponse struct {
	Items      []messageResponse `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

type appendMessageRequest struct {
	HostID          string             `json:"host_id"`
	WorkspaceID     string             `json:"workspace_id"`
	InputRevision   *int64             `json:"input_revision"`
	MCPConnectorIDs []string           `json:"mcp_connector_ids"`
	ApplicationID   string             `json:"application_id"`
	ModelID         string             `json:"model_id"`
	Content         string             `json:"content"`
	Budget          *taskBudgetRequest `json:"budget"`
}

type appendMessageResponse struct {
	Message messageResponse `json:"message"`
	Task    taskResponse    `json:"task"`
}

func (h *api) registerConversations(group *gin.RouterGroup, conversations ConversationService, tasks TaskService) {
	if conversations != nil {
		group.GET("/conversations", h.listConversations(conversations))
		group.POST("/conversations", h.createConversation(conversations))
		group.GET("/conversations/:id", h.getConversation(conversations))
		group.GET("/conversations/:id/messages", h.listConversationMessages(conversations))
	}
	if tasks != nil {
		group.POST("/conversations/:id/messages", h.appendConversationMessage(tasks))
	}
}

func (h *api) createConversation(service ConversationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request createConversationRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		value, replayed, err := service.Create(c.Request.Context(), principal, app.CreateConversationCommand{
			HostID: request.HostID, Title: request.Title, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toConversationResponse(value)
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
		result, err := service.List(c.Request.Context(), principal, page)
		if err != nil {
			writeError(c, err)
			return
		}
		response := conversationPageResponse{Items: make([]conversationResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			view, err := toConversationResponse(item)
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
		response, err := toConversationResponse(value)
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
		result, err := service.ListMessages(c.Request.Context(), principal, id, page)
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

func (h *api) appendConversationMessage(service TaskService) gin.HandlerFunc {
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
		var request appendMessageRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		budget, ok := requestBudget(c, request.Budget)
		if !ok {
			return
		}
		result, err := service.AppendMessage(c.Request.Context(), principal, id, app.AppendMessageCommand{
			HostID: request.HostID, WorkspaceID: request.WorkspaceID, InputRevision: request.InputRevision,
			MCPConnectorIDs: request.MCPConnectorIDs,
			ApplicationID:   request.ApplicationID, ModelID: request.ModelID, Content: request.Content,
			Budget: budget, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			writeError(c, err)
			return
		}
		message, err := toMessageResponse(result.Message)
		if err != nil {
			writeError(c, err)
			return
		}
		taskValue, err := toTaskResponse(app.TaskDetails{Task: result.Task})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, appendMessageResponse{Message: message, Task: taskValue})
	}
}

func toConversationResponse(value conversation.Conversation) (conversationResponse, error) {
	if !domain.ValidConversationID(value.ID) || (value.HostID != "" && !domain.ValidHostID(value.HostID)) || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return conversationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return conversationResponse{ID: value.ID, HostID: value.HostID, Title: value.Title, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}, nil
}

func toMessageResponse(value conversation.Message) (messageResponse, error) {
	if !domain.ValidMessageID(value.ID) || !domain.ValidConversationID(value.ConversationID) ||
		!domain.ValidTaskID(value.TaskID) || (value.Role != conversation.RoleUser && value.Role != conversation.RoleAssistant) || value.CreatedAt.IsZero() {
		return messageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return messageResponse{
		ID: value.ID, ConversationID: value.ConversationID, Role: value.Role, Content: value.Content,
		TaskID: value.TaskID, CreatedAt: value.CreatedAt,
	}, nil
}
