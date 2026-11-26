package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestStep5IndependentAgentChildSpawnsPersistDistinctSessionsStepsAndEvents(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "step5-independent-child-spawns", false)
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, fixture.fence.AgentSessionID)
	makeCommand := func(spawnKey string) app.AgentChildSessionCommand {
		return app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
			SpawnKey: spawnKey, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"objective":"inspect an independent source"}`), Title: "Inspect source",
			Summary: "Collect independent notes", AllowedTools: []string{"workspace_read"}}
	}
	type result struct {
		child    app.AgentChildSession
		replayed bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, spawnKey := range []string{"step5_independent_a", "step5_independent_b"} {
		command := makeCommand(spawnKey)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			child, replayed, err := service.Create(ctx, command)
			results <- result{child: child, replayed: replayed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	children := make([]app.AgentChildSession, 0, 2)
	for value := range results {
		if value.err != nil || value.replayed {
			t.Fatalf("independent child spawn failed or replayed: child=%+v replayed=%t err=%v", value.child, value.replayed, value.err)
		}
		children = append(children, value.child)
	}
	if len(children) != 2 {
		t.Fatalf("independent child spawn results=%d; want 2", len(children))
	}
	if children[0].SessionID == children[1].SessionID || children[0].Step.ID == children[1].Step.ID ||
		children[0].Step.Sequence == children[1].Step.Sequence {
		t.Fatalf("independent children shared a session or step identity: first=%+v second=%+v", children[0], children[1])
	}
	firstEventSequence := assertStep5AgentChildPersisted(t, fixture.route.store, fixture.task.ID,
		fixture.fence.AgentSessionID, children[0])
	secondEventSequence := assertStep5AgentChildPersisted(t, fixture.route.store, fixture.task.ID,
		fixture.fence.AgentSessionID, children[1])
	if firstEventSequence == secondEventSequence {
		t.Fatalf("independent children shared task event sequence %d", firstEventSequence)
	}
}

func TestStep5ConcurrentIdenticalAgentChildSpawnCreatesOneChild(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "step5-identical-child-spawn", false)
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, fixture.fence.AgentSessionID)
	command := app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
		SpawnKey: "step5_same_spawn", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect this source once"}`), Title: "Inspect source",
		Summary: "Collect source notes", AllowedTools: []string{"workspace_read"}}
	type result struct {
		child    app.AgentChildSession
		replayed bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			child, replayed, err := service.Create(ctx, command)
			results <- result{child: child, replayed: replayed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	created, replayedCount := 0, 0
	var first app.AgentChildSession
	for value := range results {
		if value.err != nil {
			t.Fatalf("identical concurrent child spawn: %v", value.err)
		}
		if first.SessionID == "" {
			first = value.child
		} else if value.child.SessionID != first.SessionID || value.child.Step.ID != first.Step.ID {
			t.Fatalf("identical spawn returned different child identities: first=%+v next=%+v", first, value.child)
		}
		if value.replayed {
			replayedCount++
		} else {
			created++
		}
	}
	if first.SessionID == "" || created != 1 || replayedCount != 1 {
		t.Fatalf("identical concurrent spawn created=%d replayed=%d child=%+v; want one create and one replay", created, replayedCount, first)
	}
	assertStep5AgentChildPersisted(t, fixture.route.store, fixture.task.ID, fixture.fence.AgentSessionID, first)
	var childCount, stepCount, eventCount int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND parent_session_id=$3 AND spawn_key=$4`,
		fixture.task.ID, fixture.task.OwnerID, fixture.fence.AgentSessionID, command.SpawnKey).Scan(&childCount); err != nil || childCount != 1 {
		t.Fatalf("identical spawn child rows=%d err=%v; want 1", childCount, err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND id=$2`,
		fixture.task.ID, first.Step.ID).Scan(&stepCount); err != nil || stepCount != 1 {
		t.Fatalf("identical spawn step rows=%d err=%v; want 1", stepCount, err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events
WHERE task_id=$1 AND event_type='task.step' AND payload->>'phase'='subagent_started' AND payload->>'session_id'=$2`,
		fixture.task.ID, first.SessionID).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("identical spawn started events=%d err=%v; want 1", eventCount, err)
	}
}

func TestStep5PlanBoundChildSuccessUnlocksAndUnknownBlocksDependencies(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "step5-child-plan-dependencies", false)
	plan := task.Plan{TaskRevision: *fixture.task.CurrentRevision, Revision: 1, Goal: "Collect and verify a report",
		Scope: "Use approved sources", VerificationMethod: "Check each child result", CostEstimateState: "unknown",
		Steps: []task.PlanStep{
			{ID: "source", Title: "Collect sources", Status: task.PlanStepPending, CompletionCriteria: []string{"source notes are recorded"}},
			{ID: "write", Title: "Write report", Status: task.PlanStepPending, DependsOn: []string{"source"},
				CompletionCriteria: []string{"report is written"}},
			{ID: "review", Title: "Review report", Status: task.PlanStepPending, DependsOn: []string{"write"},
				CompletionCriteria: []string{"report is reviewed"}},
		}}
	current := commitTaskPlanForStepBinding(t, fixture, plan, "step5-child-plan")
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, current.ID, current.OwnerID, fixture.fence.AgentSessionID)
	makeCommand := func(spawnKey, planStepID string) app.AgentChildSessionCommand {
		return app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
			SpawnKey: spawnKey, PlanStepID: planStepID, PlanRevision: 1, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(fmt.Sprintf(`{"objective":"execute plan step %s"}`, planStepID)),
			Title:     "Execute " + planStepID, Summary: "Execute the selected plan step", AllowedTools: []string{"workspace_read"}}
	}
	assertNoChild := func(spawnKey string) {
		t.Helper()
		var count int
		if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND spawn_key=$2`,
			current.ID, spawnKey).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected plan-bound spawn %q left %d child rows: %v", spawnKey, count, err)
		}
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "write", false, planStepReadinessMissingBinding)

	source, replayed, err := service.Create(ctx, makeCommand("step5_source_child", "source"))
	if err != nil || replayed {
		t.Fatalf("create source child: child=%+v replayed=%t err=%v", source, replayed, err)
	}
	checkpoints, err := app.NewAgentCheckpointService(fixture.route.store, step5AgentCheckpointSealer(t))
	if err != nil {
		t.Fatal(err)
	}
	digest := saveStep5AgentChildCheckpoint(t, ctx, checkpoints, fixture, source)
	settledSource, replayed, err := service.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: fixture.fence,
		ActorSessionID: fixture.fence.AgentSessionID, SessionID: source.SessionID, Status: task.StepSucceeded,
		CheckpointThrough: 0, CheckpointDigest: digest, Summary: "Source notes collected."})
	if err != nil || replayed || settledSource.Step.Status != task.StepSucceeded {
		t.Fatalf("settle source child: child=%+v replayed=%t err=%v", settledSource, replayed, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "write", true, "")

	write, replayed, err := service.Create(ctx, makeCommand("step5_write_child", "write"))
	if err != nil || replayed {
		t.Fatalf("create dependency-unlocked child: child=%+v replayed=%t err=%v", write, replayed, err)
	}
	unknown, replayed, err := service.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: fixture.fence,
		ActorSessionID: fixture.fence.AgentSessionID, SessionID: write.SessionID, Status: task.StepUnknown,
		CheckpointThrough: -1, Summary: "The write result could not be confirmed."})
	if err != nil || replayed || unknown.Step.Status != task.StepUnknown {
		t.Fatalf("settle write child unknown: child=%+v replayed=%t err=%v", unknown, replayed, err)
	}
	assertTaskPlanReadiness(t, fixture.route.store, current, 1, "review", false, planStepReadinessUnknown)
	if _, _, err := service.Create(ctx, makeCommand("step5_review_child", "review")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unknown dependency allowed downstream child creation: %v", err)
	}
	assertNoChild("step5_review_child")
}

func assertStep5AgentChildPersisted(t *testing.T, store *Store, taskID, parentSessionID string,
	child app.AgentChildSession) int64 {
	t.Helper()
	ctx := context.Background()
	var sessionCount, stepCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=(SELECT owner_id FROM tasks WHERE id=$1) AND session_id=$2
 AND parent_session_id=$3 AND spawn_key=$4 AND step_id=$5`,
		taskID, child.SessionID, parentSessionID, child.SpawnKey, child.Step.ID).Scan(&sessionCount); err != nil || sessionCount != 1 {
		t.Fatalf("child %q session rows=%d err=%v; want 1", child.SessionID, sessionCount, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND id=$2 AND status='running'`,
		taskID, child.Step.ID).Scan(&stepCount); err != nil || stepCount != 1 {
		t.Fatalf("child %q running step rows=%d err=%v; want 1", child.SessionID, stepCount, err)
	}
	var eventCount int
	var eventStepID string
	var eventSequence int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*),min(payload->>'step_id'),min(sequence)
FROM task_events WHERE task_id=$1 AND event_type='task.step' AND payload->>'phase'='subagent_started'
 AND payload->>'session_id'=$2`, taskID, child.SessionID).Scan(&eventCount, &eventStepID, &eventSequence); err != nil ||
		eventCount != 1 || eventStepID != child.Step.ID {
		t.Fatalf("child %q start events=%d step=%q sequence=%d err=%v; want one matching event", child.SessionID, eventCount, eventStepID, eventSequence, err)
	}
	return eventSequence
}

func step5AgentCheckpointSealer(t *testing.T) *app.Sealer {
	t.Helper()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

func saveStep5AgentChildCheckpoint(t *testing.T, ctx context.Context, checkpoints *app.AgentCheckpointService,
	fixture s03AgentTaskFixture, child app.AgentChildSession) []byte {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"child work completed"}]}`,
		child.SessionID))
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{
		TaskID: fixture.task.ID, OwnerID: fixture.task.OwnerID, InitiatorID: fixture.fence.InitiatorID,
		AgentInstanceID: fixture.fence.AgentInstanceID, AgentGeneration: fixture.fence.AgentGeneration,
		AgentSessionID: child.SessionID, AgentRootSessionID: fixture.fence.AgentSessionID,
		AuthorizationVersion: fixture.fence.AuthorizationVersion, LeaseOwner: fixture.fence.LeaseOwner,
		LeaseVersion: fixture.fence.LeaseVersion, EngineVersion: fixture.fence.EngineVersion,
		ProfileVersion: fixture.fence.ProfileVersion}, From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("save child checkpoint: %v", err)
	}
	return digest[:]
}
