package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestOnboardingRoutesUsePortalSessionAndReturnBoundedStatus(t *testing.T) {
	services := newServices()
	manager := &onboardingManagerStub{
		preferences: app.UserPreferences{OwnerID: testUserID, AssistantName: "Morde", SelectedModelID: "p1234567", DefaultHostID: testHostID, UpdatedAt: time.Now().UTC()},
		status: app.OnboardingSnapshot{
			Preferences: app.UserPreferences{OwnerID: testUserID, AssistantName: "Morde", SelectedModelID: "p1234567", DefaultHostID: testHostID},
			ModelStatus: app.ModelReady, AvailableModels: []app.OnboardingModel{{ID: "p1234567", Name: "shared", Capabilities: app.ModelCapabilities{Text: true, Tools: true, Vision: false}}},
			ComputerStatus: app.ComputerReady,
			Computers:      []app.OnboardingComputer{{ID: testHostID, Name: "worker", State: host.StateRunning}},
			CanSubmit:      true, NextAction: "start_chat",
		},
	}
	deps := services.deps()
	deps.Onboarding = manager
	engine, err := New(deps, validOptions())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	status := requestWithCookie(engine, http.MethodGet, "/api/onboarding/status", cookie, "")
	if status.Code != http.StatusOK {
		t.Fatalf("GET onboarding status = %d %s", status.Code, status.Body)
	}
	var decoded onboardingStatusResponse
	if err := json.Unmarshal(status.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode onboarding status: %v", err)
	}
	if decoded.ModelStatus != app.ModelReady || decoded.ComputerStatus != app.ComputerReady || !decoded.CanSubmit || decoded.DefaultHostID == nil || *decoded.DefaultHostID != testHostID ||
		len(decoded.Models) != 1 || decoded.Models[0].Capabilities.Vision {
		t.Fatalf("no-vision model onboarding status = %#v", decoded)
	}
	if strings.Contains(status.Body.String(), "state_reason") || strings.Contains(status.Body.String(), "aio_key") || strings.Contains(status.Body.String(), "api_key") {
		t.Fatalf("onboarding response contains internal state or secret fields: %s", status.Body)
	}

	selected := requestWithCookieBody(engine, http.MethodPut, "/api/me/preferences", `{"assistant_name":"Morde","selected_model_id":"p1234567"}`, cookie, "https://portal.example.test")
	if selected.Code != http.StatusOK || manager.saved.SelectedModelID != "p1234567" {
		t.Fatalf("PUT preferences = %d %s; saved=%#v", selected.Code, selected.Body, manager.saved)
	}

	defaultHost := requestWithCookieBody(engine, http.MethodPost, "/api/me/default-host", `{}`, cookie, "https://portal.example.test")
	if defaultHost.Code != http.StatusOK || manager.defaultHostCommand != (app.DefaultHostCommand{}) {
		t.Fatalf("POST default host = %d %s; command=%#v", defaultHost.Code, defaultHost.Body, manager.defaultHostCommand)
	}
	if strings.Contains(defaultHost.Body.String(), "state_reason") || strings.Contains(defaultHost.Body.String(), "aio_key") {
		t.Fatalf("default host response contains internal details: %s", defaultHost.Body)
	}

	apiKey := request(engine, http.MethodGet, "/api/me/preferences", "", "Bearer "+apiKeyCredential(), "")
	assertError(t, apiKey, http.StatusForbidden, "forbidden")
}

type onboardingManagerStub struct {
	preferences        app.UserPreferences
	saved              app.SaveUserPreferencesCommand
	status             app.OnboardingSnapshot
	defaultHostCommand app.DefaultHostCommand
	defaultHostResult  app.DefaultHostResult
	err                error
}

func (s *onboardingManagerStub) Preferences(context.Context, identity.Principal) (app.UserPreferences, error) {
	return s.preferences, s.err
}

func (s *onboardingManagerStub) SavePreferences(_ context.Context, _ identity.Principal, command app.SaveUserPreferencesCommand) (app.UserPreferences, error) {
	s.saved = command
	return s.preferences, s.err
}

func (s *onboardingManagerStub) Status(context.Context, identity.Principal) (app.OnboardingSnapshot, error) {
	return s.status, s.err
}

func (s *onboardingManagerStub) EnsureDefaultHost(_ context.Context, _ identity.Principal, command app.DefaultHostCommand) (app.DefaultHostResult, error) {
	s.defaultHostCommand = command
	if s.defaultHostResult.Snapshot.ModelStatus == "" {
		s.defaultHostResult.Snapshot = s.status
	}
	return s.defaultHostResult, s.err
}
