package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDiscoverValidateAllowsColonButRejectsPathSeparatorsInCWDLabel(t *testing.T) {
	valid := Discover{ProfileKey: "profile-1", ThreadID: "thread-1", Title: "Thread",
		CWDLabel: "project:notes", Status: SessionIdle}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid macOS/Linux CWD label was rejected: %v", err)
	}

	for _, label := range []string{"project/name", `project\name`} {
		t.Run(label, func(t *testing.T) {
			invalid := valid
			invalid.CWDLabel = label
			if err := invalid.Validate(); err == nil {
				t.Fatalf("CWD label containing a path separator was accepted: %q", label)
			}
		})
	}
	invalidProfile := valid
	invalidProfile.ProfileKey = "local_CODEX_HOME_profile"
	if err := invalidProfile.Validate(); err == nil {
		t.Fatal("Codex profile metadata accepted an environment variable name")
	}
}

func TestProfileConnectionRequiresOpaqueKeyAndReadLiveScopes(t *testing.T) {
	valid := ProfileConnectionRequest{ProfileKey: "profile_1", Scopes: []Scope{ScopeRead, ScopeLive, ScopeSend}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid profile connection was rejected: %v", err)
	}

	for _, invalid := range []ProfileConnectionRequest{
		{ProfileKey: "", Scopes: []Scope{ScopeRead, ScopeLive}},
		{ProfileKey: "../secret", Scopes: []Scope{ScopeRead, ScopeLive}},
		{ProfileKey: "local_CODEX_HOME_profile", Scopes: []Scope{ScopeRead, ScopeLive}},
		{ProfileKey: "profile", Scopes: nil},
		{ProfileKey: "profile", Scopes: []Scope{}},
		{ProfileKey: "profile", Scopes: []Scope{ScopeRead}},
		{ProfileKey: "profile", Scopes: []Scope{ScopeLive}},
		{ProfileKey: "profile", Scopes: []Scope{ScopeRead, ScopeLive, ScopeLive}},
		{ProfileKey: "profile", Scopes: []Scope{ScopeRead, ScopeLive, Scope("admin")}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid profile connection was accepted: %#v", invalid)
		}
	}
}

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

func TestCodexToolEventChunksValidateAndPreserveLegacyEvents(t *testing.T) {
	call := codexTestEvent("tool-input-0001", EventToolCall, map[string]any{
		"tool": "command", "call_id": "call-1", "input": strings.Repeat("界", 8*1024), "input_part": 0, "input_parts": 2,
	})
	if err := call.Validate(); err != nil {
		t.Fatalf("valid tool input chunk was rejected: %v", err)
	}
	result := codexTestEvent("tool-output-0001", EventToolResult, map[string]any{
		"tool": "command", "call_id": "call-1", "output": strings.Repeat("界", 8*1024), "output_part": 1, "output_parts": 2,
	})
	if err := result.Validate(); err != nil {
		t.Fatalf("valid tool output chunk was rejected: %v", err)
	}
	liveDelta := codexTestEvent("tool-output-delta", EventToolResult, map[string]any{
		"tool": "command", "call_id": "call-1", "output": "partial output", "output_part": 0,
	})
	if err := liveDelta.Validate(); err != nil {
		t.Fatalf("live output delta without a known parts count was rejected: %v", err)
	}

	legacy := codexTestEvent("tool-output-legacy", EventToolResult, map[string]any{
		"tool": "command", "output": strings.Repeat("x", 30*1024),
	})
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy event without chunk metadata was rejected: %v", err)
	}
	legacyWithCallID := codexTestEvent("tool-output-legacy-call", EventToolResult, map[string]any{
		"tool": "command", "call_id": "legacy/provider-call", "output": strings.Repeat("x", 30*1024),
	})
	if err := legacyWithCallID.Validate(); err != nil {
		t.Fatalf("legacy result with call ID and no chunk metadata was rejected: %v", err)
	}

	invalid := []struct {
		name    string
		kind    EventKind
		payload map[string]any
	}{
		{"input missing part", EventToolCall, map[string]any{"call_id": "call-1", "input": "args", "input_parts": 1}},
		{"input missing count", EventToolCall, map[string]any{"call_id": "call-1", "input": "args", "input_part": 0}},
		{"input part outside count", EventToolCall, map[string]any{"call_id": "call-1", "input": "args", "input_part": 1, "input_parts": 1}},
		{"fractional input part", EventToolCall, map[string]any{"call_id": "call-1", "input": "args", "input_part": 0.5, "input_parts": 1}},
		{"oversized input chunk", EventToolCall, map[string]any{"call_id": "call-1", "input": strings.Repeat("x", MaxToolPayloadChunkBytes+1), "input_part": 0, "input_parts": 1}},
		{"output missing call ID", EventToolResult, map[string]any{"output": "partial", "output_part": 0}},
		{"output missing text", EventToolResult, map[string]any{"call_id": "call-1", "output_part": 0}},
		{"output count before part", EventToolResult, map[string]any{"call_id": "call-1", "output": "partial", "output_part": 1, "output_parts": 1}},
		{"oversized output chunk", EventToolResult, map[string]any{"call_id": "call-1", "output": strings.Repeat("x", MaxToolPayloadChunkBytes+1), "output_part": 0}},
	}
	for index, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			event := codexTestEvent(fmt.Sprintf("tool-invalid-%04d", index), test.kind, test.payload)
			if err := event.Validate(); err == nil {
				t.Fatal("invalid tool event payload was accepted")
			}
		})
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
		{"question", EventQuestion, map[string]any{"question_request_id": " request ", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": "q1", "header": "Target", "question": "Which target?", "options": []any{
				map[string]any{"label": "Desktop", "description": "The authorized PC"},
			}},
		}}},
		{"approval request details", EventApprovalRequest, map[string]any{
			"approval_id": "approval-1", "approval_version": "v1", "kind": "command",
			"summary": "Run project checks", "object": "npm run check", "detail": "work/project",
			"impact": "Codex requested command execution",
		}},
		{"approval status", EventApprovalStatus, map[string]any{"approval_id": "approval-1", "approval_version": "v1", "status": "approved"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := codexTestEvent(fmt.Sprintf("event-%08d", index), test.kind, test.payload)
			if test.kind == EventApprovalRequest || test.kind == EventApprovalStatus {
				event.Source = EventSourceApproval
			}
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
	questionWithRPC := codexTestEvent("event-raw-rpc", EventQuestion, map[string]any{
		"question_request_id": " request ", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": "q1", "header": "Question", "question": "Choose", "options": []any{}, "method": "item/tool/requestUserInput"},
		},
	})
	if err := questionWithRPC.Validate(); err == nil {
		t.Fatal("question event accepted an unnormalized protocol field")
	}

	pendingQuestion := codexTestEvent("question-pending", EventQuestion, map[string]any{
		"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": " choice ", "header": "Choice", "question": "Choose one", "options": []any{
				map[string]any{"label": "A", "description": "Option A"},
			}, "is_secret": false, "is_other": true},
		},
	})
	if err := pendingQuestion.Validate(); err != nil {
		t.Fatalf("valid pending question was rejected: %v", err)
	}
	if pendingQuestion.RequiredScope() != ScopeLive {
		t.Fatalf("question required scope = %q, want live", pendingQuestion.RequiredScope())
	}
	preservedQuestionID := pendingQuestion.Payload["questions"].([]any)[0].(map[string]any)["id"]
	if preservedQuestionID != " choice " {
		t.Fatalf("opaque question ID changed during validation: %q", preservedQuestionID)
	}
	answeredQuestion := codexTestEvent("question-answered", EventQuestion, map[string]any{
		"question_request_id": " request ", "question_version": strings.Repeat("a", 64), "status": "answered",
	})
	if err := answeredQuestion.Validate(); err != nil {
		t.Fatalf("valid terminal question event was rejected: %v", err)
	}
	for name, payload := range map[string]map[string]any{
		"pending without questions": {"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending"},
		"terminal with questions":   {"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "answered", "questions": []any{}},
		"unknown status":            {"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "approved"},
		"malformed version":         {"question_request_id": "100", "question_version": "v1", "status": "answered"},
	} {
		t.Run(name, func(t *testing.T) {
			event := codexTestEvent("question-invalid-"+name, EventQuestion, payload)
			if err := event.Validate(); err == nil {
				t.Fatal("invalid question lifecycle payload was accepted")
			}
		})
	}
	wrongSource := pendingQuestion
	wrongSource.Source = EventSourceApproval
	if err := wrongSource.Validate(); err == nil {
		t.Fatal("question event accepted the approval source")
	}
	wrongSource.Source = EventSourceSnapshot
	if err := wrongSource.Validate(); err == nil {
		t.Fatal("question event accepted the snapshot source")
	}

	invalidID := codexTestEvent("question-invalid-id", EventQuestion, map[string]any{
		"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": " bad\x00id ", "header": "Choice", "question": "Choose one", "options": []any{}},
		},
	})
	if err := invalidID.Validate(); err == nil {
		t.Fatal("question event accepted a question ID containing a control character")
	}
	invalidUTF8ID := codexTestEvent("question-invalid-utf8-id", EventQuestion, map[string]any{
		"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": string([]byte{0xff}), "header": "Choice", "question": "Choose one", "options": []any{}},
		},
	})
	if err := invalidUTF8ID.Validate(); err == nil {
		t.Fatal("question event accepted a question ID with invalid UTF-8")
	}
	duplicateIDs := codexTestEvent("question-duplicate-ids", EventQuestion, map[string]any{
		"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
			map[string]any{"id": "choice", "header": "First", "question": "Choose first", "options": []any{}},
			map[string]any{"id": "choice", "header": "Second", "question": "Choose second", "options": []any{}},
		},
	})
	if err := duplicateIDs.Validate(); err == nil {
		t.Fatal("question event accepted duplicate opaque question IDs")
	}

	extraPendingField := codexTestEvent("question-extra-field", EventQuestion, map[string]any{
		"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{}, "extra": true,
	})
	if err := extraPendingField.Validate(); err == nil {
		t.Fatal("question event accepted an unsupported lifecycle field")
	}
}

func TestQuestionDisplayTextPreservesWhitespaceAndValidatesUTF8BytesAndControls(t *testing.T) {
	newQuestionEvent := func(field string, value any) EventInput {
		question := map[string]any{"id": "q1", "header": "Header", "question": "Question", "options": []any{
			map[string]any{"label": "Option", "description": "Description"},
		}}
		if field == "option_label" || field == "option_description" {
			option := question["options"].([]any)[0].(map[string]any)
			key := strings.TrimPrefix(field, "option_")
			option[key] = value
		} else {
			question[field] = value
		}
		return codexTestEvent("question-display-"+field, EventQuestion, map[string]any{
			"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending",
			"questions": []any{question},
		})
	}

	valid := newQuestionEvent("header", " \t ")
	question := valid.Payload["questions"].([]any)[0].(map[string]any)
	question["question"] = "\n\r\t"
	option := question["options"].([]any)[0].(map[string]any)
	option["label"], option["description"] = "", " \t "
	if err := valid.Validate(); err != nil {
		t.Fatalf("empty or whitespace-only display strings were rejected: %v", err)
	}
	if question["header"] != " \t " || question["question"] != "\n\r\t" || option["label"] != "" || option["description"] != " \t " {
		t.Fatal("question display strings were changed during validation")
	}

	for _, test := range []struct {
		field string
		value any
	}{
		{field: "header", value: "bad\x01text"},
		{field: "question", value: "bad\x7ftext"},
		{field: "option_label", value: "bad\x01text"},
		{field: "option_description", value: "bad\x9ftext"},
		{field: "header", value: string([]byte{0xff})},
		{field: "question", value: strings.Repeat("é", 2049)},
	} {
		t.Run(test.field, func(t *testing.T) {
			if err := newQuestionEvent(test.field, test.value).Validate(); err == nil {
				t.Fatalf("invalid display text was accepted for %s", test.field)
			}
		})
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
		{CommandInput{Type: CommandAnswerQuestion, QuestionRequestID: " request ", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{" choice ": {"A"}}}, ScopeSend},
	}
	for _, test := range valid {
		if err := test.input.Validate(); err != nil {
			t.Errorf("command type %q: %v", test.input.Type, err)
		}
		if test.input.Type == CommandAnswerQuestion && test.input.Answers[" choice "][0] != "A" {
			t.Errorf("answer question ID changed during validation: %#v", test.input.Answers)
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
	for name, input := range map[string]CommandInput{
		"missing answers":           {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64)},
		"empty answers map":         {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{}},
		"empty answer array":        {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {}}},
		"blank request ID":          {Type: CommandAnswerQuestion, QuestionRequestID: " ", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {"A"}}},
		"control in request ID":     {Type: CommandAnswerQuestion, QuestionRequestID: "request\x00id", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {"A"}}},
		"invalid UTF-8 request ID":  {Type: CommandAnswerQuestion, QuestionRequestID: string([]byte{0xff}), QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {"A"}}},
		"oversized request ID":      {Type: CommandAnswerQuestion, QuestionRequestID: strings.Repeat("x", 513), QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {"A"}}},
		"malformed version":         {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: "v1", Answers: map[string][]string{"q1": {"A"}}},
		"blank version":             {Type: CommandAnswerQuestion, QuestionRequestID: "100", Answers: map[string][]string{"q1": {"A"}}},
		"blank question ID":         {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{" \t ": {"A"}}},
		"blank answer":              {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {" \t "}}},
		"oversized answer":          {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {strings.Repeat("x", 4097)}}},
		"control in answer":         {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {"a\x00b"}}},
		"invalid UTF-8 answer":      {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {string([]byte{0xff})}}},
		"invalid UTF-8 question ID": {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{string([]byte{0xff}): {"A"}}},
		"control in question ID":    {Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q\x00id": {"A"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := input.Validate(); err == nil {
				t.Fatal("invalid question answer command was accepted")
			}
		})
	}
	maxQuestionAnswers := make(map[string][]string, 64)
	for index := range 64 {
		maxQuestionAnswers[fmt.Sprintf("q%d", index)] = make([]string, 64)
		for answerIndex := range maxQuestionAnswers[fmt.Sprintf("q%d", index)] {
			maxQuestionAnswers[fmt.Sprintf("q%d", index)][answerIndex] = " A "
		}
	}
	if err := (CommandInput{Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: maxQuestionAnswers}).Validate(); err != nil {
		t.Fatalf("maximum valid answer map and exact whitespace answer were rejected: %v", err)
	}
	tooManyQuestions := make(map[string][]string, 65)
	for index := range 65 {
		tooManyQuestions[fmt.Sprintf("q%d", index)] = []string{"A"}
	}
	if err := (CommandInput{Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: tooManyQuestions}).Validate(); err == nil {
		t.Fatal("answer map accepted more than 64 question IDs")
	}
	tooManyAnswers := make([]string, 65)
	for index := range tooManyAnswers {
		tooManyAnswers[index] = "A"
	}
	if err := (CommandInput{Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": tooManyAnswers}}).Validate(); err == nil {
		t.Fatal("answer map accepted more than 64 answers for a question")
	}
	if err := (CommandInput{Type: CommandAnswerQuestion, QuestionRequestID: "100", QuestionVersion: strings.Repeat("a", 64), Answers: map[string][]string{"q1": {strings.Repeat("x", 4096)}}}).Validate(); err != nil {
		t.Fatalf("4096-byte answer was rejected: %v", err)
	}
	for _, raw := range []string{
		`{"type":"answer_question","question_request_id":"100","question_version":"v1","answers":{"q1":"A"}}`,
		`{"type":"answer_question","question_request_id":"100","question_version":"v1","answers":{"q1":[1]}}`,
	} {
		var input CommandInput
		if err := json.Unmarshal([]byte(raw), &input); err == nil {
			t.Fatalf("answer_question accepted a non-string-array answer value: %s", raw)
		}
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
	statusEvent.Source = ""
	statusEvent.Status = "waiting_pc"
	if err := statusEvent.Validate(); err != nil {
		t.Fatalf("valid command status event: %v", err)
	}
	questionStatusEvent := codexTestEvent("morde:command:g0000002:1:waiting_pc", EventCommandStatus, map[string]any{
		"command_id": "g0000002", "type": "answer_question", "status": "waiting_pc", "result_code": "",
	})
	questionStatusEvent.Source = ""
	questionStatusEvent.Status = "waiting_pc"
	if err := questionStatusEvent.Validate(); err != nil {
		t.Fatalf("valid answer_question command status event: %v", err)
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
		name    string
		kind    EventKind
		source  EventSource
		payload map[string]any
		scope   Scope
		valid   bool
	}{
		{name: "ordinary snapshot", kind: EventUserMessage, source: EventSourceSnapshot, payload: map[string]any{"text": "hello"}, scope: ScopeRead, valid: true},
		{name: "ordinary live", kind: EventUserMessage, source: EventSourceLive, payload: map[string]any{"text": "hello"}, scope: ScopeLive, valid: true},
		{name: "ordinary event cannot claim approval", kind: EventUserMessage, source: EventSourceApproval, payload: map[string]any{"text": "hello"}},
		{name: "approval request", kind: EventApprovalRequest, source: EventSourceApproval, payload: map[string]any{"approval_id": "approval-1", "approval_version": "v1", "kind": "command"}, scope: ScopeApproval, valid: true},
		{name: "approval request cannot claim live", kind: EventApprovalRequest, source: EventSourceLive, payload: map[string]any{"approval_id": "approval-1", "approval_version": "v1", "kind": "command"}},
		{name: "approval status", kind: EventApprovalStatus, source: EventSourceApproval, payload: map[string]any{"approval_id": "approval-1", "approval_version": "v1", "status": "approved"}, scope: ScopeApproval, valid: true},
		{name: "approval status cannot claim snapshot", kind: EventApprovalStatus, source: EventSourceSnapshot, payload: map[string]any{"approval_id": "approval-1", "approval_version": "v1", "status": "approved"}},
		{name: "question uses live", kind: EventQuestion, source: EventSourceLive, payload: map[string]any{
			"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
				map[string]any{"id": " q1 ", "header": "Target", "question": "Which target?", "options": []any{}},
			},
		}, scope: ScopeLive, valid: true},
		{name: "question cannot claim approval", kind: EventQuestion, source: EventSourceApproval, payload: map[string]any{
			"question_request_id": "100", "question_version": strings.Repeat("a", 64), "status": "pending", "questions": []any{
				map[string]any{"id": "q1", "header": "Target", "question": "Which target?", "options": []any{}},
			},
		}},
		{name: "user question historical snapshot requires read", kind: EventUnsupported, source: EventSourceSnapshot, payload: map[string]any{"provider_type": "user_question", "diagnostic": "input required"}, scope: ScopeRead, valid: true},
		{name: "user question live diagnostic cannot bypass approval", kind: EventUnsupported, source: EventSourceLive, payload: map[string]any{"provider_type": "user_question", "diagnostic": "input required"}},
		{name: "user question approval diagnostic requires approval", kind: EventUnsupported, source: EventSourceApproval, payload: map[string]any{"provider_type": "user_question", "diagnostic": "input required"}, scope: ScopeApproval, valid: true},
		{name: "other unsupported snapshot", kind: EventUnsupported, source: EventSourceSnapshot, payload: map[string]any{"provider_type": "future_event", "diagnostic": "not normalized"}, scope: ScopeRead, valid: true},
		{name: "other unsupported live", kind: EventUnsupported, source: EventSourceLive, payload: map[string]any{"provider_type": "future_event", "diagnostic": "not normalized"}, scope: ScopeLive, valid: true},
		{name: "other unsupported cannot claim approval", kind: EventUnsupported, source: EventSourceApproval, payload: map[string]any{"provider_type": "future_event", "diagnostic": "not normalized"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := codexTestEvent(fmt.Sprintf("event-source-%02d", index), test.kind, test.payload)
			event.Source = test.source
			err := event.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid source-kind combination was rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid source-kind combination was accepted")
			}
			if test.valid && event.RequiredScope() != test.scope {
				t.Fatalf("required scope = %q, want %q", event.RequiredScope(), test.scope)
			}
		})
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
