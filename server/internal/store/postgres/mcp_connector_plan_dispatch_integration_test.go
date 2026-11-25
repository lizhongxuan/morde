package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresMCPPlanDispatchBindsAtomicallyAndReplays(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "mcp-plan-dispatch-atomic", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Find sources",
		Scope: "Search for sources", VerificationMethod: "Inspect returned sources", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Search sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources found"}},
			{ID: "summary", Title: "Summarize sources", Status: task.PlanStepPending, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"summary checked"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-plan-dispatch-plan")
	toolCall, operation := mcpPlanDispatchOperationFixture(t, fixture, current, "mcp_plan_dispatch_call")
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare MCP plan operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall, "mcp-plan-dispatch-step", task.StepRunning, "")
	dispatch := prepared
	dispatch.PlanRevision, dispatch.PlanStepID = 1, "sources"
	dispatched, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, dispatch, current.Lease.Owner)
	if err != nil || dispatched.Status != task.OperationDispatched || dispatched.ID != prepared.ID {
		t.Fatalf("atomic MCP plan dispatch = %#v err=%v", dispatched, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND task_revision=$2 AND plan_revision=1 AND plan_step_id='sources' AND execution_kind='mcp_connector'`,
		current.ID, *current.CurrentRevision).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("MCP plan binding count=%d err=%v; want one", bindings, err)
	}

	replayedDispatch, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, dispatch, current.Lease.Owner)
	if err != nil || replayedDispatch.ID != dispatched.ID || replayedDispatch.Status != task.OperationDispatched {
		t.Fatalf("exact MCP plan dispatch replay = %#v err=%v", replayedDispatch, err)
	}
	changedSelector := dispatch
	changedSelector.PlanStepID = "summary"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, changedSelector, current.Lease.Owner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("MCP dispatch replay with a different plan step error=%v; want idempotency conflict", err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE task_id=$1 AND task_revision=$2 AND plan_revision=1`, current.ID, *current.CurrentRevision).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("replay changed MCP binding count=%d err=%v; want one", bindings, err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='failed' WHERE id=$1`, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summary", false, planStepReadinessFailed)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='unknown' WHERE id=$1`, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summary", false, planStepReadinessUnknown)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='succeeded' WHERE id=$1`, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id=$1 AND tool_call_key=$2`,
		current.ID, toolCall.CallKey); err != nil {
		t.Fatal(err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "summary", true, "")
}

func TestPostgresMCPPlanDispatchBlocksMissingDependencyBinding(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "mcp-plan-dispatch-dependency", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Prepare a summary",
		Scope: "Search then summarize", VerificationMethod: "Check the summary", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "sources", Title: "Search sources", Status: task.PlanStepCompleted, CompletionCriteria: []string{"sources found"}},
			{ID: "summary", Title: "Summarize sources", Status: task.PlanStepCompleted, DependsOn: []string{"sources"},
				CompletionCriteria: []string{"summary checked"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-plan-dependency-plan")
	toolCall, operation := mcpPlanDispatchOperationFixture(t, fixture, current, "mcp_plan_dependency_call")
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare dependency-blocked MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall, "mcp-plan-dependency-step", task.StepRunning, "")
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("MCP dispatch without plan selectors error=%v; want conflict", err)
	}
	blocked := prepared
	blocked.PlanRevision, blocked.PlanStepID = 1, "summary"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, blocked, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dependency-blocked MCP dispatch error=%v; want conflict", err)
	}
	stored, err := fixture.route.store.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, operation.ConnectorID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("blocked MCP operation status=%q err=%v; want prepared", stored.Status, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("blocked dispatch persisted %d MCP bindings, err=%v; want none", bindings, err)
	}
}

func TestPostgresMCPPlanDispatchRollsBackBindingWhenDispatchUpdateFails(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "mcp-plan-dispatch-rollback", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Search sources",
		Scope: "Search for sources", VerificationMethod: "Inspect returned sources", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "sources", Title: "Search sources", Status: task.PlanStepCompleted,
			CompletionCriteria: []string{"sources found"}}}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-plan-rollback-plan")
	toolCall, operation := mcpPlanDispatchOperationFixture(t, fixture, current, "mcp_plan_rollback_call")
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare rollback MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall, "mcp-plan-rollback-step", task.StepRunning, "")
	if _, err := fixture.route.store.pool.Exec(ctx, `ALTER TABLE task_mcp_connector_operations
ADD CONSTRAINT mcp_plan_dispatch_rollback_test CHECK (status <> 'dispatched') NOT VALID`); err != nil {
		t.Fatalf("install dispatch-failure constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := fixture.route.store.pool.Exec(context.Background(), `ALTER TABLE task_mcp_connector_operations
DROP CONSTRAINT IF EXISTS mcp_plan_dispatch_rollback_test`); err != nil {
			t.Errorf("drop dispatch-failure constraint: %v", err)
		}
	})
	dispatch := prepared
	dispatch.PlanRevision, dispatch.PlanStepID = 1, "sources"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, dispatch, current.Lease.Owner); err == nil {
		t.Fatal("dispatch unexpectedly passed the test constraint")
	}
	stored, err := fixture.route.store.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, operation.ConnectorID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("rolled-back MCP operation status=%q err=%v; want prepared", stored.Status, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("failed dispatch left %d MCP bindings, err=%v; want none", bindings, err)
	}
}

func TestPostgresMCPPlanDispatchRejectsStalePlanRevision(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "mcp-plan-dispatch-stale", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Search sources",
		Scope: "Search for sources", VerificationMethod: "Inspect returned sources", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "sources", Title: "Search sources", Status: task.PlanStepPending,
			CompletionCriteria: []string{"sources found"}}}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "mcp-plan-stale-plan-1")
	toolCall, operation := mcpPlanDispatchOperationFixture(t, fixture, current, "mcp_plan_stale_call")
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare stale MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall, "mcp-plan-stale-step", task.StepRunning, "")
	plan.Revision = 2
	plan.Goal = "Search the revised scope"
	commitTaskPlanForStepBinding(t, fixture, plan, "mcp-plan-stale-plan-2")
	stale := prepared
	stale.PlanRevision, stale.PlanStepID = 1, "sources"
	if _, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, stale, current.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale MCP plan dispatch error=%v; want conflict", err)
	}
	stored, err := fixture.route.store.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, operation.ConnectorID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("stale MCP operation status=%q err=%v; want prepared", stored.Status, err)
	}
}

func TestPostgresMCPPlanDispatchWithoutPlanPreservesBehavior(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "mcp-plan-dispatch-no-plan", false)
	ctx := context.Background()
	current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	toolCall, operation := mcpPlanDispatchOperationFixture(t, fixture, current.Task, "mcp_no_plan_dispatch_call")
	prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, current.Task.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare no-plan MCP operation = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current.Task, toolCall, "mcp-no-plan-dispatch-step", task.StepRunning, "")
	dispatched, err := fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, current.Task.Lease.Owner)
	if err != nil || dispatched.Status != task.OperationDispatched {
		t.Fatalf("no-plan MCP dispatch = %#v err=%v", dispatched, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1`, current.Task.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("no-plan MCP dispatch created %d plan bindings, err=%v; want none", bindings, err)
	}
}

func mcpPlanDispatchOperationFixture(t *testing.T, fixture s03AgentTaskFixture, current task.Task,
	callID string) (app.TaskToolCallProgress, app.MCPConnectorOperation) {
	t.Helper()
	toolName := "morde_web_search"
	requestDigest := sha256.Sum256([]byte("MCP plan dispatch request:" + callID))
	toolCall := taskPlanBindingToolCall(fixture, current, callID, toolName, requestDigest[:])
	operation := app.MCPConnectorOperation{
		ID: mustID(t, domain.NewOperationID), TaskID: current.ID, OwnerID: current.OwnerID,
		ConnectorID: app.BuiltinSearchMCPConnectorID, Key: app.AgentOperationKey(callID), TaskRevision: current.CurrentRevision,
		CallID: callID, ToolName: toolName, ConnectorRevision: 1, GrantVersion: current.Authorization.Version,
		LeaseVersion: current.Lease.Version, RequestHash: requestDigest[:], Status: task.OperationPrepared,
	}
	return toolCall, operation
}
