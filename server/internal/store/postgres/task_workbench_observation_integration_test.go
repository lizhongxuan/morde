package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestTaskWorkbenchObservationTicketPersistenceIsHashOnlyAndSingleUse(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	tokenID := testSessionTokenID(owner.ID)
	secretHash := sha256.Sum256([]byte("observation session token:" + tokenID))
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: owner.ID, Kind: identity.KindSession,
		SecretHash: secretHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,host_id,model_id) VALUES ($1,$2,$3,'model-test')`,
		conversationID, owner.ID, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,conversation_id,host_id,model_id,objective,status,auth_kind,auth_token_id,
budget_active_seconds,budget_tool_calls,budget_model_tokens)
VALUES ($1,$2,$2,$3,$4,'model-test','private task','queued','session',$5,60,10,1000)`, taskID, owner.ID, conversationID, hostValue.ID, tokenID); err != nil {
		t.Fatal(err)
	}
	ticketHash := sha256.Sum256([]byte("ticket raw credential"))
	nonceHash := sha256.Sum256([]byte("ticket nonce"))
	value := app.TaskWorkbenchObservationRecord{
		TicketHash: ticketHash[:], Nonce: nonceHash[:], TaskID: taskID, HostID: hostValue.ID, ActorID: owner.ID,
		SessionTokenID: tokenID, RuntimeGeneration: hostValue.RuntimeGeneration, Scope: "task:read", ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := store.CreateTaskWorkbenchObservationTicket(ctx, value); err != nil {
		t.Fatalf("create observation ticket: %v", err)
	}
	stored, err := store.GetTaskWorkbenchObservationTicket(ctx, ticketHash[:])
	if err != nil || stored.TaskID != taskID || stored.HostID != hostValue.ID || len(stored.TicketHash) != sha256.Size || len(stored.SessionHash) != 0 {
		t.Fatalf("read hash-only observation ticket = %#v, %v", stored, err)
	}
	wrongGeneration := hostValue.RuntimeGeneration + 1
	wrongSessionHash := sha256.Sum256([]byte("wrong generation session"))
	if _, err := store.ConsumeTaskWorkbenchObservationTicket(ctx, ticketHash[:], wrongSessionHash[:], wrongGeneration, time.Now().UTC().Add(5*time.Minute)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-generation ticket exchange error = %v, want not found", err)
	}
	sessionHash := sha256.Sum256([]byte("session raw credential"))
	sessionExpiry := time.Now().UTC().Add(5 * time.Minute)
	consumed, err := store.ConsumeTaskWorkbenchObservationTicket(ctx, ticketHash[:], sessionHash[:], hostValue.RuntimeGeneration, sessionExpiry)
	if err != nil || consumed.UsedAt == nil || consumed.TaskID != taskID || len(consumed.SessionHash) != sha256.Size {
		t.Fatalf("consume observation ticket = %#v, %v", consumed, err)
	}
	if _, err := store.ConsumeTaskWorkbenchObservationTicket(ctx, ticketHash[:], sessionHash[:], hostValue.RuntimeGeneration, sessionExpiry); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("replayed observation ticket error = %v, want not found", err)
	}
	active, err := store.GetTaskWorkbenchObservationSession(ctx, sessionHash[:], time.Now().UTC())
	if err != nil || active.TaskID != taskID || active.ActorID != owner.ID || !active.SessionExpiresAt.Equal(consumed.SessionExpiresAt) {
		t.Fatalf("read active observation session = %#v, %v", active, err)
	}
	if _, err := store.GetTaskWorkbenchObservationSession(ctx, sessionHash[:], time.Now().UTC().Add(10*time.Minute)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired observation session error = %v, want not found", err)
	}
	expiredHash := sha256.Sum256([]byte("expired ticket"))
	expired := value
	expired.TicketHash = expiredHash[:]
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := store.CreateTaskWorkbenchObservationTicket(ctx, expired); err != nil {
		t.Fatalf("persist expired ticket fixture: %v", err)
	}
	if _, err := store.GetTaskWorkbenchObservationTicket(ctx, expiredHash[:]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired observation ticket read error = %v, want not found", err)
	}
}

func TestTaskWorkbenchObservationMigrationDownUpRoundTrip(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()

	initialVersion, latestVersion, err := provider.GetVersions(ctx)
	if err != nil {
		t.Fatalf("read initial migration versions: %v", err)
	}
	if initialVersion != latestVersion || initialVersion < 125 {
		t.Fatalf("initial migration versions = %d/%d; want fully migrated schema at or after 125", initialVersion, latestVersion)
	}
	assertTaskWorkbenchObservationMigrationTable(t, ctx, pool, true)

	if _, err := provider.DownTo(ctx, 124); err != nil {
		t.Fatalf("roll back observation migration to version 124: %v", err)
	}
	versionAfterDown, _, err := provider.GetVersions(ctx)
	if err != nil {
		t.Fatalf("read migration version after rollback: %v", err)
	}
	if versionAfterDown != 124 {
		t.Fatalf("migration version after rollback = %d; want 124", versionAfterDown)
	}
	assertTaskWorkbenchObservationMigrationTable(t, ctx, pool, false)
	if initialVersion > 125 {
		assertWorkspaceHardlinkMigrationColumn(t, ctx, pool, false)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("reapply migrations after observation rollback: %v", err)
	}
	versionAfterUp, latestAfterUp, err := provider.GetVersions(ctx)
	if err != nil {
		t.Fatalf("read migration versions after reapplying: %v", err)
	}
	if versionAfterUp != initialVersion || latestAfterUp != latestVersion {
		t.Fatalf("migration versions after reapplying = %d/%d; want original %d/%d", versionAfterUp, latestAfterUp, initialVersion, latestVersion)
	}
	assertTaskWorkbenchObservationMigrationTable(t, ctx, pool, true)
	if initialVersion > 125 {
		assertWorkspaceHardlinkMigrationColumn(t, ctx, pool, true)
	}
}

func assertTaskWorkbenchObservationMigrationTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected bool) {
	t.Helper()
	var actual bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('task_workbench_observation_tickets') IS NOT NULL`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("Task Workbench observation table exists = %t; want %t", actual, expected)
	}
}
