package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/schedule"
)

func TestScheduleTitleMigrationBackfillsAndGuardsRollback(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 2_000, 4_096)
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 128); err != nil {
		t.Fatalf("roll schema back before title migration: %v", err)
	}

	scheduleID, err := schedule.NewID()
	if err != nil {
		t.Fatal(err)
	}
	const objective = "legacy model instruction"
	if _, err := store.pool.Exec(ctx, `INSERT INTO schedules (
 id, owner_id, version, status, execution_epoch, host_id, model_id, objective, timing,
 budget_active_seconds, budget_tool_calls, budget_model_tokens, auth_kind, auth_token_id, auth_scopes,
 misfire_policy, notification_events, next_occurrence_key, next_check_at
) VALUES ($1, $2, 1, 'enabled', 1, NULL, 'model-test', $3, '{"kind":"daily"}'::jsonb,
 60, 10, 1000, 'session', '', '{}', 'skip', ARRAY['completed']::text[], 'legacy-occurrence', $4)`,
		scheduleID, owner.ID, objective, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("insert legacy schedule row: %v", err)
	}
	if _, err := provider.UpTo(ctx, 129); err != nil {
		t.Fatalf("apply schedule title migration: %v", err)
	}
	assertScheduleTitle(t, ctx, store, scheduleID, objective)

	if _, err := store.pool.Exec(ctx, `UPDATE schedules SET title='custom display title' WHERE id=$1`, scheduleID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 128); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot drop schedule titles") {
		t.Fatalf("rollback with a custom title = %v, want guarded refusal", err)
	}
	assertScheduleTitle(t, ctx, store, scheduleID, "custom display title")

	if _, err := store.pool.Exec(ctx, `UPDATE schedules SET title=objective WHERE id=$1`, scheduleID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 128); err != nil {
		t.Fatalf("rollback with only derived titles: %v", err)
	}
	if _, err := provider.UpTo(ctx, 129); err != nil {
		t.Fatalf("reapply schedule title migration: %v", err)
	}
	assertScheduleTitle(t, ctx, store, scheduleID, objective)
}

func assertScheduleTitle(t *testing.T, ctx context.Context, store *Store, id, want string) {
	t.Helper()
	var got string
	if err := store.pool.QueryRow(ctx, `SELECT title FROM schedules WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("schedule title = %q; want %q", got, want)
	}
}
