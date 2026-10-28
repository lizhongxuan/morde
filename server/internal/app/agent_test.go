package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

var _ interface {
	ExecuteTask(context.Context, task.Task) error
} = (*Agent)(nil)

func TestAgentExecuteTaskCompletesVerifiedToolLoop(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-1", Name: "inspect", Arguments: `{}`}}, Usage: ModelUsage{Known: true, InputTokens: 10, OutputTokens: 2}},
		ModelResponse{Text: "The requested check is complete.", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4}},
	)
	fixture.tools.result = AgentToolResult{Status: AgentToolSucceeded, ExternalID: "runtime-1", Output: "Observed the requested state."}
	fixture.tools.verification = AgentVerificationResult{Status: AgentVerified, Summary: "The runtime state matches the requested outcome."}

	if err := fixture.agent.ExecuteTask(context.Background(), fixture.claimed); err != nil {
		t.Fatalf("ExecuteTask() error = %v", err)
	}
	if fixture.store.details.Task.Status != task.StatusCompleted {
		t.Fatalf("task status = %q, want completed", fixture.store.details.Task.Status)
	}
	if fixture.store.details.Task.Result != "The requested check is complete." {
		t.Fatalf("task result = %q", fixture.store.details.Task.Result)
	}
	if fixture.store.details.Task.Usage.ToolCalls != 2 {
		t.Fatalf("tool call usage = %d, want action + verification", fixture.store.details.Task.Usage.ToolCalls)
	}
	if fixture.store.details.Task.Usage.ModelTokens == nil || *fixture.store.details.Task.Usage.ModelTokens != 24 {
		t.Fatalf("model token usage = %v, want 24", fixture.store.details.Task.Usage.ModelTokens)
	}
	if fixture.tools.executeCalls != 1 || fixture.tools.verifyCalls != 1 {
		t.Fatalf("tool calls: execute=%d verify=%d, want 1 each", fixture.tools.executeCalls, fixture.tools.verifyCalls)
	}
	if len(fixture.store.details.Steps) < 4 || len(fixture.store.events) < 6 {
		t.Fatalf("progress was not persisted: steps=%d events=%d", len(fixture.store.details.Steps), len(fixture.store.events))
	}
	operation := fixture.store.operations[agentOperationKey("call-1")]
	if operation.Status != task.OperationSucceeded || operation.ExternalID != "runtime-1" {
		t.Fatalf("operation = %+v, want succeeded with external id", operation)
	}
	if fixture.tools.lastRequest.OperationID != operation.ID {
		t.Fatalf("tool executor operation ID = %q, want persisted ID %q", fixture.tools.lastRequest.OperationID, operation.ID)
	}
	for _, event := range fixture.store.events {
		if strings.Contains(string(event.Payload), "model-secret") {
			t.Fatal("model credential was persisted in an event")
		}
	}
	if len(fixture.client.requests) != 2 {
		t.Fatalf("model request count = %d, want 2", len(fixture.client.requests))
	}
	secondInput := fixture.client.requests[1].Input
	var sawCall, sawOutput bool
	for _, input := range secondInput {
		sawCall = sawCall || input.Type == "function_call"
		sawOutput = sawOutput || (input.Type == "function_call_output" && input.Output == "Observed the requested state.")
	}
	if !sawCall || !sawOutput {
		t.Fatalf("second model request did not include the tool call and result: %+v", secondInput)
	}
}

func TestAgentStreamsAssistantTextAndPersistsFinalMessageAfterCompleteResponse(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "Hello world.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 3}})
	fixture.client.deltas = [][]string{{"Hello ", "world."}}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("Run() = %+v, %v; want completed", result, err)
	}
	if len(fixture.store.assistantMessages) != 1 {
		t.Fatalf("assistant messages = %#v, want exactly one completed message", fixture.store.assistantMessages)
	}
	message := fixture.store.assistantMessages[0]
	if message.Role != conversation.RoleAssistant || message.TaskID != fixture.claimed.ID || message.Content != "Hello world." {
		t.Fatalf("assistant message = %#v", message)
	}

	var deltas []map[string]any
	var deltaSequences []int64
	var final *map[string]any
	var finalSequence int64
	for _, event := range fixture.store.events {
		if event.Type != "task.message" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["done"] == true {
			final = &payload
			finalSequence = event.Sequence
		} else if payload["role"] == "assistant" {
			deltas = append(deltas, payload)
			deltaSequences = append(deltaSequences, event.Sequence)
		}
	}
	if len(deltas) != 2 || final == nil {
		t.Fatalf("assistant events: deltas=%#v final=%#v all=%#v", deltas, final, fixture.store.events)
	}
	for index, expected := range []string{"Hello ", "world."} {
		if deltas[index]["delta"] != expected || deltas[index]["message_id"] != message.ID || deltas[index]["turn_id"] == "" {
			t.Errorf("delta[%d] = %#v; want stable message and turn IDs with %q", index, deltas[index], expected)
		}
	}
	if (*final)["message_id"] != message.ID || (*final)["content"] != message.Content || (*final)["done"] != true {
		t.Fatalf("final assistant event = %#v", *final)
	}
	if deltaSequences[0] >= finalSequence || deltaSequences[1] >= finalSequence {
		t.Fatal("final assistant message event did not follow its persisted deltas")
	}
}

func TestAgentIncompleteStreamKeepsOnlyPartialEventsAndFails(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "Partial", StopReason: "incomplete", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}})
	fixture.client.deltas = [][]string{{"Part", "ial"}}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorModelIncomplete || result.Status != task.StatusFailed || fixture.store.details.Task.Status != task.StatusFailed {
		t.Fatalf("Run() = %+v, %v; task status=%q; want failed incomplete response", result, err, fixture.store.details.Task.Status)
	}
	if len(fixture.store.assistantMessages) != 0 {
		t.Fatalf("partial response persisted as final message: %#v", fixture.store.assistantMessages)
	}
	for _, event := range fixture.store.events {
		if event.Type != "task.message" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["done"] == true || payload["content"] != nil {
			t.Fatalf("partial response was marked complete: %#v", payload)
		}
	}
}

func TestAgentStopsModelRequestWhenDeltaPersistenceFails(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "must not complete", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}})
	fixture.client.deltas = [][]string{{"part"}}
	failure := errors.New("synthetic task event write failure")
	fixture.store.deltaErr = failure

	_, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if !errors.Is(err, failure) {
		t.Fatalf("Run() error = %v, want delta storage error", err)
	}
	if fixture.store.details.Task.Status != task.StatusRunning || len(fixture.store.assistantMessages) != 0 {
		t.Fatalf("failed delta write changed completion state: status=%q messages=%#v", fixture.store.details.Task.Status, fixture.store.assistantMessages)
	}
}

func TestAgentUnknownModelUsageStopsBeforeToolDispatch(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-unknown-usage", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{InputTokens: 99, OutputTokens: 99},
	})

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != task.StatusWaitingUser || result.WaitingReason != "model_usage_unknown" {
		t.Fatalf("result = %+v, want pause for unknown usage", result)
	}
	if fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("unknown usage dispatched work: prepare=%d execute=%d operations=%d", fixture.tools.prepareCalls, fixture.tools.executeCalls, len(fixture.store.operations))
	}
	if fixture.store.details.Task.Usage.ModelTokens != nil {
		t.Fatalf("unknown provider usage was recorded as %d tokens", *fixture.store.details.Task.Usage.ModelTokens)
	}
}

func TestAgentRejectsDuplicateJSONArgumentsBeforePrepare(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-duplicate-json", Name: "inspect", Arguments: `{"x":1,"x":2}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})

	_, err := fixture.agent.Run(context.Background(), fixture.claimed)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorModelOutput {
		t.Fatalf("Run() error = %v, want invalid_model_output", err)
	}
	if fixture.store.details.Task.Status != task.StatusFailed || fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 {
		t.Fatalf("invalid arguments crossed the tool boundary: task=%q prepare=%d execute=%d", fixture.store.details.Task.Status, fixture.tools.prepareCalls, fixture.tools.executeCalls)
	}
}

func TestAgentWaitsForApprovalWithoutCreatingOperation(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-approval", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.preparation = AgentToolPreparation{
		Status: AgentToolApprovalRequired,
		Fence:  AgentHostFence{RuntimeGeneration: 4, RuntimeInstanceID: "ri_test", ControlVersion: 9, AIAllowed: true},
		Approval: AgentApprovalRequest{
			Object: "Workspace project", Content: "Upload the generated report?", Impact: "The report will leave this Host.",
		},
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != task.StatusWaitingUser || result.WaitingReason != "approval_required" {
		t.Fatalf("result = %+v, want approval pause", result)
	}
	if fixture.tools.executeCalls != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("approval request executed or registered operation: execute=%d operations=%d", fixture.tools.executeCalls, len(fixture.store.operations))
	}
	if pending := fixture.store.details.PendingRequest; pending == nil || pending.ID == "" || pending.Version != 1 || pending.Status != task.PendingRequestPending || pending.ToolName != "inspect" || pending.CallID != "call-approval" {
		t.Fatalf("approval was not persisted as a stable request: %#v", pending)
	}
	var foundApproval bool
	for _, event := range fixture.store.events {
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		encoded, _ := json.Marshal(payload["detail"])
		foundApproval = foundApproval || strings.Contains(string(encoded), "The report will leave this Host.")
	}
	if !foundApproval {
		t.Fatal("approval object/content/impact was not persisted for the user")
	}
}

func TestAgentPersistsClarificationAndRecoversAnswerIntoOriginalToolCall(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-clarify", Name: agentAskUserToolName, Arguments: `{"question":"Which environment should I use?"}`}}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "I will use staging.", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}},
	)
	first, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || first.Status != task.StatusWaitingUser || first.WaitingReason != "clarification_required" {
		t.Fatalf("first Run() = %+v, %v; want clarification wait", first, err)
	}
	if fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 || len(fixture.store.details.ClarificationRequests) != 1 {
		t.Fatalf("clarification crossed runtime tool boundary or was not persisted: prepare=%d execute=%d requests=%#v", fixture.tools.prepareCalls, fixture.tools.executeCalls, fixture.store.details.ClarificationRequests)
	}
	request := fixture.store.details.ClarificationRequests[0]
	if request.ID == "" || request.Version != 1 || request.ModelStepID != fixture.store.details.Steps[0].ID || request.CallID != "call-clarify" ||
		request.Question != "Which environment should I use?" || request.Status != task.ClarificationPending {
		t.Fatalf("persisted clarification = %#v", request)
	}
	if len(fixture.client.requests) != 1 || !hasModelTool(fixture.client.requests[0].Tools, agentAskUserToolName) {
		t.Fatalf("ask_user was not registered for the model: %#v", fixture.client.requests)
	}

	fixture.store.mu.Lock()
	fixture.store.details.ClarificationRequests[0].Status = task.ClarificationAnswered
	fixture.store.details.ClarificationRequests[0].Answer = "Use staging."
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.details.Task.WaitingReason = ""
	fixture.store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: fixture.claimed.Lease.ExpiresAt}
	claimed := fixture.store.details.Task
	fixture.store.mu.Unlock()
	if _, err := fixture.agent.Run(context.Background(), claimed); err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	if fixture.store.details.Task.Status != task.StatusCompleted || fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 || fixture.tools.verifyCalls != 1 {
		t.Fatalf("resumed clarification task state=%q prepare=%d execute=%d verify=%d", fixture.store.details.Task.Status, fixture.tools.prepareCalls, fixture.tools.executeCalls, fixture.tools.verifyCalls)
	}
	var sawOriginalCall, sawAnswer bool
	for _, input := range fixture.client.requests[1].Input {
		sawOriginalCall = sawOriginalCall || input.Type == "function_call" && input.CallID == "call-clarify" && input.Name == agentAskUserToolName
		sawAnswer = sawAnswer || input.Type == "function_call_output" && input.CallID == "call-clarify" && input.Output == "Use staging."
	}
	if !sawOriginalCall || !sawAnswer {
		t.Fatalf("resume model input did not contain the exact clarification call and answer: %#v", fixture.client.requests[1].Input)
	}
}

func TestAgentNeverRunsOtherToolsAlongsideClarification(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{
			{CallID: "call-clarify-mixed", Name: agentAskUserToolName, Arguments: `{"question":"Which environment?"}`},
			{CallID: "call-inspect-mixed", Name: "inspect", Arguments: `{}`},
		}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	_, err := fixture.agent.Run(context.Background(), fixture.claimed)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorModelOutput || fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 {
		t.Fatalf("mixed clarification request result err=%v prepare=%d execute=%d", err, fixture.tools.prepareCalls, fixture.tools.executeCalls)
	}
}

func TestAgentRejectsClarificationQuestionWithNUL(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-clarify-nul", Name: agentAskUserToolName, Arguments: `{"question":"Which\u0000environment?"}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	_, err := fixture.agent.Run(context.Background(), fixture.claimed)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorModelOutput || fixture.store.details.Task.Status != task.StatusFailed {
		t.Fatalf("NUL clarification question result err=%v task=%q", err, fixture.store.details.Task.Status)
	}
	if len(fixture.store.details.ClarificationRequests) != 0 || fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 {
		t.Fatalf("invalid question was persisted/dispatched: clarifications=%d prepare=%d execute=%d", len(fixture.store.details.ClarificationRequests), fixture.tools.prepareCalls, fixture.tools.executeCalls)
	}
}

func hasModelTool(tools []ModelTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func TestAgentDoesNotOfferApprovalForStaleHostFence(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-stale-approval", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.preparation = AgentToolPreparation{
		Status:   AgentToolApprovalRequired,
		Fence:    AgentHostFence{RuntimeGeneration: 4, ControlVersion: 8, AIAllowed: true},
		Approval: AgentApprovalRequest{Object: "project", Content: "Share report?", Impact: "External upload."},
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.WaitingReason != "host_fence_changed" || fixture.tools.executeCalls != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("stale approval was not rejected: result=%+v execute=%d operations=%d", result, fixture.tools.executeCalls, len(fixture.store.operations))
	}
}

func TestAgentApprovalDecisionIsBoundToPersistedCallAndFence(t *testing.T) {
	call := ModelToolCall{CallID: "call-approved", Name: "inspect", Arguments: `{}`}
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{call}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "The inspection is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.details.PendingRequest = &task.PendingRequest{
		ID: "r1234567", TaskID: fixture.claimed.ID, Version: 2, Kind: "approval", Title: "Review inspection",
		Object: "Workspace project", Content: "Inspect project files?", Impact: "Reads the authorized workspace.",
		CallID: call.CallID, ToolName: call.Name, RequestHash: append([]byte(nil), hash[:]...),
		RuntimeGeneration: 4, RuntimeInstanceID: "ri_test", ControlVersion: 9,
		Status: task.PendingRequestApproved, Decision: task.PendingDecisionApprove,
	}
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("Run() = %+v, %v; want completed after bound approval", result, err)
	}
	if fixture.tools.lastRequest.Approval == nil || fixture.tools.lastRequest.Approval.PendingID != "r1234567" ||
		fixture.tools.lastRequest.Approval.Version != 2 || fixture.tools.lastRequest.Approval.Decision != task.PendingDecisionApprove ||
		fixture.tools.executeCalls != 1 {
		t.Fatalf("bound approval did not reach the exact tool request: request=%+v executions=%d", fixture.tools.lastRequest, fixture.tools.executeCalls)
	}
}

func TestAgentRejectedApprovalPersistsNoDispatchAndReturnsResultToModel(t *testing.T) {
	call := ModelToolCall{CallID: "call-rejected", Name: "inspect", Arguments: `{}`}
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{call}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "I will leave the workspace unchanged.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.details.PendingRequest = &task.PendingRequest{
		ID: "r1234567", TaskID: fixture.claimed.ID, Version: 2, Kind: "approval", Title: "Review inspection",
		Object: "Workspace project", Content: "Inspect project files?", Impact: "Reads the authorized workspace.",
		CallID: call.CallID, ToolName: call.Name, RequestHash: append([]byte(nil), hash[:]...),
		RuntimeGeneration: 4, RuntimeInstanceID: "ri_test", ControlVersion: 9,
		Status: task.PendingRequestRejected, Decision: task.PendingDecisionReject,
	}
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("Run() = %+v, %v; want completed after handling rejection", result, err)
	}
	if fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 {
		t.Fatalf("rejected tool crossed the runtime boundary: prepare=%d execute=%d", fixture.tools.prepareCalls, fixture.tools.executeCalls)
	}
	key := agentOperationKey(call.CallID)
	operation := fixture.store.operations[key]
	if operation.Status != task.OperationFailed || operation.Result != "The user rejected this exact action. It was not executed." {
		t.Fatalf("rejection was not persisted as a terminal no-dispatch result: %+v", operation)
	}
}

func TestAgentUnknownToolResultIsNotReplayed(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-unknown-result", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.result = AgentToolResult{Status: AgentToolUnknown}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != task.StatusWaitingUser || result.WaitingReason != "tool_result_unknown" {
		t.Fatalf("result = %+v, want unknown-result pause", result)
	}
	operation := fixture.store.operations[agentOperationKey("call-unknown-result")]
	if operation.Status != task.OperationUnknown || fixture.tools.executeCalls != 1 {
		t.Fatalf("operation = %q, execute count = %d", operation.Status, fixture.tools.executeCalls)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.details.Task.Lease = claimed.Lease
	operation.LeaseVersion = claimed.Lease.Version
	fixture.store.operations[agentOperationKey("call-unknown-result")] = operation
	fixture.store.mu.Unlock()

	recovered, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || recovered.Status != task.StatusWaitingUser || recovered.WaitingReason != "runtime_operation_unknown" {
		t.Fatalf("recovered Run() = %+v, %v; want fail-closed recovery pause", recovered, err)
	}
	if fixture.tools.reconcileCalls != 1 || fixture.tools.executeCalls != 1 || len(fixture.client.requests) != 1 {
		t.Fatalf("unknown operation was replayed during recovery: reconcile=%d execute=%d model=%d", fixture.tools.reconcileCalls, fixture.tools.executeCalls, len(fixture.client.requests))
	}
	if stored := fixture.store.operations[agentOperationKey("call-unknown-result")]; stored.Status != task.OperationUnknown {
		t.Fatalf("unknown operation status = %q, want unknown", stored.Status)
	}
}

func TestAgentWaitsForUserWhenDesktopTargetLosesFocus(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-focus-change", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.result = AgentToolResult{Status: AgentToolNeedsUser, ExternalID: "runtime-focus-change"}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusWaitingUser || result.WaitingReason != "desktop_focus_changed" {
		t.Fatalf("Run() = %+v, %v; want a user pause for focus change", result, err)
	}
	operation := fixture.store.operations[agentOperationKey("call-focus-change")]
	if operation.Status != task.OperationFailed || operation.Result != `{"error":"desktop_focus_changed"}` || fixture.tools.executeCalls != 1 || len(fixture.client.requests) != 1 {
		t.Fatalf("focus change did not pause without replay: operation=%+v execute=%d model=%d", operation, fixture.tools.executeCalls, len(fixture.client.requests))
	}
	foundStep := false
	for _, step := range fixture.store.details.Steps {
		if step.Status == task.StepWaitingUser && strings.Contains(step.Summary, "没有发送输入") {
			foundStep = true
		}
	}
	if !foundStep {
		t.Fatalf("task steps do not explain the focus pause: %+v", fixture.store.details.Steps)
	}
}

func TestAgentPausesAndRequiresResubmissionWhenWorkspaceInputChanges(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-workspace-change", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.prepareError = domain.NewError(domain.CodeWorkspaceConflict, "stale Workspace revision")

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusWaitingUser || result.WaitingReason != "workspace_input_changed" ||
		fixture.store.details.Task.Status != task.StatusWaitingUser || len(fixture.store.operations) != 0 || fixture.tools.executeCalls != 0 {
		t.Fatalf("Run() = %+v, %v; task=%#v operations=%d executes=%d; want a resubmission pause before dispatch",
			result, err, fixture.store.details.Task, len(fixture.store.operations), fixture.tools.executeCalls)
	}
}

func TestAgentPausesOnWorkspaceConflictReportedBeforeRuntimeDispatch(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-workspace-dispatch-change", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	fixture.tools.result = AgentToolResult{
		Status: AgentToolNeedsUser, WaitingReason: "workspace_input_changed",
		Output: "Workspace changed after this task was submitted.",
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	operation := fixture.store.operations[agentOperationKey("call-workspace-dispatch-change")]
	if err != nil || result.Status != task.StatusWaitingUser || result.WaitingReason != "workspace_input_changed" ||
		operation.Status != task.OperationFailed || operation.Result != `{"error":"workspace_input_changed"}` ||
		fixture.tools.executeCalls != 1 || len(fixture.client.requests) != 1 {
		t.Fatalf("Run() = %+v, %v; task=%#v operation=%#v executes=%d models=%d; want a durable resubmission pause",
			result, err, fixture.store.details.Task, operation, fixture.tools.executeCalls, len(fixture.client.requests))
	}
}

func TestAgentRearmsSamePersistedOperationAfterExactRuntimeNotFound(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-reconcile", Name: "inspect", Arguments: `{}`}}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "The requested state is confirmed.", Usage: ModelUsage{Known: true, InputTokens: 7, OutputTokens: 2}},
	)
	fixture.tools.result = AgentToolResult{Status: AgentToolUnknown}
	if _, err := fixture.agent.Run(context.Background(), fixture.claimed); err != nil {
		t.Fatalf("initial Run() error = %v", err)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.details.Task.Lease = claimed.Lease
	operation := fixture.store.operations[agentOperationKey("call-reconcile")]
	operation.LeaseVersion = claimed.Lease.Version
	fixture.store.operations[operation.Key] = operation
	fixture.store.mu.Unlock()
	fixture.tools.result = AgentToolResult{Status: AgentToolSucceeded, ExternalID: "runtime-operation", Output: "operation committed"}
	fixture.tools.reconcileResult = AgentToolResult{Status: AgentToolNotFound, ExternalID: operation.ID}

	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("recovered Run() = %+v, %v; want completed task", result, err)
	}
	if fixture.tools.reconcileCalls != 1 || fixture.tools.executeCalls != 2 || len(fixture.client.requests) != 2 {
		t.Fatalf("recovery calls: reconcile=%d execute=%d model=%d; want exact query, same-ID dispatch, and one new model turn", fixture.tools.reconcileCalls, fixture.tools.executeCalls, len(fixture.client.requests))
	}
	if stored := fixture.store.operations[operation.Key]; stored.Status != task.OperationSucceeded || stored.Result != "operation committed" {
		t.Fatalf("reconciled operation = %+v, want durable success", stored)
	}
	if fixture.tools.lastRequest.OperationID != operation.ID {
		t.Fatalf("recovered operation ID = %q, want original persisted ID %q", fixture.tools.lastRequest.OperationID, operation.ID)
	}
}

func TestAgentRecoveryResumesPersistedVerificationClaimWithoutNewModelCall(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		Text:  "The requested state is confirmed.",
		Usage: ModelUsage{Known: true, InputTokens: 7, OutputTokens: 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	fixture.tools.onVerify = cancel

	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initial Run() error = %v, want context.Canceled", err)
	}
	if len(fixture.client.requests) != 1 || fixture.tools.verifyCalls != 1 || fixture.store.details.Task.Status != task.StatusRunning {
		t.Fatalf("interrupted verification state: model=%d verify=%d task=%q", len(fixture.client.requests), fixture.tools.verifyCalls, fixture.store.details.Task.Status)
	}
	firstVerifyStepID := fixture.tools.lastVerifyReq.StepID

	fixture.tools.onVerify = nil
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("recovered Run() = %+v, %v; want completed task", result, err)
	}
	if len(fixture.client.requests) != 1 || fixture.tools.verifyCalls != 2 {
		t.Fatalf("verification recovery called model=%d times and verifier=%d times; want 1 and 2", len(fixture.client.requests), fixture.tools.verifyCalls)
	}
	if fixture.tools.lastVerifyReq.StepID != firstVerifyStepID {
		t.Fatalf("verification recovery step ID = %q; want stable original step ID %q", fixture.tools.lastVerifyReq.StepID, firstVerifyStepID)
	}
}

func TestAgentRecoveryAdoptsPreparedOperationAfterLeaseReclaim(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-prepared", Name: "inspect", Arguments: `{}`}}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "The prepared operation completed.", Usage: ModelUsage{Known: true, InputTokens: 7, OutputTokens: 2}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.store.onCreateOperation = cancel
	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initial Run() error = %v, want context.Canceled", err)
	}
	operation := fixture.store.operations[agentOperationKey("call-prepared")]
	if operation.Status != task.OperationPrepared || operation.LeaseVersion != fixture.claimed.Lease.Version {
		t.Fatalf("interrupted operation = %+v; want prepared under the original lease", operation)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = claimed.Lease
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.mu.Unlock()
	fixture.store.onCreateOperation = nil
	fixture.tools.result = AgentToolResult{Status: AgentToolSucceeded, Output: "prepared operation output"}

	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("recovered Run() = %+v, %v; want completed task", result, err)
	}
	recovered := fixture.store.operations[operation.Key]
	if recovered.ID != operation.ID || recovered.Status != task.OperationSucceeded || recovered.LeaseVersion != claimed.Lease.Version {
		t.Fatalf("recovered operation = %+v; want same ID dispatched under the new lease", recovered)
	}
	if fixture.tools.executeCalls != 1 || fixture.tools.lastRequest.OperationID != operation.ID {
		t.Fatalf("prepared recovery execute=%d operationID=%q; want one dispatch of %q", fixture.tools.executeCalls, fixture.tools.lastRequest.OperationID, operation.ID)
	}
}

func TestAgentProcessesBatchItemsIndependently(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "Item one is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "Item two is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{
		{ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect item one", Status: task.BatchItemQueued, Attempt: 1},
		{ID: "b1234568", TaskID: fixture.claimed.ID, Key: "item-2", Input: "inspect item two", Status: task.BatchItemQueued, Attempt: 1},
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("Run() = %+v, %v; want completed batch", result, err)
	}
	if fixture.store.details.BatchItems[0].Status != task.BatchItemSucceeded || fixture.store.details.BatchItems[0].Result != "Item one is complete." ||
		fixture.store.details.BatchItems[1].Status != task.BatchItemSucceeded || fixture.store.details.BatchItems[1].Result != "Item two is complete." {
		t.Fatalf("batch item results = %#v", fixture.store.details.BatchItems)
	}
	if len(fixture.client.requests) != 2 || fixture.tools.verifyCalls != 2 {
		t.Fatalf("model requests=%d verifier calls=%d, want two each", len(fixture.client.requests), fixture.tools.verifyCalls)
	}
	if !strings.Contains(fixture.client.requests[0].Input[0].Text, "inspect item one") ||
		!strings.Contains(fixture.client.requests[1].Input[0].Text, "inspect item two") {
		t.Fatalf("batch item inputs were not isolated: %#v", fixture.client.requests)
	}
	if fixture.store.details.Task.Status != task.StatusCompleted {
		t.Fatalf("task status = %q, want completed after all item checkpoints", fixture.store.details.Task.Status)
	}
	fingerprints := make(map[string]string)
	for _, event := range fixture.store.events {
		if event.Type != "task.step" {
			continue
		}
		var payload struct {
			Detail struct {
				BatchItemID      string `json:"batch_item_id"`
				InputFingerprint string `json:"input_fingerprint"`
			} `json:"detail"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Detail.BatchItemID != "" {
			fingerprints[payload.Detail.BatchItemID] = payload.Detail.InputFingerprint
		}
	}
	for _, item := range fixture.store.details.BatchItems {
		if fingerprints[item.ID] != agentBatchFingerprint(fixture.store.details.Task, item) {
			t.Fatalf("batch item %q lacks its immutable input fingerprint", item.Key)
		}
	}
}

func TestAgentBatchRecoverySkipsPersistedSuccessfulItem(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "Item one is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "Item two is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{
		{ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect item one", Status: task.BatchItemQueued, Attempt: 1},
		{ID: "b1234568", TaskID: fixture.claimed.ID, Key: "item-2", Input: "inspect item two", Status: task.BatchItemQueued, Attempt: 1},
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixture.store.onBatchItemUpdated = func(item task.BatchItem) {
		if item.ID == "b1234567" && item.Status == task.BatchItemSucceeded {
			cancel()
		}
	}

	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initial Run() error = %v, want context.Canceled", err)
	}
	if fixture.store.details.BatchItems[0].Status != task.BatchItemSucceeded || fixture.store.details.BatchItems[1].Status != task.BatchItemQueued {
		t.Fatalf("interrupted batch item states = %#v", fixture.store.details.BatchItems)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = claimed.Lease
	fixture.store.mu.Unlock()
	fixture.store.onBatchItemUpdated = nil
	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("recovered Run() = %+v, %v; want completed batch", result, err)
	}
	if len(fixture.client.requests) != 2 || fixture.tools.verifyCalls != 2 {
		t.Fatalf("recovery repeated or skipped work: model=%d verify=%d; want two each", len(fixture.client.requests), fixture.tools.verifyCalls)
	}
}

func TestAgentBatchCheckpointAndOperationKeyAreAttemptScoped(t *testing.T) {
	fixture := newAgentFixture(t)
	item := task.BatchItem{ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect", Status: task.BatchItemQueued, Attempt: 2}
	fingerprint := agentBatchFingerprint(fixture.claimed, item)
	payload, err := json.Marshal(map[string]any{"detail": map[string]any{
		"batch_item_id": item.ID, "input_fingerprint": fingerprint,
		"batch_attempt": 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.events = []task.Event{{TaskID: fixture.claimed.ID, Sequence: 1, Type: "task.step", Payload: payload, CreatedAt: time.Now().UTC()}}
	found, valid, err := fixture.agent.hasBatchFingerprint(context.Background(), fixture.claimed, agentBatchScope{Item: item, Fingerprint: fingerprint})
	if err != nil || found || !valid {
		t.Fatalf("old attempt checkpoint = found:%v valid:%v err:%v; want ignored for attempt 2", found, valid, err)
	}
	first := agentOperationKeyForBatch("call-1", &agentBatchScope{Item: task.BatchItem{ID: item.ID, Attempt: 1}})
	firstAgain := agentOperationKeyForBatch("call-1", &agentBatchScope{Item: task.BatchItem{ID: item.ID, Attempt: 1}})
	second := agentOperationKeyForBatch("call-1", &agentBatchScope{Item: task.BatchItem{ID: item.ID, Attempt: 2}})
	if first != firstAgain || first == second {
		t.Fatalf("batch operation keys do not isolate attempts: first=%s repeated=%s second=%s", first, firstAgain, second)
	}
}

func TestAgentStoresOnlyIndependentlyVerifiedBatchArtifacts(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "The report is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}})
	fixture.store.details.BatchItems = []task.BatchItem{{
		ID: "b1234567", TaskID: fixture.claimed.ID, Key: "report", Input: "create report", Status: task.BatchItemQueued, Attempt: 2,
	}}
	setAgentFixtureWorkspace(fixture)
	fixture.tools.verification = AgentVerificationResult{
		Status: AgentVerified, Summary: "The report exists in the workspace.",
		Artifacts: []AgentVerifiedArtifact{verifiedReportArtifact()},
	}
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted || len(fixture.store.details.Artifacts) != 1 {
		t.Fatalf("Run() = %+v, %v; artifacts=%#v", result, err, fixture.store.details.Artifacts)
	}
	artifact := fixture.store.details.Artifacts[0]
	if artifact.TaskID != fixture.claimed.ID || artifact.BatchItemID != "b1234567" || artifact.BatchAttempt != 2 ||
		artifact.Path != "reports/summary.txt" || artifact.Label != "Report" || !domain.ValidStepID(artifact.StepID) {
		t.Fatalf("artifact association = %#v", artifact)
	}
}

func TestAgentCommitsVerifiedArtifactWithTaskCompletion(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "The report is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}})
	setAgentFixtureWorkspace(fixture)
	fixture.tools.verification = AgentVerificationResult{
		Status: AgentVerified, Summary: "The report exists in the workspace.",
		Artifacts: []AgentVerifiedArtifact{verifiedReportArtifact()},
	}
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted || len(fixture.store.details.Artifacts) != 1 ||
		len(fixture.store.lastProgress.Artifacts) != 1 || fixture.store.recordValidatedArtifactCalls != 0 {
		t.Fatalf("Run() = %+v, %v; stored=%#v progress=%#v separate writes=%d", result, err,
			fixture.store.details.Artifacts, fixture.store.lastProgress.Artifacts, fixture.store.recordValidatedArtifactCalls)
	}
	artifact := fixture.store.lastProgress.Artifacts[0]
	if artifact.WorkspaceID != "w1234567" || artifact.Revision != 4 || artifact.SizeBytes != 5 ||
		artifact.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("completion artifact proof = %#v", artifact)
	}
}

func TestAgentRejectsUnsafeOrUnverifiedArtifactMetadata(t *testing.T) {
	for _, result := range []AgentVerificationResult{
		{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{{Label: "report", Path: "../secret.txt", MediaType: "text/plain"}}},
		{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{{Label: "report", Path: "reports\\secret.txt", MediaType: "text/plain"}}},
		{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{{Label: "report", Path: "reports/file.txt", MediaType: "not-a-media-type"}}},
		{Status: AgentNotVerified, Artifacts: []AgentVerifiedArtifact{{Label: "report", Path: "reports/file.txt", MediaType: "text/plain"}}},
	} {
		if err := validateAgentVerificationArtifacts(result); err == nil {
			t.Errorf("accepted unsafe verification result: %#v", result)
		}
	}
}

func TestAgentRejectsArtifactsFromUnverifiedResult(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{Text: "The report is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}})
	fixture.store.details.BatchItems = []task.BatchItem{{
		ID: "b1234567", TaskID: fixture.claimed.ID, Key: "report", Input: "create report", Status: task.BatchItemQueued, Attempt: 1,
	}}
	fixture.tools.verification = AgentVerificationResult{
		Status: AgentNotVerified, Summary: "The report could not be confirmed.",
		Artifacts: []AgentVerifiedArtifact{{Label: "Report", Path: "reports/summary.txt", MediaType: "text/plain"}},
	}
	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err == nil || result.Status != task.StatusFailed || len(fixture.store.details.Artifacts) != 0 ||
		fixture.store.details.BatchItems[0].Status != task.BatchItemFailed {
		t.Fatalf("unverified artifact was accepted: result=%+v err=%v artifacts=%#v item=%#v", result, err, fixture.store.details.Artifacts, fixture.store.details.BatchItems[0])
	}
}

func TestAgentArtifactWriteIsIdempotentAcrossVerificationRecovery(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "The report is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{{
		ID: "b1234567", TaskID: fixture.claimed.ID, Key: "report", Input: "create report", Status: task.BatchItemQueued, Attempt: 1,
	}}
	setAgentFixtureWorkspace(fixture)
	fixture.tools.verification = AgentVerificationResult{
		Status: AgentVerified, Summary: "The report exists in the workspace.",
		Artifacts: []AgentVerifiedArtifact{verifiedReportArtifact()},
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixture.store.onArtifactRecorded = func(task.Artifact) { cancel() }
	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) || len(fixture.store.details.Artifacts) != 1 ||
		fixture.store.details.BatchItems[0].Status != task.BatchItemRunning {
		t.Fatalf("interrupted run err=%v artifacts=%d item=%#v", err, len(fixture.store.details.Artifacts), fixture.store.details.BatchItems[0])
	}
	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = claimed.Lease
	fixture.store.mu.Unlock()
	fixture.store.onArtifactRecorded = nil
	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusCompleted || len(fixture.store.details.Artifacts) != 1 ||
		fixture.store.details.BatchItems[0].Status != task.BatchItemSucceeded {
		t.Fatalf("recovered run=%+v err=%v artifacts=%#v item=%#v", result, err, fixture.store.details.Artifacts, fixture.store.details.BatchItems[0])
	}
}

func TestAgentBatchInputFingerprintChangeFailsClosed(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "Item is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{{
		ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "original input", Status: task.BatchItemQueued, Attempt: 1,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	fixture.store.onBatchItemUpdated = func(item task.BatchItem) {
		if item.Status == task.BatchItemRunning {
			cancel()
		}
	}
	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initial Run() error = %v, want context.Canceled", err)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = claimed.Lease
	fixture.store.details.BatchItems[0].Input = "changed input"
	fixture.store.mu.Unlock()
	fixture.store.onBatchItemUpdated = nil
	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusWaitingUser || result.WaitingReason != "batch_input_changed" {
		t.Fatalf("Run() after input change = %+v, %v; want fail-closed pause", result, err)
	}
	if len(fixture.client.requests) != 0 || fixture.store.details.BatchItems[0].Status != task.BatchItemRunning {
		t.Fatalf("changed batch input was executed: model=%d item=%#v", len(fixture.client.requests), fixture.store.details.BatchItems[0])
	}
}

func TestAgentBatchKeepsPartialSuccessAndFailsOverallTask(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "Item one is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{StopReason: "refusal", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{
		{ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect item one", Status: task.BatchItemQueued, Attempt: 1},
		{ID: "b1234568", TaskID: fixture.claimed.ID, Key: "item-2", Input: "inspect item two", Status: task.BatchItemQueued, Attempt: 1},
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err == nil || result.Status != task.StatusFailed {
		t.Fatalf("Run() = %+v, %v; want failed overall batch", result, err)
	}
	if fixture.store.details.BatchItems[0].Status != task.BatchItemSucceeded || fixture.store.details.BatchItems[0].Result != "Item one is complete." ||
		fixture.store.details.BatchItems[1].Status != task.BatchItemFailed {
		t.Fatalf("partial batch result was lost or misreported: %#v", fixture.store.details.BatchItems)
	}
	if fixture.store.details.Task.Status == task.StatusCompleted {
		t.Fatal("batch with a failed item was marked completed")
	}
}

func TestAgentBatchPausesBeforeNextItemWhenModelUsageIsUnknown(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{Text: "Item one is complete.", Usage: ModelUsage{}},
		ModelResponse{Text: "Item two is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{
		{ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect item one", Status: task.BatchItemQueued, Attempt: 1},
		{ID: "b1234568", TaskID: fixture.claimed.ID, Key: "item-2", Input: "inspect item two", Status: task.BatchItemQueued, Attempt: 1},
	}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusWaitingUser || result.WaitingReason != "batch_model_usage_unknown" {
		t.Fatalf("Run() = %+v, %v; want pause before the second item", result, err)
	}
	if fixture.store.details.BatchItems[0].Status != task.BatchItemSucceeded || fixture.store.details.BatchItems[1].Status != task.BatchItemQueued ||
		len(fixture.client.requests) != 1 || fixture.store.details.Task.Status == task.StatusCompleted {
		t.Fatalf("unknown model usage advanced the batch: items=%#v model=%d task=%q", fixture.store.details.BatchItems, len(fixture.client.requests), fixture.store.details.Task.Status)
	}
}

func TestAgentBatchRecoveryReconcilesUnknownOperationBeforeResume(t *testing.T) {
	fixture := newAgentFixture(t,
		ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-batch", Name: "inspect", Arguments: `{}`}}, Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
		ModelResponse{Text: "The batch item is complete.", Usage: ModelUsage{Known: true, InputTokens: 7, OutputTokens: 2}},
	)
	fixture.store.details.BatchItems = []task.BatchItem{{
		ID: "b1234567", TaskID: fixture.claimed.ID, Key: "item-1", Input: "inspect item one", Status: task.BatchItemQueued, Attempt: 1,
	}}
	fixture.tools.result = AgentToolResult{Status: AgentToolUnknown}
	first, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || first.Status != task.StatusWaitingUser || first.WaitingReason != "tool_result_unknown" {
		t.Fatalf("initial batch Run() = %+v, %v; want pause for unknown runtime result", first, err)
	}
	operationKey := agentOperationKeyForBatch("call-batch", &agentBatchScope{Item: fixture.store.details.BatchItems[0]})
	operation := fixture.store.operations[operationKey]
	if operation.Status != task.OperationUnknown {
		t.Fatalf("batch operation = %+v, want unknown", operation)
	}

	claimed := fixture.claimed
	claimed.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.details.Task.Lease = claimed.Lease
	operation.LeaseVersion = claimed.Lease.Version
	fixture.store.operations[operationKey] = operation
	fixture.store.mu.Unlock()
	unknown, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || unknown.Status != task.StatusWaitingUser || unknown.WaitingReason != "runtime_operation_unknown" {
		t.Fatalf("batch recovery without an exact runtime answer = %+v, %v; want fail-closed pause", unknown, err)
	}
	if fixture.tools.reconcileCalls != 1 || fixture.tools.executeCalls != 1 || fixture.store.operations[operationKey].Status != task.OperationUnknown {
		t.Fatalf("uncertain batch operation was replayed: reconcile=%d execute=%d op=%q", fixture.tools.reconcileCalls, fixture.tools.executeCalls, fixture.store.operations[operationKey].Status)
	}

	claimed.Lease = task.Lease{Owner: "worker-3", Version: 3, ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	fixture.store.mu.Lock()
	fixture.store.details.Task.Status = task.StatusRunning
	fixture.store.details.Task.Lease = claimed.Lease
	operation.LeaseVersion = claimed.Lease.Version
	fixture.store.operations[operationKey] = operation
	fixture.store.mu.Unlock()
	fixture.tools.reconcileResult = AgentToolResult{Status: AgentToolNotFound, ExternalID: operation.ID}
	fixture.tools.result = AgentToolResult{Status: AgentToolSucceeded, Output: "operation recovered"}

	result, err := fixture.agent.Run(context.Background(), claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("recovered batch Run() = %+v, %v; want completed task", result, err)
	}
	stored := fixture.store.operations[operationKey]
	if fixture.tools.reconcileCalls != 2 || fixture.tools.executeCalls != 2 || stored.ID != operation.ID || stored.Status != task.OperationSucceeded {
		t.Fatalf("batch recovery reconcile=%d execute=%d operation=%+v", fixture.tools.reconcileCalls, fixture.tools.executeCalls, stored)
	}
}

func TestAgentRejectsUnregisteredToolAndDisabledHostBeforeModelCall(t *testing.T) {
	t.Run("unregistered tool", func(t *testing.T) {
		fixture := newAgentFixture(t, ModelResponse{
			ToolCalls: []ModelToolCall{{CallID: "call-unregistered", Name: "shell", Arguments: `{}`}},
			Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
		})
		_, err := fixture.agent.Run(context.Background(), fixture.claimed)
		var agentErr *AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorModelOutput {
			t.Fatalf("Run() error = %v, want invalid_model_output", err)
		}
		if fixture.tools.prepareCalls != 0 || fixture.tools.executeCalls != 0 {
			t.Fatalf("unregistered tool reached executor: prepare=%d execute=%d", fixture.tools.prepareCalls, fixture.tools.executeCalls)
		}
	})

	t.Run("AI control disabled", func(t *testing.T) {
		fixture := newAgentFixture(t)
		fixture.fences.fence.AIAllowed = false
		_, err := fixture.agent.Run(context.Background(), fixture.claimed)
		var agentErr *AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != AgentErrorHostFence {
			t.Fatalf("Run() error = %v, want host_fence_changed", err)
		}
		if len(fixture.client.requests) != 0 {
			t.Fatalf("model was called with disabled Host: %d requests", len(fixture.client.requests))
		}
	})
}

func TestAgentCancellationAfterDispatchPersistsUnknownWithoutReplay(t *testing.T) {
	fixture := newAgentFixture(t, ModelResponse{
		ToolCalls: []ModelToolCall{{CallID: "call-cancel", Name: "inspect", Arguments: `{}`}},
		Usage:     ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	fixture.tools.onExecute = cancel
	fixture.tools.result = AgentToolResult{Status: AgentToolUnknown}

	_, err := fixture.agent.Run(ctx, fixture.claimed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	operation := fixture.store.operations[agentOperationKey("call-cancel")]
	if operation.Status != task.OperationUnknown || fixture.store.details.Task.Status != task.StatusRunning {
		t.Fatalf("cancelled execution was not durably marked unknown: operation=%q task=%q", operation.Status, fixture.store.details.Task.Status)
	}
	if _, err := fixture.agent.Run(context.Background(), fixture.claimed); err != nil {
		t.Fatalf("Run() after cancellation error = %v", err)
	}
	if fixture.tools.executeCalls != 1 || fixture.store.details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("cancelled operation replayed or not held: execute=%d status=%q", fixture.tools.executeCalls, fixture.store.details.Task.Status)
	}
}

func TestValidateAgentToolArgumentsRejectsTrailingAndNestedDuplicateJSON(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{"x":{"a":1,"a":2}}`, `[]`} {
		if _, err := validateAgentToolArguments(raw); err == nil {
			t.Errorf("validateAgentToolArguments(%q) succeeded", raw)
		}
	}
}

func TestActiveBudgetContextDoesNotMisclassifyEarlierParentDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	ctx, cancel, isBudgetDeadline := activeBudgetContext(parent, 0, time.Now(), 30)
	defer cancel()
	if isBudgetDeadline {
		t.Fatal("earlier parent deadline was classified as the task active-time budget")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("parent deadline was lost")
	}

	ctx, cancel, isBudgetDeadline = activeBudgetContext(context.Background(), 0, time.Now(), 30)
	defer cancel()
	if !isBudgetDeadline {
		t.Fatal("task active-time deadline was not classified as a budget deadline")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("task budget deadline was not installed")
	}
}

type agentFixture struct {
	agent   *Agent
	claimed task.Task
	store   *agentTaskStoreFake
	client  *agentModelClientFake
	tools   *agentToolExecutorFake
	fences  *agentFenceReaderFake
	auth    *agentAuthorizerFake
}

func newAgentFixture(t *testing.T, responses ...ModelResponse) *agentFixture {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	claimed := task.Task{
		ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", ModelID: "p1234567",
		Objective: "Inspect the requested workspace state.", Status: task.StatusRunning,
		Authorization: task.Authorization{Kind: string(identity.KindSession)},
		Budget:        task.Budget{ActiveSeconds: 30, ToolCalls: 8, ModelTokens: 10000},
		Lease:         task.Lease{Owner: "worker-1", Version: 1, ExpiresAt: &expires},
	}
	store := &agentTaskStoreFake{details: TaskDetails{Task: claimed}, operations: make(map[string]task.Operation)}
	client := &agentModelClientFake{responses: append([]ModelResponse(nil), responses...)}
	tools := &agentToolExecutorFake{
		definitions: []AgentToolDefinition{{
			ModelTool:     ModelTool{Name: "inspect", Description: "Read current runtime state.", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			RequiredScope: identity.ScopeComputerObserve,
		}},
		preparation:  AgentToolPreparation{Status: AgentToolReady, Fence: AgentHostFence{RuntimeGeneration: 4, RuntimeInstanceID: "ri_test", ControlVersion: 9, AIAllowed: true}, Token: "prepared-token"},
		result:       AgentToolResult{Status: AgentToolSucceeded, Output: "tool result"},
		verification: AgentVerificationResult{Status: AgentVerified, Summary: "Verified from runtime state."},
	}
	fences := &agentFenceReaderFake{fence: AgentHostFence{RuntimeGeneration: 4, RuntimeInstanceID: "ri_test", ControlVersion: 9, AIAllowed: true}}
	auth := &agentAuthorizerFake{}
	agent, err := NewAgent(store, &agentModelResolverFake{}, client, auth, fences, tools)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return &agentFixture{agent: agent, claimed: claimed, store: store, client: client, tools: tools, fences: fences, auth: auth}
}

func setAgentFixtureWorkspace(fixture *agentFixture) {
	fixture.claimed.WorkspaceID = "w1234567"
	fixture.store.details.Task.WorkspaceID = "w1234567"
}

func verifiedReportArtifact() AgentVerifiedArtifact {
	return AgentVerifiedArtifact{
		Label: "Report", Path: "reports/summary.txt", MediaType: "text/plain",
		WorkspaceID: "w1234567", Revision: 4, SizeBytes: 5,
		SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	}
}

type agentModelResolverFake struct{}

func (*agentModelResolverFake) ResolveEndpoint(context.Context, string, string) (ModelEndpoint, error) {
	return ModelEndpoint{BaseURL: "https://model.example/v1", APIKey: "model-secret", ModelID: "test-model"}, nil
}

type agentModelClientFake struct {
	responses []ModelResponse
	requests  []ModelRequest
	deltas    [][]string
}

func (f *agentModelClientFake) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return ModelResponse{}, err
	}
	f.requests = append(f.requests, request)
	if len(f.responses) == 0 {
		return ModelResponse{}, errors.New("no scripted model response")
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	if len(f.deltas) > 0 {
		deltas := f.deltas[0]
		f.deltas = f.deltas[1:]
		for _, delta := range deltas {
			if request.OnTextDelta == nil {
				return ModelResponse{}, errors.New("stream callback is missing")
			}
			if err := request.OnTextDelta(delta); err != nil {
				return ModelResponse{}, err
			}
		}
	}
	return response, nil
}

func (*agentModelClientFake) Check(context.Context, ModelEndpoint) (ModelCapabilities, error) {
	return ModelCapabilities{}, errors.New("not used")
}

type agentAuthorizerFake struct {
	err   error
	calls []identity.Scope
}

func (f *agentAuthorizerFake) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, _ string, _ string, _ []identity.Scope, required identity.Scope) error {
	f.calls = append(f.calls, required)
	return f.err
}

type agentFenceReaderFake struct {
	fence AgentHostFence
	err   error
}

func (f *agentFenceReaderFake) ReadTaskFence(context.Context, string, string) (AgentHostFence, error) {
	return f.fence, f.err
}

type agentToolExecutorFake struct {
	definitions     []AgentToolDefinition
	preparation     AgentToolPreparation
	result          AgentToolResult
	verification    AgentVerificationResult
	prepareCalls    int
	executeCalls    int
	verifyCalls     int
	reconcileCalls  int
	onExecute       func()
	onVerify        func()
	prepareError    error
	executeError    error
	verifyError     error
	reconcileResult AgentToolResult
	lastRequest     AgentToolRequest
	lastVerifyReq   AgentVerificationRequest
}

func (f *agentToolExecutorFake) Definitions(context.Context, task.Task) ([]AgentToolDefinition, error) {
	return f.definitions, nil
}

func (f *agentToolExecutorFake) Prepare(ctx context.Context, request AgentToolRequest) (AgentToolPreparation, error) {
	f.prepareCalls++
	f.lastRequest = request
	if err := ctx.Err(); err != nil {
		return AgentToolPreparation{}, err
	}
	return f.preparation, f.prepareError
}

func (f *agentToolExecutorFake) Execute(ctx context.Context, request AgentToolRequest, preparation AgentToolPreparation) (AgentToolResult, error) {
	f.executeCalls++
	f.lastRequest = request
	if f.onExecute != nil {
		f.onExecute()
	}
	if err := ctx.Err(); err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, err
	}
	return f.result, f.executeError
}

func (f *agentToolExecutorFake) Verify(ctx context.Context, request AgentVerificationRequest) (AgentVerificationResult, error) {
	f.verifyCalls++
	f.lastVerifyReq = request
	if f.onVerify != nil {
		f.onVerify()
	}
	if err := ctx.Err(); err != nil {
		return AgentVerificationResult{Status: AgentVerifyUnknown}, err
	}
	return f.verification, f.verifyError
}

func (f *agentToolExecutorFake) ReconcileOperation(ctx context.Context, _ AgentToolRequest, _ task.Operation) (AgentToolResult, error) {
	f.reconcileCalls++
	if err := ctx.Err(); err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, err
	}
	return f.reconcileResult, nil
}

type agentTaskStoreFake struct {
	TaskStore
	mu                           sync.Mutex
	details                      TaskDetails
	operations                   map[string]task.Operation
	events                       []task.Event
	sequence                     int64
	lastProgress                 TaskProgress
	recordValidatedArtifactCalls int
	assistantMessages            []conversation.Message
	deltaErr                     error
	onCreateOperation            func()
	onBatchItemUpdated           func(task.BatchItem)
	onArtifactRecorded           func(task.Artifact)
}

func (f *agentTaskStoreFake) GetTask(ctx context.Context, id, ownerID string) (TaskDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TaskDetails{}, err
	}
	if f.details.Task.ID != id || f.details.Task.OwnerID != ownerID {
		return TaskDetails{}, fmt.Errorf("task not found")
	}
	return f.details, nil
}

func (f *agentTaskStoreFake) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Event{}, err
	}
	current := f.details.Task
	if current.Status != task.StatusRunning || current.Lease.Owner != progress.LeaseOwner || current.Lease.Version != progress.LeaseVersion {
		return task.Event{}, fmt.Errorf("stale test lease")
	}
	if progress.Usage.ActiveSeconds > current.Budget.ActiveSeconds || progress.Usage.ToolCalls > current.Budget.ToolCalls ||
		(progress.Usage.ModelTokens != nil && *progress.Usage.ModelTokens > current.Budget.ModelTokens) {
		return task.Event{}, fmt.Errorf("test task budget exceeded")
	}
	for _, next := range progress.Steps {
		updated := false
		for index := range f.details.Steps {
			if f.details.Steps[index].ID == next.ID {
				f.details.Steps[index] = next
				updated = true
				break
			}
		}
		if !updated {
			f.details.Steps = append(f.details.Steps, next)
		}
	}
	f.lastProgress = progress
	for _, artifact := range progress.Artifacts {
		f.details.Artifacts = append(f.details.Artifacts, artifact)
		f.sequence++
		payloadFields := map[string]any{
			"artifact_id": artifact.ID, "step_id": artifact.StepID, "label": artifact.Label,
			"media_type": artifact.MediaType, "path": artifact.Path, "workspace_id": artifact.WorkspaceID,
			"revision": artifact.Revision, "size_bytes": artifact.SizeBytes, "sha256": artifact.SHA256,
		}
		payload, _ := json.Marshal(payloadFields)
		f.events = append(f.events, task.Event{TaskID: artifact.TaskID, Sequence: f.sequence, Type: "task.artifact", Payload: payload})
	}
	f.details.Task.Status = progress.Status
	f.details.Task.CurrentStep = progress.CurrentStep
	f.details.Task.Result = progress.Result
	f.details.Task.WaitingReason = progress.WaitingReason
	f.details.Task.Usage = progress.Usage
	if progress.PendingRequest != nil {
		pending := *progress.PendingRequest
		pending.RequestHash = append([]byte(nil), progress.PendingRequest.RequestHash...)
		f.details.PendingRequest = &pending
	}
	if progress.ClarificationRequest != nil {
		f.details.ClarificationRequests = append(f.details.ClarificationRequests, *progress.ClarificationRequest)
	}
	if progress.AssistantMessage != nil {
		f.assistantMessages = append(f.assistantMessages, *progress.AssistantMessage)
	}
	if progress.Status != task.StatusRunning {
		f.details.Task.Lease = task.Lease{}
	}
	f.sequence++
	event := progress.Event
	event.Sequence = f.sequence
	f.events = append(f.events, event)
	if progress.AssistantMessage != nil {
		f.sequence++
		payload, _ := json.Marshal(map[string]any{
			"role": "assistant", "message_id": progress.AssistantMessage.ID, "turn_id": progress.CurrentStep,
			"content": progress.AssistantMessage.Content, "done": true,
		})
		f.events = append(f.events, task.Event{TaskID: progress.TaskID, Sequence: f.sequence, Type: "task.message", Payload: payload})
	}
	return event, nil
}

func (f *agentTaskStoreFake) AppendAssistantTextDelta(ctx context.Context, delta AssistantTextDelta) (task.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Event{}, err
	}
	if f.deltaErr != nil {
		return task.Event{}, f.deltaErr
	}
	current := f.details.Task
	if current.ID != delta.TaskID || current.Status != task.StatusRunning || current.Lease.Owner != delta.LeaseOwner || current.Lease.Version != delta.LeaseVersion {
		return task.Event{}, fmt.Errorf("stale test lease")
	}
	payload, err := json.Marshal(map[string]string{
		"role": "assistant", "message_id": delta.MessageID, "turn_id": delta.TurnID, "delta": delta.Delta,
	})
	if err != nil {
		return task.Event{}, err
	}
	f.sequence++
	event := task.Event{TaskID: delta.TaskID, Sequence: f.sequence, Type: "task.message", Payload: payload, CreatedAt: time.Now().UTC()}
	f.events = append(f.events, event)
	return event, nil
}

func (f *agentTaskStoreFake) ListUnknownOperations(ctx context.Context, ownerID, taskID string) ([]task.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ownerID != f.details.Task.OwnerID || taskID != f.details.Task.ID {
		return nil, fmt.Errorf("task not found")
	}
	var result []task.Operation
	for _, operation := range f.operations {
		if operation.Status == task.OperationUnknown {
			result = append(result, operation)
		}
	}
	return result, nil
}

func (f *agentTaskStoreFake) GetOperation(ctx context.Context, ownerID, taskID, key string) (task.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Operation{}, err
	}
	if ownerID != f.details.Task.OwnerID || taskID != f.details.Task.ID {
		return task.Operation{}, fmt.Errorf("task not found")
	}
	operation, ok := f.operations[key]
	if !ok {
		return task.Operation{}, domain.NewError(domain.CodeNotFound, "operation not found")
	}
	return operation, nil
}

func (f *agentTaskStoreFake) UpdateBatchItem(ctx context.Context, ownerID string, item task.BatchItem, leaseOwner string, leaseVersion int64) (task.BatchItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.BatchItem{}, err
	}
	currentTask := f.details.Task
	if currentTask.OwnerID != ownerID || currentTask.ID != item.TaskID || currentTask.Status != task.StatusRunning ||
		currentTask.Lease.Owner != leaseOwner || currentTask.Lease.Version != leaseVersion {
		return task.BatchItem{}, fmt.Errorf("stale batch item lease")
	}
	for index, current := range f.details.BatchItems {
		if current.ID != item.ID {
			continue
		}
		if current.Key != item.Key || current.Input != item.Input || current.Attempt != item.Attempt {
			return task.BatchItem{}, fmt.Errorf("batch item identity changed")
		}
		if current.Status == item.Status {
			if current.Result != item.Result || current.Error != item.Error {
				return task.BatchItem{}, fmt.Errorf("batch item result changed")
			}
			return current, nil
		}
		allowed := false
		switch current.Status {
		case task.BatchItemQueued:
			allowed = item.Status == task.BatchItemRunning || item.Status == task.BatchItemSucceeded || item.Status == task.BatchItemFailed || item.Status == task.BatchItemUnknown
		case task.BatchItemRunning:
			allowed = item.Status == task.BatchItemSucceeded || item.Status == task.BatchItemFailed || item.Status == task.BatchItemUnknown
		case task.BatchItemUnknown:
			allowed = item.Status == task.BatchItemRunning || item.Status == task.BatchItemSucceeded || item.Status == task.BatchItemFailed
		}
		if !allowed {
			return task.BatchItem{}, fmt.Errorf("invalid batch item transition %s -> %s", current.Status, item.Status)
		}
		item.CreatedAt, item.UpdatedAt = current.CreatedAt, current.UpdatedAt
		f.details.BatchItems[index] = item
		f.sequence++
		payload, _ := json.Marshal(map[string]any{
			"batch_item_id": item.ID, "key": item.Key, "status": string(item.Status), "result": item.Result, "error": item.Error, "attempt": item.Attempt,
		})
		f.events = append(f.events, task.Event{TaskID: item.TaskID, Sequence: f.sequence, Type: "task.batch_item", Payload: payload})
		if f.onBatchItemUpdated != nil {
			f.onBatchItemUpdated(item)
		}
		return item, nil
	}
	return task.BatchItem{}, fmt.Errorf("batch item not found")
}

func (f *agentTaskStoreFake) RecordValidatedArtifact(ctx context.Context, ownerID string, artifact task.Artifact, leaseOwner string, leaseVersion int64) (task.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordValidatedArtifactCalls++
	if err := ctx.Err(); err != nil {
		return task.Artifact{}, err
	}
	current := f.details.Task
	if current.OwnerID != ownerID || current.ID != artifact.TaskID || current.Status != task.StatusRunning ||
		current.Lease.Owner != leaseOwner || current.Lease.Version != leaseVersion {
		return task.Artifact{}, fmt.Errorf("stale artifact lease")
	}
	stepExists := false
	for _, step := range f.details.Steps {
		if step.ID == artifact.StepID && step.TaskID == artifact.TaskID {
			stepExists = true
			break
		}
	}
	if !stepExists {
		return task.Artifact{}, fmt.Errorf("artifact source step not found")
	}
	if artifact.BatchItemID != "" {
		item, ok := agentBatchItem(f.details.BatchItems, artifact.BatchItemID)
		if !ok || item.Attempt != artifact.BatchAttempt {
			return task.Artifact{}, fmt.Errorf("artifact batch attempt changed")
		}
	}
	for _, existing := range f.details.Artifacts {
		if existing.TaskID == artifact.TaskID && existing.BatchItemID == artifact.BatchItemID &&
			existing.BatchAttempt == artifact.BatchAttempt && existing.Path == artifact.Path {
			if existing.Label != artifact.Label || existing.MediaType != artifact.MediaType || existing.WorkspaceID != artifact.WorkspaceID ||
				existing.Revision != artifact.Revision || existing.SizeBytes != artifact.SizeBytes || existing.SHA256 != artifact.SHA256 {
				return task.Artifact{}, fmt.Errorf("artifact metadata changed")
			}
			return existing, nil
		}
	}
	f.details.Artifacts = append(f.details.Artifacts, artifact)
	f.sequence++
	payloadFields := map[string]any{"artifact_id": artifact.ID, "step_id": artifact.StepID, "label": artifact.Label, "media_type": artifact.MediaType,
		"path": artifact.Path, "workspace_id": artifact.WorkspaceID, "revision": artifact.Revision, "size_bytes": artifact.SizeBytes, "sha256": artifact.SHA256}
	if artifact.BatchItemID != "" {
		payloadFields["batch_item_id"] = artifact.BatchItemID
		payloadFields["batch_attempt"] = artifact.BatchAttempt
	}
	payload, _ := json.Marshal(payloadFields)
	f.events = append(f.events, task.Event{TaskID: artifact.TaskID, Sequence: f.sequence, Type: "task.artifact", Payload: payload})
	if f.onArtifactRecorded != nil {
		f.onArtifactRecorded(artifact)
	}
	return artifact, nil
}

func (f *agentTaskStoreFake) ListTaskEvents(ctx context.Context, ownerID, taskID string, after int64, limit int32) ([]task.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ownerID != f.details.Task.OwnerID || taskID != f.details.Task.ID {
		return nil, fmt.Errorf("task not found")
	}
	var result []task.Event
	for _, event := range f.events {
		if event.Sequence > after {
			result = append(result, event)
			if int32(len(result)) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (f *agentTaskStoreFake) CreateOperation(ctx context.Context, value task.Operation, leaseOwner string) (task.Operation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Operation{}, false, err
	}
	if existing, ok := f.operations[value.Key]; ok {
		if string(existing.RequestHash) != string(value.RequestHash) || existing.HostID != value.HostID {
			return task.Operation{}, false, fmt.Errorf("operation key conflict")
		}
		return existing, true, nil
	}
	if f.details.Task.Status != task.StatusRunning || f.details.Task.Lease.Owner != leaseOwner || f.details.Task.Lease.Version != value.LeaseVersion {
		return task.Operation{}, false, fmt.Errorf("stale operation lease")
	}
	f.operations[value.Key] = value
	f.appendOperationEvent(value.Key, value.Status)
	if f.onCreateOperation != nil {
		f.onCreateOperation()
	}
	return value, false, nil
}

func (f *agentTaskStoreFake) UpdateOperation(ctx context.Context, update OperationUpdate) (task.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Operation{}, err
	}
	value, ok := f.operations[update.OperationKey]
	if !ok {
		return task.Operation{}, fmt.Errorf("operation not found")
	}
	if value.Status != update.Status && !task.CanTransitionOperation(value.Status, update.Status) {
		return task.Operation{}, fmt.Errorf("invalid operation transition %s -> %s", value.Status, update.Status)
	}
	value.Status = update.Status
	value.ExternalID = update.ExternalID
	value.Result = update.Result
	value.LeaseVersion = update.LeaseVersion
	if update.Status == task.OperationDispatched || update.AdoptPrepared {
		value.RuntimeGeneration = update.RuntimeGeneration
		value.RuntimeInstanceID = update.RuntimeInstanceID
		value.ControlVersion = update.ControlVersion
		value.RuntimeLeaseVersion = update.RuntimeLeaseVersion
	}
	if update.RearmAfterNotFound {
		value.RuntimeGeneration = update.RuntimeGeneration
		value.RuntimeInstanceID = update.RuntimeInstanceID
		value.ControlVersion = update.ControlVersion
		value.RuntimeLeaseVersion = update.RuntimeLeaseVersion
	}
	f.operations[update.OperationKey] = value
	if update.Status != task.OperationPrepared || update.AdoptPrepared {
		f.appendOperationEvent(update.OperationKey, update.Status)
	}
	return value, nil
}

func (f *agentTaskStoreFake) appendOperationEvent(key string, status task.OperationStatus) {
	f.sequence++
	payload, _ := json.Marshal(map[string]string{"operation_key": key, "status": string(status)})
	f.events = append(f.events, task.Event{TaskID: f.details.Task.ID, Sequence: f.sequence, Type: "task.operation", Payload: payload})
}
