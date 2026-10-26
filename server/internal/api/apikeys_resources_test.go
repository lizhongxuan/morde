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
	principal  identity.Principal
	name       string
	hostIDs    []string
	scopes     []identity.Scope
	createCall int
}

func (s *apiKeyResourceService) ListAPIKeys(_ context.Context, principal identity.Principal) ([]identity.Token, error) {
	s.principal = principal
	return s.items, nil
}

func (s *apiKeyResourceService) CreateAPIKey(_ context.Context, principal identity.Principal, name string, hostIDs []string, scopes []identity.Scope, _ time.Duration) (CreatedAPIKey, error) {
	s.principal, s.name = principal, name
	s.hostIDs, s.scopes = hostIDs, scopes
	s.createCall++
	return s.created, nil
}

func (s *apiKeyResourceService) DeleteAPIKey(_ context.Context, principal identity.Principal, _ string) error {
	s.principal = principal
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

func TestAPIKeyCreateRequiresExplicitArraysAndReturnsOnlyGrantedMetadata(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour)
	tokens := &apiKeyResourceService{created: CreatedAPIKey{
		ID: testTokenID, Key: apiKeyCredential(), HostIDs: []string{"h1234567"},
		Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeComputerObserve}, ExpiresAt: expiresAt,
	}}
	router := apiKeyResourceRouter(t, tokens)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	missingGrants := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", `{"name":"automation"}`, cookie, origin)
	if missingGrants.Code != http.StatusBadRequest || tokens.createCall != 0 {
		t.Fatalf("create without explicit arrays = %d %s, service calls=%d", missingGrants.Code, missingGrants.Body, tokens.createCall)
	}

	response := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", `{"name":"automation","host_ids":["h1234567"],"scopes":["host:read","computer:observe"]}`, cookie, origin)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", response.Code, response.Body)
	}
	if tokens.principal.Kind != identity.KindSession || tokens.name != "automation" || len(tokens.hostIDs) != 1 || tokens.hostIDs[0] != "h1234567" || len(tokens.scopes) != 2 {
		t.Fatalf("service received principal/name/grants = %#v / %q / %#v / %#v", tokens.principal, tokens.name, tokens.hostIDs, tokens.scopes)
	}
	var body struct {
		ID      string           `json:"id"`
		Key     string           `json:"key"`
		HostIDs []string         `json:"host_ids"`
		Scopes  []identity.Scope `json:"scopes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != testTokenID || body.Key != apiKeyCredential() || len(body.HostIDs) != 1 || len(body.Scopes) != 2 {
		t.Fatalf("create response = %#v", body)
	}
	empty := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", `{"name":"locked","host_ids":[],"scopes":[]}`, cookie, origin)
	if empty.Code != http.StatusCreated || tokens.hostIDs == nil || tokens.scopes == nil || len(tokens.hostIDs) != 0 || len(tokens.scopes) != 0 {
		t.Fatalf("create with empty grants = %d %s, service host_ids=%#v scopes=%#v", empty.Code, empty.Body, tokens.hostIDs, tokens.scopes)
	}
}

func TestAPIKeyListIncludesScopesAndRevokedState(t *testing.T) {
	revokedAt := time.Now().UTC()
	tokens := &apiKeyResourceService{items: []identity.Token{{
		ID: testTokenID, Kind: identity.KindAPIKey, OwnerID: testUserID, HostIDs: []string{"h1234567"},
		Scopes: []identity.Scope{identity.ScopeTaskRead}, ExpiresAt: time.Now().UTC().Add(time.Hour),
		RevokedAt: &revokedAt,
	}}}
	router := apiKeyResourceRouter(t, tokens)
	response := requestWithCookie(router, http.MethodGet, "/api/apikeys", &http.Cookie{Name: portalCookieName, Value: sessionCredential()}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", response.Code, response.Body)
	}
	var body struct {
		Items []struct {
			HostIDs []string         `json:"host_ids"`
			Scopes  []identity.Scope `json:"scopes"`
			Revoked bool             `json:"revoked"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || len(body.Items[0].HostIDs) != 1 || body.Items[0].Scopes[0] != identity.ScopeTaskRead || !body.Items[0].Revoked {
		t.Fatalf("list response = %#v", body)
	}
}
