package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type m09PostgresMCPClient struct {
	mu         sync.Mutex
	callCount  int
	callResult app.MCPConnectorToolResult
	callErr    error
}

func (*m09PostgresMCPClient) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return []app.MCPConnectorToolDefinition{{
		Name: "update_note", Description: "Update one synthetic note",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
	}}, nil
}

func (c *m09PostgresMCPClient) CallTool(context.Context, string, http.Header, string, json.RawMessage) (app.MCPConnectorToolResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callCount++
	return c.callResult, c.callErr
}

func (c *m09PostgresMCPClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callCount
}

type m09PostgresMCPCredentials struct{}

func (m09PostgresMCPCredentials) CredentialHeaders(context.Context, app.MCPConnectorRecord) (http.Header, error) {
	return make(http.Header), nil
}

// m09PostgresMCPFaultStore injects a post-dispatch Task persistence failure
// while leaving PostgreSQL MCP operation writes untouched.
type m09PostgresMCPFaultStore struct {
	*Store
	callID      string
	failUnknown bool
	failSuccess bool
	failureUsed bool
	mu          sync.Mutex
}

func (s *m09PostgresMCPFaultStore) CommitTaskProgress(ctx context.Context, progress app.TaskProgress) (task.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failureUsed && progress.ToolCall != nil && progress.ToolCall.CallKey == s.callID && len(progress.Steps) == 1 {
		step := progress.Steps[0]
		if s.failUnknown && progress.WaitingReason == "mcp_result_unknown" ||
			s.failSuccess && step.Status == task.StepSucceeded {
			s.failureUsed = true
			return task.Event{}, errors.New("synthetic Task result receipt was lost after MCP ledger write")
		}
	}
	return s.Store.CommitTaskProgress(ctx, progress)
}

func TestPostgresMCPBrokerReconcileUsesPersistentConnectorLedger(t *testing.T) {
	tests := []struct {
		name         string
		clientResult app.MCPConnectorToolResult
		clientErr    error
		wantLedger   task.OperationStatus
		wantResult   app.AgentToolResultStatus
		inject       func(*m09PostgresMCPFaultStore)
	}{
		{
			name:       "unknown result remains unknown without redispatch",
			clientErr:  context.DeadlineExceeded,
			wantLedger: task.OperationUnknown,
			wantResult: app.AgentToolUnknown,
			inject:     func(store *m09PostgresMCPFaultStore) { store.failUnknown = true },
		},
		{
			name: "durable success is reconciled after Task result commit failure",
			clientResult: app.MCPConnectorToolResult{Text: []string{"synthetic update accepted"},
				StructuredContent: json.RawMessage(`{"updated":true}`)},
			wantLedger: task.OperationSucceeded,
			wantResult: app.AgentToolSucceeded,
			inject:     func(store *m09PostgresMCPFaultStore) { store.failSuccess = true },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var connectorID string
			fixture := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
				connectors := createEnabledMCPGrantConnectors(t, route.store, route.ownerID)
				connectorID = connectors[0].ID
				return []string{connectorID}
			})
			client := &m09PostgresMCPClient{callResult: test.clientResult, callErr: test.clientErr}

			initialBroker, err := app.NewCloudDSHToolBroker(fixture.route.store, &app.RuntimeToolExecutor{}, m09PostgresMCPCredentials{}, client)
			if err != nil {
				t.Fatal(err)
			}
			discovery := app.ModelToolCall{CallID: "m09-discover-" + fixture.task.ID, Name: "mcp_discover",
				Arguments: `{"connector_id":"` + connectorID + `"}`, TaskFence: m09TaskFence(fixture)}
			discovered, err := initialBroker.Execute(ctx, fixture.task, discovery)
			if err != nil || discovered.Status != app.AgentToolSucceeded {
				t.Fatalf("discover synthetic connector: result=%+v err=%v", discovered, err)
			}

			callID := "m09-reconcile-" + fixture.task.ID
			call := app.ModelToolCall{CallID: callID, Name: "mcp_call",
				Arguments: `{"connector_id":"` + connectorID + `","tool_name":"update_note","arguments":{"text":"synthetic"}}`,
				TaskFence: m09TaskFence(fixture)}
			approval, err := initialBroker.Execute(ctx, fixture.task, call)
			if err != nil || approval.Status != app.AgentToolNeedsUser {
				t.Fatalf("request one-action MCP approval: result=%+v err=%v", approval, err)
			}
			details, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
			if err != nil || details.PendingRequest == nil || details.PendingRequest.Status != task.PendingRequestPending {
				t.Fatalf("read pending approval: request=%+v err=%v", details.PendingRequest, err)
			}
			pending := *details.PendingRequest
			if _, err := fixture.instances.ConfirmStopped(ctx,
				signAgentLifecycleReceipt(t, fixture.key, fixture.fence, fixture.nonce)); err != nil {
				t.Fatalf("confirm initial Agent stopped at approval wait: %v", err)
			}
			response := testIdempotency(fixture.task.OwnerID, "task.pending.respond", "m09-reconcile-approval-"+fixture.task.ID)
			response.ResourceID, response.RelatedID = fixture.task.ID, pending.ID
			if err := fixture.route.store.RespondToPendingRequest(ctx, response, fixture.task.ID, pending.ID,
				pending.Version, task.PendingDecisionApprove); err != nil {
				t.Fatalf("approve exact synthetic MCP call: %v", err)
			}
			startSelectedMCPAgent(t, &fixture, 1)
			call.TaskFence = m09TaskFence(fixture)

			faultStore := &m09PostgresMCPFaultStore{Store: fixture.route.store, callID: callID}
			test.inject(faultStore)
			faultBroker, err := app.NewCloudDSHToolBroker(faultStore, &app.RuntimeToolExecutor{}, m09PostgresMCPCredentials{}, client)
			if err != nil {
				t.Fatal(err)
			}
			first, firstErr := faultBroker.Execute(ctx, fixture.task, call)
			if test.wantLedger == task.OperationUnknown {
				if firstErr == nil || first.Status != app.AgentToolUnknown || !faultStore.failureUsed {
					t.Fatalf("lost unknown-result Task receipt: result=%+v err=%v injected=%t", first, firstErr, faultStore.failureUsed)
				}
			} else if firstErr == nil || !faultStore.failureUsed {
				t.Fatalf("expected synthetic Task result commit failure, result=%+v err=%v injected=%t", first, firstErr, faultStore.failureUsed)
			}
			if client.calls() != 1 {
				t.Fatalf("synthetic connector calls before reconciliation=%d, want 1", client.calls())
			}

			var ledgerKey string
			if err := fixture.route.store.pool.QueryRow(ctx, `SELECT operation_key FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`,
				fixture.task.ID, callID).Scan(&ledgerKey); err != nil {
				t.Fatalf("find PostgreSQL MCP operation ledger: %v", err)
			}
			ledger, err := fixture.route.store.GetMCPConnectorOperation(ctx, fixture.task.OwnerID, fixture.task.ID, connectorID, ledgerKey)
			if err != nil || ledger.Status != test.wantLedger || ledger.CallID != callID {
				t.Fatalf("persistent MCP ledger=%+v err=%v; want status %s for call %s", ledger, err, test.wantLedger, callID)
			}

			// The Broker has no in-memory copy of the connector result. Rebuild it
			// against the same PostgreSQL-backed Store and recover by immutable call ID.
			restartedBroker, err := app.NewCloudDSHToolBroker(fixture.route.store, &app.RuntimeToolExecutor{}, m09PostgresMCPCredentials{}, client)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := restartedBroker.Reconcile(ctx, fixture.task, call, nil)
			if err != nil || recovered.Status != test.wantResult || client.calls() != 1 {
				t.Fatalf("reconcile durable MCP result=%+v err=%v connector calls=%d; want status=%s and no redispatch",
					recovered, err, client.calls(), test.wantResult)
			}
			persisted, err := fixture.route.store.GetMCPConnectorOperation(ctx, fixture.task.OwnerID, fixture.task.ID, connectorID, ledgerKey)
			if err != nil || persisted.Status != test.wantLedger || client.calls() != 1 {
				t.Fatalf("ledger after reconciliation=%+v err=%v connector calls=%d", persisted, err, client.calls())
			}
			if test.wantLedger == task.OperationUnknown {
				details, err = fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
				if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "mcp_result_unknown" {
					t.Fatalf("unconfirmed MCP result did not remain waiting: task=%+v err=%v", details.Task, err)
				}
			} else {
				replayed, replayErr := restartedBroker.Reconcile(ctx, fixture.task, call, nil)
				if replayErr != nil || replayed.Status != app.AgentToolSucceeded || client.calls() != 1 {
					t.Fatalf("repeat reconciliation redispatched or changed result: result=%+v err=%v calls=%d",
						replayed, replayErr, client.calls())
				}
				details, err = fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
				if err != nil || details.Task.Status != task.StatusRunning {
					t.Fatalf("reconciled MCP success did not keep Task running: task=%+v err=%v", details.Task, err)
				}
				var callSteps []task.Step
				for _, step := range details.Steps {
					if step.ToolCallKey == callID {
						callSteps = append(callSteps, step)
					}
				}
				if len(callSteps) != 1 || callSteps[0].Status != task.StepSucceeded {
					t.Fatalf("durable MCP success should settle one original step exactly once: %+v", callSteps)
				}
			}
		})
	}
}

func m09TaskFence(fixture selectedMCPAgentFixture) *app.ModelTaskFence {
	return &app.ModelTaskFence{
		TaskID: fixture.task.ID, OwnerID: fixture.task.OwnerID, InitiatorID: fixture.task.InitiatorID,
		LeaseOwner: fixture.task.Lease.Owner, LeaseVersion: fixture.task.Lease.Version,
		AgentInstanceID: fixture.fence.AgentInstanceID, AgentGeneration: fixture.fence.AgentGeneration,
		AgentSessionID: fixture.fence.AgentSessionID, AgentRootSessionID: fixture.fence.AgentSessionID,
		AuthorizationVersion: fixture.fence.AuthorizationVersion, ThroughSequence: 0,
		EngineVersion: fixture.fence.EngineVersion, ProfileVersion: fixture.fence.ProfileVersion,
	}
}
