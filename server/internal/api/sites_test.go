package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	sitedomain "morde/server/internal/domain/site"
)

const (
	testSiteID    = "site_0123456789abcdef"
	testReleaseID = "rel_0123456789abcdef"
)

type siteServiceStub struct {
	principal        identity.Principal
	hostID           string
	workspace        string
	key              string
	siteID           sitedomain.ID
	revision         int64
	port             int
	releaseID        sitedomain.ReleaseID
	created          sitedomain.Site
	release          sitedomain.Release
	sitePage         app.SitePage
	releasePage      app.SiteReleasePage
	listPage         app.PageRequest
	listSiteID       sitedomain.ID
	listCalls        int
	listReleaseCalls int
	listErr          error
	getErr           error
	createErr        error
}

func (s *siteServiceStub) Create(_ context.Context, principal identity.Principal, hostID, workspaceID, key string) (sitedomain.Site, error) {
	s.principal, s.hostID, s.workspace, s.key = principal, hostID, workspaceID, key
	return s.created, s.createErr
}

func (s *siteServiceStub) List(_ context.Context, principal identity.Principal, page app.PageRequest) (app.SitePage, error) {
	s.principal, s.listPage = principal, page
	s.listCalls++
	return s.sitePage, s.listErr
}

func (s *siteServiceStub) ListReleases(_ context.Context, principal identity.Principal, siteID sitedomain.ID, page app.PageRequest) (app.SiteReleasePage, error) {
	s.principal, s.listSiteID, s.listPage = principal, siteID, page
	s.listReleaseCalls++
	return s.releasePage, s.listErr
}

func (s *siteServiceStub) CreateCandidate(_ context.Context, principal identity.Principal, siteID sitedomain.ID, revision int64, port int, key string) (sitedomain.Release, error) {
	s.principal, s.siteID, s.revision, s.port, s.key = principal, siteID, revision, port, key
	return s.release, s.createErr
}

func (s *siteServiceStub) Materialize(_ context.Context, principal identity.Principal, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	s.principal, s.siteID, s.releaseID = principal, siteID, releaseID
	return s.release, s.getErr
}

func (s *siteServiceStub) GetRelease(_ context.Context, principal identity.Principal, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	s.principal, s.siteID, s.releaseID = principal, siteID, releaseID
	return s.release, s.getErr
}

func TestSiteListEndpointsReturnBoundedPagesAndHideInaccessibleSites(t *testing.T) {
	services := newServices()
	now := time.Now().UTC()
	services.sites.sitePage = app.SitePage{Items: []sitedomain.Site{{
		ID: testSiteID, HostID: testHostID, WorkspaceID: testWorkspaceID, State: sitedomain.StateActive, CreatedAt: now, UpdatedAt: now,
	}}, NextCursor: "site-next"}
	services.sites.releasePage = app.SiteReleasePage{Items: []sitedomain.Release{{
		ID: testReleaseID, SiteID: testSiteID, WorkspaceID: testWorkspaceID, WorkspaceRevision: 9,
		ManifestSHA256: strings.Repeat("a", 64), ServicePort: 3000, State: sitedomain.ReleaseCandidate, CreatedAt: now, UpdatedAt: now,
	}}, NextCursor: "release-next"}
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	sites := requestWithCookie(router, http.MethodGet, "/api/sites?limit=2&cursor=before", cookie, "https://portal.example.test")
	if sites.Code != http.StatusOK || !strings.Contains(sites.Body.String(), `"items":[`) || strings.Contains(sites.Body.String(), `"items":null`) || !strings.Contains(sites.Body.String(), `"next_cursor":"site-next"`) {
		t.Fatalf("list sites = %d %s", sites.Code, sites.Body)
	}
	if services.sites.principal.Kind != identity.KindSession || services.sites.listPage != (app.PageRequest{Limit: 2, Cursor: "before"}) {
		t.Fatalf("list sites arguments = principal %#v page %#v", services.sites.principal, services.sites.listPage)
	}

	badLimit := requestWithCookie(router, http.MethodGet, "/api/sites?limit=101", cookie, "https://portal.example.test")
	if badLimit.Code != http.StatusBadRequest || services.sites.listCalls != 1 {
		t.Fatalf("list sites bad limit = %d %s; calls=%d", badLimit.Code, badLimit.Body, services.sites.listCalls)
	}
	emptyCursor := requestWithCookie(router, http.MethodGet, "/api/sites?cursor=", cookie, "https://portal.example.test")
	if emptyCursor.Code != http.StatusBadRequest || services.sites.listCalls != 1 {
		t.Fatalf("list sites empty cursor = %d %s; calls=%d", emptyCursor.Code, emptyCursor.Body, services.sites.listCalls)
	}

	releases := requestWithCookie(router, http.MethodGet, "/api/sites/"+testSiteID+"/releases?limit=3", cookie, "https://portal.example.test")
	if releases.Code != http.StatusOK || !strings.Contains(releases.Body.String(), `"items":[`) || strings.Contains(releases.Body.String(), `"items":null`) || !strings.Contains(releases.Body.String(), `"next_cursor":"release-next"`) {
		t.Fatalf("list site releases = %d %s", releases.Code, releases.Body)
	}
	if services.sites.listSiteID != testSiteID || services.sites.listPage != (app.PageRequest{Limit: 3}) || services.sites.listReleaseCalls != 1 {
		t.Fatalf("list site releases arguments = site %q page %#v calls=%d", services.sites.listSiteID, services.sites.listPage, services.sites.listReleaseCalls)
	}

	services.sites.listErr = domain.NewError(domain.CodeNotFound, "site not found")
	hidden := requestWithCookie(router, http.MethodGet, "/api/sites/"+testSiteID+"/releases", cookie, "https://portal.example.test")
	if hidden.Code != http.StatusNotFound || strings.Contains(hidden.Body.String(), testSiteID) {
		t.Fatalf("inaccessible site releases = %d %s", hidden.Code, hidden.Body)
	}
}

func TestSiteEndpointsCreateOwnerCandidateAndValidatePort(t *testing.T) {
	services := newServices()
	now := time.Now().UTC()
	services.sites.created = sitedomain.Site{ID: testSiteID, HostID: testHostID, WorkspaceID: testWorkspaceID, State: sitedomain.StateActive, CreatedAt: now, UpdatedAt: now}
	services.sites.release = sitedomain.Release{
		ID: testReleaseID, SiteID: testSiteID, WorkspaceID: testWorkspaceID, WorkspaceRevision: 9,
		ManifestSHA256: strings.Repeat("a", 64), ServicePort: 3000, State: sitedomain.ReleaseCandidate, CreatedAt: now, UpdatedAt: now,
	}
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	created := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/sites", `{"host_id":"h1234567","workspace_id":"w1234567"}`, cookie, "https://portal.example.test", "Idempotency-Key", "site-create-01")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"id":"site_0123456789abcdef"`) {
		t.Fatalf("create site = %d %s", created.Code, created.Body)
	}
	if services.sites.principal.Kind != identity.KindSession || services.sites.hostID != testHostID || services.sites.workspace != testWorkspaceID || services.sites.key != "site-create-01" {
		t.Fatalf("create site arguments = %#v host=%q workspace=%q key=%q", services.sites.principal, services.sites.hostID, services.sites.workspace, services.sites.key)
	}

	withoutKey := requestWithCookieBody(router, http.MethodPost, "/api/sites", `{"host_id":"h1234567","workspace_id":"w1234567"}`, cookie, "https://portal.example.test")
	if withoutKey.Code != http.StatusBadRequest {
		t.Fatalf("create site without idempotency key = %d %s", withoutKey.Code, withoutKey.Body)
	}

	deniedPort := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/sites/"+testSiteID+"/releases", `{"workspace_revision":9,"service_port":8080}`, cookie, "https://portal.example.test", "Idempotency-Key", "release-create-01")
	if deniedPort.Code != http.StatusBadRequest {
		t.Fatalf("create candidate on denied port = %d %s", deniedPort.Code, deniedPort.Body)
	}

	services.sites.release = sitedomain.Release{ID: testReleaseID, SiteID: testSiteID, WorkspaceID: testWorkspaceID, WorkspaceRevision: 9, ManifestSHA256: strings.Repeat("a", 64), ServicePort: 3000, State: sitedomain.ReleaseCandidate}
	candidate := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/sites/"+testSiteID+"/releases", `{"workspace_revision":9,"service_port":3000}`, cookie, "https://portal.example.test", "Idempotency-Key", "release-create-01")
	if candidate.Code != http.StatusCreated || !strings.Contains(candidate.Body.String(), `"state":"candidate"`) || strings.Contains(candidate.Body.String(), `"url"`) {
		t.Fatalf("create candidate = %d %s", candidate.Code, candidate.Body)
	}
	if services.sites.siteID != testSiteID || services.sites.revision != 9 || services.sites.port != 3000 || services.sites.key != "release-create-01" {
		t.Fatalf("candidate arguments = site=%q revision=%d port=%d key=%q", services.sites.siteID, services.sites.revision, services.sites.port, services.sites.key)
	}
}

func TestSiteMaterializeAndGetReleaseExposeSafeFailureCode(t *testing.T) {
	services := newServices()
	now := time.Now().UTC()
	services.sites.release = sitedomain.Release{
		ID: testReleaseID, SiteID: testSiteID, WorkspaceID: testWorkspaceID, WorkspaceRevision: 9,
		ManifestSHA256: strings.Repeat("a", 64), ServicePort: 3000, State: sitedomain.ReleaseCandidate,
		FailureCode: sitedomain.FailureSourceUnavailable, CreatedAt: now, UpdatedAt: now,
	}
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	path := "/api/sites/" + testSiteID + "/releases/" + testReleaseID

	materialized := requestWithCookieBody(router, http.MethodPost, path+"/materialize", "{}", cookie, "https://portal.example.test")
	if materialized.Code != http.StatusOK || !strings.Contains(materialized.Body.String(), `"state":"candidate"`) || !strings.Contains(materialized.Body.String(), `"failure_code":"source_unavailable"`) {
		t.Fatalf("materialize candidate = %d %s", materialized.Code, materialized.Body)
	}
	if services.sites.principal.Kind != identity.KindSession || services.sites.siteID != testSiteID || services.sites.releaseID != testReleaseID {
		t.Fatalf("materialize arguments = %#v site=%q release=%q", services.sites.principal, services.sites.siteID, services.sites.releaseID)
	}

	status := requestWithCookie(router, http.MethodGet, path, cookie, "https://portal.example.test")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"failure_code":"source_unavailable"`) || strings.Contains(status.Body.String(), `"error"`) {
		t.Fatalf("get release status = %d %s", status.Code, status.Body)
	}
}
