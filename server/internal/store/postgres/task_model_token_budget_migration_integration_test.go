package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTaskModelTokenBudgetMigrationPreservesRowsAndGuardsDowngrade(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; Task token budget migration integration test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_task_token_budget_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	config := baseConfig.Copy()
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop Task token budget migration database: %v", err)
		}
		adminPool.Close()
	})

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 280); err != nil {
		closeProvider()
		t.Fatalf("prepare schema before Task token budget migration: %v", err)
	}
	closeProvider()

	const ownerID = "u_task_budget"
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,password_hash,role,status,quota_hosts,quota_cpu_milli,quota_mem_mib)
VALUES ($1,'task-token-budget','test','user','approved',3,4000,8192)`, ownerID); err != nil {
		t.Fatalf("insert migration test user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'Task token budget migration')`, ownerID); err != nil {
		t.Fatalf("insert migration test conversation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens)
VALUES ('q1234567',$1,$1,'c1234567','p1234567','existing Task','queued','session','',600,10,50000,0)`, ownerID); err != nil {
		t.Fatalf("insert pre-migration Task: %v", err)
	}

	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 281); err != nil {
		closeProvider()
		t.Fatalf("apply Task token budget migration: %v", err)
	}
	closeProvider()
	var existingBudget int32
	if err := pool.QueryRow(ctx, `SELECT budget_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&existingBudget); err != nil || existingBudget != 50000 {
		t.Fatalf("existing Task budget = %d, err=%v; want 50000", existingBudget, err)
	}
	for index, modelTokens := range []int64{120000, 2147483647} {
		taskID := []string{"q1234568", "q1234569"}[index]
		if _, err := pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens)
VALUES ($1,$2,$2,'c1234567','p1234567','expanded Task','queued','session','',600,10,$3,0)`, taskID, ownerID, modelTokens); err != nil {
			t.Fatalf("insert Task with %d tokens: %v", modelTokens, err)
		}
	}
	var scheduleConstraint string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
WHERE conrelid='schedules'::regclass AND conname='schedules_budget_model_tokens_check'`).Scan(&scheduleConstraint); err != nil || !strings.Contains(scheduleConstraint, "50000") {
		t.Fatalf("Schedule token constraint = %q, err=%v; expected 50000 ceiling", scheduleConstraint, err)
	}

	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 280); err == nil {
		closeProvider()
		t.Fatal("downgrade discarded Task budgets above 50000")
	}
	closeProvider()
	if _, err := pool.Exec(ctx, `DELETE FROM tasks WHERE id IN ('q1234568','q1234569')`); err != nil {
		t.Fatal(err)
	}
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 280); err != nil {
		closeProvider()
		t.Fatalf("downgrade while preserving 50000 Task budget: %v", err)
	}
	closeProvider()
}
