package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

const hostEmbedTestInstanceID = "11111111-1111-4111-8111-111111111111"

func TestHostEmbedTicketPersistenceIsHashOnlyBoundAndSingleUse(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running',runtime_generation=3 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	ownerTokenID := createEmbedTestSession(t, ctx, store, owner.ID)
	conversationID := createEmbedTestConversation(t, ctx, store, owner.ID, hostValue.ID)
	ownerTicket := createEmbedTestTicket(t, store, hostValue.ID, conversationID, owner.ID, ownerTokenID, "", app.HostEmbedTargetBrowser, host.PermissionDesktopControl)

	var storedHash []byte
	if err := store.pool.QueryRow(ctx, `SELECT ticket_hash FROM host_embed_tickets WHERE ticket_hash=$1`, ownerTicket.TicketHash).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if len(storedHash) != sha256.Size || string(storedHash) != string(ownerTicket.TicketHash) {
		t.Fatalf("stored embed credential = %x; want only the 32-byte hash", storedHash)
	}

	const consumers = 10
	start := make(chan struct{})
	results := make(chan error, consumers)
	var wait sync.WaitGroup
	for range consumers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.ConsumeHostEmbedTicket(ctx, ownerTicket.TicketHash, hostValue.ID, hostEmbedTestInstanceID, "https://portal.example.test", ownerTicket.EmbedNonce)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes, replays := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeNotFound {
			replays++
		} else {
			t.Errorf("concurrent consume error = %v", err)
		}
	}
	if successes != 1 || replays != consumers-1 {
		t.Fatalf("concurrent ticket consumption successes/replays = %d/%d; want 1/%d", successes, replays, consumers-1)
	}

	staleTicket := createEmbedTestTicket(t, store, hostValue.ID, conversationID, owner.ID, ownerTokenID, "", app.HostEmbedTargetBrowser, host.PermissionDesktopControl)
	if _, err := store.ConsumeHostEmbedTicket(ctx, staleTicket.TicketHash, "h0000000", hostEmbedTestInstanceID, "https://portal.example.test", staleTicket.EmbedNonce); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong Host consume = %v, want not_found", err)
	}
	if _, err := store.ConsumeHostEmbedTicket(ctx, staleTicket.TicketHash, hostValue.ID, "22222222-2222-4222-8222-222222222222", "https://portal.example.test", staleTicket.EmbedNonce); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong instance consume = %v, want not_found", err)
	}
	if _, err := store.ConsumeHostEmbedTicket(ctx, staleTicket.TicketHash, hostValue.ID, hostEmbedTestInstanceID, "https://other.example.test", staleTicket.EmbedNonce); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong Origin consume = %v, want not_found", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=runtime_generation+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeHostEmbedTicket(ctx, staleTicket.TicketHash, hostValue.ID, hostEmbedTestInstanceID, "https://portal.example.test", staleTicket.EmbedNonce); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("stale Host generation consume = %v, want not_found", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=3 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}

	member := createTestUser(t, store, 2, 20_000, 40_000)
	memberHost := createTestHost(t, ctx, store, member.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	memberTokenID := createEmbedTestSession(t, ctx, store, member.ID)
	memberConversationID := createEmbedTestConversation(t, ctx, store, member.ID, memberHost.ID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO host_members(host_id,owner_id,user_id,permissions) VALUES ($1,$2,$3,ARRAY['desktop:view']::text[])`,
		hostValue.ID, owner.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	memberTicket := createEmbedTestTicket(t, store, hostValue.ID, memberConversationID, member.ID, memberTokenID, "", app.HostEmbedTargetBrowser, host.PermissionDesktopView)
	if _, err := store.pool.Exec(ctx, `UPDATE host_members SET revoked_at=clock_timestamp() WHERE host_id=$1 AND user_id=$2`, hostValue.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeHostEmbedTicket(ctx, memberTicket.TicketHash, hostValue.ID, hostEmbedTestInstanceID, "https://portal.example.test", memberTicket.EmbedNonce); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member consume = %v, want not_found", err)
	}

	installationID := "i1234567"
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations(id,owner_id,display_name,platform) VALUES ($1,$2,'desktop','desktop')`, installationID, owner.ID); err != nil {
		t.Fatal(err)
	}
	clientTokenID := "tclient1"
	clientSecretHash := sha256.Sum256([]byte("client credential"))
	if _, err := store.CreateToken(ctx, identity.Token{ID: clientTokenID, OwnerID: owner.ID, Kind: identity.KindClient,
		ClientInstallationID: installationID, SecretHash: clientSecretHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	clientTicket := createEmbedTestTicket(t, store, hostValue.ID, conversationID, owner.ID, clientTokenID, installationID,
		app.HostEmbedTargetBrowser, host.PermissionDesktopView)
	if _, err := store.ConsumeHostEmbedTicket(ctx, clientTicket.TicketHash, hostValue.ID, hostEmbedTestInstanceID, "https://portal.example.test", clientTicket.EmbedNonce); err != nil {
		t.Fatalf("valid installation-bound consume = %v", err)
	}
}

func TestHostEmbedTicketMigrationDownUpRoundTrip(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	initialVersion, latestVersion, err := provider.GetVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initialVersion != latestVersion || latestVersion < 190 {
		t.Fatalf("migration version = %d/%d, want latest at least 190", initialVersion, latestVersion)
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('host_embed_tickets') IS NOT NULL`).Scan(&tableExists); err != nil || !tableExists {
		t.Fatalf("ticket table exists=%t err=%v", tableExists, err)
	}
	if _, err := provider.DownTo(ctx, 189); err != nil {
		t.Fatalf("roll back embed ticket migration: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('host_embed_tickets') IS NOT NULL`).Scan(&tableExists); err != nil || tableExists {
		t.Fatalf("ticket table after rollback exists=%t err=%v", tableExists, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply embed ticket migration: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('host_embed_tickets') IS NOT NULL`).Scan(&tableExists); err != nil || !tableExists {
		t.Fatalf("ticket table after re-apply exists=%t err=%v", tableExists, err)
	}
}

func createEmbedTestSession(t *testing.T, ctx context.Context, store *Store, userID string) string {
	t.Helper()
	tokenID := testSessionTokenID(userID)
	secretHash := sha256.Sum256([]byte("embed-session:" + tokenID))
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: userID, Kind: identity.KindSession,
		SecretHash: secretHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return tokenID
}

func createEmbedTestConversation(t *testing.T, ctx context.Context, store *Store, ownerID, hostID string) string {
	t.Helper()
	id, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,host_id,model_id) VALUES ($1,$2,$3,'model-test')`, id, ownerID, hostID); err != nil {
		t.Fatal(err)
	}
	return id
}

func createEmbedTestTicket(t *testing.T, store *Store, hostID, conversationID, actorID, tokenID, installationID, target string, ceiling host.Permission) app.HostEmbedTicketRecord {
	t.Helper()
	raw := fmt.Sprintf("test-embed-ticket-%s-%s-%d", actorID, conversationID, time.Now().UTC().UnixNano())
	hash := sha256.Sum256([]byte(raw))
	value := app.HostEmbedTicketRecord{
		TicketHash: hash[:], HostID: hostID, ConversationID: conversationID, ActorID: actorID,
		PortalTokenID: tokenID, ClientInstallationID: installationID, ServiceInstanceID: hostEmbedTestInstanceID,
		PortalOrigin: "https://portal.example.test", InitialTarget: target, EmbedNonce: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		TargetGrants: []app.HostEmbedTargetGrant{{Target: target, PermissionCeiling: ceiling}},
		RuntimeGeneration: 3, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := store.CreateHostEmbedTicket(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	return value
}
