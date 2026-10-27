package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelUsageAdminHTTPStub struct {
	items   []app.UnknownModelCall
	command app.ModelUsageReconcileCommand
	owner   string
	limit   int
	writes  int
	budget  app.ModelAccountBudget
	update  app.ModelAccountBudgetUpdate
	err     error
}

func (s *modelUsageAdminHTTPStub) ListUnknown(_ context.Context, _ identity.Principal, owner string, limit int) ([]app.UnknownModelCall, error) {
	s.owner, s.limit = owner, limit
	return s.items, s.err
}

func (s *modelUsageAdminHTTPStub) Reconcile(_ context.Context, _ identity.Principal, command app.ModelUsageReconcileCommand) (bool, error) {
	s.command = command
	s.writes++
	return false, s.err
}

func (s *modelUsageAdminHTTPStub) Budget(_ context.Context, _ identity.Principal, ownerID string) (app.ModelAccountBudget, error) {
	s.owner = ownerID
	return s.budget, s.err
}

func (s *modelUsageAdminHTTPStub) SetBudget(_ context.Context, _ identity.Principal, command app.ModelAccountBudgetUpdate) (app.ModelAccountBudget, error) {
	s.update = command
	return s.budget, s.err
}

func modelUsageAdminRouter(t *testing.T, service *modelUsageAdminHTTPStub, role identity.Role) *gin.Engine {
	t.Helper()
	deps := newServices().deps()
	deps.Verifier = verifierStub{role: role}
	if service != nil {
		deps.ModelUsageAdmin = service
	}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAdminModelUsageListAndReconcile(t *testing.T) {
	service := &modelUsageAdminHTTPStub{items: []app.UnknownModelCall{{OwnerID: "u1234567", Source: app.ModelCallTask,
		CallID: "model-call-001", ModelID: "p1234567", ProviderModel: "provider-model", TaskID: "q1234567",
		ReservedTokens: 100, CreatedAt: time.Unix(10, 0), UnknownAt: time.Unix(20, 0)}}}
	router := modelUsageAdminRouter(t, service, identity.RoleAdmin)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	listed := requestWithCookie(router, http.MethodGet, "/api/admin/model-usage/unknown?owner_id=u1234567&limit=10", cookie, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"call_id":"model-call-001"`) ||
		service.owner != "u1234567" || service.limit != 10 {
		t.Fatalf("list = %d %s", listed.Code, listed.Body)
	}
	result := requestWithCookieBody(router, http.MethodPost, "/api/admin/model-usage/reconcile",
		`{"owner_id":"u1234567","source":"task","call_id":"model-call-001","input_tokens":0,"output_tokens":4,"evidence":"provider reference 1"}`,
		cookie, "https://portal.example.test")
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"replayed":false`) || service.writes != 1 ||
		service.command.InputTokens != 0 || service.command.OutputTokens != 4 || service.command.Evidence != "provider reference 1" {
		t.Fatalf("reconcile = %d %s, %#v", result.Code, result.Body, service.command)
	}
	service.budget = app.ModelAccountBudget{OwnerID: "u1234567", Configured: true, TokenLimit: 5000,
		MaxConcurrent: 2, UsedTokens: 120, InFlight: 1, HasUnknown: true}
	budget := requestWithCookie(router, http.MethodGet, "/api/admin/users/u1234567/model-usage-budget", cookie, "")
	if budget.Code != http.StatusOK || !strings.Contains(budget.Body.String(), `"used_tokens":120`) || service.owner != "u1234567" {
		t.Fatalf("budget = %d %s", budget.Code, budget.Body)
	}
	updated := requestWithCookieBody(router, http.MethodPut, "/api/admin/users/u1234567/model-usage-budget",
		`{"token_limit":7000,"max_concurrent":4}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || service.update.OwnerID != "u1234567" || service.update.TokenLimit != 7000 || service.update.MaxConcurrent != 4 {
		t.Fatalf("update budget = %d %s, %#v", updated.Code, updated.Body, service.update)
	}
	service.items = nil
	empty := requestWithCookie(router, http.MethodGet, "/api/admin/model-usage/unknown", cookie, "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Fatalf("empty list = %d %s", empty.Code, empty.Body)
	}
}

func TestAdminModelUsageRejectsUnauthorizedAndInvalidInput(t *testing.T) {
	service := &modelUsageAdminHTTPStub{}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	userRouter := modelUsageAdminRouter(t, service, identity.RoleUser)
	if result := requestWithCookie(userRouter, http.MethodGet, "/api/admin/model-usage/unknown", cookie, ""); result.Code != http.StatusForbidden {
		t.Fatalf("user list = %d %s", result.Code, result.Body)
	}
	if result := requestWithCookie(userRouter, http.MethodGet, "/api/admin/users/u1234567/model-usage-budget", cookie, ""); result.Code != http.StatusForbidden {
		t.Fatalf("user budget = %d %s", result.Code, result.Body)
	}
	adminRouter := modelUsageAdminRouter(t, service, identity.RoleAdmin)
	for _, body := range []string{
		`{"owner_id":"u1234567","source":"task","call_id":"model-call-001","output_tokens":4,"evidence":"provider reference"}`,
		`{"owner_id":"u1234567","source":"task","call_id":"model-call-001","input_tokens":0,"output_tokens":4,"evidence":"provider reference","extra":true}`,
	} {
		result := requestWithCookieBody(adminRouter, http.MethodPost, "/api/admin/model-usage/reconcile", body, cookie, "https://portal.example.test")
		if result.Code != http.StatusBadRequest {
			t.Fatalf("invalid request = %d %s", result.Code, result.Body)
		}
	}
	for _, body := range []string{`{"token_limit":100}`, `{"token_limit":100,"max_concurrent":2,"extra":true}`} {
		result := requestWithCookieBody(adminRouter, http.MethodPut, "/api/admin/users/u1234567/model-usage-budget", body, cookie, "https://portal.example.test")
		if result.Code != http.StatusBadRequest {
			t.Fatalf("invalid budget request = %d %s", result.Code, result.Body)
		}
	}
	if result := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/model-usage/unknown?limit=2&limit=3", cookie, ""); result.Code != http.StatusBadRequest {
		t.Fatalf("duplicate limit = %d %s", result.Code, result.Body)
	}
	service.err = domain.NewError(domain.CodeIdempotencyConflict, "usage conflict")
	result := requestWithCookieBody(adminRouter, http.MethodPost, "/api/admin/model-usage/reconcile",
		`{"owner_id":"u1234567","source":"task","call_id":"model-call-001","input_tokens":0,"output_tokens":4,"evidence":"provider reference"}`,
		cookie, "https://portal.example.test")
	if result.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s", result.Code, result.Body)
	}
	withoutService := modelUsageAdminRouter(t, nil, identity.RoleAdmin)
	if result := requestWithCookie(withoutService, http.MethodGet, "/api/admin/model-usage/unknown", cookie, ""); result.Code != http.StatusNotFound {
		t.Fatalf("unassembled feature = %d %s", result.Code, result.Body)
	}
	if result := requestWithCookie(withoutService, http.MethodGet, "/api/admin/users/u1234567/model-usage-budget", cookie, ""); result.Code != http.StatusNotFound {
		t.Fatalf("unassembled budget = %d %s", result.Code, result.Body)
	}
}
