package postgres

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/project"
)

func TestPostgresModelTaskBudgetShadowPreviewReturnsDimensionLimitsWithoutWrites(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "shadow preview limits", 1000, 1000)
	ctx := context.Background()
	store := fixture.store
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=90 WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,input_tokens,output_tokens,created_at,settled_at,personal_model)
VALUES
($1,'task','shadow-settled-0001',decode(repeat('01',32),'hex'),'p1234567','test',$2,100,'settled',40,60,clock_timestamp(),clock_timestamp(),false),
($1,'task','shadow-pending-0001',decode(repeat('02',32),'hex'),'p1234567','test',$2,150,'prepared',NULL,NULL,clock_timestamp(),NULL,false)`,
		fixture.ownerID, fence.TaskID); err != nil {
		t.Fatalf("insert account/Task settled and pending usage: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_budget_ledger
(project_id,source,call_id,actor_id,task_id,model_id,rate_provider,rate_model,rate_version,currency,
 input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
 status,actual_input_tokens,actual_output_tokens,actual_micros,request_digest)
VALUES
($1,'task','shadow-settled-0001',$2,$3,'p1234567','budget-test','test',1,'USD',1000000,2000000,40,60,160,
 'settled',40,60,160,decode(repeat('03',32),'hex')),
($1,'task','shadow-pending-0001',$2,$3,'p1234567','budget-test','test',1,'USD',1000000,2000000,10,20,50,
 'reserved',NULL,NULL,NULL,decode(repeat('04',32),'hex'))`, projectID, fixture.ownerID, fence.TaskID); err != nil {
		t.Fatalf("insert project settled and pending usage: %v", err)
	}
	request := modelBudgetShadowPreviewRequest(t, store, fixture.ownerID, fence, projectID,
		&project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD",
			Version: 1, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000})
	before := modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID)
	staleRoute := request
	staleRoute.ConnectionVersion = staleRoute.ConnectionVersion.Add(time.Second)
	if _, err := store.PreviewTaskModelBudget(ctx, staleRoute); err == nil {
		t.Fatal("preview accepted a stale model connection version")
	}
	preview, err := store.PreviewTaskModelBudget(ctx, request)
	if err != nil {
		t.Fatalf("read account/Task/project shadow limits: %v", err)
	}
	if !preview.AccountApplicable || !preview.TaskApplicable || !preview.ProjectApplicable ||
		preview.AccountOutputLimit == nil || *preview.AccountOutputLimit != 700 ||
		preview.TaskOutputLimit == nil || *preview.TaskOutputLimit != 49_710 ||
		preview.ProjectOutputLimit == nil || *preview.ProjectOutputLimit != 370 || len(preview.UnknownReasons) != 0 || preview.CapturedAt.IsZero() {
		t.Fatalf("shadow preview = %#v; want account=700 Task=49710 project=370 with no unknown reasons", preview)
	}
	after := modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID)
	if !bytes.Equal(before, after) {
		t.Fatalf("shadow preview changed Task or ledger state\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestPostgresModelTaskBudgetShadowPreviewReturnsNilForUnknownAndMissingBudgets(t *testing.T) {
	fixture, projectID, fence := newProjectBudgetTaskFixture(t, "shadow preview unknown", 1000, 1000)
	ctx := context.Background()
	store := fixture.store
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,created_at,settled_at,personal_model)
VALUES ($1,'task','shadow-unknown-001',decode(repeat('05',32),'hex'),'p1234567','test',$2,50,'unknown',
 clock_timestamp(),clock_timestamp(),false)`, fixture.ownerID, fence.TaskID); err != nil {
		t.Fatalf("insert unknown account/Task usage: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_budget_ledger
(project_id,source,call_id,actor_id,task_id,model_id,rate_provider,rate_model,rate_version,currency,
 input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
 status,request_digest)
VALUES ($1,'task','shadow-unknown-001',$2,$3,'p1234567','budget-test','test',1,'USD',1000000,2000000,10,10,30,
 'unknown',decode(repeat('06',32),'hex'))`, projectID, fixture.ownerID, fence.TaskID); err != nil {
		t.Fatalf("insert unknown project usage: %v", err)
	}
	request := modelBudgetShadowPreviewRequest(t, store, fixture.ownerID, fence, projectID,
		&project.RateSnapshot{Provider: "budget-test", ModelID: "p1234567", Model: "test", Currency: "USD",
			Version: 1, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000})
	before := modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID)
	preview, err := store.PreviewTaskModelBudget(ctx, request)
	if err != nil {
		t.Fatalf("read unknown usage shadow preview: %v", err)
	}
	wantReasons := []string{"account_usage_unknown", "task_usage_unknown", "project_usage_unknown"}
	if preview.AccountOutputLimit != nil || preview.TaskOutputLimit != nil || preview.ProjectOutputLimit != nil ||
		!reflect.DeepEqual(preview.UnknownReasons, wantReasons) {
		t.Fatalf("unknown shadow preview = %#v; want nil limits and reasons %v", preview, wantReasons)
	}
	if after := modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID); !bytes.Equal(before, after) {
		t.Fatalf("unknown preview changed Task or ledger state\nbefore: %s\nafter:  %s", before, after)
	}

	if _, err := store.pool.Exec(ctx, `DELETE FROM model_account_budgets WHERE owner_id=$1`, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM project_budgets WHERE project_id=$1`, projectID); err != nil {
		t.Fatal(err)
	}
	before = modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID)
	preview, err = store.PreviewTaskModelBudget(ctx, request)
	if err != nil {
		t.Fatalf("read missing budget shadow preview: %v", err)
	}
	wantReasons = []string{"account_budget_missing", "task_usage_unknown", "project_budget_missing"}
	if preview.AccountOutputLimit != nil || preview.TaskOutputLimit != nil || preview.ProjectOutputLimit != nil ||
		!reflect.DeepEqual(preview.UnknownReasons, wantReasons) {
		t.Fatalf("missing budget shadow preview = %#v; want nil limits and reasons %v", preview, wantReasons)
	}
	if after := modelBudgetShadowStateSnapshot(t, store, fixture.ownerID, fence.TaskID, projectID); !bytes.Equal(before, after) {
		t.Fatalf("missing budget preview changed Task or ledger state\nbefore: %s\nafter:  %s", before, after)
	}
}

func modelBudgetShadowPreviewRequest(t *testing.T, store *Store, ownerID string, fence app.ModelTaskFence,
	projectID string, rate *project.RateSnapshot) app.ModelTaskBudgetShadowPreviewRequest {
	t.Helper()
	var endpoint, providerModel string
	var connectionVersion time.Time
	if err := store.pool.QueryRow(context.Background(), `SELECT base_url,model_name,updated_at FROM model_connections WHERE id=$1`,
		"p1234567").Scan(&endpoint, &providerModel, &connectionVersion); err != nil {
		t.Fatalf("read model connection snapshot for preview: %v", err)
	}
	return app.ModelTaskBudgetShadowPreviewRequest{
		Fence: fence, OwnerID: ownerID, ModelID: "p1234567", ProviderEndpoint: endpoint, ProviderModel: providerModel,
		ConnectionVersion: connectionVersion, ProjectID: projectID, PersonalModel: false,
		InputTokenReservation: 50, ProjectRate: rate,
	}
}

func modelBudgetShadowStateSnapshot(t *testing.T, store *Store, ownerID, taskID, projectID string) []byte {
	t.Helper()
	var snapshot []byte
	if err := store.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'task',(SELECT to_jsonb(task) FROM tasks task WHERE task.id=$1),
 'account_ledger',COALESCE((SELECT jsonb_agg(to_jsonb(ledger) ORDER BY ledger.source,ledger.call_id)
   FROM model_call_ledger ledger WHERE ledger.owner_id=$2),'[]'::jsonb),
 'project_ledger',COALESCE((SELECT jsonb_agg(to_jsonb(ledger) ORDER BY ledger.source,ledger.call_id)
   FROM project_budget_ledger ledger WHERE ledger.project_id=$3),'[]'::jsonb),
 'preparations',COALESCE((SELECT jsonb_agg(to_jsonb(preparation) ORDER BY preparation.source,preparation.call_id)
   FROM model_budget_preparations preparation WHERE preparation.owner_id=$2 AND preparation.task_id=$1),'[]'::jsonb))`,
		taskID, ownerID, projectID).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot Task and model ledger state: %v", err)
	}
	return snapshot
}
