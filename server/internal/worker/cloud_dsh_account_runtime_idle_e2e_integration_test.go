package worker

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestAccountAgentRuntimeIdleRenewalPersistsBeyondOriginalExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &accountRuntimeSupervisorStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-idle-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()+interval '5 seconds'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`,
		session.runtime.Fence.ServiceInstanceID, owner.ID, session.runtime.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	session.renewMu.Lock()
	session.leaseExpiresAt = time.Now().Add(5 * time.Second)
	session.renewMu.Unlock()
	if err := manager.renewIdleSessions(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || persisted.State != app.AccountAgentRuntimeRunning ||
		persisted.LeaseExpiresAt.Before(time.Now().Add(20*time.Second)) || persisted.Fence != session.runtime.Fence {
		t.Fatalf("idle Runtime was not kept alive in PostgreSQL: runtime=%+v found=%t err=%v", persisted, found, err)
	}
	candidates, err := control.ListAccountAgentRuntimeStopCandidates(ctx, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("renewed idle Runtime was selected for cleanup: candidates=%+v err=%v", candidates, err)
	}
	supervisor.mu.Lock()
	renewCalls := supervisor.renewCalls
	supervisor.mu.Unlock()
	if renewCalls != 1 || session.closed() {
		t.Fatalf("Guardian renewal count=%d, session closed=%t", renewCalls, session.closed())
	}
}
