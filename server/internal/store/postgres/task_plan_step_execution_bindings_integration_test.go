package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskPlanStepBindingsRequireDurableOperationEvidence(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-operation-binding", true)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Prepare the report",
		Scope: "Collect and verify sources", VerificationMethod: "Check the saved report", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Collect sources", Status: task.PlanStepCompleted,
				CompletionCriteria: []string{"sources are recorded"}, Outputs: []string{"source notes"}},
			{ID: "report", Title: "Prepare report", Status: task.PlanStepCompleted, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"report is verified"}, Outputs: []string{"report"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-binding-plan-1")
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessMissingBinding)

	callID := "call_plan_step_operation"
	toolName := "terminal_exec"
	digest := sha256.Sum256([]byte("terminal_exec:{\"command\":\"printf report\"}"))
	toolCall, operation := taskPlanStepOperationFixture(t, fixture, current, callID, toolName, digest[:])
	created, replayed, err := fixture.route.store.CreateOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed || created.Status != task.OperationPrepared {
		t.Fatalf("create task plan dependency operation = %#v replayed=%t err=%v", created, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, toolCall, "plan-binding-operation-step")

	var workers sync.WaitGroup
	bindErrors := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			bindErrors <- fixture.route.store.BindCloudDSHTaskPlanStepOperation(ctx, current, 1, "sources", toolCall, created)
		}()
	}
	workers.Wait()
	close(bindErrors)
	for err := range bindErrors {
		if err != nil {
			t.Fatalf("concurrent exact binding replay: %v", err)
		}
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND task_revision=$2 AND plan_revision=1 AND plan_step_id='sources'`, current.ID,
		*current.CurrentRevision).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("exact concurrent binding count=%d err=%v; want one", bindings, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessPending)

	changedStep := fixture.route.store.BindCloudDSHTaskPlanStepOperation(ctx, current, 1, "report", toolCall, created)
	if domain.CodeOf(changedStep) != domain.CodeIdempotencyConflict {
		t.Fatalf("same execution was rebound to another plan step: %v", changedStep)
	}
	otherTaskOperation := created
	otherTaskOperation.TaskID = mustID(t, domain.NewTaskID)
	if err := fixture.route.store.BindCloudDSHTaskPlanStepOperation(ctx, current, 1, "sources", toolCall, otherTaskOperation); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-task operation binding error=%v; want invalid argument", err)
	}
	otherOwner := createPreRetirementTestUser(t, fixture.route.store, 1, 2000, 4096)
	wrongOwner := current
	wrongOwner.OwnerID = otherOwner.ID
	if ready, reason, err := fixture.route.store.CheckCloudDSHTaskPlanStepReadiness(ctx, wrongOwner, 1, "report"); ready || domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner readiness = ready:%t reason:%q err:%v; want not found", ready, reason, err)
	}

	missingEvidence := created
	missingEvidence.ID = mustID(t, domain.NewOperationID)
	if err := fixture.route.store.BindCloudDSHTaskPlanStepOperation(ctx, current, 1, "sources", toolCall, missingEvidence); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("missing operation evidence binding error=%v; want conflict", err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND task_revision=$2 AND plan_revision=1`, current.ID, *current.CurrentRevision).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("rejected binding changed durable row count=%d err=%v; want one", bindings, err)
	}

	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET status='succeeded' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", true, "")
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET status='failed' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessFailed)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET status='unknown' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessUnknown)

	plan.Revision = 2
	plan.Goal = "Prepare the revised report"
	commitTaskPlanForStepBinding(t, fixture, plan, "plan-binding-plan-2")
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessPlanRevisionStale)
	latest, err := fixture.route.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, latest.Task, 2, "report", false, planStepReadinessMissingBinding)

	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=desired_revision+1 WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, latest.Task, 2, "report", false, planStepReadinessTaskRevisionStale)
}

func TestPostgresTaskPlanStepOperationDispatchBlocksUntilDependenciesSettle(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-dispatch-dependencies", true)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Prepare the report",
		Scope: "Collect and verify sources", VerificationMethod: "Check the saved report", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Collect sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources are recorded"}},
			{ID: "report", Title: "Prepare report", Status: task.PlanStepCompleted, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"report is verified"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-dispatch-dependency-plan")
	toolCall, operation := taskPlanStepOperationFixture(t, fixture, current, "call_plan_report_blocked", "terminal_exec",
		taskPlanStepTestDigest("original DSH request digest"))
	if _, _, err := fixture.route.store.CreateOperation(ctx, operation, current.Lease.Owner); err != nil {
		t.Fatalf("create dependent operation: %v", err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, toolCall, "report-call-before-dispatch")

	for name, metadata := range map[string]struct {
		revision int64
		stepID   string
	}{
		"missing metadata":    {},
		"unknown model step":  {revision: 1, stepID: "model-invented"},
		"stale plan revision": {revision: 2, stepID: "report"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, operation,
				metadata.revision, metadata.stepID, task.OperationDispatched))
			if domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("dispatch metadata (%d,%q) error=%v; want conflict", metadata.revision, metadata.stepID, err)
			}
		})
	}
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, operation,
		1, "report", task.OperationDispatched)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dependency-blocked dispatch error=%v; want conflict", err)
	}
	stored, err := fixture.route.store.GetOperation(ctx, current.OwnerID, current.ID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("blocked operation status=%q err=%v; want prepared", stored.Status, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("blocked dispatch created %d bindings, err=%v; want none", bindings, err)
	}
}

func TestPostgresTaskPlanStepOperationDispatchBindsAtomicallyAndReplays(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-dispatch-settlement", true)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Prepare the report",
		Scope: "Collect and verify sources", VerificationMethod: "Check the saved report", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Collect sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources are recorded"}},
			{ID: "report", Title: "Prepare report", Status: task.PlanStepCompleted, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"report is verified"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-dispatch-settlement-plan")
	sourceCall, sourceOperation := taskPlanStepOperationFixture(t, fixture, current, "call_plan_sources_dispatch", "terminal_exec",
		taskPlanStepTestDigest("source call arguments"))
	if _, _, err := fixture.route.store.CreateOperation(ctx, sourceOperation, current.Lease.Owner); err != nil {
		t.Fatalf("create source operation: %v", err)
	}
	sourceStepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, sourceCall,
		"source-step-running", task.StepRunning, "")
	dispatch := taskPlanOperationUpdate(current, fixture.host, sourceOperation, 1, "sources", task.OperationDispatched)
	if _, err := fixture.route.store.UpdateOperation(ctx, dispatch); err != nil {
		t.Fatalf("dispatch predecessor: %v", err)
	}
	if _, err := fixture.route.store.UpdateOperation(ctx, dispatch); err != nil {
		t.Fatalf("exact dispatched replay: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessPending)
	var sourceBindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND plan_revision=1 AND plan_step_id='sources'`, current.ID).Scan(&sourceBindings); err != nil || sourceBindings != 1 {
		t.Fatalf("predecessor binding count=%d err=%v; want one after idempotent replay", sourceBindings, err)
	}
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, sourceOperation,
		0, "", task.OperationSucceeded)); err != nil {
		t.Fatalf("settle predecessor operation: %v", err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, sourceCall,
		"source-step-settled", task.StepSucceeded, sourceStepID)
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", true, "")

	reportCall, reportOperation := taskPlanStepOperationFixture(t, fixture, current, "call_plan_report_dispatch", "terminal_exec",
		taskPlanStepTestDigest("report call arguments"))
	if _, _, err := fixture.route.store.CreateOperation(ctx, reportOperation, current.Lease.Owner); err != nil {
		t.Fatalf("create dependent report operation: %v", err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, reportCall, "report-step-ready")
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, reportOperation,
		1, "report", task.OperationDispatched)); err != nil {
		t.Fatalf("dispatch dependent after predecessor settlement: %v", err)
	}
	var reportBindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND plan_revision=1 AND plan_step_id='report'`, current.ID).Scan(&reportBindings); err != nil || reportBindings != 1 {
		t.Fatalf("dependent binding count=%d err=%v; want one", reportBindings, err)
	}
}

func TestPostgresTaskPlanStepDispatchFailsWhenPlanChangesAfterPreparation(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-dispatch-stale-plan", true)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Read source",
		Scope: "Read the file", VerificationMethod: "Inspect returned bytes", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "sources", Title: "Read source", Status: task.PlanStepCompleted, CompletionCriteria: []string{"source returned"}}}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-stale-before-dispatch-1")
	call, operation := taskPlanStepOperationFixture(t, fixture, current, "call_plan_stale_dispatch", "terminal_exec",
		taskPlanStepTestDigest("stale call arguments"))
	if _, _, err := fixture.route.store.CreateOperation(ctx, operation, current.Lease.Owner); err != nil {
		t.Fatalf("create operation before plan update: %v", err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, call, "stale-plan-operation-step")
	plan.Revision = 2
	plan.Goal = "Read updated source"
	commitTaskPlanForStepBinding(t, fixture, plan, "plan-stale-before-dispatch-2")
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, operation,
		1, "sources", task.OperationDispatched)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale plan dispatch error=%v; want conflict", err)
	}
	stored, err := fixture.route.store.GetOperation(ctx, current.OwnerID, current.ID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("stale-plan operation status=%q err=%v; want prepared", stored.Status, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("stale-plan dispatch created %d bindings, err=%v; want none", bindings, err)
	}
}

func TestPostgresTaskPlanStepOperationDispatchWithoutPlanPreservesBehavior(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-dispatch-no-plan", true)
	ctx := context.Background()
	current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	call, operation := taskPlanStepOperationFixture(t, fixture, current.Task, "call_no_plan_dispatch", "terminal_exec",
		taskPlanStepTestDigest("no plan call arguments"))
	if _, _, err := fixture.route.store.CreateOperation(ctx, operation, current.Task.Lease.Owner); err != nil {
		t.Fatalf("create operation without plan: %v", err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current.Task, call, "no-plan-operation-step")
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current.Task, fixture.host, operation,
		0, "", task.OperationDispatched)); err != nil {
		t.Fatalf("dispatch without plan metadata: %v", err)
	}
}

func TestPostgresTaskPlanStepHubCommandDispatchBindsAtomicallyAndChecksDependencies(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-hub-command-binding", true)
	ctx := context.Background()
	current := fixture.task
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, current.OwnerID); err != nil {
		t.Fatal(err)
	}
	plan := task.Plan{TaskRevision: *current.CurrentRevision, Revision: 1, Goal: "Run dependent Hub commands",
		Scope: "Use the assigned computer", VerificationMethod: "Check the durable command result", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "source", Title: "Run source command", Status: task.PlanStepPending,
				CompletionCriteria: []string{"source command completed"}, Outputs: []string{"source result"}},
			{ID: "followup", Title: "Run dependent command", Status: task.PlanStepPending, DependsOn: []string{"source"},
				CompletionCriteria: []string{"dependent command completed"}, Outputs: []string{"followup result"}},
		}}
	current = commitTaskPlanForStepBinding(t, fixture, plan, "plan-binding-hub-plan")
	toolName := "morde_hub_command_" + targetID + "_v1"

	newOperation := func(callID, stepID string) (app.TaskHubOperation, app.TaskToolCallProgress) {
		t.Helper()
		digest := sha256.Sum256([]byte(toolName + ":" + callID))
		toolCall := taskPlanBindingToolCall(fixture, current, callID, toolName, digest[:])
		return app.TaskHubOperation{OwnerID: current.OwnerID, TaskID: current.ID,
			Key: app.AgentOperationKey(callID), ToolName: toolName, TargetInstallationID: targetID,
			Action: hub.ActionCommand, TaskAuthorizationVersion: current.Authorization.Version,
			TargetGrantVersion: 1, LeaseVersion: current.Lease.Version, RequestHash: hashBytes("hub request:" + callID),
			Status: task.OperationPrepared, AgentSessionID: toolCall.AgentSessionID, CallID: callID,
			ToolCallDigest: append([]byte(nil), digest[:]...), PlanTaskRevision: *current.CurrentRevision,
			PlanRevision: 1, PlanStepID: stepID}, toolCall
	}

	followup, followupCall := newOperation("call-plan-hub-followup", "followup")
	followup, replayed, err := fixture.route.store.PrepareTaskHubOperation(ctx, followup, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare dependent Hub command = %#v replayed=%t err=%v", followup, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, followupCall, "plan-hub-followup", task.StepRunning, "")
	if err := fixture.route.store.MarkTaskHubOperationDispatched(ctx, followup, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dependent Hub dispatch without settled prerequisite = %v; want conflict", err)
	}
	var bindingCount int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND execution_kind='hub_operation'`, current.ID).Scan(&bindingCount); err != nil || bindingCount != 0 {
		t.Fatalf("blocked dependent dispatch wrote %d Hub bindings, err=%v; want none", bindingCount, err)
	}
	var status string
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT status FROM task_hub_operations WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`,
		current.OwnerID, current.ID, followup.Key).Scan(&status); err != nil || status != string(task.OperationPrepared) {
		t.Fatalf("blocked dependent Hub operation status=%q err=%v; want prepared", status, err)
	}

	source, sourceCall := newOperation("call-plan-hub-source", "source")
	source, replayed, err = fixture.route.store.PrepareTaskHubOperation(ctx, source, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare source Hub command = %#v replayed=%t err=%v", source, replayed, err)
	}
	stepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, sourceCall, "plan-hub-source", task.StepRunning, "")
	if err := fixture.route.store.MarkTaskHubOperationDispatched(ctx, source, current.Lease.Owner); err != nil {
		t.Fatalf("dispatch source Hub command: %v", err)
	}
	if err := fixture.route.store.CompleteTaskHubOperation(ctx, source, current.Lease.Owner, task.OperationSucceeded, hashBytes("source result")); err != nil {
		t.Fatalf("settle source Hub command: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded'
WHERE task_id=$1 AND id=$2`, current.ID, stepID); err != nil {
		t.Fatalf("settle source TaskStep: %v", err)
	}
	if err := fixture.route.store.MarkTaskHubOperationDispatched(ctx, followup, current.Lease.Owner); err != nil {
		t.Fatalf("dispatch dependent Hub command after prerequisite settlement: %v", err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND execution_kind='hub_operation' AND plan_step_id IN ('source','followup')`, current.ID).Scan(&bindingCount); err != nil || bindingCount != 2 {
		t.Fatalf("Hub command plan binding count=%d err=%v; want one per dispatched step", bindingCount, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "followup", true, "")
}

func TestPostgresTaskPlanStepMCPBindingUsesOperationLedger(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-mcp-binding", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Find sources",
		Scope: "Search for sources", VerificationMethod: "Inspect returned sources", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "search", Title: "Search sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources found"}},
			{ID: "summarize", Title: "Summarize sources", Status: task.PlanStepCompleted, DependsOn: []string{"search"},
				CompletionCriteria: []string{"summary checked"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-binding-mcp-plan")
	callID, toolName := "mcp_plan_binding_call", "morde_web_search"
	digest := sha256.Sum256([]byte("morde_web_search:{\"query\":\"sources\"}"))
	toolCall := taskPlanBindingToolCall(fixture, current, callID, toolName, digest[:])
	operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: current.ID, OwnerID: current.OwnerID,
		ConnectorID: app.BuiltinSearchMCPConnectorID, Key: "plan-binding-mcp", TaskRevision: current.CurrentRevision,
		CallID: callID, ToolName: toolName, ConnectorRevision: 1, GrantVersion: current.Authorization.Version,
		LeaseVersion: current.Lease.Version, RequestHash: digest[:]}
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare task plan MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, toolCall, "plan-binding-mcp-step")
	if err := fixture.route.store.BindCloudDSHTaskPlanStepMCPConnectorOperation(ctx, current, 1, "search", toolCall, prepared); err != nil {
		t.Fatalf("bind MCP operation: %v", err)
	}
	if err := fixture.route.store.BindCloudDSHTaskPlanStepMCPConnectorOperation(ctx, current, 1, "search", toolCall, prepared); err != nil {
		t.Fatalf("replay MCP operation binding: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summarize", false, planStepReadinessPending)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='succeeded' WHERE id=$1`, prepared.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summarize", true, "")
}

func TestPostgresTaskPlanStepMCPDispatchAdmissionBlocksUntilDependenciesSettle(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-mcp-dispatch-gate", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Find and summarize sources",
		Scope: "Search for sources and prepare a summary", VerificationMethod: "Inspect the stored results", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "search", Title: "Search sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources found"}},
			{ID: "summarize", Title: "Summarize sources", Status: task.PlanStepCompleted, DependsOn: []string{"search"},
				CompletionCriteria: []string{"summary checked"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-dispatch-plan-1")

	blockedCall, blockedOperation := taskPlanMCPConnectorOperationFixture(t, fixture, current,
		"call_mcp_summary_before_sources", "morde_web_search", taskPlanStepTestDigest("summary search arguments"))
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, blockedOperation, current.Lease.Owner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare dependent MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, blockedCall, "mcp-summary-before-dispatch")
	for name, metadata := range map[string]struct {
		revision int64
		stepID   string
	}{
		"missing metadata":    {},
		"unknown model step":  {revision: 1, stepID: "model-invented"},
		"stale plan revision": {revision: 2, stepID: "summarize"},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := prepared
			attempt.PlanRevision, attempt.PlanStepID = metadata.revision, metadata.stepID
			if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, attempt, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("MCP dispatch metadata (%d,%q) error=%v; want conflict", metadata.revision, metadata.stepID, err)
			}
		})
	}
	blockedDispatch := prepared
	blockedDispatch.PlanRevision, blockedDispatch.PlanStepID = 1, "summarize"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, blockedDispatch, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dependency-blocked MCP dispatch error=%v; want conflict", err)
	}
	storedBlocked, err := fixture.route.store.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID,
		app.BuiltinSearchMCPConnectorID, prepared.Key)
	if err != nil || storedBlocked.Status != task.OperationPrepared {
		t.Fatalf("blocked MCP operation status=%q err=%v; want prepared", storedBlocked.Status, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("blocked MCP dispatch created %d bindings, err=%v; want none", bindings, err)
	}

	searchCall, searchOperation := taskPlanMCPConnectorOperationFixture(t, fixture, current,
		"call_mcp_search_sources", "morde_web_search", taskPlanStepTestDigest("source search arguments"))
	searchPrepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, searchOperation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare source MCP operation = %#v replayed=%t err=%v", searchPrepared, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, searchCall, "mcp-search-before-dispatch")
	searchDispatch := searchPrepared
	searchDispatch.PlanRevision, searchDispatch.PlanStepID = 1, "search"
	dispatched, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, searchDispatch, current.Lease.Owner)
	if err != nil || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatch source MCP operation = %#v err=%v", dispatched, err)
	}
	if replayedDispatch, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, searchDispatch, current.Lease.Owner); err != nil ||
		replayedDispatch.Status != task.OperationDispatched {
		t.Fatalf("exact MCP dispatch replay = %#v err=%v", replayedDispatch, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summarize", false, planStepReadinessPending)
	if _, err := fixture.route.store.CompleteMCPConnectorOperation(ctx, dispatched, current.Lease.Owner,
		task.OperationSucceeded, `{"sources":1}`); err != nil {
		t.Fatalf("settle predecessor MCP operation: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summarize", true, "")

	dependentCall, dependentOperation := taskPlanMCPConnectorOperationFixture(t, fixture, current,
		"call_mcp_summary_after_sources", "morde_web_search", taskPlanStepTestDigest("dependent search arguments"))
	dependentPrepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, dependentOperation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare dependent MCP operation after settlement = %#v replayed=%t err=%v", dependentPrepared, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, dependentCall, "mcp-summary-ready-to-dispatch")
	dependentDispatch := dependentPrepared
	dependentDispatch.PlanRevision, dependentDispatch.PlanStepID = 1, "summarize"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, dependentDispatch, current.Lease.Owner); err != nil {
		t.Fatalf("dispatch dependent MCP operation after settlement: %v", err)
	}

	plan.Revision = 2
	plan.Goal = "Find and summarize the revised sources"
	latest := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-dispatch-plan-2")
	staleCall, staleOperation := taskPlanMCPConnectorOperationFixture(t, fixture, latest,
		"call_mcp_stale_plan_revision", "morde_web_search", taskPlanStepTestDigest("stale plan arguments"))
	stalePrepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, staleOperation, latest.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare MCP operation before plan update = %#v replayed=%t err=%v", stalePrepared, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, latest, staleCall, "mcp-stale-plan-step")
	stalePrepared.PlanRevision, stalePrepared.PlanStepID = 1, "search"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, stalePrepared, latest.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("MCP dispatch after plan revision changed error=%v; want conflict", err)
	}
}

func taskPlanMCPConnectorOperationFixture(t *testing.T, fixture s03AgentTaskFixture, current task.Task,
	callID, toolName string, requestDigest []byte) (app.TaskToolCallProgress, app.MCPConnectorOperation) {
	t.Helper()
	toolCall := taskPlanBindingToolCall(fixture, current, callID, toolName, requestDigest)
	operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: current.ID, OwnerID: current.OwnerID,
		ConnectorID: app.BuiltinSearchMCPConnectorID, Key: app.AgentOperationKey(callID), TaskRevision: current.CurrentRevision,
		CallID: callID, ToolName: toolName, ConnectorRevision: 1, CredentialVersion: 0,
		GrantVersion: current.Authorization.Version, LeaseVersion: current.Lease.Version,
		RequestHash: append([]byte(nil), requestDigest...), Status: task.OperationPrepared}
	return toolCall, operation
}

func TestPostgresTaskPlanStepChildBindingUsesSettledChildStep(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-child-binding", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Delegate source collection",
		Scope: "Collect source notes", VerificationMethod: "Check the child result", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "delegate", Title: "Collect sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources collected"}},
			{ID: "write", Title: "Write report", Status: task.PlanStepCompleted, DependsOn: []string{"delegate"},
				CompletionCriteria: []string{"report checked"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-binding-child-plan")
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	command := app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
		PlanStepID: "delegate", PlanRevision: 1,
		RootCheckpointThroughSequence: rootAgentCheckpointBaselineForTest(t, fixture.route.store, current.ID, current.OwnerID, fixture.fence.AgentSessionID),
		SpawnKey:                      "call_plan_binding_child", Arguments: []byte(`{"title":"Collect sources","objective":"Collect source notes"}`),
		Title: "Collect sources", Summary: "Collect source notes", AllowedTools: []string{"workspace_read"}}
	child, replayed, err := service.Create(ctx, command)
	if err != nil || replayed {
		t.Fatalf("create task plan child = %#v replayed=%t err=%v", child, replayed, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "write", false, planStepReadinessPending)
	replayedChild, replayed, err := service.Create(ctx, command)
	if err != nil || !replayed || replayedChild.SessionID != child.SessionID {
		t.Fatalf("replay plan-bound child spawn = %#v replayed=%t err=%v", replayedChild, replayed, err)
	}
	if _, _, err := service.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: fixture.fence,
		ActorSessionID: fixture.fence.AgentSessionID, SessionID: child.SessionID, Status: task.StepFailed,
		CheckpointThrough: -1, Summary: "No source notes were available."}); err != nil {
		t.Fatalf("settle child failure: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "write", false, planStepReadinessFailed)
}

func commitTaskPlanForStepBinding(t *testing.T, fixture s03AgentTaskFixture, plan task.Plan, callID string) task.Task {
	t.Helper()
	ctx := context.Background()
	current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatalf("load Task before plan commit: %v", err)
	}
	var sequence int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	progress := postgresCloudDSHTaskPlanProgress(t, current.Task, fixture.fence, plan, callID, mustID(t, domain.NewStepID), sequence)
	if _, err := fixture.route.store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatalf("commit task plan revision %d: %v", plan.Revision, err)
	}
	current, err = fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatalf("reload Task after plan commit: %v", err)
	}
	return current.Task
}

func taskPlanStepOperationFixture(t *testing.T, fixture s03AgentTaskFixture, current task.Task, callID, toolName string,
	requestDigest []byte) (app.TaskToolCallProgress, task.Operation) {
	t.Helper()
	toolCall := taskPlanBindingToolCall(fixture, current, callID, toolName, requestDigest)
	return toolCall, task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: current.ID,
		Key: app.AgentOperationKey(callID), RequestHash: append([]byte(nil), requestDigest...),
		TaskRevision: current.CurrentRevision, CallID: callID, ToolName: toolName, HostID: fixture.host.ID,
		RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
		ControlVersion: fixture.host.ControlVersion, LeaseVersion: current.Lease.Version,
		RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationPrepared}
}

func taskPlanOperationUpdate(current task.Task, runtimeHost host.Host, operation task.Operation,
	planRevision int64, planStepID string, status task.OperationStatus) app.OperationUpdate {
	return app.OperationUpdate{TaskID: current.ID, OperationKey: operation.Key, PlanRevision: planRevision, PlanStepID: planStepID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		RuntimeGeneration: runtimeHost.RuntimeGeneration, RuntimeInstanceID: runtimeHost.RuntimeInstanceID,
		ControlVersion: runtimeHost.ControlVersion, RuntimeLeaseVersion: current.Lease.Version, Status: status}
}

func taskPlanStepTestDigest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func taskPlanBindingToolCall(fixture s03AgentTaskFixture, current task.Task, callID, toolName string,
	requestDigest []byte) app.TaskToolCallProgress {
	return app.TaskToolCallProgress{AgentSessionID: fixture.fence.AgentSessionID, AgentInstanceID: fixture.fence.AgentInstanceID,
		AgentGeneration: fixture.fence.AgentGeneration, AuthorizationVersion: fixture.fence.AuthorizationVersion,
		TaskRevision: *current.CurrentRevision, CallKey: callID, ToolName: toolName,
		RequestDigest: append([]byte(nil), requestDigest...), Phase: string(task.StepSucceeded), DetailState: "not_recorded"}
}

func commitToolCallStepForPlanBinding(t *testing.T, fixture s03AgentTaskFixture, current task.Task,
	toolCall app.TaskToolCallProgress, eventSuffix string) {
	t.Helper()
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall, eventSuffix, task.StepSucceeded, "")
}

func commitToolCallStepForPlanBindingWithStatus(t *testing.T, fixture s03AgentTaskFixture, current task.Task,
	toolCall app.TaskToolCallProgress, eventSuffix string, status task.StepStatus, stepID string) string {
	t.Helper()
	ctx := context.Background()
	var sequence int64
	if stepID == "" {
		stepID = mustID(t, domain.NewStepID)
		if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.ID).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
	} else if err := fixture.route.store.pool.QueryRow(ctx, `SELECT sequence FROM task_steps WHERE task_id=$1 AND id=$2`, current.ID, stepID).Scan(&sequence); err != nil {
		t.Fatalf("read existing tool TaskStep sequence: %v", err)
	}
	summary := "The tool call completed."
	if status == task.StepRunning {
		summary = "The tool call is running."
	}
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: sequence, Kind: "tool",
		Status: status, Title: "Run dependency tool", Summary: summary}
	usage := current.Usage
	usage.ToolCalls++
	payload, err := json.Marshal(map[string]string{"phase": "tool_call_completed", "step_id": step.ID, "call_id": toolCall.CallKey,
		"test_case": eventSuffix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusRunning, CurrentStep: step.ID, Result: current.Result,
		Usage: usage, Steps: []task.Step{step}, ToolCall: &toolCall,
		Event: task.Event{TaskID: current.ID, Type: "task.step", Payload: payload}}); err != nil {
		t.Fatalf("commit tool TaskStep %s: %v", toolCall.CallKey, err)
	}
	return step.ID
}

func assertTaskPlanReadiness(t *testing.T, store *Store, current task.Task, planRevision int64, stepID string,
	wantReady bool, wantReason string) {
	t.Helper()
	ready, reason, err := store.CheckCloudDSHTaskPlanStepReadiness(context.Background(), current, planRevision, stepID)
	if err != nil || ready != wantReady || reason != wantReason {
		t.Fatalf("CheckCloudDSHTaskPlanStepReadiness(%q) = ready:%t reason:%q err:%v; want ready:%t reason:%q",
			stepID, ready, reason, err, wantReady, wantReason)
	}
}

func TestPostgresCloudDSHTaskPlanStepReadBindingRequiresExactSettlement(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-step-read-binding", true)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Prepare a verified report",
		Scope: "Read approved sources and prepare a report", VerificationMethod: "Check the report evidence", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Read sources", Status: task.PlanStepCompleted,
				CompletionCriteria: []string{"source material is recorded"}, Outputs: []string{"source notes"}},
			{ID: "report", Title: "Prepare report", Status: task.PlanStepCompleted, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"report is verified"}, Outputs: []string{"report"}},
			{ID: "final", Title: "Verify report", Status: task.PlanStepCompleted, DependsOn: []string{"report"},
				CompletionCriteria: []string{"report evidence is verified"}, Outputs: []string{"verified report"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "plan-read-binding-plan")

	sourceCall := taskPlanBindingToolCall(fixture, current, "call_read_sources", "morde_read_task_attachment",
		taskPlanStepTestDigest("source request"))
	sourceResultDigest := taskPlanStepTestDigest("exact source result")
	reportCall := taskPlanBindingToolCall(fixture, current, "call_read_report", "morde_workspace_read",
		taskPlanStepTestDigest("report request"))
	reportResultDigest := taskPlanStepTestDigest("exact report result")

	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessMissingBinding)
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("admit dependent read before its source settles = %v; want conflict", err)
	}
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall); err != nil {
		t.Fatalf("admit source read: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessEvidenceMissing)
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall, sourceResultDigest); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("settle read without a TaskStep = %v; want conflict", err)
	}

	sourceStepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, sourceCall,
		"read-source-running", task.StepRunning, "")
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall, sourceResultDigest); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("settle running read TaskStep = %v; want conflict", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessPending)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='failed' WHERE task_id=$1 AND id=$2`, current.ID, sourceStepID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessFailed)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='unknown' WHERE task_id=$1 AND id=$2`, current.ID, sourceStepID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessUnknown)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id=$1 AND id=$2`, current.ID, sourceStepID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "report", false, planStepReadinessPending)
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("admit dependent read before source result digest settles = %v; want conflict", err)
	}
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall, sourceResultDigest); err != nil {
		t.Fatalf("settle successful source read: %v", err)
	}
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall); err != nil {
		t.Fatalf("replay source admission: %v", err)
	}
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall, sourceResultDigest); err != nil {
		t.Fatalf("replay source settlement with the same digest: %v", err)
	}

	var executionID, storedCallID, storedSessionID string
	var storedRequestDigest, storedResultDigest []byte
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT execution_id,call_id,agent_session_id,request_digest,result_digest
FROM task_plan_step_execution_bindings WHERE task_id=$1 AND execution_kind='read_tool' AND call_id=$2`,
		current.ID, sourceCall.CallKey).Scan(&executionID, &storedCallID, &storedSessionID, &storedRequestDigest, &storedResultDigest); err != nil {
		t.Fatalf("read settled source binding: %v", err)
	}
	if executionID != taskPlanReadToolExecutionID(sourceCall.AgentSessionID, sourceCall.CallKey) || executionID == sourceCall.CallKey ||
		executionID == taskPlanReadToolExecutionID("another-session", sourceCall.CallKey) || storedCallID != sourceCall.CallKey ||
		storedSessionID != sourceCall.AgentSessionID || !bytes.Equal(storedRequestDigest, sourceCall.RequestDigest) ||
		!bytes.Equal(storedResultDigest, sourceResultDigest) {
		t.Fatalf("settled read binding identity execution=%q call=%q session=%q request=%x result=%x; want exact hashed identity and digests",
			executionID, storedCallID, storedSessionID, storedRequestDigest, storedResultDigest)
	}
	wrongRequest := sourceCall
	wrongRequest.RequestDigest = taskPlanStepTestDigest("changed source request")
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", wrongRequest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reuse read call with a different request digest = %v; want idempotency conflict", err)
	}
	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "report", sourceCall); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reuse read call for another plan step = %v; want idempotency conflict", err)
	}
	changedSourceDigest := taskPlanStepTestDigest("changed source result")
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "sources", sourceCall, changedSourceDigest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("replay settled source read with a different result digest = %v; want idempotency conflict", err)
	}

	if err := fixture.route.store.AdmitCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall); err != nil {
		t.Fatalf("admit dependent report read after source settlement: %v", err)
	}
	reportStepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, reportCall,
		"read-report-success", task.StepSucceeded, "")
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='failed' WHERE task_id=$1 AND id=$2`, current.ID, sourceStepID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall, reportResultDigest); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("settle dependent read after its source failed = %v; want conflict", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id=$1 AND id=$2`, current.ID, sourceStepID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall, reportResultDigest); err != nil {
		t.Fatalf("settle successful report read: %v", err)
	}
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall, reportResultDigest); err != nil {
		t.Fatalf("replay report settlement with the same digest: %v", err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "final", true, "")

	// Readiness must survive a fresh Store object over the same durable database
	// state; it cannot rely on the original Store's in-memory state.
	recoveredStore := New(fixture.route.store.pool)
	recoveredDetails, err := recoveredStore.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		t.Fatalf("reload Task through a reconstructed Store: %v", err)
	}
	assertTaskPlanReadiness(t, recoveredStore, recoveredDetails.Task, 1, "final", true, "")

	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='failed' WHERE task_id=$1 AND id=$2`, current.ID, reportStepID); err != nil {
		t.Fatalf("change settled predecessor state: %v", err)
	}
	assertTaskPlanReadiness(t, recoveredStore, recoveredDetails.Task, 1, "final", false, planStepReadinessFailed)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id=$1 AND id=$2`, current.ID, reportStepID); err != nil {
		t.Fatalf("restore settled predecessor state: %v", err)
	}
	assertTaskPlanReadiness(t, recoveredStore, recoveredDetails.Task, 1, "final", true, "")
	if err := fixture.route.store.SettleCloudDSHTaskPlanStepRead(ctx, current, 1, "report", reportCall,
		taskPlanStepTestDigest("changed report result")); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("replay settled report read with a different result digest = %v; want idempotency conflict", err)
	}

	for _, test := range []struct {
		name      string
		statement string
		args      []any
		reason    string
	}{
		{name: "agent session changed", statement: `UPDATE task_plan_step_execution_bindings SET agent_session_id=$3
WHERE task_id=$1 AND execution_kind='read_tool' AND call_id=$2`,
			args: []any{current.ID, reportCall.CallKey, "other-session"}, reason: planStepReadinessEvidenceMissing},
		{name: "call id changed", statement: `UPDATE task_steps SET tool_call_key=$3 WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID, "call_read_report_changed"}, reason: planStepReadinessEvidenceMissing},
		{name: "request digest changed", statement: `UPDATE task_steps SET request_digest=$3 WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID, taskPlanStepTestDigest("tampered report request")}, reason: planStepReadinessEvidenceMissing},
		{name: "task step id changed", statement: `UPDATE task_steps SET id=$3 WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID, mustID(t, domain.NewStepID)}, reason: planStepReadinessEvidenceMissing},
		{name: "task revision changed", statement: `UPDATE task_steps SET task_revision=task_revision+1 WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID}, reason: planStepReadinessEvidenceMissing},
		{name: "step failed", statement: `UPDATE task_steps SET status='failed' WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID}, reason: planStepReadinessFailed},
		{name: "step unknown", statement: `UPDATE task_steps SET status='unknown' WHERE task_id=$1 AND id=$2`,
			args: []any{current.ID, reportStepID}, reason: planStepReadinessUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := fixture.route.store.pool.Exec(ctx, test.statement, test.args...); err != nil {
				t.Fatalf("tamper TaskStep: %v", err)
			}
			assertTaskPlanReadiness(t, fixture.route.store, current, 1, "final", false, test.reason)
			var restore string
			switch test.name {
			case "agent session changed":
				restore = `UPDATE task_plan_step_execution_bindings SET agent_session_id=$3
WHERE task_id=$1 AND execution_kind='read_tool' AND call_id=$2`
				_, err := fixture.route.store.pool.Exec(ctx, restore, current.ID, reportCall.CallKey, reportCall.AgentSessionID)
				if err != nil {
					t.Fatalf("restore agent session: %v", err)
				}
			case "call id changed":
				restore = `UPDATE task_steps SET tool_call_key=$3 WHERE task_id=$1 AND id=$2`
				_, err := fixture.route.store.pool.Exec(ctx, restore, current.ID, reportStepID, reportCall.CallKey)
				if err != nil {
					t.Fatalf("restore call ID: %v", err)
				}
			case "request digest changed":
				restore = `UPDATE task_steps SET request_digest=$3 WHERE task_id=$1 AND id=$2`
				_, err := fixture.route.store.pool.Exec(ctx, restore, current.ID, reportStepID, reportCall.RequestDigest)
				if err != nil {
					t.Fatalf("restore request digest: %v", err)
				}
			case "task step id changed":
				var changedID string
				if err := fixture.route.store.pool.QueryRow(ctx, `SELECT id FROM task_steps WHERE task_id=$1 AND tool_call_key=$2`,
					current.ID, reportCall.CallKey).Scan(&changedID); err != nil {
					t.Fatalf("read changed TaskStep ID: %v", err)
				}
				if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET id=$3 WHERE task_id=$1 AND id=$2`,
					current.ID, changedID, reportStepID); err != nil {
					t.Fatalf("restore TaskStep ID: %v", err)
				}
			case "task revision changed":
				if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET task_revision=$3 WHERE task_id=$1 AND id=$2`,
					current.ID, reportStepID, *current.CurrentRevision); err != nil {
					t.Fatalf("restore TaskStep revision: %v", err)
				}
			case "step failed", "step unknown":
				if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id=$1 AND id=$2`,
					current.ID, reportStepID); err != nil {
					t.Fatalf("restore TaskStep status: %v", err)
				}
			}
		})
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "final", true, "")

	plan.Revision = 2
	plan.Goal = "Prepare the revised report"
	latest := commitTaskPlanForStepBinding(t, fixture, plan, "plan-read-binding-plan-2")
	assertTaskPlanReadiness(t, fixture.route.store, latest, 1, "final", false, planStepReadinessPlanRevisionStale)
}
