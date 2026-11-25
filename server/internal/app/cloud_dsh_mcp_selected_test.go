package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestSelectedMCPCanCallToolBeyondModelDefinitionLimit(t *testing.T) {
	tools := make([]MCPConnectorToolDefinition, 65)
	for index := range tools {
		tools[index] = MCPConnectorToolDefinition{
			Name:        fmt.Sprintf("lookup_%02d", index),
			Description: "Find a synthetic record",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}
	}
	client := &cloudDSHMCPClientStub{tools: tools, result: MCPConnectorToolResult{Text: []string{"synthetic result"}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)

	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || len(definitions) > maxCloudDSHToolDefinitions {
		t.Fatalf("bounded model tool definitions = %d, err=%v", len(definitions), err)
	}
	if _, err := validateAgentTools(definitions); err != nil {
		t.Fatalf("model tool definitions are invalid: %v", err)
	}
	mcpDefinitionCount := 0
	for _, definition := range definitions {
		if definition.Name == cloudDSHMCPDiscoverToolName || definition.Name == cloudDSHMCPCallToolName {
			mcpDefinitionCount++
		}
	}
	if mcpDefinitionCount != 2 {
		t.Fatalf("65-item catalog changed the fixed MCP definitions: count=%d definitions=%+v", mcpDefinitionCount, definitions)
	}
	discoverSelectedMCPForTest(t, broker, claimed, "k1234567")
	var pinned []MCPConnectorToolDefinition
	if err := json.Unmarshal(store.grants[0].ToolSchema, &pinned); err != nil || len(pinned) != 65 || pinned[64].Name != "lookup_64" {
		t.Fatalf("65-item MCP catalog was not pinned completely: count=%d err=%v", len(pinned), err)
	}

	call := selectedMCPCallForTest(t, "selected-65th-tool", "k1234567", "lookup_64", `{"query":"local"}`)
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("65th MCP tool bypassed approval or failed to prepare: result=%+v calls=%d err=%v", result, client.callCalls, err)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	result, err = broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || result.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "lookup_64" {
		t.Fatalf("approved 65th MCP tool was not dispatched exactly once: result=%+v calls=%d tool=%q err=%v", result, client.callCalls, client.lastTool, err)
	}
}

func TestSelectedMCPActivityUsesResolvedToolAndReusesCallIdentity(t *testing.T) {
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", Description: "Find a synthetic record",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		result: MCPConnectorToolResult{Text: []string{"synthetic result"}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	discoverSelectedMCPForTest(t, broker, claimed, "k1234567")
	current := store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-mcp", 1, "session-root"
	revision := int64(5)
	current.CurrentRevision, current.DesiredRevision = &revision, &revision
	current.Authorization.Version = 2
	store.details.Task = current
	claimed = current
	call := selectedMCPCallForTest(t, "selected-activity-1", "k1234567", "lookup", `{"query":"local"}`)
	call.TaskFence = &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
		AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version}

	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolNeedsUser || len(store.details.Steps) != 1 {
		t.Fatalf("selected MCP activity = %+v, err=%v, steps=%+v", result, err, store.details.Steps)
	}
	original := store.details.Steps[0]
	if original.AgentSessionID != current.AgentSessionID || original.ToolCallKey != call.CallID || original.ToolName != cloudDSHMCPCallToolName ||
		original.Title != "Run cloud dsh MCP tool: lookup" || original.TaskRevision == nil || *original.TaskRevision != revision ||
		original.Status != task.StepWaitingUser {
		t.Fatalf("selected MCP activity did not capture the resolved action: %+v", original)
	}
	expires := time.Now().Add(time.Minute).UTC()
	store.mu.Lock()
	resumed := store.details.Task
	resumed.Status, resumed.WaitingReason = task.StatusRunning, ""
	resumed.Lease = task.Lease{Owner: resumed.Lease.Owner, Version: resumed.Lease.Version + 1, ExpiresAt: &expires}
	store.details.Task = resumed
	store.mu.Unlock()
	claimed = resumed
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = resumed.Lease.Owner, resumed.Lease.Version
	repeated, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || repeated.Status != AgentToolNeedsUser || len(store.details.Steps) != 1 ||
		store.details.Steps[0].ID != original.ID || store.details.Steps[0].Title != original.Title {
		t.Fatalf("repeated MCP approval created a different activity: result=%+v err=%v steps=%+v", repeated, err, store.details.Steps)
	}

	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = store.details.Task.Lease.Owner, store.details.Task.Lease.Version
	result, err = broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || len(store.details.Steps) != 1 || client.callCalls != 1 {
		t.Fatalf("approved selected MCP activity = %+v, err=%v, steps=%+v, calls=%d", result, err, store.details.Steps, client.callCalls)
	}
	settled := store.details.Steps[0]
	if settled.ID != original.ID || settled.Sequence != original.Sequence || settled.ToolCallKey != call.CallID ||
		settled.ToolName != cloudDSHMCPCallToolName || settled.Title != "Run cloud dsh MCP tool: lookup" || settled.Status != task.StepSucceeded {
		t.Fatalf("selected MCP replay created a different activity: original=%+v settled=%+v", original, settled)
	}
}

func TestSelectedMCPPlanSelectorsBindAdmissionAndStayOutOfBusinessArguments(t *testing.T) {
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", Description: "Find a synthetic record",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		result: MCPConnectorToolResult{Text: []string{"synthetic result"}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	discoverSelectedMCPForTest(t, broker, claimed, "k1234567")
	current := store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-mcp", 1, "session-root"
	revision := int64(5)
	current.CurrentRevision, current.DesiredRevision = &revision, &revision
	current.Authorization.Version = 2
	plan := task.Plan{TaskRevision: revision, Revision: 3, Goal: "Find sources", Scope: "Search for sources",
		VerificationMethod: "Inspect returned sources", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "sources", Title: "Search sources", Status: task.PlanStepPending,
			CompletionCriteria: []string{"sources found"}}}}
	store.details.Task, store.details.Plan = current, &plan
	claimed = current

	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	var callSchema AgentToolDefinition
	for _, definition := range definitions {
		if definition.Name == cloudDSHMCPCallToolName {
			callSchema = definition
			break
		}
	}
	if len(callSchema.Parameters) == 0 || !bytes.Contains(callSchema.Parameters, []byte(cloudDSHPlanStepIDArgumentName)) ||
		!bytes.Contains(callSchema.Parameters, []byte(cloudDSHPlanRevisionArgumentName)) {
		t.Fatalf("MCP call schema omitted plan selectors: %s", callSchema.Parameters)
	}

	call := selectedMCPCallForTest(t, "selected-plan-call", "k1234567", "lookup",
		`{"query":"local","morde_plan_step_id":"nested-step","morde_plan_revision":99}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Arguments), &fields); err != nil {
		t.Fatal(err)
	}
	fields[cloudDSHPlanStepIDArgumentName] = json.RawMessage(`"sources"`)
	fields[cloudDSHPlanRevisionArgumentName] = json.RawMessage(`3`)
	encodedCall, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	call.Arguments = string(encodedCall)
	call.TaskFence = &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
		AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version}

	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("selected MCP approval = %+v, calls=%d err=%v", approval, client.callCalls, err)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = claimed.Lease.Owner, claimed.Lease.Version
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || client.callCalls != 1 {
		t.Fatalf("selected MCP dispatch = %+v, calls=%d err=%v", result, client.callCalls, err)
	}
	if store.lastDispatched.PlanRevision != 3 || store.lastDispatched.PlanStepID != "sources" {
		t.Fatalf("MCP dispatch did not receive the top-level plan selector: revision=%d step=%q",
			store.lastDispatched.PlanRevision, store.lastDispatched.PlanStepID)
	}
	var business map[string]json.RawMessage
	if err := json.Unmarshal(client.lastRequest, &business); err != nil || len(business) != 1 ||
		string(business["query"]) != `"local"` || hasCloudDSHPlanStepMetadata(string(client.lastRequest)) {
		t.Fatalf("MCP business arguments contain plan selectors or changed: %s err=%v", client.lastRequest, err)
	}
	step := store.details.Steps[0]
	if step.ToolName != cloudDSHMCPToolName("k1234567", "lookup") || step.Status != task.StepSucceeded {
		t.Fatalf("plan-bound TaskStep does not match the admitted MCP operation: %+v", step)
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

func TestGitHubMCPReadOnlyProfileRestrictsCatalogAndExecution(t *testing.T) {
	client := &recordingSelectedMCPClient{cloudDSHMCPClientStub: &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{
		{Name: "get_me", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
		{Name: "search_code", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)},
		{Name: "create_issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`)},
	}}}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	connector := store.values["k1234567"]
	connector.EndpointURL = "https://api.githubcopilot.com/mcp/"
	store.values["k1234567"] = connector

	discovery, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "github-discover", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"k1234567"}`})
	if err != nil || discovery.Status != AgentToolSucceeded || client.listCalls != 1 {
		t.Fatalf("GitHub read-only discovery = %+v err=%v calls=%d", discovery, err, client.listCalls)
	}
	var catalog struct {
		Tools []MCPConnectorToolDefinition `json:"tools"`
	}
	if err := json.Unmarshal([]byte(discovery.Output), &catalog); err != nil || len(catalog.Tools) != 2 ||
		catalog.Tools[0].Name != "get_me" || catalog.Tools[1].Name != "search_code" {
		t.Fatalf("GitHub discovered catalog = %+v err=%v", catalog, err)
	}
	if client.listHeaders.Get(githubMCPReadonlyHeader) != "true" || client.listHeaders.Get(githubMCPToolsHeader) != githubMCPReadonlyTools {
		t.Fatalf("GitHub discovery did not request the restricted upstream profile: headers=%v", client.listHeaders)
	}

	// Simulate an old or corrupted pinned catalog. The execution path must still
	// reject a write tool before creating an operation or contacting the server.
	writeSchema, _, err := canonicalMCPToolSchema([]MCPConnectorToolDefinition{{Name: "create_issue",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`)}})
	if err != nil {
		t.Fatal(err)
	}
	store.grants[0].ToolSchema = writeSchema
	store.grants[0].ToolSchemaSHA256 = sha256Bytes(writeSchema)
	write, err := broker.Execute(context.Background(), claimed, selectedMCPCallForTest(t, "github-write", "k1234567", "create_issue", `{"title":"no"}`))
	if err != nil || write.Status != AgentToolFailed || client.callCalls != 0 || len(store.operations) != 0 {
		t.Fatalf("stale GitHub write tool was not rejected: result=%+v err=%v remote_calls=%d operations=%d",
			write, err, client.callCalls, len(store.operations))
	}

	readSchema, _, err := canonicalMCPToolSchema([]MCPConnectorToolDefinition{{Name: "search_code",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}})
	if err != nil {
		t.Fatal(err)
	}
	store.grants[0].ToolSchema = readSchema
	store.grants[0].ToolSchemaSHA256 = sha256Bytes(readSchema)
	readCall := selectedMCPCallForTest(t, "github-read", "k1234567", "search_code", `{"query":"repo:acme/demo bug"}`)
	pending, err := broker.Execute(context.Background(), claimed, readCall)
	if err != nil || pending.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("GitHub read call bypassed approval: result=%+v err=%v remote_calls=%d", pending, err, client.callCalls)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	called, err := broker.Execute(context.Background(), store.details.Task, readCall)
	if err != nil || called.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "search_code" {
		t.Fatalf("approved GitHub read call = %+v err=%v remote_calls=%d tool=%q", called, err, client.callCalls, client.lastTool)
	}
	if client.callHeaders.Get(githubMCPReadonlyHeader) != "true" || client.callHeaders.Get(githubMCPToolsHeader) != githubMCPReadonlyTools {
		t.Fatalf("GitHub tool call did not request the restricted upstream profile: headers=%v", client.callHeaders)
	}
}

func TestGitHubMCPReadOnlyProfileRequiresExactEndpoint(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		github   bool
	}{
		{endpoint: "https://api.githubcopilot.com/mcp/", github: true},
		{endpoint: "https://api.githubcopilot.com:443/mcp/", github: true},
		{endpoint: "https://api.githubcopilot.com./mcp/", github: true},
		{endpoint: "https://api.githubcopilot.com/mcp/x/all/readonly", github: true},
		{endpoint: "https://api.githubcopilot.com.evil.invalid/mcp/"},
		{endpoint: "http://api.githubcopilot.com/mcp/"},
		{endpoint: "https://api.githubcopilot.com:444/mcp/"},
		{endpoint: "https://api.githubcopilot.com/mcp/?toolset=all"},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			if got := isGitHubMCPRemoteEndpoint(test.endpoint); got != test.github {
				t.Fatalf("isGitHubMCPRemoteEndpoint(%q) = %t, want %t", test.endpoint, got, test.github)
			}
		})
	}
}

type recordingSelectedMCPClient struct {
	*cloudDSHMCPClientStub
	listHeaders http.Header
	callHeaders http.Header
}

func (c *recordingSelectedMCPClient) ListTools(ctx context.Context, endpoint string, headers http.Header) ([]MCPConnectorToolDefinition, error) {
	c.listHeaders = headers.Clone()
	return c.cloudDSHMCPClientStub.ListTools(ctx, endpoint, headers)
}

func (c *recordingSelectedMCPClient) CallTool(ctx context.Context, endpoint string, headers http.Header,
	name string, arguments json.RawMessage) (MCPConnectorToolResult, error) {
	c.callHeaders = headers.Clone()
	return c.cloudDSHMCPClientStub.CallTool(ctx, endpoint, headers, name, arguments)
}
