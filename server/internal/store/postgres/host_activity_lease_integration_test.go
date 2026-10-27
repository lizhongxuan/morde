package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
)

func TestPostgresHostActivityLeaseFencesExpiryAndRelease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 4)
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "preview-session-1", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 4, Kind: app.HostActivityLeaseKindPreview, TTL: 30 * time.Second,
	}

	wrongGeneration := lease
	wrongGeneration.RuntimeGeneration++
	if err := store.AcquireHostActivityLease(ctx, wrongGeneration); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("generation mismatch acquire error = %v", err)
	}
	wrongControlVersion := lease
	wrongControlVersion.ControlVersion++
	if err := store.AcquireHostActivityLease(ctx, wrongControlVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("control-version mismatch acquire error = %v", err)
	}

	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("acquire current lease: %v", err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || !active {
		t.Fatalf("active lease query = %t, err=%v", active, err)
	}
	if err := store.RenewHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("renew current lease: %v", err)
	}

	// A stale release is idempotent but must not delete the current fenced row.
	if err := store.ReleaseHostActivityLease(ctx, wrongControlVersion); err != nil {
		t.Fatalf("release stale lease: %v", err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || !active {
		t.Fatalf("stale release removed current lease: active=%t err=%v", active, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE host_activity_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE host_id=$1 AND lease_id=$2`, lease.HostID, lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("expired lease query = %t, err=%v", active, err)
	}
	if err := store.RenewHostActivityLease(ctx, lease); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired lease renewal error = %v", err)
	}
	newLease := lease
	newLease.LeaseID = "preview-session-2"
	if err := store.AcquireHostActivityLease(ctx, newLease); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	var expiredRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM host_activity_leases WHERE host_id=$1 AND lease_id=$2`, lease.HostID, lease.LeaseID).Scan(&expiredRows); err != nil {
		t.Fatal(err)
	}
	if expiredRows != 0 {
		t.Fatalf("expired lease rows after new acquire = %d, want 0", expiredRows)
	}
	if err := store.ReleaseHostActivityLease(ctx, newLease); err != nil {
		t.Fatalf("release lease acquired after expiry: %v", err)
	}

	if err := store.ReleaseHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("release expired lease: %v", err)
	}
	if err := store.ReleaseHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("repeat release: %v", err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("released lease query = %t, err=%v", active, err)
	}
}

func TestPostgresHostActivityLeaseDoesNotSurviveHostFenceChange(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "gateway-session-1", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 0, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("acquire current lease: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_version=control_version+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("lease remained active after control fence change: active=%t err=%v", active, err)
	}
	if err := store.RenewHostActivityLease(ctx, lease); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("renew after control fence change error = %v", err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=runtime_generation+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("lease remained active after generation change: active=%t err=%v", active, err)
	}
}

func TestPostgresHostReleaseSerializesWithActivityLeaseAcquire(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	if err := store.MarkCreateAttempt(ctx, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	hostValue, err := store.TransitionHost(ctx, hostValue.ID, host.StateCreating, host.StateRunning, "")
	if err != nil {
		t.Fatal(err)
	}
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "gateway-release-race", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: hostValue.ControlVersion, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
	}

	type releaseResult struct {
		host    host.Host
		changed bool
		err     error
	}
	start := make(chan struct{})
	acquireDone := make(chan error, 1)
	releaseDone := make(chan releaseResult, 1)
	go func() {
		<-start
		acquireDone <- store.AcquireHostActivityLease(ctx, lease)
	}()
	go func() {
		<-start
		value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false)
		releaseDone <- releaseResult{host: value, changed: changed, err: err}
	}()
	close(start)

	acquireErr := <-acquireDone
	released := <-releaseDone
	if released.err != nil {
		t.Fatalf("begin Host release: %v", released.err)
	}
	switch {
	case acquireErr == nil:
		if !released.changed || released.host.State != host.StateStopping {
			t.Fatalf("release did not close admission with a concurrent activity lease: state=%q changed=%t", released.host.State, released.changed)
		}
		if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || !active {
			t.Fatalf("stopping Host lost concurrent lease = %t, err=%v", active, err)
		}
	case domain.CodeOf(acquireErr) == domain.CodeConflict:
		if !released.changed || released.host.State != host.StateStopping {
			t.Fatalf("release/acquire race = %v state=%q changed=%t", acquireErr, released.host.State, released.changed)
		}
	default:
		t.Fatalf("unexpected activity lease acquisition error: %v", acquireErr)
	}
}

func TestPostgresHostDeleteKeepsExistingActivityLeasesVisibleAndClosesAdmission(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 7)
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "gateway-delete-drain", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 7, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("acquire activity lease before deletion: %v", err)
	}

	deleting, err := store.TransitionHost(ctx, hostValue.ID, host.StateRunning, host.StateDeleting, "")
	if err != nil || deleting.State != host.StateDeleting {
		t.Fatalf("admit Host deletion with an active lease = %#v, %v", deleting, err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || !active {
		t.Fatalf("deleting Host hid an existing activity lease: active=%t err=%v", active, err)
	}

	lateLease := lease
	lateLease.LeaseID = "gateway-delete-late"
	if err := store.AcquireHostActivityLease(ctx, lateLease); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("new activity lease after deletion admission error = %v, want conflict", err)
	}
	if err := store.ReleaseHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("release activity lease after deletion admission: %v", err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("released lease remained active while deleting: active=%t err=%v", active, err)
	}
}

func TestPostgresHostDeleteWaitsForControlTransitionAdmissionToSettle(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "gateway-control-drain", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 0, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("acquire activity lease before control transition: %v", err)
	}
	if _, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "delete-control-race",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	}); err != nil {
		t.Fatalf("begin control transition: %v", err)
	}
	if pending, err := store.HasOutstandingControlTransitionLeases(ctx, hostValue.ID, 0); err != nil || !pending {
		t.Fatalf("control transition lease before delete = %t, err=%v", pending, err)
	}
	if _, err := store.TransitionHost(ctx, hostValue.ID, host.StateRunning, host.StateDeleting, ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delete during an unfinished control transition error = %v, want conflict", err)
	}
	current, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || current.State != host.StateRunning || current.ControlState != host.ControlTakingOver {
		t.Fatalf("Host after refused delete during control transition = %#v, %v", current, err)
	}
	if pending, err := store.HasOutstandingControlTransitionLeases(ctx, hostValue.ID, 0); err != nil || !pending {
		t.Fatalf("refused delete hid the control transition lease = %t, err=%v", pending, err)
	}
}

func TestPostgresHostDeleteSerializesWithActivityLeaseAcquire(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 12, 80_000, 160_000)

	for range 8 {
		hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 9)
		lease := app.HostActivityLease{
			HostID: hostValue.ID, LeaseID: "gateway-delete-race", RuntimeGeneration: hostValue.RuntimeGeneration,
			ControlVersion: 9, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
		}
		type transitionResult struct {
			host host.Host
			err  error
		}
		start := make(chan struct{})
		acquireDone := make(chan error, 1)
		deleteDone := make(chan transitionResult, 1)
		go func() {
			<-start
			acquireDone <- store.AcquireHostActivityLease(ctx, lease)
		}()
		go func() {
			<-start
			value, err := store.TransitionHost(ctx, hostValue.ID, host.StateRunning, host.StateDeleting, "")
			deleteDone <- transitionResult{host: value, err: err}
		}()
		close(start)

		acquireErr := <-acquireDone
		deleted := <-deleteDone
		if deleted.err != nil || deleted.host.State != host.StateDeleting {
			t.Fatalf("delete/acquire race transition = %#v, %v", deleted.host, deleted.err)
		}
		active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration)
		if err != nil {
			t.Fatalf("query lease after delete/acquire race: %v", err)
		}
		switch {
		case acquireErr == nil:
			if !active {
				t.Fatal("Host deletion hid an activity lease admitted before deletion")
			}
		case domain.CodeOf(acquireErr) == domain.CodeConflict:
			if active {
				t.Fatal("Host deletion admitted a lease after closing admission")
			}
		default:
			t.Fatalf("unexpected concurrent activity lease acquire error: %v", acquireErr)
		}
	}
}

func TestPostgresHostReleaseSerializesWithMCPRuntimeOperation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 3, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "release-mcp-race-runtime"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: runtimeInstanceID,
		ControlVersion: hostValue.ControlVersion, AIAllowed: true,
	}
	key := app.MCPRuntimeOperationKey{
		OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "release-race-session",
		HostID: hostValue.ID, IdempotencyKey: "release-race-call",
	}
	request := newMCPRuntimeBeginRequest(t, key, fence, 1)
	type releaseResult struct {
		host    host.Host
		changed bool
		err     error
	}
	start := make(chan struct{})
	operationDone := make(chan error, 1)
	releaseDone := make(chan releaseResult, 1)
	go func() {
		<-start
		_, _, err := store.BeginMCPRuntimeOperation(ctx, request)
		operationDone <- err
	}()
	go func() {
		<-start
		value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false)
		releaseDone <- releaseResult{host: value, changed: changed, err: err}
	}()
	close(start)

	operationErr := <-operationDone
	released := <-releaseDone
	if released.err != nil {
		t.Fatalf("begin Host release: %v", released.err)
	}
	switch {
	case operationErr == nil:
		if released.changed || released.host.State != host.StateRunning {
			t.Fatalf("release passed a concurrent MCP operation: state=%q changed=%t", released.host.State, released.changed)
		}
	case domain.CodeOf(operationErr) == domain.CodeInvalidState:
		if !released.changed || released.host.State != host.StateStopping {
			t.Fatalf("MCP operation passed concurrent release: operation=%v state=%q changed=%t", operationErr, released.host.State, released.changed)
		}
	default:
		t.Fatalf("unexpected MCP runtime begin error: %v", operationErr)
	}
}

func TestPostgresControlTransitionSeesOldGatewayLeasesUntilReleased(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	lease := app.HostActivityLease{
		HostID: hostValue.ID, LeaseID: "gateway-old-fence", RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 0, Kind: app.HostActivityLeaseKindGateway, TTL: 30 * time.Second,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("acquire old-fence lease: %v", err)
	}
	transition, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "takeover-lease-drain",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	})
	if err != nil {
		t.Fatalf("begin control transition: %v", err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, lease.HostID, lease.RuntimeGeneration); err != nil || active {
		t.Fatalf("old-fence lease counted as current activity: active=%t err=%v", active, err)
	}
	if pending, err := store.HasOutstandingControlTransitionLeases(ctx, hostValue.ID, transition.ExpectedControlVersion); err != nil || !pending {
		t.Fatalf("transition drain query = %t, err=%v; want outstanding old lease", pending, err)
	}
	if err := store.ReleaseHostActivityLease(ctx, lease); err != nil {
		t.Fatalf("release old-fence lease: %v", err)
	}
	if pending, err := store.HasOutstandingControlTransitionLeases(ctx, hostValue.ID, transition.ExpectedControlVersion); err != nil || pending {
		t.Fatalf("drain query after release = %t, err=%v; want clear", pending, err)
	}
}

func TestPostgresUserInputPermitSerializesWithControlTransition(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 7)
	ref := host.Ref{
		ID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: 7, ControlState: host.ControlUser,
	}

	inputStarted := make(chan struct{})
	releaseInput := make(chan struct{})
	inputDone := make(chan error, 1)
	go func() {
		inputDone <- store.WithHostInputPermit(ctx, ref, func() error {
			close(inputStarted)
			<-releaseInput
			return nil
		})
	}()
	<-inputStarted

	transitionStarted := make(chan struct{})
	transitionDone := make(chan error, 1)
	go func() {
		close(transitionStarted)
		_, err := store.BeginControlTransition(ctx, control.BeginRequest{
			HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "input-permit-release",
			Action: control.ActionRelease, ExpectedControlVersion: 7,
		})
		transitionDone <- err
	}()
	<-transitionStarted
	select {
	case err := <-transitionDone:
		t.Fatalf("control transition passed an active user input write: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseInput)
	if err := <-inputDone; err != nil {
		t.Fatalf("finish user input permit: %v", err)
	}
	if err := <-transitionDone; err != nil {
		t.Fatalf("begin release after input completed: %v", err)
	}
	if err := store.WithHostInputPermit(ctx, ref, func() error { return nil }); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("old user input fence after transition error = %v", err)
	}
}
