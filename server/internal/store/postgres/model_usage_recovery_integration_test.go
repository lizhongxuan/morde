package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
)

func TestPostgresStaleModelCallRecovery(t *testing.T) {
	store, adminService, admin, owner, reservation := modelUsageAdminFixture(t)
	ctx := context.Background()
	stale := reservation
	stale.CallID = "stale-client-call"
	staleDigest := sha256.Sum256([]byte(stale.CallID))
	stale.RequestDigest = staleDigest[:]
	if err := store.ReserveModelCall(ctx, stale); err != nil {
		t.Fatal(err)
	}
	fresh := reservation
	fresh.CallID = "fresh-client-call"
	freshDigest := sha256.Sum256([]byte(fresh.CallID))
	fresh.RequestDigest = freshDigest[:]
	if err := store.ReserveModelCall(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, owner.UserID, stale.Source, stale.CallID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'test')`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,execution_location)
 VALUES ('q1234567',$1,'c1234567','p1234567','test','queued','client',$2,$3,600,10,500,99,'server')`,
		owner.UserID, owner.TokenID, owner.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,
 reserved_tokens,state,created_at) VALUES ($1,'task','stale-task-call',decode(repeat('03',32),'hex'),'p1234567','test-model',
 'q1234567',100,'dispatched',clock_timestamp()-interval '3 minutes')`, owner.UserID); err != nil {
		t.Fatal(err)
	}

	marked, err := store.MarkStaleModelCallsUnknown(ctx)
	if err != nil || marked != 2 {
		t.Fatalf("MarkStaleModelCallsUnknown() = %d, %v", marked, err)
	}
	if err := store.BeginModelCallDispatch(ctx, stale); err == nil {
		t.Fatal("recovered stale call started a provider request")
	}
	var staleState, freshState, taskState string
	var staleSettled, freshSettled *string
	if err := store.pool.QueryRow(ctx, `SELECT state,settled_at::text FROM model_call_ledger WHERE owner_id=$1 AND source='client' AND call_id=$2`,
		owner.UserID, stale.CallID).Scan(&staleState, &staleSettled); err != nil || staleState != "unknown" || staleSettled == nil {
		t.Fatalf("stale client call = %q, %v, %v", staleState, staleSettled, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state,settled_at::text FROM model_call_ledger WHERE owner_id=$1 AND source='client' AND call_id=$2`,
		owner.UserID, fresh.CallID).Scan(&freshState, &freshSettled); err != nil || freshState != "dispatched" || freshSettled != nil {
		t.Fatalf("fresh client call = %q, %v, %v", freshState, freshSettled, err)
	}
	var taskUsage *int64
	if err := store.pool.QueryRow(ctx, `SELECT l.state,t.usage_model_tokens FROM model_call_ledger l JOIN tasks t ON t.id=l.task_id
 WHERE l.owner_id=$1 AND l.source='task' AND l.call_id='stale-task-call'`, owner.UserID).Scan(&taskState, &taskUsage); err != nil || taskState != "unknown" || taskUsage != nil {
		t.Fatalf("stale task call = %q, usage %v, %v", taskState, taskUsage, err)
	}

	unknown, err := adminService.ListUnknown(ctx, admin, owner.UserID, 10)
	if err != nil || len(unknown) != 2 {
		t.Fatalf("unknown calls = %#v, %v", unknown, err)
	}
	if _, err := adminService.Reconcile(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: owner.UserID,
		Source: app.ModelCallClient, CallID: stale.CallID, InputTokens: 10, OutputTokens: 5, Evidence: "provider reference client"}); err != nil {
		t.Fatal(err)
	}
	if _, err := adminService.Reconcile(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: owner.UserID,
		Source: app.ModelCallTask, CallID: "stale-task-call", InputTokens: 12, OutputTokens: 8, Evidence: "provider reference task"}); err != nil {
		t.Fatal(err)
	}
	budget, err := adminService.Budget(ctx, admin, owner.UserID)
	if err != nil || budget.InFlight != 1 || budget.HasUnknown || budget.UsedTokens != 135 {
		t.Fatalf("budget after reconciliation = %#v, %v", budget, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&taskUsage); err != nil || taskUsage == nil || *taskUsage != 20 {
		t.Fatalf("reconciled task usage = %v, %v", taskUsage, err)
	}
}

func TestPostgresModelCallDispatchRefreshesRecoveryAge(t *testing.T) {
	store, _, reservation := modelUsageFixture(t)
	ctx := context.Background()
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, reservation.OwnerID, reservation.Source, reservation.CallID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	marked, err := store.MarkStaleModelCallsUnknown(ctx)
	if err != nil || marked != 0 {
		t.Fatalf("fresh dispatch was recovered: marked=%d err=%v", marked, err)
	}
	var state string
	var ageSeconds float64
	if err := store.pool.QueryRow(ctx, `SELECT state,extract(epoch FROM clock_timestamp()-created_at)
FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3`, reservation.OwnerID, reservation.Source, reservation.CallID).
		Scan(&state, &ageSeconds); err != nil || state != "dispatched" || ageSeconds < 0 || ageSeconds > 2 {
		t.Fatalf("dispatch timestamp = %q, age %.3fs, err=%v", state, ageSeconds, err)
	}
}

func TestPostgresLateKnownModelUsageSettlesUnknownCall(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	reservation.CallID = "late-client-call"
	lateDigest := sha256.Sum256([]byte(reservation.CallID))
	reservation.RequestDigest = lateDigest[:]
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'test')`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,execution_location)
 VALUES ('q1234567',$1,'c1234567','p1234567','test','queued','client',$2,$3,600,10,500,99,'server')`,
		principal.UserID, principal.TokenID, principal.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,
 reserved_tokens,state,created_at) VALUES ($1,'task','late-task-call',decode(repeat('04',32),'hex'),'p1234567','test-model',
 'q1234567',100,'dispatched',clock_timestamp()-interval '3 minutes')`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source='client' AND call_id=$2`, principal.UserID, reservation.CallID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(ctx); err != nil || marked != 2 {
		t.Fatalf("MarkStaleModelCallsUnknown() = %d, %v", marked, err)
	}
	for _, settlement := range []app.ModelCallSettlement{
		{Source: reservation.Source, OwnerID: principal.UserID, CallID: reservation.CallID,
			Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 6}},
		{Source: app.ModelCallTask, OwnerID: principal.UserID, CallID: "late-task-call",
			Usage: app.ModelUsage{Known: true, InputTokens: 7, OutputTokens: 5}},
	} {
		if err := store.SettleModelCall(ctx, settlement); err != nil {
			t.Fatalf("late settle %s: %v", settlement.CallID, err)
		}
	}
	var unknown, inFlight int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='unknown'),count(*) FILTER (WHERE state='dispatched')
FROM model_call_ledger WHERE owner_id=$1`, principal.UserID).Scan(&unknown, &inFlight); err != nil || unknown != 0 || inFlight != 0 {
		t.Fatalf("late settlements left unknown=%d in_flight=%d, err=%v", unknown, inFlight, err)
	}
	var usage *int64
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&usage); err != nil || usage == nil || *usage != 12 {
		t.Fatalf("late task usage = %v, %v", usage, err)
	}
}
