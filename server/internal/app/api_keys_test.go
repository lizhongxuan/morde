package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type apiKeyHostStore struct {
	HostStore
	values map[string]host.Host
}

func (s apiKeyHostStore) GetHost(_ context.Context, id string) (host.Host, error) {
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return value, nil
}

type apiKeyTokenStore struct{ values map[string]identity.Token }

func (s *apiKeyTokenStore) GetTokenByID(_ context.Context, id string) (identity.Token, error) {
	value, ok := s.values[id]
	if !ok {
		return identity.Token{}, domain.NewError(domain.CodeNotFound, "token not found")
	}
	return value, nil
}

func (s *apiKeyTokenStore) ListTokensByOwner(_ context.Context, ownerID string) ([]identity.Token, error) {
	values := make([]identity.Token, 0, len(s.values))
	for _, value := range s.values {
		if value.OwnerID == ownerID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *apiKeyTokenStore) CreateToken(_ context.Context, value identity.Token) (identity.Token, error) {
	if _, exists := s.values[value.ID]; exists {
		return identity.Token{}, uniqueViolationError{}
	}
	value.CreatedAt = time.Now().UTC()
	s.values[value.ID] = value
	return value, nil
}

func (s *apiKeyTokenStore) RevokeToken(_ context.Context, id, ownerID string) error {
	value, ok := s.values[id]
	if !ok || value.OwnerID != ownerID {
		return domain.NewError(domain.CodeNotFound, "token not found")
	}
	now := time.Now().UTC()
	value.RevokedAt = &now
	s.values[id] = value
	return nil
}

func (s *apiKeyTokenStore) ConsumeTicket(context.Context, string) (Ticket, error) {
	return Ticket{}, identity.ErrInvalidCredential
}

type apiKeyAudit struct{ entries []audit.Entry }

func (s *apiKeyAudit) Write(_ context.Context, entry audit.Entry) error {
	s.entries = append(s.entries, entry)
	return nil
}

type apiKeySharingAuthorizer struct {
	actorID    string
	hostID     string
	permission host.Permission
	err        error
}

func (s apiKeySharingAuthorizer) Authorize(_ context.Context, hostID, actorID string, permission host.Permission) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if hostID != s.hostID || actorID != s.actorID || permission != s.permission {
		return false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return false, nil
}

func newAPIKeyService(t *testing.T, hosts map[string]host.Host) (*TokenService, *apiKeyTokenStore, *apiKeyAudit) {
	t.Helper()
	tokens := &apiKeyTokenStore{values: make(map[string]identity.Token)}
	auditor := &apiKeyAudit{}
	service, err := NewTokenService(apiKeyHostStore{values: hosts}, tokens, auditor, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service, tokens, auditor
}

func apiKeySession() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func TestCreateAPIKeyPersistsExplicitHostAndScopeGrants(t *testing.T) {
	service, tokens, auditor := newAPIKeyService(t, map[string]host.Host{
		"h1234567": {ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning},
	})
	created, err := service.CreateAPIKey(context.Background(), apiKeySession(), "  test key  ", []string{"h1234567"}, []identity.Scope{identity.ScopeComputerObserve, identity.ScopeHostRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	kind, tokenID, _, err := identity.ParseCredential(created.Key)
	if err != nil || kind != identity.KindAPIKey || tokenID != created.ID {
		t.Fatalf("created credential = %q, kind=%q, id=%q, err=%v", created.Key, kind, tokenID, err)
	}
	stored := tokens.values[created.ID]
	if stored.Kind != identity.KindAPIKey || stored.Name != "test key" || len(stored.HostIDs) != 1 || stored.HostIDs[0] != "h1234567" || len(stored.Scopes) != 2 {
		t.Fatalf("stored grants = %#v", stored)
	}
	if len(auditor.entries) != 1 || auditor.entries[0].Detail["host_ids"] == nil || auditor.entries[0].Detail["scopes"] == nil {
		t.Fatalf("audit entries = %#v", auditor.entries)
	}
	if got := time.Until(created.ExpiresAt); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("default key lifetime = %s", got)
	}
}

func TestCreateAPIKeyAllowsOnlyCurrentlyMCPSharedHost(t *testing.T) {
	actorID := "u1234567"
	sharedHost := host.Host{ID: "h7654321", OwnerID: "u7654321", State: host.StateRunning}
	sharing := apiKeySharingAuthorizer{actorID: actorID, hostID: sharedHost.ID, permission: host.PermissionMCPControl}
	tokens := &apiKeyTokenStore{values: make(map[string]identity.Token)}
	service, err := NewTokenService(apiKeyHostStore{values: map[string]host.Host{sharedHost.ID: sharedHost}}, tokens, &apiKeyAudit{}, time.Hour, 24*time.Hour, sharing)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAPIKey(context.Background(), apiKeySession(), "shared MCP", []string{sharedHost.ID}, []identity.Scope{identity.ScopeComputerOperate}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens.values[created.ID].HostIDs) != 1 || tokens.values[created.ID].HostIDs[0] != sharedHost.ID {
		t.Fatalf("shared Host allow-list = %#v", tokens.values[created.ID].HostIDs)
	}

	service, err = NewTokenService(apiKeyHostStore{values: map[string]host.Host{sharedHost.ID: sharedHost}}, &apiKeyTokenStore{values: make(map[string]identity.Token)}, &apiKeyAudit{}, time.Hour, 24*time.Hour,
		apiKeySharingAuthorizer{actorID: actorID, hostID: sharedHost.ID, permission: host.PermissionDesktopView})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAPIKey(context.Background(), apiKeySession(), "revoked", []string{sharedHost.ID}, []identity.Scope{identity.ScopeComputerOperate}, 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("CreateAPIKey() without current MCP permission = %v", err)
	}
}

func TestCreateAPIKeyRejectsInvalidAuthorityAndAllowsEmptyGrants(t *testing.T) {
	service, tokens, _ := newAPIKeyService(t, map[string]host.Host{
		"h1234567": {ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning},
		"h7654321": {ID: "h7654321", OwnerID: "u7654321", State: host.StateRunning},
	})
	tests := []struct {
		name      string
		principal identity.Principal
		hostIDs   []string
		scopes    []identity.Scope
	}{
		{name: "API key cannot create another key", principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t7654321", Scopes: []identity.Scope{identity.ScopeHostRead}}, hostIDs: []string{"h1234567"}, scopes: []identity.Scope{identity.ScopeHostRead}},
		{name: "unknown scope", principal: apiKeySession(), hostIDs: []string{"h1234567"}, scopes: []identity.Scope{"host:*"}},
		{name: "duplicate scope", principal: apiKeySession(), hostIDs: []string{"h1234567"}, scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostRead}},
		{name: "foreign host", principal: apiKeySession(), hostIDs: []string{"h7654321"}, scopes: []identity.Scope{identity.ScopeHostRead}},
		{name: "duplicate host", principal: apiKeySession(), hostIDs: []string{"h1234567", "h1234567"}, scopes: []identity.Scope{identity.ScopeHostRead}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CreateAPIKey(context.Background(), test.principal, "key", test.hostIDs, test.scopes, 0); err == nil {
				t.Fatal("CreateAPIKey() unexpectedly succeeded")
			}
		})
	}
	if len(tokens.values) != 0 {
		t.Fatalf("invalid requests created %d keys", len(tokens.values))
	}
	created, err := service.CreateAPIKey(context.Background(), apiKeySession(), "empty hosts", []string{}, []identity.Scope{identity.ScopeHostCreate}, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey() with an explicit empty host set: %v", err)
	}
	if len(tokens.values[created.ID].HostIDs) != 0 {
		t.Fatalf("empty host grant became %#v", tokens.values[created.ID].HostIDs)
	}
	empty, err := service.CreateAPIKey(context.Background(), apiKeySession(), "empty grants", []string{}, []identity.Scope{}, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey() with explicit empty grants: %v", err)
	}
	if len(tokens.values[empty.ID].HostIDs) != 0 || len(tokens.values[empty.ID].Scopes) != 0 {
		t.Fatalf("empty grants became host_ids=%#v scopes=%#v", tokens.values[empty.ID].HostIDs, tokens.values[empty.ID].Scopes)
	}
}

func TestAuthorizeTaskActionRevalidatesAPIKeyAndSessionOwner(t *testing.T) {
	service, users, hosts, _, tokens, _ := newCredentialFixture(t)
	hosts.values = append(hosts.values, host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning})
	tokens.values["t1234567"] = identity.Token{
		ID: "t1234567", OwnerID: "u1234567", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"},
		Scopes: []identity.Scope{identity.ScopeTaskRead, identity.ScopeTaskSubmit}, ExpiresAt: time.Now().Add(time.Hour),
	}
	check := func(kind identity.Kind, tokenID string, approved, current []identity.Scope, required identity.Scope) error {
		tokens.values[tokenID] = identity.Token{
			ID: tokenID, OwnerID: "u1234567", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"},
			Scopes: current, ExpiresAt: time.Now().Add(time.Hour),
		}
		return service.AuthorizeTaskAction(context.Background(), "u1234567", kind, tokenID, "h1234567", approved, required)
	}
	if err := check(identity.KindAPIKey, "t1234567", []identity.Scope{identity.ScopeTaskRead, identity.ScopeTaskSubmit}, []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskRead); err != nil {
		t.Fatalf("current approved scope should be accepted: %v", err)
	}
	if err := check(identity.KindAPIKey, "t1234567", []identity.Scope{identity.ScopeTaskSubmit}, []identity.Scope{identity.ScopeTaskRead, identity.ScopeTaskSubmit}, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("scope outside the task snapshot = %v", err)
	}
	if err := check(identity.KindAPIKey, "t1234567", []identity.Scope{identity.ScopeTaskRead}, nil, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("legacy key with no scopes = %v", err)
	}
	if err := check(identity.KindAPIKey, "t1234567", []identity.Scope{identity.ScopeTaskRead}, []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskControl); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("missing approved action scope = %v", err)
	}
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindAPIKey, "t7654321", "h1234567", []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("missing current key = %v", err)
	}
	key := tokens.values["t1234567"]
	revoked := time.Now()
	key.RevokedAt = &revoked
	tokens.values["t1234567"] = key
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindAPIKey, "t1234567", "h1234567", []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked key = %v", err)
	}
	key.RevokedAt = nil
	key.ExpiresAt = time.Now().Add(-time.Second)
	tokens.values["t1234567"] = key
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindAPIKey, "t1234567", "h1234567", []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("expired key = %v", err)
	}
	key.ExpiresAt = time.Now().Add(time.Hour)
	key.HostIDs = nil
	tokens.values["t1234567"] = key
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindAPIKey, "t1234567", "h1234567", []identity.Scope{identity.ScopeTaskRead}, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("key without Host grant = %v", err)
	}
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindSession, "t7654321", "h1234567", nil, identity.ScopeTaskRead); err != nil {
		t.Fatalf("session task should not depend on session lifetime: %v", err)
	}
	users.values["u1234567"] = identity.User{ID: "u1234567", Disabled: true}
	if err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindSession, "t7654321", "h1234567", nil, identity.ScopeTaskRead); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("disabled owner = %v", err)
	}
}

func TestAuthorizeTaskActionDoesNotTreatCredentialStoreFailureAsRevocation(t *testing.T) {
	service, _, hosts, _, tokens, _ := newCredentialFixture(t)
	hosts.values = append(hosts.values, host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning})
	tokens.err = errors.New("credential store unavailable")
	err := service.AuthorizeTaskAction(context.Background(), "u1234567", identity.KindAPIKey, "t1234567", "h1234567",
		[]identity.Scope{identity.ScopeTaskSubmit}, identity.ScopeTaskSubmit)
	if err == nil || domain.CodeOf(err) == domain.CodeUnauthenticated {
		t.Fatalf("store failure was treated as revoked authorization: %v", err)
	}
}
