package postgres

import (
	"context"
	"testing"
)

func TestPostgresTaskStepRetryMigration(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	var stepColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='task_steps'
AND column_name IN ('attempt_no','retry_of_step_id','failure_reason','retryable')`).Scan(&stepColumns); err != nil || stepColumns != 4 {
		t.Fatalf("Task step retry columns=%d err=%v; want four migrated columns", stepColumns, err)
	}
	var intentTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.task_step_retry_intents') IS NOT NULL`).Scan(&intentTable); err != nil || !intentTable {
		t.Fatalf("Task step retry intent table exists=%t err=%v", intentTable, err)
	}
	var pendingSourceUnique bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND tablename='task_step_retry_intents'
AND indexname='task_step_retry_intents_one_pending_source' AND indexdef LIKE '%UNIQUE%')`).Scan(&pendingSourceUnique); err != nil || !pendingSourceUnique {
		t.Fatalf("one-pending-retry-per-source unique index exists=%t err=%v", pendingSourceUnique, err)
	}
}
