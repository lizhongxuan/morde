package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresExpiredAccountTaskStopRecoveryRequiresExactCurrentFences(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{
		OwnerID: f.request.Fence.OwnerID, ConversationID: runtimeTaskConversation(t, f.store, f.request.Fence.TaskID),
		RuntimeID: "task-stop-recovery-runtime", LeaseOwner: "task-stop-recovery-runtime-worker",
		SupervisorNonce: append([]byte(nil), f.request.SupervisorNonce...), MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatalf("mark account Runtime ready: %v", err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := app.NewAgentStopAuthenticator("supervisor-1", f.key)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardian, err := app.NewAgentGuardianStopAuthenticator("supervisor-1", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardian, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceServiceWithGuardianStops(f.store, supervisor, agentGuardian, taskGuardian)
	if err != nil {
		t.Fatal(err)
	}
	f.request.RuntimeFence = &runtime.Fence
	lease, replayed, err := service.Start(ctx, f.request)
	if err != nil || replayed {
		t.Fatalf("start Runtime-bound Task Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	stopID := mustID(t, domain.NewTaskActionID)
	if _, err := f.store.RequestTaskAction(ctx,
		taskActionIdempotency(f.request.Fence.OwnerID, task.ActionCancel, "recovery-stop-action", f.request.Fence.TaskID, task.StatusRunning, stopID),
		f.request.Fence.TaskID, task.ActionCancel, task.StatusRunning, nil, stopID); err != nil {
		t.Fatalf("request Task cancellation: %v", err)
	}
	claim, found, err := f.store.ClaimNextTaskStop(ctx, "task-stop-recovery-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != stopID || claim.Task.AgentGeneration != lease.Fence.AgentGeneration {
		t.Fatalf("claim pending stop request: claim=%+v found=%t err=%v", claim, found, err)
	}

	identity, ready, err := service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, claim)
	if err != nil || ready {
		t.Fatalf("recovered before safe_after and Agent lease expiry: ready=%t identity=%+v err=%v", ready, identity, err)
	}
	if _, _, err := service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, app.TaskStopClaim{
		Task: claim.Task, Request: func() task.StopRequest {
			value := claim.Request
			value.ID = mustID(t, domain.NewTaskActionID)
			return value
		}(),
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("mismatched pending stop request returned recovery identity: %v", err)
	}
	if _, _, err := service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, app.TaskStopClaim{
		Task: claim.Task, Request: func() task.StopRequest { value := claim.Request; value.Action = task.ActionPause; return value }(),
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched stop action was not rejected: %v", err)
	}
	staleGeneration := claim
	staleGeneration.Task.AgentGeneration++
	if _, _, err := service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, staleGeneration); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale current Agent generation returned a recovery identity: %v", err)
	}

	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT safe_after FROM task_stop_requests WHERE id=$1`, stopID).Scan(&claim.Request.SafeAfter); err != nil {
		t.Fatal(err)
	}
	identity, ready, err = service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, claim)
	if err != nil || ready {
		t.Fatalf("recovered while Portal Agent lease was active: ready=%t identity=%+v err=%v", ready, identity, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND agent_generation=$2 AND agent_instance_id=$3 AND state='active'`, lease.Fence.TaskID,
		lease.Fence.AgentGeneration, lease.Fence.AgentInstanceID); err != nil {
		t.Fatal(err)
	}
	identity, ready, err = service.ExpiredAccountAgentTaskStopRecoveryIdentity(ctx, claim)
	if err != nil || !ready || identity.TaskFence != lease.Fence || identity.RuntimeFence != runtime.Fence ||
		identity.NonceSHA256 == "" || !identity.AgentLeaseExpiresAt.Before(time.Now()) {
		t.Fatalf("exact expired current Agent recovery identity = %+v ready=%t err=%v", identity, ready, err)
	}
}
