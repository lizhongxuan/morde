package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
)

type notificationPageResponse struct {
	Items      []notificationResponse `json:"items"`
	NextCursor *string                `json:"next_cursor"`
}

type notificationResponse struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	TaskID        *string    `json:"task_id"`
	EventSequence int64      `json:"event_sequence"`
	Summary       *string    `json:"summary"`
	Redacted      bool       `json:"redacted"`
	Actionable    bool       `json:"actionable"`
	CreatedAt     time.Time  `json:"created_at"`
	ReadAt        *time.Time `json:"read_at"`
}

func (h *api) registerNotifications(group *gin.RouterGroup, service NotificationService) {
	if service == nil {
		return
	}
	group.GET("/notifications", h.listNotifications(service))
	group.POST("/notifications/:id/read", h.markNotificationRead(service))
}

func (h *api) listNotifications(service NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		unreadOnly := false
		if raw := c.Query("unread_only"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(c, domain.InvalidArgument("unread_only", "unread_only must be a boolean"))
				return
			}
			unreadOnly = value
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, unreadOnly, page)
		if err != nil {
			writeError(c, err)
			return
		}
		response := notificationPageResponse{Items: make([]notificationResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			value, err := toNotificationResponse(principal, item)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, value)
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) markNotificationRead(service NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", notification.ValidID)
		if !ok {
			return
		}
		if _, err := service.MarkRead(c.Request.Context(), principal, id); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func toNotificationResponse(principal identity.Principal, item notification.Notification) (notificationResponse, error) {
	if !notification.ValidID(item.ID) || !domain.ValidTaskID(item.TaskID) || item.OwnerID != principal.UserID || item.EventSequence < 1 || item.CreatedAt.IsZero() {
		return notificationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	kind := "waiting_user"
	switch item.Kind {
	case notification.KindCompleted:
		kind = "completed"
	case notification.KindFailed:
		kind = "failed"
	case notification.KindApprovalRequired, notification.KindClarification, notification.KindAttentionRequired:
	default:
		return notificationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	summary := item.Summary
	redacted := summary == ""
	response := notificationResponse{
		ID: item.ID, Kind: kind, EventSequence: item.EventSequence, Actionable: item.Actionable,
		CreatedAt: item.CreatedAt, ReadAt: item.ReadAt, Redacted: redacted,
	}
	if redacted {
		response.Summary = nil
		response.Actionable = false
	} else {
		response.Summary = &summary
	}
	if item.TaskID != "" {
		taskID := item.TaskID
		response.TaskID = &taskID
	}
	return response, nil
}
