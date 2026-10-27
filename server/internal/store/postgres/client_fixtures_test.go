package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func clientTestToken(t *testing.T, ownerID, installID string, kind identity.Kind, _ []string) identity.Token {
	t.Helper()
	id, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := identity.NewCredential(kind, id)
	if err != nil {
		t.Fatal(err)
	}
	value := identity.Token{ID: id, OwnerID: ownerID, ClientInstallationID: installID, Kind: kind,
		SecretHash: hash, ExpiresAt: time.Now().Add(30 * 24 * time.Hour)}
	if kind == identity.KindDevice {
		value.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
	}
	return value
}

func createTestClientInstallation(t *testing.T, pool *pgxpool.Pool, ownerID string, _ []string) string {
	t.Helper()
	id, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO client_installations
  (id,owner_id,display_name,platform,expires_at)
  VALUES ($1,$2,'test desktop','desktop',now()+interval '30 days')`, id, ownerID); err != nil {
		t.Fatal(err)
	}
	return id
}
