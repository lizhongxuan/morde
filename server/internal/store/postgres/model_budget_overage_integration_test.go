package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

func TestPostgresAdaptiveModelOverageFreezesAndRecordsKnownDifference(t *testing.T) {
	fixture, preparation := taskModelBudgetPreparationFixture(t)
	ctx := context.Background()
	store := fixture.store
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare adaptive Task model budget: %v", err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("begin adaptive Task model dispatch: %v", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: preparation.InputTokenReservation + 5, OutputTokens: plan.MaxOutputTokens + 1}
	if err := store.RecordTaskModelBudgetOverage(ctx, reservation, usage); err != nil {
		t.Fatalf("record known adaptive model overage: %v", err)
	}
	if err := store.RecordTaskModelBudgetOverage(ctx, reservation, usage); err != nil {
		t.Fatalf("replay identical adaptive model overage: %v", err)
	}
	if err := store.RecordTaskModelBudgetOverage(ctx, reservation, app.ModelUsage{Known: true, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens + 1}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("replay changed provider usage = %v, want idempotency conflict", err)
	}

	var state, reason string
	var inputOverage, outputOverage int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.state,overage.reason_code,overage.input_overage_tokens,overage.output_overage_tokens
FROM model_call_ledger ledger JOIN model_budget_overage_reconciliations overage
 ON overage.owner_id=ledger.owner_id AND overage.source=ledger.source AND overage.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, reservation.OwnerID, reservation.CallID).
		Scan(&state, &reason, &inputOverage, &outputOverage); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || reason != "input_and_output_exceeded" || inputOverage != 5 || outputOverage != 1 {
		t.Fatalf("overage freeze/accounting = state %q reason %q deltas %d/%d", state, reason, inputOverage, outputOverage)
	}
	var taskUsage *int64
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, reservation.Task.TaskID).Scan(&taskUsage); err != nil || taskUsage != nil {
		t.Fatalf("Task usage after overage = %v, %v; want unknown", taskUsage, err)
	}
}

func TestPostgresAdaptiveProjectModelOverageRecordsTokenAndMoneyDifference(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "budget overage", 300, 300)
	ctx := context.Background()
	store := fixture.store
	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	preparation, _ := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, fence,
		"plan-budget-overage", "budget-call-overage", rate)
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare project adaptive model budget: %v", err)
	}
	if plan.MaxOutputTokens != 250 {
		t.Fatalf("project output reservation = %d, want 250 tokens", plan.MaxOutputTokens)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("begin project adaptive model dispatch: %v", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: preparation.InputTokenReservation + 5, OutputTokens: plan.MaxOutputTokens + 1}
	if err := store.RecordTaskModelBudgetOverage(ctx, reservation, usage); err != nil {
		t.Fatalf("record known project adaptive model overage: %v", err)
	}

	var accountState, projectState, currency, reason string
	var inputOverage, outputOverage, reservedMicros, actualMicros, moneyOverage int64
	if err := store.pool.QueryRow(ctx, `SELECT account.state,project.status,overage.reason_code,overage.input_overage_tokens,
overage.output_overage_tokens,overage.currency,overage.reserved_micros,overage.actual_micros,overage.money_overage_micros
FROM model_call_ledger account
JOIN project_budget_ledger project ON project.project_id=$3 AND project.source=account.source AND project.call_id=account.call_id
JOIN model_budget_overage_reconciliations overage ON overage.owner_id=account.owner_id AND overage.source=account.source AND overage.call_id=account.call_id
WHERE account.owner_id=$1 AND account.source='task' AND account.call_id=$2`,
		reservation.OwnerID, reservation.CallID, projectID).Scan(&accountState, &projectState, &reason, &inputOverage,
		&outputOverage, &currency, &reservedMicros, &actualMicros, &moneyOverage); err != nil {
		t.Fatal(err)
	}
	if accountState != "unknown" || projectState != "unknown" || reason != "input_and_output_exceeded" ||
		inputOverage != 5 || outputOverage != 1 || currency != "USD" || reservedMicros != 300 || actualMicros != 306 || moneyOverage != 6 {
		t.Fatalf("project overage freeze/accounting = account:%q project:%q reason:%q token delta:%d/%d money:%s %d/%d (+%d)",
			accountState, projectState, reason, inputOverage, outputOverage, currency, reservedMicros, actualMicros, moneyOverage)
	}
}

func TestPostgresAdaptiveProjectModelOverageReconcilesVerifiedActualUsage(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "budget overage reconciliation", 300, 300)
	ctx := context.Background()
	store := fixture.store
	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	preparation, _ := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, fence,
		"plan-budget-overage-reconcile", "budget-call-overage-reconcile", rate)
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare project adaptive model budget: %v", err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("begin project adaptive model dispatch: %v", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: preparation.InputTokenReservation + 5, OutputTokens: plan.MaxOutputTokens + 1}
	if err := store.RecordTaskModelBudgetOverage(ctx, reservation, usage); err != nil {
		t.Fatalf("record provider-reported adaptive overage: %v", err)
	}

	adminUser := createTestUser(t, store, 3, 10_000, 20_000)
	if _, err := store.pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, adminUser.ID); err != nil {
		t.Fatal(err)
	}
	adminToken := createTestCredentials(t, store, adminUser.ID)[0]
	admin := identity.Principal{UserID: adminUser.ID, TokenID: adminToken, Kind: identity.KindSession, Role: identity.RoleAdmin}
	service, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.ListUnknown(ctx, admin, fixture.ownerID, 10)
	if err != nil || len(items) != 1 || items[0].CallID != reservation.CallID || items[0].AdaptiveBudgetOverage == nil {
		t.Fatalf("admin unknown usage overage details = %#v, err=%v", items, err)
	}
	overage := items[0].AdaptiveBudgetOverage
	if overage.Status != "open" || overage.ReasonCode != "input_and_output_exceeded" ||
		overage.ReservedInputTokens != 50 || overage.ReservedOutputTokens != plan.MaxOutputTokens ||
		overage.ReportedInputTokens != usage.InputTokens || overage.ReportedOutputTokens != usage.OutputTokens ||
		overage.InputOverageTokens != 5 || overage.OutputOverageTokens != 1 || overage.Currency == nil || *overage.Currency != "USD" ||
		overage.ReservedMicros == nil || *overage.ReservedMicros != 300 || overage.ReportedMicros == nil || *overage.ReportedMicros != 306 ||
		overage.MoneyOverageMicros == nil || *overage.MoneyOverageMicros != 6 {
		t.Fatalf("admin overage view = %+v", overage)
	}

	command := app.ModelUsageReconcileCommand{OwnerID: fixture.ownerID, Source: app.ModelCallTask, CallID: reservation.CallID,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, Evidence: "provider invoice usage record #budget-123", IP: "127.0.0.1"}
	replayed, err := service.Reconcile(ctx, admin, command)
	if err != nil || replayed {
		t.Fatalf("reconcile provider-confirmed adaptive overage: replayed=%t err=%v", replayed, err)
	}
	var accountState, projectState, status, resolvedBy, evidence string
	var projectInput, projectOutput, projectMicros, verifiedInput, verifiedOutput, verifiedMicros *int64
	var taskUsage *int64
	if err := store.pool.QueryRow(ctx, `SELECT account.state,project.status,project.actual_input_tokens,project.actual_output_tokens,
project.actual_micros,task.usage_model_tokens,overage.reconciliation_status,overage.resolved_by,overage.resolution_evidence,
overage.verified_input_tokens,overage.verified_output_tokens,overage.verified_micros
FROM model_call_ledger account JOIN project_budget_ledger project
 ON project.project_id=$3 AND project.source=account.source AND project.call_id=account.call_id
JOIN tasks task ON task.id=account.task_id
JOIN model_budget_overage_reconciliations overage
 ON overage.owner_id=account.owner_id AND overage.source=account.source AND overage.call_id=account.call_id
WHERE account.owner_id=$1 AND account.source='task' AND account.call_id=$2`,
		fixture.ownerID, reservation.CallID, projectID).Scan(&accountState, &projectState, &projectInput, &projectOutput, &projectMicros,
		&taskUsage, &status, &resolvedBy, &evidence, &verifiedInput, &verifiedOutput, &verifiedMicros); err != nil {
		t.Fatal(err)
	}
	if accountState != "settled" || projectState != "settled" || projectInput == nil || *projectInput != usage.InputTokens ||
		projectOutput == nil || *projectOutput != usage.OutputTokens || projectMicros == nil || *projectMicros != 306 ||
		taskUsage == nil || *taskUsage != usage.InputTokens+usage.OutputTokens || status != "resolved" || resolvedBy != admin.UserID ||
		evidence != command.Evidence || verifiedInput == nil || *verifiedInput != usage.InputTokens || verifiedOutput == nil ||
		*verifiedOutput != usage.OutputTokens || verifiedMicros == nil || *verifiedMicros != 306 {
		t.Fatalf("resolved overage state: account=%q project=%q actual=%v/%v/%v Task=%v status=%q actor=%q evidence=%q verified=%v/%v/%v",
			accountState, projectState, projectInput, projectOutput, projectMicros, taskUsage, status, resolvedBy, evidence,
			verifiedInput, verifiedOutput, verifiedMicros)
	}
	items, err = service.ListUnknown(ctx, admin, fixture.ownerID, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("resolved overage remained in unknown usage list: %#v err=%v", items, err)
	}
	budget, err := service.Budget(ctx, admin, fixture.ownerID)
	if err != nil || budget.HasUnknown || budget.UsedTokens != usage.InputTokens+usage.OutputTokens || budget.TokenLimit != 300 {
		t.Fatalf("resolved overage changed account budget: %+v err=%v", budget, err)
	}
	replayed, err = service.Reconcile(ctx, admin, command)
	if err != nil || !replayed {
		t.Fatalf("replay identical verified overage = replayed %t err=%v", replayed, err)
	}
	changed := command
	changed.Evidence += " changed"
	if _, err := service.Reconcile(ctx, admin, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed overage evidence replay = %v, want idempotency conflict", err)
	}
}

func TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "budget no dispatch", 300, 300)
	ctx := context.Background()
	store := fixture.store
	rate := project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	preparation, _ := projectBudgetPreparationForFence(t, store, fixture.ownerID, projectID, fence,
		"plan-budget-no-dispatch", "budget-call-no-dispatch", rate)
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare project adaptive model budget: %v", err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("consume prepared plan before confirmed pre-provider cancellation: %v", err)
	}
	// The caller has not started the HTTP request, so abandoning this consumed
	// plan is safe and must remove every account/project hold atomically.
	if err := store.AbandonModelCallReservation(ctx, reservation); err != nil {
		t.Fatalf("release confirmed-not-dispatched adaptive reservation: %v", err)
	}
	var accountRows, planRows, projectRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&planRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1 AND source='task' AND call_id=$2`,
		projectID, reservation.CallID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 || planRows != 0 || projectRows != 0 {
		t.Fatalf("confirmed no-dispatch holds remain: account=%d plan=%d project=%d", accountRows, planRows, projectRows)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err == nil {
		t.Fatal("released adaptive model call dispatched on replay")
	}
}

func TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay(t *testing.T) {
	fixture, preparation := taskModelBudgetPreparationFixture(t)
	ctx := context.Background()
	store := fixture.store
	plan, err := store.PrepareTaskModelBudget(ctx, preparation)
	if err != nil {
		t.Fatalf("prepare adaptive Task model budget: %v", err)
	}
	reservation := finalizeTestModelBudgetReservation(t, preparation, plan)
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("begin adaptive Task model dispatch: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '13 minutes'
WHERE owner_id=$1 AND source='task' AND call_id=$2`, reservation.OwnerID, reservation.CallID); err != nil {
		t.Fatal(err)
	}
	marked, err := store.MarkStaleModelCallsUnknown(ctx)
	if err != nil || marked != 1 {
		t.Fatalf("stale adaptive dispatch recovery = marked %d, %v; want one unknown call", marked, err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		reservation.OwnerID, reservation.CallID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("stale adaptive ledger state = %q, %v; want unknown", state, err)
	}
	var usage *int64
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, reservation.Task.TaskID).Scan(&usage); err != nil || usage != nil {
		t.Fatalf("Task usage after unknown adaptive dispatch = %v, %v; want unknown", usage, err)
	}
	if released, err := store.CleanupExpiredTaskModelBudgetPreparations(ctx, 10); err != nil || released != 0 {
		t.Fatalf("cleanup unknown adaptive dispatch = released %d, %v; want zero", released, err)
	}
	if _, err := store.PrepareTaskModelBudget(ctx, preparation); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new prepare while prior adaptive call is unknown = %v, want fail closed", err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err == nil {
		t.Fatal("unknown adaptive model dispatch was replayed")
	}
}
