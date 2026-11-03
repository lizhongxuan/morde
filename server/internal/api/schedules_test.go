package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleRoutesServiceStub struct {
	principal    identity.Principal
	status       schedule.Status
	page         app.PageRequest
	getID        string
	create       app.CreateScheduleCommand
	createKey    string
	updateID     string
	update       app.UpdateScheduleCommand
	updateKey    string
	actionID     string
	action       app.ScheduleActionCommand
	actionKey    string
	runsID       string
	runsPage     app.PageRequest
	listCalls    int
	getCalls     int
	createCalls  int
	updateCalls  int
	actionCalls  int
	runsCalls    int
	listResult   app.SchedulePage
	runsResult   app.ScheduleRunPage
	value        schedule.Schedule
	actionResult app.ScheduleActionResult
	err          error
}

func (s *scheduleRoutesServiceStub) List(_ context.Context, principal identity.Principal, status schedule.Status, page app.PageRequest) (app.SchedulePage, error) {
	s.listCalls++
	s.principal, s.status, s.page = principal, status, page
	return s.listResult, s.err
}

func (s *scheduleRoutesServiceStub) Get(_ context.Context, principal identity.Principal, id string) (schedule.Schedule, error) {
	s.getCalls++
	s.principal, s.getID = principal, id
	return s.value, s.err
}

func (s *scheduleRoutesServiceStub) Create(_ context.Context, principal identity.Principal, command app.CreateScheduleCommand, key string) (schedule.Schedule, error) {
	s.createCalls++
	s.principal, s.create, s.createKey = principal, command, key
	return s.value, s.err
}

func (s *scheduleRoutesServiceStub) Update(_ context.Context, principal identity.Principal, id string, command app.UpdateScheduleCommand, key string) (schedule.Schedule, error) {
	s.updateCalls++
	s.principal, s.updateID, s.update, s.updateKey = principal, id, command, key
	return s.value, s.err
}

func (s *scheduleRoutesServiceStub) Action(_ context.Context, principal identity.Principal, id string, command app.ScheduleActionCommand, key string) (app.ScheduleActionResult, error) {
	s.actionCalls++
	s.principal, s.actionID, s.action, s.actionKey = principal, id, command, key
	if s.actionResult.Schedule.ID == "" {
		s.actionResult.Schedule = s.value
	}
	return s.actionResult, s.err
}

func (s *scheduleRoutesServiceStub) Runs(_ context.Context, principal identity.Principal, id string, page app.PageRequest) (app.ScheduleRunPage, error) {
	s.runsCalls++
	s.principal, s.runsID, s.runsPage = principal, id, page
	return s.runsResult, s.err
}

func scheduleRoutesRouter(principal *identity.Principal, service scheduleRoutesService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	registerScheduleRoutes(router.Group("/api"), service)
	return router
}

func scheduleRoutesTestValue() schedule.Schedule {
	now := time.Date(2026, time.November, 2, 1, 2, 3, 0, time.UTC)
	nextRun := now.Add(8 * time.Hour)
	return schedule.Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 2, Status: schedule.StatusEnabled,
		HostID: "h1234567", ModelID: "model-a", Objective: "check the weekly goal",
		Timing: schedule.Timing{
			Kind: schedule.TimingWeekly, StartsOn: "2026-11-02", LocalTime: "09:30",
			TimeZone: "Asia/Shanghai", Weekdays: []int{1, 3},
		},
		Budget:             task.Budget{ActiveSeconds: 300, ToolCalls: 12, ModelTokens: 9000},
		Authorization:      task.Authorization{Kind: string(identity.KindAPIKey), TokenID: "secret-token-id", Scopes: []string{"schedule:manage"}},
		MisfirePolicy:      schedule.MisfireRunOnce,
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed},
		NextRunAt:          &nextRun, CreatedAt: now, UpdatedAt: now,
	}
}

func TestScheduleListPaginationAndDTOOmitSensitiveFields(t *testing.T) {
	value := scheduleRoutesTestValue()
	service := &scheduleRoutesServiceStub{listResult: app.SchedulePage{Items: []schedule.Schedule{value}, NextCursor: "cursor-2"}}
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}
	response := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, service).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/schedules?status=enabled&limit=12&cursor=cursor-1", nil))
	if response.Code != http.StatusOK || service.listCalls != 1 || service.principal.UserID != principal.UserID ||
		service.status != schedule.StatusEnabled || service.page != (app.PageRequest{Limit: 12, Cursor: "cursor-1"}) {
		t.Fatalf("GET schedules status=%d calls=%d args=%+v body=%s", response.Code, service.listCalls, service, response.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(body["items"], &items); err != nil || len(items) != 1 {
		t.Fatalf("items = %s err=%v", body["items"], err)
	}
	item := items[0]
	if string(body["next_cursor"]) != `"cursor-2"` || string(item["host_id"]) != `"h1234567"` ||
		string(item["budget"]) == "null" || item["owner_id"] != nil || item["authorization"] != nil ||
		strings.Contains(response.Body.String(), "secret-token-id") {
		t.Fatalf("unexpected Schedule response fields: %s", response.Body)
	}
}

func TestScheduleCreatePassesTypedCommandPrincipalAndIdempotencyKey(t *testing.T) {
	service := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"}
	body := `{"host_id":"h1234567","model_id":"model-a","objective":"check the weekly goal","timing":{"kind":"weekly","starts_on":"2026-11-02","local_time":"09:30","time_zone":"Asia/Shanghai","weekdays":[1,3]},"budget":{"active_seconds":300,"tool_calls":12,"model_tokens":9000},"misfire_policy":"run_once","notification_events":["completed","failed"],"observation":{"url":"https://example.com","selector":"main h1","field":"text"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "schedule-create-1")
	response := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.createCalls != 1 || service.createKey != "schedule-create-1" ||
		service.principal.UserID != principal.UserID || service.create.HostID != "h1234567" || service.create.ModelID != "model-a" ||
		service.create.Budget.ActiveSeconds != 300 || service.create.Timing.Kind != schedule.TimingWeekly ||
		service.create.Observation == nil || service.create.Observation.Selector != "main h1" {
		t.Fatalf("POST schedules status=%d calls=%d command=%+v body=%s", response.Code, service.createCalls, service.create, response.Body)
	}
}

func TestScheduleCreateAndUpdateSupportHostlessSchedules(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"}
	createService := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
	createService.value.HostID = ""
	create := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(
		`{"model_id":"model-a","objective":"check the weekly goal","timing":{"kind":"daily","starts_on":"2026-11-02","local_time":"09:30","time_zone":"Asia/Shanghai"}}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Idempotency-Key", "schedule-hostless-create")
	created := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, createService).ServeHTTP(created, create)
	if created.Code != http.StatusCreated || createService.create.HostID != "" || !strings.Contains(created.Body.String(), `"host_id":null`) {
		t.Fatalf("hostless create status=%d command=%+v response=%s", created.Code, createService.create, created.Body)
	}

	for _, test := range []struct {
		name string
		body string
		want *string
	}{
		{name: "omitted host preserves it", body: `{"expected_version":2}`, want: nil},
		{name: "null host unbinds", body: `{"expected_version":2,"host_id":null}`, want: new(string)},
		{name: "string host binds", body: `{"expected_version":2,"host_id":"h8765432"}`, want: stringPointer("h8765432")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
			request := httptest.NewRequest(http.MethodPut, "/api/schedules/j1234567", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "schedule-hostless-update")
			response := httptest.NewRecorder()
			scheduleRoutesRouter(&principal, service).ServeHTTP(response, request)
			if response.Code != http.StatusOK || service.updateCalls != 1 {
				t.Fatalf("host update status=%d response=%s", response.Code, response.Body)
			}
			if test.want == nil {
				if service.update.HostID != nil {
					t.Fatalf("omitted host update = %#v, want nil", service.update.HostID)
				}
				return
			}
			if service.update.HostID == nil || *service.update.HostID != *test.want {
				t.Fatalf("host update = %#v, want %q", service.update.HostID, *test.want)
			}
		})
	}
}

func TestScheduleGetAndUpdateRoutes(t *testing.T) {
	service := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}

	getResponse := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, service).ServeHTTP(getResponse,
		httptest.NewRequest(http.MethodGet, "/api/schedules/j1234567", nil))
	if getResponse.Code != http.StatusOK || service.getCalls != 1 || service.getID != "j1234567" || service.principal.UserID != principal.UserID {
		t.Fatalf("GET schedule status=%d calls=%d id=%q body=%s", getResponse.Code, service.getCalls, service.getID, getResponse.Body)
	}

	for _, test := range []struct {
		body string
		set  bool
		want *schedule.ObservationSpec
	}{
		{body: `{"expected_version":2,"objective":"revised"}`, set: false},
		{body: `{"expected_version":2,"observation":null}`, set: true},
		{body: `{"expected_version":2,"observation":{"url":"https://example.com","selector":"main","field":"value"}}`, set: true, want: &schedule.ObservationSpec{URL: "https://example.com", Selector: "main", Field: schedule.ObservationValue}},
	} {
		updateService := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
		request := httptest.NewRequest(http.MethodPut, "/api/schedules/j1234567", strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "schedule-update-1")
		response := httptest.NewRecorder()
		scheduleRoutesRouter(&principal, updateService).ServeHTTP(response, request)
		if response.Code != http.StatusOK || updateService.updateCalls != 1 || updateService.updateID != "j1234567" || updateService.updateKey != "schedule-update-1" ||
			updateService.update.ExpectedVersion != 2 || updateService.update.ObservationSet != test.set {
			t.Fatalf("PUT schedule body=%s status=%d command=%+v response=%s", test.body, response.Code, updateService.update, response.Body)
		}
		if test.want == nil && updateService.update.Observation != nil || test.want != nil && updateService.update.Observation == nil {
			t.Fatalf("observation = %+v, want %+v", updateService.update.Observation, test.want)
		}
	}
}

func TestScheduleActionForwardsExpectedVersionAndIdempotencyKey(t *testing.T) {
	service := &scheduleRoutesServiceStub{value: scheduleRoutesTestValue()}
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}
	request := httptest.NewRequest(http.MethodPost, "/api/schedules/j1234567/actions",
		strings.NewReader(`{"action":"disable","expected_version":2,"current_run_action":"cancel"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "schedule-action-1")
	response := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.actionCalls != 1 || service.actionID != "j1234567" ||
		service.action.Action != schedule.ActionDisable || service.action.ExpectedVersion != 2 ||
		service.action.CurrentRunAction != schedule.CurrentRunCancel || service.actionKey != "schedule-action-1" {
		t.Fatalf("POST schedule action status=%d action=%+v key=%q body=%s", response.Code, service.action, service.actionKey, response.Body)
	}
}

func TestScheduleRunsPaginationAndNonNullArrays(t *testing.T) {
	service := &scheduleRoutesServiceStub{runsResult: app.ScheduleRunPage{NextCursor: ""}}
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}
	response := httptest.NewRecorder()
	scheduleRoutesRouter(&principal, service).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/schedules/j1234567/runs?limit=20&cursor=run-cursor", nil))
	if response.Code != http.StatusOK || service.runsCalls != 1 || service.runsID != "j1234567" ||
		service.runsPage != (app.PageRequest{Limit: 20, Cursor: "run-cursor"}) {
		t.Fatalf("GET runs status=%d calls=%d page=%+v body=%s", response.Code, service.runsCalls, service.runsPage, response.Body)
	}
	var runPage struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor json.RawMessage   `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &runPage); err != nil || runPage.Items == nil || string(runPage.NextCursor) != `""` {
		t.Fatalf("run arrays/cursor are not normalized: %+v err=%v body=%s", runPage, err, response.Body)
	}

	emptyService := &scheduleRoutesServiceStub{listResult: app.SchedulePage{Items: nil}, runsResult: app.ScheduleRunPage{Items: nil}}
	for _, path := range []string{"/api/schedules", "/api/schedules/j1234567/runs"} {
		emptyResponse := httptest.NewRecorder()
		scheduleRoutesRouter(&principal, emptyService).ServeHTTP(emptyResponse, httptest.NewRequest(http.MethodGet, path, nil))
		if emptyResponse.Code != http.StatusOK || !strings.Contains(emptyResponse.Body.String(), `"items":[]`) {
			t.Errorf("GET %s status=%d response=%s", path, emptyResponse.Code, emptyResponse.Body)
		}
	}
}

func TestScheduleRejectsInvalidBodiesHeadersAndQueriesBeforeCallingService(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		keys        []string
	}{
		{name: "page limit", method: http.MethodGet, path: "/api/schedules?limit=101"},
		{name: "duplicate status", method: http.MethodGet, path: "/api/schedules?status=enabled&status=disabled"},
		{name: "empty status", method: http.MethodGet, path: "/api/schedules?status="},
		{name: "unknown create field", method: http.MethodPost, path: "/api/schedules", body: `{"unexpected":true}`, contentType: "application/json"},
		{name: "unknown nested timing field", method: http.MethodPost, path: "/api/schedules", body: `{"timing":{"kind":"daily","time_zone":"UTC","unexpected":true}}`, contentType: "application/json"},
		{name: "wrong content type", method: http.MethodPost, path: "/api/schedules", body: `{}`, contentType: "text/plain"},
		{name: "missing create key", method: http.MethodPost, path: "/api/schedules", body: `{}`, contentType: "application/json"},
		{name: "duplicate create key", method: http.MethodPost, path: "/api/schedules", body: `{}`, contentType: "application/json", keys: []string{"schedule-create-1", "schedule-create-2"}},
		{name: "malformed create key", method: http.MethodPost, path: "/api/schedules", body: `{}`, contentType: "application/json", keys: []string{"short"}},
		{name: "missing update key", method: http.MethodPut, path: "/api/schedules/j1234567", body: `{"expected_version":1}`, contentType: "application/json"},
		{name: "missing expected version", method: http.MethodPut, path: "/api/schedules/j1234567", body: `{"objective":"new"}`, contentType: "application/json", keys: []string{"schedule-update-1"}},
		{name: "unknown observation field", method: http.MethodPut, path: "/api/schedules/j1234567", body: `{"expected_version":1,"observation":{"url":"https://example.com","selector":"main","field":"text","extra":1}}`, contentType: "application/json", keys: []string{"schedule-update-1"}},
		{name: "missing action key", method: http.MethodPost, path: "/api/schedules/j1234567/actions", body: `{"action":"disable","expected_version":1}`, contentType: "application/json"},
		{name: "invalid schedule id", method: http.MethodGet, path: "/api/schedules/bad"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &scheduleRoutesServiceStub{}
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			for _, key := range test.keys {
				request.Header.Add("Idempotency-Key", key)
			}
			response := httptest.NewRecorder()
			scheduleRoutesRouter(&principal, service).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || service.listCalls+service.getCalls+service.createCalls+service.updateCalls+service.actionCalls+service.runsCalls != 0 {
				t.Fatalf("status=%d service calls=%+v body=%s", response.Code, service, response.Body)
			}
		})
	}
}

func TestScheduleServiceErrorsMapForbiddenAndNotFound(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindClient, TokenID: "t1234567"}
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "forbidden", err: domain.NewError(domain.CodeForbidden, "private details"), want: http.StatusForbidden},
		{name: "not found", err: domain.NewError(domain.CodeNotFound, "private details"), want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &scheduleRoutesServiceStub{err: test.err}
			response := httptest.NewRecorder()
			scheduleRoutesRouter(&principal, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/schedules/j1234567", nil))
			if response.Code != test.want || service.getCalls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, service.getCalls, response.Body)
			}
			if strings.Contains(response.Body.String(), "private details") {
				t.Fatalf("internal service message leaked: %s", response.Body)
			}
		})
	}
}

func TestScheduleRoutesRequirePrincipal(t *testing.T) {
	service := &scheduleRoutesServiceStub{}
	response := httptest.NewRecorder()
	scheduleRoutesRouter(nil, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/schedules", nil))
	if response.Code != http.StatusUnauthorized || service.listCalls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, service.listCalls, response.Body)
	}
}
