package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func taskModelBudgetPreparationFixture(t *testing.T) (routeActionsTestFixture, app.ModelBudgetReservationPreparation) {
	t.Helper()
	fixture, _, _, fence, call := revisionGatewayFixture(t)
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	ctx := context.Background()
	var providerEndpoint, providerModel string
	var connectionVersion time.Time
	var personal bool
	if err := fixture.store.pool.QueryRow(ctx, `SELECT base_url,model_name,updated_at,owner_id IS NOT NULL
FROM model_connections WHERE id=$1`, call.ModelID).
		Scan(&providerEndpoint, &providerModel, &connectionVersion, &personal); err != nil {
		t.Fatalf("read model connection for adaptive budget fixture: %v", err)
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	providerDigest := sha256.Sum256([]byte("canonical test provider request"))
	digest := hex.EncodeToString(providerDigest[:])
	reservation := app.ModelCallReservation{
		Source: app.ModelCallTask, Task: &fence, OwnerID: fixture.ownerID, ProjectID: "", PersonalModel: personal,
		ModelID: call.ModelID, ProviderEndpoint: providerEndpoint, ProviderModel: providerModel,
		ConnectionVersion: connectionVersion, CallID: callID, DSHCallID: callID, BudgetCallID: "budget-call-0001",
		BudgetCapabilityVersion: "catalog-test-v1",
		BudgetRequestDigest:     digest,
	}
	return fixture, app.ModelBudgetReservationPreparation{
		Reservation: reservation, PlanID: "plan-budget-0001", BudgetCallID: reservation.BudgetCallID,
		ProviderRequestDigest: digest, Turn: 0, Step: 0, Stage: "short_answer", StrategyVersion: "test-v1",
		CapabilityVersion: "catalog-test-v1", GoalTokens: 256, MinimumTokens: 16, StaticSafeTokens: 512,
		ModelMaxOutputTokens: 8192, ContextWindowTokens: 32768, InputContextUpperBound: 50,
		InputTokenReservation: 50, ContextSafetyMargin: 0, ProtocolMaxOutputTokens: 32768,
	}
}

func finalizeTestModelBudgetReservation(t *testing.T, preparation app.ModelBudgetReservationPreparation,
	plan app.ModelBudgetReservationResult) app.ModelCallReservation {
	t.Helper()
	requestDigest, err := app.ModelBudgetFinalRequestDigest(preparation.ProviderRequestDigest, plan.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	reservation := preparation.Reservation
	reservation.RequestDigest = requestDigest
	reservation.ReservedTokens = preparation.InputTokenReservation + plan.MaxOutputTokens
	reservation.BudgetPlanID = plan.PlanID
	reservation.BudgetCallID = preparation.BudgetCallID
	reservation.BudgetRequestDigest = preparation.ProviderRequestDigest
	return reservation
}

func TestPostgresTaskModelBudgetPreparationReservesOnceAndSettles(t *testing.T) {
	fixture, preparation := taskModelBudgetPreparationFixture(t)
	ctx := context.Background()
	store := fixture.store

	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare adaptive Task model budget: %v", err)
	}
	if plan.PlanID != preparation.PlanID || plan.MaxOutputTokens != 256 {
		t.Fatalf("prepared plan = %#v, want plan id %q and cap 256", plan, preparation.PlanID)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.ValidateTaskModelBudgetPreparation(ctx, reservation); err != nil {
		t.Fatalf("validate prepared request identity: %v", err)
	}
	var state string
	var reserved, send, input int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.state,ledger.reserved_tokens,preparation.send_tokens,preparation.input_reserved_tokens
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, reservation.OwnerID, reservation.CallID).
		Scan(&state, &reserved, &send, &input); err != nil {
		t.Fatal(err)
	}
	if state != "prepared" || reserved != input+send || input != 50 || send != plan.MaxOutputTokens {
		t.Fatalf("prepared ledger state/reservation = %q/%d (input=%d output=%d)", state, reserved, input, send)
	}

	// Repeating the same DSH call and budget id returns the one existing plan.
	replayInput := preparation
	replayInput.PlanID = "plan-budget-replay"
	replay, err := store.PrepareTaskModelBudget(ctx, replayInput)
	if err != nil || replay != plan {
		t.Fatalf("same-key preparation replay = %#v, %v; want original %#v", replay, err, plan)
	}
	changedInput := preparation
	changedDigest := sha256.Sum256([]byte("changed provider projection"))
	changedInput.ProviderRequestDigest = hex.EncodeToString(changedDigest[:])
	changedInput.Reservation.BudgetRequestDigest = changedInput.ProviderRequestDigest
	if _, err := store.PrepareTaskModelBudget(ctx, changedInput); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same DSH call with changed request digest = %v, want idempotency conflict", err)
	}
	staleReservation := reservation
	changedCapability := preparation
	changedCapability.PlanID = "plan-budget-capability-v2"
	changedCapability.BudgetCallID = "budget-call-capability-v2"
	changedCapability.Reservation.BudgetCallID = changedCapability.BudgetCallID
	changedCapability.CapabilityVersion = "catalog-test-v2"
	changedCapability.Reservation.BudgetCapabilityVersion = changedCapability.CapabilityVersion
	changedCapability.StrategyVersion = "test-v2"
	updatedPlan, err := store.PrepareTaskModelBudget(ctx, changedCapability)
	if err != nil || updatedPlan.PlanID != changedCapability.PlanID || updatedPlan.MaxOutputTokens != plan.MaxOutputTokens {
		t.Fatalf("changed capability did not replace the unconsumed plan: %#v, %v", updatedPlan, err)
	}
	if err := store.BeginModelCallDispatch(ctx, staleReservation); err == nil {
		t.Fatal("replaced stale capability plan reached dispatch")
	}
	reservation = finalizeTestModelBudgetReservation(t, changedCapability, updatedPlan)
	var plans int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&plans); err != nil || plans != 1 {
		t.Fatalf("budget plan rows after replay/conflict = %d, %v", plans, err)
	}

	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("consume prepared plan at dispatch boundary: %v", err)
	}
	var consumed bool
	if err := store.pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, reservation.OwnerID, reservation.CallID).
		Scan(&state, &consumed); err != nil {
		t.Fatal(err)
	}
	if state != "dispatched" || !consumed {
		t.Fatalf("dispatch transition = state %q consumed=%t", state, consumed)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallTask, OwnerID: reservation.OwnerID,
		CallID: reservation.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 50, OutputTokens: 42}}); err != nil {
		t.Fatalf("settle adaptive Task model call: %v", err)
	}
	var usage *int64
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&state); err != nil || state != "settled" {
		t.Fatalf("settled ledger state = %q, %v", state, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, reservation.Task.TaskID).Scan(&usage); err != nil || usage == nil || *usage != 92 {
		t.Fatalf("settled Task usage = %v, %v; want input+output=92", usage, err)
	}
}

func TestPostgresExpiredPreparedModelBudgetIsNotDispatchedAndIsReleased(t *testing.T) {
	fixture, preparation := taskModelBudgetPreparationFixture(t)
	ctx := context.Background()
	store := fixture.store
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare adaptive Task model budget: %v", err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if released, err := store.CleanupExpiredTaskModelBudgetPreparations(ctx, 10); err != nil || released != 0 {
		t.Fatalf("cleanup unexpired adaptive budget hold = %d, %v; want zero", released, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_budget_preparations SET prepared_at=clock_timestamp()-interval '3 minutes',
expires_at=clock_timestamp()-interval '1 second'
WHERE owner_id=$1 AND source='task' AND call_id=$2`, reservation.OwnerID, reservation.CallID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err == nil {
		t.Fatal("expired adaptive plan reached dispatch")
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&state); err != nil || state != "prepared" {
		t.Fatalf("expired-but-not-dispatched ledger = %q, %v", state, err)
	}

	released, err := store.CleanupExpiredTaskModelBudgetPreparations(ctx, 10)
	if err != nil || released != 1 {
		t.Fatalf("cleanup expired adaptive budget holds = %d, %v; want one released hold", released, err)
	}
	var ledgerRows, planRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&planRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 0 || planRows != 0 {
		t.Fatalf("expired hold rows remain after cleanup: ledger=%d plan=%d", ledgerRows, planRows)
	}
}

func TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits(t *testing.T) {
	fixture, projectID, firstFence := newProjectBudgetTaskFixture(t, "budget concurrency", 300, 300)
	store := fixture.store
	ctx := context.Background()
	first, rate := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, firstFence,
		"plan-concurrent-task-a", "budget-call-task-a", project.RateSnapshot{
			Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
			InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000,
		})

	secondFence := createSecondProjectBudgetTaskFence(t, fixture, projectID)
	second, _ := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, secondFence,
		"plan-concurrent-task-b", "budget-call-task-b", rate)

	start := make(chan struct{})
	type attempt struct {
		name string
		plan app.ModelBudgetReservationResult
		err  error
	}
	results := make(chan attempt, 2)
	for _, input := range []struct {
		name        string
		preparation app.ModelBudgetReservationPreparation
	}{{"first", first}, {"second", second}} {
		input := input
		go func() {
			<-start
			plan, err := store.PrepareTaskModelBudget(ctx, input.preparation)
			results <- attempt{name: input.name, plan: plan, err: err}
		}()
	}
	close(start)

	successes, denied := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			if result.plan.MaxOutputTokens != 250 {
				t.Errorf("%s Task output cap = %d, want 250 under the shared 300-unit envelope", result.name, result.plan.MaxOutputTokens)
			}
		} else if domain.CodeOf(result.err) == domain.CodeQuotaExceeded {
			denied++
		} else {
			t.Errorf("%s Task reservation failed with unexpected error: %v", result.name, result.err)
		}
	}
	if successes != 1 || denied != 1 {
		t.Fatalf("shared-budget concurrent preparations: success=%d quota_denied=%d, want one of each", successes, denied)
	}
	var accountReserved, projectReserved, projectReservedMicros int64
	var accountRows, projectRows int
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(sum(reserved_tokens),0),count(*)
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND state IN ('prepared','dispatched')`, fixture.ownerID).
		Scan(&accountReserved, &accountRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(sum(reserved_input_tokens+reserved_output_tokens),0),COALESCE(sum(reserved_micros),0),count(*)
FROM project_budget_ledger WHERE project_id=$1 AND status='reserved'`, projectID).
		Scan(&projectReserved, &projectReservedMicros, &projectRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 1 || accountReserved > 300 || projectRows != 1 || projectReserved > 300 || projectReservedMicros > 300 {
		t.Fatalf("shared reservation totals exceed one envelope: account rows/tokens=%d/%d project rows/tokens/micros=%d/%d/%d",
			accountRows, accountReserved, projectRows, projectReserved, projectReservedMicros)
	}
}

func TestPostgresProjectFeeIntegerRoundingCapsAdaptiveOutput(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "budget rounding", 51, 1000)
	store, ctx := fixture.store, context.Background()
	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 30_000}
	preparation, rate := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, fence,
		"plan-budget-rounding", "budget-call-rounding", rate)

	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare output under integer-rounded project fee: %v", err)
	}
	if plan.MaxOutputTokens != 33 {
		t.Fatalf("project fee produced O_send=%d, want 33 tokens", plan.MaxOutputTokens)
	}
	cost33, err := rate.EstimateTokenCost(project.Usage{InputTokens: 50, OutputTokens: 33})
	if err != nil {
		t.Fatal(err)
	}
	cost34, err := rate.EstimateTokenCost(project.Usage{InputTokens: 50, OutputTokens: 34})
	if err != nil || cost33 != 51 || cost34 != 52 {
		t.Fatalf("integer-rounded boundary costs = 33 tokens:%d, 34 tokens:%d, err=%v; want 51 and 52 micros", cost33, cost34, err)
	}
	var inputTokens, reservedMicros, outputTokens int64
	if err := store.pool.QueryRow(ctx, `SELECT reserved_input_tokens,reserved_micros,reserved_output_tokens
FROM project_budget_ledger WHERE project_id=$1 AND call_id=$2 AND source='task'`, projectID, preparation.Reservation.CallID).
		Scan(&inputTokens, &reservedMicros, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if inputTokens != 50 || reservedMicros != 51 || outputTokens != 33 {
		t.Fatalf("project reservation = input:%d micros:%d output:%d; want 50/51/33", inputTokens, reservedMicros, outputTokens)
	}
}

func projectBudgetPreparationForFence(t *testing.T, store *Store, ownerID, projectID string, fence app.ModelTaskFence,
	planID, budgetCallID string, rate project.RateSnapshot) (app.ModelBudgetReservationPreparation, project.RateSnapshot) {
	t.Helper()
	var providerEndpoint, providerModel string
	var connectionVersion time.Time
	if err := store.pool.QueryRow(context.Background(), `SELECT base_url,model_name,updated_at FROM model_connections WHERE id=$1`, "p1234567").
		Scan(&providerEndpoint, &providerModel, &connectionVersion); err != nil {
		t.Fatalf("read shared test model connection: %v", err)
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte("canonical provider request\n" + callID))
	digest := hex.EncodeToString(digestBytes[:])
	rate.ModelID, rate.Model = "p1234567", providerModel
	return app.ModelBudgetReservationPreparation{
		Reservation: app.ModelCallReservation{
			Source: app.ModelCallTask, Task: &fence, OwnerID: ownerID, ProjectID: projectID, PersonalModel: false,
			ModelID: "p1234567", ProviderEndpoint: providerEndpoint, ProviderModel: providerModel,
			ConnectionVersion: connectionVersion, CallID: callID, DSHCallID: callID, BudgetCallID: budgetCallID,
			BudgetRequestDigest: digest, BudgetCapabilityVersion: "catalog-test-v1",
		},
		PlanID: planID, BudgetCallID: budgetCallID, ProviderRequestDigest: digest,
		Stage: "short_answer", StrategyVersion: "test-v1", CapabilityVersion: "catalog-test-v1",
		GoalTokens: 256, MinimumTokens: 16, StaticSafeTokens: 512,
		ModelMaxOutputTokens: 8192, ContextWindowTokens: 32768, InputContextUpperBound: 50,
		InputTokenReservation: 50, ProtocolMaxOutputTokens: 32768, ProjectRate: &rate,
	}, rate
}

func newProjectBudgetTaskFixture(t *testing.T, name string, projectLimit, accountLimit int64) (routeActionsTestFixture, string, app.ModelTaskFence) {
	t.Helper()
	fixture := newRouteActionsTestFixture(t)
	store, ctx := fixture.store, context.Background()
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	projectValue, err := project.New(projectID, fixture.ownerID, name, "adaptive model budget PostgreSQL integration")
	if err != nil {
		t.Fatal(err)
	}
	createKey := "budget-project-create-" + projectID
	if _, _, err := store.CreateProject(ctx, fixture.ownerID, projectValue, createKey); err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	limitKey := "budget-project-limit-" + projectID
	if _, err := store.SetProjectBudget(ctx, fixture.ownerID, projectID,
		project.Budget{Currency: "USD", LimitMicros: projectLimit, Version: 1}, 0, limitKey); err != nil {
		t.Fatalf("set project budget %q: %v", name, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET owner_id=NULL,updated_at=clock_timestamp()
WHERE id=$1`, "p1234567"); err != nil {
		t.Fatalf("make test connection a shared platform model: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,$2,10) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, fixture.ownerID, accountLimit); err != nil {
		t.Fatalf("set platform model account allowance: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, fixture.ownerID); err != nil {
		t.Fatalf("enable owner Task execution: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversations SET project_id=$2 WHERE id=$1`, fixture.conversation.ID, projectID); err != nil {
		t.Fatalf("attach conversation to project before Task creation: %v", err)
	}
	initial := fixture.receive(t, "budget-project-task-"+projectID, "prepare a project scoped Task",
		app.ConversationRoutingInput{ProjectID: projectID})
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("route project scoped Task: %v", err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-project-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim project Task: id=%q found=%t revision=%v err=%v", claimed.ID, found, claimed.CurrentRevision, err)
	}
	checkpointDigest := sha256.Sum256([]byte("project budget Task checkpoint"))
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000,agent_instance_id='agent-1',agent_generation=1,
agent_session_id='session-1',agent_engine_version='engine',agent_profile_version='profile',checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, taskID, checkpointDigest[:]); err != nil {
		t.Fatalf("initialize project Task checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-1',0,$3)`, taskID, fixture.ownerID, checkpointDigest[:]); err != nil {
		t.Fatalf("create project Task Agent session: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-1',1,'session-1','engine','profile','budget-supervisor',$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, fixture.ownerID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("create project Task Agent instance: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,'agent-1',1,'session-1','session-1',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, fixture.ownerID, checkpointDigest[:], claimed.Lease.Version); err != nil {
		t.Fatalf("persist project Task checkpoint: %v", err)
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: fixture.ownerID, InitiatorID: fixture.ownerID,
		TaskRevision: *claimed.CurrentRevision, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AgentRootSessionID: "session-1",
		AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
	return fixture, projectID, fence
}

func createSecondProjectBudgetTaskFence(t *testing.T, fixture routeActionsTestFixture, projectID string) app.ModelTaskFence {
	t.Helper()
	store, ctx := fixture.store, context.Background()
	conversation := createMainConversationForTest(t, store, fixture.ownerID)
	if _, err := store.pool.Exec(ctx, `UPDATE conversations SET project_id=$2 WHERE id=$1`, conversation.ID, projectID); err != nil {
		t.Fatalf("attach second Task conversation to project: %v", err)
	}
	claim := fixture.receiveInConversation(t, conversation.ID, "budget-concurrency-second-task", "second concurrent work",
		app.ConversationRoutingInput{ProjectID: projectID})
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("route second Task: %v", err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-concurrency-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim second Task: id=%q found=%t revision=%v err=%v", claimed.ID, found, claimed.CurrentRevision, err)
	}
	checkpointDigest := sha256.Sum256([]byte("budget concurrency second task checkpoint"))
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET project_id=$2,budget_model_tokens=50000,agent_instance_id='agent-2',
agent_generation=1,agent_session_id='session-2',agent_engine_version='engine',agent_profile_version='profile',
checkpoint_through_seq=0,checkpoint_digest=$3 WHERE id=$1`, taskID, projectID, checkpointDigest[:]); err != nil {
		t.Fatalf("initialize second Task checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-2',0,$3)`, taskID, fixture.ownerID, checkpointDigest[:]); err != nil {
		t.Fatalf("create second Task Agent session: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-2',1,'session-2','engine','profile',$6,$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, fixture.ownerID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version,
		"budget-supervisor-"+taskID); err != nil {
		t.Fatalf("create second Task Agent instance: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,'agent-2',1,'session-2','session-2',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, fixture.ownerID, checkpointDigest[:], claimed.Lease.Version); err != nil {
		t.Fatalf("persist second Task checkpoint: %v", err)
	}
	return app.ModelTaskFence{TaskID: taskID, OwnerID: fixture.ownerID, InitiatorID: fixture.ownerID,
		TaskRevision: *claimed.CurrentRevision, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: "agent-2", AgentGeneration: 1, AgentSessionID: "session-2", AgentRootSessionID: "session-2",
		AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
}
