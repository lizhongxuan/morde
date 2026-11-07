package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func (h *api) retryConversationMessageProcessing(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !requiredMessageActor(c, principal) {
			return
		}
		instanceID, ok := requiredMessageServiceInstance(c)
		if !ok {
			return
		}
		projectID, conversationID := "", c.Param("id")
		if c.Param("conversation_id") != "" {
			projectID, conversationID = c.Param("id"), c.Param("conversation_id")
			if !project.ValidID(projectID) {
				writeError(c, domain.NewError(domain.CodeNotFound, "project not found"))
				return
			}
		}
		if !domain.ValidConversationID(conversationID) {
			writeError(c, domain.NewError(domain.CodeNotFound, "conversation not found"))
			return
		}
		messageID, ok := safeIDParam(c, "message_id", "message_id", domain.ValidMessageID)
		if !ok || !personalAttachmentBodyIsEmpty(c) {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if err := service.RetryConversationMessageProcessing(c.Request.Context(), principal, projectID, conversationID, messageID, instanceID, key); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	}
}
