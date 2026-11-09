package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestTaskSandboxLifecyclePersistsFencedTransitions(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	claimed, _ := newTaskSandboxCallFixture(t, ctx, store, owner.ID, "sandbox-lifecycle-transitions")
	lifecycle := lifecycleForTask(t, claimed)

	prepared, replayed, err := store.PrepareTaskSandboxLifecycle(ctx, lifecycle)
	if err != nil || replayed || prepared.Status != app.TaskSandboxPrepared || prepared.SandboxGeneration != 1 || prepared.NonceSHA256 != "" {
		t.Fatalf("prepare lifecycle = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	replayedValue, replayed, err := store.PrepareTaskSandboxLifecycle(ctx, lifecycle)
	if err != nil || !replayed || replayedValue.ID != prepared.ID || replayedValue.SandboxGeneration != prepared.SandboxGeneration {
		t.Fatalf("prepare replay = %#v replayed=%t err=%v", replayedValue, replayed, err)
	}

	creating, err := store.MarkTaskSandboxLifecycleCreating(ctx, prepared)
	if err != nil || creating.Status != app.TaskSandboxCreating {
		t.Fatalf("mark creating = %#v err=%v", creating, err)
	}
	bound, err := store.BindTaskSandboxLifecycle(ctx, creating, "sandbox-local-test", time.Now().Add(time.Hour))
	if err != nil || bound.Status != app.TaskSandboxRunning || bound.SandboxID != "sandbox-local-test" || bound.ExpiresAt == nil {
		t.Fatalf("bind lifecycle = %#v err=%v", bound, err)
	}
	stopping, err := store.RequestTaskSandboxStop(ctx, bound)
	if err != nil || stopping.Status != app.TaskSandboxStopping {
		t.Fatalf("request stop = %#v err=%v", stopping, err)
	}
	nonce, nonceSHA256, err := app.NewTaskSandboxGuardianNonce()
	if err != nil {
		t.Fatal(err)
	}
	clear(nonce)
	challenged, err := store.SetTaskSandboxGuardianStopChallenge(ctx, stopping, nonceSHA256)
	if err != nil || challenged.NonceSHA256 != nonceSHA256 {
		t.Fatalf("persist Guardian stop challenge = %#v err=%v", challenged, err)
	}
	rotatedNonce, rotatedNonceSHA256, err := app.NewTaskSandboxGuardianNonce()
	if err != nil {
		t.Fatal(err)
	}
	clear(rotatedNonce)
	rotatedChallenge, err := store.SetTaskSandboxGuardianStopChallenge(ctx, challenged, rotatedNonceSHA256)
	if err != nil || rotatedChallenge.NonceSHA256 != rotatedNonceSHA256 {
		t.Fatalf("rotate Guardian stop challenge = %#v err=%v", rotatedChallenge, err)
	}
	if _, err := store.TerminateTaskSandbox(ctx, challenged, []byte("stale local termination receipt")); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("old Guardian nonce terminated sandbox: %v", err)
	}
	terminated, err := store.TerminateTaskSandbox(ctx, rotatedChallenge, []byte("synthetic local termination receipt"))
	if err != nil || terminated.Status != app.TaskSandboxTerminated || terminated.TerminatedAt == nil {
		t.Fatalf("terminate lifecycle = %#v err=%v", terminated, err)
	}
	loaded, err := store.GetTaskSandboxLifecycle(ctx, owner.ID, claimed.ID, 1)
	if err != nil || loaded.ID != terminated.ID || loaded.Status != app.TaskSandboxTerminated {
		t.Fatalf("load terminated lifecycle = %#v err=%v", loaded, err)
	}

	second := lifecycle
	second.ID = mustID(t, domain.NewOperationID)
	preparedSecond, replayed, err := store.PrepareTaskSandboxLifecycle(ctx, second)
	if err != nil || replayed || preparedSecond.SandboxGeneration != 2 {
		t.Fatalf("prepare next generation = %#v replayed=%t err=%v", preparedSecond, replayed, err)
	}
	if preparedSecond.NonceSHA256 != "" {
		t.Fatalf("prepared lifecycle unexpectedly has stop challenge %q", preparedSecond.NonceSHA256)
	}
}

func TestTaskSandboxLifecycleUnknownIsNotReplayedAndStaleAgentCannotBind(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	claimed, _ := newTaskSandboxCallFixture(t, ctx, store, owner.ID, "sandbox-lifecycle-unknown")
	lifecycle := lifecycleForTask(t, claimed)
	prepared, _, err := store.PrepareTaskSandboxLifecycle(ctx, lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	creating, err := store.MarkTaskSandboxLifecycleCreating(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}

	// A newer Agent fence may observe the task, but it cannot bind a handle
	// created by the old generation or repeat the provider create operation.
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id='sandbox-agent-new',agent_generation=2 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindTaskSandboxLifecycle(ctx, creating, "must-not-bind", time.Now().Add(time.Hour)); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale Agent generation bind = %v", err)
	}
	current := creating
	current.AgentInstanceID, current.AgentGeneration = "sandbox-agent-new", 2
	if _, err := store.BindTaskSandboxLifecycle(ctx, current, "must-not-adopt", time.Now().Add(time.Hour)); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("new Agent generation adopted provider result = %v", err)
	}

	unknown, err := store.MarkTaskSandboxLifecycleUnknown(ctx, current, "provider_result_uncertain")
	if err != nil || unknown.Status != app.TaskSandboxUnknown {
		t.Fatalf("record unknown provider result = %#v err=%v", unknown, err)
	}
	currentPrepared := current
	currentPrepared.Status = app.TaskSandboxPrepared
	currentPrepared.SandboxID = ""
	currentPrepared.SandboxGeneration = 0
	currentPrepared.ExpiresAt = nil
	currentPrepared.UnknownCode = ""
	currentPrepared.TerminationEvidence = nil
	currentPrepared.TerminatedAt = nil
	if _, _, err := store.PrepareTaskSandboxLifecycle(ctx, currentPrepared); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("prepare while provider result is unknown = %v", err)
	}
	loaded, err := store.GetTaskSandboxLifecycle(ctx, owner.ID, claimed.ID, prepared.SandboxGeneration)
	if err != nil || loaded.Status != app.TaskSandboxUnknown || loaded.UnknownCode != "provider_result_uncertain" {
		t.Fatalf("load unresolved lifecycle = %#v err=%v", loaded, err)
	}
	resolved, err := store.TerminateTaskSandbox(ctx, current, []byte("local reconciliation confirmed no sandbox was created"))
	if err != nil || resolved.Status != app.TaskSandboxTerminated || len(resolved.TerminationEvidence) == 0 {
		t.Fatalf("resolve unknown lifecycle without replay = %#v err=%v", resolved, err)
	}
	currentPrepared.ID = mustID(t, domain.NewOperationID)
	next, replayed, err := store.PrepareTaskSandboxLifecycle(ctx, currentPrepared)
	if err != nil || replayed || next.SandboxGeneration != 2 {
		t.Fatalf("prepare generation after explicit reconciliation = %#v replayed=%t err=%v", next, replayed, err)
	}
}

func lifecycleForTask(t *testing.T, value task.Task) app.TaskSandboxLifecycle {
	t.Helper()
	return app.TaskSandboxLifecycle{
		ID: mustID(t, domain.NewOperationID), TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version,
		AuthorizationVersion: value.Authorization.Version, AgentInstanceID: value.AgentInstanceID,
		AgentGeneration: value.AgentGeneration, Status: app.TaskSandboxPrepared,
	}
}
