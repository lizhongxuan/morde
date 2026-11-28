package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresAgentChildPlanBindingIsAtomicVersionedAndIdempotent(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "agent-child-plan-binding", false)
	ctx := context.Background()
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Collect and write a report",
		Scope: "Use approved sources", VerificationMethod: "Check the result", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "source", Title: "Collect sources", Status: task.PlanStepPending, CompletionCriteria: []string{"sources are recorded"}},
			{ID: "write", Title: "Write report", Status: task.PlanStepPending, DependsOn: []string{"source"},
				CompletionCriteria: []string{"report is checked"}},
			{ID: "review", Title: "Review sources", Status: task.PlanStepPending, CompletionCriteria: []string{"review is recorded"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "agent-child-plan-binding-plan")
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, current.ID, current.OwnerID, fixture.fence.AgentSessionID)
	newCommand := func(spawnKey, stepID string, revision int64) app.AgentChildSessionCommand {
		return app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
			SpawnKey: spawnKey, PlanStepID: stepID, PlanRevision: revision, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"title":"Collect sources","objective":"Collect source notes"}`),
			Title:     "Collect sources", Summary: "Collect source notes", AllowedTools: []string{"workspace_read"},
		}
	}
	assertNoChild := func(spawnKey string) {
		t.Helper()
		var count int
		if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND spawn_key=$2`,
			current.ID, spawnKey).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected plan-bound spawn %q left %d child rows: %v", spawnKey, count, err)
		}
	}

	if _, _, err := service.Create(ctx, newCommand("missing_plan_selector", "", 0)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("plan-backed child accepted missing selectors: %v", err)
	}
	assertNoChild("missing_plan_selector")
	if _, _, err := service.Create(ctx, newCommand("stale_plan_selector", "source", 2)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("child accepted a stale plan revision: %v", err)
	}
	assertNoChild("stale_plan_selector")
	if _, _, err := service.Create(ctx, newCommand("unready_plan_selector", "write", 1)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("child bypassed an unsettled plan dependency: %v", err)
	}
	assertNoChild("unready_plan_selector")

	command := newCommand("bound_source_child", "source", 1)
	child, replayed, err := service.Create(ctx, command)
	if err != nil || replayed || child.SessionID == "" || child.Step.Status != task.StepRunning {
		t.Fatalf("create plan-bound child: child=%+v replayed=%t err=%v", child, replayed, err)
	}
	var boundStepID, executionID, parentSessionID, spawnKey string
	var boundTaskRevision, boundPlanRevision int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT plan_step_id,execution_id,agent_session_id,call_id,task_revision,plan_revision
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND execution_kind='child_session' AND execution_id=$3`,
		current.OwnerID, current.ID, child.SessionID).Scan(&boundStepID, &executionID, &parentSessionID, &spawnKey,
		&boundTaskRevision, &boundPlanRevision); err != nil {
		t.Fatalf("read atomic child plan binding: %v", err)
	}
	if boundStepID != "source" || executionID != child.SessionID || parentSessionID != fixture.fence.AgentSessionID ||
		spawnKey != command.SpawnKey || boundTaskRevision != *current.CurrentRevision || boundPlanRevision != 1 {
		t.Fatalf("persisted child plan binding does not match selectors and child: step=%q execution=%q parent=%q spawn=%q task_rev=%d plan_rev=%d",
			boundStepID, executionID, parentSessionID, spawnKey, boundTaskRevision, boundPlanRevision)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "write", false, planStepReadinessPending)

	replayedChild, replayed, err := service.Create(ctx, command)
	if err != nil || !replayed || replayedChild.SessionID != child.SessionID || replayedChild.Step.ID != child.Step.ID {
		t.Fatalf("replay exact plan-bound child: child=%+v replayed=%t err=%v", replayedChild, replayed, err)
	}
	var bindingCount int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE owner_id=$1 AND task_id=$2 AND execution_kind='child_session' AND execution_id=$3`, current.OwnerID, current.ID, child.SessionID).Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("child replay changed plan binding count to %d: %v", bindingCount, err)
	}
	// Use an independent ready step here so the call reaches the spawn-key
	// idempotency check instead of being rejected earlier by write's unsettled
	// source dependency.
	changedStep := newCommand(command.SpawnKey, "review", 1)
	if _, _, err := service.Create(ctx, changedStep); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("existing spawn accepted a different plan step selector: %v", err)
	}
}

func TestPostgresAgentChildPlanBindingKeepsNoPlanSpawnBehavior(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "agent-child-no-plan", false)
	ctx := context.Background()
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, fixture.fence.AgentSessionID)
	child, replayed, err := service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: fixture.fence.AgentSessionID, SpawnKey: "legacy_no_plan_spawn", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}})
	if err != nil || replayed || child.SessionID == "" {
		t.Fatalf("no-plan child spawn behavior changed: child=%+v replayed=%t err=%v", child, replayed, err)
	}
	var bindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings
WHERE owner_id=$1 AND task_id=$2 AND execution_id=$3`, fixture.task.OwnerID, fixture.task.ID, child.SessionID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("no-plan child unexpectedly created plan binding rows: count=%d err=%v", bindings, err)
	}
}
