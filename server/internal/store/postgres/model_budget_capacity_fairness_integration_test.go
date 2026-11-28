package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresSharedProjectCapacityWaitMakesProgressUnderOtherOwnerTraffic(t *testing.T) {
	const projectLimitMicros = int64(650)
	ownerA, projectID, ownerAFence := newProjectBudgetTaskFixture(t, "cross-owner capacity fairness", projectLimitMicros, 10_000)
	ownerB, ownerBWaitFence := newOtherOwnerProjectBudgetTaskFixture(t, ownerA, projectID)
	ctx := context.Background()
	if _, err := ownerA.store.pool.Exec(ctx, `UPDATE model_account_budgets SET token_limit=10000,max_concurrent=1 WHERE owner_id=$1`, ownerB.ownerID); err != nil {
		t.Fatalf("configure waiting owner's independent account concurrency: %v", err)
	}

	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	ownerBActiveFence := createSecondProjectBudgetTaskFence(t, ownerB, projectID)
	ownerBActiveFence = setCapacityFairnessAgentID(t, ownerB, ownerBActiveFence, 100)
	ownerBActivePreparation, rate := projectBudgetPreparationForFence(t, ownerA.store, ownerB.ownerID, projectID,
		ownerBActiveFence, "fair-owner-b-active-plan", "fair-owner-b-active-budget", rate)
	ownerBActivePlan, err := ownerA.store.PrepareTaskModelBudget(ctx, ownerBActivePreparation)
	if err != nil || ownerBActivePlan.MaxOutputTokens < ownerBActivePreparation.MinimumTokens {
		t.Fatalf("prepare waiting owner's active slot: plan=%+v err=%v", ownerBActivePlan, err)
	}
	ownerBActiveReservation := finalizeTestModelBudgetReservation(t, ownerBActivePreparation, ownerBActivePlan)
	if err := ownerA.store.BeginModelCallDispatch(ctx, ownerBActiveReservation); err != nil {
		t.Fatalf("dispatch waiting owner's active call: %v", err)
	}
	ownerBWaitRequest := projectCapacityFairnessWaitRequest(t, ownerBWaitFence, projectID)
	if err := ownerA.store.WaitForTaskModelBudgetCapacity(ctx, ownerBWaitRequest); err != nil {
		t.Fatalf("persist other owner's shared-project waiter: %v", err)
	}
	confirmCapacityWaitAgentStoppedWithSupervisor(t, ownerB, ownerBWaitFence, 151, "budget-multi-owner-supervisor")
	firstWaitAt, deadlineAt := readCapacityCallDeadline(t, ownerA, ownerB.ownerID, ownerBWaitRequest.BudgetCallID)

	calls := make([]app.ModelCallReservation, 0, 7)
	for round := range 5 {
		fence := ownerAFence
		if round > 0 {
			fence = createSecondProjectBudgetTaskFence(t, ownerA, projectID)
			fence = setCapacityFairnessAgentID(t, ownerA, fence, round)
		}
		preparation, _ := projectBudgetPreparationForFence(t, ownerA.store, ownerA.ownerID, projectID, fence,
			fmt.Sprintf("fair-owner-a-plan-%02d", round), fmt.Sprintf("fair-owner-a-budget-%02d", round), rate)
		plan, err := ownerA.store.PrepareTaskModelBudget(ctx, preparation)
		if err != nil || plan.MaxOutputTokens < preparation.MinimumTokens {
			t.Fatalf("owner A fresh project call round %d: plan=%+v err=%v", round, plan, err)
		}
		if round == 4 && plan.MaxOutputTokens >= preparation.GoalTokens {
			t.Fatalf("owner A's last request should be reduced by shared project pressure: plan=%+v", plan)
		}
		reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
		if err := ownerA.store.BeginModelCallDispatch(ctx, reservation); err != nil {
			t.Fatalf("dispatch owner A project call round %d: %v", round, err)
		}
		calls = append(calls, reservation)
		if err := settleCapacityFairnessProjectCall(t, ownerA.store, projectID, reservation); err != nil {
			t.Fatalf("settle owner A project call round %d: %v", round, err)
		}
		result, err := ownerA.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
		if err != nil || result.Deferred != 1 || result.Requeued != 0 || result.Paused != 0 {
			t.Fatalf("reconcile other owner's waiter during traffic round %d: %+v err=%v", round, result, err)
		}
		assertCapacityWaitTaskState(t, ownerB, ownerBWaitFence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
		currentFirstWaitAt, currentDeadlineAt := readCapacityCallDeadline(t, ownerA, ownerB.ownerID, ownerBWaitRequest.BudgetCallID)
		if !currentFirstWaitAt.Equal(firstWaitAt) || !currentDeadlineAt.Equal(deadlineAt) || !deadlineAt.After(time.Now().UTC()) {
			t.Fatalf("other owner's wait deadline changed or expired under traffic: first=%s/%s deadline=%s/%s",
				firstWaitAt, currentFirstWaitAt, deadlineAt, currentDeadlineAt)
		}
	}

	if err := settleCapacityFairnessProjectCall(t, ownerA.store, projectID, ownerBActiveReservation); err != nil {
		t.Fatalf("settle waiting owner's prior active call: %v", err)
	}
	reconciled, err := ownerA.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || reconciled.Requeued != 1 || reconciled.Deferred != 0 || reconciled.Paused != 0 {
		t.Fatalf("reconcile other owner's waiter after its slot frees: %+v err=%v", reconciled, err)
	}
	assertCapacityWaitTaskState(t, ownerB, ownerBWaitFence.TaskID, string(task.StatusQueued), "")
	if !deadlineAt.After(time.Now().UTC()) {
		t.Fatalf("other owner's waiter missed its deadline before requeue: %s", deadlineAt)
	}
	claimed, found, err := ownerA.store.ClaimNextTask(ctx, "shared-project-capacity-fairness-worker", time.Minute)
	if err != nil || !found || claimed.ID != ownerBWaitFence.TaskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim other owner's requeued Task: task=%+v found=%t err=%v", claimed, found, err)
	}
	resumedFence := startCapacityWaitAgentAfterClaim(t, ownerB, ownerBWaitFence, claimed,
		"agent-shared-project-capacity-fairness", 152)
	ownerBWaitRequest.Fence = resumedFence
	var deadlineStillActive bool
	if err := ownerA.store.pool.QueryRow(ctx, `SELECT deadline_at>clock_timestamp() FROM model_budget_capacity_call_deadlines
WHERE owner_id=$1 AND task_id=$2 AND budget_call_id=$3`, ownerB.ownerID, resumedFence.TaskID, ownerBWaitRequest.BudgetCallID).
		Scan(&deadlineStillActive); err != nil || !deadlineStillActive {
		t.Fatalf("other owner's waiter deadline before prepare = %t err=%v", deadlineStillActive, err)
	}
	ownerBWaitPreparation, rate := projectBudgetPreparationForFence(t, ownerA.store, ownerB.ownerID, projectID,
		resumedFence, "fair-owner-b-wait-plan", ownerBWaitRequest.BudgetCallID, rate)
	ownerBWaitPlan, err := ownerA.store.PrepareTaskModelBudget(ctx, ownerBWaitPreparation)
	if err != nil || ownerBWaitPlan.MaxOutputTokens < ownerBWaitPreparation.MinimumTokens ||
		ownerBWaitPlan.MaxOutputTokens >= ownerBWaitPreparation.GoalTokens {
		t.Fatalf("prepare other owner's waiting call before deadline: plan=%+v err=%v", ownerBWaitPlan, err)
	}
	ownerBWaitReservation := finalizeTestModelBudgetReservation(t, ownerBWaitPreparation, ownerBWaitPlan)
	if err := ownerA.store.BeginModelCallDispatch(ctx, ownerBWaitReservation); err != nil {
		t.Fatalf("dispatch other owner's waiting call: %v", err)
	}
	if err := settleCapacityFairnessProjectCall(t, ownerA.store, projectID, ownerBWaitReservation); err != nil {
		t.Fatalf("settle other owner's waiting call: %v", err)
	}
	calls = append(calls, ownerBActiveReservation, ownerBWaitReservation)

	var actualMicros, reservedMicros int64
	var unknownRows int
	if err := ownerA.store.pool.QueryRow(ctx, `SELECT
COALESCE(sum(actual_micros) FILTER (WHERE status='settled'),0),
COALESCE(sum(reserved_micros) FILTER (WHERE status='reserved'),0),
count(*) FILTER (WHERE status='unknown')
FROM project_budget_ledger WHERE project_id=$1 AND source='task'`, projectID).
		Scan(&actualMicros, &reservedMicros, &unknownRows); err != nil {
		t.Fatal(err)
	}
	if actualMicros+reservedMicros > projectLimitMicros || unknownRows != 0 {
		t.Fatalf("shared project spend/reservations/unknown = %d/%d/%d, exceeds hard limit or leaves an unknown call",
			actualMicros, reservedMicros, unknownRows)
	}
	accountTokensByOwner := make(map[string]int64)
	for _, reservation := range calls {
		var accountOwner, accountTask, accountState string
		var accountInputTokens, accountOutputTokens int64
		if err := ownerA.store.pool.QueryRow(ctx, `SELECT owner_id,task_id,state,input_tokens,output_tokens
FROM model_call_ledger WHERE source='task' AND call_id=$1`, reservation.CallID).
			Scan(&accountOwner, &accountTask, &accountState, &accountInputTokens, &accountOutputTokens); err != nil {
			t.Fatalf("read account ledger attribution for %q: %v", reservation.CallID, err)
		}
		accountTokensByOwner[accountOwner] += accountInputTokens + accountOutputTokens
		var projectActor, projectTask, projectStatus string
		if err := ownerA.store.pool.QueryRow(ctx, `SELECT actor_id,task_id,status FROM project_budget_ledger
WHERE project_id=$1 AND source='task' AND call_id=$2`, projectID, reservation.CallID).
			Scan(&projectActor, &projectTask, &projectStatus); err != nil {
			t.Fatalf("read project ledger attribution for %q: %v", reservation.CallID, err)
		}
		if accountOwner != reservation.OwnerID || accountTask != reservation.Task.TaskID || accountState != "settled" ||
			projectActor != reservation.OwnerID || projectTask != reservation.Task.TaskID || projectStatus != "settled" {
			t.Fatalf("cross-owner ledger attribution for %q: account=%s/%s/%s project=%s/%s/%s want owner/task=%s/%s settled",
				reservation.CallID, accountOwner, accountTask, accountState, projectActor, projectTask, projectStatus,
				reservation.OwnerID, reservation.Task.TaskID)
		}
	}
	for ownerID, usedTokens := range accountTokensByOwner {
		if usedTokens > 10_000 {
			t.Fatalf("owner %s settled %d tokens beyond its 10000-token account hard limit", ownerID, usedTokens)
		}
	}
}

func TestPostgresModelBudgetCapacitySlotClaimProtectsFIFOWaiter(t *testing.T) {
	f, waitingFence, waitingRequest := modelBudgetCapacityWaitFixture(t, 10_000, 1)
	ctx := context.Background()

	initialFreshFence, initialFreshRequest := createSecondModelBudgetCapacityWaitTask(t, f)
	initialFreshFence = setCapacityFairnessAgentID(t, f, initialFreshFence, 0)
	initialFreshRequest.Fence = initialFreshFence
	initialReservation := prepareAndDispatchCapacityFairnessCall(t, f, initialFreshFence, initialFreshRequest, 0)
	if err := f.store.WaitForTaskModelBudgetCapacity(ctx, waitingRequest); err != nil {
		t.Fatalf("persist older capacity waiter: %v", err)
	}
	confirmCapacityWaitAgentStopped(t, f, waitingFence, 141)
	assertCapacityFairnessSlotMarker(t, f, waitingFence, waitingRequest.BudgetCallID, true)

	// Settlement frees the only provider slot, but before the periodic
	// reconciler can requeue the waiter a fresh Task reaches the real planner.
	settleCapacityFairnessCall(t, f, initialReservation)
	freshFence, freshRequest := createSecondModelBudgetCapacityWaitTask(t, f)
	freshFence = setCapacityFairnessAgentID(t, f, freshFence, 1)
	freshRequest.Fence = freshFence
	freshPreparation := capacityFairnessPreparation(t, f, freshFence, freshRequest)
	if _, err := f.store.PrepareTaskModelBudget(ctx, freshPreparation); err == nil {
		t.Fatal("fresh Task consumed the FIFO waiter's claimed concurrency slot before reconciliation")
	} else {
		var limit *domain.Error
		if domain.CodeOf(err) != domain.CodeRateLimited || !errors.As(err, &limit) ||
			limit.Details["reason"] != "model_concurrency_limit" || limit.Details["stage"] != "reservation" {
			t.Fatalf("fresh Task prepare error = %v, want pre-dispatch model concurrency limit", err)
		}
	}
	assertCapacityWaitTaskState(t, f, waitingFence.TaskID, string(task.StatusWaitingUser), app.TaskModelBudgetCapacityWaitReason)
	assertCapacityFairnessSlotMarker(t, f, waitingFence, waitingRequest.BudgetCallID, true)

	reconciled, err := f.store.ReconcileTaskModelBudgetCapacityWaits(ctx, 10)
	if err != nil || reconciled.Requeued != 1 || reconciled.Deferred != 0 {
		t.Fatalf("reconcile FIFO waiter after current call settled: %+v err=%v", reconciled, err)
	}
	assertCapacityWaitTaskState(t, f, waitingFence.TaskID, string(task.StatusQueued), "")
	assertCapacityFairnessSlotMarker(t, f, waitingFence, waitingRequest.BudgetCallID, true)

	claimed, found, err := f.store.ClaimNextTask(ctx, "capacity-fairness-waiter-worker", time.Minute)
	if err != nil || !found || claimed.ID != waitingFence.TaskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim FIFO waiter after reconcile: task=%+v found=%t err=%v", claimed, found, err)
	}
	waiterFence := startCapacityWaitAgentAfterClaim(t, f, waitingFence, claimed, "agent-capacity-fair-resumed", 142)
	waiterRequest := waitingRequest
	waiterRequest.Fence = waiterFence
	resumedCallID, err := app.DSHModelCallID(waiterFence.TaskID, waiterFence.AgentSessionID, waiterFence.ThroughSequence)
	if err != nil || resumedCallID != waiterRequest.LedgerCallID || resumedCallID != waiterRequest.DSHCallID {
		t.Fatalf("resumed waiter call identity = %q err=%v, want original ledger/DSH identity %q", resumedCallID, err, waiterRequest.LedgerCallID)
	}
	waiterPreparation := capacityFairnessPreparation(t, f, waiterFence, waiterRequest)
	waiterPlan, err := f.store.PrepareTaskModelBudget(ctx, waiterPreparation)
	if err != nil || waiterPlan.MaxOutputTokens < waiterPreparation.MinimumTokens {
		t.Fatalf("prepare resumed waiter with its reserved slot: plan=%+v err=%v", waiterPlan, err)
	}
	waiterReservation := finalizeTestModelBudgetReservation(t, waiterPreparation, waiterPlan)
	assertCapacityFairnessSlotMarker(t, f, waitingFence, waitingRequest.BudgetCallID, false)
	if err := f.store.BeginModelCallDispatch(ctx, waiterReservation); err != nil {
		t.Fatalf("dispatch resumed FIFO waiter through the existing store path: %v", err)
	}
	settleCapacityFairnessCall(t, f, waiterReservation)

	// Once the waiting Task has prepared and consumed its reserved slot, the
	// marker is gone. A fresh call can acquire the next slot after settlement.
	freshPlan, err := f.store.PrepareTaskModelBudget(ctx, freshPreparation)
	if err != nil || freshPlan.MaxOutputTokens < freshPreparation.MinimumTokens {
		t.Fatalf("fresh Task should acquire a later free slot after FIFO preparation: plan=%+v err=%v", freshPlan, err)
	}
	assertCapacityFairnessSlotMarker(t, f, waitingFence, waitingRequest.BudgetCallID, false)
}

func projectCapacityFairnessWaitRequest(t *testing.T, fence app.ModelTaskFence, projectID string) app.TaskModelBudgetCapacityWaitRequest {
	t.Helper()
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("canonical provider request\n" + callID))
	return app.TaskModelBudgetCapacityWaitRequest{Fence: fence, ModelID: "p1234567", ProjectID: projectID,
		DSHCallID: callID, LedgerCallID: callID, BudgetCallID: "fair-owner-b-budget-" + fence.TaskID,
		RequestDigest: hex.EncodeToString(digest[:]), InputTokenReservation: 50, MinimumOutputTokens: 16,
		MinimumProjectMicros: 66, ProjectBudgetCurrency: "USD"}
}

func settleCapacityFairnessProjectCall(t *testing.T, store *Store, projectID string, reservation app.ModelCallReservation) error {
	t.Helper()
	return store.SettleProjectModelCall(context.Background(), app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallTask, CallID: reservation.CallID, Usage: project.Usage{InputTokens: 50, OutputTokens: 10}})
}

func setCapacityFairnessAgentID(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence, index int) app.ModelTaskFence {
	t.Helper()
	ctx := context.Background()
	agentID := fmt.Sprintf("capacity-fairness-%02d-%s", index, fence.TaskID[1:])
	tx, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unique test Agent identity transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE task_agent_instances SET agent_instance_id=$2 WHERE task_id=$1 AND agent_generation=$3`,
		fence.TaskID, agentID, fence.AgentGeneration); err != nil {
		t.Fatalf("set unique test Agent identity: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2 WHERE id=$1 AND agent_generation=$3`,
		fence.TaskID, agentID, fence.AgentGeneration); err != nil {
		t.Fatalf("bind unique test Agent identity to Task: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_agent_checkpoints SET agent_instance_id=$2 WHERE task_id=$1 AND agent_generation=$3`,
		fence.TaskID, agentID, fence.AgentGeneration); err != nil {
		t.Fatalf("bind unique test Agent identity to checkpoint: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit unique test Agent identity: %v", err)
	}
	fence.AgentInstanceID = agentID
	return fence
}

func prepareAndDispatchCapacityFairnessCall(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence,
	waitRequest app.TaskModelBudgetCapacityWaitRequest, index int) app.ModelCallReservation {
	t.Helper()
	preparation := capacityFairnessPreparation(t, f, fence, waitRequest)
	plan, err := f.store.PrepareTaskModelBudget(context.Background(), preparation)
	if err != nil || plan.MaxOutputTokens < preparation.MinimumTokens {
		t.Fatalf("prepare fresh model call %d: plan=%+v err=%v", index, plan, err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := f.store.BeginModelCallDispatch(context.Background(), reservation); err != nil {
		t.Fatalf("dispatch fresh model call %d: %v", index, err)
	}
	return reservation
}

func capacityFairnessPreparation(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence,
	waitRequest app.TaskModelBudgetCapacityWaitRequest) app.ModelBudgetReservationPreparation {
	t.Helper()
	ctx := context.Background()
	var endpoint, providerModel string
	var connectionVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT base_url,model_name,updated_at FROM model_connections WHERE id=$1`, waitRequest.ModelID).
		Scan(&endpoint, &providerModel, &connectionVersion); err != nil {
		t.Fatalf("read model route for capacity call: %v", err)
	}
	providerDigest := waitRequest.RequestDigest
	if decoded, err := hex.DecodeString(providerDigest); err != nil || len(decoded) != sha256.Size {
		t.Fatalf("capacity call provider request digest is invalid: %v", err)
	}
	return app.ModelBudgetReservationPreparation{
		Reservation: app.ModelCallReservation{Source: app.ModelCallTask, Task: &fence, OwnerID: fence.OwnerID,
			PersonalModel: false, ModelID: waitRequest.ModelID, ProviderEndpoint: endpoint, ProviderModel: providerModel,
			ConnectionVersion: connectionVersion,
			CallID:            waitRequest.LedgerCallID, DSHCallID: waitRequest.DSHCallID, BudgetCallID: waitRequest.BudgetCallID,
			BudgetRequestDigest: providerDigest, BudgetCapabilityVersion: "catalog-test-v1"},
		PlanID: "fair-plan-" + fence.TaskID[1:], BudgetCallID: waitRequest.BudgetCallID, ProviderRequestDigest: providerDigest,
		Stage: "short_answer", StrategyVersion: "fairness-test-v1", CapabilityVersion: "catalog-test-v1",
		GoalTokens: 256, MinimumTokens: 16, StaticSafeTokens: 512, ModelMaxOutputTokens: 8192,
		ContextWindowTokens: 32768, InputContextUpperBound: 50, InputTokenReservation: 50,
		ContextSafetyMargin: 0, ProtocolMaxOutputTokens: 32768,
	}
}

func assertCapacityFairnessSlotMarker(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence,
	budgetCallID string, wantPresent bool) {
	t.Helper()
	var markerPresent bool
	if err := f.store.pool.QueryRow(context.Background(), `SELECT EXISTS (
  SELECT 1 FROM model_budget_capacity_call_deadlines
  WHERE owner_id=$1 AND task_id=$2 AND budget_call_id=$3 AND slot_reserved_at IS NOT NULL
)`, fence.InitiatorID, fence.TaskID, budgetCallID).Scan(&markerPresent); err != nil {
		t.Fatal(err)
	}
	if markerPresent != wantPresent {
		t.Fatalf("capacity slot marker present=%t, want %t", markerPresent, wantPresent)
	}
}

func settleCapacityFairnessCall(t *testing.T, f routeActionsTestFixture, reservation app.ModelCallReservation) {
	t.Helper()
	if err := f.store.SettleModelCall(context.Background(), app.ModelCallSettlement{Source: app.ModelCallTask,
		OwnerID: reservation.OwnerID, CallID: reservation.CallID,
		Usage: app.ModelUsage{Known: true, InputTokens: 50, OutputTokens: 10}}); err != nil {
		t.Fatalf("settle fresh model call %q: %v", reservation.CallID, err)
	}
}
