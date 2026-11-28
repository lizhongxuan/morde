package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
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
	value.InitiatorID = value.OwnerID
	expiresAt := time.Now().Add(time.Hour)
	value.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567",
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}, Version: 1, ExpiresAt: &expiresAt}
	value.ConversationContext = &task.ConversationContext{
		OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, ConversationID: value.ConversationID,
		Scope: "personal", ThroughSequence: 0, Digest: bytes.Repeat([]byte{3}, 32),
		AgentToolProfile: task.AgentToolProfileWebResearch,
	}
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
	children := &cloudDSHProfileNoChildStore{}
	executor.options.Tools = broker
	executor.options.Children = children
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
	allowedProfileTools := map[string]bool{
		"morde_web_search": true, "morde_web_read": true, cloudDSHResultEvidenceToolName: true,
	}
	if len(model.offeredToolNames) != len(allowedProfileTools) {
		t.Fatalf("initial web research toolset = %v; want only Broker-authorized search/read plus the internal evidence mapper", model.offeredToolNames)
	}
	for _, name := range model.offeredToolNames {
		if !allowedProfileTools[name] {
			t.Fatalf("initial web research exposed an unrelated DSH tool %q in %v", name, model.offeredToolNames)
		}
	}
	if children.listCalls != 0 {
		t.Fatalf("web research profile queried child sessions %d times; child work must stay disabled", children.listCalls)
	}
	if broker.initialContextCalls != 0 || broker.memoryProofCalls != 0 {
		t.Fatalf("web research loaded or validated personal context: initial_context_calls=%d memory_proof_calls=%d",
			broker.initialContextCalls, broker.memoryProofCalls)
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

func TestDSHWebResearchDuplicateCallDenialReturnsToAgentWithoutSecondDispatch(t *testing.T) {
	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	value.InitiatorID = value.OwnerID
	expiresAt := time.Now().Add(time.Hour)
	value.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567",
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}, Version: 1, ExpiresAt: &expiresAt}
	value.ConversationContext = &task.ConversationContext{
		OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, ConversationID: value.ConversationID,
		Scope: "personal", ThroughSequence: 0, Digest: bytes.Repeat([]byte{3}, 32),
		AgentToolProfile: task.AgentToolProfileWebResearch,
	}
	searchTool := app.AgentToolDefinition{ModelTool: app.ModelTool{Name: cloudDSHWebResearchSearchToolName,
		Description: "Search public web pages.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}}
	readTool := app.AgentToolDefinition{ModelTool: app.ModelTool{Name: cloudDSHWebResearchReadToolName,
		Description: "Read one public web page.", Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`)}}
	broker := &boundedSearchReadWorkerBrokerStub{definitions: []app.AgentToolDefinition{searchTool, readTool}}
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Tools = broker
	executor.options.Children = &cloudDSHProfileNoChildStore{}
	executor.options.Completion = completion
	model := &boundedSearchReadWorkerModel{taskID: value.ID}
	models.complete = model.Complete

	workerID := "morde-web-research-bound-worker"
	logCore, observedLogs := observer.New(zapcore.DebugLevel)
	value.Lease.Owner = workerID
	claimed := false
	leaseStore := &taskLeaseStoreStub{
		claim: func(_ context.Context, gotWorkerID string, _ time.Duration) (task.Task, bool, error) {
			if gotWorkerID != workerID {
				return task.Task{}, false, errors.New("unexpected Worker identity")
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
		PollInterval: time.Millisecond, MaxConcurrentTasks: 1, Logger: zap.New(logCore),
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()
	select {
	case <-completion.done:
	case <-ctx.Done():
		cancel()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
		}
		lastCheckpoint := "none"
		if len(checkpoints.commands) > 0 {
			lastCheckpoint = string(checkpoints.commands[len(checkpoints.commands)-1].Payload)
			if len(lastCheckpoint) > 5000 {
				lastCheckpoint = lastCheckpoint[len(lastCheckpoint)-5000:]
			}
		}
		var logSummaries []string
		for _, entry := range observedLogs.All() {
			logSummaries = append(logSummaries, fmt.Sprintf("%s %#v", entry.Message, entry.Context))
		}
		t.Fatalf("bounded web research did not complete through DSH: %v; model rounds=%d broker calls=%d last_checkpoint_tail=%s logs=%s",
			ctx.Err(), model.calls, len(broker.calls), lastCheckpoint, strings.Join(logSummaries, " | "))
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after the bounded Task completed")
	}
		if completion.completion.Result != searchReadFinalAnswer {
		t.Fatalf("bounded research completion = %q; want DSH to synthesize after the denial", completion.completion.Result)
	}
		if model.calls != 4 || !model.denialSeen {
			t.Fatalf("model rounds=%d denial_seen=%t; want DSH to receive its persisted guard denial and answer in the next turn", model.calls, model.denialSeen)
	}
	if len(broker.calls) != 2 || broker.calls[0].Name != cloudDSHWebResearchSearchToolName ||
		broker.calls[1].Name != cloudDSHWebResearchReadToolName {
		t.Fatalf("Broker dispatches = %#v; duplicate search must not dispatch", broker.calls)
	}
}

type boundedSearchReadWorkerBrokerStub struct {
	definitions []app.AgentToolDefinition
	calls       []app.ModelToolCall
}

func (b *boundedSearchReadWorkerBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *boundedSearchReadWorkerBrokerStub) Execute(_ context.Context, _ task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	b.calls = append(b.calls, call)
	switch call.Name {
	case cloudDSHWebResearchSearchToolName:
		return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: searchReadSearchResult}, nil
	case cloudDSHWebResearchReadToolName:
		return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: searchReadPageResult}, nil
	default:
		return app.AgentToolResult{}, fmt.Errorf("unexpected web research tool %q", call.Name)
	}
}

type boundedSearchReadWorkerModel struct {
	taskID     string
	calls      int
	denialSeen bool
}

func (m *boundedSearchReadWorkerModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID != m.taskID {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	m.calls++
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
		return app.ModelResponse{}, fmt.Errorf("decode bounded research model request: %w", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	switch m.calls {
	case 1:
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "web_research_search_1", Name: cloudDSHWebResearchSearchToolName,
			Arguments: `{"query":"synthetic AI research news from this week"}`}}, StopReason: "tool_calls", Usage: usage}, nil
	case 2:
		if !hasSearchReadToolResult(request.Messages, "web_research_search_1", searchReadSearchResult) {
			return app.ModelResponse{}, errors.New("search result was not returned to DSH")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "web_research_read_1", Name: cloudDSHWebResearchReadToolName,
			Arguments: `{"url":"https://news.example.test/ai-research"}`}}, StopReason: "tool_calls", Usage: usage}, nil
	case 3:
		if !hasSearchReadToolResult(request.Messages, "web_research_read_1", searchReadPageResult) {
			return app.ModelResponse{}, errors.New("page result was not returned to DSH")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "web_research_search_2", Name: cloudDSHWebResearchSearchToolName,
			Arguments: `{"query":"  SYNTHETIC AI RESEARCH NEWS FROM THIS WEEK  "}`}}, StopReason: "tool_calls", Usage: usage}, nil
	case 4:
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != "web_research_search_2" {
				continue
			}
			for _, block := range message.Content {
				if strings.Contains(strings.ToLower(block.Text), "web_research guard:") ||
					strings.Contains(strings.ToLower(block.Text), "bounded web research allowance") {
					m.denialSeen = true
				}
			}
		}
		if !m.denialSeen {
			return app.ModelResponse{}, errors.New("Worker denial feedback was not returned to DSH")
		}
		return app.ModelResponse{Text: searchReadFinalAnswer, StopReason: "stop", Usage: usage}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected bounded research model round %d", m.calls)
	}
}

type searchReadWorkerSyntheticModel struct {
	taskID            string
	calls             int
	offeredToolNames  []string
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
		if m.calls == 1 {
			m.offeredToolNames = append(m.offeredToolNames, tool.Name)
		}
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
	definitions         []app.AgentToolDefinition
	outputs             map[string]string
	calls               []searchReadWorkerCall
	initialContextCalls int
	memoryProofCalls    int
}

func (b *searchReadWorkerBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *searchReadWorkerBrokerStub) InitialContext(context.Context, task.Task) (string, error) {
	b.initialContextCalls++
	return "Unrelated private personal context that must not enter web research.", nil
}

func (*searchReadWorkerBrokerStub) CloudDSHPersonalMemoryProofEnabled() bool { return true }

func (*searchReadWorkerBrokerStub) CloudDSHPersonalMemoryProofEnabledForTask(task.Task) bool {
	return true
}

func (b *searchReadWorkerBrokerStub) ValidatePersonalMemoryProof(context.Context, task.Task, string,
	[]app.CloudDSHMemoryVersion) error {
	b.memoryProofCalls++
	return errors.New("personal memory proof is not part of web research")
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
