package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type taskRequirementConfirmationAPITestService struct {
	*taskServiceStub
	principal identity.Principal
	taskID    string
	input     app.TaskRequirementManifestConfirmationInput
	result    app.TaskRequirementManifestConfirmation
	calls     int
	err       error
}

func (s *taskRequirementConfirmationAPITestService) ConfirmTaskRequirementManifest(_ context.Context,
	principal identity.Principal, taskID string, input app.TaskRequirementManifestConfirmationInput) (app.TaskRequirementManifestConfirmation, error) {
	s.principal, s.taskID, s.input = principal, taskID, input
	s.calls++
	return s.result, s.err
}

func TestConfirmTaskRequirementManifestRequiresAuthenticatedStrictIdempotentRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &taskRequirementConfirmationAPITestService{taskServiceStub: &taskServiceStub{}, result: app.TaskRequirementManifestConfirmation{
		TaskID: "q1234567", TaskRevision: 3, PlanRevision: 2, PlanEventSequence: 19,
		ManifestDigest: strings.Repeat("a", 64), RequirementsCompleteAttested: true,
		AttestedAt:  confirmationTestTime(time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)),
		ConfirmedAt: time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC),
	}}
	principal := identity.Principal{UserID: testUserID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testTokenID}
	engine := taskRequirementConfirmationTestRouter(service, &principal)
	body := `{"expected_task_revision":3,"expected_plan_revision":2,"expected_plan_event_sequence":19,"requirements_complete_attested":true}`
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/requirement-manifest/confirm", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"`+strings.Repeat("a", 64)+`"`)
	request.Header.Set("Idempotency-Key", "confirm-plan-19")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("confirmation status=%d calls=%d body=%s; want 200 and one call", response.Code, service.calls, response.Body)
	}
	if service.principal.UserID != principal.UserID || service.principal.Kind != principal.Kind || service.taskID != "q1234567" ||
		service.input.TaskRevision != 3 || service.input.PlanRevision != 2 || service.input.PlanEventSequence != 19 ||
		service.input.ManifestDigest != strings.Repeat("a", 64) || !service.input.RequirementsCompleteAttested || service.input.IdempotencyKey != "confirm-plan-19" {
		t.Fatalf("confirmation service received wrong authenticated fence: principal=%+v task=%q input=%+v",
			service.principal, service.taskID, service.input)
	}
	responseBody := response.Body.String()
	if !strings.Contains(responseBody, `"plan_event_sequence":19`) || !strings.Contains(responseBody, `"manifest_digest":"`+strings.Repeat("a", 64)+`"`) ||
		strings.Contains(responseBody, "requirement_set_verified") || strings.Contains(responseBody, "task_authorization_version") ||
		strings.Contains(responseBody, "confirmed_by_user_id") || response.Header().Get("ETag") != `"`+strings.Repeat("a", 64)+`"` {
		t.Fatalf("confirmation response leaked internal auth or implied verification: %s", responseBody)
	}
}

func confirmationTestTime(value time.Time) *time.Time { return &value }

func TestConfirmTaskRequirementManifestRejectsUnauthenticatedMalformedAndMissingIdempotency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &taskRequirementConfirmationAPITestService{taskServiceStub: &taskServiceStub{}}
	principal := identity.Principal{UserID: testUserID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testTokenID}
	validBody := `{"expected_task_revision":3,"expected_plan_revision":2,"expected_plan_event_sequence":19}`
	tests := []struct {
		name         string
		principal    *identity.Principal
		body         string
		query        string
		key          string
		ifMatch      string
		extraIfMatch []string
	}{
		{name: "missing authenticated principal", body: validBody, key: "confirm-plan-19", ifMatch: `"` + strings.Repeat("a", 64) + `"`},
		{name: "model supplied actor field", principal: &principal,
			body: `{"expected_task_revision":3,"expected_plan_revision":2,"expected_plan_event_sequence":19,"actor_user_id":"u9999999"}`,
			key:  "confirm-plan-19", ifMatch: `"` + strings.Repeat("a", 64) + `"`},
		{name: "body supplied digest", principal: &principal,
			body: `{"expected_task_revision":3,"expected_plan_revision":2,"expected_plan_event_sequence":19,"expected_manifest_digest":"` + strings.Repeat("a", 64) + `"}`,
			key:  "confirm-plan-19", ifMatch: `"` + strings.Repeat("a", 64) + `"`},
		{name: "missing If-Match", principal: &principal, body: validBody, key: "confirm-plan-19"},
		{name: "malformed If-Match", principal: &principal, body: validBody, key: "confirm-plan-19", ifMatch: strings.Repeat("a", 64)},
		{name: "multiple If-Match validators", principal: &principal, body: validBody, key: "confirm-plan-19",
			ifMatch: `"` + strings.Repeat("a", 64) + `", "` + strings.Repeat("b", 64) + `"`},
		{name: "repeated If-Match headers", principal: &principal, body: validBody, key: "confirm-plan-19",
			ifMatch: `"` + strings.Repeat("a", 64) + `"`, extraIfMatch: []string{`"` + strings.Repeat("a", 64) + `"`}},
		{name: "missing idempotency key", principal: &principal, body: validBody, ifMatch: `"` + strings.Repeat("a", 64) + `"`},
		{name: "query string", principal: &principal, body: validBody, query: "?actor=u9999999", key: "confirm-plan-19", ifMatch: `"` + strings.Repeat("a", 64) + `"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := taskRequirementConfirmationTestRouter(service, test.principal)
			request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/requirement-manifest/confirm"+test.query, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.ifMatch != "" {
				request.Header.Set("If-Match", test.ifMatch)
			}
			for _, value := range test.extraIfMatch {
				request.Header.Add("If-Match", value)
			}
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code < http.StatusBadRequest || service.calls != 0 {
				t.Fatalf("status=%d service calls=%d body=%s; want request rejected before service", response.Code, service.calls, response.Body)
			}
		})
	}
}

func TestConfirmTaskRequirementManifestPropagatesStalePlanConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &taskRequirementConfirmationAPITestService{taskServiceStub: &taskServiceStub{}, err: domain.NewError(domain.CodeConflict, "stale plan")}
	principal := identity.Principal{UserID: testUserID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testTokenID}
	engine := taskRequirementConfirmationTestRouter(service, &principal)
	body := `{"expected_task_revision":3,"expected_plan_revision":2,"expected_plan_event_sequence":19}`
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/requirement-manifest/confirm", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"`+strings.Repeat("a", 64)+`"`)
	request.Header.Set("Idempotency-Key", "confirm-plan-19")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || service.calls != 1 {
		t.Fatalf("stale confirmation status=%d calls=%d body=%s; want 409 and one call", response.Code, service.calls, response.Body)
	}
}

func taskRequirementConfirmationTestRouter(service TaskService, principal *identity.Principal) *gin.Engine {
	engine := gin.New()
	if principal != nil {
		engine.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	handler := &api{options: Options{ServiceInstanceID: "11111111-1111-4111-8111-111111111111"}}
	handler.registerTasks(engine.Group("/api"), service)
	return engine
}
