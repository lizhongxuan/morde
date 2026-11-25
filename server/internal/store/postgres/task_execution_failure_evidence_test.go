package postgres

import (
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestValidTaskToolCallFailureEvidence(t *testing.T) {
	baseStep := task.Step{Kind: "tool", Status: task.StepFailed}
	tests := []struct {
		name  string
		call  app.TaskToolCallProgress
		step  task.Step
		valid bool
	}{
		{name: "no failure", call: app.TaskToolCallProgress{Phase: "execute"}, step: baseStep, valid: true},
		{name: "not dispatched retryable", call: app.TaskToolCallProgress{Phase: "prepare", FailureReason: "not_dispatched", Retryable: true}, step: baseStep, valid: true},
		{name: "not dispatched non-retryable", call: app.TaskToolCallProgress{Phase: "prepare", FailureReason: "not_dispatched"}, step: baseStep, valid: true},
		{name: "connector rate limited", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "mcp_call", FailureReason: "connector_rate_limited", Retryable: true}, step: baseStep, valid: true},
		{name: "definitive tool failure", call: app.TaskToolCallProgress{Phase: "execute", FailureReason: "tool_execution_failed"}, step: baseStep, valid: true},
		{name: "definitive tool failure cannot retry", call: app.TaskToolCallProgress{Phase: "execute", FailureReason: "tool_execution_failed", Retryable: true}, step: baseStep},
		{name: "definitive tool failure needs execute phase", call: app.TaskToolCallProgress{Phase: "prepare", FailureReason: "tool_execution_failed"}, step: baseStep},
		{name: "definitive tool failure needs failed step", call: app.TaskToolCallProgress{Phase: "execute", FailureReason: "tool_execution_failed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}},
		{name: "definitive tool failure needs tool step", call: app.TaskToolCallProgress{Phase: "execute", FailureReason: "tool_execution_failed"}, step: task.Step{Kind: "agent", Status: task.StepFailed}},
		{name: "failed test run", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "test_failed"}, step: baseStep, valid: true},
		{name: "failed test run cannot retry", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "test_failed", Retryable: true}, step: baseStep},
		{name: "failed test run needs execute phase", call: app.TaskToolCallProgress{Phase: "prepare", ToolName: "shell_exec", FailureReason: "test_failed"}, step: baseStep},
		{name: "failed test run needs failed step", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "test_failed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}},
		{name: "failed test run needs tool step", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "test_failed"}, step: task.Step{Kind: "agent", Status: task.StepFailed}},
		{name: "passing test run", call: app.TaskToolCallProgress{Phase: "succeeded", ToolName: "shell_exec", FailureReason: "test_passed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}, valid: true},
		{name: "passing full-suite test run", call: app.TaskToolCallProgress{Phase: "succeeded", ToolName: "shell_exec", FailureReason: "test_suite_passed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}, valid: true},
		{name: "passing test run via execute phase", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "test_passed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}, valid: true},
		{name: "passing test run cannot retry", call: app.TaskToolCallProgress{Phase: "succeeded", ToolName: "shell_exec", FailureReason: "test_suite_passed", Retryable: true}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}},
		{name: "passing test run needs succeeded step", call: app.TaskToolCallProgress{Phase: "succeeded", ToolName: "shell_exec", FailureReason: "test_passed"}, step: baseStep},
		{name: "passing test run needs tool step", call: app.TaskToolCallProgress{Phase: "succeeded", ToolName: "shell_exec", FailureReason: "test_passed"}, step: task.Step{Kind: "agent", Status: task.StepSucceeded}},
		{name: "passing test run needs a run phase", call: app.TaskToolCallProgress{Phase: "prepare", ToolName: "shell_exec", FailureReason: "test_passed"}, step: task.Step{Kind: "tool", Status: task.StepSucceeded}},
		{name: "unknown failure reason", call: app.TaskToolCallProgress{Phase: "execute", FailureReason: "raw_error"}, step: baseStep},
		{name: "retryable without reason", call: app.TaskToolCallProgress{Phase: "execute", Retryable: true}, step: baseStep},
		{name: "connector rate limit cannot be non-retryable", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "mcp_call", FailureReason: "connector_rate_limited"}, step: baseStep},
		{name: "connector rate limit requires MCP tool", call: app.TaskToolCallProgress{Phase: "execute", ToolName: "shell_exec", FailureReason: "connector_rate_limited", Retryable: true}, step: baseStep},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validTaskToolCallFailureEvidence(test.call, test.step); got != test.valid {
				t.Fatalf("validTaskToolCallFailureEvidence() = %t, want %t", got, test.valid)
			}
		})
	}
}
