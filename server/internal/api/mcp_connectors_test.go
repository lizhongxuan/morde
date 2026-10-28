package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type mcpConnectorManagerStub struct {
	item       app.MCPConnectorView
	command    app.CreateMCPConnectorCommand
	update     app.UpdateMCPConnectorCommand
	rotation   app.RotateMCPConnectorCredentialCommand
	revision   app.MCPConnectorRevisionCommand
	enabled    bool
	deletes    int
	creates    int
	updates    int
	rotations  int
	stateSets  int
	oauthStart app.MCPConnectorOAuthStart
	oauthCalls int
	oauthArgs  [5]string
	oauthRev   int64
	callback   [4]string
}

func (s *mcpConnectorManagerStub) List(context.Context, identity.Principal) ([]app.MCPConnectorView, error) {
	return []app.MCPConnectorView{s.item}, nil
}
func (s *mcpConnectorManagerStub) Get(context.Context, identity.Principal, string) (app.MCPConnectorView, error) {
	return s.item, nil
}
func (s *mcpConnectorManagerStub) Create(_ context.Context, _ identity.Principal, command app.CreateMCPConnectorCommand) (app.MCPConnectorView, error) {
	s.creates++
	s.command = command
	return s.item, nil
}
func (s *mcpConnectorManagerStub) Update(_ context.Context, _ identity.Principal, _ string, command app.UpdateMCPConnectorCommand) (app.MCPConnectorView, error) {
	s.updates++
	s.update = command
	return s.item, nil
}
func (s *mcpConnectorManagerStub) RotateCredential(_ context.Context, _ identity.Principal, _ string, command app.RotateMCPConnectorCredentialCommand) (app.MCPConnectorView, error) {
	s.rotations++
	s.rotation = command
	return s.item, nil
}
func (s *mcpConnectorManagerStub) SetEnabled(_ context.Context, _ identity.Principal, _ string, enabled bool, command app.MCPConnectorRevisionCommand) (app.MCPConnectorView, error) {
	s.stateSets++
	s.enabled, s.revision = enabled, command
	return s.item, nil
}
func (s *mcpConnectorManagerStub) Delete(_ context.Context, _ identity.Principal, _ string, command app.MCPConnectorRevisionCommand) error {
	s.deletes++
	s.revision = command
	return nil
}

func (s *mcpConnectorManagerStub) BeginOAuth(_ context.Context, _ identity.Principal, _ string, revision int64, clientID, clientSecret string) (app.MCPConnectorOAuthStart, error) {
	s.oauthCalls++
	s.oauthRev = revision
	s.oauthArgs = [5]string{"u1234567", clientID, clientSecret, s.item.ID, s.item.EndpointURL}
	if s.oauthStart.Kind == "" {
		s.oauthStart = app.MCPConnectorOAuthStart{Kind: "authorize", AuthorizationURL: "https://auth.example.test/authorize?state=opaque", RedirectURI: "https://portal.example.test/api/mcp/oauth/callback"}
	}
	return s.oauthStart, nil
}

func (s *mcpConnectorManagerStub) CompleteOAuth(_ context.Context, state, code, providerError, issuer string) error {
	s.callback = [4]string{state, code, providerError, issuer}
	return nil
}

func TestMCPConnectorAPIStoresWriteOnlyCredentialAndRequiresSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &mcpConnectorManagerStub{item: testMCPConnectorView()}
	services := newServices()
	deps := services.deps()
	deps.MCPConnectors = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	body := `{"name":"Notion","endpoint_url":"https://mcp.notion.com/mcp","credential":{"kind":"oauth","access_token":"secret-token"}}`
	response := requestWithCookieBody(router, http.MethodPost, "/api/mcp/connectors", body, cookie, "https://portal.example.test")
	if response.Code != http.StatusCreated || manager.creates != 1 || !strings.Contains(response.Body.String(), `"has_credential":true`) ||
		strings.Contains(response.Body.String(), "secret-token") || strings.Contains(response.Body.String(), `"credential"`) ||
		!strings.Contains(response.Body.String(), `"id":"k1234567"`) {
		t.Fatalf("create response = %d %s; creates=%d", response.Code, response.Body, manager.creates)
	}
	if string(manager.command.CredentialJSON) != `{"kind":"oauth","access_token":"secret-token"}` {
		t.Fatalf("credential was not forwarded to service: %s", manager.command.CredentialJSON)
	}
	unknown := requestWithCookieBody(router, http.MethodPost, "/api/mcp/connectors", `{"name":"Notion","endpoint_url":"https://mcp.notion.com/mcp","secret":"bad"}`, cookie, "https://portal.example.test")
	if unknown.Code != http.StatusBadRequest || manager.creates != 1 {
		t.Fatalf("unknown field response = %d %s; creates=%d", unknown.Code, unknown.Body, manager.creates)
	}
	apiKey := request(router, http.MethodGet, "/api/mcp/connectors", "", "Bearer "+apiKeyCredential(), "")
	if apiKey.Code != http.StatusForbidden {
		t.Fatalf("API key connector list = %d %s", apiKey.Code, apiKey.Body)
	}
}

func TestMCPConnectorAPIRoutesUseExpectedRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &mcpConnectorManagerStub{item: testMCPConnectorView()}
	services := newServices()
	deps := services.deps()
	deps.MCPConnectors = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	update := requestWithCookieBody(router, http.MethodPut, "/api/mcp/connectors/k1234567", `{"name":"Notion","endpoint_url":"https://mcp.notion.com/mcp","expected_revision":4}`, cookie, origin)
	if update.Code != http.StatusOK || manager.updates != 1 || manager.update.ExpectedRevision != 4 {
		t.Fatalf("update = %d %s; request=%#v", update.Code, update.Body, manager.update)
	}
	rotate := requestWithCookieBody(router, http.MethodPut, "/api/mcp/connectors/k1234567/credential", `{"credential":{"token":"new"},"expected_revision":5}`, cookie, origin)
	if rotate.Code != http.StatusOK || manager.rotations != 1 || manager.rotation.ExpectedRevision != 5 || string(manager.rotation.CredentialJSON) != `{"token":"new"}` ||
		strings.Contains(rotate.Body.String(), "new") {
		t.Fatalf("rotate = %d %s; request=%#v", rotate.Code, rotate.Body, manager.rotation)
	}
	enabled := requestWithCookieBody(router, http.MethodPut, "/api/mcp/connectors/k1234567/enabled", `{"enabled":false,"expected_revision":6}`, cookie, origin)
	if enabled.Code != http.StatusOK || manager.stateSets != 1 || manager.enabled || manager.revision.ExpectedRevision != 6 {
		t.Fatalf("enabled = %d %s; enabled=%t revision=%+v", enabled.Code, enabled.Body, manager.enabled, manager.revision)
	}
	deleted := requestWithCookieBody(router, http.MethodDelete, "/api/mcp/connectors/k1234567?expected_revision=7", "", cookie, origin)
	if deleted.Code != http.StatusNoContent || manager.deletes != 1 || manager.revision.ExpectedRevision != 7 {
		t.Fatalf("delete = %d %s; deletes=%d revision=%+v", deleted.Code, deleted.Body, manager.deletes, manager.revision)
	}
	missingRevision := requestWithCookieBody(router, http.MethodDelete, "/api/mcp/connectors/k1234567", "", cookie, origin)
	if missingRevision.Code != http.StatusBadRequest || manager.deletes != 1 {
		t.Fatalf("delete without revision = %d %s; deletes=%d", missingRevision.Code, missingRevision.Body, manager.deletes)
	}
}

func TestMCPConnectorOAuthAuthorizationAndUnauthenticatedCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &mcpConnectorManagerStub{item: testMCPConnectorView()}
	services := newServices()
	deps := services.deps()
	deps.MCPConnectors = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	started := requestWithCookieBody(router, http.MethodPost, "/api/mcp/connectors/k1234567/oauth/authorize",
		`{"expected_revision":3,"client_id":"client-id","client_secret":"client-secret"}`, cookie, origin)
	if started.Code != http.StatusOK || manager.oauthCalls != 1 || manager.oauthRev != 3 || manager.oauthArgs[1] != "client-id" || manager.oauthArgs[2] != "client-secret" ||
		!strings.Contains(started.Body.String(), `"kind":"authorize"`) || !strings.Contains(started.Body.String(), "authorization_url") ||
		strings.Contains(started.Body.String(), "client-secret") {
		t.Fatalf("OAuth start = %d %s; calls=%d revision=%d args=%#v", started.Code, started.Body, manager.oauthCalls, manager.oauthRev, manager.oauthArgs)
	}
	unauthorized := request(router, http.MethodPost, "/api/mcp/connectors/k1234567/oauth/authorize", `{"expected_revision":3}`, "Bearer "+apiKeyCredential(), origin)
	if unauthorized.Code != http.StatusForbidden || manager.oauthCalls != 1 {
		t.Fatalf("API key OAuth start = %d %s; calls=%d", unauthorized.Code, unauthorized.Body, manager.oauthCalls)
	}
	callback := request(router, http.MethodGet,
		"/api/mcp/oauth/callback?state=opaque&code=code-value&iss=https%3A%2F%2Fauth.example.test", "", "", "")
	if callback.Code != http.StatusOK || callback.Header().Get("Cache-Control") != "no-store" || callback.Header().Get("X-Robots-Tag") != "noindex, nofollow" ||
		manager.callback != [4]string{"opaque", "code-value", "", "https://auth.example.test"} {
		t.Fatalf("OAuth callback = %d headers=%v body=%q args=%#v", callback.Code, callback.Header(), callback.Body, manager.callback)
	}
	denied := request(router, http.MethodGet, "/api/mcp/oauth/callback?state=opaque&error=access_denied&error_description=do-not-reflect", "", "", "")
	if denied.Code != http.StatusOK || strings.Contains(denied.Body.String(), "do-not-reflect") || manager.callback[2] != "access_denied" {
		t.Fatalf("OAuth denial callback = %d body=%q args=%#v", denied.Code, denied.Body, manager.callback)
	}
}

func testMCPConnectorView() app.MCPConnectorView {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	return app.MCPConnectorView{
		ID: "k1234567", Name: "Notion", EndpointURL: "https://mcp.notion.com/mcp", Enabled: false,
		HasCredential: true, CredentialVersion: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
