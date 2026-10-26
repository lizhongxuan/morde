package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
)

func TestPostgresControlTransitionTakeoverAndRelease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)

	takeover := control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "takeover-001",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	}
	started, err := store.BeginControlTransition(ctx, takeover)
	if err != nil {
		t.Fatalf("begin takeover: %v", err)
	}
	if started.Phase != control.PhaseBlocking || started.BlockedControlVersion != 1 || started.TargetControlVersion != 2 {
		t.Fatalf("started takeover = %#v", started)
	}

	replay, err := store.BeginControlTransition(ctx, takeover)
	if err != nil || replay.RequestID != started.RequestID || replay.Phase != started.Phase {
		t.Fatalf("idempotent takeover replay = %#v, err=%v", replay, err)
	}
	different := takeover
	different.Action = control.ActionRelease
	if _, err := store.BeginControlTransition(ctx, different); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("request ID reuse error = %v", err)
	}
	if _, err := store.GetActiveControlTransition(ctx, hostValue.ID); err != nil {
		t.Fatalf("get active transition: %v", err)
	}
	storedHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || storedHost.ControlState != host.ControlTakingOver || storedHost.ControlVersion != 1 {
		t.Fatalf("Host after begin = %#v, err=%v", storedHost, err)
	}

	if _, err := store.SetControlTransitionPhase(ctx, hostValue.ID, started.RequestID, control.PhaseDraining); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("draining without task snapshot error = %v", err)
	}
	if _, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, started.RequestID, json.RawMessage(`{"receipt_id":"receipt-1"}`)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("receipt before draining error = %v", err)
	}
	if _, err := store.CompleteControlTransition(ctx, hostValue.ID, started.RequestID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("takeover without receipt error = %v", err)
	}

	withTasks, err := store.SaveControlTaskIDs(ctx, hostValue.ID, started.RequestID, []string{"q0000002", "q0000001"})
	if err != nil || !withTasks.TaskIDsSaved || len(withTasks.TaskIDs) != 2 || withTasks.TaskIDs[0] != "q0000001" {
		t.Fatalf("save task snapshot = %#v, err=%v", withTasks, err)
	}
	if _, err := store.SaveControlTaskIDs(ctx, hostValue.ID, started.RequestID, []string{"q0000001"}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed task snapshot error = %v", err)
	}
	draining, err := store.SetControlTransitionPhase(ctx, hostValue.ID, started.RequestID, control.PhaseDraining)
	if err != nil || draining.Phase != control.PhaseDraining {
		t.Fatalf("advance to draining = %#v, err=%v", draining, err)
	}
	const receipt = `{"receipt_id":"receipt-1","target_control_version":2}`
	drained, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, started.RequestID, json.RawMessage(receipt))
	if err != nil || drained.Phase != control.PhaseDrained || !json.Valid(drained.DrainedReceipt) {
		t.Fatalf("save drained receipt = %#v, err=%v", drained, err)
	}
	// JSON object key ordering and insignificant whitespace do not change the
	// idempotent receipt value.
	if _, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, started.RequestID,
		json.RawMessage(`{ "target_control_version": 2, "receipt_id": "receipt-1" }`)); err != nil {
		t.Fatalf("equivalent receipt replay: %v", err)
	}
	if _, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, started.RequestID,
		json.RawMessage(`{"receipt_id":"different"}`)); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different receipt replay error = %v", err)
	}

	completed, err := store.CompleteControlTransition(ctx, hostValue.ID, started.RequestID)
	if err != nil || completed.Phase != control.PhaseCompleted {
		t.Fatalf("complete takeover = %#v, err=%v", completed, err)
	}
	if _, err := store.CompleteControlTransition(ctx, hostValue.ID, started.RequestID); err != nil {
		t.Fatalf("completed takeover replay: %v", err)
	}
	storedHost, err = store.GetHost(ctx, hostValue.ID)
	if err != nil || storedHost.ControlState != host.ControlUser || storedHost.ControlVersion != 2 {
		t.Fatalf("Host after takeover = %#v, err=%v", storedHost, err)
	}
	if _, err := store.GetActiveControlTransition(ctx, hostValue.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("active transition after completion error = %v", err)
	}

	release := control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "release-001",
		Action: control.ActionRelease, ExpectedControlVersion: 2,
	}
	releasing, err := store.BeginControlTransition(ctx, release)
	if err != nil || releasing.BlockedControlVersion != 3 || releasing.TargetControlVersion != 4 {
		t.Fatalf("begin release = %#v, err=%v", releasing, err)
	}
	// A saved empty snapshot remains distinguishable from a missing snapshot;
	// release does not require a drained receipt.
	releasing, err = store.SaveControlTaskIDs(ctx, hostValue.ID, release.RequestID, nil)
	if err != nil || !releasing.TaskIDsSaved || len(releasing.TaskIDs) != 0 {
		t.Fatalf("save empty release snapshot = %#v, err=%v", releasing, err)
	}
	if _, err := store.CompleteControlTransition(ctx, hostValue.ID, release.RequestID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("release without runtime drain error = %v", err)
	}
	if _, err := store.SetControlTransitionPhase(ctx, hostValue.ID, release.RequestID, control.PhaseDraining); err != nil {
		t.Fatalf("advance release to draining: %v", err)
	}
	if _, err := store.SetControlTransitionPhase(ctx, hostValue.ID, release.RequestID, control.PhaseDrained); err != nil {
		t.Fatalf("mark release drained: %v", err)
	}
	released, err := store.CompleteControlTransition(ctx, hostValue.ID, release.RequestID)
	if err != nil || released.Phase != control.PhaseCompleted {
		t.Fatalf("complete release = %#v, err=%v", released, err)
	}
	storedHost, err = store.GetHost(ctx, hostValue.ID)
	if err != nil || storedHost.ControlState != host.ControlAI || storedHost.ControlVersion != 4 {
		t.Fatalf("Host after release = %#v, err=%v", storedHost, err)
	}
}

func TestPostgresControlTransitionOwnershipCASAndFailure(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 7)

	request := control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "release-failure",
		Action: control.ActionRelease, ExpectedControlVersion: 6,
	}
	if _, err := store.BeginControlTransition(ctx, request); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale control version error = %v", err)
	}
	wrongOwner := request
	wrongOwner.OwnerID = otherOwner.ID
	wrongOwner.ExpectedControlVersion = 7
	if _, err := store.BeginControlTransition(ctx, wrongOwner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong owner begin error = %v", err)
	}

	request.ExpectedControlVersion = 7
	transition, err := store.BeginControlTransition(ctx, request)
	if err != nil {
		t.Fatalf("begin release: %v", err)
	}
	if err := store.RecordControlTransitionFailure(ctx, hostValue.ID, request.RequestID, "runtime_unavailable"); err != nil {
		t.Fatalf("record transition failure: %v", err)
	}
	stored, err := store.GetControlTransition(ctx, hostValue.ID, request.RequestID)
	if err != nil || stored.FailureReason != "runtime_unavailable" || stored.Phase != transition.Phase {
		t.Fatalf("failed transition record = %#v, err=%v", stored, err)
	}
	storedHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || storedHost.ControlState != host.ControlReturning || storedHost.ControlVersion != transition.BlockedControlVersion {
		t.Fatalf("Host after recorded failure = %#v, err=%v", storedHost, err)
	}
	if _, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "release-again",
		Action: control.ActionRelease, ExpectedControlVersion: transition.BlockedControlVersion,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new transition while failed transition is active error = %v", err)
	}
}

func TestPostgresCompleteControlTransitionRequiresExactBlockedFence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)

	takeover, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "takeover-fence",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveControlTaskIDs(ctx, hostValue.ID, takeover.RequestID, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetControlTransitionPhase(ctx, hostValue.ID, takeover.RequestID, control.PhaseDraining); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, takeover.RequestID,
		json.RawMessage(`{"receipt_id":"receipt-fence"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_version=control_version+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteControlTransition(ctx, hostValue.ID, takeover.RequestID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("takeover with changed blocked version error = %v", err)
	}
	stillActive, err := store.GetActiveControlTransition(ctx, hostValue.ID)
	if err != nil || stillActive.Phase != control.PhaseDrained {
		t.Fatalf("takeover after fence mismatch = %#v, err=%v", stillActive, err)
	}

	// A separate Host exercises the release direction's exact transitional
	// state check while keeping the recorded blocked version unchanged.
	releaseHost := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 10)
	release, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: releaseHost.ID, OwnerID: owner.ID, RequestID: "release-fence",
		Action: control.ActionRelease, ExpectedControlVersion: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveControlTaskIDs(ctx, releaseHost.ID, release.RequestID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='ai' WHERE id=$1`, releaseHost.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteControlTransition(ctx, releaseHost.ID, release.RequestID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("release with changed transitional state error = %v", err)
	}
	stillActive, err = store.GetActiveControlTransition(ctx, releaseHost.ID)
	if err != nil || stillActive.Phase != control.PhaseBlocking {
		t.Fatalf("release after fence mismatch = %#v, err=%v", stillActive, err)
	}
}

func TestPostgresControlTransitionConcurrentBeginIsSerialized(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			_, err := store.BeginControlTransition(ctx, control.BeginRequest{
				HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "takeover-race-" + string(rune('a'+i)),
				Action: control.ActionTakeOver, ExpectedControlVersion: 0,
			})
			results <- err
		}(i)
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeInvalidState {
			conflicts++
		} else {
			t.Fatalf("concurrent begin error = %v", err)
		}
	}
	if successes != 1 || conflicts != callers-1 {
		t.Fatalf("concurrent begin results: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestPostgresControlTransitionValidationAndMissingReads(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)

	badRequests := []control.BeginRequest{
		{HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "bad id", Action: control.ActionTakeOver},
		{HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "too-high", Action: control.ActionTakeOver, ExpectedControlVersion: int64(^uint64(0) >> 1)},
	}
	for _, request := range badRequests {
		if _, err := store.BeginControlTransition(ctx, request); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid begin request %#v error = %v", request, err)
		}
	}
	if _, err := store.GetControlTransition(ctx, hostValue.ID, "missing"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing transition error = %v", err)
	}
	if _, err := store.GetActiveControlTransition(ctx, hostValue.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing active transition error = %v", err)
	}
	if _, err := store.SaveControlDrainedReceipt(ctx, hostValue.ID, "missing", json.RawMessage(`{"receipt_id":"x"}`)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing receipt transition error = %v", err)
	}
}

func createRunningControlHost(t *testing.T, ctx context.Context, store *Store, ownerID string, controlState host.ControlState, controlVersion int64) host.Host {
	t.Helper()
	value := createTestHost(t, ctx, store, ownerID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running', control_state=$2, control_version=$3 WHERE id=$1`,
		value.ID, string(controlState), controlVersion); err != nil {
		t.Fatalf("prepare running control Host: %v", err)
	}
	return value
}
