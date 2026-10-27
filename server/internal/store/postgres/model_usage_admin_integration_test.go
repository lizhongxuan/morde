package postgres

import (
	"context"
	"math"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func modelUsageAdminFixture(t *testing.T) (*Store, *app.ModelUsageAdminService, identity.Principal, identity.Principal, app.ModelCallReservation) {
	t.Helper()
	store, owner, reservation := modelUsageFixture(t)
	admin := createTestUser(t, store, 3, 10_000, 20_000)
	if _, err := store.pool.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	adminToken := createTestCredentials(t, store, admin.ID)[0]
	principal := identity.Principal{UserID: admin.ID, TokenID: adminToken, Kind: identity.KindSession, Role: identity.RoleAdmin}
	service, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, service, principal, owner, reservation
}

func TestPostgresModelUsageAdminConcurrentReconcileAndAudit(t *testing.T) {
	store, service, admin, owner, reservation := modelUsageAdminFixture(t)
	ctx := context.Background()
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: owner.UserID, Source: reservation.Source,
		CallID: reservation.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	items, err := service.ListUnknown(ctx, admin, owner.UserID, 10)
	if err != nil || len(items) != 1 || items[0].CallID != reservation.CallID || items[0].UnknownAt.IsZero() {
		t.Fatalf("unknown calls = %#v, %v", items, err)
	}
	command := app.ModelUsageReconcileCommand{OwnerID: owner.UserID, Source: reservation.Source, CallID: reservation.CallID,
		InputTokens: 12, OutputTokens: 8, Evidence: "provider reference abc123", IP: "127.0.0.1"}
	type result struct {
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replayed, err := service.Reconcile(ctx, admin, command)
			results <- result{replayed: replayed, err: err}
		}()
	}
	wg.Wait()
	close(results)
	commits, replays := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent reconciliation: %v", result.err)
		}
		if result.replayed {
			replays++
		} else {
			commits++
		}
	}
	if commits != 1 || replays != 1 {
		t.Fatalf("commits=%d replays=%d", commits, replays)
	}
	var count, tokens int64
	var actor, evidence string
	err = store.pool.QueryRow(ctx, `SELECT count(*),min(actor_id),min(detail->>'evidence'),
 min((detail->>'input_count')::bigint+(detail->>'output_count')::bigint) FROM audit_logs WHERE action=$1`,
		string(audit.ActionModelUsageReconcile)).Scan(&count, &actor, &evidence, &tokens)
	if err != nil || count != 1 || actor != admin.UserID || evidence != command.Evidence || tokens != 20 {
		t.Fatalf("reconciliation audit = count %d actor %q evidence %q tokens %d: %v", count, actor, evidence, tokens, err)
	}
	changed := command
	changed.OutputTokens++
	if _, err := service.Reconcile(ctx, admin, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed replay = %v", err)
	}
	reservation.CallID = "next-model-call"
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatalf("settled unknown still blocks budget: %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: owner.UserID, Source: reservation.Source,
		CallID: reservation.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	command.CallID = reservation.CallID
	competing := command
	competing.InputTokens++
	results = make(chan result, 2)
	for _, value := range []app.ModelUsageReconcileCommand{command, competing} {
		wg.Add(1)
		go func(value app.ModelUsageReconcileCommand) {
			defer wg.Done()
			replayed, err := service.Reconcile(ctx, admin, value)
			results <- result{replayed: replayed, err: err}
		}(value)
	}
	wg.Wait()
	close(results)
	commits, conflicts := 0, 0
	for result := range results {
		if result.err == nil && !result.replayed {
			commits++
		} else if domain.CodeOf(result.err) == domain.CodeIdempotencyConflict {
			conflicts++
		} else {
			t.Fatalf("competing reconciliation = %#v", result)
		}
	}
	if commits != 1 || conflicts != 1 {
		t.Fatalf("competing submissions: commits=%d conflicts=%d", commits, conflicts)
	}
}

func TestPostgresModelUsageAdminRebuildsTaskOnlyAfterAllUnknownCalls(t *testing.T) {
	store, service, admin, owner, _ := modelUsageAdminFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'test')`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,execution_location)
 VALUES ('q1234567',$1,'c1234567','p1234567','test','queued','client',$2,$3,600,10,500,NULL,'server')`,
		owner.UserID, owner.TokenID, owner.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,
 reserved_tokens,state,input_tokens,output_tokens,settled_at) VALUES
 ($1,'task','settled-model-1',decode(repeat('01',32),'hex'),'p1234567','model','q1234567',100,'settled',5,2,now()),
 ($1,'task','unknown-model-1',decode(repeat('02',32),'hex'),'p1234567','model','q1234567',100,'unknown',NULL,NULL,now()),
 ($1,'task','unknown-model-2',decode(repeat('03',32),'hex'),'p1234567','model','q1234567',100,'unknown',NULL,NULL,now())`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	command := app.ModelUsageReconcileCommand{OwnerID: owner.UserID, Source: app.ModelCallTask, CallID: "unknown-model-1",
		InputTokens: 8, OutputTokens: 4, Evidence: "provider reference one"}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT reject_model_reconcile_for_test CHECK (action <> 'model.usage.reconcile') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, admin, command); err == nil {
		t.Fatal("task reconciliation succeeded without its audit record")
	}
	var usage *int64
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT t.usage_model_tokens,l.state FROM tasks t JOIN model_call_ledger l ON l.task_id=t.id
 WHERE t.id='q1234567' AND l.call_id='unknown-model-1'`).Scan(&usage, &state); err != nil || usage != nil || state != "unknown" {
		t.Fatalf("task and ledger after audit failure = %v, %q, %v", usage, state, err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs DROP CONSTRAINT reject_model_reconcile_for_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, admin, command); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&usage); err != nil || usage != nil {
		t.Fatalf("usage before all reconciliation = %v, %v", usage, err)
	}
	command.CallID, command.InputTokens, command.OutputTokens, command.Evidence = "unknown-model-2", 3, 1, "provider reference two"
	if _, err := service.Reconcile(ctx, admin, command); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&usage); err != nil || usage == nil || *usage != 23 {
		t.Fatalf("rebuilt task usage = %v, %v", usage, err)
	}
}

func TestPostgresModelUsageAdminRejectsStaleAuthorityAndRollsBackWithoutAudit(t *testing.T) {
	store, service, admin, owner, reservation := modelUsageAdminFixture(t)
	ctx := context.Background()
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	command := app.ModelUsageReconcileCommand{OwnerID: owner.UserID, Source: reservation.Source, CallID: reservation.CallID,
		InputTokens: 1, OutputTokens: 2, Evidence: "provider reference 42"}
	if _, err := service.Reconcile(ctx, admin, command); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("active dispatched call = %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: owner.UserID, Source: reservation.Source,
		CallID: reservation.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,
 reserved_tokens,state,input_tokens,output_tokens,settled_at)
 VALUES ($1,'client','previous-model-call',decode(repeat('01',32),'hex'),'p1234567','model',1,'settled',1,0,now())`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	overflow := command
	overflow.InputTokens, overflow.OutputTokens = math.MaxInt64, 0
	if _, err := service.Reconcile(ctx, admin, overflow); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("account usage overflow = %v", err)
	}
	other := createTestUser(t, store, 3, 10_000, 20_000)
	otherSession := createTestCredentials(t, store, other.ID)[0]
	stale := identity.Principal{UserID: other.ID, TokenID: otherSession, Kind: identity.KindSession, Role: identity.RoleAdmin}
	if _, err := service.Reconcile(ctx, stale, command); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("stale administrator role = %v", err)
	}
	if _, err := service.ListUnknown(ctx, stale, "", 10); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("stale administrator list = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET client_installation_id=(SELECT id FROM client_installations WHERE owner_id=$2 LIMIT 1) WHERE id=$1`, admin.TokenID, admin.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, admin, command); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("installation-bound administrator session = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET client_installation_id=NULL WHERE id=$1`, admin.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=now() WHERE id=$1`, admin.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, admin, command); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revoked administrator session = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, admin.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT reject_model_reconcile_for_test CHECK (action <> 'model.usage.reconcile') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, admin, command); err == nil {
		t.Fatal("reconciliation succeeded without its audit record")
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3`,
		owner.UserID, string(reservation.Source), reservation.CallID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("ledger after audit failure = %q, %v", state, err)
	}
}

func TestPostgresModelUsageAdminBudgetConfigurationAndUsage(t *testing.T) {
	store, service, admin, owner, reservation := modelUsageAdminFixture(t)
	ctx := context.Background()

	budget, err := service.Budget(ctx, admin, owner.UserID)
	if err != nil || !budget.Configured || budget.TokenLimit != 1000 || budget.MaxConcurrent != 3 || budget.UsedTokens != 0 {
		t.Fatalf("initial budget = %#v, %v", budget, err)
	}
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	budget, err = service.Budget(ctx, admin, owner.UserID)
	if err != nil || budget.UsedTokens != 100 || budget.InFlight != 1 || budget.HasUnknown {
		t.Fatalf("reserved budget = %#v, %v", budget, err)
	}
	updated, err := service.SetBudget(ctx, admin, app.ModelAccountBudgetUpdate{
		OwnerID: owner.UserID, TokenLimit: 1000, MaxConcurrent: 1, IP: "127.0.0.1",
	})
	if err != nil || updated.MaxConcurrent != 1 || updated.InFlight != 1 {
		t.Fatalf("lower concurrency during in-flight call = %#v, %v", updated, err)
	}
	second := reservation
	second.CallID = "test-call-0002"
	if err := store.ReserveModelCall(ctx, second); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("concurrency limit = %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: owner.UserID, Source: reservation.Source,
		CallID: reservation.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 8}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: owner.UserID, Source: second.Source,
		CallID: second.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	budget, err = service.Budget(ctx, admin, owner.UserID)
	if err != nil || budget.UsedTokens != 120 || budget.InFlight != 0 || !budget.HasUnknown {
		t.Fatalf("unknown usage budget = %#v, %v", budget, err)
	}
	if _, err := service.Reconcile(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: owner.UserID, Source: second.Source,
		CallID: second.CallID, InputTokens: 10, OutputTokens: 5, Evidence: "provider usage record 2"}); err != nil {
		t.Fatal(err)
	}
	updated, err = service.SetBudget(ctx, admin, app.ModelAccountBudgetUpdate{
		OwnerID: owner.UserID, TokenLimit: 30, MaxConcurrent: 2, IP: "127.0.0.1",
	})
	if err != nil || updated.UsedTokens != 35 || updated.HasUnknown {
		t.Fatalf("budget below current use = %#v, %v", updated, err)
	}
	third := reservation
	third.CallID = "test-call-0003"
	if err := store.ReserveModelCall(ctx, third); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("lower token limit did not block new reservation: %v", err)
	}
	var audits int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action=$2`,
		admin.UserID, string(audit.ActionModelBudgetUpdate)).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("budget audit count = %d, %v", audits, err)
	}
}

func TestPostgresModelUsageAdminConcurrentFirstBudgetAudit(t *testing.T) {
	store, service, admin, owner, _ := modelUsageAdminFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `DELETE FROM model_account_budgets WHERE owner_id=$1`, owner.UserID); err != nil {
		t.Fatal(err)
	}

	commands := []app.ModelAccountBudgetUpdate{
		{OwnerID: owner.UserID, TokenLimit: 5000, MaxConcurrent: 2, IP: "127.0.0.1"},
		{OwnerID: owner.UserID, TokenLimit: 8000, MaxConcurrent: 3, IP: "127.0.0.1"},
	}
	results := make(chan error, len(commands))
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func(command app.ModelAccountBudgetUpdate) {
			defer wg.Done()
			_, err := service.SetBudget(ctx, admin, command)
			results <- err
		}(command)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent first budget update: %v", err)
		}
	}

	var firstConfiguration, subsequentConfiguration int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE detail->>'configured'='false'),
 count(*) FILTER (WHERE detail->>'configured'='true') FROM audit_logs WHERE actor_id=$1 AND action=$2 AND target=$3`,
		admin.UserID, string(audit.ActionModelBudgetUpdate), owner.UserID).Scan(&firstConfiguration, &subsequentConfiguration); err != nil {
		t.Fatal(err)
	}
	if firstConfiguration != 1 || subsequentConfiguration != 1 {
		t.Fatalf("concurrent budget audit first=%d subsequent=%d", firstConfiguration, subsequentConfiguration)
	}
	var previousLimit int64
	if err := store.pool.QueryRow(ctx, `SELECT (detail->>'previous_token_limit')::bigint FROM audit_logs
 WHERE actor_id=$1 AND action=$2 AND target=$3 AND detail->>'configured'='true'`,
		admin.UserID, string(audit.ActionModelBudgetUpdate), owner.UserID).Scan(&previousLimit); err != nil {
		t.Fatal(err)
	}
	if previousLimit != commands[0].TokenLimit && previousLimit != commands[1].TokenLimit {
		t.Fatalf("second budget audit previous limit = %d", previousLimit)
	}
}
