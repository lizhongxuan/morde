package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestCloudTaskCancelledBackgroundChildRecoversAfterNodeRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify Task session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Create a child job, then continue after it is cancelled.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 8, ModelTokens: 1000}, IdempotencyKey: "f01-child-cancel-restart-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "f01-child-cancel-worker-old", 5*time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim initial Task: id=%s found=%t err=%v", claimed.ID, found, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stopKey := bytes.Repeat([]byte{0x64}, 32)
	instances, err := newF01AgentInstances(store, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	node, script := f01NodeRuntimePaths(t)
	model := &cancelRestartModel{childCheckpointed: make(chan struct{}), parentRecovered: make(chan struct{})}
	tools := &cancelRestartTools{definition: cloudDSHWriteTool()}
	firstSupervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	firstExecutor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-child-cancel-supervisor", Supervisor: firstSupervisor,
		Instances: instances, Checkpoints: checkpoints, Children: children, Models: model, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- firstExecutor.ExecuteTask(firstCtx, claimed) }()
	defer cancelFirst()

	select {
	case <-model.childCheckpointed:
	case <-ctx.Done():
		t.Fatalf("background child did not reach its persisted checkpoint: %v; root_calls=%d child_calls=%d writes=%d",
			ctx.Err(), model.callCount(fenceGeneration(firstSupervisor)), model.childCallCount(fenceGeneration(firstSupervisor)), tools.writes.Load())
	case err := <-firstDone:
		t.Fatalf("first Agent stopped before background child checkpoint: %v; writes=%d child_calls=%d",
			err, tools.writes.Load(), model.childCallCount(1))
	}
	firstSupervisor.child.mu.Lock()
	fence := firstSupervisor.child.launch.Fence
	firstSupervisor.child.mu.Unlock()
	jobs, err := children.List(ctx, fence, fence.AgentSessionID)
	if err != nil || len(jobs) != 1 || !jobs[0].Background || jobs[0].Step.Status != task.StepRunning ||
		jobs[0].CheckpointThrough < 0 || len(jobs[0].CheckpointDigest) != 32 {
		t.Fatalf("running child checkpoint before cancel = %#v err=%v", jobs, err)
	}
	jobID := jobs[0].SessionID
	requested, replayed, err := children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{
		Fence: fence, ActorSessionID: fence.AgentSessionID, SessionID: jobID,
	})
	if err != nil || replayed || !requested.CancelRequested || requested.Step.Status != task.StepRunning {
		t.Fatalf("persist child cancellation: child=%#v replayed=%t err=%v", requested, replayed, err)
	}
	oldChildCheckpoint := append([]byte(nil), jobs[0].CheckpointDigest...)
	oldChildThrough := jobs[0].CheckpointThrough
	rootBeforeRestart, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil {
		t.Fatalf("read root recovery checkpoint: %v", err)
	}
	oldRootCheckpointThrough := rootBeforeRestart.Task.CheckpointThroughSequence
	oldRootCheckpointDigest := append([]byte(nil), rootBeforeRestart.Task.CheckpointDigest...)
	if oldRootCheckpointThrough < 0 || len(oldRootCheckpointDigest) != 32 {
		t.Fatalf("parent checkpoint was not durable before interruption: through=%d digest=%d", oldRootCheckpointThrough, len(oldRootCheckpointDigest))
	}

	firstSupervisor.child.mu.Lock()
	oldProcess := firstSupervisor.child.cmd
	firstSupervisor.child.mu.Unlock()
	if oldProcess == nil || oldProcess.Process == nil {
		t.Fatal("real Node process was not available for restart simulation")
	}
	if err := oldProcess.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop old Node process: %v", err)
	}
	cancelFirst()
	select {
	case runErr := <-firstDone:
		if runErr == nil || errors.Is(runErr, ErrCloudDSHCompletionPending) {
			t.Fatalf("interrupted Agent result = %v, want a recoverable interruption", runErr)
		}
	case <-ctx.Done():
		t.Fatalf("old Agent did not stop and persist its receipt: %v", ctx.Err())
	}
	var oldState string
	if err := pool.QueryRow(ctx, `SELECT state FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`,
		submitted.Task.ID, fence.AgentGeneration).Scan(&oldState); err != nil || oldState != "stopped" {
		t.Fatalf("old Agent stop proof state=%q err=%v", oldState, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "f01-child-cancel-worker-new", 5*time.Minute)
	if err != nil || !found || recovered.ID != submitted.Task.ID || recovered.AgentSessionID != fence.AgentSessionID ||
		recovered.CheckpointThroughSequence != oldRootCheckpointThrough || !bytes.Equal(recovered.CheckpointDigest, oldRootCheckpointDigest) {
		t.Fatalf("claim recoverable parent session: task=%#v found=%t err=%v", recovered, found, err)
	}
	secondSupervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	secondExecutor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-child-cancel-supervisor", Supervisor: secondSupervisor,
		Instances: instances, Checkpoints: checkpoints, Children: children, Models: model, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondExecutor.ExecuteTask(ctx, recovered); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("parent Agent recovery after child cancel = %v, want a completed candidate; root_calls=%d child_calls=%d",
			err, model.callCount(fence.AgentGeneration+1), model.childCallCount(fence.AgentGeneration+1))
	}
	select {
	case <-model.parentRecovered:
	default:
		t.Fatal("parent model was not called in the new Agent generation")
	}

	var childStatus, childSummary string
	var cancelRequested bool
	var childThrough int64
	var childDigest []byte
	if err := pool.QueryRow(ctx, `SELECT st.status,st.summary,s.cancel_requested_at IS NOT NULL,s.checkpoint_through_seq,s.checkpoint_digest
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, submitted.Task.ID, owner.ID, jobID).Scan(
		&childStatus, &childSummary, &cancelRequested, &childThrough, &childDigest); err != nil {
		t.Fatalf("read reconciled child job: %v", err)
	}
	if childStatus != string(task.StepUnknown) || !cancelRequested ||
		childSummary != "outcome unknown after cancellation and confirmed Agent restart" ||
		childThrough != oldChildThrough || !bytes.Equal(childDigest, oldChildCheckpoint) {
		t.Fatalf("cancelled child convergence status=%s summary=%q cancel=%t checkpoint=%d/%x; want unknown with its pinned checkpoint",
			childStatus, childSummary, cancelRequested, childThrough, childDigest)
	}
	parent, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || parent.Task.AgentGeneration != fence.AgentGeneration+1 || parent.Task.AgentSessionID != fence.AgentSessionID ||
		parent.Task.CheckpointThroughSequence <= oldRootCheckpointThrough || len(parent.Task.CheckpointDigest) != 32 {
		t.Fatalf("parent session did not recover under the next Agent generation: task=%#v err=%v", parent.Task, err)
	}
	if tools.writes.Load() != 1 || model.childCallCount(fence.AgentGeneration+1) != 0 {
		t.Fatalf("cancelled child side effect was replayed: writes=%d new_generation_child_model_calls=%d",
			tools.writes.Load(), model.childCallCount(fence.AgentGeneration+1))
	}
}

func newF01AgentInstances(store app.AgentInstanceStore, key []byte) (*app.AgentInstanceService, error) {
	auth, err := app.NewAgentStopAuthenticator("f01-child-cancel-supervisor", key)
	if err != nil {
		return nil, err
	}
	return app.NewAgentInstanceService(store, auth)
}

func f01NodeRuntimePaths(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real dsh subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	return node, script
}

type cancelRestartModel struct {
	mu                sync.Mutex
	rootCalls         map[int64]int
	childCalls        map[int64]int
	childCheckpointed chan struct{}
	parentRecovered   chan struct{}
	childOnce         sync.Once
	parentOnce        sync.Once
}

func (m *cancelRestartModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model call omitted its Agent fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID {
		m.mu.Lock()
		if m.rootCalls == nil {
			m.rootCalls = make(map[int64]int)
		}
		m.rootCalls[call.Task.AgentGeneration]++
		callNumber := m.rootCalls[call.Task.AgentGeneration]
		m.mu.Unlock()
		if call.Task.AgentGeneration > 1 {
			m.parentOnce.Do(func() { close(m.parentRecovered) })
			return app.ModelResponse{Text: "Parent resumed after the cancelled child was marked unknown.",
				StopReason: "stop", Usage: usage}, nil
		}
		if callNumber == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "spawn_cancelled_child", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Cancelable file job","objective":"Write the file, then wait for follow-up instructions.","tool_names":["file_write"],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 2 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "list_cancelled_jobs", Name: cloudDSHJobListToolName,
				Arguments: `{}`}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 3 {
			output, ok := f01ToolResult(call.RawRequest, "list_cancelled_jobs")
			var jobs []cloudDSHJobView
			if !ok || json.Unmarshal([]byte(output), &jobs) != nil || len(jobs) != 1 || jobs[0].JobID == "" {
				return app.ModelResponse{}, errors.New("cancel test job was not listed")
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "get_cancelled_job_001", Name: cloudDSHJobGetToolName,
				Arguments: fmt.Sprintf(`{"job_id":%q}`, jobs[0].JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		previousGet := fmt.Sprintf("get_cancelled_job_%03d", callNumber-3)
		output, ok := f01ToolResult(call.RawRequest, previousGet)
		var job cloudDSHJobView
		if !ok || json.Unmarshal([]byte(output), &job) != nil || job.JobID == "" ||
			(job.Status != "running" && job.Status != "cancelling") {
			return app.ModelResponse{}, errors.New("cancel test job status changed before the interruption")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: fmt.Sprintf("get_cancelled_job_%03d", callNumber-2),
			Name: cloudDSHJobGetToolName, Arguments: fmt.Sprintf(`{"job_id":%q}`, job.JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
	}

	m.mu.Lock()
	if m.childCalls == nil {
		m.childCalls = make(map[int64]int)
	}
	m.childCalls[call.Task.AgentGeneration]++
	callNumber := m.childCalls[call.Task.AgentGeneration]
	m.mu.Unlock()
	if call.Task.AgentGeneration > 1 {
		return app.ModelResponse{}, errors.New("cancelled child runtime was incorrectly resumed")
	}
	if callNumber == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "cancel_child_write", Name: "file_write",
			Arguments: `{"workspace_id":"w1234567","path":"cancel-check.txt","content":"write once"}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	m.childOnce.Do(func() { close(m.childCheckpointed) })
	<-ctx.Done()
	return app.ModelResponse{}, ctx.Err()
}

func (m *cancelRestartModel) childCallCount(generation int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.childCalls[generation]
}

func (m *cancelRestartModel) callCount(generation int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootCalls[generation]
}

func fenceGeneration(supervisor *f01CapturingSupervisor) int64 {
	supervisor.child.mu.Lock()
	defer supervisor.child.mu.Unlock()
	return supervisor.child.launch.Fence.AgentGeneration
}

type cancelRestartTools struct {
	definition app.AgentToolDefinition
	writes     atomic.Int32
}

func (tools *cancelRestartTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{tools.definition}, nil
}

func (tools *cancelRestartTools) Execute(_ context.Context, _ task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	if call.Name != "file_write" {
		return app.AgentToolResult{}, fmt.Errorf("unexpected test tool %q", call.Name)
	}
	tools.writes.Add(1)
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: `{"path":"cancel-check.txt","revision":1}`}, nil
}
