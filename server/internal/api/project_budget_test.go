package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectBudgetRoutesServiceStub struct {
	principal         identity.Principal
	projectID         string
	command           app.SetProjectBudgetCommand
	view              app.ProjectBudgetView
	ledgerPage        app.ProjectLedgerPage
	ledgerPageRequest app.PageRequest
	getErr            error
	setErr            error
	ledgerErr         error
	getCalls          int
	setCalls          int
	ledgerCalls       int
}

func (s *projectBudgetRoutesServiceStub) GetBudget(_ context.Context, principal identity.Principal, projectID string) (app.ProjectBudgetView, error) {
	s.getCalls++
	s.principal, s.projectID = principal, projectID
	return s.view, s.getErr
}

func (s *projectBudgetRoutesServiceStub) SetBudget(_ context.Context, principal identity.Principal, projectID string, command app.SetProjectBudgetCommand) (app.ProjectBudgetView, error) {
	s.setCalls++
	s.principal, s.projectID, s.command = principal, projectID, command
	return s.view, s.setErr
}

func (s *projectBudgetRoutesServiceStub) ListLedger(_ context.Context, principal identity.Principal, projectID string, page app.PageRequest) (app.ProjectLedgerPage, error) {
	s.ledgerCalls++
	s.principal, s.projectID, s.ledgerPageRequest = principal, projectID, page
	return s.ledgerPage, s.ledgerErr
}

func projectBudgetAPIRouter(principal *identity.Principal, service projectBudgetRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectBudgetRoutes(router.Group("/api"), service)
	return router
}

func projectBudgetAPIRequest(router http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestProjectBudgetRoutesReadSummaryAndSetOnlyBudgetFields(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectBudgetRoutesServiceStub{view: app.ProjectBudgetView{
		Configured: true, Budget: project.Budget{Currency: "USD", LimitMicros: 30_000_000, Version: 4},
		SpentMicros: 2_000_000, ReservedMicros: 3_000_000, HasUnknown: true,
	}}
	router := projectBudgetAPIRouter(&principal, service)

	got := projectBudgetAPIRequest(router, http.MethodGet, "/api/projects/j1234567/budget", "", "")
	var response projectBudgetResponse
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || got.Code != http.StatusOK ||
		response != (projectBudgetResponse{Configured: true, Currency: "USD", LimitMicros: 30_000_000, Version: 4,
			SpentMicros: 2_000_000, ReservedMicros: 3_000_000, HasUnknown: true}) ||
		service.getCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.projectID != "j1234567" {
		t.Fatalf("GET budget = %d %s; response=%#v service=%#v err=%v", got.Code, got.Body, response, service, err)
	}

	got = projectBudgetAPIRequest(router, http.MethodPut, "/api/projects/j1234567/budget",
		`{"currency":"EUR","limit_micros":9000000,"expected_version":4}`, "budget-write-0001")
	if got.Code != http.StatusOK || service.setCalls != 1 || service.projectID != "j1234567" ||
		service.command != (app.SetProjectBudgetCommand{Currency: "EUR", LimitMicros: 9_000_000, ExpectedVersion: 4, IdempotencyKey: "budget-write-0001"}) ||
		!reflect.DeepEqual(service.principal, principal) {
		t.Fatalf("PUT budget = %d %s; service=%#v", got.Code, got.Body, service)
	}
}

func TestProjectBudgetRoutesRequireSessionAndPreconditions(t *testing.T) {
	service := &projectBudgetRoutesServiceStub{}
	if got := projectBudgetAPIRequest(projectBudgetAPIRouter(nil, service), http.MethodGet, "/api/projects/j1234567/budget", "", ""); got.Code != http.StatusUnauthorized || service.getCalls != 0 {
		t.Fatalf("unauthenticated GET = %d %s calls=%d", got.Code, got.Body, service.getCalls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectBudgetAPIRouter(&principal, service)
	for _, test := range []struct {
		name string
		path string
		body string
		key  string
	}{
		{name: "invalid id", path: "/api/projects/bad/budget", body: `{"currency":"USD","limit_micros":1,"expected_version":0}`, key: "budget-write-0001"},
		{name: "missing version", path: "/api/projects/j1234567/budget", body: `{"currency":"USD","limit_micros":1}`, key: "budget-write-0001"},
		{name: "client supplied rate", path: "/api/projects/j1234567/budget", body: `{"currency":"USD","limit_micros":1,"expected_version":0,"rate":{}}`, key: "budget-write-0001"},
		{name: "missing idempotency", path: "/api/projects/j1234567/budget", body: `{"currency":"USD","limit_micros":1,"expected_version":0}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := projectBudgetAPIRequest(router, http.MethodPut, test.path, test.body, test.key)
			if got.Code != http.StatusBadRequest || service.setCalls != 0 {
				t.Fatalf("invalid PUT = %d %s calls=%d", got.Code, got.Body, service.setCalls)
			}
		})
	}
	if got := projectBudgetAPIRequest(router, http.MethodPost, "/api/projects/j1234567/budget/reservations", `{}`, ""); got.Code != http.StatusNotFound {
		t.Fatalf("budget reservation route must not accept client input: %d %s", got.Code, got.Body)
	}
}

func TestProjectBudgetLedgerRouteReturnsOnlyTraceMetadata(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectBudgetRoutesServiceStub{ledgerPage: app.ProjectLedgerPage{
		Items: []app.ProjectLedgerEntry{{
			CallID: "task-call-0001", ActorID: principal.UserID, TaskID: "q1234567", Source: app.ModelCallTask,
			ModelID: "p1234567", RateVersion: 3, Currency: "USD", Status: app.ProjectLedgerUnknown,
			ReservedUsage: project.Usage{InputTokens: 100, OutputTokens: 20}, ReservedMicros: 500,
			CreatedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		}}, NextCursor: "next-page",
	}}
	got := projectBudgetAPIRequest(projectBudgetAPIRouter(&principal, service), http.MethodGet,
		"/api/projects/j1234567/budget/ledger?limit=10&cursor=before", "", "")
	var response struct {
		Items      []projectLedgerEntryResponse `json:"items"`
		NextCursor *string                      `json:"next_cursor"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || got.Code != http.StatusOK || len(response.Items) != 1 ||
		response.Items[0].ActorID != principal.UserID || response.Items[0].TaskID != "q1234567" ||
		response.Items[0].Status != app.ProjectLedgerUnknown || response.Items[0].ActualMicros != nil ||
		response.NextCursor == nil || *response.NextCursor != "next-page" ||
		service.ledgerCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.projectID != "j1234567" ||
		service.ledgerPageRequest != (app.PageRequest{Limit: 10, Cursor: "before"}) ||
		strings.Contains(got.Body.String(), "prompt") || strings.Contains(got.Body.String(), "credential") {
		t.Fatalf("GET budget ledger = %d %s; response=%#v service=%#v err=%v", got.Code, got.Body, response, service, err)
	}
	if got := projectBudgetAPIRequest(projectBudgetAPIRouter(nil, service), http.MethodGet, "/api/projects/j1234567/budget/ledger", "", ""); got.Code != http.StatusUnauthorized || service.ledgerCalls != 1 {
		t.Fatalf("unauthenticated ledger GET = %d %s calls=%d", got.Code, got.Body, service.ledgerCalls)
	}
	if got := projectBudgetAPIRequest(projectBudgetAPIRouter(&principal, nil), http.MethodGet, "/api/projects/j1234567/budget/ledger", "", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing service ledger GET = %d %s", got.Code, got.Body)
	}
}

func TestProjectBudgetRoutesReturnServiceErrorsAndUnavailable(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectBudgetRoutesServiceStub{getErr: domain.NewError(domain.CodeForbidden, "project role is insufficient")}
	got := projectBudgetAPIRequest(projectBudgetAPIRouter(&principal, service), http.MethodGet, "/api/projects/j1234567/budget", "", "")
	if got.Code != http.StatusForbidden || service.getCalls != 1 {
		t.Fatalf("forbidden GET = %d %s calls=%d", got.Code, got.Body, service.getCalls)
	}
	service.getErr = nil
	service.setErr = errors.New("database unavailable")
	got = projectBudgetAPIRequest(projectBudgetAPIRouter(&principal, service), http.MethodPut, "/api/projects/j1234567/budget",
		`{"currency":"USD","limit_micros":1000000,"expected_version":0}`, "budget-write-0001")
	if got.Code < http.StatusInternalServerError || service.setCalls != 1 {
		t.Fatalf("store error PUT = %d %s calls=%d", got.Code, got.Body, service.setCalls)
	}
	got = projectBudgetAPIRequest(projectBudgetAPIRouter(&principal, nil), http.MethodGet, "/api/projects/j1234567/budget", "", "")
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing service GET = %d %s", got.Code, got.Body)
	}
}
