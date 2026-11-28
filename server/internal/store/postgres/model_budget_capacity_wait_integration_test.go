package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func modelBudgetCapacityWaitFixture(t *testing.T, accountLimit int64, maxConcurrent int32) (routeActionsTestFixture, app.ModelTaskFence, app.TaskModelBudgetCapacityWaitRequest) {
	t.Helper()
	f, _, _, fence, _ := revisionGatewayFixture(t)
	ctx := context.Background()
	fence.AgentRootSessionID = fence.AgentSessionID
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET owner_id=NULL,updated_at=clock_timestamp()
WHERE id='p1234567'`); err != nil {
		t.Fatalf("make test model route shared: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,$2,$3) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`,
		f.ownerID, accountLimit, maxConcurrent); err != nil {
		t.Fatalf("configure platform model budget: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=0 WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatalf("initialize Task model usage: %v", err)
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("capacity-wait:" + fence.TaskID))
	return f, fence, app.TaskModelBudgetCapacityWaitRequest{
		Fence: fence, ModelID: "p1234567", DSHCallID: callID, LedgerCallID: callID,
		BudgetCallID: "capacity-budget-" + fence.TaskID[1:], RequestDigest: hex.EncodeToString(digest[:]),
		InputTokenReservation: 50, MinimumOutputTokens: 25,
	}
}

func createSecondModelBudgetCapacityWaitTask(t *testing.T, f routeActionsTestFixture) (app.ModelTaskFence, app.TaskModelBudgetCapacityWaitRequest) {
	t.Helper()
	ctx := context.Background()
	conversation := createMainConversationForTest(t, f.store, f.ownerID)
	claim := f.receiveInConversation(t, conversation.ID, "capacity-wait-second-task", "a smaller model request",
		app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-wait-second-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim second capacity-wait Task: id=%q found=%t err=%v", claimed.ID, found, err)
	}
	digest := sha256.Sum256([]byte("second capacity-wait checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000,usage_model_tokens=0,agent_instance_id='agent-capacity-2',
agent_generation=1,agent_session_id='session-capacity-2',agent_engine_version='engine',agent_profile_version='profile',
checkpoint_through_seq=0,checkpoint_digest=$2 WHERE id=$1`, taskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-capacity-2',0,$3)`, taskID, f.ownerID, digest[:]); err != nil {
		t.Fatal(err)
	}
	initialNonceDigest := sha256.Sum256([]byte("capacity-wait-agent-nonce:" + taskID))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-capacity-2',1,'session-capacity-2','engine','profile','revision-supervisor',$3,$4,$5,1,30000,
clock_timestamp()+interval '30 seconds','active')`, taskID, f.ownerID, initialNonceDigest[:], claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,'agent-capacity-2',1,'session-capacity-2','session-capacity-2',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, f.ownerID, digest[:], claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: f.ownerID, InitiatorID: f.ownerID,
		TaskRevision: *claimed.CurrentRevision, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: "agent-capacity-2", AgentGeneration: 1, AgentSessionID: "session-capacity-2",
		AgentRootSessionID: "session-capacity-2", AuthorizationVersion: 1, ThroughSequence: 0,
		EngineVersion: "engine", ProfileVersion: "profile"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := sha256.Sum256([]byte("small-capacity-wait:" + taskID))
	request := app.TaskModelBudgetCapacityWaitRequest{Fence: fence, ModelID: "p1234567", DSHCallID: callID, LedgerCallID: callID,
		BudgetCallID: "capacity-budget-" + taskID[1:], RequestDigest: hex.EncodeToString(requestDigest[:]),
		InputTokenReservation: 25, MinimumOutputTokens: 25}
	return fence, request
}

func confirmCapacityWaitAgentStopped(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence, nonceByte byte) {
	t.Helper()
	confirmCapacityWaitAgentStoppedWithSupervisor(t, f, fence, nonceByte, "revision-supervisor")
}

func confirmCapacityWaitAgentStoppedWithSupervisor(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence,
	nonceByte byte, supervisorID string) {
	t.Helper()
	nonce := make([]byte, 32)
	nonce[0] = nonceByte
	nonceHash := sha256.Sum256(nonce)
	if _, err := f.store.pool.Exec(context.Background(), `UPDATE task_agent_instances SET stop_nonce_digest=$3
WHERE task_id=$1 AND agent_generation=$2`, fence.TaskID, fence.AgentGeneration, nonceHash[:]); err != nil {
		t.Fatalf("set test Agent stop nonce digest: %v", err)
	}
	key := make([]byte, 32)
	key[0] = nonceByte + 1
	confirmTaskModelRecoveryAgentStop(t, f.store, key, app.AgentInstanceFence{
		TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentRootSessionID,
		SupervisorID: supervisorID, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion,
	}, nonce)
}

func startCapacityWaitAgentAfterClaim(t *testing.T, f routeActionsTestFixture, previous app.ModelTaskFence,
	claimed task.Task, agentID string, nonceByte byte) app.ModelTaskFence {
	t.Helper()
	if claimed.CurrentRevision == nil {
		t.Fatal("claimed capacity-wait Task has no current revision")
	}
	key := make([]byte, 32)
	key[0] = nonceByte + 1
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = nonceByte
	lease, replayed, err := instances.Start(context.Background(), app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: agentID, AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion,
	}, SupervisorNonce: nonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start Agent after capacity requeue: replayed=%t err=%v", replayed, err)
	}
	updated := previous
	updated.TaskRevision = *claimed.CurrentRevision
	updated.LeaseOwner, updated.LeaseVersion = claimed.Lease.Owner, claimed.Lease.Version
	updated.AgentInstanceID, updated.AgentGeneration = lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration
	updated.AgentSessionID, updated.AgentRootSessionID = lease.Fence.AgentSessionID, lease.Fence.AgentSessionID
	updated.AuthorizationVersion = lease.Fence.AuthorizationVersion
	updated.EngineVersion, updated.ProfileVersion = lease.Fence.EngineVersion, lease.Fence.ProfileVersion
	if _, err := f.store.pool.Exec(context.Background(), `UPDATE task_agent_checkpoints SET agent_instance_id=$3,agent_generation=$4,
engine_version=$5,profile_version=$6,auth_version=$7,lease_version=$8
WHERE task_id=$1 AND agent_session_id=$2 AND through_seq=$9`, updated.TaskID, updated.AgentSessionID,
		updated.AgentInstanceID, updated.AgentGeneration, updated.EngineVersion, updated.ProfileVersion,
		updated.AuthorizationVersion, updated.LeaseVersion, updated.ThroughSequence); err != nil {
		t.Fatalf("bind restored checkpoint to the current Agent generation: %v", err)
	}
	return updated
}

func insertCapacityTestReservation(t *testing.T, f routeActionsTestFixture, taskID string, tokens int64, state string) string {
	t.Helper()
	callID := fmt.Sprintf("capacity-hold-%s", taskID[1:])
	digest := sha256.Sum256([]byte(callID))
	var settled any
	var input, output any
	if state == "settled" {
		settled, input, output = time.Now().UTC(), tokens, int64(0)
	}
	_, err := f.store.pool.Exec(context.Background(), `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,settled_at,input_tokens,output_tokens)
VALUES ($1,'task',$2,$3,'p1234567','test',$4,$5,$6,$7,$8,$9)`, f.ownerID, callID, digest[:], taskID, max(int64(1), tokens), state, settled, input, output)
	if err != nil {
		t.Fatalf("insert capacity test model ledger row: %v", err)
	}
	return callID
}

func assertCapacityWaitTaskState(t *testing.T, f routeActionsTestFixture, taskID, wantStatus, wantReason string) {
	t.Helper()
	var status, reason, leaseOwner string
	var leaseExpires any
	if err := f.store.pool.QueryRow(context.Background(), `SELECT status,waiting_reason,lease_owner,lease_expires_at
FROM tasks WHERE id=$1`, taskID).Scan(&status, &reason, &leaseOwner, &leaseExpires); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || reason != wantReason || leaseOwner != "" || leaseExpires != nil {
		t.Fatalf("Task status/reason/lease = %q/%q/%q/%v, want %q/%q with cleared lease",
			status, reason, leaseOwner, leaseExpires, wantStatus, wantReason)
	}
}

func readCapacityCallDeadline(t *testing.T, f routeActionsTestFixture, ownerID, budgetCallID string) (time.Time, time.Time) {
	t.Helper()
	var firstWaitAt, deadlineAt time.Time
	if err := f.store.pool.QueryRow(context.Background(), `SELECT first_wait_at,deadline_at
FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2`, ownerID, budgetCallID).
		Scan(&firstWaitAt, &deadlineAt); err != nil {
		t.Fatalf("read model budget capacity call deadline: %v", err)
	}
	return firstWaitAt, deadlineAt
}

func assertCapacityDeadlineRows(t *testing.T, f routeActionsTestFixture, ownerID string, want int) {
	t.Helper()
	var count int
	if err := f.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_budget_capacity_call_deadlines WHERE owner_id=$1`, ownerID).
		Scan(&count); err != nil || count != want {
		t.Fatalf("model budget capacity call deadline rows=%d err=%v, want %d", count, err, want)
	}
}

func TestPostgresModelBudgetCapacityWaitKeepsDeadlineAcrossRewait(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist initial capacity wait: %v", err)
	}
	firstWaitAt, deadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	confirmCapacityWaitAgentStopped(t, f, fence, 126)
	woken, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || woken.Requeued != 1 {
		t.Fatalf("requeue first capacity wait: %+v err=%v", woken, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-deadline-rewait-worker", time.Minute)
	if err != nil || !found || claimed.ID != fence.TaskID {
		t.Fatalf("claim requeued Task: id=%q found=%t err=%v", claimed.ID, found, err)
	}
	request.Fence = startCapacityWaitAgentAfterClaim(t, f, fence, claimed, "agent-capacity-rewait", 127)
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist same BudgetCallID capacity re-wait: %v", err)
	}
	gotFirstWaitAt, gotDeadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	if !gotFirstWaitAt.Equal(firstWaitAt) || !gotDeadlineAt.Equal(deadlineAt) {
		t.Fatalf("same-call re-wait extended deadline: first/deadline=%s/%s, want %s/%s",
			gotFirstWaitAt, gotDeadlineAt, firstWaitAt, deadlineAt)
	}
	var createdAt, expiresAt time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT created_at,expires_at FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).
		Scan(&createdAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if !createdAt.Equal(firstWaitAt) || !expiresAt.Equal(deadlineAt) {
		t.Fatalf("re-wait episode deadline=%s/%s, want original %s/%s", createdAt, expiresAt, firstWaitAt, deadlineAt)
	}
	var agentID string
	var generation, leaseVersion int64
	if err := f.store.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,lease_version
FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2`, f.ownerID, request.BudgetCallID).
		Scan(&agentID, &generation, &leaseVersion); err != nil {
		t.Fatal(err)
	}
	if agentID != request.Fence.AgentInstanceID || generation != request.Fence.AgentGeneration || leaseVersion != request.Fence.LeaseVersion {
		t.Fatalf("deadline Agent fence=%s/%d/%d, want current %s/%d/%d", agentID, generation, leaseVersion,
			request.Fence.AgentInstanceID, request.Fence.AgentGeneration, request.Fence.LeaseVersion)
	}
}

func TestPostgresModelBudgetCapacityWaitExpiredCallRequiresCurrentStopReceipt(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist initial capacity wait: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, fence, 128)
	woken, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || woken.Requeued != 1 {
		t.Fatalf("requeue first capacity wait: %+v err=%v", woken, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-deadline-expired-rewait-worker", time.Minute)
	if err != nil || !found || claimed.ID != fence.TaskID {
		t.Fatalf("claim requeued Task: id=%q found=%t err=%v", claimed.ID, found, err)
	}
	request.Fence = startCapacityWaitAgentAfterClaim(t, f, fence, claimed, "agent-capacity-expired-rewait", 129)
	if _, err := f.store.pool.Exec(ctx, `WITH expired AS (SELECT clock_timestamp() AS now)
UPDATE model_budget_capacity_call_deadlines deadline
SET first_wait_at=expired.now-interval '6 minutes', deadline_at=expired.now-interval '1 minute'
FROM expired WHERE owner_id=$1 AND budget_call_id=$2`, f.ownerID, request.BudgetCallID); err != nil {
		t.Fatalf("expire persistent BudgetCallID deadline: %v", err)
	}
	firstWaitAt, deadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	if !deadlineAt.Before(time.Now().UTC()) || !deadlineAt.Equal(firstWaitAt.Add(5*time.Minute)) {
		t.Fatalf("test deadline was not expired at the original five-minute boundary: %s/%s", firstWaitAt, deadlineAt)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist expired same-call re-wait: %v", err)
	}
	deferred, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || deferred.Deferred != 1 || deferred.Paused != 0 {
		t.Fatalf("expired call without current Agent stop receipt = %+v err=%v", deferred, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	confirmCapacityWaitAgentStopped(t, f, request.Fence, 130)
	paused, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || paused.Paused != 1 || paused.Deferred != 0 {
		t.Fatalf("expired call after current Agent stop receipt = %+v err=%v", paused, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), app.TaskModelBudgetCapacityExpiredReason)
	assertCapacityDeadlineRows(t, f, f.ownerID, 0)
}

func TestPostgresModelBudgetCapacityWaitDeadlineRejectsIdentityChanges(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist initial capacity wait: %v", err)
	}
	originalFirstWaitAt, originalDeadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	confirmCapacityWaitAgentStopped(t, f, fence, 131)
	woken, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || woken.Requeued != 1 {
		t.Fatalf("requeue first capacity wait: %+v err=%v", woken, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-deadline-conflict-worker", time.Minute)
	if err != nil || !found || claimed.ID != fence.TaskID {
		t.Fatalf("claim requeued Task: id=%q found=%t err=%v", claimed.ID, found, err)
	}
	request.Fence = startCapacityWaitAgentAfterClaim(t, f, fence, claimed, "agent-capacity-conflict", 132)
	changedDigest := sha256.Sum256([]byte("changed BudgetCallID request digest"))
	conflicting := request
	conflicting.RequestDigest = hex.EncodeToString(changedDigest[:])
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, conflicting); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed request digest error=%v, want idempotency conflict", err)
	}
	firstWaitAt, deadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	if !firstWaitAt.Equal(originalFirstWaitAt) || !deadlineAt.Equal(originalDeadlineAt) {
		t.Fatalf("digest conflict changed persistent deadline: %s/%s, want %s/%s", firstWaitAt, deadlineAt,
			originalFirstWaitAt, originalDeadlineAt)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist original request after conflict rollback: %v", err)
	}
	secondFence, secondRequest := createSecondModelBudgetCapacityWaitTask(t, f)
	secondRequest.BudgetCallID = request.BudgetCallID
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, secondRequest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different Task with same BudgetCallID error=%v, want idempotency conflict", err)
	}
	assertCapacityDeadlineRows(t, f, f.ownerID, 1)
	var status, leaseOwner string
	if err := f.store.pool.QueryRow(ctx, `SELECT status,lease_owner FROM tasks WHERE id=$1`, secondFence.TaskID).Scan(&status, &leaseOwner); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusRunning) || leaseOwner == "" {
		t.Fatalf("conflicting second Task was altered: status=%s lease=%q", status, leaseOwner)
	}
}

func TestPostgresModelBudgetCapacityWaitDeadlinesAreIndependentPerCall(t *testing.T) {
	f, _, firstRequest := modelBudgetCapacityWaitFixture(t, 2000, 5)
	_, secondRequest := createSecondModelBudgetCapacityWaitTask(t, f)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, firstRequest); err != nil {
		t.Fatalf("persist first call capacity wait: %v", err)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, secondRequest); err != nil {
		t.Fatalf("persist second call capacity wait: %v", err)
	}
	assertCapacityDeadlineRows(t, f, f.ownerID, 2)
	firstStart, firstDeadline := readCapacityCallDeadline(t, f, f.ownerID, firstRequest.BudgetCallID)
	secondStart, secondDeadline := readCapacityCallDeadline(t, f, f.ownerID, secondRequest.BudgetCallID)
	if !firstDeadline.Equal(firstStart.Add(5*time.Minute)) || !secondDeadline.Equal(secondStart.Add(5*time.Minute)) ||
		firstRequest.BudgetCallID == secondRequest.BudgetCallID {
		t.Fatalf("independent deadlines = %s/%s and %s/%s", firstStart, firstDeadline, secondStart, secondDeadline)
	}
	if _, err := f.store.pool.Exec(ctx, `WITH expired AS (SELECT clock_timestamp() AS now)
UPDATE model_budget_capacity_call_deadlines deadline
SET first_wait_at=expired.now-interval '6 minutes', deadline_at=expired.now-interval '1 minute'
FROM expired WHERE owner_id=$1 AND budget_call_id=$2`, f.ownerID, firstRequest.BudgetCallID); err != nil {
		t.Fatal(err)
	}
	stillCurrentStart, stillCurrentDeadline := readCapacityCallDeadline(t, f, f.ownerID, secondRequest.BudgetCallID)
	if !stillCurrentStart.Equal(secondStart) || !stillCurrentDeadline.Equal(secondDeadline) {
		t.Fatalf("expiring first call changed second call deadline: %s/%s, want %s/%s", stillCurrentStart,
			stillCurrentDeadline, secondStart, secondDeadline)
	}
}

func TestPostgresModelBudgetCapacityWaitConcurrentStoresCreateOneDeadline(t *testing.T) {
	f, _, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	stores := []*Store{f.store, New(f.store.pool)}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var group sync.WaitGroup
	for _, store := range stores {
		group.Add(1)
		go func(store *Store) {
			defer group.Done()
			<-start
			results <- store.WaitForTaskModelBudgetCapacity(context.Background(), request)
		}(store)
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent identical capacity wait: %v", err)
		}
	}
	assertCapacityDeadlineRows(t, f, f.ownerID, 1)
	var waitRows, waitEvents int
	if err := f.store.pool.QueryRow(context.Background(), `SELECT
(SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1),
(SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.status' AND payload->>'reason'='model_budget_capacity'
 AND payload->>'status'='waiting_user')`, request.Fence.TaskID).Scan(&waitRows, &waitEvents); err != nil {
		t.Fatal(err)
	}
	if waitRows != 1 || waitEvents != 1 {
		t.Fatalf("concurrent duplicate wait rows/events=%d/%d, want 1/1", waitRows, waitEvents)
	}
}

func TestPostgresModelBudgetCapacityWaitReplayPreservesMigratedEpisodeExpiry(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist capacity wait before rolling-migration replay: %v", err)
	}
	firstWaitAt, _ := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	var originalExpiry time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT expires_at FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).
		Scan(&originalExpiry); err != nil {
		t.Fatal(err)
	}
	// Model a 00269 waiter whose independent clock_timestamp() calls produced
	// an expiry a microsecond earlier than its five-minute upper bound.
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_budget_capacity_waits
SET expires_at=expires_at-interval '1 microsecond' WHERE task_id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	var migratedExpiry time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT expires_at FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).
		Scan(&migratedExpiry); err != nil {
		t.Fatal(err)
	}
	if !migratedExpiry.Before(originalExpiry) {
		t.Fatalf("legacy episode expiry was not made earlier for replay check: %s >= %s", migratedExpiry, originalExpiry)
	}
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2`,
		f.ownerID, request.BudgetCallID); err != nil {
		t.Fatalf("remove new-version marker before migration replay: %v", err)
	}
	provider, closeProvider, err := newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatalf("open migration provider for 00270 compatibility check: %v", err)
	}
	if _, err := provider.DownTo(ctx, 269); err != nil {
		closeProvider()
		t.Fatalf("roll back empty 00270 marker table to simulate an existing 00269 deployment: %v", err)
	}
	closeProvider()
	provider, closeProvider, err = newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatalf("reopen migration provider for 00270 compatibility check: %v", err)
	}
	if _, err := provider.UpTo(ctx, 270); err != nil {
		closeProvider()
		t.Fatalf("apply 00270 over the existing 00269 waiter: %v", err)
	}
	closeProvider()
	migratedFirstWaitAt, migratedDeadline := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	if !migratedFirstWaitAt.Equal(firstWaitAt) || !migratedDeadline.Equal(migratedExpiry) ||
		!migratedDeadline.After(migratedFirstWaitAt) || migratedDeadline.After(migratedFirstWaitAt.Add(5*time.Minute)) {
		t.Fatalf("00270 backfill identity/deadline=%s/%s, want %s/%s within five minutes",
			migratedFirstWaitAt, migratedDeadline, firstWaitAt, migratedExpiry)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("replay capacity wait with migrated episode expiry: %v", err)
	}
	var retainedExpiry time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT expires_at FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).
		Scan(&retainedExpiry); err != nil {
		t.Fatal(err)
	}
	gotFirstWaitAt, gotDeadlineAt := readCapacityCallDeadline(t, f, f.ownerID, request.BudgetCallID)
	if !retainedExpiry.Equal(migratedExpiry) || !gotFirstWaitAt.Equal(migratedFirstWaitAt) || !gotDeadlineAt.Equal(migratedDeadline) {
		t.Fatalf("rolling migration replay changed expiry/deadline: expiry=%s ledger=%s/%s; want %s and %s/%s",
			retainedExpiry, gotFirstWaitAt, gotDeadlineAt, migratedExpiry, migratedFirstWaitAt, migratedDeadline)
	}
}

func TestPostgresModelBudgetCapacityWaitPrepareClearsDeadlineOnFreshAndReplay(t *testing.T) {
	f, preparation := taskModelBudgetPreparationFixture(t)
	ctx := context.Background()
	call := preparation.Reservation
	fence := *call.Task
	dshCallID := call.DSHCallID
	if dshCallID == "" {
		dshCallID = call.CallID
	}
	var checkpointDigest []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&checkpointDigest); err != nil {
		t.Fatalf("read preparation checkpoint digest: %v", err)
	}
	insertMatchingDeadline := func() {
		t.Helper()
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_budget_capacity_call_deadlines
(owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
 model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,first_wait_at,deadline_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,COALESCE($18::text,''),$19,$20,$21,
 decode($22,'hex'),$23,clock.started_at,clock.started_at+interval '5 minutes'
FROM (SELECT clock_timestamp() AS started_at) clock`, call.OwnerID, fence.TaskID, fence.OwnerID, fence.TaskRevision,
			fence.AuthorizationVersion, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration,
			fence.AgentSessionID, fence.AgentRootSessionID, fence.EngineVersion, fence.ProfileVersion, fence.ThroughSequence,
			fence.RecoveryCallID, fence.RecoveryDSHCallID, call.ModelID, call.ProjectID, dshCallID, call.CallID,
			preparation.BudgetCallID, preparation.ProviderRequestDigest, checkpointDigest); err != nil {
			t.Fatalf("insert matching call deadline marker: %v", err)
		}
	}

	insertMatchingDeadline()
	assertCapacityDeadlineRows(t, f, f.ownerID, 1)
	created, err := f.store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare model budget with pending deadline marker: %v", err)
	}
	assertCapacityDeadlineRows(t, f, f.ownerID, 0)

	// Simulate a stale marker surviving an earlier process boundary and verify
	// that replay-existing preparation removes it in the same transaction.
	insertMatchingDeadline()
	assertCapacityDeadlineRows(t, f, f.ownerID, 1)
	replayed, err := f.store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("replay existing model budget preparation with pending deadline marker: %v", err)
	}
	if replayed.PlanID != created.PlanID || replayed.MaxOutputTokens != created.MaxOutputTokens {
		t.Fatalf("replayed preparation=%+v, want original result %+v", replayed, created)
	}
	assertCapacityDeadlineRows(t, f, f.ownerID, 0)
}

func TestPostgresModelBudgetCapacityWaitRequiresStopThenRequeuesAfterReservationRelease(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 100, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("replay identical capacity wait: %v", err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	var waitRows int
	var eventType string
	var payload []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).Scan(&waitRows); err != nil || waitRows != 1 {
		t.Fatalf("durable waiter rows=%d err=%v, want one", waitRows, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT event_type,payload FROM task_events WHERE task_id=$1 ORDER BY sequence DESC LIMIT 1`, fence.TaskID).
		Scan(&eventType, &payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil || eventType != "task.status" ||
		event["reason"] != app.TaskModelBudgetCapacityWaitReason || event["status"] != string(task.StatusWaitingUser) {
		t.Fatalf("waiting status event = %s %s err=%v", eventType, payload, err)
	}
	if event["message"] != "模型调用并发额度暂满，任务将在额度释放后自动继续。" ||
		event["waiting_reason"] != app.TaskModelBudgetCapacityWaitReason {
		t.Fatalf("capacity wait status event payload = %s", payload)
	}
	notifications, err := f.store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(notifications) != 0 {
		t.Fatalf("automatic capacity wait generated user-action notification events: %#v err=%v", notifications, err)
	}

	withoutReceipt, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || withoutReceipt.Deferred != 1 || withoutReceipt.Requeued != 0 {
		t.Fatalf("reconcile before Agent stop receipt = %+v err=%v", withoutReceipt, err)
	}
	confirmCapacityWaitAgentStopped(t, f, fence, 101)
	// A dispatched reservation consumes 30 of the 25-token slack above the
	// waiter's 75-token minimum; it must remain waiting until that hold clears.
	reservationID := insertCapacityTestReservation(t, f, fence.TaskID, 30, "dispatched")
	blocked, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || blocked.Deferred != 1 || blocked.Requeued != 0 {
		t.Fatalf("reconcile while reservation is active = %+v err=%v", blocked, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`, f.ownerID, reservationID); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Requeued != 1 || result.Paused != 0 {
		t.Fatalf("reconcile after stop receipt and capacity release = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusQueued), "")
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).Scan(&waitRows); err != nil || waitRows != 0 {
		t.Fatalf("waiter rows after requeue=%d err=%v", waitRows, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-requeue-worker", time.Minute)
	if err != nil || !found || claimed.ID != fence.TaskID {
		t.Fatalf("existing claim loop did not claim requeued Task: task=%q found=%t err=%v", claimed.ID, found, err)
	}
}

func TestPostgresModelBudgetCapacityWaitPausesOnlyAfterReceiptAtDeadline(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_budget_capacity_waits SET created_at=clock_timestamp()-interval '6 minutes',
	expires_at=clock_timestamp()-interval '2 minutes' WHERE task_id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	deferred, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || deferred.Deferred != 1 || deferred.Paused != 0 {
		t.Fatalf("expired waiter without verified stop receipt = %+v err=%v", deferred, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	confirmCapacityWaitAgentStopped(t, f, fence, 102)
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 {
		t.Fatalf("expired waiter after verified stop receipt = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), app.TaskModelBudgetCapacityExpiredReason)
	var eventType string
	var payload []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT event_type,payload FROM task_events WHERE task_id=$1 ORDER BY sequence DESC LIMIT 1`, fence.TaskID).
		Scan(&eventType, &payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil || eventType != "task.status" ||
		event["status"] != string(task.StatusPaused) || event["reason"] != app.TaskModelBudgetCapacityExpiredReason {
		t.Fatalf("deadline event = %s %s err=%v", eventType, payload, err)
	}
}

func TestPostgresModelBudgetCapacityWaitPausesOnHardAccountExhaustion(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 100, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	insertCapacityTestReservation(t, f, fence.TaskID, 100, "settled")
	confirmCapacityWaitAgentStopped(t, f, fence, 103)
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 {
		t.Fatalf("reconcile after hard account budget exhaustion = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), app.TaskModelBudgetCapacityExhaustedReason)
}

func TestPostgresModelBudgetCapacityWaitRechecksCurrentMinimumAfterWake(t *testing.T) {
	f, oldFence, request := modelBudgetCapacityWaitFixture(t, 100, 5)
	ctx := context.Background()
	// The saved planner snapshot needs 75 tokens, but settled spend leaves 40.
	// A current planner snapshot with a smaller input bound and minimum can use
	// that positive slack safely after the Task is re-claimed.
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist capacity wait with old minimum: %v", err)
	}
	insertCapacityTestReservation(t, f, oldFence.TaskID, 60, "settled")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=60 WHERE id=$1`, oldFence.TaskID); err != nil {
		t.Fatal(err)
	}
	confirmCapacityWaitAgentStopped(t, f, oldFence, 111)

	woken, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || woken.Requeued != 1 || woken.Paused != 0 {
		t.Fatalf("positive account slack below old minimum should requeue for fresh planning: %+v err=%v", woken, err)
	}
	assertCapacityWaitTaskState(t, f, oldFence.TaskID, string(task.StatusQueued), "")
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-replan-worker", time.Minute)
	if err != nil || !found || claimed.ID != oldFence.TaskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim requeued Task for fresh budget preparation: task=%+v found=%t err=%v", claimed, found, err)
	}

	key := make([]byte, 32)
	key[0] = 112
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 113
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "agent-capacity-replan", AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion,
	}, SupervisorNonce: nonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start Agent after capacity wake: replayed=%t err=%v", replayed, err)
	}
	freshFence := oldFence
	freshFence.LeaseOwner, freshFence.LeaseVersion = claimed.Lease.Owner, claimed.Lease.Version
	freshFence.AgentInstanceID, freshFence.AgentGeneration = lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration
	freshFence.AgentSessionID, freshFence.AgentRootSessionID = lease.Fence.AgentSessionID, lease.Fence.AgentSessionID
	freshFence.AuthorizationVersion = lease.Fence.AuthorizationVersion
	freshFence.EngineVersion, freshFence.ProfileVersion = lease.Fence.EngineVersion, lease.Fence.ProfileVersion

	var endpoint, providerModel string
	var connectionVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT base_url,model_name,updated_at FROM model_connections WHERE id=$1`, request.ModelID).
		Scan(&endpoint, &providerModel, &connectionVersion); err != nil {
		t.Fatalf("read current provider projection for replanning: %v", err)
	}
	callID, err := app.DSHModelCallID(freshFence.TaskID, freshFence.AgentSessionID, freshFence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	providerDigest := sha256.Sum256([]byte("current model budget projection:" + freshFence.TaskID))
	digest := hex.EncodeToString(providerDigest[:])
	preparation := app.ModelBudgetReservationPreparation{
		Reservation: app.ModelCallReservation{Source: app.ModelCallTask, Task: &freshFence, OwnerID: f.ownerID,
			PersonalModel: false, ModelID: request.ModelID, ProviderEndpoint: endpoint, ProviderModel: providerModel,
			ConnectionVersion: connectionVersion, CallID: callID, DSHCallID: callID,
			BudgetCallID: "capacity-replan-budget-01", BudgetRequestDigest: digest,
			BudgetCapabilityVersion: "catalog-test-v1"},
		PlanID: "capacity-replan-plan-01", BudgetCallID: "capacity-replan-budget-01", ProviderRequestDigest: digest,
		Stage: "short_answer", StrategyVersion: "current-test-v2", CapabilityVersion: "catalog-test-v1",
		GoalTokens: 256, MinimumTokens: 16, StaticSafeTokens: 512, ModelMaxOutputTokens: 8192,
		ContextWindowTokens: 32768, InputContextUpperBound: 20, InputTokenReservation: 20,
		ContextSafetyMargin: 0, ProtocolMaxOutputTokens: 32768,
	}
	plan, err := f.store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil || plan.MaxOutputTokens != 20 {
		t.Fatalf("fresh preparation should recompute against the 40-token account remainder: plan=%+v err=%v", plan, err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if reservation.ReservedTokens != 40 {
		t.Fatalf("fresh reservation = %d tokens, want the 20 input + 20 output account remainder", reservation.ReservedTokens)
	}
	if err := f.store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("dispatch revalidated fresh budget plan: %v", err)
	}
	var spent, reserved int64
	if err := f.store.pool.QueryRow(ctx, `SELECT
COALESCE(SUM(input_tokens+output_tokens) FILTER (WHERE state='settled'),0)::bigint,
COALESCE(SUM(reserved_tokens) FILTER (WHERE state IN ('prepared','dispatched')),0)::bigint
FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, f.ownerID).Scan(&spent, &reserved); err != nil {
		t.Fatal(err)
	}
	if spent != 60 || reserved != 40 || spent+reserved > 100 {
		t.Fatalf("fresh dispatch exceeded current account cap: settled=%d reserved=%d total=%d limit=100", spent, reserved, spent+reserved)
	}
}

func TestPostgresModelBudgetCapacityWaitPausesOnHardTaskBudgetExhaustion(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, fence, 109)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=budget_model_tokens WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 {
		t.Fatalf("reconcile after hard Task budget exhaustion = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), app.TaskModelBudgetCapacityExhaustedReason)
}

func TestPostgresModelBudgetCapacityWaitPausesOnHardProjectBudgetExhaustion(t *testing.T) {
	f, projectID, fence := newProjectBudgetTaskFixture(t, "capacity wait project budget", 500, 1000)
	ctx := context.Background()
	fence.AgentRootSessionID = fence.AgentSessionID
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=0 WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("project capacity wait:" + fence.TaskID))
	request := app.TaskModelBudgetCapacityWaitRequest{Fence: fence, ModelID: "p1234567", ProjectID: projectID,
		DSHCallID: callID, LedgerCallID: callID, BudgetCallID: "project-capacity-budget-1",
		RequestDigest: hex.EncodeToString(digest[:]), InputTokenReservation: 25, MinimumOutputTokens: 25,
		MinimumProjectMicros: 75, ProjectBudgetCurrency: "USD"}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist project model budget capacity wait: %v", err)
	}
	confirmCapacityWaitAgentStoppedWithSupervisor(t, f, fence, 110, "budget-supervisor")
	if _, err := f.store.pool.Exec(ctx, `UPDATE project_budgets SET limit_micros=50 WHERE project_id=$1`, projectID); err != nil {
		t.Fatal(err)
	}
	projectSpendDigest := sha256.Sum256([]byte("settled project capacity spend:" + fence.TaskID))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO project_budget_ledger
(project_id,source,call_id,actor_id,task_id,model_id,rate_provider,rate_model,rate_version,currency,
input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
status,actual_input_tokens,actual_output_tokens,actual_micros,request_digest,settled_at)
VALUES ($1,'task','capacity-project-spend-01',$2,$3,'p1234567','provider','test-model',1,'USD',1000000,1000000,
1,0,50,'settled',1,0,50,$4,clock_timestamp())`, projectID, f.ownerID, fence.TaskID, projectSpendDigest[:]); err != nil {
		t.Fatalf("insert settled project spend at current hard limit: %v", err)
	}
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 {
		t.Fatalf("reconcile after hard project budget exhaustion = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), app.TaskModelBudgetCapacityExhaustedReason)
}

func TestPostgresModelBudgetCapacityWaitDoesNotClaimApprovalOrClarification(t *testing.T) {
	for _, reason := range []string{"approval_required", "clarification_required"} {
		t.Run(reason, func(t *testing.T) {
			f, _, _, fence, _ := revisionGatewayFixture(t)
			if _, err := f.store.pool.Exec(context.Background(), `UPDATE tasks SET status='waiting_user',waiting_reason=$2,
lease_owner='',lease_expires_at=NULL WHERE id=$1`, fence.TaskID, reason); err != nil {
				t.Fatal(err)
			}
			result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(context.Background(), 10)
			if err != nil || result != (app.TaskModelBudgetCapacityReconcileResult{}) {
				t.Fatalf("capacity reconcile changed unrelated wait: %+v err=%v", result, err)
			}
			claimed, found, err := f.store.ClaimNextTask(context.Background(), "unrelated-wait-worker", time.Minute)
			if err != nil || found {
				t.Fatalf("claim loop claimed %s wait: task=%q found=%t err=%v", reason, claimed.ID, found, err)
			}
			assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), reason)
		})
	}
}

func TestPostgresModelBudgetCapacityWaitConcurrentStoresWakeOnce(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, fence, 104)
	stores := []*Store{f.store, New(f.store.pool)}
	start := make(chan struct{})
	type outcome struct {
		result app.TaskModelBudgetCapacityReconcileResult
		err    error
	}
	results := make(chan outcome, len(stores))
	var group sync.WaitGroup
	for _, store := range stores {
		group.Add(1)
		go func(store *Store) {
			defer group.Done()
			<-start
			result, err := store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
			results <- outcome{result: result, err: err}
		}(store)
	}
	close(start)
	group.Wait()
	close(results)
	var requeued int32
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Reconcile: %v", result.err)
		}
		requeued += result.result.Requeued
	}
	if requeued != 1 {
		t.Fatalf("concurrent Reconcile woke %d Tasks, want exactly one", requeued)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusQueued), "")
	var queuedEvents, waiters int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type='task.status' AND payload->>'status'='queued'
 AND payload->>'reason'='model_budget_capacity'),
(SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1) FROM task_events WHERE task_id=$1`, fence.TaskID).
		Scan(&queuedEvents, &waiters); err != nil || queuedEvents != 1 || waiters != 0 {
		t.Fatalf("concurrent wake event/waiter state = queued:%d waiters:%d err=%v", queuedEvents, waiters, err)
	}
}

func TestPostgresModelBudgetCapacityWaitAuthorizationInvalidationPausesAfterReceipt(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET auth_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	withoutReceipt, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || withoutReceipt.Deferred != 1 || withoutReceipt.Paused != 0 {
		t.Fatalf("authorization invalidation before stop receipt = %+v err=%v", withoutReceipt, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	confirmCapacityWaitAgentStopped(t, f, fence, 107)
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 {
		t.Fatalf("authorization invalidation after stop receipt = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), taskModelBudgetCapacityInvalidatedReason)
}

func TestPostgresModelBudgetCapacityWaitCheckpointInvalidationPausesAndDeletesWait(t *testing.T) {
	f, fence, request := modelBudgetCapacityWaitFixture(t, 1000, 5)
	ctx := context.Background()
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist model budget capacity wait: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, fence, 108)
	changed := sha256.Sum256([]byte("changed capacity-wait checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_digest=$2 WHERE task_id=$1 AND owner_id=$3 AND session_id=$4`,
		fence.TaskID, changed[:], fence.OwnerID, fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Discarded != 0 {
		t.Fatalf("checkpoint invalidation reconcile = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, fence.TaskID, string(task.StatusPaused), taskModelBudgetCapacityInvalidatedReason)
	var waitRows int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1`, fence.TaskID).Scan(&waitRows); err != nil || waitRows != 0 {
		t.Fatalf("invalidated checkpoint waiter rows=%d err=%v, want zero", waitRows, err)
	}
}

func TestPostgresModelBudgetCapacityWaitDefersLargeHeadWithoutStarvingSmallRequest(t *testing.T) {
	f, largeFence, largeRequest := modelBudgetCapacityWaitFixture(t, 150, 5)
	smallFence, smallRequest := createSecondModelBudgetCapacityWaitTask(t, f)
	largeRequest.InputTokenReservation, largeRequest.MinimumOutputTokens = 100, 50
	if err := f.store.WaitForTaskModelBudgetCapacity(context.Background(), largeRequest); err != nil {
		t.Fatalf("persist older large waiter: %v", err)
	}
	if err := f.store.WaitForTaskModelBudgetCapacity(context.Background(), smallRequest); err != nil {
		t.Fatalf("persist newer small waiter: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, largeFence, 105)
	confirmCapacityWaitAgentStopped(t, f, smallFence, 106)
	insertCapacityTestReservation(t, f, largeFence.TaskID, 50, "dispatched")

	first, err := f.store.ReconcileTaskModelBudgetCapacityWaits(context.Background(), 10)
	if err != nil || first.Deferred != 1 || first.Requeued != 0 {
		t.Fatalf("older large waiter should defer on held capacity: %+v err=%v", first, err)
	}
	assertCapacityWaitTaskState(t, f, largeFence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	second, err := f.store.ReconcileTaskModelBudgetCapacityWaits(context.Background(), 10)
	if err != nil || second.Requeued != 1 || second.Paused != 0 {
		t.Fatalf("newer small waiter should use available capacity on next fair pass: %+v err=%v", second, err)
	}
	assertCapacityWaitTaskState(t, f, largeFence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	assertCapacityWaitTaskState(t, f, smallFence.TaskID, string(task.StatusQueued), "")
}

func modelBudgetCapacityRecoveryWaitFixture(t *testing.T) (routeActionsTestFixture, app.ModelTaskFence,
	app.TaskModelBudgetCapacityWaitRequest, task.ModelRecoveryIntent) {
	t.Helper()
	f, failure, failedCall, originalFence := createTaskModelRecoveryNoDispatch(t)
	ctx := context.Background()
	details, err := f.store.GetTask(ctx, originalFence.TaskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("capacity-wait recovery toolset"))
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, app.TaskModelRecoveryCommand{
		Task: details.Task, Failure: failure, CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
		CheckpointDigest: append([]byte(nil), details.Task.CheckpointDigest...), ToolsetDigest: toolsetDigest[:], Auto: true,
	})
	if err != nil || !accepted {
		t.Fatalf("schedule recovery before capacity wait: accepted=%t err=%v", accepted, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-recovery-worker", time.Minute)
	if err != nil || !found || claimed.ModelRecovery == nil || claimed.ModelRecovery.RecoveryCallID != intent.RecoveryCallID {
		t.Fatalf("claim scheduled recovery: task=%+v found=%t err=%v", claimed, found, err)
	}
	key := make([]byte, 32)
	key[0] = 121
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	startNonce := make([]byte, 32)
	startNonce[0] = 122
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "agent-capacity-recovery", AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion,
	}, SupervisorNonce: startNonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start recovery Agent: replayed=%t err=%v", replayed, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *claimed.ModelRecovery, lease.Fence); err != nil {
		t.Fatalf("begin recovery attempt: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET owner_id=NULL,updated_at=clock_timestamp() WHERE id='p1234567'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,1000,5)`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=0 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	recoveryFence := originalFence
	recoveryFence.AgentInstanceID, recoveryFence.AgentGeneration = lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration
	recoveryFence.LeaseOwner, recoveryFence.LeaseVersion = lease.Fence.LeaseOwner, lease.Fence.LeaseVersion
	recoveryFence.AgentRootSessionID = originalFence.AgentSessionID
	recoveryFence.RecoveryCallID, recoveryFence.RecoveryDSHCallID = intent.RecoveryCallID, intent.RecoveryDSHCallID
	digest := sha256.Sum256([]byte("recovery capacity wait:" + claimed.ID))
	request := app.TaskModelBudgetCapacityWaitRequest{Fence: recoveryFence, ModelID: failedCall.ModelID,
		DSHCallID: intent.RecoveryDSHCallID, LedgerCallID: intent.RecoveryCallID, BudgetCallID: "capacity-recovery-budget-1",
		RequestDigest: hex.EncodeToString(digest[:]), InputTokenReservation: 25, MinimumOutputTokens: 25}
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist recovery capacity wait: %v", err)
	}
	// Recovery admission must still see the prior stopped writer's checkpoint.
	// Once the wait is persisted, the waiting Agent owns that checkpoint and its
	// stop receipt is the proof used by capacity reconciliation.
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_checkpoints SET agent_instance_id=$3,agent_generation=$4,
engine_version=$5,profile_version=$6,auth_version=$7,lease_version=$8 WHERE task_id=$1 AND agent_session_id=$2 AND through_seq=0`,
		claimed.ID, lease.Fence.AgentSessionID, lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration,
		lease.Fence.EngineVersion, lease.Fence.ProfileVersion, lease.Fence.AuthorizationVersion, lease.Fence.LeaseVersion); err != nil {
		t.Fatalf("persist waiting recovery Agent checkpoint: %v", err)
	}
	return f, recoveryFence, request, intent
}

func TestPostgresModelBudgetCapacityWaitReschedulesVerifiedRecoveryAttempt(t *testing.T) {
	f, recoveryFence, request, _ := modelBudgetCapacityRecoveryWaitFixture(t)
	ctx := context.Background()
	deferred, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || deferred.Deferred != 1 || deferred.Requeued != 0 {
		t.Fatalf("recovery did not wait for its stop receipt: %+v err=%v", deferred, err)
	}
	confirmCapacityWaitAgentStopped(t, f, recoveryFence, 124)
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Requeued != 1 {
		t.Fatalf("recovery capacity wait requeue = %+v err=%v", result, err)
	}
	var status, reason, recoveryState string
	var taskSequence, recoverySequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT task.status,task.waiting_reason,task.event_seq,recovery.state,recovery.event_sequence
FROM tasks task JOIN task_model_recovery_schedules recovery ON recovery.task_id=task.id AND recovery.task_revision=task.current_revision
	WHERE task.id=$1`, request.Fence.TaskID).Scan(&status, &reason, &taskSequence, &recoveryState, &recoverySequence); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusQueued) || reason != taskModelRecoveryScheduledReason || recoveryState != "scheduled" || taskSequence != recoverySequence {
		t.Fatalf("recovery requeue state = Task:%s/%s/%d recovery:%s/%d", status, reason, taskSequence, recoveryState, recoverySequence)
	}
	next, found, err := f.store.ClaimNextTask(ctx, "capacity-recovery-reclaim", time.Minute)
	if err != nil || !found || next.ID != request.Fence.TaskID || next.ModelRecovery == nil || next.ModelRecovery.State != task.ModelRecoveryScheduled {
		t.Fatalf("existing claim loop did not resume recovery: task=%+v found=%t err=%v", next, found, err)
	}
}

func TestPostgresModelBudgetCapacityRecoveryInvalidationSpendsAttempt(t *testing.T) {
	f, recoveryFence, _, intent := modelBudgetCapacityRecoveryWaitFixture(t)
	ctx := context.Background()
	confirmCapacityWaitAgentStopped(t, f, recoveryFence, 125)
	changed := sha256.Sum256([]byte("changed recovery checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_digest=$2 WHERE id=$1`, recoveryFence.TaskID, changed[:]); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || result.Paused != 1 || result.Requeued != 0 || result.Discarded != 0 {
		t.Fatalf("invalidated recovery capacity wait = %+v err=%v", result, err)
	}
	assertCapacityWaitTaskState(t, f, recoveryFence.TaskID, string(task.StatusPaused), taskModelBudgetCapacityInvalidatedReason)
	var recoveryState string
	var activeRecoveries, waitRows int
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_model_recovery_schedules
WHERE task_id=$1 AND task_revision=$2 AND recovery_call_id=$3`, recoveryFence.TaskID, recoveryFence.TaskRevision,
		intent.RecoveryCallID).Scan(&recoveryState); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_model_recovery_schedules
WHERE task_id=$1 AND task_revision=$2 AND state IN ('attempting','scheduled')`, recoveryFence.TaskID, recoveryFence.TaskRevision).
		Scan(&activeRecoveries); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_waits WHERE task_id=$1`, recoveryFence.TaskID).
		Scan(&waitRows); err != nil {
		t.Fatal(err)
	}
	if recoveryState != "spent" || activeRecoveries != 0 || waitRows != 0 {
		t.Fatalf("invalidated recovery rows = state:%s active:%d waiters:%d; want spent/0/0", recoveryState, activeRecoveries, waitRows)
	}
	if claimed, found, err := f.store.ClaimNextTask(ctx, "invalidated-recovery-worker", time.Minute); err != nil || found {
		t.Fatalf("invalidated recovery remained claimable: task=%q found=%t err=%v", claimed.ID, found, err)
	}
}
