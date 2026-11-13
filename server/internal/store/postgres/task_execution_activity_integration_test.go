package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskToolStepPersistsFencedDetailAndCascades(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	createChild := func(spawnKey string) app.AgentChildSession {
		t.Helper()
		child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{
			Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: spawnKey,
			Arguments: []byte(`{"title":"Inspect workspace","objective":"read a file"}`),
			Title:     "Inspect workspace", Summary: "Inspect the authorized workspace.", AllowedTools: []string{"file_read"},
		})
		if err != nil || replayed {
			t.Fatalf("create child Agent %s: replayed=%t err=%v", spawnKey, replayed, err)
		}
		return child
	}
	childA, childB := createChild("activity-child-a"), createChild("activity-child-b")
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task revision: task=%+v err=%v", current.Task, err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("morde_workspace_file_read:{\"path\":\"src/main.go\"}"))
	makeProgress := func(status task.StepStatus, summary string, toolDigest []byte, agentSessionID string, id string, stepSequence int64) app.TaskProgress {
		phase := string(status)
		if status == task.StepRunning {
			phase = "prepare"
		}
		step := task.Step{ID: id, TaskID: current.Task.ID, Sequence: stepSequence, Kind: "tool", Status: status,
			Title: "Read src/main.go", Summary: summary, Phase: phase}
		document := app.TaskStepDetailDocument{Object: "src/main.go", Input: "Read the requested file.", Result: summary}
		plain, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		aad, err := json.Marshal(struct {
			OwnerID  string `json:"owner_id"`
			TaskID   string `json:"task_id"`
			StepID   string `json:"step_id"`
			Revision int64  `json:"task_revision"`
		}{current.Task.OwnerID, current.Task.ID, step.ID, *current.Task.CurrentRevision})
		if err != nil {
			t.Fatal(err)
		}
		ciphertext, err := sealer.SealWithAAD(plain, aad)
		if err != nil {
			t.Fatal(err)
		}
		plainDigest := sha256.Sum256(plain)
		usage := current.Task.Usage
		usage.ToolCalls++
		payload, err := json.Marshal(map[string]any{
			"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind, "status": step.Status,
			"title": step.Title, "summary": step.Summary,
		})
		if err != nil {
			t.Fatal(err)
		}
		return app.TaskProgress{
			TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
			Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: payload},
			ToolCall: &app.TaskToolCallProgress{
				AgentSessionID: agentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
				AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
				TaskRevision: *current.Task.CurrentRevision, CallKey: "call-activity-1", ToolName: "morde_workspace_file_read",
				RequestDigest: append([]byte(nil), toolDigest...), Phase: phase, DetailState: "available",
				DetailCiphertext: ciphertext, DetailSHA256: plainDigest[:], DetailSizeBytes: int32(len(plain)),
			},
		}
	}

	first, err := f.store.CommitTaskProgress(ctx, makeProgress(task.StepRunning, "Reading file.", digest[:], lease.Fence.AgentSessionID, stepID, sequence))
	if err != nil {
		t.Fatalf("persist running tool step: %v", err)
	}
	var preparedPhase string
	if err := f.store.pool.QueryRow(ctx, `SELECT phase FROM task_steps WHERE task_id=$1 AND id=$2`, current.Task.ID, stepID).Scan(&preparedPhase); err != nil || preparedPhase != "prepare" {
		t.Fatalf("persisted TaskStep prepare phase=%q err=%v", preparedPhase, err)
	}
	second, err := f.store.CommitTaskProgress(ctx, makeProgress(task.StepSucceeded, "Read completed.", digest[:], lease.Fence.AgentSessionID, stepID, sequence))
	if err != nil {
		t.Fatalf("settle existing tool step: %v", err)
	}
	if second.Sequence <= first.Sequence {
		t.Fatalf("step event sequence did not advance: first=%d second=%d", first.Sequence, second.Sequence)
	}
	details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted *task.Step
	for i := range details.Steps {
		if details.Steps[i].ID == stepID {
			persisted = &details.Steps[i]
			break
		}
	}
	if persisted == nil || persisted.Status != task.StepSucceeded || persisted.AgentSessionID != lease.Fence.AgentSessionID ||
		persisted.TaskRevision == nil || *persisted.TaskRevision != *current.Task.CurrentRevision || persisted.ToolCallKey != "call-activity-1" ||
		persisted.ToolName != "morde_workspace_file_read" || persisted.DetailState != "available" || persisted.Phase != string(task.StepSucceeded) {
		t.Fatalf("persisted task activity step = %#v", persisted)
	}
	stored, err := f.store.GetTaskStepDetail(ctx, current.Task.OwnerID, current.Task.ID, stepID)
	if err != nil || stored.EventSequence != second.Sequence || stored.TaskRevision != *current.Task.CurrentRevision || stored.Truncated {
		t.Fatalf("read persisted step detail = %+v, err=%v", stored, err)
	}
	plain, err := sealer.OpenWithAAD(stored.Ciphertext, aadForTaskStepDetail(current.Task.OwnerID, current.Task.ID, stepID, stored.TaskRevision))
	if err != nil {
		t.Fatalf("open persisted step detail: %v", err)
	}
	var document app.TaskStepDetailDocument
	if err := json.Unmarshal(plain, &document); err != nil || document.Object != "src/main.go" || document.Result != "Read completed." {
		t.Fatalf("persisted step detail document = %+v, err=%v", document, err)
	}

	conflictingDigest := sha256.Sum256([]byte("changed request"))
	conflictStepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	conflict := makeProgress(task.StepRunning, "Changed input.", conflictingDigest[:], lease.Fence.AgentSessionID, conflictStepID, sequence+1)
	conflict.Event.Payload, err = json.Marshal(map[string]any{
		"step_id": conflict.Steps[0].ID, "sequence": conflict.Steps[0].Sequence, "kind": "tool",
		"status": "running", "title": "Read src/main.go", "summary": "Changed input.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CommitTaskProgress(ctx, conflict); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same-session call ID with changed input error = %v", err)
	}
	var conflictedStepCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND id=$2`, current.Task.ID, conflictStepID).Scan(&conflictedStepCount); err != nil || conflictedStepCount != 0 {
		t.Fatalf("conflicting activity step survived transaction rollback: count=%d err=%v", conflictedStepCount, err)
	}
	for index, child := range []app.AgentChildSession{childA, childB} {
		childStepID, err := domain.NewStepID()
		if err != nil {
			t.Fatal(err)
		}
		progress := makeProgress(task.StepSucceeded, "Child read completed.", digest[:], child.SessionID, childStepID, sequence+1+int64(index))
		if _, err := f.store.CommitTaskProgress(ctx, progress); err != nil {
			t.Fatalf("persist same call ID from child Agent %s: %v", child.SessionID, err)
		}
	}
	details, err = f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	childCallCount := 0
	for _, persistedStep := range details.Steps {
		if persistedStep.ToolCallKey == "call-activity-1" && persistedStep.ToolName == "morde_workspace_file_read" {
			childCallCount++
		}
	}
	if childCallCount != 3 {
		t.Fatalf("same call ID across root and child sessions produced %d steps, want 3", childCallCount)
	}

	if _, err := f.store.pool.Exec(ctx, `DELETE FROM conversation_messages WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatalf("delete Task-linked messages: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, current.Task.ID); err != nil {
		t.Fatalf("delete Task with activity: %v", err)
	}
	var remaining int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_step_details WHERE task_id=$1`, current.Task.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("task step detail rows after Task deletion = %d, err=%v", remaining, err)
	}
}

func TestTaskExecutionActivityMigrationPreservesUnattributedHistory(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load Task revision: task=%+v err=%v", current.Task, err)
	}
	provider, closeProvider, err := newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 193); err != nil {
		t.Fatalf("roll back empty execution activity migration: %v", err)
	}
	legacyStepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'tool','succeeded','Legacy file read','Historical action without proven Agent attribution')`,
		legacyStepID, current.Task.ID, sequence); err != nil {
		t.Fatalf("insert pre-migration Task step: %v", err)
	}
	if _, err := provider.UpTo(ctx, 194); err != nil {
		t.Fatalf("upgrade existing Task history: %v", err)
	}
	assertLegacyStep := func() {
		t.Helper()
		var status, detailState string
		var unattributed bool
		if err := f.store.pool.QueryRow(ctx, `SELECT status,detail_state,
agent_session_id IS NULL AND task_revision IS NULL AND tool_call_key IS NULL AND tool_name IS NULL AND request_digest IS NULL
FROM task_steps WHERE task_id=$1 AND id=$2`, current.Task.ID, legacyStepID).Scan(&status, &detailState, &unattributed); err != nil ||
			status != string(task.StepSucceeded) || detailState != "not_recorded" || !unattributed {
			t.Fatalf("legacy Task step after migration: status=%s detail=%s unattributed=%t err=%v", status, detailState, unattributed, err)
		}
	}
	assertLegacyStep()
	if _, err := provider.DownTo(ctx, 193); err != nil {
		t.Fatalf("roll back metadata-only migration with legacy row: %v", err)
	}
	if _, err := provider.UpTo(ctx, 194); err != nil {
		t.Fatalf("reapply execution activity migration: %v", err)
	}
	assertLegacyStep()
	requestDigest := sha256.Sum256([]byte("morde_workspace_file_read:{}"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_steps SET agent_session_id=$3,task_revision=$4,tool_call_key='migration-call',
tool_name='morde_workspace_file_read',request_digest=$5,phase='succeeded' WHERE task_id=$1 AND id=$2`,
		current.Task.ID, legacyStepID, lease.Fence.AgentSessionID, *current.Task.CurrentRevision, requestDigest[:]); err != nil {
		t.Fatalf("add proven activity identity: %v", err)
	}
	if _, err := provider.DownTo(ctx, 193); err == nil {
		t.Fatal("migration rollback discarded newly recorded Task activity identity")
	}
}

func aadForTaskStepDetail(ownerID, taskID, stepID string, revision int64) []byte {
	value, _ := json.Marshal(struct {
		OwnerID  string `json:"owner_id"`
		TaskID   string `json:"task_id"`
		StepID   string `json:"step_id"`
		Revision int64  `json:"task_revision"`
	}{ownerID, taskID, stepID, revision})
	return value
}
