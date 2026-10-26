package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestClientDenialAuditFailureRollsBackState(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	id, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, id)
	if err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("ABCDEFGH"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{
		ID: id, DisplayName: "phone", Platform: "ios", SecretHash: secretHash,
		UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	installClientAuditFailure(t, pool)
	if err := store.DenyClientAuthorization(ctx, codeHash[:], owner.ID, "127.0.0.1"); err == nil {
		t.Fatal("denial succeeded despite audit failure")
	}
	pairing, err := store.GetClientAuthorization(ctx, id)
	if err != nil || pairing.State != "pending" {
		t.Fatalf("denial after audit failure = %#v %v", pairing, err)
	}
	assertClientAuditCount(t, pool, "client.deny", id, 0)
	removeClientAuditFailure(t, pool)
	if err := store.DenyClientAuthorization(ctx, codeHash[:], owner.ID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	pairing, err = store.GetClientAuthorization(ctx, id)
	if err != nil || pairing.State != "denied" {
		t.Fatalf("committed denial = %#v %v", pairing, err)
	}
	assertClientAuditCount(t, pool, "client.deny", id, 1)
}

func TestClientRevocationAuditFailureRollsBackCredentials(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	id, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, id)
	if err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("BCDEFGHJ"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{
		ID: id, DisplayName: "desktop", Platform: "desktop", WantsWorkspaceSync: true,
		SecretHash: secretHash, UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	installationID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.ApproveClientAuthorization(ctx, codeHash[:], owner.ID, []string{}, installationID, "127.0.0.1", time.Now().UTC().Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	syncToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	clientToken.ExpiresAt, syncToken.ExpiresAt = installation.ExpiresAt, installation.ExpiresAt
	if _, err := store.ClaimClientAuthorization(ctx, id, secretHash, clientToken, &syncToken); err != nil {
		t.Fatal(err)
	}
	installClientAuditFailure(t, pool)
	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1"); err == nil {
		t.Fatal("revocation succeeded despite audit failure")
	}
	active, err := store.ActiveClientInstallation(ctx, installationID, owner.ID)
	if err != nil || !active {
		t.Fatalf("installation after audit failure = %v %v", active, err)
	}
	client, err := store.GetTokenByID(ctx, clientToken.ID)
	if err != nil || client.RevokedAt != nil {
		t.Fatalf("client credential after audit failure = %#v %v", client, err)
	}
	syncCredential, err := store.GetTokenByID(ctx, syncToken.ID)
	if err != nil || syncCredential.RevokedAt != nil {
		t.Fatalf("sync credential after audit failure = %#v %v", syncCredential, err)
	}
	assertClientAuditCount(t, pool, "client.revoke", installationID, 0)
	removeClientAuditFailure(t, pool)
	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	active, err = store.ActiveClientInstallation(ctx, installationID, owner.ID)
	if err != nil || active {
		t.Fatalf("committed revocation = %v %v", active, err)
	}
	client, err = store.GetTokenByID(ctx, clientToken.ID)
	syncCredential, syncErr := store.GetTokenByID(ctx, syncToken.ID)
	if err != nil || syncErr != nil || client.RevokedAt == nil || syncCredential.RevokedAt == nil {
		t.Fatalf("committed token revocation = %#v %v; %#v %v", client, err, syncCredential, syncErr)
	}
	assertClientAuditCount(t, pool, "client.revoke", installationID, 1)
}

func installClientAuditFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_client_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'injected client audit failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE TRIGGER reject_client_audit
BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_client_audit()`); err != nil {
		t.Fatal(err)
	}
}

func removeClientAuditFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER reject_client_audit ON audit_logs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DROP FUNCTION reject_client_audit()`); err != nil {
		t.Fatal(err)
	}
}

func assertClientAuditCount(t *testing.T, pool *pgxpool.Pool, action, target string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action=$1 AND target=$2`, action, target).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("audit %s for %s = %d, want %d", action, target, count, want)
	}
}
