package postgres

import (
	"context"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func TestAdminControlsPersistAuditLocalNotificationsAndFireOnce(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 8000, 16000)
	createTestUser(t, store, 2, 8000, 16000)
	admin := identity.Principal{UserID: owner.ID, TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	service, err := app.NewAdminControlsService(store)
	if err != nil {
		t.Fatal(err)
	}

	initial, err := service.List(ctx, admin)
	if err != nil || initial.AccountsTotal != 2 || len(initial.Rules) != 0 || len(initial.Notifications) != 0 || !initial.NotifyConfigured {
		t.Fatalf("initial admin controls = %#v err=%v", initial, err)
	}
	rule, err := service.Create(ctx, admin, 2, "local threshold")
	if err != nil || rule.ID < 1 || rule.Version != 1 || !rule.Enabled {
		t.Fatalf("create rule = %#v err=%v", rule, err)
	}
	firedNow, err := concurrentAdminControlEvaluations(ctx, service, admin, 2)
	if err != nil || firedNow != 1 {
		t.Fatalf("concurrent first evaluation fired %d times, err=%v", firedNow, err)
	}
	var firedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_control_notifications WHERE kind='rule' AND rule_id=$1`, rule.ID).Scan(&firedRows); err != nil || firedRows != 1 {
		t.Fatalf("first threshold fired %d times, err=%v", firedRows, err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT version FROM admin_control_rules WHERE id=$1`, rule.ID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("rule version after first firing = %d err=%v", version, err)
	}
	if _, err := service.Update(ctx, admin, app.UpdateAdminControlRuleCommand{ID: rule.ID, ExpectedVersion: 1, Rearm: true}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale update error = %v", err)
	}
	updated, err := service.Update(ctx, admin, app.UpdateAdminControlRuleCommand{ID: rule.ID, ExpectedVersion: 2, Rearm: true})
	if err != nil || updated.Version != 3 || updated.LastFiredAt != nil || updated.FiredAccounts != 0 {
		t.Fatalf("rearm rule = %#v err=%v", updated, err)
	}
	firedNow, err = concurrentAdminControlEvaluations(ctx, service, admin, 2)
	if err != nil || firedNow != 1 {
		t.Fatalf("concurrent rearmed evaluation fired %d times, err=%v", firedNow, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_control_notifications WHERE kind='rule' AND rule_id=$1`, rule.ID).Scan(&firedRows); err != nil || firedRows != 2 {
		t.Fatalf("rearmed threshold fired %d times total, err=%v", firedRows, err)
	}
	if err := service.Delete(ctx, admin, rule.ID, 3); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT version FROM admin_control_rules WHERE id=$1`, rule.ID).Scan(&version); err != nil || version != 4 {
		t.Fatalf("rule version after rearmed firing = %d err=%v", version, err)
	}
	if err := service.Delete(ctx, admin, rule.ID, 4); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	notified, err := service.NotifyTest(ctx, admin)
	if err != nil || !notified.Sent || !notified.Configured {
		t.Fatalf("local notify-test = %#v err=%v", notified, err)
	}
	final, err := service.List(ctx, admin)
	if err != nil || len(final.Rules) != 0 || len(final.Notifications) != 3 || final.Notifications[0].Kind != "test" {
		t.Fatalf("final controls = %#v err=%v", final, err)
	}
	var auditCounts struct {
		Create int64
		Update int64
		Delete int64
		Fire   int64
		Eval   int64
		Test   int64
	}
	if err := pool.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE action=$1),count(*) FILTER (WHERE action=$2),count(*) FILTER (WHERE action=$3),
count(*) FILTER (WHERE action=$4),count(*) FILTER (WHERE action=$5),count(*) FILTER (WHERE action=$6)
FROM audit_logs WHERE actor_id=$7`,
		string(audit.ActionAdminControlRuleCreate), string(audit.ActionAdminControlRuleUpdate), string(audit.ActionAdminControlRuleDelete),
		string(audit.ActionAdminControlRuleFire), string(audit.ActionAdminControlEvaluate), string(audit.ActionAdminControlNotifyTest), owner.ID).
		Scan(&auditCounts.Create, &auditCounts.Update, &auditCounts.Delete, &auditCounts.Fire, &auditCounts.Eval, &auditCounts.Test); err != nil {
		t.Fatal(err)
	}
	if auditCounts.Create != 1 || auditCounts.Update != 1 || auditCounts.Delete != 1 || auditCounts.Fire != 2 || auditCounts.Eval != 4 || auditCounts.Test != 1 {
		t.Fatalf("admin control audits = %+v", auditCounts)
	}
}

func concurrentAdminControlEvaluations(ctx context.Context, service *app.AdminControlsService, admin identity.Principal, count int) (int, error) {
	type result struct {
		fired int
		err   error
	}
	var wait sync.WaitGroup
	results := make(chan result, count)
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := service.Evaluate(ctx, admin)
			if err != nil {
				results <- result{err: err}
			} else {
				results <- result{fired: len(value.Fired)}
			}
		}()
	}
	wait.Wait()
	close(results)
	var fired int
	for value := range results {
		if value.err != nil {
			return fired, value.err
		}
		fired += value.fired
	}
	return fired, nil
}
