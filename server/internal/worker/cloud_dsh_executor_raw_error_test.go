package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHRawErrorTestTools struct {
	*cloudDSHTestTools
	executeErr error
}

func (t *cloudDSHRawErrorTestTools) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	t.calls = append(t.calls, call)
	t.claimed = append(t.claimed, claimed)
	return app.AgentToolResult{}, t.executeErr
}

func TestCloudDSHExecutorKeepsRawToolErrorUnknownWithoutRetry(t *testing.T) {
	executor, _, store, _, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	tools := &cloudDSHRawErrorTestTools{
		cloudDSHTestTools: &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}},
		executeErr:        errors.New("tool execution ended before its outcome could be confirmed"),
	}
	executor.options.Tools = tools
	completer := &cloudDSHTestCompleter{instances: store}
	executor.options.Completion = completer
	models.responses = []app.ModelResponse{{
		ToolCalls: []app.ModelToolCall{{CallID: "call_read_raw_error_1", Name: tool.Name,
			Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
		StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2},
	}}

	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("raw tool error execution = %v; want an unknown execution failure", err)
	}
	var failure interface {
		ToolExecutionDiagnostic() cloudDSHToolExecutionDiagnostic
	}
	if !errors.As(err, &failure) {
		t.Fatalf("raw tool error lost its structured diagnostic: %T %v", err, err)
	}
	diagnostic := failure.ToolExecutionDiagnostic()
	if diagnostic.Stage != "tool_execution" || diagnostic.ReasonCode != "tool_executor_error" ||
		diagnostic.Dispatch != "unknown" || diagnostic.ToolName != tool.Name || diagnostic.CallID != "call_read_raw_error_1" {
		t.Fatalf("raw tool error was not kept unknown: %+v", diagnostic)
	}
	if len(models.calls) != 1 || len(tools.calls) != 1 || store.stopped != 1 || completer.calls != 0 {
		t.Fatalf("models=%d tool calls=%d stopped=%d completions=%d; want one model request, one tool call, stopped execution, and no completion",
			len(models.calls), len(tools.calls), store.stopped, completer.calls)
	}
	if models.calls[0].Task == nil || models.calls[0].Task.AgentSessionID == "" {
		t.Fatal("tool error was not associated with the original DSH session")
	}
	if !validCloudDSHModelRequest(models.calls[0].RawRequest, value.ModelID,
		cloudDSHExpectedRootDefinitions([]app.AgentToolDefinition{tool})) {
		t.Fatalf("model request did not use the authorized root tool set: %s", json.RawMessage(models.calls[0].RawRequest))
	}
}
