package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	nestedCoordinatorCall = "nested-coordinator"
	nestedAlphaCall       = "nested-alpha"
	nestedBetaCall        = "nested-beta"
	nestedAlphaOutput     = "nested-alpha-output"
	nestedBetaOutput      = "nested-beta-output"
	nestedAllowedTool     = "nested_allowed_read"
	nestedForbiddenTool   = "nested_not_delegated"
	nestedRootResult      = "Root verified nested-alpha-result and nested-beta-result."
)

// This runs the real DSH subprocess against a real PostgreSQL session tree.
// Its model is local and deterministic; the two worker calls are held at a
// barrier so observing both calls before release proves they actually overlap.
func TestCloudDSHNestedCoordinatorRunsIndependentWorkersConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	otherOwner := createF01User(t, ctx, store)
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
		t.Fatalf("verify synthetic session: %v", err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Coordinate two independent local research workers and report their results.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 32, ModelTokens: 50000}, IdempotencyKey: "nested-coordinator-e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "nested-e2e-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim nested Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x36}, 32))
	if err != nil {
		t.Fatal(err)
	}
	node, script := f01NodeRuntimePaths(t)
	const supervisorID = "nested-e2e-supervisor"
	instances, runtimes, _ := newF01ConversationRuntime(t, store, node, script, supervisorID, bytes.Repeat([]byte{0x37}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	model := newNestedE2EModel(pool)
	defer model.releaseWorkers()
	tools := &nestedE2ETools{}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: supervisorID, AccountRuntimes: runtimes, Instances: instances, Checkpoints: checkpoints,
		Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: tools, Children: children, Completion: &parentSiblingCancelCompletion{store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- executor.ExecuteTask(ctx, claimed) }()

	entered := make(map[string]nestedWorkerStart, 2)
	for len(entered) < 2 {
		select {
		case worker := <-model.started:
			if _, exists := entered[worker.sessionID]; exists {
				t.Fatalf("worker model session %q entered twice", worker.sessionID)
			}
			entered[worker.sessionID] = worker
		case runErr := <-done:
			t.Fatalf("Task ended before both workers ran: %v; calls=%v", runErr, model.snapshot())
		case <-time.After(30 * time.Second):
			t.Fatalf("workers did not overlap: entered=%v calls=%v", entered, model.snapshot())
		case <-ctx.Done():
			t.Fatalf("waiting for concurrent workers: %v; calls=%v", ctx.Err(), model.snapshot())
		}
	}
	var coordinatorID string
	labels := make(map[string]bool, 2)
	for _, worker := range entered {
		if coordinatorID != "" && coordinatorID != worker.parentSessionID {
			t.Fatalf("workers belong to different coordinators: %#v", entered)
		}
		coordinatorID = worker.parentSessionID
		labels[worker.title] = true
	}
	if coordinatorID == "" || len(entered) != 2 || !labels[nestedAlphaCall] || !labels[nestedBetaCall] {
		t.Fatalf("invalid nested worker tree: %#v", entered)
	}

	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.AgentSessionID == "" {
		t.Fatalf("load active root Task: %v", err)
	}
	rootID := details.Task.AgentSessionID
	coordinators, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, rootID)
	if err != nil || len(coordinators) != 1 || coordinators[0].SessionID != coordinatorID || coordinators[0].Step.Status != task.StepRunning {
		t.Fatalf("persisted coordinators = %#v, err=%v", coordinators, err)
	}
	workers, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinatorID)
	if err != nil || len(workers) != 2 {
		t.Fatalf("persisted nested workers = %#v, err=%v", workers, err)
	}
	for _, worker := range workers {
		if _, ok := entered[worker.SessionID]; !ok || worker.Step.Status != task.StepRunning ||
			!worker.Background || !sameWorkerToolNames(worker.AllowedTools, []string{nestedAllowedTool}) {
			t.Fatalf("worker is not a running, permission-limited nested session: %#v", worker)
		}
	}

	// A caller cannot create a third level even while the depth-2 parent runs.
	fence := f07AgentFence(details.Task, supervisorID)
	_, _, err = children.Create(ctx, app.AgentChildSessionCommand{
		Fence: fence, ParentSessionID: workers[0].SessionID, SpawnKey: "nested-third-level",
		Arguments: []byte(`{}`), Title: "Forbidden third level", AllowedTools: []string{},
		RootCheckpointThroughSequence: workers[0].RootCheckpointThroughSequence,
	})
	if domain.CodeOf(err) != domain.CodeInvalidState || !strings.Contains(err.Error(), "delegation depth is exceeded") {
		t.Fatalf("depth-2 worker did not receive the depth-limit error: %v", err)
	}
	var thirdLevelCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND spawn_key='nested-third-level'`, claimed.ID).Scan(&thirdLevelCount); err != nil || thirdLevelCount != 0 {
		t.Fatalf("rejected third-level session persisted: count=%d err=%v", thirdLevelCount, err)
	}
	wrongOwnerFence := fence
	wrongOwnerFence.OwnerID = otherOwner.ID
	if _, err := children.Get(ctx, wrongOwnerFence, rootID, workers[0].SessionID); err == nil {
		t.Fatal("another user could read the nested session")
	}

	model.releaseWorkers()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("nested Task did not finish: %v; calls=%v", runErr, model.snapshot())
		}
	case <-ctx.Done():
		t.Fatalf("nested Task did not finish: %v; calls=%v", ctx.Err(), model.snapshot())
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != nestedRootResult {
		t.Fatalf("nested Task result: status=%s result=%q err=%v", details.Task.Status, details.Task.Result, err)
	}
	coordinators, err = f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, rootID)
	if err != nil || len(coordinators) != 1 || coordinators[0].Step.Status != task.StepSucceeded ||
		!strings.Contains(coordinators[0].Step.Summary, "nested-alpha-result") || !strings.Contains(coordinators[0].Step.Summary, "nested-beta-result") {
		t.Fatalf("coordinator did not settle actual worker results: %#v err=%v", coordinators, err)
	}
	workers, err = f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinatorID)
	if err != nil || len(workers) != 2 || workers[0].Step.Status != task.StepSucceeded || workers[1].Step.Status != task.StepSucceeded {
		t.Fatalf("nested worker settlements = %#v, err=%v", workers, err)
	}
	if calls := tools.calls.Load(); calls != 0 {
		t.Fatalf("synthetic application tool unexpectedly executed %d times", calls)
	}
}

type nestedWorkerStart struct {
	sessionID, parentSessionID, title string
}

type nestedE2ETools struct{ calls atomic.Int32 }

func (*nestedE2ETools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	definition := func(name string) app.AgentToolDefinition {
		return app.AgentToolDefinition{ModelTool: app.ModelTool{Name: name, Description: "Synthetic local read-only tool",
			Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)}}
	}
	return []app.AgentToolDefinition{definition(nestedAllowedTool), definition(nestedForbiddenTool)}, nil
}

func (tools *nestedE2ETools) Execute(context.Context, task.Task, app.ModelToolCall) (app.AgentToolResult, error) {
	tools.calls.Add(1)
	return app.AgentToolResult{}, errors.New("the synthetic application tool must not execute")
}

type nestedE2EModel struct {
	pool        *pgxpool.Pool
	mu          sync.Mutex
	turns       map[string]int
	calls       []string
	started     chan nestedWorkerStart
	release     chan struct{}
	releaseOnce sync.Once
}

func newNestedE2EModel(pool *pgxpool.Pool) *nestedE2EModel {
	return &nestedE2EModel{pool: pool, turns: make(map[string]int), started: make(chan nestedWorkerStart, 2), release: make(chan struct{})}
}

func (model *nestedE2EModel) releaseWorkers() { model.releaseOnce.Do(func() { close(model.release) }) }

func (model *nestedE2EModel) snapshot() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]string(nil), model.calls...)
}

func (model *nestedE2EModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("nested model call has no Task fence")
	}
	var parentID, title string
	if err := model.pool.QueryRow(ctx, `SELECT COALESCE(s.parent_session_id,''),COALESCE(st.title,'') FROM task_agent_sessions s
LEFT JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).
		Scan(&parentID, &title); err != nil {
		return app.ModelResponse{}, fmt.Errorf("read nested model session: %w", err)
	}
	model.mu.Lock()
	model.turns[call.Task.AgentSessionID]++
	turn := model.turns[call.Task.AgentSessionID]
	model.calls = append(model.calls, fmt.Sprintf("session=%s parent=%s title=%s turn=%d", call.Task.AgentSessionID, parentID, title, turn))
	model.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	response := func(calls ...app.ModelToolCall) app.ModelResponse {
		return app.ModelResponse{ToolCalls: calls, StopReason: "tool_calls", Usage: usage}
	}
	tools, err := nestedModelToolNames(call.RawRequest)
	if err != nil {
		return app.ModelResponse{}, err
	}
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID {
		if parentID != "" {
			return app.ModelResponse{}, errors.New("root session has a parent")
		}
		if turn == 1 {
			if !tools[cloudDSHDelegateToolName] || !tools[nestedAllowedTool] || !tools[nestedForbiddenTool] {
				return app.ModelResponse{}, fmt.Errorf("root lacks authorized tools: %v", tools)
			}
			return response(app.ModelToolCall{CallID: nestedCoordinatorCall, Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"nested-coordinator","objective":"Run alpha and beta workers and report both actual results.","tool_names":["nested_allowed_read"]}`}), nil
		}
		if turn == 2 {
			output, ok := f01ToolResult(call.RawRequest, nestedCoordinatorCall)
			if !ok || !strings.Contains(output, "nested-alpha-result") || !strings.Contains(output, "nested-beta-result") {
				return app.ModelResponse{}, fmt.Errorf("root did not receive coordinator results: %q", output)
			}
			return app.ModelResponse{Text: nestedRootResult, StopReason: "stop", Usage: usage}, nil
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected root turn %d", turn)
	}
	if parentID == call.Task.AgentRootSessionID {
		if !tools[cloudDSHDelegateToolName] || !tools[cloudDSHJobOutputToolName] || !tools[nestedAllowedTool] || tools[nestedForbiddenTool] {
			return app.ModelResponse{}, fmt.Errorf("coordinator tools violated parent subset: %v", tools)
		}
		switch turn {
		case 1:
			return response(
				app.ModelToolCall{CallID: nestedAlphaCall, Name: cloudDSHDelegateToolName,
					Arguments: `{"title":"nested-alpha","objective":"Return nested-alpha-result.","tool_names":["nested_allowed_read"],"run_in_background":true}`},
				app.ModelToolCall{CallID: nestedBetaCall, Name: cloudDSHDelegateToolName,
					Arguments: `{"title":"nested-beta","objective":"Return nested-beta-result.","tool_names":["nested_allowed_read"],"run_in_background":true}`},
			), nil
		case 2:
			alpha, err := nestedBackgroundHandle(call.RawRequest, nestedAlphaCall)
			if err != nil {
				return app.ModelResponse{}, err
			}
			beta, err := nestedBackgroundHandle(call.RawRequest, nestedBetaCall)
			if err != nil || alpha == beta {
				return app.ModelResponse{}, errors.New("nested worker job handles are invalid")
			}
			if err := model.waitForSettledWorkers(ctx, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID); err != nil {
				return app.ModelResponse{}, err
			}
			return response(
				app.ModelToolCall{CallID: nestedAlphaOutput, Name: cloudDSHJobOutputToolName, Arguments: fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, alpha)},
				app.ModelToolCall{CallID: nestedBetaOutput, Name: cloudDSHJobOutputToolName, Arguments: fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, beta)},
			), nil
		case 3:
			alpha, okAlpha := f01ToolResult(call.RawRequest, nestedAlphaOutput)
			beta, okBeta := f01ToolResult(call.RawRequest, nestedBetaOutput)
			if !okAlpha || !okBeta || !strings.Contains(alpha, "nested-alpha-result") || !strings.Contains(beta, "nested-beta-result") {
				return app.ModelResponse{}, fmt.Errorf("coordinator did not receive both worker outputs: %q %q", alpha, beta)
			}
			return app.ModelResponse{Text: "Coordinator verified nested-alpha-result and nested-beta-result.", StopReason: "stop", Usage: usage}, nil
		case 4:
			handoff, err := nestedModelHandoff(call.RawRequest)
			if err != nil {
				return app.ModelResponse{}, fmt.Errorf("read coordinator review handoff: %w", err)
			}
			if !strings.Contains(handoff, "nested-alpha-result") || !strings.Contains(handoff, "nested-beta-result") || strings.Count(handoff, `"status":"succeeded"`) != 2 {
				return app.ModelResponse{}, fmt.Errorf("coordinator review did not include both actual successes: %q", handoff)
			}
			return app.ModelResponse{Text: "Coordinator verified nested-alpha-result and nested-beta-result.", StopReason: "stop", Usage: usage}, nil
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected coordinator turn %d", turn)
	}
	if turn != 1 || (title != nestedAlphaCall && title != nestedBetaCall) || !tools[nestedAllowedTool] ||
		tools[nestedForbiddenTool] || tools[cloudDSHDelegateToolName] || tools[cloudDSHJobOutputToolName] {
		return app.ModelResponse{}, fmt.Errorf("depth-2 worker tool scope/turn is invalid: title=%s turn=%d tools=%v", title, turn, tools)
	}
	select {
	case model.started <- nestedWorkerStart{sessionID: call.Task.AgentSessionID, parentSessionID: parentID, title: title}:
	case <-ctx.Done():
		return app.ModelResponse{}, ctx.Err()
	}
	select {
	case <-model.release:
		return app.ModelResponse{Text: title + "-result", StopReason: "stop", Usage: usage}, nil
	case <-ctx.Done():
		return app.ModelResponse{}, ctx.Err()
	}
}

func nestedModelToolNames(raw json.RawMessage) (map[string]bool, error) {
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode nested model tools: %w", err)
	}
	result := make(map[string]bool, len(request.Tools))
	for _, tool := range request.Tools {
		result[tool.Name] = true
	}
	return result, nil
}

func nestedBackgroundHandle(raw json.RawMessage, callID string) (string, error) {
	output, ok := f01ToolResult(raw, callID)
	var handle struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if !ok || json.Unmarshal([]byte(output), &handle) != nil || handle.JobID == "" || handle.Status == "" {
		return "", fmt.Errorf("background handle for %s is invalid: %q", callID, output)
	}
	return handle.JobID, nil
}

func (model *nestedE2EModel) waitForSettledWorkers(ctx context.Context, taskID, ownerID, coordinatorID string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		err := model.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions s
JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.parent_session_id=$3 AND st.status='succeeded'`, taskID, ownerID, coordinatorID).Scan(&count)
		if err != nil {
			return fmt.Errorf("read nested worker settlement: %w", err)
		}
		if count == 2 {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func nestedModelHandoff(raw json.RawMessage) (string, error) {
	var request struct {
		Messages []struct {
			Source struct {
				Plugin string `json:"plugin"`
			} `json:"source"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", err
	}
	var handoff string
	for _, message := range request.Messages {
		if message.Source.Plugin == "morde-subagent-handoff" {
			for _, block := range message.Content {
				handoff += block.Text
			}
		}
	}
	return handoff, nil
}
