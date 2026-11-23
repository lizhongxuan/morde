package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestNestedDelegateAdmissionFindsOnlyTheActorsOwnDurableChild(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "nested-admission", false)
	children, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	root := fixture.fence.AgentSessionID
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, root)
	create := func(parent, key string) app.AgentChildSession {
		t.Helper()
		child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
			ParentSessionID: parent, SpawnKey: key, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"objective":"read"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}})
		if err != nil || replayed {
			t.Fatalf("create child of %q: replayed=%t err=%v", parent, replayed, err)
		}
		return child
	}
	first := create(root, "parent_first")
	other := create(root, "parent_other")
	_ = create(first.SessionID, "shared_call")
	message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
		fixture.task.ID, "nested-admission-steer", "Change the next action.")
	admit := func(actor, callID string, want app.AgentToolCallAdmissionStatus) {
		t.Helper()
		childFence := fixture.modelFence
		childFence.AgentSessionID = actor
		childFence.AgentRootSessionID = root
		admission, err := fixture.route.store.AdmitAgentToolCall(ctx, app.ModelToolCall{
			CallID: callID, Name: "dsh_delegate", Arguments: `{"objective":"read"}`, TaskFence: &childFence,
		}, false)
		if err != nil || admission.Status != want {
			t.Fatalf("admission actor=%q call=%q: admission=%+v err=%v want=%s", actor, callID, admission, err, want)
		}
		if want == app.AgentToolCallSuperseded && (len(admission.PendingMessages) != 1 || admission.PendingMessages[0].ID != message.ID) {
			t.Fatalf("supersession did not preserve the pending steer: %+v", admission.PendingMessages)
		}
	}
	admit(first.SessionID, app.AgentSessionToolCallID(fixture.task.ID, first.SessionID, "shared_call"), app.AgentToolCallAdmitted)
	admit(other.SessionID, app.AgentSessionToolCallID(fixture.task.ID, other.SessionID, "shared_call"), app.AgentToolCallSuperseded)
	admit(first.SessionID, "shared_call", app.AgentToolCallSuperseded)
}

func TestNestedCancellationFencesAdmissionAndFinalDispatch(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "nested-cancellation-fence", true)
	children, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	root := fixture.fence.AgentSessionID
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, root)
	create := func(parent, key string, background bool) app.AgentChildSession {
		t.Helper()
		child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
			ParentSessionID: parent, SpawnKey: key, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"objective":"read"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}, Background: background})
		if err != nil || replayed {
			t.Fatalf("create child of %q: replayed=%t err=%v", parent, replayed, err)
		}
		return child
	}
	coordinator := create(root, "cancel_coordinator", true)
	sibling := create(root, "independent_coordinator", true)
	worker := create(coordinator.SessionID, "nested_existing", false)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(fixture.route.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"started"}]}`, worker.SessionID))
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{
		TaskID: fixture.task.ID, OwnerID: fixture.task.OwnerID, InitiatorID: fixture.fence.InitiatorID,
		AgentInstanceID: fixture.fence.AgentInstanceID, AgentGeneration: fixture.fence.AgentGeneration,
		AgentSessionID: worker.SessionID, AgentRootSessionID: root, AuthorizationVersion: fixture.fence.AuthorizationVersion,
		LeaseOwner: fixture.fence.LeaseOwner, LeaseVersion: fixture.fence.LeaseVersion,
		EngineVersion: fixture.fence.EngineVersion, ProfileVersion: fixture.fence.ProfileVersion},
		From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("save worker checkpoint: %v", err)
	}
	workerFence := s03ChildToolFence(fixture, worker, 0)
	callID := app.AgentSessionToolCallID(fixture.task.ID, worker.SessionID, "prepared_read")
	call := app.ModelToolCall{CallID: callID, Name: "workspace_read", Arguments: `{"path":"report.md"}`, TaskFence: &workerFence}
	admitted, err := fixture.route.store.AdmitAgentToolCall(ctx, call, false)
	if err != nil || admitted.Status != app.AgentToolCallAdmitted {
		t.Fatalf("admit tool before cancellation: admission=%+v err=%v", admitted, err)
	}
	operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: fixture.task.ID,
		Key: app.AgentOperationKey(callID), RequestHash: hashBytes("nested prepared read"), HostID: fixture.host.ID,
		RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
		ControlVersion: fixture.host.ControlVersion, LeaseVersion: fixture.task.Lease.Version,
		RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationPrepared}
	if _, replayed, err := fixture.route.store.CreateOperation(ctx, operation, fixture.task.Lease.Owner); err != nil || replayed {
		t.Fatalf("prepare tool before cancellation: replayed=%t err=%v", replayed, err)
	}
	_, _, err = children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
		ActorSessionID: root, SessionID: coordinator.SessionID, CallID: "cancel_coordinator"})
	if err != nil {
		t.Fatalf("cancel coordinator: %v", err)
	}
	_, err = fixture.route.store.UpdateOperation(ctx, app.OperationUpdate{TaskID: fixture.task.ID,
		OperationKey: operation.Key, LeaseOwner: fixture.task.Lease.Owner, LeaseVersion: fixture.task.Lease.Version,
		RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
		ControlVersion: fixture.host.ControlVersion, RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("prepared tool dispatched after ancestor cancellation: %v", err)
	}
	stored, err := fixture.route.store.GetOperation(ctx, fixture.task.OwnerID, fixture.task.ID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("cancelled tool's ledger changed after blocked dispatch: operation=%+v err=%v", stored, err)
	}
	if _, err := fixture.route.store.AdmitAgentToolCall(ctx, call, true); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("prepared but undispatched operation was resumed after cancellation: %v", err)
	}
	newCall := call
	newCall.CallID = app.AgentSessionToolCallID(fixture.task.ID, worker.SessionID, "new_after_cancel")
	if _, err := fixture.route.store.AdmitAgentToolCall(ctx, newCall, false); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new tool was admitted after ancestor cancellation: %v", err)
	}
	coordinatorFence := s03ChildToolFence(fixture, coordinator, 0)
	existingSpawn := app.ModelToolCall{CallID: app.AgentSessionToolCallID(fixture.task.ID, coordinator.SessionID, "nested_existing"),
		Name: "dsh_delegate", Arguments: `{"objective":"read"}`, TaskFence: &coordinatorFence}
	if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, existingSpawn, true); err != nil || admission.Status != app.AgentToolCallAdmitted {
		t.Fatalf("existing delegated child could not be reconciled after cancellation: admission=%+v err=%v", admission, err)
	}
	existingSpawn.Arguments = `{"objective":"changed"}`
	if _, err := fixture.route.store.AdmitAgentToolCall(ctx, existingSpawn, true); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("cancelled delegated call recovered with different arguments: %v", err)
	}
	siblingFence := s03ChildToolFence(fixture, sibling, 0)
	siblingCall := app.ModelToolCall{CallID: app.AgentSessionToolCallID(fixture.task.ID, sibling.SessionID, "independent_read"),
		Name: "workspace_read", Arguments: `{"path":"other.md"}`, TaskFence: &siblingFence}
	if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, siblingCall, false); err != nil || admission.Status != app.AgentToolCallAdmitted {
		t.Fatalf("sibling was affected by cancellation: admission=%+v err=%v", admission, err)
	}
	if _, err := children.Get(ctx, fixture.fence, coordinator.SessionID, worker.SessionID); err != nil {
		t.Fatalf("cancelled coordinator could not inspect its worker during cleanup: %v", err)
	}
	settlement := app.AgentChildSessionSettleCommand{Fence: fixture.fence, ActorSessionID: coordinator.SessionID,
		SessionID: worker.SessionID, Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: digest[:], Summary: "finished"}
	if _, _, err := children.Settle(ctx, settlement); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("worker succeeded after ancestor cancellation: %v", err)
	}
	settlement.Status, settlement.Summary = task.StepFailed, "cancelled after stop confirmation"
	if child, replayed, err := children.Settle(ctx, settlement); err != nil || replayed || child.Step.Status != task.StepFailed {
		t.Fatalf("cancelled worker could not settle for cleanup: child=%+v replayed=%t err=%v", child, replayed, err)
	}
}

func TestNestedCancellationRacesFinalDispatch(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "nested-cancellation-race", true)
	children, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	root := fixture.fence.AgentSessionID
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, root)
	create := func(parent, key string, background bool) app.AgentChildSession {
		t.Helper()
		child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
			ParentSessionID: parent, SpawnKey: key, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"objective":"read"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}, Background: background})
		if err != nil || replayed {
			t.Fatalf("create race child: replayed=%t err=%v", replayed, err)
		}
		return child
	}
	sibling := create(root, "race_sibling", true)
	for index := 0; index < 4; index++ {
		coordinator := create(root, fmt.Sprintf("race_parent_%d", index), true)
		worker := create(coordinator.SessionID, fmt.Sprintf("race_worker_%d", index), false)
		workerFence := s03ChildToolFence(fixture, worker, 0)
		callID := app.AgentSessionToolCallID(fixture.task.ID, worker.SessionID, fmt.Sprintf("race_call_%d", index))
		call := app.ModelToolCall{CallID: callID, Name: "workspace_read", Arguments: `{"path":"report.md"}`, TaskFence: &workerFence}
		if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, call, false); err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("admit race operation: admission=%+v err=%v", admission, err)
		}
		operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: fixture.task.ID,
			Key: app.AgentOperationKey(callID), RequestHash: hashBytes(fmt.Sprintf("race read %d", index)), HostID: fixture.host.ID,
			RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
			ControlVersion: fixture.host.ControlVersion, LeaseVersion: fixture.task.Lease.Version,
			RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationPrepared}
		if _, replayed, err := fixture.route.store.CreateOperation(ctx, operation, fixture.task.Lease.Owner); err != nil || replayed {
			t.Fatalf("prepare race operation: replayed=%t err=%v", replayed, err)
		}
		start := make(chan struct{})
		cancelResult, dispatchResult := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, _, err := children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
				ActorSessionID: root, SessionID: coordinator.SessionID, CallID: fmt.Sprintf("race_cancel_%d", index)})
			cancelResult <- err
		}()
		go func() {
			<-start
			_, err := fixture.route.store.UpdateOperation(ctx, app.OperationUpdate{TaskID: fixture.task.ID,
				OperationKey: operation.Key, LeaseOwner: fixture.task.Lease.Owner, LeaseVersion: fixture.task.Lease.Version,
				RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
				ControlVersion: fixture.host.ControlVersion, RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched})
			dispatchResult <- err
		}()
		close(start)
		cancelErr, dispatchErr := <-cancelResult, <-dispatchResult
		if cancelErr != nil {
			t.Fatalf("race cancellation failed: %v", cancelErr)
		}
		stored, err := fixture.route.store.GetOperation(ctx, fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil {
			t.Fatalf("read race operation: %v", err)
		}
		if dispatchErr == nil {
			if stored.Status != task.OperationDispatched {
				t.Fatalf("dispatch won but operation status = %s", stored.Status)
			}
			if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, call, true); err != nil || admission.Status != app.AgentToolCallAdmitted {
				t.Fatalf("already-dispatched race operation could not reconcile: admission=%+v err=%v", admission, err)
			}
		} else {
			if domain.CodeOf(dispatchErr) != domain.CodeInvalidState || stored.Status != task.OperationPrepared {
				t.Fatalf("cancellation won but dispatch changed ledger: operation=%+v err=%v", stored, dispatchErr)
			}
			if _, err := fixture.route.store.AdmitAgentToolCall(ctx, call, true); domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("undispatched race operation resumed after cancellation: %v", err)
			}
		}
		call.CallID = app.AgentSessionToolCallID(fixture.task.ID, worker.SessionID, fmt.Sprintf("race_new_%d", index))
		if _, err := fixture.route.store.AdmitAgentToolCall(ctx, call, false); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("new call was admitted after race cancellation: %v", err)
		}
	}
	siblingFence := s03ChildToolFence(fixture, sibling, 0)
	siblingCall := app.ModelToolCall{CallID: app.AgentSessionToolCallID(fixture.task.ID, sibling.SessionID, "race_sibling_call"),
		Name: "workspace_read", Arguments: `{"path":"other.md"}`, TaskFence: &siblingFence}
	if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, siblingCall, false); err != nil || admission.Status != app.AgentToolCallAdmitted {
		t.Fatalf("independent branch was affected by cancellation races: admission=%+v err=%v", admission, err)
	}
}
