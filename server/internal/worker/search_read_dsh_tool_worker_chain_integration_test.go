package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const (
	searchReadSearchCallID = "morde-search-read-worker-search-001"
	searchReadReadCallID   = "morde-search-read-worker-read-001"
	searchReadQuery        = "synthetic AI research news from this week"
	searchReadURL          = "https://news.example.test/ai-research"
	searchReadSearchResult = `{"results":[{"title":"Synthetic AI research announcement","url":"https://news.example.test/ai-research","snippet":"A lab announced a new research system.","published_date":"2026-11-07"}]}`
	searchReadPageResult   = `{"url":"https://news.example.test/ai-research","title":"Synthetic AI research announcement","published_at":"2026-11-07","text":"Published November 7, 2026. A lab announced a new research system."}`
	searchReadFinalAnswer  = "The lab announced a new AI research system on November 7, 2026. [Source](https://news.example.test/ai-research)."
)

// TestDSHSearchThenReadCompletesThroughWorkerWithSourceDate exercises one
// queued Task through the local Node/DSH runtime, a synthetic search/read tool
// broker, and a synthetic model. It verifies the full sequence and citation
// data without contacting a provider or public service.
func TestDSHSearchThenReadCompletesThroughWorkerWithSourceDate(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	searchTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "morde_web_search", Description: "Search for recent public pages.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"time_range":{"type":"string"}},"required":["query","time_range"],"additionalProperties":false}`),
	}}
	readTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "morde_web_read", Description: "Read one public HTTPS page and verify its contents.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
	}}
	broker := &searchReadWorkerBrokerStub{
		definitions: []app.AgentToolDefinition{searchTool, readTool},
		outputs:     map[string]string{searchTool.Name: searchReadSearchResult, readTool.Name: searchReadPageResult},
	}
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Tools = broker
	executor.options.Completion = completion
	model := &searchReadWorkerSyntheticModel{taskID: value.ID}
	models.complete = model.Complete

	workerID := "morde-search-read-worker"
	value.Lease.Owner = workerID
	claimed := false
	leaseStore := &taskLeaseStoreStub{
		claim: func(_ context.Context, gotWorkerID string, _ time.Duration) (task.Task, bool, error) {
			if gotWorkerID != workerID {
				return task.Task{}, false, errors.New("unexpected worker identity")
			}
			if claimed {
				return task.Task{}, false, nil
			}
			claimed = true
			return value, true, nil
		},
		renew: func(_ context.Context, taskID, gotWorkerID string, version int64, lease time.Duration) (task.Task, error) {
			if taskID != value.ID || gotWorkerID != workerID || version != value.Lease.Version {
				return task.Task{}, errors.New("unexpected Task lease renewal")
			}
			expiresAt := time.Now().Add(lease)
			value.Lease.ExpiresAt = &expiresAt
			return value, nil
		},
	}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: executor, WorkerID: workerID, LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()

	select {
	case <-completion.done:
	case <-ctx.Done():
		t.Fatalf("search/read Task did not complete through DSH: %v; rounds=%d brokerCalls=%d", ctx.Err(), model.callCount(), broker.callCount())
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after the synthetic Task completed")
	}

	if completion.completion.Result != searchReadFinalAnswer || completion.task.ID != value.ID || completion.completion.AgentSessionID == "" {
		t.Fatalf("completed DSH result = %#v for Task %#v; want final cited answer and checkpoint", completion.completion, completion.task)
	}
	if model.callCount() != 3 || !model.searchResultSeen || !model.readResultSeen || !model.searchToolOffered || !model.readToolOffered {
		t.Fatalf("model search/read observations = %#v; want search, exact result, read, page date, and final-answer rounds", model)
	}
	calls := broker.snapshotCalls()
	want := []searchReadWorkerCall{
		{taskID: value.ID, ownerID: value.OwnerID, initiatorID: value.InitiatorID, callID: searchReadSearchCallID,
			name: "morde_web_search", arguments: `{"query":"synthetic AI research news from this week","time_range":"week"}`, output: searchReadSearchResult},
		{taskID: value.ID, ownerID: value.OwnerID, initiatorID: value.InitiatorID, callID: searchReadReadCallID,
			name: "morde_web_read", arguments: `{"url":"https://news.example.test/ai-research"}`, output: searchReadPageResult},
	}
	if len(calls) != len(want) {
		t.Fatalf("search/read broker call count=%d calls=%#v; want %d", len(calls), calls, len(want))
	}
	for index, expected := range want {
		got := calls[index]
		if got.taskID != expected.taskID || got.ownerID != expected.ownerID || got.initiatorID != expected.initiatorID ||
			got.callID != expected.callID || got.name != expected.name || got.arguments != expected.arguments || got.output != expected.output ||
			got.operation.TaskID != value.ID || got.operation.Key != app.AgentOperationKey(expected.callID) ||
			got.operation.Status != task.OperationSucceeded || got.operation.Result != expected.output ||
			got.step.TaskID != value.ID || got.step.Sequence != int64(index+1) || got.step.Kind != "tool" ||
			got.step.Status != task.StepSucceeded || got.step.Title != expected.name || got.step.Summary != expected.output {
			t.Fatalf("search/read call %d = %#v; want exact Task-bound operation and ordered work-chain record %#v", index, got, expected)
		}
	}
}

type searchReadWorkerSyntheticModel struct {
	taskID            string
	calls             int
	searchToolOffered bool
	readToolOffered   bool
	searchResultSeen  bool
	readResultSeen    bool
}

func (m *searchReadWorkerSyntheticModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID != m.taskID {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	m.calls++
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode DSH search/read request: %w", err)
	}
	for _, tool := range request.Tools {
		switch tool.Name {
		case "morde_web_search":
			m.searchToolOffered = true
		case "morde_web_read":
			m.readToolOffered = true
		}
	}
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	switch m.calls {
	case 1:
		if !m.searchToolOffered || !m.readToolOffered {
			return app.ModelResponse{}, errors.New("search and web-read tools were not offered to DSH")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: searchReadSearchCallID, Name: "morde_web_search",
			Arguments: `{"query":"synthetic AI research news from this week","time_range":"week"}`}}, StopReason: "tool_calls", Usage: usage}, nil
	case 2:
		if !hasSearchReadToolResult(request.Messages, searchReadSearchCallID, searchReadSearchResult) {
			return app.ModelResponse{}, errors.New("exact search result was not returned to the next DSH round")
		}
		m.searchResultSeen = true
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: searchReadReadCallID, Name: "morde_web_read",
			Arguments: `{"url":"https://news.example.test/ai-research"}`}}, StopReason: "tool_calls", Usage: usage}, nil
	case 3:
		if !hasSearchReadToolResult(request.Messages, searchReadReadCallID, searchReadPageResult) ||
			!strings.Contains(searchReadPageResult, "2026-11-07") || !strings.Contains(searchReadPageResult, searchReadURL) {
			return app.ModelResponse{}, errors.New("verified page date/source were not returned to the final DSH round")
		}
		m.readResultSeen = true
		return app.ModelResponse{Text: searchReadFinalAnswer, StopReason: "stop", Usage: usage}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic model round %d", m.calls)
	}
}

func hasSearchReadToolResult(messages []struct {
	Role       string `json:"role"`
	ToolCallID string `json:"toolCallId"`
	Content    []struct {
		Text string `json:"text"`
	} `json:"content"`
}, callID, output string) bool {
	for _, message := range messages {
		if message.Role != "tool" || message.ToolCallID != callID {
			continue
		}
		for _, block := range message.Content {
			if block.Text == output {
				return true
			}
		}
	}
	return false
}

func (m *searchReadWorkerSyntheticModel) callCount() int { return m.calls }

type searchReadWorkerCall struct {
	taskID, ownerID, initiatorID string
	callID, name, arguments      string
	output                       string
	operation                    task.Operation
	step                         task.Step
}

type searchReadWorkerBrokerStub struct {
	definitions []app.AgentToolDefinition
	outputs     map[string]string
	calls       []searchReadWorkerCall
}

func (b *searchReadWorkerBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *searchReadWorkerBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	var expected searchReadWorkerCall
	switch call.Name {
	case "morde_web_search":
		expected = searchReadWorkerCall{callID: searchReadSearchCallID, name: call.Name,
			arguments: `{"query":"synthetic AI research news from this week","time_range":"week"}`}
	case "morde_web_read":
		expected = searchReadWorkerCall{callID: searchReadReadCallID, name: call.Name,
			arguments: `{"url":"https://news.example.test/ai-research"}`}
	default:
		return app.AgentToolResult{}, fmt.Errorf("unexpected search/read tool %q", call.Name)
	}
	if call.CallID != expected.callID || call.Arguments != expected.arguments {
		return app.AgentToolResult{}, fmt.Errorf("unexpected synthetic search/read call: %#v", call)
	}
	expected.taskID, expected.ownerID, expected.initiatorID = claimed.ID, claimed.OwnerID, claimed.InitiatorID
	expected.output = b.outputs[call.Name]
	expected.operation = task.Operation{ID: "operation-" + call.CallID, TaskID: claimed.ID,
		Key: app.AgentOperationKey(call.CallID), Status: task.OperationSucceeded, Result: expected.output}
	expected.step = task.Step{ID: "step-" + call.CallID, TaskID: claimed.ID, Sequence: int64(len(b.calls) + 1),
		Kind: "tool", Status: task.StepSucceeded, Title: call.Name, Summary: expected.output, CreatedAt: time.Now()}
	b.calls = append(b.calls, expected)
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: expected.output}, nil
}

func (b *searchReadWorkerBrokerStub) callCount() int { return len(b.calls) }

func (b *searchReadWorkerBrokerStub) snapshotCalls() []searchReadWorkerCall {
	return append([]searchReadWorkerCall(nil), b.calls...)
}
