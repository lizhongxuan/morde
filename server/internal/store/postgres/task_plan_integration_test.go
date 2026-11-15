package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresCloudDSHTaskPlanPersistsReplaysAndFencesRevisions(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed {
		t.Fatalf("start Agent instance: replayed=%t err=%v", replayed, err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || details.Task.CurrentRevision == nil || details.Task.ExecutionLocation != "server" {
		t.Fatalf("load running server Task: details=%+v err=%v", details.Task, err)
	}
	taskRevision := *details.Task.CurrentRevision
	plan := task.Plan{TaskRevision: taskRevision, Revision: 1, Goal: "Prepare report", CostEstimateState: "unknown", Steps: []task.PlanStep{
		{ID: "sources", Title: "Collect sources", DependsOn: []string{}, Outputs: []string{"source notes"},
			CompletionCriteria: []string{"sources are cited"}, Status: task.PlanStepPending},
	}}
	firstStepID := mustID(t, domain.NewStepID)
	firstProgress := postgresCloudDSHTaskPlanProgress(t, details.Task, lease.Fence, plan, "plan-call-1", firstStepID, 1)

	var workers sync.WaitGroup
	results := make(chan task.Event, 2)
	errors := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			event, err := f.store.CommitTaskProgress(ctx, firstProgress)
			results <- event
			errors <- err
		}()
	}
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent task plan commit: %v", err)
		}
	}
	var committed task.Event
	for event := range results {
		if committed.Sequence == 0 {
			committed = event
		} else {
			var firstPayload, nextPayload any
			if json.Unmarshal(committed.Payload, &firstPayload) != nil || json.Unmarshal(event.Payload, &nextPayload) != nil ||
				event.Sequence != committed.Sequence || event.Type != committed.Type || !reflect.DeepEqual(firstPayload, nextPayload) {
				t.Fatalf("concurrent replay returned a different event identity or payload: first=%d/%s next=%d/%s",
					committed.Sequence, committed.Type, event.Sequence, event.Type)
			}
		}
	}
	if committed.Type != "task.plan" || committed.Sequence != details.Task.EventSequence+1 {
		t.Fatalf("first plan event = %+v; want one event after sequence %d", committed, details.Task.EventSequence)
	}

	changedPlan := plan
	changedPlan.Goal = "Changed report"
	changed := postgresCloudDSHTaskPlanProgress(t, details.Task, lease.Fence, changedPlan, "plan-call-1", firstStepID, 1)
	if _, err := f.store.CommitTaskProgress(ctx, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed replay call was not fenced: %v", err)
	}

	stalePlan := plan
	staleStepID := mustID(t, domain.NewStepID)
	stale := postgresCloudDSHTaskPlanProgress(t, details.Task, lease.Fence, stalePlan, "plan-call-stale", staleStepID, 2)
	if _, err := f.store.CommitTaskProgress(ctx, stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale plan revision error = %v, want conflict", err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || len(current.Steps) != 1 || current.Task.Usage.ToolCalls != details.Task.Usage.ToolCalls+1 {
		t.Fatalf("duplicate or stale plan changed the Task: steps=%d usage=%+v err=%v", len(current.Steps), current.Task.Usage, err)
	}
	revised := plan
	revised.Revision = 2
	revised.Goal = "Prepare a revised report"
	revised.Steps = append([]task.PlanStep(nil), plan.Steps...)
	revised.Steps[0].Status = task.PlanStepRunning
	secondProgress := postgresCloudDSHTaskPlanProgress(t, current.Task, lease.Fence, revised,
		"plan-call-2", mustID(t, domain.NewStepID), 2)
	second, err := f.store.CommitTaskProgress(ctx, secondProgress)
	if err != nil || second.Type != "task.plan" || second.Sequence != committed.Sequence+1 {
		t.Fatalf("commit revised plan: event=%+v err=%v", second, err)
	}
	current, err = f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Plan == nil || current.Plan.Revision != 2 || current.Plan.Goal != revised.Goal ||
		current.Task.EventSequence != second.Sequence || len(current.Steps) != 2 || current.Task.Usage.ToolCalls != details.Task.Usage.ToolCalls+2 {
		t.Fatalf("current plan projection = %+v task=%+v steps=%d err=%v", current.Plan, current.Task, len(current.Steps), err)
	}
	var planEvents int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.plan'`, lease.Fence.TaskID).Scan(&planEvents); err != nil || planEvents != 2 {
		t.Fatalf("append-only plan event count = %d, err=%v", planEvents, err)
	}
}

func postgresCloudDSHTaskPlanProgress(t *testing.T, current task.Task, fence app.AgentInstanceFence,
	plan task.Plan, callID, stepID string, stepSequence int64) app.TaskProgress {
	t.Helper()
	commit := app.CloudDSHTaskPlanCommit{TaskID: current.ID, OwnerID: current.OwnerID, AgentSessionID: fence.AgentSessionID,
		AuthorizationVersion: fence.AuthorizationVersion, CallID: callID, Plan: plan}
	commit.FingerprintSHA256 = postgresCloudDSHTaskPlanFingerprint(commit)
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: stepSequence, Kind: "tool", Status: task.StepSucceeded,
		Title: "Update task plan", Summary: "Saved the current advisory plan revision."}
	payload, err := json.Marshal(map[string]any{
		"step_id": step.ID, "sequence": step.Sequence, "task_revision": plan.TaskRevision, "plan_revision": plan.Revision, "plan": plan,
		"detail": map[string]any{"tool": "morde_task_plan_update", "call_id": callID, "agent_session_id": fence.AgentSessionID,
			"authorization_version": fence.AuthorizationVersion, "fingerprint_sha256": commit.FingerprintSHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	usage := current.Usage
	usage.ToolCalls++
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s", commit.TaskID, callID, plan.Revision, plan.Goal)))
	return app.TaskProgress{TaskID: current.ID, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: step.ID, Result: current.Result, Usage: usage, Steps: []task.Step{step},
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: fence.AgentSessionID, AgentInstanceID: fence.AgentInstanceID,
			AgentGeneration: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion,
			TaskRevision: plan.TaskRevision, CallKey: callID, ToolName: "morde_task_plan_update", RequestDigest: digest[:],
			Phase: string(task.StepSucceeded), DetailState: "not_recorded"},
		Event: task.Event{TaskID: current.ID, Type: "task.plan", Payload: payload}, CloudDSHTaskPlan: &commit}
}

func postgresCloudDSHTaskPlanFingerprint(value app.CloudDSHTaskPlanCommit) string {
	value.FingerprintSHA256 = ""
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
