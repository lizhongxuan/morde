package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type personalMemoryHistoryService interface {
	GetMemoryHistory(context.Context, identity.Principal, string, string, string, int32) (app.PersonalMemoryHistoryPage, error)
}

type personalMemoryHistoryPageResponse struct {
	Items      []personalMemoryRevisionResponse `json:"items"`
	NextCursor *string                          `json:"next_cursor"`
}

type personalMemoryRevisionResponse struct {
	Revision     int64                             `json:"revision"`
	ChangedAt    time.Time                         `json:"changed_at"`
	Body         json.RawMessage                   `json:"body"`
	Restorable   bool                              `json:"restorable"`
	SourceStatus string                            `json:"source_status"`
	Sources      []personalMemoryHistorySourceView `json:"sources"`
}

type personalMemoryHistorySourceView struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func RegisterPersonalMemoryHistoryRoute(group *gin.RouterGroup, service personalMemoryHistoryService) {
	group.GET("/:type/:id/history", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		cursor, ok := workspaceSyncQueryText(c, "cursor", 2048, false)
		if !ok {
			return
		}
		limit, err := optionalWorkspaceSyncInt(c, "limit", 100, 1, 100)
		if err != nil {
			writeError(c, err)
			return
		}
		page, err := service.GetMemoryHistory(c.Request.Context(), principal, c.Param("type"), c.Param("id"), cursor, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]personalMemoryRevisionResponse, 0, len(page.Items))
		for _, revision := range page.Items {
			sources := make([]personalMemoryHistorySourceView, 0, len(revision.Sources))
			for _, source := range revision.Sources {
				sources = append(sources, personalMemoryHistorySourceView{
					Kind: source.Kind, ID: source.ID, ConversationID: source.ConversationID,
				})
			}
			items = append(items, personalMemoryRevisionResponse{
				Revision: revision.Revision, ChangedAt: revision.ChangedAt, Body: revision.Body,
				Restorable: revision.Restorable, SourceStatus: revision.SourceStatus, Sources: sources,
			})
		}
		c.JSON(http.StatusOK, personalMemoryHistoryPageResponse{Items: items, NextCursor: cursorPointer(page.NextCursor)})
	})
}
