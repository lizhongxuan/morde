package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type cloudDSHMCPStoreStub struct {
	*cloudDSHBrokerTaskStore
	*mcpConnectorStoreStub
	grants     []TaskMCPConnectorGrant
	operations map[string]MCPConnectorOperation
	binding    *workspace.Binding
}

func (s *cloudDSHMCPStoreStub) GetWorkspaceBinding(_ context.Context, hostID, workspaceID string) (workspace.Binding, error) {
	if s.binding == nil || s.binding.HostID != hostID || s.binding.Workspace.ID != workspaceID {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	return *s.binding, nil
}

func (s *cloudDSHMCPStoreStub) ListTaskMCPConnectorGrants(_ context.Context, ownerID, taskID string) ([]TaskMCPConnectorGrant, error) {
	if s.details.Task.OwnerID != ownerID || s.details.Task.ID != taskID {
		return nil, domain.NewError(domain.CodeNotFound, "task not found")
	}
	return append([]TaskMCPConnectorGrant(nil), s.grants...), nil
}

func (s *cloudDSHMCPStoreStub) SetTaskMCPConnectorToolSchema(_ context.Context, ownerID, taskID, connectorID string,
	expectedGrantVersion int64, schema, digest []byte) (TaskMCPConnectorGrant, error) {
	for index := range s.grants {
		grant := &s.grants[index]
		if grant.OwnerID == ownerID && grant.TaskID == taskID && grant.ConnectorID == connectorID && grant.GrantVersion == expectedGrantVersion && grant.RevokedAt == nil {
			if len(grant.ToolSchema) != 0 && (!bytes.Equal(grant.ToolSchema, schema) || !bytes.Equal(grant.ToolSchemaSHA256, digest)) {
				return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeConflict, "schema already pinned")
			}
			grant.ToolSchema, grant.ToolSchemaSHA256 = append([]byte(nil), schema...), append([]byte(nil), digest...)
			return *grant, nil
		}
	}
	return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeConflict, "grant changed")
}

func (s *cloudDSHMCPStoreStub) RevokeTaskMCPConnectorGrant(context.Context, string, string, string, int64) (TaskMCPConnectorGrant, error) {
	return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeUnsupported, "not used")
}

func (s *cloudDSHMCPStoreStub) PrepareMCPConnectorOperation(_ context.Context, value MCPConnectorOperation, _ string) (MCPConnectorOperation, bool, error) {
	if s.operations == nil {
		s.operations = make(map[string]MCPConnectorOperation)
	}
	if stored, ok := s.operations[value.Key]; ok {
		if stored.ConnectorID != value.ConnectorID || stored.ToolName != value.ToolName || !bytes.Equal(stored.RequestHash, value.RequestHash) {
			return MCPConnectorOperation{}, false, domain.NewError(domain.CodeIdempotencyConflict, "request changed")
		}
		if stored.Status == task.OperationPrepared {
			stored.LeaseVersion = value.LeaseVersion
			s.operations[value.Key] = stored
		}
		return stored, true, nil
	}
	value.CreatedAt, value.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	s.operations[value.Key] = value
	return value, false, nil
}

func (s *cloudDSHMCPStoreStub) MarkMCPConnectorOperationDispatched(_ context.Context, value MCPConnectorOperation, _ string) (MCPConnectorOperation, error) {
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationPrepared {
		return MCPConnectorOperation{}, domain.NewError(domain.CodeConflict, "operation is not prepared")
	}
	stored.Status = task.OperationDispatched
	s.operations[value.Key] = stored
	return stored, nil
}

func (s *cloudDSHMCPStoreStub) CompleteMCPConnectorOperation(_ context.Context, value MCPConnectorOperation, _ string,
	status task.OperationStatus, result string) (MCPConnectorOperation, error) {
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationDispatched || stored.CancelRequested {
		return MCPConnectorOperation{}, domain.NewError(domain.CodeConflict, "operation is not dispatched")
	}
	stored.Status, stored.Result = status, result
	s.operations[value.Key] = stored
	return stored, nil
}

func (s *cloudDSHMCPStoreStub) MarkMCPConnectorOperationUnknown(_ context.Context, value MCPConnectorOperation, _ string, result string) error {
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationDispatched {
		return domain.NewError(domain.CodeConflict, "operation is not dispatched")
	}
	stored.Status, stored.Result = task.OperationUnknown, result
	s.operations[value.Key] = stored
	return nil
}

func (s *cloudDSHMCPStoreStub) GetMCPConnectorOperation(_ context.Context, ownerID, taskID, connectorID, key string) (MCPConnectorOperation, error) {
	value, ok := s.operations[key]
	if !ok || value.OwnerID != ownerID || value.TaskID != taskID || value.ConnectorID != connectorID {
		return MCPConnectorOperation{}, domain.NewError(domain.CodeNotFound, "operation not found")
	}
	return value, nil
}

type cloudDSHMCPClientStub struct {
	tools       []MCPConnectorToolDefinition
	result      MCPConnectorToolResult
	err         error
	listCalls   int
	callCalls   int
	lastTool    string
	lastRequest json.RawMessage
	onCall      func()
}

func (s *cloudDSHMCPClientStub) ListTools(context.Context, string, http.Header) ([]MCPConnectorToolDefinition, error) {
	s.listCalls++
	return append([]MCPConnectorToolDefinition(nil), s.tools...), nil
}

func (s *cloudDSHMCPClientStub) CallTool(_ context.Context, _ string, _ http.Header, name string, arguments json.RawMessage) (MCPConnectorToolResult, error) {
	s.callCalls++
	s.lastTool, s.lastRequest = name, append(json.RawMessage(nil), arguments...)
	if s.onCall != nil {
		s.onCall()
	}
	return s.result, s.err
}

type cloudDSHMCPCredentialStub struct{}

func (cloudDSHMCPCredentialStub) CredentialHeaders(context.Context, MCPConnectorRecord) (http.Header, error) {
	return make(http.Header), nil
}

func TestCloudDSHMCPBrokerPinsGrantAndReplaysDurableResult(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	inputRevision, checkpointRevision := int64(4), int64(7)
	fixture.store.details.Task.HostID = "h1234567"
	fixture.store.details.Task.WorkspaceID = "w1234567"
	fixture.store.details.Task.InputRevision = &inputRevision
	fixture.store.details.Task.WorkspaceCheckpointRevision = &checkpointRevision
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k1234567"
	connectorStore := &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
		connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, CredentialVersion: 1, Revision: 1},
	}}
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub:   connectorStore,
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorID: connectorID, ConnectorRevision: 1, CredentialVersion: 1, GrantVersion: 1}},
		binding: &workspace.Binding{OwnerID: fixture.store.details.Task.OwnerID, HostID: fixture.store.details.Task.HostID,
			Workspace: workspace.Workspace{ID: fixture.store.details.Task.WorkspaceID}, BindingVersion: 9},
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{
		tools:  []MCPConnectorToolDefinition{{Name: "lookup", Description: "Find records", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		result: MCPConnectorToolResult{StructuredContent: json.RawMessage(`{"count":1}`), Text: []string{"one record"}},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.mcpDefinitions(context.Background(), claimed)
	if err != nil || len(definitions) != 1 || definitions[0].Name != cloudDSHMCPToolName(connectorID, "lookup") || definitions[0].RequiredScope != identity.ScopeMCPControl {
		t.Fatalf("MCP definitions = %#v, err=%v", definitions, err)
	}
	grant := store.grants[0]
	if len(grant.ToolSchema) == 0 || !bytes.Equal(grant.ToolSchemaSHA256, func() []byte { digest := sha256.Sum256(grant.ToolSchema); return digest[:] }()) {
		t.Fatalf("pinned MCP schema/digest = %q / %x", grant.ToolSchema, grant.ToolSchemaSHA256)
	}
	call := ModelToolCall{CallID: "mcp-call-1", Name: definitions[0].Name, Arguments: `{"query":"agent"}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "lookup" {
		t.Fatalf("MCP execution = %#v, err=%v; calls=%d tool=%q", first, err, client.callCalls, client.lastTool)
	}
	var output cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(first.Output), &output); err != nil || output.IsError || string(output.StructuredContent) != `{"count":1}` || len(output.Text) != 1 || output.Text[0] != "one record" {
		t.Fatalf("MCP result envelope = %#v, err=%v", output, err)
	}
	stored := store.operations[agentOperationKey(call.CallID)]
	if store.details.Task.Usage.ToolCalls != 1 || stored.Status != task.OperationSucceeded || stored.HostID != fixture.store.details.Task.HostID ||
		stored.WorkspaceID != fixture.store.details.Task.WorkspaceID || stored.BindingVersion != 9 || stored.InputRevision == nil ||
		*stored.InputRevision != inputRevision || stored.WorkspaceCheckpointRevision == nil || *stored.WorkspaceCheckpointRevision != checkpointRevision {
		t.Fatalf("MCP budget/operation = %d / %#v", store.details.Task.Usage.ToolCalls, stored)
	}
	replayed, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || replayed.Output != first.Output || client.callCalls != 1 {
		t.Fatalf("replayed MCP result = %#v, err=%v; remote calls=%d", replayed, err, client.callCalls)
	}
	client.tools[0].Description = "Changed after the grant"
	if _, err := broker.mcpDefinitions(context.Background(), claimed); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed tool schema error = %v", err)
	}
}

func TestCloudDSHMCPBrokerPersistsUnknownWithoutReplaying(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k7654321"
	connectorStore := &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
		connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, CredentialVersion: 0, Revision: 1},
	}}
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub:   connectorStore,
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorID: connectorID, ConnectorRevision: 1, GrantVersion: 1}},
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{
		tools: []MCPConnectorToolDefinition{{Name: "change", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		err:   context.DeadlineExceeded,
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "mcp-call-timeout", Name: definitions[0].Name, Arguments: `{}`}
	result, err := broker.Execute(context.Background(), claimed, call)
	operation := store.operations[agentOperationKey(call.CallID)]
	if err != nil || result.Status != AgentToolUnknown || operation.Status != task.OperationUnknown || client.callCalls != 1 ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_result_unknown" {
		t.Fatalf("unknown MCP result = %#v, err=%v, operation=%#v, task=%#v, calls=%d", result, err, operation, store.details.Task, client.callCalls)
	}
}

func TestCloudDSHMCPConfirmedUnsupportedResultPausesAndReplaysWithoutCallingAgain(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k8765432"
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorID: connectorID, ConnectorRevision: 1, GrantVersion: 1}},
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{
		tools:  []MCPConnectorToolDefinition{{Name: "change", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		result: MCPConnectorToolResult{UnsupportedContent: true},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "mcp-call-unsupported", Name: definitions[0].Name, Arguments: `{}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	operation := store.operations[agentOperationKey(call.CallID)]
	var output cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(operation.Result), &output); err != nil {
		t.Fatal(err)
	}
	if err != nil || first.Status != AgentToolNeedsUser || !output.UnsupportedContent || operation.Status != task.OperationSucceeded ||
		client.callCalls != 1 || store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_result_unsupported" {
		t.Fatalf("unsupported MCP result = %#v, err=%v, operation=%#v, task=%#v, calls=%d", first, err, operation, store.details.Task, client.callCalls)
	}
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = claimed.Lease
	replayed, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || replayed.Status != AgentToolNeedsUser || client.callCalls != 1 || store.details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("reconciled unsupported MCP result = %#v, err=%v, calls=%d, task=%#v", replayed, err, client.callCalls, store.details.Task)
	}
}

func TestCloudDSHMCPBrokerDiscardsResultAfterGrantRevocation(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k2345678"
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorID: connectorID, ConnectorRevision: 1, GrantVersion: 1}},
	}
	fixture.executor.store = store
	callID := "mcp-call-revoked"
	client := &cloudDSHMCPClientStub{
		tools:  []MCPConnectorToolDefinition{{Name: "change", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		result: MCPConnectorToolResult{Text: []string{"completed remotely"}},
		onCall: func() {
			operation := store.operations[agentOperationKey(callID)]
			operation.CancelRequested = true
			store.operations[operation.Key] = operation
		},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: callID, Name: definitions[0].Name, Arguments: `{}`})
	operation := store.operations[agentOperationKey(callID)]
	if err != nil || result.Status != AgentToolNeedsUser || operation.Status != task.OperationUnknown || !operation.CancelRequested ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_connector_revoked" {
		t.Fatalf("revoked MCP result = %#v, err=%v, operation=%#v, task=%#v", result, err, operation, store.details.Task)
	}
}

func TestCloudDSHMCPStructuredResultHasJSONComplexityLimits(t *testing.T) {
	for _, value := range []string{`{"count":1}`, `[1,"ok"]`, `true`} {
		if !validCloudDSHMCPJSON([]byte(value)) {
			t.Errorf("valid structured result rejected: %s", value)
		}
	}
	if validCloudDSHMCPJSON([]byte(`{"count":1,"count":2}`)) {
		t.Fatal("structured result with duplicate object keys was accepted")
	}
	deep := strings.Repeat("[", maxAgentJSONDepth+2) + "0" + strings.Repeat("]", maxAgentJSONDepth+2)
	if validCloudDSHMCPJSON([]byte(deep)) {
		t.Fatal("structured result exceeded the nesting limit")
	}
}
