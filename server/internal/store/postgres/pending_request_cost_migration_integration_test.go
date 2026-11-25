package postgres

import (
	"context"
	"testing"
)

func TestPendingRequestCostMigrationRepairsAppliedVersionSchemaDrift(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 248); err != nil {
		closeProvider()
		t.Fatalf("prepare schema at migration 248: %v", err)
	}
	closeProvider()
	if _, err := pool.Exec(ctx, `ALTER TABLE task_pending_requests
DROP CONSTRAINT IF EXISTS task_pending_requests_this_cost_micros_check,
DROP COLUMN IF EXISTS this_cost_micros,
ADD COLUMN current_cost_micros bigint`); err != nil {
		t.Fatalf("simulate the deployed cost-column drift: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("repair schema at migration 249: %v", err)
	}
	var expectedColumn, legacyColumn, expectedConstraint bool
	if err := pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='task_pending_requests' AND column_name='this_cost_micros'),
EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='task_pending_requests' AND column_name='current_cost_micros'),
EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='task_pending_requests'::regclass AND conname='task_pending_requests_this_cost_micros_check')`).Scan(
		&expectedColumn, &legacyColumn, &expectedConstraint); err != nil {
		t.Fatalf("inspect repaired task pending request schema: %v", err)
	}
	if !expectedColumn || !legacyColumn || !expectedConstraint {
		t.Fatalf("repaired schema: this_cost=%t current_cost=%t check=%t; want both columns and the nonnegative nullable check",
			expectedColumn, legacyColumn, expectedConstraint)
	}

	owner := createTestUser(t, store, 1, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "pending-cost-column-repair", "read task details")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit read-only fixture Task: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.ID != created.Task.ID {
		t.Fatalf("read Task details after schema repair: task=%+v err=%v", details.Task, err)
	}
}
