package app

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
)

const (
	defaultNotificationPageSize int32 = 50
	maxNotificationPageSize     int32 = 100
	maxNotificationEventBatch   int32 = 100
)

type NotificationService struct {
	store NotificationStore
}

func NewNotificationService(store NotificationStore) (*NotificationService, error) {
	if store == nil {
		return nil, errors.New("notification store is required")
	}
	return &NotificationService{store: store}, nil
}

func (s *NotificationService) ProcessPending(ctx context.Context, limit int32) (int, error) {
	if ctx == nil {
		return 0, errors.New("notification processing context is required")
	}
	if limit == 0 {
		limit = maxNotificationEventBatch
	}
	if limit < 1 || limit > maxNotificationEventBatch {
		return 0, domain.InvalidArgument("limit", "notification event batch size is out of range")
	}
	events, err := s.store.ListUnprocessedNotificationEvents(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list unprocessed task notification events: %w", err)
	}
	processed := 0
	for _, event := range events {
		id := ""
		if event.NotificationEnabled && (event.Type == "task.result" || event.Type == "task.waiting_user") {
			id, err = notification.NewID()
			if err != nil {
				return processed, err
			}
		}
		effect, err := notification.DeriveEventEffect(event, id)
		if err != nil {
			return processed, fmt.Errorf("derive notification from task event: %w", err)
		}
		consumed, err := s.store.ConsumeTaskNotificationEvent(ctx, event, effect.Notification, effect.ClosePending)
		if err != nil {
			return processed, fmt.Errorf("consume task notification event %s/%d: %w", event.TaskID, event.Sequence, err)
		}
		if consumed {
			processed++
		}
	}
	return processed, nil
}

func (s *NotificationService) List(ctx context.Context, principal identity.Principal, unreadOnly bool, page PageRequest) (NotificationPage, error) {
	if err := validateNotificationPrincipal(principal); err != nil {
		return NotificationPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultNotificationPageSize
	}
	if page.Limit < 1 || page.Limit > maxNotificationPageSize || len(page.Cursor) > 1024 || !utf8.ValidString(page.Cursor) {
		return NotificationPage{}, domain.InvalidArgument("page", "notification page request is out of range")
	}
	result, err := s.store.ListNotifications(ctx, principal.UserID, unreadOnly, page)
	if err != nil {
		return NotificationPage{}, err
	}
	if result.Items == nil {
		result.Items = []notification.Notification{}
	}
	return result, nil
}

func (s *NotificationService) MarkRead(ctx context.Context, principal identity.Principal, id string) (notification.Notification, error) {
	if err := validateNotificationPrincipal(principal); err != nil {
		return notification.Notification{}, err
	}
	if !notification.ValidID(id) {
		return notification.Notification{}, domain.NewError(domain.CodeNotFound, "notification not found")
	}
	return s.store.MarkNotificationRead(ctx, principal.UserID, id)
}

func validateNotificationPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidUserID(principal.UserID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}
