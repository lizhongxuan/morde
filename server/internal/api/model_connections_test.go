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

type modelConnectionManagerStub struct {
	created   app.ModelConnectionView
	items     []app.ModelConnectionView
	principal identity.Principal
	command   app.ModelConnectionCommand
	calls     int
	checkErr  error
}

func (s *modelConnectionManagerStub) List(_ context.Context, principal identity.Principal) ([]app.ModelConnectionView, error) {
	s.principal = principal
	return s.items, nil
}

func (s *modelConnectionManagerStub) Get(context.Context, identity.Principal, string) (app.ModelConnectionView, error) {
	return s.created, nil
}

func (s *modelConnectionManagerStub) Create(_ context.Context, _ identity.Principal, command app.ModelConnectionCommand) (app.ModelConnectionView, error) {
	s.calls++
	s.command = command
	return s.created, nil
}

func (s *modelConnectionManagerStub) Update(_ context.Context, _ identity.Principal, _ string, command app.ModelConnectionCommand) (app.ModelConnectionView, error) {
	s.calls++
	s.command = command
	return s.created, nil
}

func (*modelConnectionManagerStub) Delete(context.Context, identity.Principal, string) error {
	return nil
}

func (s *modelConnectionManagerStub) Check(context.Context, identity.Principal, string) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, s.checkErr
}

func TestModelConnectionAPIRequiresSessionAndNeverReturnsAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &modelConnectionManagerStub{created: app.ModelConnectionView{
		ID: "p1234567", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
		HasAPIKey: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	services := newServices()
	deps := services.deps()
	deps.ModelConnections = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	body := `{"name":"OpenAI","base_url":"https://api.openai.com/v1","model_name":"gpt-test","api_key":"secret-model-key","shared":false}`
	response := requestWithCookieBody(router, http.MethodPost, "/api/model-connections", body, cookie, "https://portal.example.test")
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "secret-model-key") || strings.Contains(response.Body.String(), `"api_key"`) {
		t.Fatalf("create response = %d %s", response.Code, response.Body)
	}
	if manager.calls != 1 || manager.command.APIKey != "secret-model-key" {
		t.Fatalf("create command not forwarded: calls=%d command=%+v", manager.calls, manager.command)
	}

	unknown := requestWithCookieBody(router, http.MethodPost, "/api/model-connections", `{"name":"OpenAI","base_url":"https://api.openai.com/v1","model_name":"gpt-test","api_key":"secret-model-key","unknown":true}`, cookie, "https://portal.example.test")
	if unknown.Code != http.StatusBadRequest || manager.calls != 1 {
		t.Fatalf("unknown field response = %d %s, calls=%d", unknown.Code, unknown.Body, manager.calls)
	}

	apiKey := request(router, http.MethodGet, "/api/model-connections", "", "Bearer "+apiKeyCredential(), "")
	if apiKey.Code != http.StatusForbidden {
		t.Fatalf("API key request status = %d body=%s", apiKey.Code, apiKey.Body)
	}
}

func TestClientAvailableModelsReturnsSafeMetadata(t *testing.T) {
	checkedAt := time.Now().UTC()
	manager := &modelConnectionManagerStub{items: []app.ModelConnectionView{{
		ID: "p1234567", Name: "Shared", BaseURL: "https://private-provider.example.test/v1", ModelName: "model-a",
		HasAPIKey: true, Capabilities: app.ModelCapabilities{Text: true, Tools: true}, CheckedAt: &checkedAt,
	}}}
	services := newServices()
	deps := services.deps()
	deps.ModelConnections = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	clientCredential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/available-models", "", "Bearer "+clientCredential, "")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"model_name":"model-a"`) ||
		!strings.Contains(body, `"id":"p1234567"`) || strings.Contains(body, "private-provider.example.test") ||
		strings.Contains(body, "has_api_key") || strings.Contains(body, "api_key") {
		t.Fatalf("client available models = %d %s", response.Code, body)
	}
	if manager.principal.Kind != identity.KindClient || manager.principal.TokenID != testTokenID {
		t.Fatalf("available models principal = %#v", manager.principal)
	}
}

func TestModelConnectionUpdateRejectsSharedField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &modelConnectionManagerStub{created: app.ModelConnectionView{
		ID: "p1234567", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
		HasAPIKey: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	services := newServices()
	deps := services.deps()
	deps.ModelConnections = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBody(router, http.MethodPut, "/api/model-connections/p1234567", `{"name":"OpenAI","base_url":"https://api.openai.com/v1","model_name":"gpt-test","shared":true}`, cookie, "https://portal.example.test")
	if response.Code != http.StatusBadRequest || manager.calls != 0 {
		t.Fatalf("update shared field response = %d %s, calls=%d", response.Code, response.Body, manager.calls)
	}
}

func TestModelConnectionCheckReturnsSafeProviderCategory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &modelConnectionManagerStub{
		created: app.ModelConnectionView{ID: "p1234567", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", HasAPIKey: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		checkErr: func() error {
			err := domain.NewError(domain.CodeUpstreamError, "private provider error")
			err.Details["reason"] = "authentication_rejected"
			err.Details["secret"] = "provider-secret"
			return err
		}(),
	}
	services := newServices()
	deps := services.deps()
	deps.ModelConnections = manager
	deps.ModelGateway = &modelGatewayHTTPTest{checkErr: manager.checkErr}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/model-connections/p1234567/check", "{}", cookie, "https://portal.example.test", "Idempotency-Key", "probe-key-0001")
	body := response.Body.String()
	if response.Code != http.StatusBadGateway || !strings.Contains(body, "authentication_rejected") || strings.Contains(body, "provider-secret") || strings.Contains(body, "private provider error") {
		t.Fatalf("provider check response = %d %s", response.Code, body)
	}
}
