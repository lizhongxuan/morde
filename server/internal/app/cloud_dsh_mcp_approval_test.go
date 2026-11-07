package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain/task"
)

func TestCloudDSHMCPExternalCallWaitsForExactApproval(t *testing.T) {
	_, store, client, broker, claimed, aliases := newCloudDSHMCPApprovalFixture(t)
	arguments := `{"to":"demo@example.test","body":"synthetic approval test","api_token":"synthetic-test-token"}`
	call := selectedMCPCallForTest(t, "mcp-approval-call-1", "k1234567", "send_email", arguments)
	first, err := broker.Execute(context.Background(), claimed, call)
	pending := store.details.PendingRequest
	if err != nil || first.Status != AgentToolNeedsUser || pending == nil || pending.Status != task.PendingRequestPending ||
		pending.TaskID != claimed.ID || pending.CallID != call.CallID || pending.ToolName != aliases["send_email"] || len(pending.RequestHash) != 32 ||
		client.callCalls != 0 || len(store.operations) != 0 || store.details.Task.Usage.ToolCalls != 0 || store.details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("external call before approval = %#v err=%v pending=%#v calls=%d operations=%d task=%#v",
			first, err, pending, client.callCalls, len(store.operations), store.details.Task)
	}
	if !strings.Contains(pending.Object, "Research") || !strings.Contains(pending.Object, "k1234567") ||
		!strings.Contains(pending.Content, "send_email") || !strings.Contains(pending.Content, "demo@example.test") ||
		!strings.Contains(pending.Content, "synthetic approval test") || !strings.Contains(pending.Content, "Request fingerprint:") ||
		!strings.Contains(pending.Content, hex.EncodeToString(pending.RequestHash)) ||
		strings.Contains(pending.Content, "synthetic-test-token") || !strings.Contains(pending.Content, "[redacted]") {
		t.Fatalf("approval prompt is not precise and safely redacted: object=%q content=%q impact=%q", pending.Object, pending.Content, pending.Impact)
	}
	fingerprint := append([]byte(nil), pending.RequestHash...)
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task

	approved, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approved.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "send_email" ||
		!bytes.Equal(client.lastRequest, []byte(arguments)) || store.details.Task.Usage.ToolCalls != 1 {
		t.Fatalf("approved exact external call = %#v err=%v calls=%d tool=%q request=%s usage=%d",
			approved, err, client.callCalls, client.lastTool, client.lastRequest, store.details.Task.Usage.ToolCalls)
	}
	replayed, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || replayed.Status != AgentToolSucceeded || client.callCalls != 1 {
		t.Fatalf("exact approved replay = %#v err=%v remote calls=%d", replayed, err, client.callCalls)
	}
	if !bytes.Equal(fingerprint, pending.RequestHash) {
		t.Fatal("approval request hash changed after dispatch")
	}
}

func TestCloudDSHMCPApprovalDoesNotTransferToChangedArgumentsOrTool(t *testing.T) {
	for _, test := range []struct {
		name        string
		change      func(ModelToolCall, map[string]string) ModelToolCall
		beforeRetry func(*cloudDSHMCPStoreStub)
	}{
		{
			name: "changed arguments",
			change: func(call ModelToolCall, _ map[string]string) ModelToolCall {
				var request selectedMCPRequest
				_ = json.Unmarshal([]byte(call.Arguments), &request)
				request.Arguments = json.RawMessage(`{"to":"other@example.test","body":"different synthetic payload"}`)
				encoded, _ := json.Marshal(request)
				call.Arguments = string(encoded)
				return call
			},
		},
		{
			name: "changed remote tool",
			change: func(call ModelToolCall, _ map[string]string) ModelToolCall {
				var request selectedMCPRequest
				_ = json.Unmarshal([]byte(call.Arguments), &request)
				request.ToolName, request.Arguments = "create_event", json.RawMessage(`{"title":"synthetic event"}`)
				encoded, _ := json.Marshal(request)
				call.Arguments = string(encoded)
				return call
			},
		},
		{
			name: "changed connector grant version",
			beforeRetry: func(store *cloudDSHMCPStoreStub) {
				store.grants[0].GrantVersion++
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, client, broker, claimed, aliases := newCloudDSHMCPApprovalFixture(t)
			call := selectedMCPCallForTest(t, "mcp-approval-change-1", "k1234567", "send_email", `{"to":"demo@example.test","body":"synthetic approval test"}`)
			first, err := broker.Execute(context.Background(), claimed, call)
			if err != nil || first.Status != AgentToolNeedsUser {
				t.Fatalf("initial approval request = %#v err=%v", first, err)
			}
			originalHash := append([]byte(nil), store.details.PendingRequest.RequestHash...)
			resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
			claimed = store.details.Task

			changed := call
			if test.change != nil {
				changed = test.change(call, aliases)
			}
			if test.beforeRetry != nil {
				test.beforeRetry(store)
			}
			result, err := broker.Execute(context.Background(), claimed, changed)
			pending := store.details.PendingRequest
			var request selectedMCPRequest
			if err := json.Unmarshal([]byte(changed.Arguments), &request); err != nil {
				t.Fatal(err)
			}
			if err != nil || result.Status != AgentToolNeedsUser || pending == nil || pending.Status != task.PendingRequestPending ||
				pending.Version != 2 || pending.ToolName != aliases[request.ToolName] || bytes.Equal(pending.RequestHash, originalHash) ||
				client.callCalls != 0 || len(store.operations) != 0 {
				t.Fatalf("changed call reused approval or dispatched: result=%#v err=%v pending=%#v calls=%d operations=%d",
					result, err, pending, client.callCalls, len(store.operations))
			}
		})
	}
}

func TestCloudDSHMCPRejectedExternalCallNeverDispatches(t *testing.T) {
	_, store, client, broker, claimed, _ := newCloudDSHMCPApprovalFixture(t)
	call := selectedMCPCallForTest(t, "mcp-approval-reject-1", "k1234567", "send_email", `{"to":"demo@example.test","body":"synthetic rejection test"}`)
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("initial approval request = %#v err=%v calls=%d", first, err, client.callCalls)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionReject)
	claimed = store.details.Task

	rejected, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || rejected.Status != AgentToolFailed || client.callCalls != 0 || len(store.operations) != 0 {
		t.Fatalf("rejected external call = %#v err=%v calls=%d operations=%d", rejected, err, client.callCalls, len(store.operations))
	}
}

func TestCloudDSHMCPTradeApprovalShowsItemAndExactAmountBeforeDispatch(t *testing.T) {
	_, store, client, broker, claimed, _ := newCloudDSHMCPApprovalFixture(t)
	arguments, err := json.Marshal(map[string]any{
		"aaa_note":          strings.Repeat("unrelated details ", 40),
		"project":           "Summer trip",
		"merchant":          "Example Hotel",
		"item":              "Two-night stay, 12-14 Nov",
		"quantity":          1,
		"amount":            349,
		"currency":          "CNY",
		"payment_token":     "synthetic-payment-token",
		"payment_method_id": "pm_synthetic_secret_42",
		"card_number":       "4111111111111111",
		"cvv":               "987654321",
		"card":              map[string]any{"number": "4000000000000002", "security_code": "876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := selectedMCPCallForTest(t, "mcp-trade-confirmation-1", "k1234567", "create_booking", string(arguments))
	result, err := broker.Execute(context.Background(), claimed, call)
	pending := store.details.PendingRequest
	if err != nil || result.Status != AgentToolNeedsUser || pending == nil || pending.Status != task.PendingRequestPending ||
		client.callCalls != 0 || len(store.operations) != 0 || store.details.Task.Usage.ToolCalls != 0 {
		t.Fatalf("trade request ran before approval: result=%#v err=%v pending=%#v calls=%d operations=%d usage=%d",
			result, err, pending, client.callCalls, len(store.operations), store.details.Task.Usage.ToolCalls)
	}
	for _, expected := range []string{`"project":"Summer trip"`, "Example Hotel", "Two-night stay, 12-14 Nov",
		`"amount":349`, `"currency":"CNY"`, "[redacted]"} {
		if !strings.Contains(pending.Content, expected) {
			t.Errorf("trade approval did not show %q: %q", expected, pending.Content)
		}
	}
	for _, secret := range []string{"synthetic-payment-token", "pm_synthetic_secret_42", "4111111111111111",
		"987654321", "4000000000000002", "876543"} {
		if strings.Contains(pending.Content, secret) {
			t.Errorf("trade approval exposed sensitive payment data %q", secret)
		}
	}
	if len([]rune(pending.Content)) > 512 {
		t.Fatalf("approval content exceeds the persisted limit: %d", len([]rune(pending.Content)))
	}
	fingerprint := append([]byte(nil), pending.RequestHash...)
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	approved, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || approved.Status != AgentToolSucceeded || client.callCalls != 1 || !bytes.Equal(client.lastRequest, arguments) {
		t.Fatalf("approved trade call changed the exact request: result=%#v err=%v calls=%d request=%s",
			approved, err, client.callCalls, client.lastRequest)
	}
	if !bytes.Equal(fingerprint, pending.RequestHash) {
		t.Fatal("trade approval request hash changed after dispatch")
	}
	if client.callCalls != 1 || len(store.operations) != 1 {
		t.Fatalf("approved trade dispatch count = %d, operations = %d", client.callCalls, len(store.operations))
	}
}

func TestCloudDSHMCPApprovalPreviewKeepsKeyFieldsBeforeLongOtherArguments(t *testing.T) {
	arguments := json.RawMessage(`{"aaa_note":"` + strings.Repeat("unrelated ", 50) + `","project":"Local trip","item":"Train ticket","amount":123,"currency":"CNY"}`)
	preview := cloudDSHMCPApprovalArgumentPreview(arguments)
	for _, expected := range []string{`"project":"Local trip"`, `"item":"Train ticket"`, `"amount":123`, `"currency":"CNY"`} {
		if !strings.Contains(preview, expected) {
			t.Errorf("approval preview omitted key field %q: %s", expected, preview)
		}
	}
	if firstKeyField, firstOtherArgument := strings.Index(preview, `"amount":123`), strings.Index(preview, `"aaa_note"`); firstKeyField < 0 || firstOtherArgument < 0 || firstKeyField > firstOtherArgument {
		t.Fatalf("long unrelated arguments appeared before key fields: %s", preview)
	}
	if utf8.RuneCountInString(preview) > 240 {
		t.Fatalf("approval argument preview exceeds its limit: %d", utf8.RuneCountInString(preview))
	}
}

func TestCloudDSHMCPApprovalPreviewRedactsCardAndPaymentSecrets(t *testing.T) {
	preview := cloudDSHMCPApprovalArgumentPreview(json.RawMessage(`{"card_number":"4111111111111111","cvv":"987654321","payment_method_id":"pm_synthetic_secret_42","card":{"number":"4000000000000002","cvc":"876543"},"amount":349,"currency":"CNY"}`))
	for _, secret := range []string{"4111111111111111", "987654321", "pm_synthetic_secret_42", "4000000000000002", "876543"} {
		if strings.Contains(preview, secret) {
			t.Errorf("approval preview exposed payment data %q: %s", secret, preview)
		}
	}
	for _, expected := range []string{`"amount":349`, `"currency":"CNY"`, "[redacted]"} {
		if !strings.Contains(preview, expected) {
			t.Errorf("approval preview omitted %q: %s", expected, preview)
		}
	}
	if len([]rune(preview)) > 240 {
		t.Fatalf("approval argument preview exceeds its limit: %d", len([]rune(preview)))
	}
}

func newCloudDSHMCPApprovalFixture(t *testing.T) (*runtimeToolExecutorFixture, *cloudDSHMCPStoreStub,
	*cloudDSHMCPClientStub, *CloudDSHToolBroker, task.Task, map[string]string) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k1234567"
	ownerID := fixture.store.details.Task.OwnerID
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: ownerID, Name: "Research", EndpointURL: "https://connector.example.test/mcp", Enabled: true, CredentialVersion: 1, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{
			TaskID: fixture.store.details.Task.ID, OwnerID: ownerID, ConnectorOwnerID: ownerID,
			ConnectorID: connectorID, ConnectorRevision: 1, CredentialVersion: 1, GrantVersion: 1,
		}},
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{
		result: MCPConnectorToolResult{Text: []string{"synthetic remote stub success"}},
		tools: []MCPConnectorToolDefinition{
			{
				Name: "send_email", Description: "Send an email",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"body":{"type":"string"},"api_token":{"type":"string"}},"required":["to","body"],"additionalProperties":false}`),
			},
			{
				Name: "create_event", Description: "Create a calendar event",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`),
			},
			{
				Name: "create_booking", Description: "Create a booking",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"aaa_note":{"type":"string"},"project":{"type":"string"},"merchant":{"type":"string"},"item":{"type":"string"},"quantity":{"type":"integer"},"amount":{"type":"integer"},"currency":{"type":"string"},"payment_token":{"type":"string"},"payment_method_id":{"type":"string"},"card_number":{"type":"string"},"cvv":{"type":"string"},"card":{"type":"object","properties":{"number":{"type":"string"},"security_code":{"type":"string"}},"additionalProperties":false}},"required":["merchant","item","quantity","amount","currency"],"additionalProperties":false}`),
			},
		},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	aliases := make(map[string]string, len(client.tools))
	for _, tool := range client.tools {
		aliases[tool.Name] = cloudDSHMCPToolName(connectorID, tool.Name)
	}
	return fixture, store, client, broker, claimed, aliases
}

func resumeCloudDSHMCPApproval(t *testing.T, store *cloudDSHMCPStoreStub, decision task.PendingDecision) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	pending := store.details.PendingRequest
	if pending == nil || pending.Kind != "approval" || pending.Status != task.PendingRequestPending {
		t.Fatalf("no pending MCP approval to respond to: %#v", pending)
	}
	switch decision {
	case task.PendingDecisionApprove:
		pending.Status = task.PendingRequestApproved
	case task.PendingDecisionReject:
		pending.Status = task.PendingRequestRejected
	default:
		t.Fatalf("invalid synthetic approval decision: %q", decision)
	}
	pending.Decision = decision
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	expires := time.Now().Add(time.Minute).UTC()
	store.details.Task.Lease = task.Lease{Owner: "mcp-approval-worker", Version: pending.Version + 1, ExpiresAt: &expires}
}
