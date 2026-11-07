package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
)

func (h *api) associateConversationMessageAttachment(service TaskService) gin.HandlerFunc {
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
		conversationID, ok := safeID(c, "id", domain.ValidConversationID)
		if !ok {
			return
		}
		messageID, ok := safeIDParam(c, "message_id", "message_id", domain.ValidMessageID)
		if !ok {
			return
		}
		var request struct {
			AttachmentID string `json:"attachment_id"`
		}
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if err := service.AssociateConversationMessageAttachment(c.Request.Context(), principal, conversationID, messageID,
			c.Param("client_attachment_id"), request.AttachmentID, instanceID); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
