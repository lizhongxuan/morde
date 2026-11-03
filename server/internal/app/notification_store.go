package app

import (
	"context"

	"morde/server/internal/domain/notification"
)

type NotificationPage struct {
	Items      []notification.Notification
	NextCursor string
}

// NotificationStore consumes durable task events and stores owner-scoped notices.
type NotificationStore interface {
	ListUnprocessedNotificationEvents(context.Context, int32) ([]notification.TaskEvent, error)
	ConsumeTaskNotificationEvent(context.Context, notification.TaskEvent, *notification.Notification, bool) (bool, error)
	ListUnprocessedNotificationSourceEvents(context.Context, int32) ([]notification.SourceEvent, error)
	ConsumeNotificationSourceEvent(context.Context, notification.SourceEvent, notification.Notification) (bool, error)
	ListNotifications(context.Context, string, bool, PageRequest) (NotificationPage, error)
	MarkNotificationRead(context.Context, string, string) (notification.Notification, error)
}
