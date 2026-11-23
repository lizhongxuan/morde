package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

func TestPostgresTaskModelUnknownUsageStaysReservedAcrossRecoveryAndReconcilesOnce(t *testing.T) {
	ctx := context.Background()
	store, gateway, model, owner, modelID := modelProbeFixture(t, app.ModelUsage{})
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'task model recovery')`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("task model recovery checkpoint"))
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,
 lease_owner,lease_version,lease_expires_at,execution_location,auth_version,auth_expires_at,agent_instance_id,
 agent_generation,agent_session_id,agent_engine_version,agent_profile_version,checkpoint_through_seq,checkpoint_digest)
 VALUES ('q1234567',$1,$1,'c1234567',$2,'task model recovery','running','session',$3,NULL,600,10,500,0,
 'worker-1',3,clock_timestamp()+interval '1 hour','server',7,clock_timestamp()+interval '1 hour',
 'agent-1',2,'session-1','engine','profile',0,$4)`, owner.UserID, modelID, owner.TokenID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ('q1234567',$1,'session-1',0,$2)`, owner.UserID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ('q1234567',$1,'agent-1',2,'session-1','engine','profile','supervisor-1',$2,'worker-1',3,7,30000,
 clock_timestamp()+interval '30 seconds','active')`, owner.UserID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ('q1234567',$1,'agent-1',2,'session-1','session-1',0,0,$2,decode('01','hex'),'engine','profile',7,3)`, owner.UserID, digest[:]); err != nil {
		t.Fatal(err)
	}

	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: owner.UserID, InitiatorID: owner.UserID,
		LeaseOwner: "worker-1", LeaseVersion: 3, AgentInstanceID: "agent-1", AgentGeneration: 2,
		AgentSessionID: "session-1", AgentRootSessionID: "session-1", AuthorizationVersion: 7,
		ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"provider":"morde","model":"` + modelID +
		`","messages":[{"role":"user","content":[{"type":"text","text":"Produce one synthetic answer."}]}],"max_tokens":32}`)
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: modelID, CallID: callID, RawRequest: raw}

	if _, err := gateway.Complete(ctx, call); err == nil {
		t.Fatal("Task model call with unknown provider usage unexpectedly succeeded")
	} else if model.calls != 1 {
		t.Fatalf("Task model call stopped before synthetic Provider dispatch: provider calls=%d err=%v", model.calls, err)
	}
	if model.calls != 1 {
		t.Fatalf("synthetic provider calls after unknown usage = %d, want 1", model.calls)
	}
	var ledgerState string
	var reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT state,reserved_tokens FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		owner.UserID, callID).Scan(&ledgerState, &reserved); err != nil || ledgerState != "unknown" || reserved <= 0 {
		t.Fatalf("Task model ledger state=%q reserved=%d err=%v", ledgerState, reserved, err)
	}
	adminUser := createTestUser(t, store, 3, 10_000, 20_000)
	if _, err := store.pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, adminUser.ID); err != nil {
		t.Fatal(err)
	}
	adminToken := createTestCredentials(t, store, adminUser.ID)[0]
	admin := identity.Principal{UserID: adminUser.ID, TokenID: adminToken, Kind: identity.KindSession, Role: identity.RoleAdmin}
	adminService, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := adminService.Budget(ctx, admin, owner.UserID)
	if err != nil || !budget.HasUnknown || budget.UsedTokens != 0 {
		t.Fatalf("budget while Task model usage is unknown = %+v err=%v", budget, err)
	}
	if _, err := gateway.Complete(ctx, call); err == nil || model.calls != 1 {
		t.Fatalf("checkpoint recovery redispatched unknown Task call: provider calls=%d err=%v", model.calls, err)
	}

	unknown, err := adminService.ListUnknown(ctx, admin, owner.UserID, 10)
	if err != nil || len(unknown) != 1 || unknown[0].Source != app.ModelCallTask || unknown[0].CallID != callID {
		t.Fatalf("unknown Task model calls = %+v err=%v", unknown, err)
	}
	command := app.ModelUsageReconcileCommand{OwnerID: owner.UserID, Source: app.ModelCallTask, CallID: callID,
		InputTokens: 9, OutputTokens: 4, Evidence: "synthetic provider usage record"}
	if replayed, err := adminService.Reconcile(ctx, admin, command); err != nil || replayed {
		t.Fatalf("reconcile Task usage replayed=%t err=%v", replayed, err)
	}
	if replayed, err := adminService.Reconcile(ctx, admin, command); err != nil || !replayed {
		t.Fatalf("repeat Task usage reconciliation replayed=%t err=%v", replayed, err)
	}
	budget, err = adminService.Budget(ctx, admin, owner.UserID)
	if err != nil || budget.HasUnknown || budget.InFlight != 0 || budget.UsedTokens != 13 {
		t.Fatalf("budget after Task usage reconciliation = %+v err=%v", budget, err)
	}
	var taskUsage int64
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&taskUsage); err != nil || taskUsage != 13 {
		t.Fatalf("reconciled Task model usage=%d err=%v", taskUsage, err)
	}
	if _, err := gateway.Complete(ctx, call); err == nil || model.calls != 1 {
		t.Fatalf("reconciled Task call without a stored response was redispatched: provider calls=%d err=%v", model.calls, err)
	}
	var responseCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		owner.UserID, callID).Scan(&responseCount); err != nil || responseCount != 0 {
		t.Fatalf("unknown provider response was fabricated during reconciliation: rows=%d err=%v", responseCount, err)
	}
}
