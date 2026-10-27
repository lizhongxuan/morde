package postgres

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestOrphanHostVolumeCleanupSurvivesMissingHostAndGuardsHostReuse(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	instanceID, hostID := "cleanup-test", "h7654321"
	admin := createTestSuperAdmin(t, store)
	user := createTestUser(t, store, 1, 2000, 4096)

	pending, err := store.EnsureOrphanHostVolumeCleanup(ctx, instanceID, hostID, "default", "orphan-sandbox", 1, true)
	if err != nil || !pending {
		t.Fatalf("ensure missing-Host cleanup = pending %t, err %v", pending, err)
	}
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 33); err == nil || !strings.Contains(err.Error(), "cannot roll back with pending orphan Host volume cleanups") {
		closeProvider()
		t.Fatalf("migration rollback with pending cleanup = %v", err)
	}
	closeProvider()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("restore latest migration after rejected rollback: %v", err)
	}
	user, err = store.GetUserByID(ctx, user.ID)
	if err != nil || user.Status != identity.StatusPending {
		t.Fatalf("migrated user status = %q, %v; want pending", user.Status, err)
	}
	user.Status = identity.StatusApproved
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusPending, user); err != nil {
		t.Fatalf("approve migrated user for cleanup checks: %v", err)
	}
	params := testCreateHostParams(t, user.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	params.Host.ID = hostID
	if _, _, err := store.CreateHost(ctx, params); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("create Host while volume cleanup is pending = %v, want conflict", err)
	}

	values, err := store.ListOrphanHostVolumeCleanups(ctx, instanceID)
	if err != nil || len(values) != 1 || values[0] != hostID {
		t.Fatalf("list missing-Host cleanup = %v, %v", values, err)
	}
	if err := store.CompleteOrphanHostVolumeCleanup(ctx, instanceID, hostID); err != nil {
		t.Fatal(err)
	}
	values, err = store.ListOrphanHostVolumeCleanups(ctx, instanceID)
	if err != nil || len(values) != 0 {
		t.Fatalf("list completed cleanup = %v, %v; want empty", values, err)
	}

	if _, _, err := store.CreateHost(ctx, params); err != nil {
		t.Fatalf("create Host after cleanup completed: %v", err)
	}
	if _, err := store.EnsureOrphanHostVolumeCleanup(ctx, instanceID, hostID, "default", "orphan-sandbox", 1, true); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("queue cleanup for active Host = %v, want conflict", err)
	}
}

func TestOrphanHostVolumeCleanupRetainsCompletedSandboxIdentity(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	instanceID, hostID := "cleanup-test", "h7654321"

	pending, err := store.EnsureOrphanHostVolumeCleanup(ctx, instanceID, hostID, "default", "orphan-sandbox", 1, false)
	if err != nil || !pending {
		t.Fatalf("ensure terminated-sandbox cleanup = pending %t, err %v", pending, err)
	}
	if err := store.CompleteOrphanHostVolumeCleanup(ctx, instanceID, hostID); err != nil {
		t.Fatal(err)
	}
	pending, err = store.EnsureOrphanHostVolumeCleanup(ctx, instanceID, hostID, "default", "orphan-sandbox", 1, false)
	if err != nil || pending {
		t.Fatalf("repeat completed sandbox cleanup = pending %t, err %v; want completed", pending, err)
	}
	pending, err = store.EnsureOrphanHostVolumeCleanup(ctx, instanceID, hostID, "default", "replacement-sandbox", 2, false)
	if err != nil || !pending {
		t.Fatalf("new sandbox identity cleanup = pending %t, err %v", pending, err)
	}
	values, err := store.ListOrphanHostVolumeCleanups(ctx, instanceID)
	if err != nil || len(values) != 1 || values[0] != hostID {
		t.Fatalf("list replacement cleanup = %v, %v", values, err)
	}
}
