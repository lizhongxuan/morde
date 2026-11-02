package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type adminOperationsServiceStub struct {
	settings  app.AdminSettings
	invites   app.AdminInvitePage
	sites     app.AdminSitePage
	nudge     app.AdminNudgePolicy
	code      string
	update    app.UpdateAdminNudgeCommand
	updateErr error
}

func (s *adminOperationsServiceStub) Settings(context.Context, identity.Principal) (app.AdminSettings, error) {
	return s.settings, nil
}
func (s *adminOperationsServiceStub) InviteCode(context.Context, identity.Principal) (string, error) {
	return s.code, nil
}
func (s *adminOperationsServiceStub) ListInvites(_ context.Context, _ identity.Principal, filter app.AdminInviteFilter) (app.AdminInvitePage, error) {
	page := s.invites
	page.Limit, page.Offset = filter.Limit, filter.Offset
	return page, nil
}
func (s *adminOperationsServiceStub) ListSites(_ context.Context, _ identity.Principal, limit, offset int) (app.AdminSitePage, error) {
	page := s.sites
	page.Limit, page.Offset = limit, offset
	return page, nil
}
func (s *adminOperationsServiceStub) NudgePolicy(context.Context, identity.Principal) (app.AdminNudgePolicy, error) {
	return s.nudge, nil
}
func (s *adminOperationsServiceStub) PublicNudgePolicy(context.Context) (app.AdminNudgePolicy, error) {
	return app.AdminNudgePolicy{Version: 1}, nil
}
func (s *adminOperationsServiceStub) UpdateNudgePolicy(_ context.Context, _ identity.Principal, command app.UpdateAdminNudgeCommand) (app.AdminNudgePolicy, error) {
	s.update = command
	if s.updateErr != nil {
		return app.AdminNudgePolicy{}, s.updateErr
	}
	updatedAt := time.Now().UTC()
	return app.AdminNudgePolicy{Enabled: command.Enabled, URL: command.URL, TextZH: command.TextZH, TextEN: command.TextEN,
		Version: command.ExpectedVersion + 1, UpdatedAt: &updatedAt}, nil
}

func TestAdminOperationsRoutesRequireAdminAndReturnSafeEmptyStates(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	settings := app.AdminSettings{RegistrationReviewRequired: true, DefaultPersonalDataQuotaBytes: 5 << 30, DefaultPersonalAttachmentBytes: 300 << 20}
	nudge := app.AdminNudgePolicy{Version: 1}
	operations := &adminOperationsServiceStub{settings: settings, invites: app.AdminInvitePage{Items: []app.AdminInvite{}, Total: 0, TotalInviters: 0},
		sites: app.AdminSitePage{Items: []app.AdminSite{}, Summary: app.AdminSiteSummary{}}, nudge: nudge, code: "0123456789abcdef0123456789abcdef"}
	deps := services.deps()
	deps.AdminOperations = operations
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/admin/settings", cookie, "")
	var got adminSettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != http.StatusOK || !got.RegistrationReviewRequired ||
		got.AutomaticNewUserModelBudget || got.AutomaticInvitationReward || got.AutomaticContributionReward || got.DefaultPersonalDataQuotaBytes != 5<<30 {
		t.Fatalf("admin settings = %d %+v err=%v body=%s", response.Code, got, err, response.Body)
	}
	invites := requestWithCookie(router, http.MethodGet, "/api/admin/invites?limit=10&offset=0", cookie, "")
	if invites.Code != http.StatusOK || !containsJSON(invites.Body.String(), `"items":[]`) || !containsJSON(invites.Body.String(), `"total_inviters":0`) {
		t.Fatalf("empty invite page = %d %s", invites.Code, invites.Body)
	}
	sites := requestWithCookie(router, http.MethodGet, "/api/admin/sites", cookie, "")
	if sites.Code != http.StatusOK || !containsJSON(sites.Body.String(), `"github_stats":"not_configured"`) ||
		!containsJSON(sites.Body.String(), `"traffic":"not_collected"`) {
		t.Fatalf("empty admin sites = %d %s", sites.Code, sites.Body)
	}
	public := request(router, http.MethodGet, "/api/nudges", "", "", "")
	if public.Code != http.StatusOK || !containsJSON(public.Body.String(), `"enabled":false`) || containsJSON(public.Body.String(), `"url":"https`) {
		t.Fatalf("public disabled nudge = %d %s", public.Code, public.Body)
	}
	code := requestWithCookie(router, http.MethodGet, "/api/me/invite-code", cookie, "")
	if code.Code != http.StatusOK || !containsJSON(code.Body.String(), operations.code) {
		t.Fatalf("invite code = %d %s", code.Code, code.Body)
	}
	services.verifier.role = identity.RoleUser
	userDeps := services.deps()
	userDeps.AdminOperations = operations
	userRouter, err := New(userDeps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/invites", cookie, "")
	assertError(t, denied, http.StatusForbidden, "forbidden")
}

func TestAdminNudgeUpdatePassesCASAndMapsConflict(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	operations := &adminOperationsServiceStub{nudge: app.AdminNudgePolicy{Version: 1}}
	deps := services.deps()
	deps.AdminOperations = operations
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	updated := requestWithCookieBody(router, http.MethodPut, "/api/admin/nudges",
		`{"expected_version":1,"enabled":true,"url":"https://github.com/morde/example","text_zh":"支持 Morde","text_en":""}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || operations.update.ExpectedVersion != 1 || !operations.update.Enabled || operations.update.IP == "" {
		t.Fatalf("nudge update = %d %s command=%+v", updated.Code, updated.Body, operations.update)
	}
	operations.updateErr = domain.NewError(domain.CodeConflict, "nudge policy version changed")
	stale := requestWithCookieBody(router, http.MethodPut, "/api/admin/nudges",
		`{"expected_version":1,"enabled":false,"url":"","text_zh":"","text_en":""}`, cookie, "https://portal.example.test")
	assertError(t, stale, http.StatusConflict, "conflict")
}

func containsJSON(value, fragment string) bool { return strings.Contains(value, fragment) }
