package postgres

import (
	"context"
	"testing"
)

func TestTaskMediaStagingMigrationRoundTripsSchema(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()

	if _, err := provider.DownTo(ctx, 131); err != nil {
		t.Fatalf("roll schema back before Task media migration: %v", err)
	}
	assertTaskMediaStagingSchema(t, ctx, store, false)
	if _, err := provider.UpTo(ctx, 132); err != nil {
		t.Fatalf("apply Task media staging migration: %v", err)
	}
	assertTaskMediaStagingSchema(t, ctx, store, true)
	if _, err := provider.DownTo(ctx, 131); err != nil {
		t.Fatalf("roll back empty Task media staging migration: %v", err)
	}
	assertTaskMediaStagingSchema(t, ctx, store, false)
	if _, err := provider.UpTo(ctx, 132); err != nil {
		t.Fatalf("reapply Task media staging migration: %v", err)
	}
	assertTaskMediaStagingSchema(t, ctx, store, true)
}

func assertTaskMediaStagingSchema(t *testing.T, ctx context.Context, store *Store, want bool) {
	t.Helper()
	var tableExists, viewExists, approvalColumnExists, initiatorColumnExists bool
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('task_media_staged_objects') IS NOT NULL`).Scan(&tableExists); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('task_media_operation_details') IS NOT NULL`).Scan(&viewExists); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'task_pending_requests'
    AND column_name = 'media_preview_operation_id'
)`).Scan(&approvalColumnExists); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'task_media_staged_objects'
    AND column_name = 'initiator_id'
)`).Scan(&initiatorColumnExists); err != nil {
		t.Fatal(err)
	}
	if tableExists != want || viewExists != want || approvalColumnExists != want || initiatorColumnExists != want {
		t.Fatalf("Task media staging schema table/view/approval/initiator = %t/%t/%t/%t, want %t", tableExists, viewExists, approvalColumnExists, initiatorColumnExists, want)
	}
}
