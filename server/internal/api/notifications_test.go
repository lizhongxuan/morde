package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
)

type notificationAPITestService struct {
	page      app.NotificationPage
	principal identity.Principal
	markedID  string
}

func (s *notificationAPITestService) List(_ context.Context, principal identity.Principal, _ bool, _ app.PageRequest) (app.NotificationPage, error) {
	s.principal = principal
	return s.page, nil
}

func (s *notificationAPITestService) MarkRead(_ context.Context, principal identity.Principal, id string) (notification.Notification, error) {
	s.principal, s.markedID = principal, id
	return notification.Notification{ID: id}, nil
}

func TestNotificationHTTPRoutesUseSessionAndSafeResponses(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	notices := &notificationAPITestService{page: app.NotificationPage{Items: []notification.Notification{{
		ID: "n1234567", OwnerID: testUserID, TaskID: "q1234567", EventSequence: 2,
		Kind: notification.KindApprovalRequired, Summary: "任务需要你处理", RequestID: "private-request-id", RequestVersion: 8,
		Actionable: true, CreatedAt: now,
	}}}}
	services := newServices()
	deps := services.deps()
	deps.Notifications = notices
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	listed := requestWithCookie(router, http.MethodGet, "/api/notifications?unread_only=true", cookie, "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "private-request-id") ||
		strings.Contains(listed.Body.String(), "request_version") || !strings.Contains(listed.Body.String(), "任务需要你处理") ||
		notices.principal.Kind != identity.KindSession {
		t.Fatalf("notification response exposed internal request data or lost session: status=%d body=%s principal=%+v", listed.Code, listed.Body, notices.principal)
	}
	marked := requestWithCookieBody(router, http.MethodPost, "/api/notifications/n1234567/read", `{}`, cookie, "https://portal.example.test")
	if marked.Code != http.StatusNoContent || notices.markedID != "n1234567" || notices.principal.Kind != identity.KindSession {
		t.Fatalf("mark notification read status=%d id=%q principal=%+v body=%s", marked.Code, notices.markedID, notices.principal, marked.Body)
	}
	invalid := requestWithCookie(router, http.MethodGet, "/api/notifications?unread_only=yes", cookie, "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid notification filter status=%d body=%s", invalid.Code, invalid.Body)
	}
}
