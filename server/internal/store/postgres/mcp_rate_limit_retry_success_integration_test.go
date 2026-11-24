package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type m09RetryTaskAuthorizer struct{}

func (m09RetryTaskAuthorizer) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

type m09RetryFenceReader struct{}

func (m09RetryFenceReader) ReadTaskFence(context.Context, string, string) (app.AgentHostFence, error) {
	return app.AgentHostFence{RuntimeGeneration: 1, ControlVersion: 0, AIAllowed: true}, nil
}

type m09RetrySandboxRouter struct{}

func (m09RetrySandboxRouter) For(string) (app.SandboxClient, error) {
	return nil, errors.New("unused in MCP retry test")
}
func (m09RetrySandboxRouter) Profiles() []string { return nil }

type m09RetryWorkspaceBlocks struct{}

func (m09RetryWorkspaceBlocks) Open(string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (m09RetryWorkspaceBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}
func (m09RetryWorkspaceBlocks) ReservationBytes(size int64) (int64, error)        { return size, nil }
func (m09RetryWorkspaceBlocks) BlockAllocatedBytes(string, string) (int64, error) { return 0, nil }

type m09RetryMCPClient struct {
	callCount int
}

func (*m09RetryMCPClient) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return []app.MCPConnectorToolDefinition{{Name: "lookup", Description: "Synthetic lookup",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}}, nil
}

func (c *m09RetryMCPClient) CallTool(_ context.Context, _ string, _ http.Header, name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	c.callCount++
	if name != "lookup" || string(arguments) != `{"query":"retry fixture"}` {
		return app.MCPConnectorToolResult{}, errors.New("unexpected synthetic connector request")
	}
	if c.callCount == 1 {
		return app.MCPConnectorToolResult{IsError: true,
			StructuredContent: json.RawMessage(`{"code":"connector_rate_limited","terminal_for_attempt":true,"retry_after_seconds":1}`),
			Text:              []string{"synthetic connector limit"}}, nil
	}
	return app.MCPConnectorToolResult{Text: []string{"synthetic connector result after retry"}}, nil
}

type m09RetryCredentialProvider struct{}

func (m09RetryCredentialProvider) CredentialHeaders(context.Context, app.MCPConnectorRecord) (http.Header, error) {
	return make(http.Header), nil
}

func m09RetryBroker(t *testing.T, store *Store, client *m09RetryMCPClient) *app.CloudDSHToolBroker {
	t.Helper()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewRuntimeControl(store, m09RetrySandboxRouter{}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := app.NewWorkspaceSyncService(store, m09RetryWorkspaceBlocks{}, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.NewRuntimeToolExecutor(store, m09RetryTaskAuthorizer{}, m09RetryFenceReader{}, control, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtime, m09RetryCredentialProvider{}, client)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func m09RetryTaskFence(value task.Task, sequence int64) *app.ModelTaskFence {
	return &app.ModelTaskFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version, AgentInstanceID: value.AgentInstanceID,
		AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID, AgentRootSessionID: value.AgentSessionID,
		AuthorizationVersion: value.Authorization.Version, ThroughSequence: sequence,
		EngineVersion: value.AgentEngineVersion, ProfileVersion: value.AgentProfileVersion}
}

func m09RetryMCPCall(t *testing.T, callID, connectorID string) app.ModelToolCall {
	t.Helper()
	arguments, err := json.Marshal(struct {
		ConnectorID string          `json:"connector_id"`
		ToolName    string          `json:"tool_name"`
		Arguments   json.RawMessage `json:"arguments"`
	}{ConnectorID: connectorID, ToolName: "lookup", Arguments: json.RawMessage(`{"query":"retry fixture"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return app.ModelToolCall{CallID: callID, Name: "mcp_call", Arguments: string(arguments)}
}

func m09StopSelectedAgent(t *testing.T, fixture *selectedMCPAgentFixture) {
	t.Helper()
	if _, err := fixture.instances.ConfirmStopped(context.Background(), signAgentLifecycleReceipt(t, fixture.key, fixture.fence, fixture.nonce)); err != nil {
		t.Fatalf("confirm selected MCP Agent stopped: %v", err)
	}
}

func m09ApprovePendingMCPCall(t *testing.T, fixture *selectedMCPAgentFixture, key string) {
	t.Helper()
	ctx := context.Background()
	details, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || details.PendingRequest == nil || details.PendingRequest.Status != task.PendingRequestPending {
		t.Fatalf("read pending MCP approval: pending=%+v err=%v", details.PendingRequest, err)
	}
	pending := details.PendingRequest
	response := app.IdempotencyRecord{OwnerID: fixture.task.OwnerID, Operation: "task.pending.respond", Key: key,
		RequestHash: hashBytes("approve:" + key), ResourceID: fixture.task.ID, RelatedID: pending.ID}
	if err := fixture.route.store.RespondToPendingRequest(ctx, response, fixture.task.ID, pending.ID, pending.Version, task.PendingDecisionApprove); err != nil {
		t.Fatalf("approve exact MCP retry call: %v", err)
	}
}

func TestPostgresMCPConnectorRateLimitRetryExecutesAndPersistsSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var connectorID string
	fixture := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
		connector := createEnabledMCPGrantConnectors(t, route.store, route.ownerID)[0]
		connectorID = connector.ID
		return []string{connectorID}
	})
	checkpointDigest := hashBytes("m09-mcp-rate-limit-retry-checkpoint")
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=0,checkpoint_digest=$2 WHERE id=$1`,
		fixture.task.ID, checkpointDigest); err != nil {
		t.Fatalf("seed pinned DSH checkpoint: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, fixture.task.ID, fixture.fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("seed Agent Session checkpoint: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,
engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,$4,$5,$5,0,0,$6,'sealed',$7,$8,$9,$10)`, fixture.task.ID, fixture.task.OwnerID,
		fixture.fence.AgentInstanceID, fixture.fence.AgentGeneration, fixture.fence.AgentSessionID, checkpointDigest,
		fixture.fence.EngineVersion, fixture.fence.ProfileVersion, fixture.fence.AuthorizationVersion, fixture.fence.LeaseVersion); err != nil {
		t.Fatalf("persist pinned DSH checkpoint: %v", err)
	}
	fixture.task.CheckpointThroughSequence, fixture.task.CheckpointDigest = 0, checkpointDigest
	client := &m09RetryMCPClient{}
	broker := m09RetryBroker(t, fixture.route.store, client)

	discoveryArgs, err := json.Marshal(map[string]string{"connector_id": connectorID})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := broker.Execute(ctx, fixture.task, app.ModelToolCall{CallID: "m09-retry-discover",
		Name: "mcp_discover", Arguments: string(discoveryArgs)}); err != nil || result.Status != app.AgentToolSucceeded {
		t.Fatalf("discover synthetic MCP connector: result=%+v err=%v", result, err)
	}

	sourceCall := m09RetryMCPCall(t, "m09-source-rate-limit", connectorID)
	sourceCall.TaskFence = m09RetryTaskFence(fixture.task, fixture.task.CheckpointThroughSequence)
	if result, err := broker.Execute(ctx, fixture.task, sourceCall); err != nil || result.Status != app.AgentToolNeedsUser || client.callCount != 0 {
		t.Fatalf("MCP action before exact approval: result=%+v calls=%d err=%v", result, client.callCount, err)
	}
	m09StopSelectedAgent(t, &fixture)
	m09ApprovePendingMCPCall(t, &fixture, "m09-source-approval")
	startSelectedMCPAgent(t, &fixture, 1)
	sourceCall.TaskFence = m09RetryTaskFence(fixture.task, fixture.task.CheckpointThroughSequence)
	terminal, err := broker.Execute(ctx, fixture.task, sourceCall)
	if err != nil || terminal.Status != app.AgentToolFailed || !terminal.TerminalForAttempt || client.callCount != 1 {
		t.Fatalf("initial MCP terminal rate limit: result=%+v calls=%d err=%v", terminal, client.callCount, err)
	}
	if err := broker.FailTerminalToolAttempt(ctx, fixture.task, sourceCall); err != nil {
		t.Fatalf("persist terminal MCP Task failure: %v", err)
	}
	m09StopSelectedAgent(t, &fixture)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=0,checkpoint_digest=$2 WHERE id=$1`,
		fixture.task.ID, checkpointDigest); err != nil {
		t.Fatalf("restore pinned DSH checkpoint before user retry: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, fixture.task.ID, fixture.fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("restore Agent Session checkpoint before user retry: %v", err)
	}

	details, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceStep *task.Step
	for i := range details.Steps {
		if details.Steps[i].ToolCallKey == sourceCall.CallID {
			sourceStep = &details.Steps[i]
			break
		}
	}
	if sourceStep == nil || sourceStep.Status != task.StepFailed || sourceStep.Retryable || sourceStep.RetryAfterAt == nil ||
		!sourceStep.RetryAfterAt.After(time.Now()) || sourceStep.FailureReason != "connector_rate_limited" {
		t.Fatalf("terminal MCP source step should wait for Retry-After = %+v", sourceStep)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET updated_at=clock_timestamp()-interval '10 minutes'
WHERE task_id=$1 AND call_id=$2`, fixture.task.ID, sourceCall.CallID); err != nil {
		t.Fatalf("expire synthetic connector Retry-After: %v", err)
	}
	details, err = fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	sourceStep = nil
	for i := range details.Steps {
		if details.Steps[i].ToolCallKey == sourceCall.CallID {
			sourceStep = &details.Steps[i]
			break
		}
	}
	if sourceStep == nil || !sourceStep.Retryable || sourceStep.RetryAfterAt == nil || sourceStep.RetryAfterAt.After(time.Now()) {
		t.Fatalf("terminal MCP source step should become retryable after Retry-After = %+v", sourceStep)
	}
	if details.Task.CurrentRevision == nil {
		t.Fatal("failed MCP Task has no current revision")
	}
	attemptStepID := mustID(t, domain.NewStepID)
	retryRequest := app.TaskStepRetryRequest{TaskID: fixture.task.ID, StepID: sourceStep.ID,
		ExpectedTaskRevision: *details.Task.CurrentRevision, ExpectedAttempt: sourceStep.Attempt,
		AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	retryRecord := app.IdempotencyRecord{OwnerID: fixture.task.OwnerID, Operation: "task.step.retry", Key: "m09-mcp-retry-success",
		RequestHash: hashBytes("m09-mcp-retry-success:" + sourceStep.ID), RelatedID: sourceStep.ID}
	requested, err := fixture.route.store.RequestTaskStepRetry(ctx, retryRecord, retryRequest)
	if err != nil || requested.Replayed || requested.Intent.AttemptStepID != attemptStepID {
		t.Fatalf("reserve MCP retry intent: intent=%+v replayed=%t err=%v task=%+v", requested.Intent, requested.Replayed, err, details.Task)
	}

	startSelectedMCPAgent(t, &fixture, 2)
	retryIntent := fixture.task.StepRetry
	if retryIntent == nil || retryIntent.AttemptStepID != attemptStepID || retryIntent.RetryCallID != retryRequest.RetryCallID {
		t.Fatalf("claimed retry intent = %+v", retryIntent)
	}
	if err := fixture.route.store.ConsumeTaskStepRetryIntent(ctx, *retryIntent, fixture.fence); err != nil {
		t.Fatalf("consume retry intent at new execution fence: %v", err)
	}
	retryCall := m09RetryMCPCall(t, retryRequest.RetryCallID, connectorID)
	retryCall.TaskFence = m09RetryTaskFence(fixture.task, retryIntent.CheckpointThroughSequence)
	if result, err := broker.Execute(ctx, fixture.task, retryCall); err != nil || result.Status != app.AgentToolNeedsUser || client.callCount != 1 {
		t.Fatalf("MCP retry before fresh approval: result=%+v calls=%d err=%v", result, client.callCount, err)
	}
	m09StopSelectedAgent(t, &fixture)
	m09ApprovePendingMCPCall(t, &fixture, "m09-mcp-retry-approval")
	startSelectedMCPAgent(t, &fixture, 3)
	if fixture.task.StepRetry == nil || !fixture.task.StepRetry.Consumed {
		t.Fatalf("resumed MCP retry lost consumed immutable intent: %+v", fixture.task.StepRetry)
	}
	if err := fixture.route.store.ConsumeTaskStepRetryIntent(ctx, *fixture.task.StepRetry, fixture.fence); err != nil {
		t.Fatalf("resume consumed retry intent after approval: %v", err)
	}
	retryCall.TaskFence = m09RetryTaskFence(fixture.task, retryIntent.CheckpointThroughSequence)
	succeeded, err := broker.Execute(ctx, fixture.task, retryCall)
	if err != nil || succeeded.Status != app.AgentToolSucceeded || client.callCount != 2 {
		t.Fatalf("execute approved MCP retry: result=%+v calls=%d err=%v", succeeded, client.callCount, err)
	}
	replayed, err := broker.Reconcile(ctx, fixture.task, retryCall, nil)
	if err != nil || replayed.Status != app.AgentToolSucceeded || replayed.Output != succeeded.Output || client.callCount != 2 {
		t.Fatalf("reconcile successful MCP retry: result=%+v calls=%d err=%v", replayed, client.callCount, err)
	}

	final, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	var retainedSource, successfulRetry *task.Step
	for i := range final.Steps {
		step := &final.Steps[i]
		if step.ID == sourceStep.ID {
			retainedSource = step
		}
		if step.ID == attemptStepID {
			successfulRetry = step
		}
	}
	if retainedSource == nil || retainedSource.Status != task.StepFailed || retainedSource.FailureReason != "connector_rate_limited" ||
		successfulRetry == nil || successfulRetry.Status != task.StepSucceeded || successfulRetry.RetryOfStepID != sourceStep.ID ||
		successfulRetry.Attempt != 2 || successfulRetry.ToolCallKey != retryRequest.RetryCallID {
		t.Fatalf("source/retry steps after connector success: source=%+v retry=%+v", retainedSource, successfulRetry)
	}
	var sourceLedgerStatus, retryLedgerStatus, retryLedgerResult string
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT status FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`,
		fixture.task.ID, sourceCall.CallID).Scan(&sourceLedgerStatus); err != nil {
		t.Fatal(err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT status,result FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`,
		fixture.task.ID, retryRequest.RetryCallID).Scan(&retryLedgerStatus, &retryLedgerResult); err != nil {
		t.Fatal(err)
	}
	if sourceLedgerStatus != string(task.OperationFailed) || retryLedgerStatus != string(task.OperationSucceeded) || !strings.Contains(retryLedgerResult, "synthetic connector result after retry") {
		t.Fatalf("source/retry MCP ledger statuses/results=%s / %s %q", sourceLedgerStatus, retryLedgerStatus, retryLedgerResult)
	}
	var retryCount int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2`,
		fixture.task.ID, sourceStep.ID).Scan(&retryCount); err != nil || retryCount != 1 {
		t.Fatalf("persisted MCP retry step count=%d err=%v; want exactly one", retryCount, err)
	}
	events, err := fixture.route.store.ListTaskEvents(ctx, fixture.task.OwnerID, fixture.task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenRetrySuccess := false
	for _, event := range events {
		if event.Type != "task.step" {
			continue
		}
		var payload struct {
			StepID string `json:"step_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode MCP retry Task event: %v", err)
		}
		if payload.StepID == attemptStepID && payload.Status == string(task.StepSucceeded) {
			seenRetrySuccess = true
		}
	}
	if !seenRetrySuccess {
		t.Fatal("successful MCP retry has no persisted task.step event")
	}
}
