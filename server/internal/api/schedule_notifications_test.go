package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleAPITestService struct {
	value         schedule.Schedule
	createCommand app.CreateScheduleCommand
	createKey     string
	createCalls   int
	updateCommand app.UpdateScheduleCommand
	updateCalls   int
}

func (s *scheduleAPITestService) Create(_ context.Context, principal identity.Principal, command app.CreateScheduleCommand, key string) (schedule.Schedule, error) {
	s.createCalls++
	s.createCommand, s.createKey = command, key
	s.value = schedule.Schedule{
		ID: "j1234567", OwnerID: principal.UserID, Version: 1, Status: schedule.StatusEnabled, HostID: command.HostID,
		ModelID: command.ModelID, Objective: command.Objective, Timing: command.Timing, Budget: command.Budget,
		MisfirePolicy: command.MisfirePolicy, NotificationEvents: command.NotificationEvents, Observation: command.Observation,
		NextOccurrenceKey: "once:2026-11-01T05:30:00Z", NextRunAt: timePointer(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)),
		NextCheckAt: timePointer(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), ExecutionEpoch: 1,
	}
	return s.value, nil
}
func (s *scheduleAPITestService) List(context.Context, identity.Principal, app.ScheduleListRequest) (app.SchedulePage, error) {
	return app.SchedulePage{Items: []schedule.Schedule{s.value}, NextCursor: "next"}, nil
}
func (s *scheduleAPITestService) Get(context.Context, identity.Principal, string) (schedule.Schedule, error) {
	return s.value, nil
}
func (s *scheduleAPITestService) Update(_ context.Context, _ identity.Principal, _ string, command app.UpdateScheduleCommand) (schedule.Schedule, error) {
	s.updateCalls++
	s.updateCommand = command
	if command.ObservationSet {
		s.value.Observation = command.Observation
	}
	return s.value, nil
}
func (s *scheduleAPITestService) Action(context.Context, identity.Principal, string, app.ScheduleActionCommand, string) (app.ScheduleActionResult, error) {
	return app.ScheduleActionResult{Schedule: s.value}, nil
}
func (s *scheduleAPITestService) Runs(context.Context, identity.Principal, string, app.PageRequest) (app.ScheduleRunPage, error) {
	return app.ScheduleRunPage{Items: []schedule.Run{}}, nil
}

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

func TestScheduleAndNotificationHTTPRoutesUseSessionAndSafeResponses(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	schedules := &scheduleAPITestService{}
	notices := &notificationAPITestService{page: app.NotificationPage{Items: []notification.Notification{{
		ID: "n1234567", OwnerID: testUserID, TaskID: "q1234567", EventSequence: 2,
		Kind: notification.KindApprovalRequired, Summary: "任务需要你处理", RequestID: "private-request-id", RequestVersion: 8,
		Actionable: true, CreatedAt: now,
	}}}}
	services := newServices()
	deps := services.deps()
	deps.Schedules, deps.Notifications = schedules, notices
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	post := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(`{"host_id":"h1234567","model_id":"p1234567","objective":"check changes","timing":{"kind":"once","run_at":"2026-11-01T01:30:00-04:00","time_zone":"America/New_York"},"budget":{"active_seconds":60,"tool_calls":2,"model_tokens":1000},"misfire_policy":"run_once","notification_events":["failed"],"observation":{"url":"https://example.test/status","selector":"#status","field":"text"}}`))
	post.Host = "portal.example.test"
	post.RemoteAddr = "192.0.2.1:12345"
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Origin", "https://portal.example.test")
	post.Header.Set("Idempotency-Key", "schedule-create-001")
	post.AddCookie(cookie)
	created := httptest.NewRecorder()
	router.ServeHTTP(created, post)
	if created.Code != http.StatusCreated || schedules.createCalls != 1 || schedules.createKey != "schedule-create-001" ||
		schedules.createCommand.Timing.TimeZone != "America/New_York" || schedules.createCommand.Budget != (task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 1000}) ||
		schedules.createCommand.Observation == nil || schedules.createCommand.Observation.Selector != "#status" {
		t.Fatalf("create schedule status=%d calls=%d key=%q command=%#v body=%s", created.Code, schedules.createCalls, schedules.createKey, schedules.createCommand, created.Body)
	}
	var scheduleBody map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &scheduleBody); err != nil || scheduleBody["id"] != "j1234567" {
		t.Fatalf("schedule response=%v err=%v", scheduleBody, err)
	}
	updated := requestWithCookieBody(router, http.MethodPatch, "/api/schedules/j1234567", `{"expected_version":1,"observation":{"url":"https://example.test/status","selector":".status","field":"value"}}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || schedules.updateCalls != 1 || !schedules.updateCommand.ObservationSet || schedules.updateCommand.Observation == nil || schedules.updateCommand.Observation.Field != schedule.ObservationValue {
		t.Fatalf("update observation status=%d calls=%d command=%#v body=%s", updated.Code, schedules.updateCalls, schedules.updateCommand, updated.Body)
	}
	cleared := requestWithCookieBody(router, http.MethodPatch, "/api/schedules/j1234567", `{"expected_version":1,"observation":null}`, cookie, "https://portal.example.test")
	if cleared.Code != http.StatusOK || schedules.updateCalls != 2 || !schedules.updateCommand.ObservationSet || schedules.updateCommand.Observation != nil {
		t.Fatalf("clear observation status=%d calls=%d command=%#v body=%s", cleared.Code, schedules.updateCalls, schedules.updateCommand, cleared.Body)
	}
	invalidObservation := requestWithCookieBody(router, http.MethodPatch, "/api/schedules/j1234567", `{"expected_version":1,"observation":{"url":"https://example.test/status","selector":"#status","field":"text","script":"alert(1)"}}`, cookie, "https://portal.example.test")
	if invalidObservation.Code != http.StatusBadRequest || schedules.updateCalls != 2 {
		t.Fatalf("unexpected observation property status=%d calls=%d body=%s", invalidObservation.Code, schedules.updateCalls, invalidObservation.Body)
	}

	listed := requestWithCookie(router, http.MethodGet, "/api/schedules?status=enabled&limit=10", cookie, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"items":[{"id":"j1234567"`) {
		t.Fatalf("list schedules status=%d body=%s", listed.Code, listed.Body)
	}
	notificationList := requestWithCookie(router, http.MethodGet, "/api/notifications?unread_only=true", cookie, "")
	if notificationList.Code != http.StatusOK || strings.Contains(notificationList.Body.String(), "private-request-id") ||
		strings.Contains(notificationList.Body.String(), "request_version") || !strings.Contains(notificationList.Body.String(), "任务需要你处理") || notices.principal.Kind != identity.KindSession {
		t.Fatalf("notification response exposed internal request data or lost session: status=%d body=%s principal=%+v", notificationList.Code, notificationList.Body, notices.principal)
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

func timePointer(value time.Time) *time.Time { return &value }
