package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
