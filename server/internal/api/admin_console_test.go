package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type adminConsoleStub struct {
	agents         map[string]int64
	installations  app.AdminPage[app.AdminInstallation]
	authorizations app.AdminPage[app.AdminAuthorization]
	calls          app.AdminPage[app.AdminModelCall]
	usage          app.AdminUsage
	stats          app.AdminStats
	principal      identity.Principal
	usageDays      int
	err            error
}

func (s *adminConsoleStub) AgentStates(_ context.Context, principal identity.Principal) (map[string]int64, error) {
	s.principal = principal
	return s.agents, s.err
}
func (s *adminConsoleStub) Installations(_ context.Context, principal identity.Principal, _ string, limit, offset int) (app.AdminPage[app.AdminInstallation], error) {
	s.principal = principal
	page := s.installations
	page.Limit, page.Offset = limit, offset
	return page, s.err
}
func (s *adminConsoleStub) Authorizations(_ context.Context, principal identity.Principal, _ string, limit, offset int) (app.AdminPage[app.AdminAuthorization], error) {
	s.principal = principal
	page := s.authorizations
	page.Limit, page.Offset = limit, offset
	return page, s.err
}
func (s *adminConsoleStub) ModelCalls(_ context.Context, principal identity.Principal, _ string, limit, offset int) (app.AdminPage[app.AdminModelCall], error) {
	s.principal = principal
	page := s.calls
	page.Limit, page.Offset = limit, offset
	return page, s.err
}
func (s *adminConsoleStub) Usage(_ context.Context, principal identity.Principal, days int) (app.AdminUsage, error) {
	s.principal, s.usageDays = principal, days
	return s.usage, s.err
}
func (s *adminConsoleStub) Stats(_ context.Context, principal identity.Principal) (app.AdminStats, error) {
	s.principal = principal
	return s.stats, s.err
}

type adminPresenceStub struct {
	count  int
	online bool
}

func (s adminPresenceStub) Serve(context.Context, identity.Principal, app.HubPeer) error { return nil }
func (s adminPresenceStub) OnlineInstallationCount() int                                 { return s.count }
func (s adminPresenceStub) InstallationOnline(string, string) bool                       { return s.online }

type adminAuditWriterStub struct {
	entries []audit.Entry
	err     error
}

func (s *adminAuditWriterStub) Write(_ context.Context, entry audit.Entry) error {
	s.entries = append(s.entries, entry)
	return s.err
}

func (s *adminServiceStub) GetUser(_ context.Context, principal identity.Principal, _ string) (AdminUserRecord, error) {
	s.principal = principal
	return s.user, s.err
}

func (s *adminServiceStub) ListUserPage(_ context.Context, principal identity.Principal, status *identity.AccountStatus, limit, offset int) (app.AdminPage[AdminUserRecord], error) {
	s.principal, s.status = principal, status
	start := min(offset, len(s.users))
	end := min(start+limit, len(s.users))
	return app.AdminPage[AdminUserRecord]{Items: s.users[start:end], Total: int64(len(s.users)), Limit: limit, Offset: offset}, s.err
}

func TestAdminConsoleAPIsUseRealViewsAndProtectSensitiveFields(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	checkedAt := time.Now().UTC()
	services.admin.capacity = CapacityRecord{
		Limit:        TotalsView{Hosts: 10, CPUMilli: 8000, MemMiB: 16384},
		Allocated:    TotalsView{Hosts: 2, CPUMilli: 2000, MemMiB: 4096},
		HostsByState: map[host.State]int{host.StateRunning: 2},
	}
	services.admin.user = AdminUserRecord{
		ID: testUserID, Name: "alice", Role: identity.RoleUser, Status: identity.StatusApproved,
		Quota: TotalsView{Hosts: 2}, Usage: TotalsView{Hosts: 1},
		DefaultHostResources: HostResourcesView{CPUMilli: 1000, MemMiB: 2048}, CreatedAt: checkedAt,
	}
	inputTokens, outputTokens := int64(8), int64(3)
	console := &adminConsoleStub{
		agents:         map[string]int64{"running": 1},
		installations:  app.AdminPage[app.AdminInstallation]{Items: []app.AdminInstallation{{ID: "i1234567", DisplayName: "Laptop", Platform: "desktop", CreatedAt: checkedAt}}, Total: 1},
		authorizations: app.AdminPage[app.AdminAuthorization]{Items: []app.AdminAuthorization{{ID: "t1234567", DisplayName: "Laptop", Platform: "desktop", State: "claimed", CreatedAt: checkedAt, ExpiresAt: checkedAt}}, Total: 1},
		calls:          app.AdminPage[app.AdminModelCall]{Items: []app.AdminModelCall{{Source: "client", CallID: "call-123456", ModelID: "p1234567", ProviderModel: "model", ReservedTokens: 20, State: "settled", InputTokens: &inputTokens, OutputTokens: &outputTokens, CreatedAt: checkedAt}}, Total: 1},
		usage: app.AdminUsage{Days: 30, From: checkedAt.Add(-30 * 24 * time.Hour), To: checkedAt,
			Summary: app.AdminUsageSummary{Calls: 3, SettledCalls: 2, UnknownCalls: 1, InputTokens: 8, OutputTokens: 3, ActiveAccounts: 1}},
		stats: app.AdminStats{Users: app.AdminUserCounts{Total: 4, Approved: 2, Pending: 1, Rejected: 0, Disabled: 1},
			Platforms: []app.AdminPlatformStats{{Platform: "desktop", Installations: 3, ActiveInstallations: 2, Users: 2}},
			Cohorts:   []app.AdminCohortStats{{Week: "2026-W44", Signups: 2, WithInstallation: 1}}},
	}
	models := &modelConnectionManagerStub{items: []app.ModelConnectionView{
		{ID: "p1234567", Name: "Shared", BaseURL: "https://provider.example/v1", ModelName: "model", Shared: true, HasAPIKey: true, CreatedAt: checkedAt, UpdatedAt: checkedAt},
		{ID: "p7654321", Name: "Private", BaseURL: "https://private.example/v1", ModelName: "other", Shared: false, HasAPIKey: true, CreatedAt: checkedAt, UpdatedAt: checkedAt},
	}}
	deps := services.deps()
	deps.AdminConsole = console
	deps.ModelConnections = models
	deps.Hub = adminPresenceStub{count: 2, online: true}
	deps.Readiness = readinessStub{err: errors.New("postgres password and internal URL")}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	key := "Bearer " + apiKeyCredential()

	overview := request(router, http.MethodGet, "/api/admin/overview", "", key, "")
	if overview.Code != http.StatusOK || !strings.Contains(overview.Body.String(), `"online_installations":2`) ||
		!strings.Contains(overview.Body.String(), `"agents_by_state":{"running":1}`) ||
		!strings.Contains(overview.Body.String(), `"cpu_milli":8000`) || !strings.Contains(overview.Body.String(), "一个或多个平台依赖尚未就绪") ||
		strings.Contains(overview.Body.String(), "postgres password") || strings.Contains(overview.Body.String(), "internal URL") {
		t.Fatalf("admin overview = %d %s", overview.Code, overview.Body)
	}

	connections := request(router, http.MethodGet, "/api/admin/model-connections", "", key, "")
	if connections.Code != http.StatusOK || !strings.Contains(connections.Body.String(), "Shared") || !strings.Contains(connections.Body.String(), `"configuration_status":"configured"`) || strings.Contains(connections.Body.String(), "Private") || strings.Contains(connections.Body.String(), `"api_key":`) {
		t.Fatalf("admin model connections = %d %s", connections.Code, connections.Body)
	}

	usage := request(router, http.MethodGet, "/api/admin/usage?days=30", "", key, "")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), `"calls":3`) || console.usageDays != 30 {
		t.Fatalf("admin usage = %d %s; days=%d", usage.Code, usage.Body, console.usageDays)
	}
	if response := request(router, http.MethodGet, "/api/admin/usage?days=31", "", key, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid admin usage period = %d %s", response.Code, response.Body)
	}
	stats := request(router, http.MethodGet, "/api/admin/stats", "", key, "")
	if stats.Code != http.StatusOK || !strings.Contains(stats.Body.String(), `"rejected":0`) || !strings.Contains(stats.Body.String(), "2026-W44") {
		t.Fatalf("admin stats = %d %s", stats.Code, stats.Body)
	}

	user := request(router, http.MethodGet, "/api/admin/users/"+testUserID, "", key, "")
	if user.Code != http.StatusOK || !strings.Contains(user.Body.String(), `"name":"alice"`) {
		t.Fatalf("admin user detail = %d %s", user.Code, user.Body)
	}
	installations := request(router, http.MethodGet, "/api/admin/users/"+testUserID+"/installations?limit=10&offset=5", "", key, "")
	if installations.Code != http.StatusOK || !strings.Contains(installations.Body.String(), `"online":true`) || !strings.Contains(installations.Body.String(), `"offset":5`) {
		t.Fatalf("admin user installations = %d %s", installations.Code, installations.Body)
	}
	authorizations := request(router, http.MethodGet, "/api/admin/users/"+testUserID+"/authorizations", "", key, "")
	if authorizations.Code != http.StatusOK || !strings.Contains(authorizations.Body.String(), "claimed") || strings.Contains(authorizations.Body.String(), "secret_hash") || strings.Contains(authorizations.Body.String(), "user_code") {
		t.Fatalf("admin user authorizations = %d %s", authorizations.Code, authorizations.Body)
	}
	ledger := request(router, http.MethodGet, "/api/admin/users/"+testUserID+"/model-usage/ledger", "", key, "")
	if ledger.Code != http.StatusOK || !strings.Contains(ledger.Body.String(), `"input_tokens":8`) || strings.Contains(ledger.Body.String(), "request_digest") {
		t.Fatalf("admin user model ledger = %d %s", ledger.Code, ledger.Body)
	}
	if console.principal.Role != identity.RoleAdmin {
		t.Fatalf("admin console received wrong principal: %#v", console.principal)
	}
	var page map[string]any
	if err := json.Unmarshal(ledger.Body.Bytes(), &page); err != nil || page["total"] != float64(1) {
		t.Fatalf("ledger page metadata = %#v, %v", page, err)
	}
}

func TestAdminConsoleRoutesRequireAdministratorAndValidatePage(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.AdminConsole = &adminConsoleStub{}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	key := "Bearer " + apiKeyCredential()
	for _, path := range []string{
		"/api/admin/overview", "/api/admin/model-connections", "/api/admin/usage", "/api/admin/stats",
		"/api/admin/users/" + testUserID, "/api/admin/users/" + testUserID + "/installations",
		"/api/admin/users/" + testUserID + "/authorizations", "/api/admin/users/" + testUserID + "/model-usage/ledger",
	} {
		if response := request(router, http.MethodGet, path, "", key, ""); response.Code != http.StatusForbidden {
			t.Fatalf("non-admin GET %s = %d %s", path, response.Code, response.Body)
		}
	}
	services.verifier.role = identity.RoleAdmin
	router = newRouter(t, services, Options{})
	for _, path := range []string{
		"/api/admin/users/" + testUserID + "/installations?limit=101",
		"/api/admin/users/" + testUserID + "/authorizations?offset=-1",
		"/api/admin/users/" + testUserID + "/model-usage/ledger?limit=1&limit=2",
	} {
		if response := request(router, http.MethodGet, path, "", key, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid page GET %s = %d %s", path, response.Code, response.Body)
		}
	}
}

func TestAdminModelConnectionMutationWritesSecretFreeAudit(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	checkedAt := time.Now().UTC()
	manager := &modelConnectionManagerStub{created: app.ModelConnectionView{
		ID: "p1234567", Name: "Shared", BaseURL: "https://provider.example/v1", ModelName: "model", Shared: true,
		HasAPIKey: true, CreatedAt: checkedAt, UpdatedAt: checkedAt,
	}}
	writer := &adminAuditWriterStub{}
	deps := services.deps()
	deps.ModelConnections = manager
	deps.AdminAudit = writer
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBody(router, http.MethodPost, "/api/model-connections", `{"name":"Shared","base_url":"https://provider.example/v1","model_name":"model","api_key":"do-not-audit-this-key","shared":true}`, cookie, "https://portal.example.test")
	if response.Code != http.StatusCreated || len(writer.entries) != 1 {
		t.Fatalf("admin create response=%d %s audits=%#v", response.Code, response.Body, writer.entries)
	}
	entry := writer.entries[0]
	if entry.Action != audit.ActionModelConnectionCreate || entry.Target != manager.created.ID || entry.ActorID == "" ||
		entry.Detail["base_host"] != "provider.example" || entry.Detail["has_api_key"] != true ||
		strings.Contains(response.Body.String(), "do-not-audit-this-key") {
		t.Fatalf("model connection audit or response leaked secret: entry=%#v body=%s", entry, response.Body)
	}
}
