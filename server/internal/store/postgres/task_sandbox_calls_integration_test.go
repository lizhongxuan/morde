package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestTaskSandboxCallReceiptReplayRecoveryAndUnknownFence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	claimed, call := newTaskSandboxCallFixture(t, ctx, store, owner.ID, "task-sandbox-call-receipt")
	wrongWorkspace := call
	wrongWorkspace.WorkspaceID = mustID(t, domain.NewWorkspaceID)
	if _, _, err := store.PrepareTaskSandboxCall(ctx, wrongWorkspace); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("call bound to a different Workspace = %v", err)
	}
	wrongProject := call
	wrongProject.ProjectID, wrongProject.ProjectResourceVersion = "j1234567", 1
	if _, _, err := store.PrepareTaskSandboxCall(ctx, wrongProject); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("unsupported project Task Sandbox call = %v", err)
	}

	prepared, replayed, err := store.PrepareTaskSandboxCall(ctx, call)
	if err != nil || replayed || prepared.ID != call.ID || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare sandbox call = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	retry := call
	retry.ID = mustID(t, domain.NewOperationID)
	preparedAgain, replayed, err := store.PrepareTaskSandboxCall(ctx, retry)
	if err != nil || !replayed || preparedAgain.ID != call.ID || preparedAgain.Status != task.OperationPrepared {
		t.Fatalf("same-call prepare replay = %#v replayed=%t err=%v", preparedAgain, replayed, err)
	}
	changed := retry
	changed.RequestHash = hashBytes("different sandbox arguments")
	if _, _, err := store.PrepareTaskSandboxCall(ctx, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same call ID with changed arguments = %v", err)
	}

	// A prepared call has not crossed the side-effect boundary, so a new Task
	// lease can adopt it without creating a second receipt.
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "recovered-sandbox-worker", time.Minute)
	if err != nil || !found || recovered.ID != claimed.ID || recovered.Lease.Version == call.LeaseVersion {
		t.Fatalf("recover Task lease = %#v found=%t err=%v", recovered, found, err)
	}
	recovered.AgentInstanceID, recovered.AgentGeneration, recovered.AgentSessionID = "sandbox-agent-2", 2, "sandbox-session-2"
	recovered.AgentEngineVersion, recovered.AgentProfileVersion = "dsh-test-engine", "morde-test-profile"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=$3,agent_session_id=$4,
agent_engine_version=$5,agent_profile_version=$6 WHERE id=$1`, recovered.ID, recovered.AgentInstanceID, recovered.AgentGeneration,
		recovered.AgentSessionID, recovered.AgentEngineVersion, recovered.AgentProfileVersion); err != nil {
		t.Fatalf("bind recovered Task Sandbox fixture to new Agent generation: %v", err)
	}
	if _, err := store.MarkTaskSandboxCallDispatched(ctx, prepared, "stale-sandbox", 1); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale lease dispatched prepared sandbox call = %v", err)
	}
	call.LeaseOwner, call.LeaseVersion = recovered.Lease.Owner, recovered.Lease.Version
	call.AgentInstanceID, call.AgentGeneration, call.AgentSessionID = recovered.AgentInstanceID, recovered.AgentGeneration, recovered.AgentSessionID
	call.AgentEngineVersion, call.AgentProfileVersion = recovered.AgentEngineVersion, recovered.AgentProfileVersion
	adopted, replayed, err := store.PrepareTaskSandboxCall(ctx, call)
	if err != nil || !replayed || adopted.ID != prepared.ID || adopted.LeaseVersion != recovered.Lease.Version ||
		adopted.AgentInstanceID != recovered.AgentInstanceID || adopted.AgentGeneration != recovered.AgentGeneration {
		t.Fatalf("adopt prepared sandbox call = %#v replayed=%t err=%v", adopted, replayed, err)
	}
	staleAgent := adopted
	staleAgent.AgentInstanceID, staleAgent.AgentGeneration, staleAgent.AgentSessionID = "sandbox-agent-1", 1, "sandbox-session-1"
	if _, err := store.MarkTaskSandboxCallDispatched(ctx, staleAgent, "stale-agent-sandbox", 1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("recovered old Agent generation dispatched Task Sandbox call = %v", err)
	}

	dispatched, err := store.MarkTaskSandboxCallDispatched(ctx, adopted, "sandbox-test-01", 1)
	if err != nil || dispatched.Status != task.OperationDispatched || dispatched.SandboxID != "sandbox-test-01" {
		t.Fatalf("dispatch sandbox call = %#v err=%v", dispatched, err)
	}
	if _, err := store.MarkTaskSandboxCallDispatched(ctx, adopted, "sandbox-test-01", 1); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate sandbox dispatch = %v", err)
	}
	if _, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, strings.Repeat("x", maxTaskSandboxCallOutputBytes+1)); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("oversized sandbox output = %v", err)
	}
	if _, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, string([]byte{0xff})); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("non-UTF-8 sandbox output = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id='sandbox-agent-3',agent_generation=3,
agent_session_id='sandbox-session-3' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatalf("advance Task Agent fence while sandbox call is in flight: %v", err)
	}
	if _, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, "late stale result"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale Agent completed a dispatched Task Sandbox call = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=$3,agent_session_id=$4,
agent_engine_version=$5,agent_profile_version=$6 WHERE id=$1`, claimed.ID, call.AgentInstanceID, call.AgentGeneration,
		call.AgentSessionID, call.AgentEngineVersion, call.AgentProfileVersion); err != nil {
		t.Fatalf("restore current Task Agent fence: %v", err)
	}
	completed, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, "synthetic result")
	if err != nil || completed.Status != task.OperationSucceeded || completed.ExitCode == nil || *completed.ExitCode != 0 {
		t.Fatalf("complete sandbox call = %#v err=%v", completed, err)
	}
	completedAgain, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, "synthetic result")
	if err != nil || completedAgain.ID != completed.ID || completedAgain.ResultSHA256 == nil {
		t.Fatalf("same-result completion replay = %#v err=%v", completedAgain, err)
	}
	if _, err := store.CompleteTaskSandboxCall(ctx, dispatched, task.OperationSucceeded, 0, "different result"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("same call with changed result = %v", err)
	}
	loaded, err := store.GetTaskSandboxCall(ctx, owner.ID, claimed.ID, call.CallID)
	if err != nil || loaded.Output != "synthetic result" || loaded.Status != task.OperationSucceeded {
		t.Fatalf("read completed sandbox call = %#v err=%v", loaded, err)
	}
	other := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	if _, err := store.GetTaskSandboxCall(ctx, other.ID, claimed.ID, call.CallID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner sandbox call read = %v", err)
	}

	unknownCall := call
	unknownCall.ID = mustID(t, domain.NewOperationID)
	unknownCall.CallID = "tool-call-uncertain"
	unknownCall.RequestHash = hashBytes("uncertain sandbox arguments")
	unknownPrepared, replayed, err := store.PrepareTaskSandboxCall(ctx, unknownCall)
	if err != nil || replayed {
		t.Fatalf("prepare uncertain call = %#v replayed=%t err=%v", unknownPrepared, replayed, err)
	}
	unknownDispatched, err := store.MarkTaskSandboxCallDispatched(ctx, unknownPrepared, "sandbox-test-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskSandboxCallUnknown(ctx, unknownDispatched, "dispatch_result_unknown"); err != nil {
		t.Fatalf("mark unknown sandbox call: %v", err)
	}
	if err := store.MarkTaskSandboxCallUnknown(ctx, unknownDispatched, "dispatch_result_unknown"); err != nil {
		t.Fatalf("replay unknown mark: %v", err)
	}
	if _, _, err := store.PrepareTaskSandboxCall(ctx, unknownCall); err != nil {
		t.Fatalf("read unknown call without redispatch: %v", err)
	}
	newCall := unknownCall
	newCall.ID = mustID(t, domain.NewOperationID)
	newCall.CallID = "tool-call-after-unknown"
	newCall.RequestHash = hashBytes("new call while old outcome is unknown")
	if _, _, err := store.PrepareTaskSandboxCall(ctx, newCall); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("new sandbox call while prior result is unknown = %v", err)
	}
}

func TestTaskSandboxCallConcurrentPrepareUsesOneReceipt(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	_, call := newTaskSandboxCallFixture(t, ctx, store, owner.ID, "task-sandbox-concurrent-prepare")
	secondCall := call
	secondCall.ID = mustID(t, domain.NewOperationID)

	start := make(chan struct{})
	results := make(chan struct {
		value    app.TaskSandboxCall
		replayed bool
		err      error
	}, 2)
	var workers sync.WaitGroup
	for _, value := range []app.TaskSandboxCall{call, secondCall} {
		workers.Add(1)
		go func(value app.TaskSandboxCall) {
			defer workers.Done()
			<-start
			prepared, replayed, err := store.PrepareTaskSandboxCall(ctx, value)
			results <- struct {
				value    app.TaskSandboxCall
				replayed bool
				err      error
			}{value: prepared, replayed: replayed, err: err}
		}(value)
	}
	close(start)
	workers.Wait()
	close(results)
	var firstID string
	var fresh, replay int
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent sandbox prepare: %v", result.err)
		}
		if firstID == "" {
			firstID = result.value.ID
		} else if result.value.ID != firstID {
			t.Fatalf("concurrent prepares created multiple receipts: %q and %q", firstID, result.value.ID)
		}
		if result.replayed {
			replay++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replay != 1 {
		t.Fatalf("concurrent prepare fresh=%d replay=%d; want one of each", fresh, replay)
	}
}

func newTaskSandboxCallFixture(t *testing.T, ctx context.Context, store *Store, ownerID, idempotencyKey string) (task.Task, app.TaskSandboxCall) {
	t.Helper()
	workspaceValue := createTestWorkspace(t, ctx, store, ownerID, "task-sandbox-"+idempotencyKey)
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, ownerID, "", idempotencyKey, "run a sandbox task")
	submission.Task.InitiatorID = ownerID
	submission.Task.ExecutionLocation = "server"
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("task-context:" + idempotencyKey)
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Hostless Task for sandbox receipt: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "sandbox-receipt-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Hostless Task for sandbox receipt = %#v found=%t err=%v", claimed, found, err)
	}
	claimed.AgentInstanceID, claimed.AgentGeneration, claimed.AgentSessionID = "sandbox-agent-1", 1, "sandbox-session-1"
	claimed.AgentEngineVersion, claimed.AgentProfileVersion = "dsh-test-engine", "morde-test-profile"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=$3,agent_session_id=$4,
agent_engine_version=$5,agent_profile_version=$6 WHERE id=$1`, claimed.ID, claimed.AgentInstanceID, claimed.AgentGeneration,
		claimed.AgentSessionID, claimed.AgentEngineVersion, claimed.AgentProfileVersion); err != nil {
		t.Fatalf("bind Task Sandbox fixture to Agent generation: %v", err)
	}
	if claimed.InputRevision == nil || claimed.WorkspaceCheckpointRevision == nil {
		t.Fatalf("claimed Task has no Workspace snapshot: %#v", claimed)
	}
	contextDigest := sha256.Sum256([]byte("task-context:" + idempotencyKey))
	return claimed, app.TaskSandboxCall{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		WorkspaceID: claimed.WorkspaceID, WorkspaceOwnerID: ownerID, CallID: "tool-call-001",
		LeaseOwner: claimed.Lease.Owner, InputRevision: *claimed.InputRevision, WorkspaceCheckpoint: *claimed.WorkspaceCheckpointRevision,
		AuthorizationVersion: claimed.Authorization.Version, AgentInstanceID: claimed.AgentInstanceID,
		AgentGeneration: claimed.AgentGeneration, AgentSessionID: claimed.AgentSessionID,
		AgentEngineVersion: claimed.AgentEngineVersion, AgentProfileVersion: claimed.AgentProfileVersion,
		LeaseVersion:  claimed.Lease.Version,
		ContextDigest: contextDigest[:], RequestHash: hashBytes("sandbox command args"), Status: task.OperationPrepared,
	}
}
