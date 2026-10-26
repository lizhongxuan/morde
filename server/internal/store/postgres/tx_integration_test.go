package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAtomicPostgresOperations(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}

	owner := createTestUser(t, store, 1, 2000, 4096)
	created := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	if created.State != host.StateCreating || len(created.Workspaces) != 0 {
		t.Fatalf("created host = %#v", created)
	}

	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatalf("mark first create attempt: %v", err)
	}
	if err := store.MarkCreateAttempt(ctx, created.ID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("second create attempt error = %v", err)
	}

	activeAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.TouchHosts(ctx, []string{created.ID}, activeAt); err != nil {
		t.Fatalf("touch host: %v", err)
	}
	activeHost, err := store.GetHost(ctx, created.ID)
	if err != nil || activeHost.LastActiveAt == nil || !activeHost.LastActiveAt.Equal(activeAt) {
		t.Fatalf("last active = %v, err=%v; want %v", activeHost.LastActiveAt, err, activeAt)
	}
	if err := store.TouchHosts(ctx, nil, activeAt); err != nil {
		t.Fatalf("empty activity batch: %v", err)
	}

	if _, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, nil)); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("owner quota error = %v", err)
	}

	capacityOwner := createTestUser(t, store, 3, 10_000, 20_000)
	capacityLimited := app.Capacity{CPUMilli: 1_000, MemMiB: 4_096}
	if _, _, err := store.CreateHost(ctx, testCreateHostParams(t, capacityOwner.ID, capacityLimited, nil)); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("cluster capacity error = %v", err)
	}

	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatalf("transition creating host to running: %v", err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateFailed, "stale"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale host transition error = %v", err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateRunning, host.StateDeleting, ""); err != nil {
		t.Fatalf("transition running host to deleting: %v", err)
	}
	standalone := createTestWorkspace(t, ctx, store, owner.ID, "deletable")
	claimed, err := store.ClaimWorkspaceForDelete(ctx, standalone.ID, standalone.Revision)
	if err != nil || claimed.State != workspace.StateDeleting {
		t.Fatalf("claimed workspace = %#v, err=%v", claimed, err)
	}
	if _, err := store.ClaimWorkspaceForDelete(ctx, standalone.ID, standalone.Revision); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("second workspace claim error = %v", err)
	}
	if completed, err := store.CompleteWorkspaceDelete(ctx, claimed.ID, claimed.Revision); err != nil || completed.State != workspace.StateDeleted {
		t.Fatalf("complete workspace deletion = %#v, err=%v", completed, err)
	}

	t.Run("parallel quota and workspace attach", func(t *testing.T) {
		parallelOwner := createTestUser(t, store, 1, 20_000, 40_000)
		const attempts = 8
		paramsList := make([]app.CreateHostParams, attempts)
		for i := range attempts {
			paramsList[i] = testCreateHostParams(t, parallelOwner.ID, capacity, nil)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var succeeded int
		var failures []error
		for _, params := range paramsList {
			params := params
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := store.CreateHost(ctx, params)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					succeeded++
				} else {
					failures = append(failures, err)
				}
			}()
		}
		wg.Wait()
		if succeeded != 1 || len(failures) != attempts-1 {
			t.Fatalf("parallel creates: succeeded=%d failures=%d", succeeded, len(failures))
		}
		for _, err := range failures {
			if domain.CodeOf(err) != domain.CodeQuotaExceeded {
				t.Errorf("parallel create error = %v", err)
			}
		}

		attachOwner := createTestUser(t, store, 4, 20_000, 40_000)
		detached := createTestWorkspace(t, ctx, store, attachOwner.ID, "existing")
		const attachAttempts = 8
		attachParams := make([]app.CreateHostParams, attachAttempts)
		for i := range attachAttempts {
			attachParams[i] = testCreateHostParams(t, attachOwner.ID, capacity, nil)
			attachParams[i].WorkspaceIDs = []string{detached.ID}
		}
		wg = sync.WaitGroup{}
		mu = sync.Mutex{}
		succeeded = 0
		failures = nil
		for _, params := range attachParams {
			params := params
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := store.CreateHost(ctx, params)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					succeeded++
				} else {
					failures = append(failures, err)
				}
			}()
		}
		wg.Wait()
		if succeeded != 1 || len(failures) != attachAttempts-1 {
			t.Fatalf("parallel workspace attaches: succeeded=%d failures=%d", succeeded, len(failures))
		}
		for _, err := range failures {
			if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
				t.Errorf("parallel workspace attach error = %v", err)
			}
		}
	})

	t.Run("ticket is consumed once", func(t *testing.T) {
		tokenID, err := domain.NewTokenID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateToken(ctx, identity.Token{
			ID: tokenID, OwnerID: owner.ID, Kind: identity.KindTicket, HostID: created.ID,
			SecretHash: []byte("hash"), ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		const attempts = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		var succeeded int
		var failures []error
		for range attempts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := store.ConsumeTicket(ctx, tokenID)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					succeeded++
				} else {
					failures = append(failures, err)
				}
			}()
		}
		wg.Wait()
		if succeeded != 1 || len(failures) != attempts-1 {
			t.Fatalf("ticket consumes: succeeded=%d failures=%d", succeeded, len(failures))
		}
		for _, err := range failures {
			if !errors.Is(err, identity.ErrInvalidCredential) {
				t.Errorf("ticket consume error = %v", err)
			}
		}
	})

	t.Run("leader lock is session scoped", func(t *testing.T) {
		other := New(pool)
		lock, acquired, err := store.TryLeaderLock(ctx)
		if err != nil || !acquired || lock == nil {
			t.Fatalf("first lock acquired=%t lock=%v err=%v", acquired, lock, err)
		}
		if _, acquired, err := other.TryLeaderLock(ctx); err != nil || acquired {
			t.Fatalf("second lock acquired=%t err=%v", acquired, err)
		}
		if err := lock.Release(ctx); err != nil {
			t.Fatal(err)
		}
		if err := lock.Release(ctx); err != nil {
			t.Fatalf("repeated release: %v", err)
		}
		reacquired, acquired, err := other.TryLeaderLock(ctx)
		if err != nil || !acquired || reacquired == nil {
			t.Fatalf("lock reacquired=%t lock=%v err=%v", acquired, reacquired, err)
		}
		if err := reacquired.Release(ctx); err != nil {
			t.Fatal(err)
		}

		createLock, acquired, err := store.TryHostCreateLock(ctx, "h1234567")
		if err != nil || !acquired || createLock == nil {
			t.Fatalf("host create lock acquired=%t lock=%v err=%v", acquired, createLock, err)
		}
		if _, acquired, err := other.TryHostCreateLock(ctx, "h1234567"); err != nil || acquired {
			t.Fatalf("duplicate host create lock acquired=%t err=%v", acquired, err)
		}
		independentLock, acquired, err := other.TryHostCreateLock(ctx, "h7654321")
		if err != nil || !acquired || independentLock == nil {
			t.Fatalf("independent host create lock acquired=%t lock=%v err=%v", acquired, independentLock, err)
		}
		if err := independentLock.Release(ctx); err != nil {
			t.Fatal(err)
		}
		if err := createLock.Release(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHostCreateIdempotencyReplaysAtomically(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "host-create-retry")
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindAPIKey, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: owner.ID, Kind: identity.KindAPIKey, HostIDs: []string{},
		Scopes: []identity.Scope{identity.ScopeHostCreate}, Name: "host-create-retry",
		SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create Host create API key: %v", err)
	}
	params := testCreateHostParams(t, owner.ID, capacity, []string{workspaceValue.ID})
	params.GrantTokenID = token.ID
	requestHash := sha256.Sum256([]byte(`{"template_id":"small","name":"retry"}`))
	params.Idempotency = app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "host.create", Key: "host-create-retry-001",
		RequestHash: requestHash[:], ResourceID: params.Host.ID,
	}

	created, replayed, err := store.CreateHost(ctx, params)
	if err != nil || replayed || created.ID != params.Host.ID || len(created.Workspaces) != 1 {
		t.Fatalf("first CreateHost() = %q, replay=%t, err=%v", created.ID, replayed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET quota_hosts=0 WHERE id=$1`, owner.ID); err != nil {
		t.Fatalf("lower owner quota after create: %v", err)
	}

	retry, replayed, err := store.CreateHost(ctx, params)
	if err != nil || !replayed || retry.ID != created.ID || len(retry.Workspaces) != 1 {
		t.Fatalf("retry CreateHost() = %q, replay=%t, err=%v; want %q replay", retry.ID, replayed, err, created.ID)
	}
	updatedToken, err := store.GetTokenByID(ctx, token.ID)
	if err != nil || len(updatedToken.HostIDs) != 1 || updatedToken.HostIDs[0] != created.ID {
		t.Fatalf("API Key Host grant after replay = %#v, err=%v", updatedToken.HostIDs, err)
	}
	lookup, found, err := store.GetHostCreateReplay(ctx, params.Idempotency)
	if err != nil || !found || lookup.ID != created.ID {
		t.Fatalf("GetHostCreateReplay() = %q, found=%t, err=%v", lookup.ID, found, err)
	}

	changed := params
	changedHash := sha256.Sum256([]byte(`{"template_id":"small","name":"changed"}`))
	changed.Idempotency.RequestHash = changedHash[:]
	if _, _, err := store.CreateHost(ctx, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with changed request error = %v, want idempotency conflict", err)
	}
	var hostCount, keyCount, bindingCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE owner_id=$1`, owner.ID).Scan(&hostCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation='host.create'`, owner.ID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_bindings WHERE host_id=$1`, created.ID).Scan(&bindingCount); err != nil {
		t.Fatal(err)
	}
	if hostCount != 1 || keyCount != 1 || bindingCount != 1 {
		t.Fatalf("persisted hosts/idempotency keys/bindings = %d/%d/%d, want 1/1/1", hostCount, keyCount, bindingCount)
	}
}

func TestWorkspaceBindingCanMoveBetweenHosts(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	value := createTestWorkspace(t, ctx, store, owner.ID, "shared")
	firstParams := testCreateHostParams(t, owner.ID, capacity, []string{value.ID})
	first, _, err := store.CreateHost(ctx, firstParams)
	if err != nil || len(first.Workspaces) != 1 || first.Workspaces[0].State != workspace.BindingPreparing {
		t.Fatalf("create first host binding = %#v, err=%v", first.Workspaces, err)
	}
	active, err := store.TransitionWorkspaceBinding(ctx, first.ID, value.ID, 1, workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil || active.BindingVersion != 2 {
		t.Fatalf("activate first binding = %#v, err=%v", active, err)
	}
	unbinding, err := store.TransitionWorkspaceBinding(ctx, first.ID, value.ID, 2, workspace.BindingActive, workspace.BindingUnbinding, 0, 0)
	if err != nil || unbinding.BindingVersion != 3 {
		t.Fatalf("begin first unbind = %#v, err=%v", unbinding, err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, first.ID, value.ID, 3, workspace.BindingUnbinding, workspace.BindingDetached, 0, 0); err != nil {
		t.Fatal(err)
	}
	params := testCreateHostParams(t, owner.ID, capacity, []string{value.ID})
	second, _, err := store.CreateHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := store.GetWorkspaceBindingsByWorkspace(ctx, value.ID)
	if err != nil || len(bindings) != 2 || bindings[0].State != workspace.BindingDetached || bindings[1].HostID != second.ID || bindings[1].State != workspace.BindingPreparing {
		t.Fatalf("workspace bindings after rebind = %#v, err=%v", bindings, err)
	}
}

func TestWorkspaceBindingAppendEnforcesHostLimit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	workspaceIDs := make([]string, workspace.MaxBindingsPerHost)
	for i := range workspaceIDs {
		workspaceIDs[i] = createTestWorkspace(t, ctx, store, owner.ID, fmt.Sprintf("binding-limit-%02d", i)).ID
	}
	created := createTestHost(t, ctx, store, owner.ID, capacity, workspaceIDs)
	if len(created.Workspaces) != workspace.MaxBindingsPerHost {
		t.Fatalf("created host bindings = %d, want %d", len(created.Workspaces), workspace.MaxBindingsPerHost)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	extra := createTestWorkspace(t, ctx, store, owner.ID, "binding-limit-extra")
	_, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: created.ID, Workspace: extra, Directory: extra.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: extra.Revision,
	})
	if domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("append binding at host limit error = %v, want capacity exceeded", err)
	}
	bindings, err := store.GetWorkspaceBindingsByHost(ctx, created.ID)
	if err != nil || len(bindings) != workspace.MaxBindingsPerHost {
		t.Fatalf("host bindings after rejected append = %d, err=%v", len(bindings), err)
	}
	for _, binding := range bindings {
		if binding.State != workspace.BindingPreparing || binding.BindingVersion != 1 {
			t.Fatalf("existing binding changed after rejected append: %#v", binding)
		}
	}
	if _, err := store.GetWorkspaceBinding(ctx, created.ID, extra.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("rejected binding lookup error = %v, want not found", err)
	}
}

func TestWorkspaceBindingAppendSerializesAtHostLimit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	workspaceIDs := make([]string, workspace.MaxBindingsPerHost-1)
	for i := range workspaceIDs {
		workspaceIDs[i] = createTestWorkspace(t, ctx, store, owner.ID, fmt.Sprintf("binding-race-%02d", i)).ID
	}
	created := createTestHost(t, ctx, store, owner.ID, capacity, workspaceIDs)
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	extra := []workspace.Workspace{
		createTestWorkspace(t, ctx, store, owner.ID, "binding-race-extra-a"),
		createTestWorkspace(t, ctx, store, owner.ID, "binding-race-extra-b"),
	}
	start := make(chan struct{})
	errors := make(chan error, len(extra))
	for _, value := range extra {
		value := value
		go func() {
			<-start
			_, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
				OwnerID: owner.ID, HostID: created.ID, Workspace: value, Directory: value.ID,
				State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: value.Revision,
			})
			errors <- err
		}()
	}
	close(start)
	succeeded, limited := 0, 0
	for range extra {
		err := <-errors
		if err == nil {
			succeeded++
			continue
		}
		switch domain.CodeOf(err) {
		case domain.CodeCapacityExceeded:
			limited++
		default:
			t.Fatalf("concurrent append error = %v", err)
		}
	}
	if succeeded != 1 || limited != 1 {
		t.Fatalf("concurrent append results: succeeded=%d limited=%d, want 1 each", succeeded, limited)
	}
	bindings, err := store.GetWorkspaceBindingsByHost(ctx, created.ID)
	if err != nil || len(bindings) != workspace.MaxBindingsPerHost {
		t.Fatalf("host bindings after concurrent append = %d, err=%v", len(bindings), err)
	}
}

func TestWorkspaceBindingSyncStatusIsScopedAndIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	value := createTestWorkspace(t, ctx, store, owner.ID, "binding-sync-status")
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{value.ID}))
	if err != nil || len(created.Workspaces) != 1 {
		t.Fatalf("create Host binding = %#v, %v", created.Workspaces, err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	preparing := created.Workspaces[0]
	active, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, preparing.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, preparing.DesiredRevision, preparing.AppliedRevision)
	if err != nil {
		t.Fatalf("activate binding: %v", err)
	}

	paths := []string{"src/main.go", "README.md"}
	conflict, err := store.SetWorkspaceBindingSyncStatus(ctx, created.ID, value.ID, active.BindingVersion, workspace.BindingSyncConflict, paths)
	if err != nil || conflict.SyncState != workspace.BindingSyncConflict || !equalStringSlices(conflict.SyncConflicts, paths) {
		t.Fatalf("set conflict status = %#v, %v", conflict, err)
	}
	replayed, err := store.SetWorkspaceBindingSyncStatus(ctx, created.ID, value.ID, active.BindingVersion, workspace.BindingSyncConflict, paths)
	if err != nil || replayed.SyncState != workspace.BindingSyncConflict || !equalStringSlices(replayed.SyncConflicts, paths) {
		t.Fatalf("replay conflict status = %#v, %v", replayed, err)
	}
	healthy, err := store.SetWorkspaceBindingSyncStatus(ctx, created.ID, value.ID, active.BindingVersion, workspace.BindingSyncHealthy, nil)
	if err != nil || healthy.SyncState != workspace.BindingSyncHealthy || len(healthy.SyncConflicts) != 0 {
		t.Fatalf("clear conflict status = %#v, %v", healthy, err)
	}
	current, err := store.GetWorkspaceBinding(ctx, created.ID, value.ID)
	if err != nil || current.SyncState != workspace.BindingSyncHealthy || len(current.SyncConflicts) != 0 {
		t.Fatalf("read current binding status = %#v, %v", current, err)
	}
	if _, err := store.SetWorkspaceBindingSyncStatus(ctx, created.ID, value.ID, active.BindingVersion-1, workspace.BindingSyncConflict, paths); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale binding status update error = %v, want workspace conflict", err)
	}
}

func TestHostDeletionRequiresDetachedWorkspaceAndFencesRebind(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	value := createTestWorkspace(t, ctx, store, owner.ID, "delete-fence")
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{value.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateRunning, host.StateDeleting, ""); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("delete with preparing binding error = %v, want workspace busy", err)
	}
	unbinding, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, 1,
		workspace.BindingPreparing, workspace.BindingUnbinding, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	detached, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, unbinding.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateRunning, host.StateDeleting, ""); err != nil {
		t.Fatalf("delete after verified detach: %v", err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, detached.BindingVersion,
		workspace.BindingDetached, workspace.BindingPreparing, 0, 0); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("rebind while Host is deleting error = %v, want workspace busy", err)
	}
}

func TestFailedWorkspaceBindingRemainsExclusiveUntilRecovered(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	value := createTestWorkspace(t, ctx, store, owner.ID, "failed-binding")
	first, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{value.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, first.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	failed, err := store.TransitionWorkspaceBinding(ctx, first.ID, value.ID, 1,
		workspace.BindingPreparing, workspace.BindingFailed, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{value.ID})); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("bind Workspace while previous binding is failed error = %v, want workspace busy", err)
	}
	recovered, err := store.TransitionWorkspaceBinding(ctx, first.ID, value.ID, failed.BindingVersion,
		workspace.BindingFailed, workspace.BindingPreparing, 0, 0)
	if err != nil || recovered.State != workspace.BindingPreparing {
		t.Fatalf("recover failed binding on the same Host = %#v, %v", recovered, err)
	}
}

func TestWorkspaceBindingTransitionDoesNotRewindConcurrentWorkspaceRevision(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 100_000, 200_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	value := createTestWorkspace(t, ctx, store, owner.ID, "binding-revision-race")
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{value.ID}))
	if err != nil || len(created.Workspaces) != 1 {
		t.Fatalf("create host with preparing binding = %#v, err=%v", created.Workspaces, err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck
	var lockedID string
	if err := blocker.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, value.ID).Scan(&lockedID); err != nil {
		t.Fatalf("lock Workspace for sync commit: %v", err)
	}
	transition := make(chan error, 1)
	go func() {
		_, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, 1,
			workspace.BindingPreparing, workspace.BindingActive, 0, 0)
		transition <- err
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	waitForPostgresLockWait(t, waitCtx, pool, "%FROM workspaces WHERE id=$1 FOR UPDATE%", 1)
	if _, err := blocker.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes) VALUES ($1, 1, 0, 0)`, value.ID); err != nil {
		t.Fatalf("insert next immutable workspace revision: %v", err)
	}
	if _, err := blocker.Exec(ctx, `UPDATE workspaces SET revision=1, updated_at=now() WHERE id=$1`, value.ID); err != nil {
		t.Fatalf("advance workspace revision: %v", err)
	}
	if _, err := blocker.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=1, updated_at=now() WHERE host_id=$1 AND workspace_id=$2`, created.ID, value.ID); err != nil {
		t.Fatalf("advance binding desired revision: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent workspace sync: %v", err)
	}
	select {
	case err := <-transition:
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("stale binding transition error = %v, want workspace conflict", err)
		}
	case <-waitCtx.Done():
		t.Fatalf("binding transition did not finish after workspace sync: %v", waitCtx.Err())
	}

	current, err := store.GetWorkspaceBinding(ctx, created.ID, value.ID)
	if err != nil || current.State != workspace.BindingPreparing || current.BindingVersion != 1 ||
		current.DesiredRevision != 1 || current.AppliedRevision != 0 {
		t.Fatalf("binding after stale transition = %#v, err=%v", current, err)
	}
}

func TestAPIKeyHostGrantCommitsWithHost(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindAPIKey, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: owner.ID, Kind: identity.KindAPIKey,
		HostIDs: []string{}, Scopes: []identity.Scope{identity.ScopeHostCreate},
		Name: "create-host", SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	params := testCreateHostParams(t, owner.ID, capacity, nil)
	params.GrantTokenID = token.ID
	created, _, err := store.CreateHost(ctx, params)
	if err != nil {
		t.Fatalf("create host and grant API key: %v", err)
	}
	updatedToken, err := store.GetTokenByID(ctx, token.ID)
	if err != nil || len(updatedToken.HostIDs) != 1 || updatedToken.HostIDs[0] != created.ID {
		t.Fatalf("API key host grants = %#v, err=%v", updatedToken.HostIDs, err)
	}
	if err := store.RevokeToken(ctx, token.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	failedParams := testCreateHostParams(t, owner.ID, capacity, nil)
	failedParams.GrantTokenID = token.ID
	if _, _, err := store.CreateHost(ctx, failedParams); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revoked API key grant error = %v", err)
	}
	if _, err := store.GetHost(ctx, failedParams.Host.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("host persisted after grant rollback: %v", err)
	}
	if len(failedParams.WorkspaceIDs) != 0 {
		t.Fatal("failed host request unexpectedly contained workspaces")
	}
}

func newTemporaryStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; PostgreSQL atomic integration tests were not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse CH_TEST_DATABASE_URL: %v", err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatalf("connect to test PostgreSQL server: %v", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_test_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatalf("create temporary test database: %v", err)
	}

	testConfig := baseConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("connect to temporary test database: %v", err)
	}
	if err := Migrate(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("migrate temporary test database: %v", err)
	}

	t.Cleanup(func() {
		testPool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop temporary test database: %v", err)
		}
		adminPool.Close()
	})
	return New(testPool), testPool
}

func createTestUser(t *testing.T, store *Store, quotaHosts, quotaCPU, quotaMemory int) identity.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(context.Background(), identity.User{
		ID: id, Name: "user-" + id, PasswordHash: "hash", Role: identity.RoleUser,
		Status:     identity.StatusPending,
		QuotaHosts: quotaHosts, QuotaCPUMilli: quotaCPU, QuotaMemMiB: quotaMemory,
	})
	if err != nil {
		t.Fatal(err)
	}
	user.Status = identity.StatusApproved
	user, err = store.UpdateUser(context.Background(), "system", identity.StatusPending, user)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func createTestHost(t *testing.T, ctx context.Context, store *Store, ownerID string, capacity app.Capacity, workspaceIDs []string) host.Host {
	t.Helper()
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, ownerID, capacity, workspaceIDs))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func testCreateHostParams(t *testing.T, ownerID string, capacity app.Capacity, workspaceIDs []string) app.CreateHostParams {
	t.Helper()
	hostID, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	h := host.Host{
		ID: hostID, OwnerID: ownerID, Name: "test", TemplateID: "test-template", RuntimeProfile: "default",
		State: host.StateCreating, CPUMilli: 1000, MemMiB: 1024,
		CPULimitMilli: 2000, MemLimitMiB: 2048, AIOKeyEnc: []byte("encrypted"),
		RuntimeCredentialEnc: []byte("runtime encrypted"), RuntimeGeneration: 1,
	}
	return app.CreateHostParams{Host: h, WorkspaceIDs: append([]string(nil), workspaceIDs...), Capacity: capacity}
}

func createTestWorkspace(t *testing.T, ctx context.Context, store *Store, ownerID, name string) workspace.Workspace {
	t.Helper()
	id, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("workspace:" + id))
	value, replayed, err := store.CreateWorkspace(ctx, workspace.Workspace{
		ID: id, OwnerID: ownerID, Name: name, State: workspace.StateReady,
		QuotaBytes: 2_048 << 20,
	}, app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "workspace.create", Key: "test:" + id,
		RequestHash: hash[:], ResourceID: id,
	})
	if err != nil || replayed {
		t.Fatalf("create test workspace = %#v, replayed=%t, err=%v", value, replayed, err)
	}
	return value
}
