package postgres

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestPostgresAdminModelBudgetDefaultUsesCASAndAudit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := service.Get(ctx, admin)
	if err != nil || initial.Configured || initial.TokenLimit != 0 || initial.MaxConcurrent != 0 || initial.Version != 0 {
		t.Fatalf("initial default = %+v, %v", initial, err)
	}
	created, err := service.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 45000, MaxConcurrent: 3}, "192.0.2.7")
	if err != nil || !created.Configured || created.TokenLimit != 45000 || created.MaxConcurrent != 3 || created.Version != 1 {
		t.Fatalf("create default = %+v, %v", created, err)
	}
	updated, err := service.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 1, TokenLimit: 90000, MaxConcurrent: 5}, "192.0.2.7")
	if err != nil || updated.TokenLimit != 90000 || updated.MaxConcurrent != 5 || updated.Version != 2 {
		t.Fatalf("update default = %+v, %v", updated, err)
	}
	if _, err := service.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 1, TokenLimit: 1, MaxConcurrent: 1}, "192.0.2.7"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale default update = %v, want conflict", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, tokenLimit := range []int64{120000, 150000} {
		group.Add(1)
		go func(tokenLimit int64) {
			defer group.Done()
			<-start
			_, err := service.Update(ctx, admin, app.UpdateModelBudgetDefaultCommand{ExpectedVersion: 2, TokenLimit: tokenLimit, MaxConcurrent: 4}, "192.0.2.7")
			results <- err
		}(tokenLimit)
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
			t.Fatalf("concurrent default update = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent default update successes=%d conflicts=%d", successes, conflicts)
	}
	var auditCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action=$1 AND target='admin_model_budget_defaults'`, "model.budget.update").Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("default budget audits=%d err=%v", auditCount, err)
	}
}

func TestPostgresLoadModelBudgetDefaultForApprovalRequiresConfiguration(t *testing.T) {
	store, _ := newTemporaryStore(t)
	err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		_, err := loadModelBudgetDefaultInTx(context.Background(), tx)
		return err
	})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("missing default budget error = %v", err)
	}
}
