package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPendingRequestCostMigrationRepairsAppliedVersionSchemaDrift(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 248); err != nil {
		closeProvider()
		t.Fatalf("prepare schema at migration 248: %v", err)
	}
	closeProvider()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	legacySubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "pending-cost-column-legacy", "review legacy approval")
	legacyTask, err := store.SubmitTask(ctx, legacySubmission)
	if err != nil {
		t.Fatalf("submit legacy approval Task before schema repair: %v", err)
	}
	legacyClaimed, legacyFound, err := store.ClaimNextTask(ctx, "pending-cost-legacy-worker", time.Minute)
	if err != nil || !legacyFound || legacyClaimed.ID != legacyTask.Task.ID {
		t.Fatalf("claim legacy Task before schema repair = %q, %t, %v", legacyClaimed.ID, legacyFound, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE task_pending_requests
DROP CONSTRAINT IF EXISTS task_pending_requests_this_cost_micros_check,
DROP COLUMN IF EXISTS this_cost_micros,
ADD COLUMN current_cost_micros bigint`); err != nil {
		t.Fatalf("simulate the deployed cost-column drift: %v", err)
	}
	legacyPendingID := mustID(t, domain.NewPendingRequestID)
	legacyCurrentCostMicros := int64(54_321)
	if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests (
id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash,
runtime_generation, runtime_instance_id, control_version, status, decision, current_cost_micros
) VALUES ($1,$2,1,'approval','Legacy approval','export.txt','Export file?','File leaves this Host.',
'legacy-cost-call','file_export',$3,1,'legacy-runtime',1,'pending','',$4)`,
		legacyPendingID, legacyTask.Task.ID, hashBytes("legacy-current-cost"), legacyCurrentCostMicros); err != nil {
		t.Fatalf("seed historical current_cost_micros before schema repair: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("repair schema at migration 249: %v", err)
	}
	var expectedColumn, legacyColumn, expectedConstraint bool
	if err := pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='task_pending_requests' AND column_name='this_cost_micros'),
EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='task_pending_requests' AND column_name='current_cost_micros'),
EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='task_pending_requests'::regclass AND conname='task_pending_requests_this_cost_micros_check')`).Scan(
		&expectedColumn, &legacyColumn, &expectedConstraint); err != nil {
		t.Fatalf("inspect repaired task pending request schema: %v", err)
	}
	if !expectedColumn || !legacyColumn || !expectedConstraint {
		t.Fatalf("repaired schema: this_cost=%t current_cost=%t check=%t; want both columns and the nonnegative nullable check",
			expectedColumn, legacyColumn, expectedConstraint)
	}
	var preservedLegacyCost int64
	var newActionCostIsNull bool
	if err := pool.QueryRow(ctx, `SELECT current_cost_micros, this_cost_micros IS NULL
FROM task_pending_requests WHERE id=$1 AND task_id=$2`, legacyPendingID, legacyTask.Task.ID).Scan(
		&preservedLegacyCost, &newActionCostIsNull); err != nil {
		t.Fatalf("read historical cost after schema repair: %v", err)
	}
	if preservedLegacyCost != legacyCurrentCostMicros || !newActionCostIsNull {
		t.Fatalf("legacy cost after schema repair = current:%d this_is_null:%t; want preserved current:%d and unknown new action cost",
			preservedLegacyCost, newActionCostIsNull, legacyCurrentCostMicros)
	}

	repairOwner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	repairHost := createRunningControlHost(t, ctx, store, repairOwner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, repairOwner.ID, repairHost.ID, "pending-cost-column-repair", "approve an external action")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit approval fixture Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "pending-cost-migration-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %+v, %t, %v", claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	expiresAt := time.Now().UTC().Add(time.Hour)
	thisCostMicros, totalCostMicros := int64(12_345), int64(67_890)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: 1, Kind: "approval", Title: "Approve export",
		Object: "report.txt", Content: "Export this file?", Impact: "The file will leave this Host.",
		CallID: "call-cost-migration", ToolName: "file_export", RequestHash: hashBytes("exact-export-call"),
		RuntimeGeneration: 2, RuntimeInstanceID: "runtime-instance-2", ControlVersion: 3, Status: task.PendingRequestPending,
		ExpiresAt: &expiresAt, ThisCostMicros: &thisCostMicros, TotalCostMicros: &totalCostMicros,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
			Title: request.Title, Summary: request.Content}},
		Event:          task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
		PendingRequest: request,
	}); err != nil {
		t.Fatalf("persist pending request after schema repair: %v", err)
	}
	details, err := store.GetTask(ctx, claimed.ID, repairOwner.ID)
	if err != nil {
		t.Fatalf("read Task details after schema repair: %v", err)
	}
	if details.Task.ID != claimed.ID || details.Task.Status != task.StatusWaitingUser || details.PendingRequest == nil {
		t.Fatalf("Task details after schema repair = task=%+v pending=%+v; want waiting_user with pending request", details.Task, details.PendingRequest)
	}
	got := details.PendingRequest
	if got.ID != pendingID || got.Status != task.PendingRequestPending || got.Decision != "" ||
		got.ThisCostMicros == nil || *got.ThisCostMicros != thisCostMicros ||
		got.TotalCostMicros == nil || *got.TotalCostMicros != totalCostMicros {
		t.Fatalf("Task details pending request after schema repair = %+v; want ID %q, pending state, and costs %d/%d",
			got, pendingID, thisCostMicros, totalCostMicros)
	}

	tasks, err := app.NewTaskService(store, k4FixtureAuthorization{}, k4FixtureModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts: k4FixtureHostService{}, TLSAuthorizer: k4FixtureHostService{},
		Verifier: k4FixtureVerifier{ownerID: repairOwner.ID}, Auth: k4FixtureAuth{}, Tasks: tasks,
	}, api.Options{Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20,
		MCPMaxBodyBytes: 1 << 20, SessionTTL: time.Hour, HostScheme: "http", PortalDomain: "morde.test",
		HostSuffix: "hosts.invalid", PreviewPortMin: 1024})
	if err != nil {
		t.Fatalf("create Task details API: %v", err)
	}
	credential, _, err := identity.NewCredential(identity.KindSession, testSessionTokenID(repairOwner.ID))
	if err != nil {
		t.Fatal(err)
	}
	requestTask := httptest.NewRequest(http.MethodGet, "/api/tasks/"+claimed.ID, nil)
	requestTask.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
	responseTask := httptest.NewRecorder()
	router.ServeHTTP(responseTask, requestTask)
	if responseTask.Code != http.StatusOK || !json.Valid(responseTask.Body.Bytes()) {
		t.Fatalf("GET /api/tasks/:id response=%d body=%s; want a valid Task detail", responseTask.Code, responseTask.Body.String())
	}
	var taskResponse struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		PendingRequest *struct {
			ID              string `json:"id"`
			Status          string `json:"status"`
			ThisCostMicros  *int64 `json:"this_cost_micros"`
			TotalCostMicros *int64 `json:"total_cost_micros"`
		} `json:"pending_request"`
	}
	if err := json.Unmarshal(responseTask.Body.Bytes(), &taskResponse); err != nil ||
		taskResponse.ID != claimed.ID || taskResponse.Status != string(task.StatusWaitingUser) || taskResponse.PendingRequest == nil ||
		taskResponse.PendingRequest.ID != pendingID || taskResponse.PendingRequest.Status != string(task.PendingRequestPending) ||
		taskResponse.PendingRequest.ThisCostMicros == nil || *taskResponse.PendingRequest.ThisCostMicros != thisCostMicros ||
		taskResponse.PendingRequest.TotalCostMicros == nil || *taskResponse.PendingRequest.TotalCostMicros != totalCostMicros {
		t.Fatalf("GET /api/tasks/:id pending request = %+v err=%v; want request %q and costs %d/%d",
			taskResponse.PendingRequest, err, pendingID, thisCostMicros, totalCostMicros)
	}

	requestApprovals := httptest.NewRequest(http.MethodGet,
		"/api/me/task-approvals?conversation_id="+created.Task.ConversationID, nil)
	requestApprovals.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
	responseApprovals := httptest.NewRecorder()
	router.ServeHTTP(responseApprovals, requestApprovals)
	if responseApprovals.Code != http.StatusOK || !json.Valid(responseApprovals.Body.Bytes()) {
		t.Fatalf("GET /api/me/task-approvals response=%d body=%s; want a valid approval page",
			responseApprovals.Code, responseApprovals.Body.String())
	}
	var approvalResponse struct {
		Items []struct {
			TaskID   string `json:"task_id"`
			Approval struct {
				ID              string `json:"id"`
				Status          string `json:"status"`
				ThisCostMicros  *int64 `json:"this_cost_micros"`
				TotalCostMicros *int64 `json:"total_cost_micros"`
			} `json:"approval"`
		} `json:"items"`
	}
	if err := json.Unmarshal(responseApprovals.Body.Bytes(), &approvalResponse); err != nil || len(approvalResponse.Items) != 1 ||
		approvalResponse.Items[0].TaskID != claimed.ID || approvalResponse.Items[0].Approval.ID != pendingID ||
		approvalResponse.Items[0].Approval.Status != string(task.PendingRequestPending) ||
		approvalResponse.Items[0].Approval.ThisCostMicros == nil || *approvalResponse.Items[0].Approval.ThisCostMicros != thisCostMicros ||
		approvalResponse.Items[0].Approval.TotalCostMicros == nil || *approvalResponse.Items[0].Approval.TotalCostMicros != totalCostMicros {
		t.Fatalf("GET /api/me/task-approvals items = %+v err=%v; want Task %q approval %q and costs %d/%d",
			approvalResponse.Items, err, claimed.ID, pendingID, thisCostMicros, totalCostMicros)
	}
}
