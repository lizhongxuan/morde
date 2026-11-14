package codex

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEventInputValidationEnforcesKindPayloadAndByteLimit(t *testing.T) {
	valid := codexTestEvent("event-00000001", EventUserMessage, map[string]any{"text": "hello"})
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event: %v", err)
	}

	unsupportedKind := valid
	unsupportedKind.Kind = EventKind("raw_json_rpc")
	if err := unsupportedKind.Validate(); err == nil {
		t.Fatal("unsupported event kind was accepted")
	}

	unsupportedPayload := valid
	unsupportedPayload.Payload = map[string]any{"text": "hello", "authorization": "secret"}
	if err := unsupportedPayload.Validate(); err == nil {
		t.Fatal("event payload accepted a field outside the kind allowlist")
	}

	oversized := codexTestEvent("event-00000002", EventAssistantDelta, map[string]any{
		"text": strings.Repeat("x", MaxEventBytes/2), "segment_id": strings.Repeat("y", MaxEventBytes/2), "delta_index": 1,
	})
	if err := oversized.Validate(); err == nil {
		t.Fatalf("event larger than %d bytes was accepted", MaxEventBytes)
	}
}

func TestEventBatchValidationEnforcesBatchLimitAndUniqueProviderIDs(t *testing.T) {
	duplicate := EventBatch{Events: []EventInput{
		codexTestEvent("event-duplicate", EventUserMessage, map[string]any{"text": "first"}),
		codexTestEvent("event-duplicate", EventUserMessage, map[string]any{"text": "second"}),
	}}
	if _, err := duplicate.Validate(); err == nil {
		t.Fatal("event batch accepted duplicate provider IDs")
	}

	events := make([]EventInput, 16)
	for index := range events {
		events[index] = codexTestEvent(fmt.Sprintf("event-%08d", index), EventUserMessage,
			map[string]any{"text": strings.Repeat("x", MaxEventBytes/2)})
	}
	oversized := EventBatch{Events: events}
	if _, err := oversized.Validate(); err == nil {
		t.Fatalf("event batch larger than %d bytes was accepted", MaxEventBatchBytes)
	}
}

func TestNormalizedEventKindsValidateSummaryAndQuestionFields(t *testing.T) {
	tests := []struct {
		name    string
		kind    EventKind
		payload map[string]any
	}{
		{"reasoning summary", EventReasoningSummary, map[string]any{"text": "Reasoning summary"}},
		{"plan", EventPlan, map[string]any{"summary": "Plan", "items": []string{"Inspect", "Implement"}, "explanation": "Two steps"}},
		{"tool call summary", EventToolCallSummary, map[string]any{"tool": "browser", "summary": "Search the release notes", "call_id": "call-1", "status": "running"}},
		{"tool result summary", EventToolResultSummary, map[string]any{"tool": "browser", "summary": "Found two relevant pages", "status": "completed"}},
		{"command summary", EventCommandSummary, map[string]any{"summary": "Ran checks", "output_summary": "12 passed", "exit_code": 0, "status": "completed"}},
		{"file change summary", EventFileChangeSummary, map[string]any{"path_label": "src/main.go", "change": "updated", "summary": "Added retry handling", "changes": []string{"update src/main.go"}}},
		{"subagent summary", EventSubagentSummary, map[string]any{"agent_label": "reviewer", "summary": "Reviewed authorization", "status": "completed"}},
		{"question", EventQuestion, map[string]any{"questions": []any{
			map[string]any{"id": "q1", "header": "Target", "question": "Which target?", "options": []any{
				map[string]any{"label": "Desktop", "description": "The authorized PC"},
			}},
		}}},
		{"approval status", EventApprovalStatus, map[string]any{"approval_id": "approval-1", "approval_version": "v1", "status": "approved"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := codexTestEvent(fmt.Sprintf("event-%08d", index), test.kind, test.payload)
			if err := event.Validate(); err != nil {
				t.Fatalf("normalized event was rejected: %v", err)
			}
		})
	}

	commandWithRawInput := codexTestEvent("event-raw-command", EventCommandSummary, map[string]any{
		"summary": "Ran a command", "command": "raw command line",
	})
	if err := commandWithRawInput.Validate(); err == nil {
		t.Fatal("command summary accepted a raw command field")
	}
	questionWithRPC := codexTestEvent("event-raw-rpc", EventQuestion, map[string]any{"questions": []any{
		map[string]any{"id": "q1", "header": "Question", "question": "Choose", "options": []any{}, "method": "item/tool/requestUserInput"},
	}})
	if err := questionWithRPC.Validate(); err == nil {
		t.Fatal("question event accepted an unnormalized protocol field")
	}
}

func TestCommandEnumsAndRequiredScopes(t *testing.T) {
	valid := []struct {
		input CommandInput
		scope Scope
	}{
		{CommandInput{Type: CommandSend, Text: "hello"}, ScopeSend},
		{CommandInput{Type: CommandInterrupt, TurnID: "turn-1"}, ScopeInterrupt},
		{CommandInput{Type: CommandApproval, ApprovalID: "approval-1", ApprovalVersion: "v1", Decision: "approve"}, ScopeApproval},
	}
	for _, test := range valid {
		if err := test.input.Validate(); err != nil {
			t.Errorf("command type %q: %v", test.input.Type, err)
		}
		if got := test.input.RequiredScope(); got != test.scope {
			t.Errorf("command type %q required scope = %q, want %q", test.input.Type, got, test.scope)
		}
	}

	if err := (CommandInput{Type: CommandType("execute"), Text: "hello"}).Validate(); err == nil {
		t.Fatal("unknown command type was accepted")
	}
	if err := (CommandInput{Type: CommandApproval, ApprovalID: "approval-1", ApprovalVersion: "v1", Decision: "allow"}).Validate(); err == nil {
		t.Fatal("unknown approval decision was accepted")
	}
	if (CommandInput{Type: CommandSend}).RequiredScope() != "" {
		t.Fatal("empty command payload unexpectedly implied a scope")
	}
	if (CommandStatus("waiting_pc")).ValidResult() || !CommandUnknown.ValidResult() {
		t.Fatal("command result status allowlist is incorrect")
	}
}

func TestCommandStatusEventsAreValidatedAndServerManaged(t *testing.T) {
	statusEvent := codexTestEvent("morde:command:g0000001:1:waiting_pc", EventCommandStatus, map[string]any{
		"command_id": "g0000001", "type": "send", "status": "waiting_pc", "result_code": "",
	})
	statusEvent.Status = "waiting_pc"
	if err := statusEvent.Validate(); err != nil {
		t.Fatalf("valid command status event: %v", err)
	}

	invalidStatus := statusEvent
	invalidStatus.Payload = map[string]any{
		"command_id": "g0000001", "type": "send", "status": "retrying", "result_code": "",
	}
	if err := invalidStatus.Validate(); err == nil {
		t.Fatal("command status event accepted an unknown status")
	}

	withMessage := statusEvent
	withMessage.Payload = map[string]any{
		"command_id": "g0000001", "type": "send", "status": "waiting_pc", "result_code": "", "text": "private request",
	}
	if err := withMessage.Validate(); err == nil {
		t.Fatal("command status event accepted a message payload")
	}
	if _, err := (EventBatch{Events: []EventInput{statusEvent}}).Validate(); err == nil {
		t.Fatal("PC event batch accepted a server-managed command status event")
	}

	reservedProviderID := codexTestEvent("morde:command:g0000001:2:failed", EventTurnFailed, map[string]any{"summary": "failed"})
	reservedProviderID.Status = "failed"
	if err := reservedProviderID.Validate(); err == nil {
		t.Fatal("provider event used a reserved server command event ID")
	}
}

func TestEventSourcesMapToGrantedScopes(t *testing.T) {
	tests := []struct {
		source EventSource
		scope  Scope
	}{
		{EventSourceSnapshot, ScopeRead},
		{EventSourceLive, ScopeLive},
		{EventSourceApproval, ScopeApproval},
	}
	for _, test := range tests {
		event := codexTestEvent("event-source", EventUserMessage, map[string]any{"text": "hello"})
		event.Source = test.source
		if err := event.Validate(); err != nil {
			t.Errorf("event source %q was rejected: %v", test.source, err)
		}
		if got := event.RequiredScope(); got != test.scope {
			t.Errorf("event source %q required scope = %q, want %q", test.source, got, test.scope)
		}
	}
	missing := codexTestEvent("event-source-missing", EventUserMessage, map[string]any{"text": "hello"})
	missing.Source = ""
	if err := missing.Validate(); err == nil {
		t.Fatal("event without a source was accepted")
	}
	unsupported := codexTestEvent("event-source-invalid", EventUserMessage, map[string]any{"text": "hello"})
	unsupported.Source = "admin"
	if err := unsupported.Validate(); err == nil {
		t.Fatal("event with an unsupported source was accepted")
	}
}

func codexTestEvent(id string, kind EventKind, payload map[string]any) EventInput {
	return EventInput{ProviderEventID: id, Kind: kind, Source: EventSourceLive, OccurredAt: time.Now().UTC(), Payload: payload}
}
