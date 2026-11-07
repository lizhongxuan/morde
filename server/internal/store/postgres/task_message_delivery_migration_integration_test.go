package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestConversationMessageDeliveryMigrationContract(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	var defaultValue string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT column_default, is_nullable='NO'
FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='conversation_messages' AND column_name='delivery_status'`).Scan(&defaultValue, &notNull); err != nil {
		t.Fatalf("read delivery status column: %v", err)
	}
	if defaultValue != "'applied'::text" || !notNull {
		t.Fatalf("delivery status default/not-null = %q/%t", defaultValue, notNull)
	}
	var checkExists, indexExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_constraint WHERE conname='conversation_messages_delivery_status_check'
), to_regclass('conversation_messages_pending_delivery') IS NOT NULL`).Scan(&checkExists, &indexExists); err != nil {
		t.Fatalf("read delivery status constraint/index: %v", err)
	}
	if !checkExists || !indexExists {
		t.Fatalf("delivery status constraint/index = %t/%t", checkExists, indexExists)
	}
}

func TestUnfinishedTaskConversationIndexIncludesPaused(t *testing.T) {
	_, pool := newTemporaryStore(t)
	var predicate string
	var unique bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_get_expr(indpred, indrelid),indisunique FROM pg_index
WHERE indexrelid=to_regclass('tasks_active_per_conversation')`).Scan(&predicate, &unique); err != nil {
		t.Fatalf("read unfinished Task conversation index predicate: %v", err)
	}
	if unique {
		t.Fatal("active conversation task lookup must allow multiple tasks")
	}
	for _, status := range []string{"queued", "running", "waiting_user", "pausing", "paused", "cancelling"} {
		if !strings.Contains(predicate, status) {
			t.Errorf("unfinished Task conversation index predicate %q omits %q", predicate, status)
		}
	}
}

func TestUnfinishedTaskConflictPreflightIncludesPausedWithoutMutation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	conversation := createMainConversationForTest(t, store, owner.ID)
	pausedID := mustID(t, domain.NewTaskID)
	queuedID := mustID(t, domain.NewTaskID)
	for _, task := range []struct{ id, status string }{{pausedID, "paused"}, {queuedID, "queued"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,model_id,objective,status,auth_kind,budget_active_seconds,budget_tool_calls,budget_model_tokens)
VALUES ($1,$2,$3,'migration-test','preserve old row',$4,'session',60,10,1000)`, task.id, owner.ID, conversation.ID, task.status); err != nil {
			t.Fatalf("insert %s Task: %v", task.status, err)
		}
	}
	query, err := os.ReadFile("../../../migrations/preflight/unfinished_task_conversation_conflicts.sql")
	if err != nil {
		t.Fatalf("read unfinished Task preflight: %v", err)
	}
	rows, err := pool.Query(ctx, string(query))
	if err != nil {
		t.Fatalf("run unfinished Task preflight: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("paused and queued Tasks were not reported as a conversation conflict")
	}
	var conversationID, ownerID string
	var taskIDs, statuses []string
	var conflictCount int64
	if err := rows.Scan(&conversationID, &ownerID, &taskIDs, &statuses, &conflictCount); err != nil {
		t.Fatalf("read unfinished Task conflict: %v", err)
	}
	if conversationID != conversation.ID || ownerID != owner.ID || conflictCount != 2 || len(taskIDs) != 2 || len(statuses) != 2 {
		t.Fatalf("preflight conflict = conversation %s owner %s tasks %v statuses %v count %d", conversationID, ownerID, taskIDs, statuses, conflictCount)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, conversation.ID).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("preflight changed old Tasks: remaining=%d err=%v", remaining, err)
	}
}
