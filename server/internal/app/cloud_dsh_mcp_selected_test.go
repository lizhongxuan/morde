package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func (s *cloudDSHMCPStoreStub) ListSelectedTaskMCPConnectors(_ context.Context, claimed task.Task) ([]string, error) {
	if claimed.ID != s.details.Task.ID || claimed.OwnerID != s.details.Task.OwnerID {
		return nil, domain.NewError(domain.CodeUnauthenticated, "task changed")
	}
	ids := make([]string, 0, len(s.grants))
	for _, grant := range s.grants {
		if grant.RevokedAt == nil {
			ids = append(ids, grant.ConnectorID)
		}
	}
	return ids, nil
}

func (s *cloudDSHMCPStoreStub) GrantSelectedTaskMCPConnector(_ context.Context, claimed task.Task, connectorID string) (TaskMCPConnectorGrant, error) {
	if claimed.ID != s.details.Task.ID || claimed.OwnerID != s.details.Task.OwnerID {
		return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeUnauthenticated, "task changed")
	}
	for _, grant := range s.grants {
		if grant.ConnectorID == connectorID && grant.RevokedAt == nil {
			return grant, nil
		}
	}
	return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "connector is not selected")
}

func (s *cloudDSHMCPStoreStub) PinSelectedTaskMCPSchema(ctx context.Context, claimed task.Task, connectorID string,
	version int64, schema, digest []byte) (TaskMCPConnectorGrant, error) {
	grant, err := s.GrantSelectedTaskMCPConnector(ctx, claimed, connectorID)
	if err != nil {
		return TaskMCPConnectorGrant{}, err
	}
	if claimed.ProjectID != "" {
		if err := s.AuthorizeProjectMCPConnector(ctx, claimed.ProjectID, taskActorID(claimed), grant.ConnectorOwnerID, connectorID, grant.ProjectGrantVersion); err != nil {
			return TaskMCPConnectorGrant{}, err
		}
	}
	return s.SetTaskMCPConnectorToolSchema(ctx, claimed.OwnerID, claimed.ID, connectorID, version, schema, digest)
}

func discoverSelectedMCPForTest(t *testing.T, broker *CloudDSHToolBroker, claimed task.Task, connectorID string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"connector_id": connectorID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "test-selected-discover", Name: cloudDSHMCPDiscoverToolName, Arguments: string(raw)})
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("discover selected MCP connector: result=%+v err=%v", result, err)
	}
}

func selectedMCPCallForTest(t *testing.T, callID, connectorID, toolName, arguments string) ModelToolCall {
	t.Helper()
	raw, err := json.Marshal(struct {
		ConnectorID string          `json:"connector_id"`
		ToolName    string          `json:"tool_name"`
		Arguments   json.RawMessage `json:"arguments"`
	}{ConnectorID: connectorID, ToolName: toolName, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return ModelToolCall{CallID: callID, Name: cloudDSHMCPCallToolName, Arguments: string(raw)}
}

func TestSelectedMCPSchemaUsesOnlyLocalReferences(t *testing.T) {
	for _, tc := range []struct {
		name      string
		schema    string
		arguments string
		allowed   bool
	}{
		{name: "local fragment", schema: `{"type":"object","properties":{"query":{"$ref":"#/$defs/query"}},"required":["query"],"$defs":{"query":{"type":"string","minLength":1}},"additionalProperties":false}`, arguments: `{"query":"hello"}`, allowed: true},
		{name: "invalid arguments", schema: `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`, arguments: `{"query":4}`},
		{name: "file URI", schema: `{"type":"object","properties":{"query":{"$ref":"file:///tmp/secret.json"}}}`, arguments: `{}`},
		{name: "absolute path", schema: `{"type":"object","properties":{"query":{"$ref":"/tmp/secret.json"}}}`, arguments: `{}`},
		{name: "relative path", schema: `{"type":"object","properties":{"query":{"$ref":"../secret.json"}}}`, arguments: `{}`},
		{name: "network URI", schema: `{"type":"object","properties":{"query":{"$ref":"https://example.invalid/schema"}}}`, arguments: `{}`},
		{name: "dynamic external reference", schema: `{"type":"object","$dynamicRef":"file:///tmp/secret.json"}`, arguments: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSelectedMCPArguments([]byte(tc.schema), []byte(tc.arguments))
			if (err == nil) != tc.allowed {
				t.Fatalf("schema accepted=%t want=%t err=%v", err == nil, tc.allowed, err)
			}
		})
	}
}

func TestSelectedMCPCatalogRejectsControlCharactersInToolName(t *testing.T) {
	_, _, err := canonicalMCPToolSchema([]MCPConnectorToolDefinition{{Name: "unsafe\nname", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	if err == nil {
		t.Fatal("MCP tool with a control character was accepted")
	}
}

type failingMCPDiscoveryClient struct{ calls int }

func (c *failingMCPDiscoveryClient) ListTools(context.Context, string, http.Header) ([]MCPConnectorToolDefinition, error) {
	c.calls++
	return nil, context.DeadlineExceeded
}

func (*failingMCPDiscoveryClient) CallTool(context.Context, string, http.Header, string, json.RawMessage) (MCPConnectorToolResult, error) {
	return MCPConnectorToolResult{}, errors.New("unexpected MCP call")
}

func selectedMCPTestFixture(t *testing.T, client MCPConnectorToolClient) (*CloudDSHToolBroker, *cloudDSHMCPStoreStub, task.Task) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.ExecutionLocation = "server"
	value := fixture.store.details.Task
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			"k1234567": {ID: "k1234567", OwnerID: value.OwnerID, Name: "Synthetic", EndpointURL: "http://127.0.0.1/synthetic", Enabled: true, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: value.ID, OwnerID: value.OwnerID, ConnectorOwnerID: value.OwnerID,
			ConnectorID: "k1234567", ConnectorRevision: 1, GrantVersion: 1}},
	}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	return broker, store, value
}

func TestSelectedMCPDiscoveryIsLazyAndChecksToolSchema(t *testing.T) {
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", Description: "Find a synthetic record",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		result: MCPConnectorToolResult{Text: []string{"synthetic result"}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || client.listCalls != 0 || client.callCalls != 0 {
		t.Fatalf("tool setup contacted MCP: definitions=%d list=%d call=%d err=%v", len(definitions), client.listCalls, client.callCalls, err)
	}
	if _, err := validateAgentTools(definitions); err != nil {
		t.Fatalf("fixed MCP tools are invalid: %v", err)
	}
	list, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-list-1", Name: cloudDSHMCPDiscoverToolName, Arguments: `{}`})
	if err != nil || list.Status != AgentToolSucceeded || client.listCalls != 0 {
		t.Fatalf("listing selected connectors contacted MCP: result=%+v calls=%d err=%v", list, client.listCalls, err)
	}
	foreign, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-foreign-1", Name: cloudDSHMCPDiscoverToolName, Arguments: `{"connector_id":"k7654321"}`})
	if err != nil || foreign.Status != AgentToolFailed || client.listCalls != 0 {
		t.Fatalf("unselected connector reached MCP: result=%+v calls=%d err=%v", foreign, client.listCalls, err)
	}
	discovery, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-discover-1", Name: cloudDSHMCPDiscoverToolName, Arguments: `{"connector_id":"k1234567"}`})
	if err != nil || discovery.Status != AgentToolSucceeded || client.listCalls != 1 || len(store.grants[0].ToolSchema) == 0 {
		t.Fatalf("selected discovery = %+v calls=%d err=%v", discovery, client.listCalls, err)
	}
	for _, arguments := range []string{
		`{"connector_id":"k1234567","tool_name":"unknown","arguments":{}}`,
		`{"connector_id":"k1234567","tool_name":"lookup","arguments":{"query":3}}`,
		`{"connector_id":"k7654321","tool_name":"lookup","arguments":{"query":"no"}}`,
	} {
		result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-invalid-1", Name: cloudDSHMCPCallToolName, Arguments: arguments})
		if err != nil || result.Status != AgentToolFailed || client.callCalls != 0 || len(store.operations) != 0 {
			t.Fatalf("invalid selected call reached MCP: result=%+v calls=%d err=%v", result, client.callCalls, err)
		}
	}
	call := ModelToolCall{CallID: "selected-call-1", Name: cloudDSHMCPCallToolName,
		Arguments: `{"connector_id":"k1234567","tool_name":"lookup","arguments":{"query":"local"}}`}
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("selected call bypassed approval: result=%+v calls=%d err=%v", approval, client.callCalls, err)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "lookup" {
		t.Fatalf("approved selected call = %+v calls=%d err=%v", result, client.callCalls, err)
	}
}

func TestSelectedMCPDiscoveryFailureDoesNotBlockTask(t *testing.T) {
	client := &failingMCPDiscoveryClient{}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-offline-1", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"k1234567"}`})
	if err != nil || result.Status != AgentToolFailed || client.calls != 1 || store.details.Task.Status != task.StatusRunning {
		t.Fatalf("offline MCP blocked the task: result=%+v task=%s calls=%d err=%v", result, store.details.Task.Status, client.calls, err)
	}
	if principal := (identity.Principal{Kind: identity.Kind(claimed.Authorization.Kind), Scopes: agentScopeSnapshot(claimed.Authorization.Scopes)}); !principal.HasScope(identity.ScopeMCPControl) {
		t.Fatal("test requires an MCP-authorized task")
	}
}

func TestSelectedMCPRevocationDuringDiscoveryDoesNotExposeCatalog(t *testing.T) {
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	client.onList = func() {
		revoked := time.Now()
		store.grants[0].RevokedAt = &revoked
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "selected-revoke-1", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"k1234567"}`})
	if err != nil || result.Status != AgentToolFailed || len(store.grants[0].ToolSchema) != 0 || client.callCalls != 0 {
		t.Fatalf("revoked discovery exposed a catalog: result=%+v calls=%d err=%v", result, client.callCalls, err)
	}
}
