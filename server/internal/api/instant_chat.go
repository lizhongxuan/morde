package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type InstantChatService interface {
	List(context.Context, identity.Principal) ([]app.InstantChatConversation, error)
	Messages(context.Context, identity.Principal, string) ([]app.InstantChatMessage, error)
	Send(context.Context, identity.Principal, app.InstantChatSend) (app.InstantChatReply, error)
}

type instantChatSendRequest struct {
	ConversationID string `json:"conversation_id"`
	ModelID        string `json:"model_id"`
	Content        string `json:"content"`
}

type instantChatConversationResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	ModelID   string    `json:"model_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type instantChatMessageResponse struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"created_at"`
}

func (h *api) registerInstantChat(group *gin.RouterGroup) {
	group.GET("/instant-chat/conversations", h.listInstantChatConversations)
	group.GET("/instant-chat/conversations/:id/messages", h.listInstantChatMessages)
	group.POST("/instant-chat/messages", h.sendInstantChatMessage)
}

func (h *api) listInstantChatConversations(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	if h.deps.InstantChat == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.InstantChat.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := make([]instantChatConversationResponse, 0, len(items))
	for _, item := range items {
		response = append(response, instantChatConversationResponse{ID: item.ID, Title: item.Title,
			ModelID: item.ModelID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": response})
}

func (h *api) listInstantChatMessages(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidConversationID)
	if !ok {
		return
	}
	if h.deps.InstantChat == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.InstantChat.Messages(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response := make([]instantChatMessageResponse, 0, len(items))
	for _, item := range items {
		response = append(response, instantChatMessageResponse{ID: item.ID, ConversationID: item.ConversationID,
			Role: item.Role, Content: item.Content, CreatedAt: item.CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": response})
}

func (h *api) sendInstantChatMessage(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	if h.deps.InstantChat == nil {
		serviceUnavailable(c)
		return
	}
	var request instantChatSendRequest
	if !decodeStrictModelJSON(c, &request) {
		return
	}
	result, err := h.deps.InstantChat.Send(c.Request.Context(), principal, app.InstantChatSend{
		ConversationID: request.ConversationID, ModelID: request.ModelID,
		Content: request.Content, RequestKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"conversation": instantChatConversationResponse{ID: result.Conversation.ID, Title: result.Conversation.Title,
			ModelID: result.Conversation.ModelID, CreatedAt: result.Conversation.CreatedAt, UpdatedAt: result.Conversation.UpdatedAt},
		"message": instantChatMessageResponse{ID: result.Message.ID, ConversationID: result.Message.ConversationID,
			Role: result.Message.Role, Content: result.Message.Content, CreatedAt: result.Message.CreatedAt},
		"reply": instantChatMessageResponse{ID: result.Reply.ID, ConversationID: result.Reply.ConversationID,
			Role: result.Reply.Role, Content: result.Reply.Content, CreatedAt: result.Reply.CreatedAt},
	})
}

var _ InstantChatService = (*app.TaskChatCompatibilityService)(nil)
