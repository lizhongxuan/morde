package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/domain/identity"
)

type apiKeyResourceService struct {
	items      []identity.Token
	created    CreatedAPIKey
	name       string
	createCall int
}

func (s *apiKeyResourceService) ListAPIKeys(context.Context, identity.Principal) ([]identity.Token, error) {
	return s.items, nil
}
func (s *apiKeyResourceService) CreateAPIKey(_ context.Context, _ identity.Principal, name string, _ time.Duration) (CreatedAPIKey, error) {
	s.name = name
	s.createCall++
	return s.created, nil
}
func (s *apiKeyResourceService) DeleteAPIKey(context.Context, identity.Principal, string) error {
	return nil
}

func apiKeyResourceRouter(t *testing.T, tokens TokenService) http.Handler {
	t.Helper()
	services := newServices()
	deps := services.deps()
	deps.Tokens = tokens
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAPIKeyCreateNeedsNoGrantsAndRejectsLegacyFields(t *testing.T) {
	tokens := &apiKeyResourceService{created: CreatedAPIKey{ID: testTokenID, Key: apiKeyCredential(), ExpiresAt: time.Now().Add(time.Hour)}}
	router := apiKeyResourceRouter(t, tokens)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	response := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", `{"name":"automation"}`, cookie, origin)
	if response.Code != http.StatusCreated || tokens.createCall != 1 || tokens.name != "automation" {
		t.Fatalf("create: %d %s, call=%d name=%q", response.Code, response.Body, tokens.createCall, tokens.name)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != testTokenID || body["key"] != apiKeyCredential() {
		t.Fatalf("response: %#v", body)
	}
	if _, ok := body["host_ids"]; ok {
		t.Fatalf("old grant field leaked: %#v", body)
	}
	if _, ok := body["scopes"]; ok {
		t.Fatalf("old scope field leaked: %#v", body)
	}
	legacy := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", `{"name":"automation","host_ids":[],"scopes":[]}`, cookie, origin)
	if legacy.Code != http.StatusBadRequest || tokens.createCall != 1 {
		t.Fatalf("legacy request: %d %s", legacy.Code, legacy.Body)
	}
}

func TestAPIKeyListOmitsStaticGrants(t *testing.T) {
	revokedAt := time.Now().UTC()
	tokens := &apiKeyResourceService{items: []identity.Token{{ID: testTokenID, Kind: identity.KindAPIKey, OwnerID: testUserID, ExpiresAt: time.Now().Add(time.Hour), RevokedAt: &revokedAt}}}
	response := requestWithCookie(apiKeyResourceRouter(t, tokens), http.MethodGet, "/api/apikeys", &http.Cookie{Name: portalCookieName, Value: sessionCredential()}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list: %d %s", response.Code, response.Body)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0]["revoked"] != true {
		t.Fatalf("list: %#v", body)
	}
	if _, ok := body.Items[0]["host_ids"]; ok {
		t.Fatalf("old grant field leaked: %#v", body)
	}
}
