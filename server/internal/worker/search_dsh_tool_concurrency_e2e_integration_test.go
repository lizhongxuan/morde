package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

func TestDSHSearchTasksInSameConversationKeepResultsAndLedgerSeparated(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL")) == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; concurrent DSH search isolation integration was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner task execution: %v", err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatalf("create audit service: %v", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatalf("create credential service: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify local session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create Task service: %v", err)
	}
	budget := task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 1000}
	first, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Search synthetic topic alpha.", Budget: budget,
		IdempotencyKey: "morde-search-parallel-alpha-2026-11-08",
	})
	if err != nil {
		t.Fatalf("submit first search Task: %v", err)
	}
	second, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ConversationID: first.Conversation.ID, ModelID: "p1234567", Objective: "Search synthetic topic beta.", Budget: budget,
		IdempotencyKey: "morde-search-parallel-beta-2026-11-08",
	})
	if err != nil {
		t.Fatalf("submit second search Task: %v", err)
	}
	if first.Task.ID == second.Task.ID || first.Task.ConversationID != second.Task.ConversationID {
		t.Fatalf("expected separate Tasks in one conversation: first=%+v second=%+v", first.Task, second.Task)
	}

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatalf("open local Workspace blocks: %v", err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close local Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatalf("create Workspace sync service: %v", err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatalf("create local sealer: %v", err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatalf("create local Runtime control: %v", err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatalf("create Runtime tool executor: %v", err)
	}
	searchAdapter := newParallelSearchAdapter()
	defer searchAdapter.release()
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatalf("create Morde DSH tool broker: %v", err)
	}
	if err := broker.SetBuiltinSearch(searchAdapter); err != nil {
		t.Fatalf("install synthetic built-in search adapter: %v", err)
	}

	node, dshScript := f01NodeRuntimePaths(t)
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, dshScript,
		"parallel-search-dsh-worker", bytes.Repeat([]byte{0x72}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatalf("create checkpoint service: %v", err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatalf("create child session service: %v", err)
	}
	models := newParallelSearchModels(first.Task.ID, second.Task.ID)
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "parallel-search-dsh-worker", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: models, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: broker, Children: children, Completion: broker,
	})
	if err != nil {
		t.Fatalf("create Cloud DSH executor: %v", err)
	}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "parallel-search-dsh-worker", LeaseDuration: 30 * time.Second,
		PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create TaskWorker: %v", err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		select {
		case err := <-workerDone:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("TaskWorker stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	})

	entered := map[string]bool{}
	for range 2 {
		select {
		case query := <-searchAdapter.entered:
			if entered[query] {
				t.Fatalf("search query %q was dispatched more than once", query)
			}
			entered[query] = true
		case <-ctx.Done():
			t.Fatalf("both same-conversation Tasks did not reach search concurrently: %v", ctx.Err())
		}
	}
	if !entered["synthetic topic alpha"] || !entered["synthetic topic beta"] {
		t.Fatalf("parallel search queries = %#v; want each Task's distinct query", entered)
	}
	for _, taskID := range []string{first.Task.ID, second.Task.ID} {
		details, err := store.GetTask(ctx, taskID, owner.ID)
		if err != nil || details.Task.Status != task.StatusRunning {
			t.Fatalf("Task %q while both searches are in flight = status %q err=%v", taskID, details.Task.Status, err)
		}
	}
	searchAdapter.release()

	for _, want := range []struct {
		id, answer, callID, result string
		version                    int64
	}{
		{first.Task.ID, "Verified alpha result.", "morde-parallel-search-alpha", "result for synthetic topic alpha", first.Task.Authorization.Version},
		{second.Task.ID, "Verified beta result.", "morde-parallel-search-beta", "result for synthetic topic beta", second.Task.Authorization.Version},
	} {
		details, err := waitForSearchDSHTask(ctx, store, want.id, owner.ID)
		if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != want.answer {
			t.Fatalf("Task %q completion = status %q reason %q result %q pending=%+v steps=%+v modelResult=%q adapterCalls=%#v err=%v; want %q",
				want.id, details.Task.Status, details.Task.WaitingReason, details.Task.Result, details.PendingRequest, details.Steps,
				models.resultFor(want.id), searchAdapter.callCounts(), err, want.answer)
		}
		if result := models.resultFor(want.id); result != want.result {
			t.Fatalf("Task %q model received %q; want its own search result %q", want.id, result, want.result)
		}
		operation, err := store.GetMCPConnectorOperation(ctx, owner.ID, want.id, app.BuiltinSearchMCPConnectorID,
			app.AgentOperationKey(want.callID))
		if err != nil || operation.Status != task.OperationSucceeded || operation.ToolName != "morde_web_search" ||
			operation.GrantVersion != want.version || !strings.Contains(operation.Result, want.result) {
			t.Fatalf("Task %q operation = %#v err=%v; want its own successful result", want.id, operation, err)
		}
	}
	if calls := searchAdapter.callCounts(); calls["synthetic topic alpha"] != 1 || calls["synthetic topic beta"] != 1 {
		t.Fatalf("remote search calls = %#v; want one isolated call per Task", calls)
	}
}

type parallelSearchAdapter struct {
	entered     chan string
	releaseOnce sync.Once
	released    chan struct{}
	mu          sync.Mutex
	calls       map[string]int
}

func newParallelSearchAdapter() *parallelSearchAdapter {
	return &parallelSearchAdapter{entered: make(chan string, 2), released: make(chan struct{}), calls: make(map[string]int)}
}

func (*parallelSearchAdapter) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return nil, errors.New("built-in search definitions are installed locally")
}

func (adapter *parallelSearchAdapter) CallTool(ctx context.Context, endpoint string, headers http.Header,
	name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	if endpoint != "" || len(headers) != 0 || name != "searxng_web_search" {
		return app.MCPConnectorToolResult{}, errors.New("unexpected endpoint, credentials, or remote tool")
	}
	var request struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil || request.Query == "" {
		return app.MCPConnectorToolResult{}, errors.New("search query is invalid")
	}
	adapter.mu.Lock()
	adapter.calls[request.Query]++
	adapter.mu.Unlock()
	select {
	case adapter.entered <- request.Query:
	case <-ctx.Done():
		return app.MCPConnectorToolResult{}, ctx.Err()
	}
	select {
	case <-adapter.released:
	case <-ctx.Done():
		return app.MCPConnectorToolResult{}, ctx.Err()
	}
	return app.MCPConnectorToolResult{Text: []string{"result for " + request.Query}}, nil
}

func (adapter *parallelSearchAdapter) release() {
	adapter.releaseOnce.Do(func() { close(adapter.released) })
}

func (adapter *parallelSearchAdapter) callCounts() map[string]int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	result := make(map[string]int, len(adapter.calls))
	for query, count := range adapter.calls {
		result[query] = count
	}
	return result
}

type parallelSearchModels struct {
	models map[string]*parallelSearchModel
}

func newParallelSearchModels(firstTaskID, secondTaskID string) *parallelSearchModels {
	return &parallelSearchModels{models: map[string]*parallelSearchModel{
		firstTaskID:  {taskID: firstTaskID, query: "synthetic topic alpha", callID: "morde-parallel-search-alpha", expectedResult: "result for synthetic topic alpha", answer: "Verified alpha result."},
		secondTaskID: {taskID: secondTaskID, query: "synthetic topic beta", callID: "morde-parallel-search-beta", expectedResult: "result for synthetic topic beta", answer: "Verified beta result."},
	}}
}

func (models *parallelSearchModels) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("synthetic model call is missing its Task fence")
	}
	model, ok := models.models[call.Task.TaskID]
	if !ok {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task %q", call.Task.TaskID)
	}
	return model.complete(ctx, call)
}

func (models *parallelSearchModels) resultFor(taskID string) string {
	if model := models.models[taskID]; model != nil {
		model.mu.Lock()
		defer model.mu.Unlock()
		return model.receivedResult
	}
	return ""
}

type parallelSearchModel struct {
	mu             sync.Mutex
	taskID         string
	query          string
	callID         string
	expectedResult string
	answer         string
	turn           int
	receivedResult string
}

func (model *parallelSearchModel) complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	if call.Task.TaskID != model.taskID {
		return app.ModelResponse{}, fmt.Errorf("Task fence %q reached model for %q", call.Task.TaskID, model.taskID)
	}
	model.turn++
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	if model.turn == 1 {
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(call.RawRequest, &request); err != nil {
			return app.ModelResponse{}, fmt.Errorf("decode DSH tool offer: %w", err)
		}
		for _, tool := range request.Tools {
			if tool.Name == "morde_web_search" {
				arguments, err := json.Marshal(map[string]string{"query": model.query, "time_range": "week"})
				if err != nil {
					return app.ModelResponse{}, fmt.Errorf("encode parallel search arguments: %w", err)
				}
				return app.ModelResponse{ToolCalls: []app.ModelToolCall{{
					CallID: model.callID, Name: tool.Name, Arguments: string(arguments),
				}}, StopReason: "tool_calls", Usage: usage}, nil
			}
		}
		return app.ModelResponse{}, errors.New("Morde built-in search was not offered to the DSH model")
	}
	if model.turn != 2 {
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic model turn %d", model.turn)
	}
	var request struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode search result turn: %w", err)
	}
	for _, message := range request.Messages {
		if message.Role != "tool" || message.ToolCallID != model.callID {
			continue
		}
		for _, content := range message.Content {
			var output struct {
				IsError bool     `json:"is_error"`
				Text    []string `json:"text"`
			}
			if err := json.Unmarshal([]byte(content.Text), &output); err != nil || output.IsError {
				continue
			}
			for _, result := range output.Text {
				if strings.TrimSpace(result) == model.expectedResult {
					model.receivedResult = result
					return app.ModelResponse{Text: model.answer, StopReason: "stop", Usage: usage}, nil
				}
			}
		}
	}
	return app.ModelResponse{}, fmt.Errorf("Task %q did not receive its own search result", model.taskID)
}
