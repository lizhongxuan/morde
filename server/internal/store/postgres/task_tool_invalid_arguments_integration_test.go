package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskToolInvalidArgumentsPersistsSanitizedPrepareEvidence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil || current.Task.Status != task.StatusRunning {
		t.Fatalf("load running Task revision: task=%+v err=%v", current.Task, err)
	}
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}

	const toolName = "morde_web_search"
	const callKey = "call-invalid-arguments-1"
	const rawArguments = `{"query":"private-invalid-argument-marker-7a5f"}`
	digest := sha256.Sum256([]byte("morde-dsh-invalid-runtime-tool-arguments-v1\x00" + toolName + "\x00" + rawArguments))
	makeProgress := func(id string, stepSequence int64, requestDigest []byte) app.TaskProgress {
		step := task.Step{ID: id, TaskID: current.Task.ID, Sequence: stepSequence, Kind: "tool", Status: task.StepFailed,
			Title: "Prepare web search", Summary: "The tool arguments were invalid; the action was not executed.",
			Phase: "prepare", DetailState: "not_recorded"}
		usage := current.Task.Usage
		usage.ToolCalls++
		payload, err := json.Marshal(map[string]any{
			"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind, "status": step.Status,
			"title": step.Title, "summary": step.Summary,
			"detail": map[string]any{
				"tool": toolName, "phase": "prepare", "failed": true,
				"failure_reason": "invalid_tool_arguments",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return app.TaskProgress{
			TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
			Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: payload},
			ToolCall: &app.TaskToolCallProgress{
				AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
				AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
				TaskRevision: *current.Task.CurrentRevision, CallKey: callKey, ToolName: toolName,
				RequestDigest: append([]byte(nil), requestDigest...), Phase: "prepare", DetailState: "not_recorded",
				FailureReason: "", Retryable: false,
			},
		}
	}

	committedEvent, err := f.store.CommitTaskProgress(ctx, makeProgress(stepID, sequence, digest[:]))
	if err != nil {
		t.Fatalf("persist invalid tool arguments prepare evidence: %v", err)
	}
	if committedEvent.Sequence < 1 {
		t.Fatalf("committed task event sequence = %d", committedEvent.Sequence)
	}

	details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Task.Status != task.StatusRunning || details.Task.Usage.ToolCalls != current.Task.Usage.ToolCalls+1 {
		t.Fatalf("Task status/usage after prepare failure = %s/%d, want running/%d",
			details.Task.Status, details.Task.Usage.ToolCalls, current.Task.Usage.ToolCalls+1)
	}
	var persisted *task.Step
	for i := range details.Steps {
		if details.Steps[i].ID == stepID {
			persisted = &details.Steps[i]
			break
		}
	}
	if persisted == nil || persisted.Status != task.StepFailed || persisted.Phase != "prepare" ||
		persisted.FailureReason != "" || persisted.Retryable || persisted.ToolCallKey != callKey ||
		persisted.ToolName != toolName ||
		persisted.TaskRevision == nil || *persisted.TaskRevision != *current.Task.CurrentRevision ||
		!bytes.Equal(persisted.RequestDigest, digest[:]) {
		t.Fatalf("persisted invalid-arguments TaskStep = %#v", persisted)
	}
	if persisted.DetailState != "not_recorded" {
		t.Fatalf("invalid-arguments TaskStep detail state = %q, want not_recorded", persisted.DetailState)
	}

	var persistedPayload []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND sequence=$2 AND event_type='task.step'`,
		current.Task.ID, committedEvent.Sequence).Scan(&persistedPayload); err != nil {
		t.Fatalf("load committed task.step event: %v", err)
	}
	var event struct {
		Detail map[string]any `json:"detail"`
	}
	if err := json.Unmarshal(persistedPayload, &event); err != nil {
		t.Fatalf("decode persisted task event: %v", err)
	}
	if event.Detail["failure_reason"] != "invalid_tool_arguments" || event.Detail["phase"] != "prepare" ||
		strings.Contains(string(persistedPayload), rawArguments) || strings.Contains(string(persistedPayload), "private-invalid-argument-marker-7a5f") {
		t.Fatalf("persisted event detail is not stable and sanitized: %s", persistedPayload)
	}

	assertNoDuplicateCommit := func(progress app.TaskProgress, description string) {
		t.Helper()
		if _, err := f.store.CommitTaskProgress(ctx, progress); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			t.Fatalf("%s replay error = %v, want idempotency conflict", description, err)
		}
		var sameCallSteps, eventCount int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND agent_session_id=$2 AND tool_call_key=$3`,
			current.Task.ID, lease.Fence.AgentSessionID, callKey).Scan(&sameCallSteps); err != nil {
			t.Fatal(err)
		}
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND sequence=$2`,
			current.Task.ID, committedEvent.Sequence).Scan(&eventCount); err != nil {
			t.Fatal(err)
		}
		latest, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
		if err != nil {
			t.Fatal(err)
		}
		if sameCallSteps != 1 || eventCount != 1 || latest.Task.Status != task.StatusRunning ||
			latest.Task.Usage.ToolCalls != current.Task.Usage.ToolCalls+1 || latest.Task.EventSequence != committedEvent.Sequence {
			t.Fatalf("%s replay changed persisted state: tool steps=%d event=%d status=%s usage=%d sequence=%d",
				description, sameCallSteps, eventCount, latest.Task.Status, latest.Task.Usage.ToolCalls, latest.Task.EventSequence)
		}
	}

	changedDigest := append([]byte(nil), digest[:]...)
	changedDigest[0] ^= 0xff
	assertNoDuplicateCommit(makeProgress(stepID, sequence, changedDigest), "same-step changed-digest")
	changedStepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	assertNoDuplicateCommit(makeProgress(changedStepID, sequence+1, digest[:]), "same-call changed-step-identity")
}
