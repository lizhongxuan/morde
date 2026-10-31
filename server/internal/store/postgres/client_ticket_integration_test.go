package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func TestClientTicketConsumeReturnsInstallationAndRevocationRejectsPendingTicket(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)

	consumedID := createClientBoundTicket(t, ctx, store, owner.ID, installationID, hostValue.ID)
	consumed, err := store.ConsumeTicket(ctx, consumedID)
	if err != nil || consumed.ClientInstallationID != installationID || consumed.UserID != owner.ID || consumed.HostID != hostValue.ID {
		t.Fatalf("ConsumeTicket(client) = %#v, %v", consumed, err)
	}

	pendingID := createClientBoundTicket(t, ctx, store, owner.ID, installationID, hostValue.ID)
	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1", audit.ActorSession, nil); err != nil {
		t.Fatalf("revoke client installation: %v", err)
	}
	if _, err := store.ConsumeTicket(ctx, pendingID); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(revoked client installation) = %v, want invalid credential", err)
	}
}

func createClientBoundTicket(t *testing.T, ctx context.Context, store *Store, ownerID, installationID, hostID string) string {
	t.Helper()
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindTicket, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: ownerID, ClientInstallationID: installationID,
		Kind: identity.KindTicket, HostID: hostID, SecretHash: secretHash,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create client-bound ticket: %v", err)
	}
	return tokenID
}
