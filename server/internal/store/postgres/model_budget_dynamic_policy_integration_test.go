package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresAdaptiveBudgetReplanUsesCurrentPolicyAndProjectRate(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "dynamic budget rate", 51, 1_000)
	store, ctx := fixture.store, context.Background()

	rateV1 := project.RateSnapshot{
		Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 30_000,
	}
	first, rateV1 := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, fence,
		"plan-dynamic-rate-v1", "budget-dynamic-rate-01", rateV1)
	firstPlan, err := store.PrepareTaskModelBudget(ctx, first)
	if err != nil || firstPlan.MaxOutputTokens != 33 {
		t.Fatalf("initial preparation = %+v, err=%v; want 33 output tokens under rate v1", firstPlan, err)
	}
	staleReservation := finalizeTestModelBudgetReservation(t, first, firstPlan)

	var initialProjectReserve int64
	if err := store.pool.QueryRow(ctx, `SELECT reserved_micros FROM project_budget_ledger
WHERE project_id=$1 AND source='task' AND call_id=$2 AND status='reserved'`, projectID, first.Reservation.CallID).
		Scan(&initialProjectReserve); err != nil || initialProjectReserve != 51 {
		t.Fatalf("initial project hold = %d, err=%v; want 51 micros", initialProjectReserve, err)
	}

	// The same logical BudgetCallID survives a capacity wait. Seed its already
	// persisted deadline, then re-prepare the same ledger call after both the
	// reviewed policy and project rate have changed.
	var checkpointDigest []byte
	if err := store.pool.QueryRow(ctx, `SELECT checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID).
		Scan(&checkpointDigest); err != nil {
		t.Fatalf("read checkpoint digest for persisted deadline: %v", err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO model_budget_capacity_call_deadlines
(owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
 model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,first_wait_at,deadline_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,COALESCE($18::text,''),$19,$20,$21,
 decode($22,'hex'),$23,clock.started_at,clock.started_at+interval '5 minutes'
FROM (SELECT clock_timestamp() AS started_at) clock`, first.Reservation.OwnerID, fence.TaskID, fence.OwnerID, fence.TaskRevision,
		fence.AuthorizationVersion, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration,
		fence.AgentSessionID, fence.AgentRootSessionID, fence.EngineVersion, fence.ProfileVersion, fence.ThroughSequence,
		fence.RecoveryCallID, fence.RecoveryDSHCallID, first.Reservation.ModelID, projectID, staleReservation.DSHCallID,
		staleReservation.CallID, first.BudgetCallID, first.ProviderRequestDigest, checkpointDigest)
	if err != nil {
		t.Fatalf("persist capacity deadline for the same BudgetCallID: %v", err)
	}

	rateV2 := rateV1
	rateV2.Version = 2
	rateV2.OutputMicrosPerMillion = 10_000
	second := first
	second.PlanID = "plan-dynamic-rate-v2"
	second.StrategyVersion = "test-v2"
	second.GoalTokens = 80
	second.ProjectRate = &rateV2
	secondPlan, err := store.PrepareTaskModelBudget(ctx, second)
	if err != nil || secondPlan.PlanID != second.PlanID || secondPlan.MaxOutputTokens != 80 {
		t.Fatalf("re-preparation after policy/rate change = %+v, err=%v; want current plan at 80 tokens", secondPlan, err)
	}

	if err := store.ValidateTaskModelBudgetPreparation(ctx, staleReservation); err == nil {
		t.Fatal("old signed cap remained valid after its unconsumed hold was replaced")
	}
	currentReservation := finalizeTestModelBudgetReservation(t, second, secondPlan)
	if err := store.ValidateTaskModelBudgetPreparation(ctx, currentReservation); err != nil {
		t.Fatalf("validate current policy/rate-bound plan: %v", err)
	}

	var deadlineRows, preparationRows, accountRows, projectRows int
	var accountReserved, projectInputReserved, projectOutputReserved, projectMicros int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_call_deadlines
WHERE owner_id=$1 AND budget_call_id=$2`, fixture.ownerID, first.BudgetCallID).Scan(&deadlineRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_preparations
WHERE owner_id=$1 AND source='task' AND call_id=$2`, fixture.ownerID, first.Reservation.CallID).Scan(&preparationRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(reserved_tokens),0)
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 AND state='prepared'`,
		fixture.ownerID, first.Reservation.CallID).Scan(&accountRows, &accountReserved); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(reserved_input_tokens),0),
COALESCE(sum(reserved_output_tokens),0),COALESCE(sum(reserved_micros),0)
FROM project_budget_ledger WHERE project_id=$1 AND source='task' AND call_id=$2 AND status='reserved'`,
		projectID, first.Reservation.CallID).Scan(&projectRows, &projectInputReserved, &projectOutputReserved, &projectMicros); err != nil {
		t.Fatal(err)
	}
	if deadlineRows != 0 || preparationRows != 1 || accountRows != 1 || accountReserved != 130 ||
		projectRows != 1 || projectInputReserved != 50 || projectOutputReserved != 80 || projectMicros != 51 {
		t.Fatalf("post-replan holds/deadline: deadline=%d plans=%d account rows/tokens=%d/%d project rows/input/output/micros=%d/%d/%d/%d",
			deadlineRows, preparationRows, accountRows, accountReserved, projectRows, projectInputReserved,
			projectOutputReserved, projectMicros)
	}
	var projectLimit int64
	if err := store.pool.QueryRow(ctx, `SELECT limit_micros FROM project_budgets WHERE project_id=$1 ORDER BY version DESC LIMIT 1`,
		projectID).Scan(&projectLimit); err != nil || projectMicros > projectLimit {
		t.Fatalf("re-planned project hold %d exceeds current project limit %d (err=%v)", projectMicros, projectLimit, err)
	}
}

func TestPostgresAdaptiveBudgetCapacityWakeUsesCurrentProjectRate(t *testing.T) {
	fixture, projectID, firstFence := newProjectBudgetTaskFixture(t, "capacity rate update", 110, 10_000)
	store, ctx := fixture.store, context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE model_account_budgets SET max_concurrent=1 WHERE owner_id=$1`, fixture.ownerID); err != nil {
		t.Fatalf("limit test account to one in-flight model call: %v", err)
	}

	rateV1 := project.RateSnapshot{
		Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 30_000,
	}
	active, rateV1 := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, firstFence,
		"plan-capacity-rate-holder", "budget-capacity-holder-01", rateV1)
	activePlan, err := store.PrepareTaskModelBudget(ctx, active)
	if err != nil || activePlan.MaxOutputTokens != 256 {
		t.Fatalf("prepare the reservation occupying the concurrency slot: plan=%+v err=%v", activePlan, err)
	}

	waitFence := createSecondProjectBudgetTaskFence(t, fixture, projectID)
	waitCallID, err := app.DSHModelCallID(waitFence.TaskID, waitFence.AgentSessionID, waitFence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	waitDigest := sha256.Sum256([]byte("capacity-rate-wait:" + waitFence.TaskID))
	minimumCost, err := rateV1.EstimateTokenCost(project.Usage{InputTokens: 50, OutputTokens: 16})
	if err != nil || minimumCost != 51 {
		t.Fatalf("estimate v1 minimum project hold=%d err=%v; want 51 micros", minimumCost, err)
	}
	request := app.TaskModelBudgetCapacityWaitRequest{
		Fence: waitFence, ModelID: "p1234567", ProjectID: projectID,
		DSHCallID: waitCallID, LedgerCallID: waitCallID,
		BudgetCallID: "budget-capacity-rate-01", RequestDigest: hex.EncodeToString(waitDigest[:]),
		InputTokenReservation: 50, MinimumOutputTokens: 16,
		MinimumProjectMicros: minimumCost, ProjectBudgetCurrency: "USD",
	}
	if err := store.WaitForTaskModelBudgetCapacity(ctx, request); err != nil {
		t.Fatalf("persist waiter while the account slot is occupied: %v", err)
	}
	firstWaitAt, deadlineAt := readCapacityCallDeadline(t, fixture, fixture.ownerID, request.BudgetCallID)
	confirmCapacityWaitAgentStoppedWithSupervisor(t, fixture, waitFence, 142, "budget-supervisor-"+waitFence.TaskID)

	// Expire the unsent reservation to release both the account concurrency
	// slot and project hold using the production cleanup transaction.
	if _, err := store.pool.Exec(ctx, `UPDATE model_budget_preparations SET prepared_at=clock_timestamp()-interval '3 minutes',
expires_at=clock_timestamp()-interval '1 second'
WHERE owner_id=$1 AND source='task' AND call_id=$2`, fixture.ownerID, active.Reservation.CallID); err != nil {
		t.Fatalf("expire active unsent preparation: %v", err)
	}
	if released, err := store.CleanupExpiredTaskModelBudgetPreparations(ctx, 10); err != nil || released != 1 {
		t.Fatalf("release active unsent preparation: released=%d err=%v; want one", released, err)
	}
	woken, err := store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || woken.Requeued != 1 || woken.Paused != 0 {
		t.Fatalf("requeue waiter after releasing provider capacity: %+v err=%v", woken, err)
	}
	assertCapacityWaitTaskState(t, fixture, waitFence.TaskID, string(task.StatusQueued), "")
	claimed, found, err := store.ClaimNextTask(ctx, "capacity-rate-replan-worker", time.Minute)
	if err != nil || !found || claimed.ID != waitFence.TaskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim requeued Task: task=%+v found=%t err=%v", claimed, found, err)
	}
	freshFence := startCapacityWaitAgentAfterClaim(t, fixture, waitFence, claimed, "agent-capacity-rate-replan", 143)
	firstWaitAfterWake, deadlineAfterWake := readCapacityCallDeadline(t, fixture, fixture.ownerID, request.BudgetCallID)
	if !firstWaitAt.Equal(firstWaitAfterWake) || !deadlineAt.Equal(deadlineAfterWake) {
		t.Fatalf("requeue changed the original BudgetCallID deadline: %s/%s -> %s/%s",
			firstWaitAt, deadlineAt, firstWaitAfterWake, deadlineAfterWake)
	}

	// The stage strategy and project rate changed while the Task waited. A
	// new Agent attempt keeps the logical BudgetCallID but must build its send
	// cap from the current rate snapshot instead of the queued minimum snapshot.
	rateV2 := rateV1
	rateV2.Version = 2
	rateV2.OutputMicrosPerMillion = 10_000
	preparation, _ := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, freshFence,
		"plan-capacity-rate-v2", request.BudgetCallID, rateV2)
	preparation.ProviderRequestDigest = request.RequestDigest
	preparation.Reservation.BudgetRequestDigest = request.RequestDigest
	preparation.StrategyVersion = "capacity-rate-strategy-v2"
	preparation.GoalTokens = 8_000
	preparation.StaticSafeTokens = 8_192
	preparation.ModelMaxOutputTokens = 8_192
	preparation.ContextWindowTokens = 32_768
	preparation.InputContextUpperBound = 50
	preparation.InputTokenReservation = 50
	preparation.ProtocolMaxOutputTokens = 32_768
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil || plan.MaxOutputTokens != 6_000 {
		t.Fatalf("post-wake preparation did not use v2 project rate and strategy: plan=%+v err=%v; want 6000", plan, err)
	}
	if err := store.ValidateTaskModelBudgetPreparation(ctx, finalizeTestModelBudgetReservation(t, preparation, plan)); err != nil {
		t.Fatalf("validate post-wake signed cap: %v", err)
	}
	var deadlineRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_capacity_call_deadlines
WHERE owner_id=$1 AND budget_call_id=$2`, fixture.ownerID, request.BudgetCallID).Scan(&deadlineRows); err != nil || deadlineRows != 0 {
		t.Fatalf("successful prepare left same-call deadline marker rows=%d err=%v", deadlineRows, err)
	}

	var reservedTokens, projectOutput, projectMicros, projectLimit int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.reserved_tokens,project_ledger.reserved_output_tokens,
project_ledger.reserved_micros,budget.limit_micros
FROM model_call_ledger ledger JOIN project_budget_ledger project_ledger
 ON project_ledger.project_id=$3 AND project_ledger.source='task' AND project_ledger.call_id=ledger.call_id AND project_ledger.status='reserved'
JOIN project_budgets budget ON budget.project_id=project_ledger.project_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2 AND ledger.state='prepared'
ORDER BY budget.version DESC LIMIT 1`, fixture.ownerID, preparation.Reservation.CallID, projectID).
		Scan(&reservedTokens, &projectOutput, &projectMicros, &projectLimit); err != nil {
		t.Fatalf("read current post-wake account/project reservations: %v", err)
	}
	if reservedTokens != 6_050 || projectOutput != 6_000 || projectMicros != 110 || projectMicros > projectLimit {
		t.Fatalf("post-wake reservations account=%d project output/micros=%d/%d limit=%d; want 6050 and 6000/110 within limit",
			reservedTokens, projectOutput, projectMicros, projectLimit)
	}
}
