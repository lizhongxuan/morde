package api

import (
	"net/http"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestAdminWorkspaceEndpointsAreAdminOnlyAndReturnMetadata(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	listed := requestWithCookie(router, http.MethodGet, "/api/admin/workspaces?limit=1&cursor=before", cookie, "")
	body := listed.Body.String()
	for _, want := range []string{`"items":[`, `"next_cursor":"next-page"`, `"owner_id":"` + testUserID + `"`, `"host_id":"` + testHostID + `"`, `"sync_state":"healthy"`} {
		if listed.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("admin workspace list = %d %s; missing %q", listed.Code, body, want)
		}
	}
	for _, forbidden := range []string{"directory", "manifest", "file_content", "secret_hash", "block_digest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("admin workspace list exposed %q: %s", forbidden, body)
		}
	}
	if services.adminWorkspaces.pageReq != (app.PageRequest{Limit: 1, Cursor: "before"}) || services.adminWorkspaces.principal.Role != identity.RoleAdmin {
		t.Fatalf("admin workspace list request = %#v principal=%#v", services.adminWorkspaces.pageReq, services.adminWorkspaces.principal)
	}

	detail := requestWithCookie(router, http.MethodGet, "/api/admin/workspaces/"+testWorkspaceID, cookie, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"id":"`+testWorkspaceID+`"`) || !strings.Contains(detail.Body.String(), `"host_name":"worker"`) {
		t.Fatalf("admin workspace detail = %d %s", detail.Code, detail.Body)
	}
	services.adminWorkspaces.err = domain.NewError(domain.CodeNotFound, "workspace not found")
	missing := requestWithCookie(router, http.MethodGet, "/api/admin/workspaces/"+testWorkspaceID, cookie, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing admin workspace = %d %s", missing.Code, missing.Body)
	}

	nonAdminServices := newServices()
	nonAdminRouter := newRouter(t, nonAdminServices, Options{})
	userCookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	deniedList := requestWithCookie(nonAdminRouter, http.MethodGet, "/api/admin/workspaces", userCookie, "")
	deniedDetail := requestWithCookie(nonAdminRouter, http.MethodGet, "/api/admin/workspaces/"+testWorkspaceID, userCookie, "")
	if deniedList.Code != http.StatusForbidden || deniedDetail.Code != http.StatusForbidden {
		t.Fatalf("non-admin workspaces list/detail = %d/%d", deniedList.Code, deniedDetail.Code)
	}
	if response := request(nonAdminRouter, http.MethodGet, "/api/admin/workspaces", "", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin workspace list = %d %s", response.Code, response.Body)
	}
}

func TestAdminWorkspaceListRejectsInvalidPagination(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, path := range []string{"/api/admin/workspaces?limit=0", "/api/admin/workspaces?limit=1&limit=2"} {
		response := requestWithCookie(router, http.MethodGet, path, cookie, "")
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d %s, want 400", path, response.Code, response.Body)
		}
	}
}
