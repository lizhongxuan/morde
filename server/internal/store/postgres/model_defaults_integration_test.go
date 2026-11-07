package postgres

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
)

func TestPostgresAdminModelDefaultCASAuditAndCandidateFence(t *testing.T) {
	store, pool, modelID := modelDefaultFixture(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	defaults, err := app.NewAdminModelDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := defaults.Get(ctx, admin)
	if err != nil || initial.Configured || initial.ModelID != "" || initial.Version != 0 {
		t.Fatalf("initial default = %+v, %v", initial, err)
	}

	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text,capability_tools)
VALUES ('p1000001',$1,'private checked','https://model.example/v1','private-model',decode(repeat('01',29),'hex'),clock_timestamp(),true,true),
       ('p1000002',NULL,'shared unchecked','https://model.example/v1','unchecked-model',decode(repeat('01',29),'hex'),NULL,false,false)`, admin.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAdminModelDefault(ctx, admin, 0, "p1000001", "192.0.2.20"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("private default candidate = %v, want not found", err)
	}
	if _, err := store.SetAdminModelDefault(ctx, admin, 0, "p1000002", "192.0.2.20"); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("unchecked default candidate = %v, want unsupported", err)
	}
	if _, err := defaults.Update(ctx, admin, 0, modelID, "192.0.2.20"); err != nil {
		t.Fatalf("set checked shared default: %v", err)
	}
	if _, err := store.SetAdminModelDefault(ctx, admin, 0, "", "192.0.2.20"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale default update = %v, want conflict", err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action=$1 AND target='admin_model_defaults'`, string(audit.ActionModelDefaultUpdate)).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("default audit count after rejected writes = %d, %v", auditCount, err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, modelID := range []string{modelID, ""} {
		wait.Add(1)
		go func(modelID string) {
			defer wait.Done()
			<-start
			_, err := store.SetAdminModelDefault(ctx, admin, 1, modelID, "192.0.2.21")
			results <- err
		}(modelID)
	}
	close(start)
	wait.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if domain.CodeOf(err) == domain.CodeConflict {
			conflicted++
			continue
		}
		t.Fatalf("concurrent default update = %v", err)
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent default updates: %d succeeded, %d conflicted", succeeded, conflicted)
	}
	current, err := defaults.Get(ctx, admin)
	if err != nil || current.Version != 2 {
		t.Fatalf("default after CAS race = %+v, %v", current, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action=$1 AND target='admin_model_defaults'`, string(audit.ActionModelDefaultUpdate)).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("default audit count after CAS race = %d, %v", auditCount, err)
	}
}

func TestPostgresAdminDefaultAllowsCheckedTextOnlyModel(t *testing.T) {
	store, _, _ := modelDefaultFixture(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text,capability_tools)
VALUES ('p1000003',NULL,'text only','https://model.example/v1','text-model',decode(repeat('01',29),'hex'),clock_timestamp(),true,false)`); err != nil {
		t.Fatal(err)
	}
	defaults, err := app.NewAdminModelDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := defaults.Update(ctx, admin, 0, "p1000003", "192.0.2.20")
	if err != nil || !updated.Configured || updated.ModelID != "p1000003" {
		t.Fatalf("text-only default = %+v, %v", updated, err)
	}
	available, err := defaults.Available(ctx)
	if err != nil || !available.Configured || available.ModelID != "p1000003" {
		t.Fatalf("text-only default availability = %+v, %v", available, err)
	}
}

func modelDefaultFixture(t *testing.T) (*Store, *pgxpool.Pool, string) {
	t.Helper()
	store, pool := newTemporaryStore(t)
	if _, err := store.pool.Exec(context.Background(), `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text,capability_tools)
VALUES ('p1000000',NULL,'default candidate','https://model.example/v1','default-model',decode(repeat('01',29),'hex'),clock_timestamp(),true,true)`); err != nil {
		t.Fatal(err)
	}
	return store, pool, "p1000000"
}
