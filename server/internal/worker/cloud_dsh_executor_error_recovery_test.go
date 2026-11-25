package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/app"
)

func TestCloudDSHExecutorLetsSameAgentRecoverFromDefinitiveReadFailure(t *testing.T) {
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	read := cloudDSHReadTool()
	search := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "workspace_search", Description: "Search readable workspace files.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"},"query":{"type":"string"}},"required":["workspace_id","query"],"additionalProperties":false}`),
	}}
	const failure = "README.md changed while the requested read was starting"
	tools := &cloudDSHTestTools{
		definitions: []app.AgentToolDefinition{read, search},
		result:      app.AgentToolResult{Status: app.AgentToolFailed, Output: failure},
	}
	tools.afterExecute = func() {
		if len(tools.calls) == 2 {
			tools.result = app.AgentToolResult{Status: app.AgentToolSucceeded, Output: `{"matches":["docs/guide.md"]}`}
		}
	}
	executor.options.Tools = tools
	completer := &cloudDSHTestCompleter{instances: store}
	executor.options.Completion = completer
	value.Budget.ToolCalls = 2
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_read_failed_1", Name: read.Name,
			Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{ToolCalls: []app.ModelToolCall{{CallID: "call_search_alternative_1", Name: search.Name,
			Arguments: `{"workspace_id":"w1234567","query":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}},
		{Text: "README.md 的读取未成功；我改用授权的工作区搜索，找到了 docs/guide.md。", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 12}},
	}

	if err := executor.ExecuteTask(context.Background(), value); err != nil {
		t.Fatalf("recovered read task returned error: %v", err)
	}
	if len(models.calls) != 3 || len(tools.calls) != 2 || completer.calls != 1 ||
		completer.completion.Result != "README.md 的读取未成功；我改用授权的工作区搜索，找到了 docs/guide.md。" {
		t.Fatalf("models=%d tools=%#v completions=%d result=%q; want one failed read, one alternative read, and a completed task",
			len(models.calls), tools.calls, completer.calls, completer.completion.Result)
	}
	if tools.calls[0].CallID != "call_read_failed_1" || tools.calls[0].Name != read.Name ||
		tools.calls[1].CallID != "call_search_alternative_1" || tools.calls[1].Name != search.Name {
		t.Fatalf("tool dispatches = %#v; want each model-selected read action dispatched exactly once", tools.calls)
	}
	if supervisor.launch.Fence.AgentSessionID == "" || models.calls[0].Task == nil || models.calls[1].Task == nil || models.calls[2].Task == nil ||
		models.calls[0].Task.AgentSessionID != supervisor.launch.Fence.AgentSessionID ||
		models.calls[1].Task.AgentSessionID != supervisor.launch.Fence.AgentSessionID ||
		models.calls[2].Task.AgentSessionID != supervisor.launch.Fence.AgentSessionID {
		t.Fatalf("model requests did not remain in the original DSH session: launch=%+v calls=%+v",
			supervisor.launch.Fence, models.calls)
	}
	if len(tools.claimed) != 2 || tools.claimed[0].InitiatorID != value.InitiatorID || tools.claimed[1].InitiatorID != value.InitiatorID ||
		!validCloudDSHModelRequest(models.calls[0].RawRequest, value.ModelID,
			cloudDSHExpectedRootDefinitions([]app.AgentToolDefinition{read, search})) ||
		!validCloudDSHModelRequest(models.calls[1].RawRequest, value.ModelID,
			cloudDSHExpectedRootDefinitions([]app.AgentToolDefinition{read, search})) ||
		!validCloudDSHModelRequest(models.calls[2].RawRequest, value.ModelID,
			cloudDSHExpectedRootDefinitions([]app.AgentToolDefinition{read, search})) {
		t.Fatalf("tool authorization or model request fence failed: claimed=%+v", tools.claimed)
	}

	assertToolResult := func(callIndex int, callID, want string) {
		t.Helper()
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(models.calls[callIndex].RawRequest, &request); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == callID && len(message.Content) == 1 && message.Content[0].Text == want {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("model request %d has %d matching tool results for %q; want one %q: %s",
				callIndex+1, found, callID, want, models.calls[callIndex].RawRequest)
		}
	}
	assertToolResult(1, "call_read_failed_1", "Tool failed: "+failure)
	assertToolResult(2, "call_read_failed_1", "Tool failed: "+failure)
	assertToolResult(2, "call_search_alternative_1", `{"matches":["docs/guide.md"]}`)

	var persistedFailureResults int
	for _, command := range checkpoints.commands {
		var payload struct {
			Events []struct {
				Type string `json:"type"`
				Data struct {
					Message struct {
						ToolCallID string `json:"toolCallId"`
						IsError    bool   `json:"isError"`
						Content    []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"message"`
				} `json:"data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			t.Fatalf("decode persisted checkpoint: %v", err)
		}
		for _, event := range payload.Events {
			if event.Type == "tool/result" && event.Data.Message.ToolCallID == "call_read_failed_1" {
				persistedFailureResults++
				if len(event.Data.Message.Content) != 1 ||
					event.Data.Message.Content[0].Text != "Tool failed: "+failure {
					t.Fatalf("failed work item was not retained as the confirmed failure: %+v", event.Data.Message)
				}
			}
		}
	}
	if persistedFailureResults != 1 {
		t.Fatalf("persisted failed work item count=%d; want exactly one", persistedFailureResults)
	}
}

func TestCloudDSHExecutorLetsSameAgentCorrectInvalidRuntimeArguments(t *testing.T) {
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	read := cloudDSHReadTool()
	const invalidArgumentsResult = "The tool call was not executed because its arguments do not match the declared input format. Correct the arguments and try again."
	tools := &cloudDSHTestTools{
		definitions: []app.AgentToolDefinition{read},
		result:      app.AgentToolResult{Status: app.AgentToolFailed, Output: invalidArgumentsResult},
	}
	tools.afterExecute = func() {
		if len(tools.calls) == 2 {
			tools.result = app.AgentToolResult{Status: app.AgentToolSucceeded, Output: `{"path":"README.md","text":"authorized content"}`}
		}
	}
	executor.options.Tools = tools
	completer := &cloudDSHTestCompleter{instances: store}
	executor.options.Completion = completer
	value.Budget.ToolCalls = 2
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_read_invalid_args_1", Name: read.Name,
			Arguments: `{"path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{ToolCalls: []app.ModelToolCall{{CallID: "call_read_corrected_args_1", Name: read.Name,
			Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}},
		{Text: "我补齐了工作区参数，已读取 README.md。", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 12}},
	}

	if err := executor.ExecuteTask(context.Background(), value); err != nil {
		t.Fatalf("invalid-argument recovery task returned error: %v", err)
	}
	if len(models.calls) != 3 || len(tools.calls) != 2 || completer.calls != 1 ||
		completer.completion.Result != "我补齐了工作区参数，已读取 README.md。" {
		t.Fatalf("models=%d tools=%#v completions=%d result=%q; want one rejected call, one corrected read, and a completed task",
			len(models.calls), tools.calls, completer.calls, completer.completion.Result)
	}
	var invalidArguments, correctedArguments map[string]json.RawMessage
	if json.Unmarshal([]byte(tools.calls[0].Arguments), &invalidArguments) != nil ||
		json.Unmarshal([]byte(tools.calls[1].Arguments), &correctedArguments) != nil {
		t.Fatalf("tool arguments are not valid JSON: %#v", tools.calls)
	}
	var invalidPath, correctedPath, correctedWorkspaceID string
	if json.Unmarshal(invalidArguments["path"], &invalidPath) != nil ||
		json.Unmarshal(correctedArguments["path"], &correctedPath) != nil ||
		json.Unmarshal(correctedArguments["workspace_id"], &correctedWorkspaceID) != nil ||
		tools.calls[0].CallID != "call_read_invalid_args_1" || tools.calls[0].Name != read.Name ||
		len(invalidArguments) != 1 || invalidPath != "README.md" ||
		tools.calls[1].CallID != "call_read_corrected_args_1" || tools.calls[1].Name != read.Name ||
		len(correctedArguments) != 2 || correctedWorkspaceID != "w1234567" || correctedPath != "README.md" {
		t.Fatalf("tool dispatches=%#v; want one failed call and one new, corrected call", tools.calls)
	}
	for index, call := range models.calls {
		if call.Task == nil || supervisor.launch.Fence.AgentSessionID == "" ||
			call.Task.AgentSessionID != supervisor.launch.Fence.AgentSessionID {
			t.Fatalf("model request %d left the original DSH session: launch=%+v call=%+v",
				index+1, supervisor.launch.Fence, call)
		}
	}
	assertToolResult := func(callIndex int, callID, want string) {
		t.Helper()
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(models.calls[callIndex].RawRequest, &request); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == callID && len(message.Content) == 1 && message.Content[0].Text == want {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("model request %d has %d matching tool results for %q; want one %q: %s",
				callIndex+1, found, callID, want, models.calls[callIndex].RawRequest)
		}
	}
	assertToolResult(1, "call_read_invalid_args_1", "Tool failed: "+invalidArgumentsResult)
	assertToolResult(2, "call_read_invalid_args_1", "Tool failed: "+invalidArgumentsResult)
	assertToolResult(2, "call_read_corrected_args_1", `{"path":"README.md","text":"authorized content"}`)

	var persistedFailureResults, persistedSuccessResults int
	for _, command := range checkpoints.commands {
		var payload struct {
			Events []struct {
				Type string `json:"type"`
				Data struct {
					Message struct {
						ToolCallID string `json:"toolCallId"`
						IsError    bool   `json:"isError"`
						Content    []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"message"`
				} `json:"data"`
			} `json:"events"`
		}
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			t.Fatalf("decode persisted checkpoint: %v", err)
		}
		for _, event := range payload.Events {
			if event.Type != "tool/result" || len(event.Data.Message.Content) != 1 {
				continue
			}
			message := event.Data.Message
			switch message.ToolCallID {
			case "call_read_invalid_args_1":
				persistedFailureResults++
				if message.Content[0].Text != "Tool failed: "+invalidArgumentsResult {
					t.Fatalf("pre-dispatch failure was not retained: %+v", message)
				}
			case "call_read_corrected_args_1":
				persistedSuccessResults++
				if message.IsError || message.Content[0].Text != `{"path":"README.md","text":"authorized content"}` {
					t.Fatalf("corrected tool result was not retained: %+v", message)
				}
			}
		}
	}
	if persistedFailureResults != 1 || persistedSuccessResults != 1 {
		t.Fatalf("persisted failure results=%d success results=%d; want exactly one each",
			persistedFailureResults, persistedSuccessResults)
	}
}

func TestCloudDSHExecutorKeepsUnknownReadOutcomeDistinctFromDefinitiveFailure(t *testing.T) {
	executor, _, store, _, models, value := cloudDSHFixture(t)
	read := cloudDSHReadTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{read}, result: app.AgentToolResult{Status: app.AgentToolUnknown}}
	executor.options.Tools = tools
	models.responses = []app.ModelResponse{{ToolCalls: []app.ModelToolCall{{CallID: "call_read_unknown_1", Name: read.Name,
		Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
		StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}}}

	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || errors.Is(err, ErrCloudDSHCompletionPending) || len(models.calls) != 1 || len(tools.calls) != 1 || store.stopped != 1 {
		t.Fatalf("unknown outcome became a normal result or was retried: err=%v models=%d tools=%d stopped=%d",
			err, len(models.calls), len(tools.calls), store.stopped)
	}
	failure, ok := err.(interface {
		ToolExecutionDiagnostic() cloudDSHToolExecutionDiagnostic
	})
	if !ok {
		t.Fatalf("unknown outcome lost its structured tool diagnostic: %T %v", err, err)
	}
	diagnostic := failure.ToolExecutionDiagnostic()
	if diagnostic.Stage != "tool_execution" || diagnostic.ReasonCode != "operation_outcome_unknown" ||
		diagnostic.Dispatch != "unknown" || diagnostic.ToolName != read.Name || diagnostic.CallID != "call_read_unknown_1" {
		t.Fatalf("unknown outcome was reclassified as a definitive failure: %+v", diagnostic)
	}
}
