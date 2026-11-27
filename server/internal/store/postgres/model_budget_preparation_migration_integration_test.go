package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; model budget preparation migration integration test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_budget_prep_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	config := baseConfig.Copy()
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop model budget preparation migration database: %v", err)
		}
		adminPool.Close()
	})

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 264); err != nil {
		closeProvider()
		t.Fatalf("prepare schema before model budget preparation migration: %v", err)
	}
	if _, err := provider.UpTo(ctx, 268); err != nil {
		closeProvider()
		t.Fatalf("apply adaptive model budget and recovery identity migrations: %v", err)
	}
	closeProvider()

	var tableExists, expiryIndexExists, overageTableExists, dshCallIDColumnExists, recoveryDSHCallIDColumnExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.model_budget_preparations') IS NOT NULL`).Scan(&tableExists); err != nil || !tableExists {
		t.Fatalf("model budget preparations table exists=%t err=%v", tableExists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_indexes WHERE schemaname='public' AND indexname='model_budget_preparations_expiry_idx'
)`).Scan(&expiryIndexExists); err != nil || !expiryIndexExists {
		t.Fatalf("model budget preparation expiry index exists=%t err=%v", expiryIndexExists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.model_budget_overage_reconciliations') IS NOT NULL`).Scan(&overageTableExists); err != nil || !overageTableExists {
		t.Fatalf("model budget overage reconciliation table exists=%t err=%v", overageTableExists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema='public' AND table_name='model_budget_preparations' AND column_name='dsh_call_id')`).Scan(&dshCallIDColumnExists); err != nil || !dshCallIDColumnExists {
		t.Fatalf("model budget preparation DSH call identity column exists=%t err=%v", dshCallIDColumnExists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema='public' AND table_name='task_model_recovery_schedules' AND column_name='recovery_dsh_call_id')`).Scan(&recoveryDSHCallIDColumnExists); err != nil || !recoveryDSHCallIDColumnExists {
		t.Fatalf("Task recovery DSH call identity column exists=%t err=%v", recoveryDSHCallIDColumnExists, err)
	}

	const ownerID = "u_budget_prep"
	const taskID = "q1234567"
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,password_hash,role,status,quota_hosts,quota_cpu_milli,quota_mem_mib)
VALUES ($1,'model-budget-preparation','test','user','approved',3,4000,8192)`, ownerID); err != nil {
		t.Fatalf("insert migration test user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'budget preparation migration')`, ownerID); err != nil {
		t.Fatalf("insert migration test conversation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens)
VALUES ($1,$2,$2,'c1234567','p1234567','budget preparation migration','queued','session','',600,10,50000,0)`, taskID, ownerID); err != nil {
		t.Fatalf("insert migration test Task: %v", err)
	}

	insertLedger := func(callID, state string, settledAt *time.Time, inputTokens, outputTokens *int64) error {
		_, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,input_tokens,output_tokens,settled_at)
VALUES ($1,'task',$2,decode(repeat('11',32),'hex'),'p1234567','deepseek-flash',$3,10000,$4,$5,$6,$7)`,
			ownerID, callID, taskID, state, inputTokens, outputTokens, settledAt)
		return err
	}
	zero, ten, five := int64(0), int64(10), int64(5)
	now := time.Now().UTC()
	for _, state := range []string{"prepared", "dispatched"} {
		if err := insertLedger("mdl_"+state+"_ok", state, nil, nil, nil); err != nil {
			t.Fatalf("insert %s ledger row with NULL settled_at: %v", state, err)
		}
	}
	for _, state := range []string{"unknown", "rejected", "settled"} {
		var input, output *int64
		if state == "settled" {
			input, output = &ten, &five
		}
		if err := insertLedger("mdl_"+state+"_ok", state, &now, input, output); err != nil {
			t.Fatalf("insert %s ledger row with settled_at: %v", state, err)
		}
	}
	if err := expectModelBudgetPreparationConstraintFailure(ctx, pool, func(tx pgx.Tx, callID string) error {
		_, err := tx.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,settled_at)
VALUES ($1,'task',$2,decode(repeat('11',32),'hex'),'p1234567','deepseek-flash',$3,10000,'prepared',clock_timestamp())`, ownerID, callID, taskID)
		return err
	}, "prepared state must retain NULL settled_at"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"settled", "unknown", "rejected"} {
		if err := expectModelBudgetPreparationConstraintFailure(ctx, pool, func(tx pgx.Tx, callID string) error {
			_, err := tx.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,settled_at,input_tokens,output_tokens)
VALUES ($1,'task',$2,decode(repeat('11',32),'hex'),'p1234567','deepseek-flash',$3,10000,$4,NULL,$5,$6)`,
				ownerID, callID, taskID, state, &zero, &zero)
			return err
		}, state+" state must retain its settled_at semantics"); err != nil {
			t.Fatal(err)
		}
	}
	if err := expectModelBudgetPreparationConstraintFailure(ctx, pool, func(tx pgx.Tx, callID string) error {
		return insertModelBudgetPreparationLedger(ctx, tx, ownerID, taskID, callID, "settled", &now, nil, nil)
	}, "settled state must retain its usage pair constraint"); err != nil {
		t.Fatal(err)
	}

	valid := modelBudgetPreparationTestRow{
		OwnerID: ownerID, CallID: "mdl_valid_0001", TaskID: taskID, TaskRevision: 1, LeaseOwner: "worker-1", LeaseVersion: 3,
		AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1", AgentRootSessionID: "root-session-1",
		AuthorizationVersion: 7, EngineVersion: "engine", ProfileVersion: "profile", ThroughSequence: 0, Turn: 1, Step: 0,
		PlanID: "plan_valid_0001", BudgetCallID: "budget_valid_0001", CapabilityVersion: "2026-11-27",
		Digest: bytesOf(32, 0x22), ModelID: "p1234567", Provider: "deepseek", ProviderModel: "deepseek-flash",
		Endpoint: "https://api.deepseek.com", ConnectionVersion: now, Stage: "task_step", StrategyVersion: "task-output-v1",
		ModelMaxOutputTokens: 128000, ContextWindowTokens: 128000, ContextInputUpperBoundTokens: 4000, ContextSafetyMarginTokens: 1000,
		GoalTokens: 12000, MinimumTokens: 512, SafeTokens: 123000, SendTokens: 12000, InputReservedTokens: 4000,
		PreparedAt: now, ExpiresAt: now.Add(2 * time.Minute),
	}
	if err := insertLedger(valid.CallID, "prepared", nil, nil, nil); err != nil {
		t.Fatalf("insert valid prepared ledger row: %v", err)
	}
	validTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertModelBudgetPreparation(ctx, validTx, valid); err != nil {
		_ = validTx.Rollback(ctx)
		t.Fatalf("insert valid model budget preparation: %v", err)
	}
	if err := validTx.Commit(ctx); err != nil {
		t.Fatalf("commit valid model budget preparation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_budget_overage_reconciliations
(owner_id,source,call_id,plan_id,budget_call_id,task_id,reason_code,reserved_input_tokens,reserved_output_tokens,
 actual_input_tokens,actual_output_tokens)
VALUES ($1,'task',$2,$3,$4,$5,'input_bound_exceeded',50,10,55,10)`, ownerID, valid.CallID, valid.PlanID,
		valid.BudgetCallID, valid.TaskID); err != nil {
		t.Fatalf("insert valid model budget overage reconciliation: %v", err)
	}
	var storedPlanID, storedBudgetCallID, storedCapabilityVersion string
	var storedDigest []byte
	var storedSend, storedInput, storedTaskRevision, storedLeaseVersion, storedAgentGeneration, storedAuthorizationVersion int64
	var storedThrough, storedTurn, storedStep int64
	var storedLeaseOwner, storedAgentInstance, storedAgentSession, storedAgentRootSession, storedEngine, storedProfile string
	scanErr := pool.QueryRow(ctx, `SELECT plan_id,budget_call_id,request_digest,send_tokens,input_reserved_tokens,task_revision,lease_owner,
lease_version,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,authorization_version,engine_version,profile_version,
through_sequence,turn,step,capability_version
FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`, ownerID, valid.CallID).
		Scan(&storedPlanID, &storedBudgetCallID, &storedDigest, &storedSend, &storedInput, &storedTaskRevision, &storedLeaseOwner,
			&storedLeaseVersion, &storedAgentInstance, &storedAgentGeneration, &storedAgentSession, &storedAgentRootSession,
			&storedAuthorizationVersion, &storedEngine, &storedProfile, &storedThrough, &storedTurn, &storedStep, &storedCapabilityVersion)
	if scanErr != nil || storedPlanID != valid.PlanID || storedBudgetCallID != valid.BudgetCallID || len(storedDigest) != 32 ||
		storedSend != valid.SendTokens || storedInput != valid.InputReservedTokens || storedTaskRevision != valid.TaskRevision ||
		storedLeaseOwner != valid.LeaseOwner || storedLeaseVersion != valid.LeaseVersion || storedAgentInstance != valid.AgentInstanceID ||
		storedAgentGeneration != valid.AgentGeneration || storedAgentSession != valid.AgentSessionID ||
		storedAgentRootSession != valid.AgentRootSessionID || storedAuthorizationVersion != valid.AuthorizationVersion ||
		storedEngine != valid.EngineVersion || storedProfile != valid.ProfileVersion || storedThrough != valid.ThroughSequence ||
		storedTurn != valid.Turn || storedStep != valid.Step || storedCapabilityVersion != valid.CapabilityVersion {
		t.Fatalf("stored preparation = plan:%q budget_call:%q digest_len:%d send:%d input:%d revision:%d lease:%q/%d agent:%q/%d session:%q root:%q auth:%d engine:%q profile:%q through:%d turn:%d step:%d err=%v",
			storedPlanID, storedBudgetCallID, len(storedDigest), storedSend, storedInput, storedTaskRevision, storedLeaseOwner,
			storedLeaseVersion, storedAgentInstance, storedAgentGeneration, storedAgentSession, storedAgentRootSession,
			storedAuthorizationVersion, storedEngine, storedProfile, storedThrough, storedTurn, storedStep, scanErr)
	}

	invalidRows := []struct {
		name   string
		mutate func(*modelBudgetPreparationTestRow)
	}{
		{name: "digest length", mutate: func(row *modelBudgetPreparationTestRow) { row.Digest = []byte("short") }},
		{name: "send exceeds goal and safe", mutate: func(row *modelBudgetPreparationTestRow) { row.SendTokens = row.GoalTokens + 1 }},
		{name: "minimum exceeds goal", mutate: func(row *modelBudgetPreparationTestRow) { row.MinimumTokens = row.GoalTokens + 1 }},
		{name: "expiry before preparation", mutate: func(row *modelBudgetPreparationTestRow) { row.ExpiresAt = row.PreparedAt }},
		{name: "expiry exceeds maximum lifetime", mutate: func(row *modelBudgetPreparationTestRow) { row.ExpiresAt = row.PreparedAt.Add(6 * time.Minute) }},
		{name: "consumed after expiry", mutate: func(row *modelBudgetPreparationTestRow) { consumedAt := row.ExpiresAt; row.ConsumedAt = &consumedAt }},
		{name: "task identity mismatch", mutate: func(row *modelBudgetPreparationTestRow) { row.TaskID = "q7654321" }},
		{name: "model identity mismatch", mutate: func(row *modelBudgetPreparationTestRow) { row.ProviderModel = "different-model" }},
		{name: "missing generation fence", mutate: func(row *modelBudgetPreparationTestRow) { row.AgentGeneration = 0 }},
		{name: "invalid turn", mutate: func(row *modelBudgetPreparationTestRow) { row.Turn = -1 }},
		{name: "invalid step", mutate: func(row *modelBudgetPreparationTestRow) { row.Step = -1 }},
		{name: "duplicate plan id", mutate: func(row *modelBudgetPreparationTestRow) { row.PlanID = valid.PlanID }},
		{name: "duplicate budget call id", mutate: func(row *modelBudgetPreparationTestRow) { row.BudgetCallID = valid.BudgetCallID }},
		{name: "empty capability version", mutate: func(row *modelBudgetPreparationTestRow) { row.CapabilityVersion = "" }},
	}
	for index, test := range invalidRows {
		t.Run(test.name, func(t *testing.T) {
			row := valid
			row.CallID = fmt.Sprintf("mdl_invalid_%04d", index)
			row.PlanID = fmt.Sprintf("plan_invalid_%04d", index)
			row.BudgetCallID = fmt.Sprintf("budget_invalid_%04d", index)
			test.mutate(&row)
			if err := expectModelBudgetPreparationConstraintFailure(ctx, pool, func(tx pgx.Tx, callID string) error {
				if err := insertModelBudgetPreparationLedger(ctx, tx, ownerID, taskID, callID, "prepared", nil, nil, nil); err != nil {
					return err
				}
				row.CallID = callID
				return insertModelBudgetPreparation(ctx, tx, row)
			}, "invalid preparation row was accepted"); err != nil {
				t.Fatal(err)
			}
		})
	}

	// The overage migration must refuse rollback while reconciliation evidence exists.
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 264); err == nil || !strings.Contains(err.Error(), "cannot roll back model budget overage reconciliation records") {
		closeProvider()
		t.Fatalf("rollback with an overage reconciliation row = %v, want guarded failure", err)
	}
	closeProvider()
	if _, err := pool.Exec(ctx, `DELETE FROM model_budget_overage_reconciliations WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		ownerID, valid.CallID); err != nil {
		t.Fatalf("delete migration test overage reconciliation: %v", err)
	}

	// The preparation migration must refuse rollback while a plan row exists.
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 264); err == nil || !strings.Contains(err.Error(), "plan records or prepared model calls exist") {
		closeProvider()
		t.Fatalf("rollback with a preparation row = %v, want guarded failure", err)
	}
	closeProvider()
	if _, err := pool.Exec(ctx, `DELETE FROM model_budget_preparations WHERE owner_id=$1 AND call_id=$2`, ownerID, valid.CallID); err != nil {
		t.Fatalf("delete migration test preparation: %v", err)
	}
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 264); err == nil || !strings.Contains(err.Error(), "plan records or prepared model calls exist") {
		closeProvider()
		t.Fatalf("rollback with a prepared ledger row = %v, want guarded failure", err)
	}
	closeProvider()
	if _, err := pool.Exec(ctx, `UPDATE model_call_ledger SET state='dispatched' WHERE owner_id=$1 AND state='prepared'`, ownerID); err != nil {
		t.Fatalf("convert test prepared rows to legacy in-flight state: %v", err)
	}
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 264); err != nil {
		closeProvider()
		t.Fatalf("rollback after all preparation evidence is removed: %v", err)
	}
	closeProvider()
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.model_budget_preparations') IS NULL,
to_regclass('public.model_budget_overage_reconciliations') IS NULL`).Scan(&tableExists, &overageTableExists); err != nil || !tableExists || !overageTableExists {
		t.Fatalf("budget tables removed after safe rollback=%t/%t err=%v", tableExists, overageTableExists, err)
	}
}

type modelBudgetPreparationTestRow struct {
	OwnerID                      string
	CallID                       string
	TaskID                       string
	TaskRevision                 int64
	LeaseOwner                   string
	LeaseVersion                 int64
	AgentInstanceID              string
	AgentGeneration              int64
	AgentSessionID               string
	AgentRootSessionID           string
	AuthorizationVersion         int64
	EngineVersion                string
	ProfileVersion               string
	ThroughSequence              int64
	Turn                         int64
	Step                         int64
	PlanID                       string
	BudgetCallID                 string
	Digest                       []byte
	ModelID                      string
	Provider                     string
	ProviderModel                string
	Endpoint                     string
	ConnectionVersion            time.Time
	CapabilityVersion            string
	Stage                        string
	StrategyVersion              string
	ModelMaxOutputTokens         int64
	ContextWindowTokens          int64
	ContextInputUpperBoundTokens int64
	ContextSafetyMarginTokens    int64
	GoalTokens                   int64
	MinimumTokens                int64
	SafeTokens                   int64
	SendTokens                   int64
	InputReservedTokens          int64
	PreparedAt                   time.Time
	ExpiresAt                    time.Time
	ConsumedAt                   *time.Time
}

func insertModelBudgetPreparation(ctx context.Context, tx pgx.Tx, row modelBudgetPreparationTestRow) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_budget_preparations
(owner_id,source,call_id,task_id,task_revision,lease_owner,lease_version,agent_instance_id,agent_generation,agent_session_id,
 agent_root_session_id,authorization_version,engine_version,profile_version,through_sequence,turn,step,
plan_id,budget_call_id,dsh_call_id,request_digest,model_id,provider,provider_model,endpoint,
 connection_version,capability_version,stage,strategy_version,model_max_output_tokens,context_window_tokens,context_input_upper_bound_tokens,
 context_safety_margin_tokens,goal_tokens,minimum_tokens,safe_tokens,send_tokens,input_reserved_tokens,prepared_at,expires_at,consumed_at)
VALUES ($1,'task',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40)`,
		row.OwnerID, row.CallID, row.TaskID, row.TaskRevision, row.LeaseOwner, row.LeaseVersion, row.AgentInstanceID,
		row.AgentGeneration, row.AgentSessionID, row.AgentRootSessionID, row.AuthorizationVersion, row.EngineVersion,
		row.ProfileVersion, row.ThroughSequence, row.Turn, row.Step, row.PlanID, row.BudgetCallID, row.CallID, row.Digest,
		row.ModelID, row.Provider, row.ProviderModel, row.Endpoint, row.ConnectionVersion, row.CapabilityVersion, row.Stage, row.StrategyVersion,
		row.ModelMaxOutputTokens, row.ContextWindowTokens, row.ContextInputUpperBoundTokens, row.ContextSafetyMarginTokens,
		row.GoalTokens, row.MinimumTokens, row.SafeTokens, row.SendTokens, row.InputReservedTokens,
		row.PreparedAt, row.ExpiresAt, row.ConsumedAt)
	return err
}

func insertModelBudgetPreparationLedger(ctx context.Context, tx pgx.Tx, ownerID, taskID, callID, state string,
	settledAt *time.Time, inputTokens, outputTokens *int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,input_tokens,output_tokens,settled_at)
VALUES ($1,'task',$2,decode(repeat('11',32),'hex'),'p1234567','deepseek-flash',$3,10000,$4,$5,$6,$7)`,
		ownerID, callID, taskID, state, inputTokens, outputTokens, settledAt)
	return err
}

func expectModelBudgetPreparationConstraintFailure(ctx context.Context, pool *pgxpool.Pool,
	insert func(pgx.Tx, string) error, message string) error {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("%s: generate test identity: %w", message, err)
	}
	callID := "mdl_bad_" + hex.EncodeToString(random[:])
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin test transaction: %w", message, err)
	}
	if err := insert(tx, callID); err == nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("%s: expected a PostgreSQL constraint error", message)
	}
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("%s: rollback failed test row: %w", message, err)
	}
	return nil
}

func bytesOf(size int, value byte) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}
