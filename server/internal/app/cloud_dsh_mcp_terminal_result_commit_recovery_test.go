package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/domain/task"
)

type cloudDSHMCPFinalResultCommitFaultStore struct {
	*cloudDSHMCPStoreStub
	callID                    string
	connectorID               string
	injected                  bool
	resultCommitAttempts      int
	resultCommitFailureReason bool
}

func (s *cloudDSHMCPFinalResultCommitFaultStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if progress.Event.Type == "task.result" {
		s.resultCommitAttempts++
		if !s.injected && s.persistedTerminalEvidence(progress) {
			s.injected = true
			s.resultCommitFailureReason = true
			return task.Event{}, errors.New("synthetic task.result commit failure")
		}
	}
	return s.cloudDSHMCPStoreStub.CommitTaskProgress(ctx, progress)
}

func (s *cloudDSHMCPFinalResultCommitFaultStore) persistedTerminalEvidence(progress TaskProgress) bool {
	if progress.TaskID != s.details.Task.ID || progress.Status != task.StatusFailed ||
		progress.Result != "The selected connector rate limited this attempt." {
		return false
	}

	s.mu.RLock()
	current := s.details.Task
	steps := append([]task.Step(nil), s.details.Steps...)
	s.mu.RUnlock()
	if current.CurrentRevision == nil {
		return false
	}
	failedStepFound := false
	for _, step := range steps {
		if step.AgentSessionID == current.AgentSessionID && step.ToolCallKey == s.callID &&
			step.Status == task.StepFailed && step.TaskRevision != nil && *step.TaskRevision == *current.CurrentRevision &&
			step.Summary == "The connector rate limited this attempt; it was not retried." {
			failedStepFound = true
			break
		}
	}
	if !failedStepFound {
		return false
	}

	operation, ok := s.operations[agentOperationKey(s.callID)]
	if !ok || operation.TaskID != current.ID || operation.OwnerID != current.OwnerID ||
		operation.ConnectorID != s.connectorID || operation.CallID != s.callID || operation.Status != task.OperationFailed {
		return false
	}
	retryAfter, ok := MCPConnectorTerminalRetryAfterSeconds(operation.Result)
	return ok && retryAfter == 300
}

func TestCloudDSHMCPFinalTaskResultCommitFailureReconcilesWithoutRedispatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	fixture.store.details.Task.HostID = ""
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.store.details.Task.ExecutionLocation = "server"
	current := fixture.store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-mcp", 1, "session-root"
	revision := int64(5)
	current.CurrentRevision, current.DesiredRevision = &revision, &revision
	current.Authorization.Version = 2
	fixture.store.details.Task = current
	connectorID := "k1234567"
	baseStore := &cloudDSHMCPStoreStub{
		cloudDSHBrokerTaskStore: &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store},
		mcpConnectorStoreStub: &mcpConnectorStoreStub{values: map[string]MCPConnectorRecord{
			connectorID: {ID: connectorID, OwnerID: fixture.store.details.Task.OwnerID, Name: "Synthetic", EndpointURL: "https://mcp.example.test", Enabled: true, Revision: 1},
		}},
		grants: []TaskMCPConnectorGrant{{TaskID: fixture.store.details.Task.ID, OwnerID: fixture.store.details.Task.OwnerID,
			ConnectorOwnerID: fixture.store.details.Task.OwnerID, ConnectorID: connectorID, ConnectorRevision: 1, GrantVersion: 1}},
	}
	store := &cloudDSHMCPFinalResultCommitFaultStore{
		cloudDSHMCPStoreStub: baseStore,
		callID:               "terminal-rate-limit-final-commit-1",
		connectorID:          connectorID,
	}
	fixture.executor.store = store
	client := &cloudDSHMCPClientStub{
		tools: []MCPConnectorToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		result: MCPConnectorToolResult{IsError: true,
			StructuredContent: json.RawMessage(`{"code":"connector_rate_limited","terminal_for_attempt":true,"retry_after_seconds":300}`),
			Text:              []string{"synthetic provider detail"}},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, cloudDSHMCPCredentialStub{}, client)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	discoverSelectedMCPForTest(t, broker, claimed, connectorID)
	call := selectedMCPCallForTest(t, store.callID, connectorID, "lookup", `{"query":"synthetic"}`)
	call.TaskFence = &ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AgentInstanceID: claimed.AgentInstanceID,
		AgentGeneration: claimed.AgentGeneration, AgentSessionID: claimed.AgentSessionID, AgentRootSessionID: claimed.AgentSessionID,
		AuthorizationVersion: claimed.Authorization.Version}
	if result, err := broker.Execute(context.Background(), claimed, call); err != nil || result.Status != AgentToolNeedsUser {
		t.Fatalf("unapproved MCP action = %+v err=%v", result, err)
	}
	resumeCloudDSHMCPApproval(t, baseStore, task.PendingDecisionApprove)
	claimed = store.details.Task
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = claimed.Lease.Owner, claimed.Lease.Version

	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolFailed || !result.TerminalForAttempt || client.callCalls != 1 {
		t.Fatalf("terminal MCP execution = %+v err=%v connector calls=%d", result, err, client.callCalls)
	}
	assertCloudDSHMCPFinalCommitSourcePersisted(t, store, claimed, call)
	if store.details.Task.Status != task.StatusRunning {
		t.Fatalf("Task status after failed-step commit = %s, want running", store.details.Task.Status)
	}

	if err := broker.FailTerminalToolAttempt(context.Background(), claimed, call); err == nil {
		t.Fatal("first task.result commit unexpectedly succeeded")
	}
	if !store.injected || !store.resultCommitFailureReason || store.resultCommitAttempts != 1 ||
		store.details.Task.Status != task.StatusRunning || countCloudDSHMCPTaskResultEvents(store.events) != 0 {
		t.Fatalf("injected final commit state: injected=%t source-evidence=%t attempts=%d task=%+v task.result events=%d",
			store.injected, store.resultCommitFailureReason, store.resultCommitAttempts, store.details.Task,
			countCloudDSHMCPTaskResultEvents(store.events))
	}
	assertCloudDSHMCPFinalCommitSourcePersisted(t, store, claimed, call)

	reconciled, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || reconciled.Status != AgentToolFailed || !reconciled.TerminalForAttempt || client.callCalls != 1 ||
		store.details.Task.Status != task.StatusRunning {
		t.Fatalf("same-call reconciliation = %+v err=%v connector calls=%d task=%+v", reconciled, err, client.callCalls, store.details.Task)
	}
	assertCloudDSHMCPFinalCommitSourcePersisted(t, store, claimed, call)

	if err := broker.FailTerminalToolAttempt(context.Background(), claimed, call); err != nil {
		t.Fatalf("retry terminal Task failure commit: %v", err)
	}
	if store.resultCommitAttempts != 2 || store.details.Task.Status != task.StatusFailed ||
		store.details.Task.Result != "The selected connector rate limited this attempt." ||
		countCloudDSHMCPTaskResultEvents(store.events) != 1 {
		t.Fatalf("final Task state/events: attempts=%d task=%+v task.result events=%d",
			store.resultCommitAttempts, store.details.Task, countCloudDSHMCPTaskResultEvents(store.events))
	}
	assertCloudDSHMCPFinalCommitSourcePersisted(t, store, claimed, call)
	if client.callCalls != 1 {
		t.Fatalf("connector was redispatched during recovery: calls=%d", client.callCalls)
	}
}

func assertCloudDSHMCPFinalCommitSourcePersisted(t *testing.T, store *cloudDSHMCPFinalResultCommitFaultStore, claimed task.Task, call ModelToolCall) {
	t.Helper()
	var failedStep *task.Step
	for index := range store.details.Steps {
		step := &store.details.Steps[index]
		if step.AgentSessionID == claimed.AgentSessionID && step.ToolCallKey == call.CallID {
			failedStep = step
			break
		}
	}
	operation, ok := store.operations[agentOperationKey(call.CallID)]
	if failedStep == nil || failedStep.Status != task.StepFailed ||
		failedStep.Summary != "The connector rate limited this attempt; it was not retried." ||
		failedStep.TaskRevision == nil || claimed.CurrentRevision == nil || *failedStep.TaskRevision != *claimed.CurrentRevision ||
		!ok || operation.CallID != call.CallID || operation.TaskID != claimed.ID || operation.ConnectorID != store.connectorID ||
		operation.Status != task.OperationFailed {
		t.Fatalf("persisted source failure / MCP ledger: step=%+v operation=%+v found=%t", failedStep, operation, ok)
	}
	if retryAfter, valid := MCPConnectorTerminalRetryAfterSeconds(operation.Result); !valid || retryAfter != 300 {
		t.Fatalf("persisted MCP terminal Retry-After = %d valid=%t, want 300 seconds", retryAfter, valid)
	}
}

func countCloudDSHMCPTaskResultEvents(events []task.Event) int {
	count := 0
	for _, event := range events {
		if event.Type == "task.result" {
			count++
		}
	}
	return count
}
