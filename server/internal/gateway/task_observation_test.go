package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

type testTaskWorkbenchObservationService struct {
	mu                    sync.Mutex
	consumed              bool
	revoked               bool
	eventCalls            int
	terminalEvents        bool
	revokeAfterFirstEvent bool
	cursorExpired         bool
}

func (s *testTaskWorkbenchObservationService) ConsumeWorkbenchObservationTicket(_ context.Context, rawTicket, hostID string, generation int64) (string, app.TaskWorkbenchObservationSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rawTicket != "opaque-ticket" || hostID != testHostID || generation != 9 || s.consumed {
		return "", app.TaskWorkbenchObservationSession{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	s.consumed = true
	return "opaque-task-read-cookie", app.TaskWorkbenchObservationSession{
		TaskID: "q1234567", HostID: hostID, ActorID: testUserID, RuntimeGeneration: generation,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}, nil
}

func (s *testTaskWorkbenchObservationService) ValidateWorkbenchObservationSession(_ context.Context, rawCookie, hostID string, generation int64) (app.TaskWorkbenchObservationSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rawCookie != "opaque-task-read-cookie" || hostID != testHostID || generation != 9 || s.revoked {
		return app.TaskWorkbenchObservationSession{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return app.TaskWorkbenchObservationSession{TaskID: "q1234567", HostID: hostID, ActorID: testUserID,
		RuntimeGeneration: generation, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (s *testTaskWorkbenchObservationService) GetWorkbenchObservationSnapshot(_ context.Context, rawCookie, hostID string, generation int64) (app.TaskWorkbenchObservationSnapshot, error) {
	if rawCookie != "opaque-task-read-cookie" || hostID != testHostID || generation != 9 {
		return app.TaskWorkbenchObservationSnapshot{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return app.TaskWorkbenchObservationSnapshot{
		TaskID: "q1234567", HostID: hostID, Status: task.StatusRunning, EventSequence: 5,
		UpdatedAt: time.Now().UTC(), Artifacts: []app.TaskWorkbenchObservationArtifact{},
	}, nil
}

func (s *testTaskWorkbenchObservationService) ListWorkbenchObservationEvents(_ context.Context, rawCookie, hostID string, generation, after int64, _ int32) (app.TaskWorkbenchObservationEventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventCalls++
	if rawCookie != "opaque-task-read-cookie" || hostID != testHostID || generation != 9 || s.revoked && s.eventCalls > 1 {
		return app.TaskWorkbenchObservationEventPage{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	if s.cursorExpired {
		return app.TaskWorkbenchObservationEventPage{TaskID: "q1234567", Status: task.StatusRunning, CurrentSequence: 6},
			domain.NewError(domain.CodeEventCursorExpired, "event cursor expired")
	}
	status := task.StatusRunning
	items := []app.TaskWorkbenchObservationEvent{}
	sequence := int64(1)
	if after < sequence {
		items = append(items, app.TaskWorkbenchObservationEvent{Sequence: sequence, Type: "task.status", CreatedAt: time.Now().UTC()})
	}
	if s.terminalEvents {
		status = task.StatusCompleted
	}
	if s.revokeAfterFirstEvent && s.eventCalls == 1 {
		s.revoked = true
	}
	return app.TaskWorkbenchObservationEventPage{TaskID: "q1234567", Status: status, CurrentSequence: sequence, Items: items}, nil
}

func taskObservationTestGateway(t *testing.T, service *testTaskWorkbenchObservationService) (*gatewayHandler, *gateway, *testRuntime) {
	t.Helper()
	runtime := &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning, RuntimeGeneration: 9}}
	handler, g := newTestGateway(t, runtime, &testVerifier{}, &testHostService{}, func(deps *Deps) {
		deps.TaskObservation = service
	})
	return handler, g, runtime
}

func TestTaskObservationTicketExchangeSetsIsolatedCookieAndCleansURL(t *testing.T) {
	service := &testTaskWorkbenchObservationService{}
	handler, _, _ := taskObservationTestGateway(t, service)
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/task-auth?ticket=opaque-ticket", nil, "", "")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/__ch/wb/?task_id=q1234567" {
		t.Fatalf("exchange response = %d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("exchange security headers = %#v", response.Header())
	}
	cookies := response.Header().Values("Set-Cookie")
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0], taskReadCookieName+"=opaque-task-read-cookie") ||
		!strings.Contains(cookies[0], "Path=/__ch/") || !strings.Contains(cookies[0], "HttpOnly") ||
		!strings.Contains(cookies[0], "Secure") || !strings.Contains(cookies[0], "SameSite=Strict") || strings.Contains(cookies[0], hostCookieName+"=") {
		t.Fatalf("isolated TaskRead cookie = %#v", cookies)
	}
	replay := request(handler, http.MethodGet, hostAuthority(), "/__ch/task-auth?ticket=opaque-ticket", nil, "", "")
	if replay.Code != http.StatusNotFound {
		t.Fatalf("ticket replay status = %d, want 404", replay.Code)
	}
}

func TestTaskReadCookieOnlyAllowsWorkbenchAssetsAndTaskReadRoutes(t *testing.T) {
	service := &testTaskWorkbenchObservationService{terminalEvents: true}
	handler, g, _ := taskObservationTestGateway(t, service)
	assets := filepath.Join(g.deps.Config.WorkbenchDir, "assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index-12345678.js"), []byte("safe asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskCookie := taskReadCookieName + "=opaque-task-read-cookie"
	index := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/?task_id=q1234567", nil, taskCookie, "")
	if index.Code != http.StatusOK || index.Header().Get("Cache-Control") != "no-store" || index.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("TaskRead SPA = %d headers=%#v body=%s", index.Code, index.Header(), index.Body)
	}
	asset := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/assets/index-12345678.js", nil, taskCookie, "")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "safe asset") || asset.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("TaskRead static asset = %d headers=%#v body=%s", asset.Code, asset.Header(), asset.Body)
	}
	unknownStatic := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/private-route", nil, taskCookie, "")
	if unknownStatic.Code != http.StatusNotFound {
		t.Fatalf("TaskRead arbitrary SPA path status = %d, want 404", unknownStatic.Code)
	}
	wrongTask := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/?task_id=q7654321", nil, taskCookie, "")
	if wrongTask.Code != http.StatusNotFound {
		t.Fatalf("TaskRead wrong task page status = %d, want 404", wrongTask.Code)
	}
	snapshot := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/tasks/q1234567", nil, taskCookie, "")
	if snapshot.Code != http.StatusOK || !strings.Contains(snapshot.Body.String(), `"event_sequence":5`) || strings.Contains(snapshot.Body.String(), "objective") {
		t.Fatalf("TaskRead snapshot = %d %s", snapshot.Code, snapshot.Body)
	}
	events := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/tasks/q1234567/events?after=0", nil, taskCookie, "")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), "event: task.event") || !strings.Contains(events.Body.String(), `"sequence":1`) || strings.Contains(events.Body.String(), "payload") {
		t.Fatalf("TaskRead SSE = %d %s", events.Code, events.Body)
	}
	control := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/control", nil, taskCookie, "")
	if control.Code != http.StatusNotFound {
		t.Fatalf("TaskRead cookie reached Host control API: %d %s", control.Code, control.Body)
	}
}

func TestTaskObservationEventPollStopsWhenAuthorizationIsRevoked(t *testing.T) {
	service := &testTaskWorkbenchObservationService{revokeAfterFirstEvent: true}
	handler, _, _ := taskObservationTestGateway(t, service)
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/tasks/q1234567/events?after=0", nil,
		taskReadCookieName+"=opaque-task-read-cookie", "")
	service.mu.Lock()
	calls := service.eventCalls
	service.mu.Unlock()
	if response.Code != http.StatusOK || calls < 2 || !strings.Contains(response.Body.String(), "event: task.event") {
		t.Fatalf("revoked event stream = %d calls=%d %s", response.Code, calls, response.Body)
	}
}

func TestTaskObservationEventCursorExpiryRequestsFreshSnapshot(t *testing.T) {
	service := &testTaskWorkbenchObservationService{cursorExpired: true}
	handler, _, _ := taskObservationTestGateway(t, service)
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/tasks/q1234567/events?after=0", nil,
		taskReadCookieName+"=opaque-task-read-cookie", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: task.snapshot_required") ||
		!strings.Contains(response.Body.String(), `"current_sequence":6`) || strings.Contains(response.Body.String(), "payload") {
		t.Fatalf("expired observation cursor = %d %s", response.Code, response.Body)
	}
}
