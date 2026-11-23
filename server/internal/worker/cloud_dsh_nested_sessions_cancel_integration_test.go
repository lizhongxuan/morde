package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// Cancellation is exercised through real TaskWorker/TaskStopWorker, PostgreSQL
// and the DSH subprocess. The separate sibling Task is kept in a model call
// until the nested Task and its entire Agent tree have stopped.
func TestCloudDSHNestedTaskCancellationLeavesIndependentTaskRunning(t *testing.T) {
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
		t.Fatalf("verify local test identity: %v", err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	budget := task.Budget{ActiveSeconds: 120, ToolCalls: 32, ModelTokens: 50000}
	parent, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Start a coordinator and worker, then wait for cancellation.",
		Budget: budget, IdempotencyKey: "nested-cancel-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ConversationID: parent.Conversation.ID, ModelID: "p1234567", Objective: "Finish an independent task.",
		Budget: budget, IdempotencyKey: "nested-cancel-sibling",
	})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x38}, 32))
	if err != nil {
		t.Fatal(err)
	}
	node, script := f01NodeRuntimePaths(t)
	const supervisorID = "nested-cancel-supervisor"
	instances, runtimes, supervisor := newF01ConversationRuntime(t, store, node, script, supervisorID, bytes.Repeat([]byte{0x39}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	model := newNestedCancelModel(pool, parent.Task.ID, sibling.Task.ID)
	defer model.releaseSibling()
	tools := &nestedE2ETools{}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: supervisorID, AccountRuntimes: runtimes, Instances: instances, Checkpoints: checkpoints,
		Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: tools, Children: children, Completion: &parentSiblingCancelCompletion{store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewTaskWorker(TaskWorkerOptions{Store: store, Executor: executor, WorkerID: "nested-cancel-worker",
		LeaseDuration: 3 * time.Second, PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2})
	if err != nil {
		t.Fatal(err)
	}
	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{Store: store,
		Executor: taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
			return task.OperationCancelled, "stopped by nested cancellation test", false, nil
		}), WorkerID: "nested-cancel-stop", LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond,
		OperationTimeout: time.Second, RetryAfter: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	workerDone, stopDone := make(chan error, 1), make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	go func() { stopDone <- stopWorker.Run(workerCtx) }()
	t.Cleanup(func() {
		stop()
		for name, done := range map[string]<-chan error{"TaskWorker": workerDone, "TaskStopWorker": stopDone} {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%s stopped: %v", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s did not stop", name)
			}
		}
	})

	for name, started := range map[string]<-chan struct{}{
		"nested worker": model.workerStarted, "coordinator": model.coordinatorStarted, "sibling": model.siblingStarted,
	} {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("%s did not start: %v; calls=%v; runtime=%s", name, ctx.Err(), model.snapshot(), supervisor.output())
		}
	}
	parentDetails, err := store.GetTask(ctx, parent.Task.ID, owner.ID)
	if err != nil || parentDetails.Task.Status != task.StatusRunning || parentDetails.Task.AgentSessionID == "" {
		t.Fatalf("parent Task was not active: %#v err=%v", parentDetails.Task, err)
	}
	rootID := parentDetails.Task.AgentSessionID
	coordinators, err := f07ReadPersistedChildren(ctx, pool, parent.Task.ID, owner.ID, rootID)
	if err != nil || len(coordinators) != 1 {
		t.Fatalf("coordinator was not persisted: %#v err=%v", coordinators, err)
	}
	workers, err := f07ReadPersistedChildren(ctx, pool, parent.Task.ID, owner.ID, coordinators[0].SessionID)
	if err != nil || len(workers) != 1 || workers[0].Step.Status != task.StepRunning {
		t.Fatalf("nested worker was not persisted as running: %#v err=%v", workers, err)
	}
	if _, err := tasks.RequestAction(ctx, principal, parent.Task.ID, task.ActionCancel, task.StatusRunning, "nested-cancel-parent-action"); err != nil {
		t.Fatalf("cancel parent Task: %v", err)
	}
	select {
	case <-model.workerCancelled:
	case <-ctx.Done():
		t.Fatalf("nested worker did not observe cancellation: %v", ctx.Err())
	}
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, parent.Task.ID, owner.ID)
		return err == nil && current.Task.Status == task.StatusCancelled, err
	}, nil, nil); err != nil {
		t.Fatalf("parent Task did not finish cancellation: %v; runtime=%s", err, supervisor.output())
	}
	for _, session := range []app.AgentChildSession{coordinators[0], workers[0]} {
		var cancelRequested bool
		var status string
		err := pool.QueryRow(ctx, `SELECT s.cancel_requested_at IS NOT NULL, st.status FROM task_agent_sessions s
JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, parent.Task.ID, owner.ID, session.SessionID).Scan(&cancelRequested, &status)
		if err != nil || !cancelRequested || status == string(task.StepRunning) {
			t.Fatalf("cancelled session %s is not durably stopped: cancel=%t status=%s err=%v", session.SessionID, cancelRequested, status, err)
		}
	}
	select {
	case <-model.siblingCancelled:
		t.Fatal("cancelling the nested Task cancelled the independent Task model")
	default:
	}
	siblingDetails, err := store.GetTask(ctx, sibling.Task.ID, owner.ID)
	if err != nil || siblingDetails.Task.Status != task.StatusRunning {
		t.Fatalf("independent Task did not remain running: status=%s err=%v", siblingDetails.Task.Status, err)
	}
	model.releaseSibling()
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, sibling.Task.ID, owner.ID)
		return err == nil && current.Task.Status == task.StatusCompleted && current.Task.Result == "Nested sibling completed independently.", err
	}, nil, nil); err != nil {
		t.Fatalf("independent Task did not finish: %v; calls=%v", err, model.snapshot())
	}
	if calls := tools.calls.Load(); calls != 0 {
		t.Fatalf("synthetic application tool executed unexpectedly: %d", calls)
	}
}

type nestedCancelModel struct {
	pool                                                       *pgxpool.Pool
	parentTaskID, siblingTaskID                                string
	mu                                                         sync.Mutex
	turns                                                      map[string]int
	calls                                                      []string
	workerStarted, coordinatorStarted, siblingStarted          chan struct{}
	workerCancelled, siblingCancelled, siblingRelease          chan struct{}
	workerOnce, coordinatorOnce, siblingOnce, workerCancelOnce sync.Once
	siblingCancelOnce, siblingReleaseOnce                      sync.Once
}

func newNestedCancelModel(pool *pgxpool.Pool, parentTaskID, siblingTaskID string) *nestedCancelModel {
	return &nestedCancelModel{pool: pool, parentTaskID: parentTaskID, siblingTaskID: siblingTaskID, turns: make(map[string]int),
		workerStarted: make(chan struct{}), coordinatorStarted: make(chan struct{}), siblingStarted: make(chan struct{}),
		workerCancelled: make(chan struct{}), siblingCancelled: make(chan struct{}), siblingRelease: make(chan struct{})}
}

func (model *nestedCancelModel) releaseSibling() {
	model.siblingReleaseOnce.Do(func() { close(model.siblingRelease) })
}

func (model *nestedCancelModel) snapshot() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]string(nil), model.calls...)
}

func (model *nestedCancelModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("nested cancellation model has no Task fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}
	if call.Task.TaskID == model.siblingTaskID {
		model.siblingOnce.Do(func() { close(model.siblingStarted) })
		select {
		case <-model.siblingRelease:
			return app.ModelResponse{Text: "Nested sibling completed independently.", StopReason: "stop", Usage: usage}, nil
		case <-ctx.Done():
			model.siblingCancelOnce.Do(func() { close(model.siblingCancelled) })
			return app.ModelResponse{}, ctx.Err()
		}
	}
	if call.Task.TaskID != model.parentTaskID {
		return app.ModelResponse{}, errors.New("model call crossed its Task boundary")
	}
	var parentID string
	if err := model.pool.QueryRow(ctx, `SELECT COALESCE(parent_session_id,'') FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).Scan(&parentID); err != nil {
		return app.ModelResponse{}, err
	}
	model.mu.Lock()
	model.turns[call.Task.AgentSessionID]++
	turn := model.turns[call.Task.AgentSessionID]
	model.calls = append(model.calls, fmt.Sprintf("session=%s parent=%s turn=%d", call.Task.AgentSessionID, parentID, turn))
	model.mu.Unlock()
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID && turn == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "nested-cancel-coordinator", Name: cloudDSHDelegateToolName,
			Arguments: `{"title":"cancel-coordinator","objective":"Start a worker and wait.","tool_names":["nested_allowed_read"],"run_in_background":true}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	if parentID == call.Task.AgentRootSessionID {
		if turn == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "nested-cancel-worker", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"cancel-worker","objective":"Wait until cancelled.","tool_names":["nested_allowed_read"],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		model.coordinatorOnce.Do(func() { close(model.coordinatorStarted) })
	} else if parentID != "" {
		model.workerOnce.Do(func() { close(model.workerStarted) })
	}
	<-ctx.Done()
	if parentID != "" && parentID != call.Task.AgentRootSessionID {
		model.workerCancelOnce.Do(func() { close(model.workerCancelled) })
	}
	return app.ModelResponse{}, ctx.Err()
}
