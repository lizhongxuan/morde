package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type terminalDeliveryGatewayStoreTest struct {
	*modelGatewayStoreTest
	modelID     string
	projectID   string
	record      TaskTerminalDeliveryModelResponseRecord
	found       bool
	disposition TaskTerminalDeliveryModelCallDisposition
}

func (s *terminalDeliveryGatewayStoreTest) TaskTerminalDeliveryModelID(context.Context,
	TaskTerminalDeliveryModelFence) (string, error) {
	return s.modelID, nil
}

func (s *terminalDeliveryGatewayStoreTest) ModelTaskProjectIDForTerminalDelivery(context.Context,
	TaskTerminalDeliveryModelFence) (string, error) {
	return s.projectID, nil
}

func (s *terminalDeliveryGatewayStoreTest) RecoverTaskTerminalDeliveryModelResponse(context.Context,
	TaskTerminalDeliveryModelFence) (TaskTerminalDeliveryModelResponseRecord, bool, error) {
	return s.record, s.found, nil
}

func (s *terminalDeliveryGatewayStoreTest) TaskTerminalDeliveryModelCallDisposition(context.Context,
	TaskTerminalDeliveryModelFence) (TaskTerminalDeliveryModelCallDisposition, error) {
	return s.disposition, nil
}

func terminalDeliveryTestFence(t *testing.T, leased bool) TaskTerminalDeliveryModelFence {
	t.Helper()
	digest := sha256.Sum256([]byte("terminal source event"))
	fence := TaskTerminalDeliveryModelFence{
		DeliveryID: 42, TaskID: "q1234567", OwnerID: "u1234567", ConversationID: "c1234567", InitiatorID: "u1234567",
		TaskRevision: 2, AuthorizationVersion: 1, SourceKind: TaskTerminalDeliverySourceFailure,
		SourceEventSequence: 9, SourceEventType: "task.result", SourceSHA256: append([]byte(nil), digest[:]...),
		ClaimGeneration: 3, ModelCallID: "terminal_delivery_42",
	}
	if leased {
		fence.LeaseOwner = "terminal-worker"
		fence.LeaseExpiresAt = time.Now().UTC().Add(time.Minute)
	}
	if !ValidTaskTerminalDeliveryModelFence(fence) {
		t.Fatalf("test terminal delivery fence is invalid: %+v", fence)
	}
	return fence
}

func terminalDeliveryTestRequest(modelID, text string) json.RawMessage {
	encoded, _ := json.Marshal(struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Tools     []any `json:"tools"`
		MaxTokens int   `json:"max_tokens"`
	}{Provider: "morde", Model: modelID, Messages: []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{{Role: "user", Content: []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}}}}, Tools: []any{}, MaxTokens: 32})
	return encoded
}

func TestTaskTerminalDeliveryModelFenceSeparatesDispatchAndRecovery(t *testing.T) {
	active := terminalDeliveryTestFence(t, true)
	ready := terminalDeliveryTestFence(t, false)
	if !ValidTaskTerminalDeliveryModelDispatchFence(active) || ValidTaskTerminalDeliveryModelDispatchFence(ready) {
		t.Fatalf("dispatch and recovery fence validation was not separated: active=%t ready=%t",
			ValidTaskTerminalDeliveryModelDispatchFence(active), ValidTaskTerminalDeliveryModelDispatchFence(ready))
	}
	callID, err := DSHTerminalDeliveryModelCallID(ready)
	if err != nil || callID != "terminal_delivery_42" {
		t.Fatalf("stable delivery call ID = %q, %v", callID, err)
	}
	otherGeneration := active
	otherGeneration.ClaimGeneration++
	otherCallID, otherErr := DSHTerminalDeliveryModelCallID(otherGeneration)
	if !ValidTaskTerminalDeliveryModelFence(otherGeneration) || otherErr != nil || otherCallID != callID {
		t.Fatalf("a reclaim generation changed the stable delivery identity: %+v", otherGeneration)
	}
}

func TestTaskTerminalDeliveryModelRecoveryDoesNotResolveCurrentConnection(t *testing.T) {
	fence := terminalDeliveryTestFence(t, false)
	modelID := "p1234567"
	raw := terminalDeliveryTestRequest(modelID, "summarize the failure")
	request, err := parseDSHModelRequest(context.Background(), ModelTaskFence{}, modelID, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	modelCall := ModelCall{Source: ModelCallTerminalDelivery, TerminalDelivery: &fence, ModelID: modelID,
		CallID: fence.ModelCallID, RawRequest: raw, Request: request}
	_, requestDigest, _, err := validateModelCall(modelCall)
	if err != nil {
		t.Fatal(err)
	}
	response := ModelResponse{Text: "The task failed because the provider was unavailable.", StopReason: "stop",
		ProviderReportedModel: "test-model", Usage: ModelUsage{Known: true, InputTokens: 21, OutputTokens: 12}}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealTaskTerminalDeliveryModelResponse(sealer, fence, modelID, requestDigest[:], response)
	if err != nil {
		t.Fatal(err)
	}
	cipherDigest := sha256.Sum256(ciphertext)
	store := &terminalDeliveryGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, record: TaskTerminalDeliveryModelResponseRecord{
		ModelID: modelID, RequestDigest: requestDigest[:], Ciphertext: ciphertext, CiphertextSHA256: cipherDigest[:],
	}, found: true}
	resolver := &modelGatewayResolverTest{}
	service, err := NewModelGatewayService(store, resolver, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := service.RecoverTaskTerminalDeliveryDSHModelResponse(context.Background(), fence, raw)
	if err != nil || !found || recovered.Text != response.Text || resolver.model != "" {
		t.Fatalf("recover settled reply after connection changes: found=%t response=%+v resolver=%+v err=%v",
			found, recovered, resolver, err)
	}
	if _, found, err := service.RecoverTaskTerminalDeliveryDSHModelResponse(context.Background(), fence,
		terminalDeliveryTestRequest(modelID, "a changed request")); found || domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("recovery accepted a changed request: found=%t err=%v", found, err)
	}
	tampered := store.record
	tampered.Ciphertext = append([]byte(nil), tampered.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff
	store.record = tampered
	if _, found, err := service.RecoverTaskTerminalDeliveryDSHModelResponse(context.Background(), fence, raw); found || domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("recovery accepted tampered ciphertext: found=%t err=%v", found, err)
	}
}

func TestCompleteTaskTerminalDeliveryModelReportsSafeAndUnknownOutcomes(t *testing.T) {
	for _, test := range []struct {
		name        string
		reserveErr  error
		clientErr   error
		disposition TaskTerminalDeliveryModelCallDisposition
		wantCalls   int
	}{
		{name: "quota before dispatch", reserveErr: domain.NewError(domain.CodeQuotaExceeded, "no budget"),
			disposition: TaskTerminalDeliveryModelNotDispatched},
		{name: "provider call unknown", clientErr: &ModelClientError{Kind: ModelErrorTimeout},
			disposition: TaskTerminalDeliveryModelUnknown, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fence := terminalDeliveryTestFence(t, true)
			store := &terminalDeliveryGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{reserveErr: test.reserveErr},
				modelID: "p1234567", disposition: test.disposition}
			client := &modelGatewayClientTest{response: ModelResponse{Text: "A safe reply.", StopReason: "stop",
				ProviderReportedModel: "test-model", Usage: ModelUsage{Known: true, InputTokens: 4, OutputTokens: 3}}, err: test.clientErr}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
			if err != nil {
				t.Fatal(err)
			}
			setTestModelResponseSealer(t, service)
			response, disposition, err := service.CompleteTaskTerminalDeliveryDSHModel(context.Background(), fence,
				"p1234567", terminalDeliveryTestRequest("p1234567", "make a concise failure reply"))
			if err == nil || disposition != test.disposition || client.calls != test.wantCalls {
				t.Fatalf("terminal delivery outcome = response=%+v disposition=%q calls=%d err=%v",
					response, disposition, client.calls, err)
			}
			if test.disposition == TaskTerminalDeliveryModelUnknown && store.settlement.Source != ModelCallTerminalDelivery {
				t.Fatalf("unknown provider use was not retained in the terminal ledger: %+v", store.settlement)
			}
		})
	}
}

func TestTaskTerminalDeliveryRecoveryDiagnosisIsSentAsSafeModelFact(t *testing.T) {
	event := []byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_stage":"model_response_validation","failure_category":"unauthorized_tool","failure_reason":"tool_not_in_task_allowlist","tool_name":"morde_mcp_private","tool_call_id":"call_private","tool_dispatched":false}}`)
	diagnosis := TaskTerminalDeliveryRecoveryDiagnosisFromEvent(event, TaskTerminalDeliveryReadOnlyToolExecution{})
	const expected = "模型提出了当前任务未授权的工具操作，系统在派发前已拦截。"
	if diagnosis != expected {
		t.Fatalf("recovery diagnosis = %q, want %q", diagnosis, expected)
	}

	eventDigest := sha256.Sum256([]byte("waiting event"))
	objectiveDigest := sha256.Sum256([]byte("today's news"))
	facts := TaskTerminalDeliverySourceFacts{
		Identity: TaskTerminalDeliveryIdentity{
			DeliveryID: 42, TaskID: "q1234567", OwnerID: "u1234567", ConversationID: "c1234567", InitiatorID: "u1234567",
			TaskRevision: 2, AuthorizationVersion: 1, SourceKind: TaskTerminalDeliverySourceWaiting,
			SourceEventSequence: 9, SourceEventType: "task.waiting_user", SourceSHA256: eventDigest[:],
		},
		TaskStatus: "waiting_user", ModelID: "p1234567", ObjectiveSummary: "today's news", ObjectiveSHA256: objectiveDigest[:],
		WaitingReason: "reconciliation_required", RecoveryDiagnosis: diagnosis,
	}
	factsDigest, err := TaskTerminalDeliveryFactsDigest(facts)
	if err != nil {
		t.Fatalf("digest terminal explanation facts: %v", err)
	}
	facts.FactsSHA256 = factsDigest
	request, err := BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
	if err != nil {
		t.Fatalf("build terminal explanation request: %v", err)
	}
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatalf("decode terminal explanation request: %v", err)
	}
	if len(decoded.Messages) != 2 || decoded.Messages[1].Role != "user" || len(decoded.Messages[1].Content) != 1 {
		t.Fatalf("terminal explanation request messages = %#v", decoded.Messages)
	}
	var modelFacts struct {
		RecoveryDiagnosis string `json:"recovery_diagnosis"`
	}
	if err := json.Unmarshal([]byte(decoded.Messages[1].Content[0].Text), &modelFacts); err != nil {
		t.Fatalf("decode terminal explanation facts: %v", err)
	}
	if modelFacts.RecoveryDiagnosis != expected {
		t.Fatalf("model recovery diagnosis = %q, want %q", modelFacts.RecoveryDiagnosis, expected)
	}
	for _, internalValue := range []string{"unauthorized_tool", "tool_not_in_task_allowlist", "morde_mcp_private", "call_private"} {
		if strings.Contains(string(request), internalValue) {
			t.Fatalf("terminal explanation model request exposed internal value %q", internalValue)
		}
	}
}

func TestTaskTerminalDeliveryRecoveryDiagnosisUsesPlainFallbackForUnknownCause(t *testing.T) {
	event := []byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_category":"private_provider_failure","failure_reason":"private_reason","recovery_stage":"cloud_dsh_run","recovery_reason":"unclassified_runtime_error"}}`)
	diagnosis := TaskTerminalDeliveryRecoveryDiagnosisFromEvent(event, TaskTerminalDeliveryReadOnlyToolExecution{})
	if diagnosis != "模型请求未能得到确认，现有记录无法判定具体原因。" ||
		strings.Contains(diagnosis, "private_provider_failure") || strings.Contains(diagnosis, "private_reason") {
		t.Fatalf("unknown recovery diagnosis is not a safe plain fallback: %q", diagnosis)
	}
}

func TestTaskTerminalDeliveryExplainsTaskBudgetReservationFailure(t *testing.T) {
	event := []byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_stage":"model_call_reservation","failure_category":"task_budget_exhausted","failure_reason":"task_budget_reservation_exceeded"}}`)
	diagnosis := TaskTerminalDeliveryRecoveryDiagnosisFromEvent(event, TaskTerminalDeliveryReadOnlyToolExecution{})
	const expected = "模型请求在派发前被任务预算检查拦截；所需的保守预算预留超过任务剩余额度。"
	if diagnosis != expected {
		t.Fatalf("task budget diagnosis = %q, want %q", diagnosis, expected)
	}
	if !validTerminalDeliveryRecoveryDiagnosis(diagnosis) || strings.Contains(diagnosis, "task_budget_exhausted") ||
		strings.Contains(diagnosis, "task_budget_reservation_exceeded") {
		t.Fatalf("task budget diagnosis is not a safe model fact: %q", diagnosis)
	}

	unknown := TaskTerminalDeliveryRecoveryDiagnosisFromEvent([]byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_stage":"private_stage","failure_category":"task_budget_exhausted","failure_reason":"private_reason"}}`), TaskTerminalDeliveryReadOnlyToolExecution{})
	if unknown != "任务的模型预算不足，模型请求未能发送。" {
		t.Fatalf("unrecognized budget details did not use the bounded fallback: %q", unknown)
	}
}

func TestTaskTerminalDeliveryBudgetDiagnosisIncludesSafeExecutionCounts(t *testing.T) {
	event := []byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_stage":"model_call_reservation","failure_category":"task_budget_exhausted","failure_reason":"task_budget_reservation_exceeded","query":"private query","url":"https://private.example/news","result":"private result","tool_call_id":"call_private"}}`)
	execution := TaskTerminalDeliveryReadOnlyToolExecution{SuccessfulWebSearchCalls: 10, SuccessfulWebReadCalls: 3}
	const base = "模型请求在派发前被任务预算检查拦截；所需的保守预算预留超过任务剩余额度。"
	expected := "此前有10 次网页搜索和 3 次网页读取调用记录为成功；这只能说明调用完成，不能证明网页信息已核实或答复已完成。后续" + base
	diagnosis := TaskTerminalDeliveryRecoveryDiagnosisFromEvent(event, execution)
	if diagnosis != expected || !validTerminalDeliveryRecoveryDiagnosis(diagnosis) {
		t.Fatalf("budget diagnosis with execution evidence = %q, want %q", diagnosis, expected)
	}

	eventDigest := sha256.Sum256(event)
	objectiveDigest := sha256.Sum256([]byte("看看今天有什么新闻"))
	facts := TaskTerminalDeliverySourceFacts{
		Identity: TaskTerminalDeliveryIdentity{
			DeliveryID: 42, TaskID: "q1234567", OwnerID: "u1234567", ConversationID: "c1234567", InitiatorID: "u1234567",
			TaskRevision: 2, AuthorizationVersion: 1, SourceKind: TaskTerminalDeliverySourceWaiting,
			SourceEventSequence: 9, SourceEventType: "task.waiting_user", SourceSHA256: eventDigest[:],
		},
		TaskStatus: "waiting_user", ModelID: "p1234567", ObjectiveSummary: "看看今天有什么新闻", ObjectiveSHA256: objectiveDigest[:],
		WaitingReason: "external_dependency", RecoveryDiagnosis: diagnosis, ReadOnlyToolExecution: execution,
	}
	factsDigest, err := TaskTerminalDeliveryFactsDigest(facts)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedFacts := facts
	mismatchedFacts.ReadOnlyToolExecution.SuccessfulWebSearchCalls++
	if _, err := TaskTerminalDeliveryFactsDigest(mismatchedFacts); err == nil {
		t.Fatal("facts accepted execution counts that do not match the recovery diagnosis")
	}
	facts.FactsSHA256 = factsDigest
	request, err := BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
	if err != nil {
		t.Fatalf("build budget explanation request: %v", err)
	}
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &decoded); err != nil || len(decoded.Messages) != 2 {
		t.Fatalf("decode budget explanation request: messages=%+v err=%v", decoded.Messages, err)
	}
	var modelFacts struct {
		RecoveryDiagnosis     string `json:"recovery_diagnosis"`
		ReadOnlyToolExecution struct {
			SuccessfulWebSearchCalls int64 `json:"successful_web_search_calls"`
			SuccessfulWebReadCalls   int64 `json:"successful_web_read_calls"`
		} `json:"read_only_tool_execution"`
	}
	if err := json.Unmarshal([]byte(decoded.Messages[1].Content[0].Text), &modelFacts); err != nil {
		t.Fatalf("decode safe execution facts: %v", err)
	}
	if modelFacts.RecoveryDiagnosis != expected || modelFacts.ReadOnlyToolExecution.SuccessfulWebSearchCalls != 10 ||
		modelFacts.ReadOnlyToolExecution.SuccessfulWebReadCalls != 3 {
		t.Fatalf("model did not receive bounded execution facts: %+v", modelFacts)
	}
	for _, privateValue := range []string{"morde_web_search", "morde_web_read", "private query", "https://private.example/news",
		"private result", "call_private", facts.Identity.TaskID, facts.Identity.ConversationID} {
		if strings.Contains(string(request), privateValue) {
			t.Fatalf("terminal explanation request exposed private value %q", privateValue)
		}
	}
	if !strings.Contains(decoded.Messages[0].Content[0].Text, "Never claim that news results were verified") {
		t.Fatal("model instructions did not explicitly forbid claiming that news results were verified")
	}
}
