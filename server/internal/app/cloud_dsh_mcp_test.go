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
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type cloudDSHMCPStoreStub struct {
	*cloudDSHBrokerTaskStore
	*mcpConnectorStoreStub
	grants                []TaskMCPConnectorGrant
	operations            map[string]MCPConnectorOperation
	binding               *workspace.Binding
	projectGrantActive    bool
	projectGrantVersion   int64
	projectAuthorizeCalls int
	completeErr           error
	markUnknownErr        error
}

func (s *cloudDSHMCPStoreStub) AuthorizeProjectMCPConnector(_ context.Context, projectID, actorID, connectorOwnerID, connectorID string, expectedVersion int64) error {
	s.projectAuthorizeCalls++
	if !s.projectGrantActive || expectedVersion != s.projectGrantVersion || projectID != s.details.Task.ProjectID ||
		actorID != taskActorID(s.details.Task) || connectorID == "" || connectorOwnerID == "" {
		return domain.NewError(domain.CodeForbidden, "project connector authorization is no longer active")
	}
	return nil
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
	if s.completeErr != nil {
		return MCPConnectorOperation{}, s.completeErr
	}
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationDispatched || stored.CancelRequested {
		return MCPConnectorOperation{}, domain.NewError(domain.CodeConflict, "operation is not dispatched")
	}
	stored.Status, stored.Result = status, result
	s.operations[value.Key] = stored
	return stored, nil
}

func (s *cloudDSHMCPStoreStub) MarkMCPConnectorOperationUnknown(_ context.Context, value MCPConnectorOperation, _ string, result string) error {
	if s.markUnknownErr != nil {
		return s.markUnknownErr
	}
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
	onList      func()
	onCall      func()
}

func (s *cloudDSHMCPClientStub) ListTools(context.Context, string, http.Header) ([]MCPConnectorToolDefinition, error) {
	s.listCalls++
	if s.onList != nil {
		s.onList()
	}
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

func TestCanonicalMCPToolSchemaDefaultsOmittedAdditionalProperties(t *testing.T) {
	encoded, digest, err := canonicalMCPToolSchema([]MCPConnectorToolDefinition{
		{Name: "omitted", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
		{Name: "explicit-false", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "explicit-true", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)},
	})
	if err != nil || len(digest) == 0 {
		t.Fatalf("canonical MCP schema = %s, digest=%x, err=%v", encoded, digest, err)
	}
	var tools []MCPConnectorToolDefinition
	if err := json.Unmarshal(encoded, &tools); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "omitted", want: false},
		{name: "explicit-false", want: false},
		{name: "explicit-true", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var schema map[string]any
			for _, tool := range tools {
				if tool.Name == test.name {
					if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			actual, ok := schema["additionalProperties"].(bool)
			if !ok || actual != test.want {
				t.Fatalf("additionalProperties = %#v, want %t", schema["additionalProperties"], test.want)
			}
		})
	}
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
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID, ConnectorOwnerID: fixture.store.details.Task.OwnerID,
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
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	grant := store.grants[0]
	if len(grant.ToolSchema) == 0 || !bytes.Equal(grant.ToolSchemaSHA256, func() []byte { digest := sha256.Sum256(grant.ToolSchema); return digest[:] }()) {
		t.Fatalf("pinned MCP schema/digest = %q / %x", grant.ToolSchema, grant.ToolSchemaSHA256)
	}
	call := selectedMCPCallForTest(t, "mcp-call-1", connectorID, "lookup", `{"query":"agent"}`)
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 || len(store.operations) != 0 ||
		store.details.Task.Usage.ToolCalls != 0 || store.details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("MCP call before approval = %#v err=%v calls=%d operations=%d task=%#v", approval, err, client.callCalls, len(store.operations), store.details.Task)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolSucceeded || client.callCalls != 1 || client.lastTool != "lookup" {
		t.Fatalf("MCP execution = %#v, err=%v; calls=%d tool=%q", first, err, client.callCalls, client.lastTool)
	}
	var output cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(first.Output), &output); err != nil || output.IsError || string(output.StructuredContent) != `{"count":1}` || len(output.Text) != 1 || output.Text[0] != "one record" {
		t.Fatalf("MCP result envelope = %#v, err=%v", output, err)
	}
	for _, event := range store.events {
		if bytes.Contains(event.Payload, []byte(`"output"`)) || bytes.Contains(event.Payload, []byte("one record")) {
			t.Fatalf("MCP result was duplicated into a client-visible Task event: %s", event.Payload)
		}
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
	changedCatalog, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "changed-catalog", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"` + connectorID + `"}`})
	if err != nil || changedCatalog.Status != AgentToolFailed {
		t.Fatalf("changed tool schema result = %+v err=%v", changedCatalog, err)
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
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID, ConnectorOwnerID: fixture.store.details.Task.OwnerID,
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
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	call := selectedMCPCallForTest(t, "mcp-call-timeout", connectorID, "change", `{}`)
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 || len(store.operations) != 0 {
		t.Fatalf("MCP timeout call before approval = %#v err=%v calls=%d operations=%d", approval, err, client.callCalls, len(store.operations))
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	result, err := broker.Execute(context.Background(), claimed, call)
	operation := store.operations[agentOperationKey(call.CallID)]
	if err != nil || result.Status != AgentToolUnknown || operation.Status != task.OperationUnknown || client.callCalls != 1 ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_result_unknown" {
		t.Fatalf("unknown MCP result = %#v, err=%v, operation=%#v, task=%#v, calls=%d", result, err, operation, store.details.Task, client.callCalls)
	}
	store.details.Task.Status, store.details.Task.WaitingReason = task.StatusRunning, ""
	store.details.Task.Lease = claimed.Lease
	replayed, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || replayed.Status != AgentToolUnknown || client.callCalls != 1 || store.details.Task.WaitingReason != "mcp_result_unknown" {
		t.Fatalf("unknown MCP operation was retried: result=%+v calls=%d err=%v", replayed, client.callCalls, err)
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
			connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1, CredentialVersion: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID, ConnectorOwnerID: fixture.store.details.Task.OwnerID,
			ConnectorID: connectorID, ConnectorRevision: 1, CredentialVersion: 1, GrantVersion: 1}},
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
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	call := selectedMCPCallForTest(t, "mcp-call-unsupported", connectorID, "change", `{}`)
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 || len(store.operations) != 0 {
		t.Fatalf("unsupported MCP call before approval = %#v err=%v calls=%d operations=%d", approval, err, client.callCalls, len(store.operations))
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
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
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID, ConnectorOwnerID: fixture.store.details.Task.OwnerID,
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
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	call := selectedMCPCallForTest(t, callID, connectorID, "change", `{}`)
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("revoked MCP call before approval = %#v err=%v calls=%d", approval, err, client.callCalls)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	result, err := broker.Execute(context.Background(), claimed, call)
	operation := store.operations[agentOperationKey(callID)]
	if err != nil || result.Status != AgentToolNeedsUser || operation.Status != task.OperationUnknown || !operation.CancelRequested ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_connector_revoked" {
		t.Fatalf("revoked MCP result = %#v, err=%v, operation=%#v, task=%#v", result, err, operation, store.details.Task)
	}
}

func TestCloudDSHMCPBrokerMarksUnknownWhenResultCommitFails(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	connectorID := "k3456789"
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Research", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1, CredentialVersion: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorOwnerID: fixture.store.details.Task.OwnerID, ConnectorID: connectorID, ConnectorRevision: 1, CredentialVersion: 1, GrantVersion: 1}},
		completeErr: domain.NewError(domain.CodeInternal, "result commit failed"),
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	call := selectedMCPCallForTest(t, "result-commit-failed", connectorID, "lookup", `{}`)
	approval, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || approval.Status != AgentToolNeedsUser || client.callCalls != 0 {
		t.Fatalf("result-commit-failed MCP call before approval = %#v err=%v calls=%d", approval, err, client.callCalls)
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	result, err := broker.Execute(context.Background(), claimed, call)
	operation := store.operations[agentOperationKey("result-commit-failed")]
	if err != nil || result.Status != AgentToolUnknown || client.callCalls != 1 || operation.Status != task.OperationUnknown ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "mcp_result_unknown" {
		t.Fatalf("failed result commit = %#v, err=%v, calls=%d, operation=%#v task=%#v", result, err, client.callCalls, operation, store.details.Task)
	}
}

func TestCloudDSHMCPBrokerRechecksProjectConnectorAuthorization(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	fixture.store.details.Task.ProjectID = "j1234567"
	fixture.store.details.Task.InitiatorID = "u7654321"
	connectorID := "k3456789"
	connectorOwnerID := "u2345678"
	store := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: connectorOwnerID, Name: "Shared", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorOwnerID: connectorOwnerID, ProjectID: fixture.store.details.Task.ProjectID, ProjectGrantVersion: 4,
			ConnectorID: connectorID, ConnectorRevision: 1, CredentialVersion: 0, GrantVersion: 1}},
		projectGrantActive: true, projectGrantVersion: 4,
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{tools: []MCPConnectorToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	if store.projectAuthorizeCalls != 2 {
		t.Fatalf("discover project MCP tools: authCalls=%d", store.projectAuthorizeCalls)
	}
	store.projectGrantActive = false
	discovery := ModelToolCall{CallID: "project-mcp-discover", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"` + connectorID + `"}`}
	if result, err := broker.Execute(context.Background(), claimed, discovery); err != nil || result.Status != AgentToolFailed {
		t.Fatalf("discovery after project grant revoke = %+v err=%v", result, err)
	}
	if result, err := broker.Execute(context.Background(), claimed, selectedMCPCallForTest(t, "project-mcp-call", connectorID, "lookup", `{}`)); err != nil || result.Status != AgentToolFailed || client.callCalls != 0 {
		t.Fatalf("dispatch after project grant revoke = result=%+v err=%v, remoteCalls=%d", result, err, client.callCalls)
	}
	store.projectGrantActive = true
	store.projectAuthorizeCalls = 0
	store.grants[0].ToolSchema, store.grants[0].ToolSchemaSHA256 = nil, nil
	client.onList = func() { store.projectGrantActive = false }
	if result, err := broker.Execute(context.Background(), claimed, discovery); err != nil || result.Status != AgentToolFailed ||
		store.projectAuthorizeCalls != 2 || client.listCalls != 2 || len(store.grants[0].ToolSchema) != 0 {
		t.Fatalf("revocation during project tool discovery = result=%#v err=%v authCalls=%d listCalls=%d schema=%q",
			result, err, store.projectAuthorizeCalls, client.listCalls, store.grants[0].ToolSchema)
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
