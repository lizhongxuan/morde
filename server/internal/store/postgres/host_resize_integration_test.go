package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type integrationResizeSandbox struct {
	app.SandboxClient
	current app.SandboxResourceStatus
}

func (s *integrationResizeSandbox) ReadResources(context.Context, string) (app.SandboxResourceStatus, error) {
	return s.current, nil
}

func (s *integrationResizeSandbox) UpdateResources(_ context.Context, _ string, expected app.SandboxResourceStatus, target app.HostResources) (app.SandboxResourceStatus, error) {
	if s.current != expected {
		return app.SandboxResourceStatus{}, app.ErrSandboxConflict
	}
	s.current.Resources = target
	return s.current, nil
}

type integrationResizeRouter struct{ client app.SandboxClient }

func (r integrationResizeRouter) For(string) (app.SandboxClient, error) { return r.client, nil }
func (r integrationResizeRouter) Profiles() []string                    { return []string{"container"} }

func TestStoppedHostResourcesAreAtomicAndVersioned(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 3000, 4096)
	capacity := app.Capacity{CPUMilli: 8000, MemMiB: 16384}
	created := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetHostResources(ctx, created.ID)
	if err != nil || before.Version != 1 || before.State != host.StateStopped {
		t.Fatalf("initial resources: %#v, %v", before, err)
	}
	params := app.UpdateStoppedHostResourcesParams{
		HostID: created.ID, ExpectedVersion: before.Version,
		Resources: app.HostResources{CPUMilli: 1800, MemMiB: 3072}, Capacity: capacity,
		ActorID: "system", ActorKind: audit.ActorSystem,
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.UpdateStoppedHostResources(ctx, params)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var success, conflicts int
	for err := range results {
		switch {
		case err == nil:
			success++
		case domain.CodeOf(err) == domain.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected update error: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("concurrent update: success=%d conflict=%d", success, conflicts)
	}
	after, err := store.GetHostResources(ctx, created.ID)
	if err != nil || after.Version != 2 || after.Resources != params.Resources {
		t.Fatalf("confirmed resources: %#v, %v", after, err)
	}
	stored, err := store.GetHost(ctx, created.ID)
	if err != nil || stored.CPUMilli != 1800 || stored.MemMiB != 3072 || stored.CPULimitMilli != 1800 || stored.MemLimitMiB != 3072 {
		t.Fatalf("stored requests and limits: %#v, %v", stored, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='host.resources.update' AND target=$1`, created.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("resource audit count=%d err=%v", auditCount, err)
	}
	params.ExpectedVersion = after.Version
	params.Resources.CPUMilli = 4000
	if _, err := store.UpdateStoppedHostResources(ctx, params); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("quota error: %v", err)
	}
	params.Resources.CPUMilli = 1800
	params.Capacity.CPUMilli = 1000
	if _, err := store.UpdateStoppedHostResources(ctx, params); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("capacity error: %v", err)
	}
	unchanged, err := store.GetHostResources(ctx, created.ID)
	if err != nil || unchanged != after {
		t.Fatalf("failed updates changed resources: %#v, %v", unchanged, err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateStopped, host.StateCreating, "restore_pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	params.Capacity = capacity
	if _, err := store.UpdateStoppedHostResources(ctx, params); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("stopped-only method must route running host to online update: %v", err)
	}
}

func TestOnlineResourceReservationBlocksConcurrentCreateAndRestore(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 6144)
	roomy := app.Capacity{CPUMilli: 8000, MemMiB: 16384}
	running := createTestHost(t, ctx, store, owner.ID, roomy, nil)
	if err := store.MarkCreateAttempt(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-live-resize"
	if err := store.SetSandboxID(ctx, running.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, running.ID, 1, 0, "", "runtime-live-resize"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, running.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	stopped := createTestHost(t, ctx, store, owner.ID, roomy, nil)
	if _, err := store.TransitionHost(ctx, stopped.ID, host.StateCreating, host.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetHostResources(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	capacity := app.Capacity{CPUMilli: 3000, MemMiB: 4096}
	params := app.UpdateStoppedHostResourcesParams{
		HostID: running.ID, ExpectedVersion: before.Version,
		Resources: app.HostResources{CPUMilli: 2500, MemMiB: 3072},
		Capacity:  capacity, ActorID: "system", ActorKind: audit.ActorSystem,
	}
	reserved, err := store.BeginOnlineHostResources(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.OldRuntime != (app.HostResources{CPUMilli: running.CPULimitMilli, MemMiB: running.MemLimitMiB}) {
		t.Fatalf("old runtime = %#v", reserved.OldRuntime)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	createParams := testCreateHostParams(t, owner.ID, capacity, nil)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, err := store.CreateHost(ctx, createParams)
		errors <- err
	}()
	go func() {
		defer wg.Done()
		_, _, err := store.BeginHostRestore(ctx, app.RestoreHostParams{
			HostID: stopped.ID, RuntimeCredentialEnc: []byte("new runtime credential"),
			ExpiresAt: time.Now().UTC().Add(time.Hour), Capacity: capacity,
		})
		errors <- err
	}()
	wg.Wait()
	close(errors)
	for err := range errors {
		if domain.CodeOf(err) != domain.CodeCapacityExceeded {
			t.Fatalf("concurrent allocation err = %v", err)
		}
	}
	if err := store.AbortOnlineHostResources(ctx, reserved, true); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetHostResources(ctx, running.ID)
	if err != nil || after.UpdateState != "unknown" || after.Resources != before.Resources {
		t.Fatalf("unknown result = %#v, %v", after, err)
	}
	params.ExpectedVersion = after.Version
	if _, err := store.BeginOnlineHostResources(ctx, params); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unknown result allowed another resize: %v", err)
	}
	stopping, changed, err := store.BeginHostRelease(ctx, running.ID, false)
	if err != nil || !changed || stopping.State != host.StateStopping {
		t.Fatalf("release start = %#v, %t, %v", stopping, changed, err)
	}
	stillReserved, err := store.GetHostResources(ctx, running.ID)
	if err != nil || stillReserved.UpdateState != "unknown" {
		t.Fatalf("reservation disappeared before sandbox deletion: %#v, %v", stillReserved, err)
	}
	if _, err := store.CompleteHostRelease(ctx, running.ID, running.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.GetHostResources(ctx, running.ID)
	if err != nil || cleared.State != host.StateStopped || cleared.UpdateState != "idle" || cleared.Version != after.Version+1 {
		t.Fatalf("reservation after confirmed deletion = %#v, %v", cleared, err)
	}
	if _, changed, err := store.BeginHostRestore(ctx, app.RestoreHostParams{
		HostID: running.ID, RuntimeCredentialEnc: []byte("new runtime credential"),
		ExpiresAt: time.Now().UTC().Add(time.Hour), Capacity: capacity,
	}); err != nil || !changed {
		t.Fatalf("restore after verified release = changed %t, err %v", changed, err)
	}
}

func TestOnlineResourceCompletionPersistsLimitsAndAudit(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 4000, 6144)
	capacity := app.Capacity{CPUMilli: 4000, MemMiB: 6144}
	running := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	if err := store.MarkCreateAttempt(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-complete-resize"
	if err := store.SetSandboxID(ctx, running.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, running.ID, 1, 0, "", "runtime-complete-resize"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, running.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetHostResources(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := app.HostResources{CPUMilli: 1500, MemMiB: 3072}
	sandbox := &integrationResizeSandbox{current: app.SandboxResourceStatus{
		Resources:   app.HostResources{CPUMilli: running.CPULimitMilli, MemMiB: running.MemLimitMiB},
		ContainerID: "container-live-resize", StartedAt: "start-live-resize",
	}}
	service, err := app.NewAdminHostResourcesService(store, capacity, integrationResizeRouter{sandbox})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Update(ctx, identity.Principal{Kind: identity.KindSystem, Role: identity.RoleAdmin}, running.ID, target, before.Version)
	if err != nil || result.State != host.StateRunning || result.Resources != target || result.Version != before.Version+2 || sandbox.current.Resources != target {
		t.Fatalf("online completion = %#v, %v", result, err)
	}
	stored, err := store.GetHost(ctx, running.ID)
	if err != nil || stored.CPUMilli != 1500 || stored.MemMiB != 3072 || stored.CPULimitMilli != 1500 || stored.MemLimitMiB != 3072 {
		t.Fatalf("persisted limits = %#v, %v", stored, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='host.resources.update' AND target=$1`, running.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count = %d, %v", count, err)
	}
}
