package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type apiKeyTokenStore struct{ values map[string]identity.Token }

func (s *apiKeyTokenStore) GetTokenByID(_ context.Context, id string) (identity.Token, error) {
	value, ok := s.values[id]
	if !ok {
		return identity.Token{}, domain.NewError(domain.CodeNotFound, "token not found")
	}
	return value, nil
}
func (s *apiKeyTokenStore) ListTokensByOwner(_ context.Context, ownerID string) ([]identity.Token, error) {
	values := make([]identity.Token, 0)
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

func apiKeySession() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func TestCreateAPIKeyUsesCurrentAccountPermissions(t *testing.T) {
	tokens := &apiKeyTokenStore{values: make(map[string]identity.Token)}
	auditor := &apiKeyAudit{}
	service, err := NewTokenService(tokens, auditor, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAPIKey(context.Background(), apiKeySession(), "  my key  ", 0)
	if err != nil {
		t.Fatal(err)
	}
	kind, tokenID, _, err := identity.ParseCredential(created.Key)
	if err != nil || kind != identity.KindAPIKey || tokenID != created.ID {
		t.Fatalf("credential: %q %q %q %v", created.Key, kind, tokenID, err)
	}
	stored := tokens.values[created.ID]
	if stored.Name != "my key" || len(stored.HostIDs) != 0 || len(stored.Scopes) != 0 {
		t.Fatalf("stored key: %#v", stored)
	}
	if len(auditor.entries) != 1 || auditor.entries[0].Detail["authorization"] != "account_current" {
		t.Fatalf("audit: %#v", auditor.entries)
	}
	if got := time.Until(created.ExpiresAt); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("ttl = %s", got)
	}
	if _, err := service.CreateAPIKey(context.Background(), identity.Principal{Kind: identity.KindAPIKey}, "another", 0); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("key creates key: %v", err)
	}
	if _, err := service.CreateAPIKey(context.Background(), apiKeySession(), "", 0); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := service.CreateAPIKey(context.Background(), apiKeySession(), "too long ttl", 25*time.Hour); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("long ttl: %v", err)
	}
}
