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
	"morde/server/internal/domain/task"
)

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
