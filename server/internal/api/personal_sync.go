package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalSyncService interface {
	GetSnapshot(context.Context, identity.Principal, string, int32) (app.PersonalSnapshotPage, error)
	GetChanges(context.Context, identity.Principal, int64, *int64, int32) (personal.ChangePage, error)
}

type personalSnapshotResponse struct {
	Watermark  int64                    `json:"watermark"`
	Items      []personalObjectResponse `json:"items"`
	NextCursor *string                  `json:"next_cursor"`
}

type personalChangePageResponse struct {
	Items             []personal.Change `json:"items"`
	ThroughSequence   int64             `json:"through_sequence"`
	NextAfterSequence int64             `json:"next_after_sequence"`
	HasMore           bool              `json:"has_more"`
}

func registerPersonalSyncRoutes(group *gin.RouterGroup, service personalSyncService) {
	group.GET("/sync/snapshot", func(c *gin.Context) {
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
		page, err := service.GetSnapshot(c.Request.Context(), principal, cursor, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		response := personalSnapshotResponse{Watermark: page.Watermark, Items: make([]personalObjectResponse, 0, len(page.Items)), NextCursor: cursorPointer(page.NextCursor)}
		for _, item := range page.Items {
			response.Items = append(response.Items, personalObjectResponse{
				ObjectType: item.ObjectType, ObjectID: item.ObjectID, Revision: item.Revision, Body: item.Body,
			})
		}
		c.JSON(http.StatusOK, response)
	})
	group.GET("/sync/changes", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		after, ok := requiredPersonalSyncSequence(c, "after_sequence")
		if !ok {
			return
		}
		through, err := optionalPersonalSyncSequence(c, "through_sequence")
		if err != nil {
			writeError(c, err)
			return
		}
		limit, err := optionalWorkspaceSyncInt(c, "limit", 100, 1, 100)
		if err != nil {
			writeError(c, err)
			return
		}
		page, err := service.GetChanges(c.Request.Context(), principal, after, through, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		if page.Items == nil {
			page.Items = []personal.Change{}
		}
		c.JSON(http.StatusOK, personalChangePageResponse{
			Items: page.Items, ThroughSequence: page.ThroughSequence,
			NextAfterSequence: page.NextAfterSequence, HasMore: page.HasMore,
		})
	})
}

func requiredPersonalSyncSequence(c *gin.Context, key string) (int64, bool) {
	value, ok := singleQuery(c, key)
	if !ok {
		return 0, false
	}
	if value == "" {
		writeError(c, domain.InvalidArgument(key, "query parameter is required"))
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		writeError(c, domain.InvalidArgument(key, "query parameter is invalid"))
		return 0, false
	}
	return parsed, true
}

func optionalPersonalSyncSequence(c *gin.Context, key string) (*int64, error) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, domain.InvalidArgument(key, "query parameter must appear once")
	}
	parsed, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || parsed < 0 {
		return nil, domain.InvalidArgument(key, "query parameter is invalid")
	}
	return &parsed, nil
}
