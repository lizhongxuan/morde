package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

const personalHistoryImportMaxBodyBytes int64 = 8 << 20

type PersonalHistoryImportService interface {
	Import(context.Context, identity.Principal, conversation.HistoryImport) (conversation.HistoryImportResult, error)
}

type personalHistoryImportMessageResponse struct {
	SourceMessageID string            `json:"source_message_id"`
	MessageID       string            `json:"message_id"`
	ConversationID  string            `json:"conversation_id"`
	Role            conversation.Role `json:"role"`
	Content         string            `json:"content"`
	Sequence        int64             `json:"sequence"`
	AttachmentIDs   []string          `json:"attachment_ids"`
	Replay          bool              `json:"replay"`
}

type personalHistoryImportResponse struct {
	ConversationID string                                 `json:"conversation_id"`
	Title          string                                 `json:"title"`
	Replay         bool                                   `json:"replay"`
	Messages       []personalHistoryImportMessageResponse `json:"messages"`
}

// RegisterPersonalHistoryImportRoutes registers the opt-in history import
// endpoint. It is intentionally separate from Task submission routes.
func RegisterPersonalHistoryImportRoutes(group *gin.RouterGroup, service PersonalHistoryImportService) {
	group.POST("/history/import", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, personalHistoryImportMaxBodyBytes)
		var input conversation.HistoryImport
		if !decodeStrictJSON(c, &input) {
			return
		}
		result, err := service.Import(c.Request.Context(), principal, input)
		if err != nil {
			writeError(c, err)
			return
		}
		response := personalHistoryImportResponse{
			ConversationID: result.Conversation.ID,
			Title:          result.Conversation.Title,
			Replay:         result.Replay,
			Messages:       make([]personalHistoryImportMessageResponse, 0, len(result.Messages)),
		}
		for _, imported := range result.Messages {
			message := imported.Message
			response.Messages = append(response.Messages, personalHistoryImportMessageResponse{
				SourceMessageID: imported.SourceMessageID,
				MessageID:       message.ID,
				ConversationID:  message.ConversationID,
				Role:            message.Role,
				Content:         message.Content,
				Sequence:        message.Sequence,
				AttachmentIDs:   imported.AttachmentIDs,
				Replay:          imported.Replay,
			})
		}
		c.JSON(http.StatusOK, response)
	})
}
