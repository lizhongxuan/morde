package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresClaimNextTaskWithOwnerLimitIsAtomicAcrossStores(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	otherPool, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
	if err != nil {
		t.Fatalf("open second Portal database pool: %v", err)
	}
	t.Cleanup(otherPool.Close)
	otherStore := New(otherPool)

	ownerA := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ownerB := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	for i := range 3 {
		if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, ownerA.ID, "", fmt.Sprintf("owner-a-%d", i), "owner A task")); err != nil {
			t.Fatalf("submit owner A task %d: %v", i, err)
		}
	}
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, ownerB.ID, "", "owner-b", "owner B task")); err != nil {
		t.Fatalf("submit owner B task: %v", err)
	}

	type claimResult struct {
		ownerID string
		found   bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 3)
	var workers sync.WaitGroup
	for _, candidate := range []struct {
		store  *Store
		worker string
	}{{store, "portal-one-a"}, {otherStore, "portal-two-a"}, {store, "portal-one-b"}} {
		candidate := candidate
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			claimed, found, err := candidate.store.ClaimNextTaskWithOwnerLimit(ctx, candidate.worker, time.Minute, 2)
			results <- claimResult{ownerID: claimed.OwnerID, found: found, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	ownerAClaims := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("claim task: %v", result.err)
		}
		if result.found && result.ownerID == ownerA.ID {
			ownerAClaims++
		}
	}
	if ownerAClaims != 2 {
		t.Fatalf("owner A claims = %d, want exactly 2 under concurrent claims", ownerAClaims)
	}

	other, found, err := otherStore.ClaimNextTaskWithOwnerLimit(ctx, "portal-two-b", time.Minute, 2)
	if err != nil || !found || other.OwnerID != ownerB.ID {
		t.Fatalf("claim from a different account at capacity: owner=%q found=%t err=%v, want owner B", other.OwnerID, found, err)
	}
}

func TestPostgresClaimNextTaskWithOwnerLimitReclaimsExpiredLease(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	for i := range 2 {
		if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", fmt.Sprintf("expired-%d", i), "expired lease task")); err != nil {
			t.Fatalf("submit task %d: %v", i, err)
		}
	}
	first, found, err := store.ClaimNextTaskWithOwnerLimit(ctx, "worker-before-expiry", time.Minute, 1)
	if err != nil || !found {
		t.Fatalf("initial claim: found=%t err=%v", found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatalf("expire first Task lease: %v", err)
	}
	reclaimed, found, err := store.ClaimNextTaskWithOwnerLimit(ctx, "worker-after-expiry", time.Minute, 1)
	if err != nil || !found || reclaimed.ID != first.ID || reclaimed.Lease.Version != first.Lease.Version+1 {
		t.Fatalf("reclaim expired Task: id=%q version=%d found=%t err=%v, want id=%q version=%d",
			reclaimed.ID, reclaimed.Lease.Version, found, err, first.ID, first.Lease.Version+1)
	}
	if _, found, err := store.ClaimNextTaskWithOwnerLimit(ctx, "worker-at-capacity", time.Minute, 1); err != nil || found {
		t.Fatalf("claim while reclaimed lease is active: found=%t err=%v, want no task", found, err)
	}
}
