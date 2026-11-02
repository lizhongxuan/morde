package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type adminControlsServiceStub struct {
	principal       identity.Principal
	listCalls       int
	createCalls     int
	updateCalls     int
	deleteCalls     int
	evaluateCalls   int
	notifyTestCalls int
	threshold       int64
	note            string
	command         app.UpdateAdminControlRuleCommand
	deletedID       int64
	deletedVersion  int64
}

func (s *adminControlsServiceStub) List(_ context.Context, principal identity.Principal) (app.AdminControls, error) {
	s.principal, s.listCalls = principal, s.listCalls+1
	return app.AdminControls{Rules: []app.AdminControlRule{}, Notifications: []app.AdminControlNotification{}, AccountsTotal: 2, NotifyConfigured: true}, nil
}
func (s *adminControlsServiceStub) Create(_ context.Context, principal identity.Principal, threshold int64, note string) (app.AdminControlRule, error) {
	s.principal, s.threshold, s.note = principal, threshold, note
	s.createCalls++
	return app.AdminControlRule{ID: 3, Threshold: threshold, Enabled: true, Note: note, CreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Version: 1}, nil
}
func (s *adminControlsServiceStub) Update(_ context.Context, principal identity.Principal, command app.UpdateAdminControlRuleCommand) (app.AdminControlRule, error) {
	s.principal, s.command = principal, command
	s.updateCalls++
	return app.AdminControlRule{ID: command.ID, Threshold: 10, Enabled: true, Note: "updated", CreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Version: command.ExpectedVersion + 1}, nil
}
func (s *adminControlsServiceStub) Delete(_ context.Context, principal identity.Principal, id, expectedVersion int64) error {
	s.principal, s.deletedID, s.deletedVersion = principal, id, expectedVersion
	s.deleteCalls++
	return nil
}
func (s *adminControlsServiceStub) Evaluate(_ context.Context, principal identity.Principal) (app.AdminControlEvaluation, error) {
	s.principal, s.evaluateCalls = principal, s.evaluateCalls+1
	return app.AdminControlEvaluation{AccountsTotal: 2, Fired: []app.AdminControlRule{}}, nil
}
func (s *adminControlsServiceStub) NotifyTest(_ context.Context, principal identity.Principal) (app.AdminControlNotifyTest, error) {
	s.principal, s.notifyTestCalls = principal, s.notifyTestCalls+1
	return app.AdminControlNotifyTest{Sent: true, Configured: true, CreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}, nil
}

func TestAdminControlsRoutesValidateAndForwardSession(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	controlService := &adminControlsServiceStub{}
	deps := services.deps()
	deps.AdminControls = controlService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	listed := requestWithCookie(router, http.MethodGet, "/api/admin/controls", cookie, "")
	if listed.Code != http.StatusOK || controlService.listCalls != 1 || controlService.principal.Role != identity.RoleAdmin {
		t.Fatalf("GET controls = %d %s calls=%d principal=%+v", listed.Code, listed.Body, controlService.listCalls, controlService.principal)
	}
	invalid := requestWithCookieBody(router, http.MethodPost, "/api/admin/controls/rules", `{"threshold":0}`, cookie, "https://portal.example.test")
	if invalid.Code != http.StatusBadRequest || controlService.createCalls != 0 {
		t.Fatalf("POST invalid control = %d %s calls=%d", invalid.Code, invalid.Body, controlService.createCalls)
	}
	created := requestWithCookieBody(router, http.MethodPost, "/api/admin/controls/rules", `{"threshold":5,"note":"watch"}`, cookie, "https://portal.example.test")
	if created.Code != http.StatusCreated || controlService.createCalls != 1 || controlService.threshold != 5 || controlService.note != "watch" {
		t.Fatalf("POST control = %d %s calls=%d threshold=%d note=%q", created.Code, created.Body, controlService.createCalls, controlService.threshold, controlService.note)
	}
	updated := requestWithCookieBody(router, http.MethodPut, "/api/admin/controls/rules/7", `{"expected_version":4,"enabled":false,"rearm":true}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || controlService.updateCalls != 1 || controlService.command.ID != 7 ||
		controlService.command.ExpectedVersion != 4 || controlService.command.Enabled == nil || *controlService.command.Enabled || !controlService.command.Rearm {
		t.Fatalf("PUT control = %d %s calls=%d command=%+v", updated.Code, updated.Body, controlService.updateCalls, controlService.command)
	}
	missingVersion := requestWithCookie(router, http.MethodDelete, "/api/admin/controls/rules/7", cookie, "https://portal.example.test")
	if missingVersion.Code != http.StatusBadRequest || controlService.deleteCalls != 0 {
		t.Fatalf("DELETE control without version = %d %s calls=%d", missingVersion.Code, missingVersion.Body, controlService.deleteCalls)
	}
	deleted := requestWithCookie(router, http.MethodDelete, "/api/admin/controls/rules/7?expected_version=4", cookie, "https://portal.example.test")
	if deleted.Code != http.StatusNoContent || controlService.deleteCalls != 1 || controlService.deletedID != 7 || controlService.deletedVersion != 4 {
		t.Fatalf("DELETE control = %d %s calls=%d id=%d version=%d", deleted.Code, deleted.Body, controlService.deleteCalls, controlService.deletedID, controlService.deletedVersion)
	}
	for _, route := range []struct {
		path  string
		calls *int
	}{
		{path: "/api/admin/controls/evaluate", calls: &controlService.evaluateCalls},
		{path: "/api/admin/controls/notify-test", calls: &controlService.notifyTestCalls},
	} {
		response := requestWithCookie(router, http.MethodPost, route.path, cookie, "https://portal.example.test")
		if response.Code != http.StatusOK || *route.calls != 1 {
			t.Errorf("POST %s = %d %s calls=%d", route.path, response.Code, response.Body, *route.calls)
		}
	}

	services.verifier.role = identity.RoleUser
	userDeps := services.deps()
	userDeps.AdminControls = controlService
	userRouter, err := New(userDeps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/controls", cookie, "")
	if denied.Code != http.StatusForbidden || controlService.listCalls != 1 {
		t.Fatalf("non-admin controls = %d %s calls=%d", denied.Code, denied.Body, controlService.listCalls)
	}
}
