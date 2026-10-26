package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/task"
)

type notificationStoreFake struct {
	events        []notification.TaskEvent
	page          NotificationPage
	marked        notification.Notification
	listEventsErr error
	consumeErr    error
	listErr       error
	markErr       error
	limit         int32
	ownerID       string
	pageRequest   PageRequest
	unreadOnly    bool
	markedOwner   string
	markedID      string
	consumed      []notification.TaskEvent
	effects       []notification.EventEffect
}

func (s *notificationStoreFake) ListUnprocessedNotificationEvents(_ context.Context, limit int32) ([]notification.TaskEvent, error) {
	s.limit = limit
	return append([]notification.TaskEvent(nil), s.events...), s.listEventsErr
}

func (s *notificationStoreFake) ConsumeTaskNotificationEvent(_ context.Context, event notification.TaskEvent, value *notification.Notification, closePending bool) (bool, error) {
	if s.consumeErr != nil {
		return false, s.consumeErr
	}
	s.consumed = append(s.consumed, event)
	s.effects = append(s.effects, notification.EventEffect{Notification: value, ClosePending: closePending})
	return true, nil
}

func (s *notificationStoreFake) ListNotifications(_ context.Context, ownerID string, unreadOnly bool, page PageRequest) (NotificationPage, error) {
	s.ownerID, s.unreadOnly, s.pageRequest = ownerID, unreadOnly, page
	return s.page, s.listErr
}

func (s *notificationStoreFake) MarkNotificationRead(_ context.Context, ownerID, id string) (notification.Notification, error) {
	s.markedOwner, s.markedID = ownerID, id
	return s.marked, s.markErr
}

func notificationPrincipal(kind identity.Kind) identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: kind, TokenID: "t1234567"}
}

func TestNotificationServiceProcessesTaskEventsWithoutCopyingPayload(t *testing.T) {
	created := time.Now().UTC()
	store := &notificationStoreFake{events: []notification.TaskEvent{
		{OwnerID: "u1234567", TaskID: "q1234567", Sequence: 3, Type: "task.result", Payload: []byte(`{"result":"private result"}`), TaskStatus: task.StatusCompleted, NotificationEnabled: true, CreatedAt: created},
		{OwnerID: "u1234567", TaskID: "q2234567", Sequence: 4, Type: "task.waiting_user", Payload: []byte(`{"detail":{"pending_request_id":"r1234567","pending_request_version":2,"pending_request_kind":"approval","approval":{"content":"private content"}}}`), TaskStatus: task.StatusWaitingUser, NotificationEnabled: true, CreatedAt: created},
		{OwnerID: "u1234567", TaskID: "q3234567", Sequence: 5, Type: "task.response", Payload: []byte(`{"decision":"approve"}`), TaskStatus: task.StatusQueued, CreatedAt: created},
	}}
	service, err := NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessPending(context.Background(), 25)
	if err != nil || processed != 3 || store.limit != 25 {
		t.Fatalf("ProcessPending() = %d, %v; limit=%d", processed, err, store.limit)
	}
	if len(store.effects) != 3 || store.effects[0].Notification == nil || store.effects[0].Notification.Kind != notification.KindCompleted ||
		store.effects[1].Notification == nil || store.effects[1].Notification.Kind != notification.KindApprovalRequired ||
		!store.effects[1].Notification.Actionable || store.effects[2].Notification != nil || !store.effects[2].ClosePending {
		t.Fatalf("processed effects = %#v", store.effects)
	}
	if got := store.effects[1].Notification.Summary; got == "" || got == "private content" || got == "private result" {
		t.Fatalf("unsafe summary = %q", got)
	}
}

func TestNotificationServiceRetriesStoreErrors(t *testing.T) {
	wantErr := errors.New("database unavailable")
	store := &notificationStoreFake{
		events:     []notification.TaskEvent{{OwnerID: "u1234567", TaskID: "q1234567", Sequence: 1, Type: "task.result", Payload: []byte(`{}`), TaskStatus: task.StatusCompleted, NotificationEnabled: true, CreatedAt: time.Now()}},
		consumeErr: wantErr,
	}
	service, err := NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessPending(context.Background(), 1)
	if processed != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("ProcessPending() = %d, %v", processed, err)
	}
	if _, err := service.ProcessPending(context.Background(), 101); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("out of range batch error = %v", err)
	}
}

func TestNotificationServiceConsumesUnsubscribedEventsWithoutCreatingNotice(t *testing.T) {
	store := &notificationStoreFake{events: []notification.TaskEvent{{
		OwnerID: "u1234567", TaskID: "q1234567", Sequence: 2, Type: "task.result",
		Payload: []byte(`{"status":"completed"}`), TaskStatus: task.StatusCompleted,
		NotificationEnabled: false, CreatedAt: time.Now(),
	}}}
	service, err := NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessPending(context.Background(), 1)
	if err != nil || processed != 1 || len(store.effects) != 1 || store.effects[0].Notification != nil || !store.effects[0].ClosePending {
		t.Fatalf("unsubscribed result processing = %d effects %#v err=%v", processed, store.effects, err)
	}
}

func TestNotificationServiceListsAndMarksReadForSessionOwner(t *testing.T) {
	value := notification.Notification{ID: "n1234567", OwnerID: "u1234567", TaskID: "q1234567", EventSequence: 1,
		Kind: notification.KindCompleted, Summary: "任务已完成", CreatedAt: time.Now()}
	store := &notificationStoreFake{page: NotificationPage{Items: []notification.Notification{value}}, marked: value}
	service, err := NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), notificationPrincipal(identity.KindSession), true, PageRequest{})
	if err != nil || !reflect.DeepEqual(page.Items, []notification.Notification{value}) || store.ownerID != "u1234567" || !store.unreadOnly || store.pageRequest.Limit != defaultNotificationPageSize {
		t.Fatalf("List() = %#v, %v; owner=%q page=%#v", page, err, store.ownerID, store.pageRequest)
	}
	read, err := service.MarkRead(context.Background(), notificationPrincipal(identity.KindSession), value.ID)
	if err != nil || read.ID != value.ID || store.markedOwner != "u1234567" || store.markedID != value.ID {
		t.Fatalf("MarkRead() = %#v, %v; owner=%q id=%q", read, err, store.markedOwner, store.markedID)
	}
	if _, err := service.List(context.Background(), notificationPrincipal(identity.KindAPIKey), false, PageRequest{}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API Key was allowed to list notifications: %v", err)
	}
	if _, err := service.MarkRead(context.Background(), notificationPrincipal(identity.KindSession), "bad"); domain.CodeOf(err) != domain.CodeNotFound || store.markedID != value.ID {
		t.Fatalf("invalid ID error=%v, store ID=%q", err, store.markedID)
	}
}

func TestNotificationServiceRejectsInvalidPageAndContext(t *testing.T) {
	service, err := NewNotificationService(&notificationStoreFake{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), notificationPrincipal(identity.KindSession), false, PageRequest{Limit: 101}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid page error = %v", err)
	}
}
