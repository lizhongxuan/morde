package api

import (
	"context"
	"encoding/json"
	"errors"
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
	createErr error
	updateErr error
	checkErr  error
	listErr   error
}

type adminModelDefaultAPIStub struct {
	value app.AdminModelDefaultAvailability
}

func (s adminModelDefaultAPIStub) Get(context.Context, identity.Principal) (app.AdminModelDefaultView, error) {
	return app.AdminModelDefaultView{Configured: s.value.Configured, ModelID: s.value.ModelID, Version: 1}, nil
}

func (s adminModelDefaultAPIStub) Update(context.Context, identity.Principal, int64, string, string) (app.AdminModelDefaultView, error) {
	return app.AdminModelDefaultView{}, nil
}

func (s adminModelDefaultAPIStub) Available(context.Context) (app.AdminModelDefaultAvailability, error) {
	return s.value, nil
}

func (s *modelConnectionManagerStub) List(_ context.Context, principal identity.Principal) ([]app.ModelConnectionView, error) {
	s.principal = principal
	return s.items, s.listErr
}

func (s *modelConnectionManagerStub) Get(context.Context, identity.Principal, string) (app.ModelConnectionView, error) {
	return s.created, nil
}

func (s *modelConnectionManagerStub) Create(_ context.Context, principal identity.Principal, command app.ModelConnectionCommand) (app.ModelConnectionView, error) {
	s.calls++
	s.principal = principal
	s.command = command
	return s.created, s.createErr
}

func (s *modelConnectionManagerStub) Update(_ context.Context, _ identity.Principal, _ string, command app.ModelConnectionCommand) (app.ModelConnectionView, error) {
	s.calls++
	s.command = command
	return s.created, s.updateErr
}

func (*modelConnectionManagerStub) Delete(context.Context, identity.Principal, string) error {
	return nil
}

func (s *modelConnectionManagerStub) Check(context.Context, identity.Principal, string) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, s.checkErr
}

func TestModelConnectionAPIAllowsClientPersonalCreateAndNeverReturnsAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &modelConnectionManagerStub{created: app.ModelConnectionView{
		ID: "p1234567", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
		Shared: false, HasAPIKey: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
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
	body := `{"name":"OpenAI","base_url":"https://api.openai.com/v1","model_name":"gpt-test","api_key":"secret-model-key","shared":false}`
	response := request(router, http.MethodPost, "/api/model-connections", body, "Bearer "+clientCredential, "")
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "secret-model-key") || strings.Contains(response.Body.String(), `"api_key"`) {
		t.Fatalf("create response = %d %s", response.Code, response.Body)
	}
	if manager.calls != 1 || manager.command.APIKey != "secret-model-key" || manager.principal.Kind != identity.KindClient || manager.command.Shared {
		t.Fatalf("create command not forwarded: calls=%d command=%+v", manager.calls, manager.command)
	}

	unknown := request(router, http.MethodPost, "/api/model-connections", `{"name":"OpenAI","base_url":"https://api.openai.com/v1","model_name":"gpt-test","api_key":"secret-model-key","unknown":true}`, "Bearer "+clientCredential, "")
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
		HasAPIKey: true, Capabilities: app.ModelCapabilities{Text: true, Tools: true,
			MediaGeneration: app.ModelMediaGenerationCapabilities{ImageGeneration: true}}, CheckedAt: &checkedAt,
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
		!strings.Contains(body, `"media_generation":{"image_generation":true,"video_generation":false,"speech_generation":false}`) ||
		!strings.Contains(body, `"default_model_configured":false`) || !strings.Contains(body, `"default_model_id":null`) ||
		strings.Contains(body, "has_api_key") || strings.Contains(body, "api_key") {
		t.Fatalf("client available models = %d %s", response.Code, body)
	}
	if manager.principal.Kind != identity.KindClient || manager.principal.TokenID != testTokenID {
		t.Fatalf("available models principal = %#v", manager.principal)
	}
}

func TestClientAvailableModelsIncludesConfiguredDefault(t *testing.T) {
	checkedAt := time.Now().UTC()
	manager := &modelConnectionManagerStub{items: []app.ModelConnectionView{{
		ID: "p1234567", Name: "Shared", BaseURL: "https://private-provider.example.test/v1", ModelName: "model-a",
		Shared: true, HasAPIKey: true, Capabilities: app.ModelCapabilities{Text: true, Tools: true}, CheckedAt: &checkedAt,
	}}}
	services := newServices()
	deps := services.deps()
	deps.ModelConnections = manager
	deps.AdminModelDefaults = adminModelDefaultAPIStub{value: app.AdminModelDefaultAvailability{Configured: true, ModelID: "p1234567"}}
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
	if response.Code != http.StatusOK || !strings.Contains(body, `"default_model_configured":true`) ||
		!strings.Contains(body, `"default_model_id":"p1234567"`) {
		t.Fatalf("configured model metadata = %d %s", response.Code, body)
	}
}

func TestClientAvailableModelsAllowsTextOnlyDefault(t *testing.T) {
	checkedAt := time.Now().UTC()
	manager := &modelConnectionManagerStub{items: []app.ModelConnectionView{{
		ID: "p1234567", Name: "Shared", ModelName: "text-only", Shared: true, HasAPIKey: true,
		Capabilities: app.ModelCapabilities{Text: true}, CheckedAt: &checkedAt,
	}}}
	deps := newServices().deps()
	deps.ModelConnections = manager
	deps.AdminModelDefaults = adminModelDefaultAPIStub{value: app.AdminModelDefaultAvailability{Configured: true, ModelID: "p1234567"}}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/available-models", "", "Bearer "+credential, "")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"configuration_status":"configured"`) ||
		!strings.Contains(body, `"default_model_id":"p1234567"`) || !strings.Contains(body, `"tools":false`) {
		t.Fatalf("text-only default metadata = %d %s", response.Code, body)
	}
}

func TestModelConfigurationStatusDoesNotDependOnProbeCapabilities(t *testing.T) {
	checkedAt := time.Now().UTC()
	text := app.ModelConnectionView{
		ID: "p1234567", Name: "Text", BaseURL: "https://api.example.test/v1", ModelName: "text-model",
		HasAPIKey: true, CheckedAt: &checkedAt, CreatedAt: checkedAt, UpdatedAt: checkedAt,
		Capabilities: app.ModelCapabilities{Text: true},
	}
	unchecked := text
	unchecked.ID, unchecked.CheckedAt = "p7654321", nil
	unchecked.Capabilities = app.ModelCapabilities{}
	shared := unchecked
	shared.ID, shared.Shared = "p1111111", true
	incomplete := unchecked
	incomplete.ID, incomplete.HasAPIKey = "p2222222", false
	tests := []struct {
		name      string
		items     []app.ModelConnectionView
		status    app.ModelConfigurationStatus
		available []string
	}{
		{name: "none", status: app.ModelConfigurationNotConfigured},
		{name: "personal not checked", items: []app.ModelConnectionView{unchecked}, status: app.ModelConfigurationConfigured},
		{name: "shared not checked", items: []app.ModelConnectionView{shared}, status: app.ModelConfigurationConfigured},
		{name: "text without tools", items: []app.ModelConnectionView{text}, status: app.ModelConfigurationConfigured, available: []string{text.ID}},
		{name: "incomplete record", items: []app.ModelConnectionView{incomplete}, status: app.ModelConfigurationUnknown},
		{name: "valid and incomplete", items: []app.ModelConnectionView{incomplete, unchecked}, status: app.ModelConfigurationConfigured},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &modelConnectionManagerStub{items: test.items}
			deps := newServices().deps()
			deps.ModelConnections = manager
			router, err := New(deps, validOptions())
			if err != nil {
				t.Fatal(err)
			}
			credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/available-models", "/api/model-connections"} {
				response := request(router, http.MethodGet, path, "", "Bearer "+credential, "")
				if path == "/api/model-connections" && len(test.items) > 0 {
					incomplete := false
					for _, item := range test.items {
						incomplete = incomplete || !item.HasAPIKey
					}
					if incomplete {
						if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "not_configured") {
							t.Fatalf("incomplete connection list = %d %s", response.Code, response.Body)
						}
						continue
					}
				}
				var body struct {
					ConfigurationStatus app.ModelConfigurationStatus `json:"configuration_status"`
					Items               []struct {
						ID string `json:"id"`
					} `json:"items"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.ConfigurationStatus != test.status {
					t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body)
				}
				if path == "/api/available-models" {
					if len(body.Items) != len(test.available) {
						t.Fatalf("available models = %+v, want %v", body.Items, test.available)
					}
					for index, id := range test.available {
						if body.Items[index].ID != id {
							t.Fatalf("available models = %+v, want %v", body.Items, test.available)
						}
					}
				}
			}
		})
	}
}

func TestModelConfigurationLookupFailureDoesNotClaimUnconfigured(t *testing.T) {
	manager := &modelConnectionManagerStub{listErr: errors.New("unavailable")}
	deps := newServices().deps()
	deps.ModelConnections = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/available-models", "", "Bearer "+credential, "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "not_configured") {
		t.Fatalf("model lookup failure = %d %s", response.Code, response.Body)
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
	services.verifier.role = identity.RoleAdmin
	deps := services.deps()
	deps.ModelConnections = manager
	deps.ModelGateway = &modelGatewayHTTPTest{checkErr: manager.checkErr}
	deps.AdminAudit = &adminAuditWriterStub{}
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
