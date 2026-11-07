package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPersonalMainConversationScopeMigrationRemovesProjectReferencesOnly(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 129); err != nil {
		t.Fatalf("roll back before personal main-conversation cleanup: %v", err)
	}

	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	personalOwner := createTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Project chat", "legacy main scope", "main-scope-project")
	projectConversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	projectConversation, replayed, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: projectConversationID, OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Project history survives",
	}, testIdempotency(owner.ID, "project_conversation.create", "main-scope-project-conversation"))
	if err != nil || replayed {
		t.Fatalf("create project conversation = replay %t, err %v", replayed, err)
	}
	// This database is deliberately at migration 129. Seed only the historical
	// rows needed to prove migration 130 preserves existing project history.
	taskID, messageID := mustID(t, domain.NewTaskID), mustID(t, domain.NewMessageID)
	if _, err := pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,project_id,initiator_id,model_id,objective,status,
auth_kind,auth_token_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,execution_location)
VALUES ($1,$2,$3,$4,$2,'migration-test-model','retained project history','queued','session',$5,120,5,1000,'server')`,
		taskID, owner.ID, projectConversation.ID, projectValue.ID, testSessionTokenID(owner.ID)); err != nil {
		t.Fatalf("seed historical project Task: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages(id,owner_id,actor_id,conversation_id,role,task_id,content,sequence)
VALUES ($1,$2,$2,$3,'user',$4,'retained project history',1)`, messageID, owner.ID, projectConversation.ID, taskID); err != nil {
		t.Fatalf("seed historical project message: %v", err)
	}

	var serviceInstanceID string
	if err := pool.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&serviceInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_main_conversations(service_instance_id,owner_id,conversation_id,version)
VALUES ($1::uuid,$2,$3,1)`, serviceInstanceID, owner.ID, projectConversation.ID); err != nil {
		t.Fatalf("seed legacy project main pointer: %v", err)
	}
	requestHash := sha256.Sum256([]byte("legacy project main result"))
	for _, operation := range []string{"main_conversation.ensure", "main_conversation.set"} {
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys(owner_id,operation,key,request_hash,resource_id,related_id)
VALUES ($1,$2,$3,$4,$5,'1')`, owner.ID, operation, "legacy-"+operation, requestHash[:], projectConversation.ID); err != nil {
			t.Fatalf("seed legacy %s replay: %v", operation, err)
		}
	}
	personalID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	personalRecord := app.IdempotencyRecord{OwnerID: personalOwner.ID, Operation: "main_conversation.ensure", Key: "personal-main-preserved",
		RequestHash: requestHash[:], ResourceID: personalID}
	if _, replayed, err := store.EnsureMainConversation(ctx, conversation.Conversation{ID: personalID, OwnerID: personalOwner.ID}, personalRecord); err != nil || replayed {
		t.Fatalf("create personal main conversation = replay %t, err %v", replayed, err)
	}

	if _, err := provider.UpTo(ctx, 130); err != nil {
		t.Fatalf("apply personal main-conversation cleanup: %v", err)
	}
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM account_main_conversations WHERE owner_id=$1 AND conversation_id=$2`, 0, owner.ID, projectConversation.ID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM account_main_conversations WHERE owner_id=$1 AND conversation_id=$2`, 1, personalOwner.ID, personalID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation IN ('main_conversation.ensure','main_conversation.set') AND resource_id=$2`, 0, owner.ID, projectConversation.ID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation='main_conversation.ensure' AND resource_id=$2`, 1, personalOwner.ID, personalID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation='project_conversation.create' AND resource_id=$2`, 1, owner.ID, projectConversation.ID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM conversations WHERE id=$1 AND owner_id=$2 AND project_id=$3`, 1, projectConversation.ID, owner.ID, projectValue.ID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND owner_id=$2 AND content='retained project history'`, 1, projectConversation.ID, owner.ID)
	assertMigrationRowCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND owner_id=$2`, 1, projectConversation.ID, owner.ID)
}

func assertMigrationRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query row count = %d; want %d", got, want)
	}
}
