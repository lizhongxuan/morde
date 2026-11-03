package postgres

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestPostgresFirstApprovalRequiresAndGrantsConfiguredModelBudget(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	approved := identity.StatusApproved
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &approved}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("approval without default budget = %v", err)
	}
	stored, err := store.GetUserByID(ctx, member.ID)
	var budgetRows, auditRows int
	if err != nil || stored.Status != identity.StatusPending {
		t.Fatalf("account after missing-default approval = %+v, %v", stored, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_account_budgets WHERE owner_id=$1`, member.ID).Scan(&budgetRows); err != nil || budgetRows != 0 {
		t.Fatalf("budget after missing-default approval = %d, %v", budgetRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='user.update' AND target=$1`, member.ID).Scan(&auditRows); err != nil || auditRows != 0 {
		t.Fatalf("audit after missing-default approval = %d, %v", auditRows, err)
	}

	defaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 45000, MaxConcurrent: 3}, "192.0.2.10"); err != nil {
		t.Fatalf("configure default model budget: %v", err)
	}
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &approved}); err != nil {
		t.Fatalf("approve registered account: %v", err)
	}
	assertModelBudget(t, ctx, pool, member.ID, 45000, 3)

	disabled := identity.StatusDisabled
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Status: &disabled}); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 1, TokenLimit: 90000, MaxConcurrent: 5}, "192.0.2.10"); err != nil {
		t.Fatalf("change default model budget: %v", err)
	}
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusDisabled, Status: &approved}); err != nil {
		t.Fatalf("restore account: %v", err)
	}
	assertModelBudget(t, ctx, pool, member.ID, 45000, 3)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='model.budget.update' AND target=$1`, member.ID).Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("first approval budget audits = %d, %v", auditRows, err)
	}
}

func TestPostgresRejectedAccountGetsDefaultModelBudgetOnFirstApproval(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 32000, MaxConcurrent: 2}, "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	rejected := identity.StatusRejected
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &rejected}); err != nil {
		t.Fatalf("reject account: %v", err)
	}
	var budgetRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_account_budgets WHERE owner_id=$1`, member.ID).Scan(&budgetRows); err != nil || budgetRows != 0 {
		t.Fatalf("rejected account budget = %d, %v", budgetRows, err)
	}
	approved := identity.StatusApproved
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusRejected, Status: &approved}); err != nil {
		t.Fatalf("approve previously rejected account: %v", err)
	}
	assertModelBudget(t, ctx, pool, member.ID, 32000, 2)
}

func TestPostgresConcurrentInitialApprovalGrantsOneBudget(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 50000, MaxConcurrent: 4}, "192.0.2.12"); err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	approved := identity.StatusApproved
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &approved})
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("concurrent approval = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("approval successes=%d conflicts=%d", successes, conflicts)
	}
	assertModelBudget(t, ctx, pool, member.ID, 50000, 4)
	var auditRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='model.budget.update' AND target=$1`, member.ID).Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("concurrent grant audits = %d, %v", auditRows, err)
	}
}

func TestPostgresFirstApprovalPreservesManualModelBudget(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 80000, MaxConcurrent: 5}, "192.0.2.13"); err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,700,1)`, member.ID); err != nil {
		t.Fatal(err)
	}
	approved := identity.StatusApproved
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &approved}); err != nil {
		t.Fatalf("approve manually-budgeted account: %v", err)
	}
	assertModelBudget(t, ctx, pool, member.ID, 700, 1)
	var grantAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='model.budget.update' AND target=$1`, member.ID).Scan(&grantAudits); err != nil || grantAudits != 0 {
		t.Fatalf("manual budget grant audits = %d, %v", grantAudits, err)
	}
}

func TestPostgresFirstApprovalBudgetWorksWithModelGatewaySubstitute(t *testing.T) {
	store, credentials, _, _ := clientLoginFixture(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	defaults, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 50000, MaxConcurrent: 1}, "192.0.2.14"); err != nil {
		t.Fatal(err)
	}
	member := pendingTestUser(t)
	password := "approved-model-password"
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	member.PasswordHash = string(passwordHash)
	if _, err := store.CreateUser(ctx, member); err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	approved := identity.StatusApproved
	if _, err := users.Update(ctx, admin, member.ID, app.UpdateUserCommand{ExpectedStatus: identity.StatusPending, Status: &approved}); err != nil {
		t.Fatalf("approve account: %v", err)
	}
	login, err := credentials.Login(ctx, member.Name, password, "192.0.2.14")
	if err != nil {
		t.Fatalf("login approved account: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatalf("verify approved account session: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	model := &meteredProbeModel{usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4}}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := models.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "approval model", BaseURL: "https://model.example/v1", ModelName: "test-model", APIKey: "synthetic-key", Shared: true,
	})
	if err != nil {
		t.Fatalf("create model connection: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := gateway.CheckModel(ctx, principal, connection.ID, "approval-budget-check-0001")
	if err != nil || !capabilities.Text || model.calls != 1 {
		t.Fatalf("model gateway result = %+v calls=%d err=%v", capabilities, model.calls, err)
	}
	assertModelBudget(t, ctx, store.pool, member.ID, 50000, 1)
	var state string
	var settledTokens int64
	if err := store.pool.QueryRow(ctx, `SELECT state,input_tokens+output_tokens FROM model_call_ledger WHERE owner_id=$1`, member.ID).
		Scan(&state, &settledTokens); err != nil || state != "settled" || settledTokens != 12 {
		t.Fatalf("approved account model usage = %q/%d err=%v", state, settledTokens, err)
	}
}

func assertModelBudget(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID string, wantLimit int64, wantConcurrent int32) {
	t.Helper()
	var tokenLimit int64
	var maxConcurrent int32
	if err := pool.QueryRow(ctx, `SELECT token_limit,max_concurrent FROM model_account_budgets WHERE owner_id=$1`, ownerID).
		Scan(&tokenLimit, &maxConcurrent); err != nil || tokenLimit != wantLimit || maxConcurrent != wantConcurrent {
		t.Fatalf("account model budget = %d/%d, want %d/%d, err=%v", tokenLimit, maxConcurrent, wantLimit, wantConcurrent, err)
	}
}
