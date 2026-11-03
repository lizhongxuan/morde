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
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectMCPConnectorRoutesStub struct {
	principal       identity.Principal
	projectID       string
	connectorID     string
	expectedVersion int64
	items           []app.ProjectMCPConnector
	grant           app.ProjectMCPConnector
	revoked         app.ProjectMCPConnector
	listCalls       int
	grantCalls      int
	revokeCalls     int
}

func (s *projectMCPConnectorRoutesStub) List(_ context.Context, principal identity.Principal, projectID string) ([]app.ProjectMCPConnector, error) {
	s.listCalls++
	s.principal, s.projectID = principal, projectID
	return s.items, nil
}

func (s *projectMCPConnectorRoutesStub) Grant(_ context.Context, principal identity.Principal, projectID, connectorID string, expectedVersion int64) (app.ProjectMCPConnector, error) {
	s.grantCalls++
	s.principal, s.projectID, s.connectorID, s.expectedVersion = principal, projectID, connectorID, expectedVersion
	return s.grant, nil
}

func (s *projectMCPConnectorRoutesStub) Revoke(_ context.Context, principal identity.Principal, projectID, connectorID string, expectedVersion int64) (app.ProjectMCPConnector, error) {
	s.revokeCalls++
	s.principal, s.projectID, s.connectorID, s.expectedVersion = principal, projectID, connectorID, expectedVersion
	return s.revoked, nil
}

func projectMCPConnectorAPIRouter(principal *identity.Principal, service projectMCPConnectorRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterProjectMCPConnectorRoutes(router.Group("/api"), service)
	return router
}

func projectMCPConnectorAPIRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func projectMCPConnectorAPIFixture(active bool) app.ProjectMCPConnector {
	now := time.Now().UTC()
	grant := project.MCPConnectorGrant{
		ProjectID: "j1234567", ConnectorOwnerID: "u1234567", ConnectorID: "k1234567", Version: 2,
		AuthorizedBy: "u1234567", Active: active, CreatedAt: now, UpdatedAt: now,
	}
	item := app.ProjectMCPConnector{Grant: grant, Name: "Calendar", Enabled: true, ConnectorRevision: 3}
	if !active {
		revokedAt := now
		item.Grant.RevokedAt = &revokedAt
	}
	return item
}

func TestProjectMCPConnectorRoutesListGrantAndRevokeWithoutCredentialFields(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &projectMCPConnectorRoutesStub{items: []app.ProjectMCPConnector{projectMCPConnectorAPIFixture(true)},
		grant: projectMCPConnectorAPIFixture(true), revoked: projectMCPConnectorAPIFixture(false)}
	router := projectMCPConnectorAPIRouter(&principal, service)

	got := projectMCPConnectorAPIRequest(router, http.MethodGet, "/api/projects/j1234567/mcp-connectors", "")
	var listed projectMCPConnectorListResponse
	if err := json.Unmarshal(got.Body.Bytes(), &listed); err != nil || got.Code != http.StatusOK || service.listCalls != 1 ||
		service.projectID != "j1234567" || service.principal.UserID != principal.UserID || len(listed.Items) != 1 ||
		listed.Items[0].ConnectorOwnerID != principal.UserID || listed.Items[0].ConnectorID != "k1234567" || !listed.Items[0].Enabled {
		t.Fatalf("list project connectors = %d %s, response=%#v service=%#v err=%v", got.Code, got.Body, listed, service, err)
	}
	if strings.Contains(got.Body.String(), "credential") || strings.Contains(got.Body.String(), "endpoint_url") {
		t.Fatalf("project connector response leaked secret/configuration data: %s", got.Body.String())
	}

	got = projectMCPConnectorAPIRequest(router, http.MethodPut, "/api/projects/j1234567/mcp-connectors/k1234567", `{"expected_version":0}`)
	if got.Code != http.StatusOK || service.grantCalls != 1 || service.projectID != "j1234567" ||
		service.connectorID != "k1234567" || service.expectedVersion != 0 || service.principal.UserID != principal.UserID {
		t.Fatalf("grant project connector = %d %s service=%#v", got.Code, got.Body, service)
	}

	got = projectMCPConnectorAPIRequest(router, http.MethodDelete, "/api/projects/j1234567/mcp-connectors/k1234567", `{"expected_version":1}`)
	var revoked projectMCPConnectorResponse
	if err := json.Unmarshal(got.Body.Bytes(), &revoked); err != nil || got.Code != http.StatusOK || service.revokeCalls != 1 ||
		service.expectedVersion != 1 || revoked.Active || revoked.GrantVersion != 2 || revoked.RevokedAt == nil {
		t.Fatalf("revoke project connector = %d %s response=%#v service=%#v err=%v", got.Code, got.Body, revoked, service, err)
	}
}

func TestProjectMCPConnectorRoutesRejectInvalidAndUnauthenticatedRequests(t *testing.T) {
	service := &projectMCPConnectorRoutesStub{}
	if got := projectMCPConnectorAPIRequest(projectMCPConnectorAPIRouter(nil, service), http.MethodGet,
		"/api/projects/j1234567/mcp-connectors", ""); got.Code != http.StatusUnauthorized || service.listCalls != 0 {
		t.Fatalf("unauthenticated list = %d %s calls=%d", got.Code, got.Body, service.listCalls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := projectMCPConnectorAPIRouter(&principal, service)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/projects/bad/mcp-connectors", ""},
		{http.MethodPut, "/api/projects/j1234567/mcp-connectors/bad", `{"expected_version":0}`},
		{http.MethodPut, "/api/projects/j1234567/mcp-connectors/k1234567", `{}`},
		{http.MethodPut, "/api/projects/j1234567/mcp-connectors/k1234567", `{"expected_version":0,"owner_id":"u7654321"}`},
		{http.MethodDelete, "/api/projects/j1234567/mcp-connectors/k1234567", `{"expected_version":0}`},
	} {
		got := projectMCPConnectorAPIRequest(router, test.method, test.path, test.body)
		if got.Code != http.StatusBadRequest || service.listCalls != 0 || service.grantCalls != 0 || service.revokeCalls != 0 {
			t.Fatalf("invalid request %s %s = %d %s calls=%d/%d/%d", test.method, test.path, got.Code, got.Body,
				service.listCalls, service.grantCalls, service.revokeCalls)
		}
	}
	if got := projectMCPConnectorAPIRequest(projectMCPConnectorAPIRouter(&principal, nil), http.MethodGet,
		"/api/projects/j1234567/mcp-connectors", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing service list = %d %s", got.Code, got.Body)
	}
}
