package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type adminHostResourcesStub struct {
	snapshot  app.HostResourceSnapshot
	err       error
	updates   int
	resources app.HostResources
	version   int64
	principal identity.Principal
}

func (s *adminHostResourcesStub) Get(_ context.Context, principal identity.Principal, _ string) (app.HostResourceSnapshot, error) {
	s.principal = principal
	return s.snapshot, s.err
}

func (s *adminHostResourcesStub) Update(_ context.Context, principal identity.Principal, _ string, resources app.HostResources, version int64) (app.HostResourceSnapshot, error) {
	s.principal, s.resources, s.version = principal, resources, version
	s.updates++
	return s.snapshot, s.err
}

func adminResourcesRouter(t *testing.T, service *adminHostResourcesStub, role identity.Role) *gin.Engine {
	t.Helper()
	deps := newServices().deps()
	deps.Verifier = verifierStub{role: role}
	deps.AdminHostResources = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAdminHostResourcesGetAndUpdate(t *testing.T) {
	service := &adminHostResourcesStub{snapshot: app.HostResourceSnapshot{
		HostID: testHostID, State: host.StateRunning, Resources: app.HostResources{CPUMilli: 2000, MemMiB: 4096}, Version: 3, UpdateState: "idle",
	}}
	router := adminResourcesRouter(t, service, identity.RoleAdmin)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	got := requestWithCookie(router, http.MethodGet, "/api/admin/hosts/"+testHostID+"/resources", cookie, "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"update_state":"idle"`) || !strings.Contains(got.Body.String(), `"cpu_milli":2000`) {
		t.Fatalf("GET resources = %d %s", got.Code, got.Body)
	}
	changed := requestWithCookieBody(router, http.MethodPatch, "/api/admin/hosts/"+testHostID+"/resources", `{"cpu_milli":3000,"mem_mib":6144,"expected_version":3}`, cookie, "https://portal.example.test")
	if changed.Code != http.StatusOK || service.updates != 1 || service.version != 3 || service.resources != (app.HostResources{CPUMilli: 3000, MemMiB: 6144}) || service.principal.Role != identity.RoleAdmin {
		t.Fatalf("PATCH resources = %d %s; service=%#v", changed.Code, changed.Body, service)
	}
}

func TestAdminHostResourcesRejectsNonAdminAndInvalidRequests(t *testing.T) {
	service := &adminHostResourcesStub{snapshot: app.HostResourceSnapshot{HostID: testHostID, State: host.StateStopped, UpdateState: "idle"}}
	userRouter := adminResourcesRouter(t, service, identity.RoleUser)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	if got := requestWithCookieBody(userRouter, http.MethodPatch, "/api/admin/hosts/"+testHostID+"/resources", `{"cpu_milli":1000,"mem_mib":2048,"expected_version":1}`, cookie, "https://portal.example.test"); got.Code != http.StatusForbidden {
		t.Fatalf("non-admin update = %d %s", got.Code, got.Body)
	}
	adminRouter := adminResourcesRouter(t, service, identity.RoleAdmin)
	invalid := requestWithCookieBody(adminRouter, http.MethodPatch, "/api/admin/hosts/"+testHostID+"/resources", `{"cpu_milli":1000,"mem_mib":2048,"expected_version":1,"owner_id":"u1234567"}`, cookie, "https://portal.example.test")
	if invalid.Code != http.StatusBadRequest || service.updates != 0 {
		t.Fatalf("unexpected input = %d %s", invalid.Code, invalid.Body)
	}
	service.err = domain.NewError(domain.CodeConflict, "resource version changed")
	conflict := requestWithCookieBody(adminRouter, http.MethodPatch, "/api/admin/hosts/"+testHostID+"/resources", `{"cpu_milli":1000,"mem_mib":2048,"expected_version":1}`, cookie, "https://portal.example.test")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("version conflict = %d %s", conflict.Code, conflict.Body)
	}
}
