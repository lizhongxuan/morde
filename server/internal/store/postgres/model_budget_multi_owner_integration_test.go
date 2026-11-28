package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresConcurrentSharedProjectBudgetAcrossOwnersPreservesLedgerAttribution(t *testing.T) {
	firstFixture, projectID, firstFence := newProjectBudgetTaskFixture(t, "multi-owner budget", 300, 300)
	store, ctx := firstFixture.store, context.Background()
	secondFixture, secondFence := newOtherOwnerProjectBudgetTaskFixture(t, firstFixture, projectID)

	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	first, rate := projectBudgetPreparationForFence(t, store, firstFixture.ownerID, projectID, firstFence,
		"plan-multi-owner-a", "budget-call-multi-owner-a", rate)
	second, _ := projectBudgetPreparationForFence(t, store, secondFixture.ownerID, projectID, secondFence,
		"plan-multi-owner-b", "budget-call-multi-owner-b", rate)

	type ownerAttempt struct {
		name        string
		ownerID     string
		preparation app.ModelBudgetReservationPreparation
		plan        app.ModelBudgetReservationResult
		err         error
	}
	attempts := []*ownerAttempt{
		{name: "owner_a", ownerID: firstFixture.ownerID, preparation: first},
		{name: "owner_b", ownerID: secondFixture.ownerID, preparation: second},
	}
	start := make(chan struct{})
	results := make(chan *ownerAttempt, len(attempts))
	for _, attempt := range attempts {
		attempt := attempt
		go func() {
			<-start
			attempt.plan, attempt.err = store.PrepareTaskModelBudget(ctx, attempt.preparation)
			results <- attempt
		}()
	}
	close(start)

	var winner, loser *ownerAttempt
	for range len(attempts) {
		attempt := <-results
		if attempt.err == nil {
			if winner != nil {
				t.Fatal("shared project admitted two concurrent owners beyond its hard budget")
			}
			winner = attempt
		} else if domain.CodeOf(attempt.err) == domain.CodeQuotaExceeded {
			if loser != nil {
				t.Fatalf("more than one owner was denied: %v", attempt.err)
			}
			loser = attempt
		} else {
			t.Fatalf("%s prepare failed unexpectedly: %v", attempt.name, attempt.err)
		}
	}
	if winner == nil || loser == nil || winner.plan.MaxOutputTokens != 250 {
		t.Fatalf("concurrent shared project admission winner=%+v loser=%+v; want one 250-token hold and one quota denial", winner, loser)
	}

	for _, attempt := range attempts {
		var ledgerRows int
		var reserved int64
		if err := store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(reserved_tokens),0)
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 AND state IN ('prepared','dispatched')`,
			attempt.ownerID, attempt.preparation.Reservation.CallID).Scan(&ledgerRows, &reserved); err != nil {
			t.Fatal(err)
		}
		wantRows, wantReserved := 0, int64(0)
		if attempt == winner {
			wantRows, wantReserved = 1, 300
		}
		if ledgerRows != wantRows || reserved != wantReserved || reserved > 300 {
			t.Fatalf("%s account hold rows/tokens=%d/%d, want %d/%d within its 300-token limit",
				attempt.name, ledgerRows, reserved, wantRows, wantReserved)
		}
	}
	var projectRows int
	var projectTokens, projectMicros int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(reserved_input_tokens+reserved_output_tokens),0),
COALESCE(sum(reserved_micros),0) FROM project_budget_ledger WHERE project_id=$1 AND status='reserved'`, projectID).
		Scan(&projectRows, &projectTokens, &projectMicros); err != nil {
		t.Fatal(err)
	}
	if projectRows != 1 || projectTokens != 300 || projectMicros != 300 || projectMicros > 300 {
		t.Fatalf("shared project reservations rows/tokens/micros=%d/%d/%d, want 1/300/300 within its 300-micro limit",
			projectRows, projectTokens, projectMicros)
	}

	winnerReservation := finalizeTestModelBudgetReservation(t, winner.preparation, winner.plan)
	if err := store.BeginModelCallDispatch(ctx, winnerReservation); err != nil {
		t.Fatalf("dispatch winning owner budget: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallTask, CallID: winnerReservation.CallID, Usage: project.Usage{InputTokens: 50, OutputTokens: 0}}); err != nil {
		t.Fatalf("settle winning owner budget: %v", err)
	}
	var winnerProjectActor, winnerProjectStatus string
	var winnerProjectActualMicros int64
	if err := store.pool.QueryRow(ctx, `SELECT actor_id,status,actual_micros FROM project_budget_ledger
WHERE project_id=$1 AND source='task' AND call_id=$2`, projectID, winnerReservation.CallID).
		Scan(&winnerProjectActor, &winnerProjectStatus, &winnerProjectActualMicros); err != nil {
		t.Fatal(err)
	}
	if winnerProjectActor != winner.ownerID || winnerProjectStatus != "settled" || winnerProjectActualMicros != 50 {
		t.Fatalf("winning project charge actor/status/actual=%s/%s/%d, want %s/settled/50",
			winnerProjectActor, winnerProjectStatus, winnerProjectActualMicros, winner.ownerID)
	}

	// Once the first owner's known usage is settled, the other owner can use
	// only the remaining project allowance. Its account and Task budgets remain
	// independent from the first owner's ledger.
	loserPlan, err := store.PrepareTaskModelBudget(ctx, loser.preparation)
	if err != nil || loserPlan.MaxOutputTokens != 200 {
		t.Fatalf("remaining shared project allowance plan=%+v err=%v, want 200 output tokens", loserPlan, err)
	}
	loserReservation := finalizeTestModelBudgetReservation(t, loser.preparation, loserPlan)
	if err := store.BeginModelCallDispatch(ctx, loserReservation); err != nil {
		t.Fatalf("dispatch second owner budget: %v", err)
	}
	if err := store.RecordTaskModelBudgetOverage(ctx, loserReservation,
		app.ModelUsage{Known: true, InputTokens: 55, OutputTokens: 201}); err != nil {
		t.Fatalf("record second owner project overage: %v", err)
	}

	var accountOwner, accountState, accountTaskID string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id,state,task_id FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, loser.ownerID, loserReservation.CallID).
		Scan(&accountOwner, &accountState, &accountTaskID); err != nil {
		t.Fatal(err)
	}
	var projectActor, projectTaskID, projectStatus, currency string
	var reservedMicros, actualMicros, moneyOverage int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.actor_id,ledger.task_id,ledger.status,ledger.currency,
ledger.reserved_micros,overage.actual_micros,overage.money_overage_micros
FROM project_budget_ledger ledger JOIN model_budget_overage_reconciliations overage
 ON overage.owner_id=$2 AND overage.source=ledger.source AND overage.call_id=ledger.call_id
WHERE ledger.project_id=$1 AND ledger.source='task' AND ledger.call_id=$3`, projectID, loser.ownerID, loserReservation.CallID).
		Scan(&projectActor, &projectTaskID, &projectStatus, &currency, &reservedMicros, &actualMicros, &moneyOverage); err != nil {
		t.Fatal(err)
	}
	var reconciliationOwner, reason string
	var inputOverage, outputOverage int64
	if err := store.pool.QueryRow(ctx, `SELECT owner_id,reason_code,input_overage_tokens,output_overage_tokens
FROM model_budget_overage_reconciliations WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		loser.ownerID, loserReservation.CallID).Scan(&reconciliationOwner, &reason, &inputOverage, &outputOverage); err != nil {
		t.Fatal(err)
	}
	if accountOwner != loser.ownerID || accountState != "unknown" || accountTaskID != loserReservation.Task.TaskID ||
		projectActor != loser.ownerID || projectTaskID != loserReservation.Task.TaskID || projectStatus != "unknown" || currency != "USD" ||
		reservedMicros != 250 || actualMicros != 256 || moneyOverage != 6 || reconciliationOwner != loser.ownerID ||
		reason != "input_and_output_exceeded" || inputOverage != 5 || outputOverage != 1 {
		t.Fatalf("overage attribution account=%s/%s/%s project=%s/%s/%s %s %d/%d (+%d) reconciliation=%s/%s %d/%d",
			accountOwner, accountState, accountTaskID, projectActor, projectTaskID, projectStatus, currency,
			reservedMicros, actualMicros, moneyOverage, reconciliationOwner, reason, inputOverage, outputOverage)
	}
	var winnerState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		winner.ownerID, winnerReservation.CallID).Scan(&winnerState); err != nil || winnerState != "settled" {
		t.Fatalf("first owner's settled account ledger changed after other owner's overage: %s err=%v", winnerState, err)
	}
}

func newOtherOwnerProjectBudgetTaskFixture(t *testing.T, base routeActionsTestFixture, projectID string) (routeActionsTestFixture, app.ModelTaskFence) {
	t.Helper()
	store, ctx := base.store, context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2_000, 4_096)
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'member',1)`,
		projectID, owner.ID); err != nil {
		t.Fatalf("add second project budget owner as a project member: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,300,10)`, owner.ID); err != nil {
		t.Fatalf("set second owner model account budget: %v", err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	conversation := createMainConversationForTest(t, store, owner.ID)
	fixture := routeActionsTestFixture{store: store, ownerID: owner.ID, conversation: conversation, instanceID: base.instanceID,
		authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}}
	if _, err := store.pool.Exec(ctx, `UPDATE conversations SET project_id=$2 WHERE id=$1`, conversation.ID, projectID); err != nil {
		t.Fatalf("attach second owner's conversation to shared project: %v", err)
	}
	claim := fixture.receive(t, "budget-multi-owner-"+owner.ID, "second owner's shared project Task",
		app.ConversationRoutingInput{ProjectID: projectID})
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("route second owner's shared project Task: %v", err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-multi-owner-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.CurrentRevision == nil {
		t.Fatalf("claim second owner's project Task: id=%s found=%t err=%v", claimed.ID, found, err)
	}
	checkpointDigest := sha256.Sum256([]byte("second owner shared-project checkpoint digest"))
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET project_id=$2,budget_model_tokens=50000,agent_instance_id='agent-other-owner',
agent_generation=1,agent_session_id='session-other-owner',agent_engine_version='engine',agent_profile_version='profile',
	checkpoint_through_seq=0,checkpoint_digest=$3 WHERE id=$1`, taskID, projectID, checkpointDigest[:]); err != nil {
		t.Fatalf("initialize second owner's project Task checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-other-owner',0,$3)`, taskID, owner.ID, checkpointDigest[:]); err != nil {
		t.Fatalf("create second owner's project Agent session: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-other-owner',1,'session-other-owner','engine','profile','budget-multi-owner-supervisor',$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, owner.ID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("create second owner's project Agent instance: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,'agent-other-owner',1,'session-other-owner','session-other-owner',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, owner.ID, checkpointDigest[:], claimed.Lease.Version); err != nil {
		t.Fatalf("persist second owner's project Task checkpoint: %v", err)
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: owner.ID, InitiatorID: owner.ID,
		TaskRevision: *claimed.CurrentRevision, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: "agent-other-owner", AgentGeneration: 1, AgentSessionID: "session-other-owner", AgentRootSessionID: "session-other-owner",
		AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
	return fixture, fence
}
