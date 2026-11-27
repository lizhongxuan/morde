package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskModelRecoveryPreparesDistinctBudgetAndDSHIdentities(t *testing.T) {
	f, failure, failedCall, failedFence := createTaskModelRecoveryNoDispatch(t)
	ctx := context.Background()
	details, err := f.store.GetTask(ctx, failedFence.TaskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("verified read-only recovery toolset"))
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, app.TaskModelRecoveryCommand{
		Task: details.Task, Failure: failure, CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
		CheckpointDigest: append([]byte(nil), details.Task.CheckpointDigest...), ToolsetDigest: toolsetDigest[:], Auto: true,
	})
	if err != nil || !accepted || intent.State != task.ModelRecoveryScheduled {
		t.Fatalf("schedule recovery before adaptive preparation: intent=%+v accepted=%t err=%v", intent, accepted, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "recovery-budget-worker", time.Minute)
	if err != nil || !found || claimed.ID != failedFence.TaskID || claimed.ModelRecovery == nil {
		t.Fatalf("claim Task for adaptive recovery: task=%+v found=%t err=%v", claimed, found, err)
	}
	key := make([]byte, 32)
	key[0] = 101
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 102
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-recovery-budget",
		AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion,
	}, SupervisorNonce: nonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start recovery Agent for adaptive preparation: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *claimed.ModelRecovery, lease.Fence); err != nil {
		t.Fatalf("consume the scheduled recovery attempt: %v", err)
	}
	current, err := f.store.GetTask(ctx, failedFence.TaskID, f.ownerID)
	if err != nil || current.Task.ModelRecovery == nil || current.Task.ModelRecovery.State != task.ModelRecoveryAttempting {
		t.Fatalf("read the current recovery attempt fence: task=%+v err=%v", current.Task, err)
	}
	var endpoint, providerModel string
	var connectionVersion time.Time
	var personalModel bool
	if err := f.store.pool.QueryRow(ctx, `SELECT base_url,model_name,updated_at,owner_id IS NOT NULL
FROM model_connections WHERE id=$1`, failedCall.ModelID).Scan(&endpoint, &providerModel, &connectionVersion, &personalModel); err != nil {
		t.Fatal(err)
	}
	recoveryFence := app.ModelTaskFence{TaskID: current.Task.ID, OwnerID: current.Task.OwnerID, InitiatorID: current.Task.InitiatorID,
		TaskRevision: *current.Task.CurrentRevision, LeaseOwner: current.Task.Lease.Owner, LeaseVersion: current.Task.Lease.Version,
		AgentInstanceID: current.Task.AgentInstanceID, AgentGeneration: current.Task.AgentGeneration,
		AgentSessionID: current.Task.AgentSessionID, AgentRootSessionID: current.Task.AgentSessionID,
		AuthorizationVersion: current.Task.Authorization.Version, ThroughSequence: intent.CheckpointThroughSequence,
		EngineVersion: current.Task.AgentEngineVersion, ProfileVersion: current.Task.AgentProfileVersion,
		RecoveryCallID: intent.RecoveryCallID, RecoveryDSHCallID: intent.RecoveryDSHCallID}
	providerDigest := sha256.Sum256([]byte("recovery provider projection"))
	digest := hex.EncodeToString(providerDigest[:])
	preparation := app.ModelBudgetReservationPreparation{
		Reservation: app.ModelCallReservation{Source: app.ModelCallTask, Task: &recoveryFence,
			OwnerID: recoveryFence.InitiatorID, PersonalModel: personalModel, ModelID: failedCall.ModelID,
			ProviderEndpoint: endpoint, ProviderModel: providerModel, ConnectionVersion: connectionVersion,
			CallID: intent.RecoveryCallID, DSHCallID: intent.RecoveryDSHCallID,
			BudgetCallID: "recovery-budget-call-0001", BudgetRequestDigest: digest,
			BudgetCapabilityVersion: "recovery-capability-v1"},
		PlanID: "recovery-budget-plan-0001", BudgetCallID: "recovery-budget-call-0001", ProviderRequestDigest: digest,
		Turn: 1, Step: 0, Stage: "short_answer", StrategyVersion: "recovery-test-v1", CapabilityVersion: "recovery-capability-v1",
		GoalTokens: 256, MinimumTokens: 16, StaticSafeTokens: 512, ModelMaxOutputTokens: 8192,
		ContextWindowTokens: 32768, InputContextUpperBound: 64, InputTokenReservation: 64,
		ContextSafetyMargin: 0, ProtocolMaxOutputTokens: 32768,
	}
	plan, err := f.store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil || plan.PlanID != preparation.PlanID || plan.MaxOutputTokens != 256 {
		t.Fatalf("prepare adaptive recovery budget: plan=%+v err=%v", plan, err)
	}
	requestDigest, err := app.ModelBudgetFinalRequestDigest(digest, plan.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	reservation := preparation.Reservation
	reservation.RequestDigest = requestDigest
	reservation.ReservedTokens = preparation.InputTokenReservation + plan.MaxOutputTokens
	reservation.BudgetPlanID = plan.PlanID
	reservation.BudgetCallID = preparation.BudgetCallID
	reservation.BudgetRequestDigest = digest
	if err := f.store.ValidateTaskModelBudgetPreparation(ctx, reservation); err != nil {
		t.Fatalf("validate final recovery request against the prepared budget: %v", err)
	}
	var storedCallID, storedDSHCallID, state string
	if err := f.store.pool.QueryRow(ctx, `SELECT ledger.call_id,preparation.dsh_call_id,ledger.state
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, f.ownerID, intent.RecoveryCallID).
		Scan(&storedCallID, &storedDSHCallID, &state); err != nil {
		t.Fatal(err)
	}
	if storedCallID != intent.RecoveryCallID || storedDSHCallID != intent.RecoveryDSHCallID || state != "prepared" ||
		storedDSHCallID == failedCall.CallID || storedCallID == storedDSHCallID {
		t.Fatalf("recovery plan reused a failed identity or lost its ledger hold: ledger=%q DSH=%q state=%q",
			storedCallID, storedDSHCallID, state)
	}
}
