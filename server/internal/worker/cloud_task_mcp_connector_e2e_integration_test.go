package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

func TestCloudTaskMCPConnectorSurvivesNodeRestartWithoutRedispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x39}, 32))
	if err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify task session = %#v, %v", principal, err)
	}
	connectorService, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	connector, err := connectorService.Create(ctx, principal, app.CreateMCPConnectorCommand{
		Name: "Local MCP test", EndpointURL: "https://connector.example.test/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	connector, err = connectorService.SetEnabled(ctx, principal, connector.ID, true, app.MCPConnectorRevisionCommand{ExpectedRevision: connector.Revision})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Look up the record and summarize it.",
		Budget:          task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000},
		MCPConnectorIDs: []string{connector.ID}, IdempotencyKey: "f01-mcp-node-restart-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "f01-mcp-node-worker", 30*time.Second)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim MCP task = id %s found=%t err=%v", claimed.ID, found, err)
	}

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close unused Workspace block store: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	provider := newF01LocalMCPProvider()
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, connectorService, provider)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real dsh subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath(node); err != nil {
		t.Fatalf("locate Node executable: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x41}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-mcp-restart-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	model := &f01MCPRestartModel{}
	supervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	interruptingTools := &f01InterruptAfterMCPDispatch{inner: broker, supervisor: supervisor}
	firstRun, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-mcp-restart-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Models: model, Tools: interruptingTools,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRunErr := firstRun.ExecuteTask(ctx, claimed)
	if firstRunErr == nil {
		t.Fatal("first dsh worker unexpectedly survived the injected process interruption")
	}
	if !interruptingTools.interrupted.Load() {
		t.Fatalf("the interruption did not occur after the MCP ledger committed: err=%v model_calls=%d provider_calls=%d dsh_frames=%s",
			firstRunErr, model.callCount(), provider.remoteCalls.Load(), supervisor.output())
	}
	operationKey := f01OperationKey("f01-mcp-call-001")
	operation, err := store.GetMCPConnectorOperation(ctx, owner.ID, submitted.Task.ID, connector.ID, operationKey)
	if err != nil || operation.Status != task.OperationSucceeded || operation.Result == "" {
		t.Fatalf("persisted MCP operation after interruption = %#v, %v", operation, err)
	}
	if provider.remoteCalls.Load() != 1 {
		t.Fatalf("local MCP provider calls after first run = %d, want 1", provider.remoteCalls.Load())
	}
	afterInterruption, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || afterInterruption.Task.AgentSessionID == "" || afterInterruption.Task.AgentGeneration < 1 ||
		afterInterruption.Task.CheckpointThroughSequence < 0 || len(afterInterruption.Task.CheckpointDigest) != 32 {
		t.Fatalf("persisted dsh recovery point after interruption = %#v, %v", afterInterruption.Task, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "f01-mcp-node-recovery", 30*time.Second)
	if err != nil || !found || recovered.ID != submitted.Task.ID || recovered.AgentSessionID != afterInterruption.Task.AgentSessionID ||
		recovered.AgentGeneration != afterInterruption.Task.AgentGeneration || recovered.CheckpointThroughSequence < 0 || len(recovered.CheckpointDigest) != 32 {
		t.Fatalf("claim recoverable MCP task = %#v found=%t err=%v", recovered, found, err)
	}
	secondRun, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-mcp-restart-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Models: model, Tools: broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRun.ExecuteTask(ctx, recovered); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("recovered dsh run = %v, want completion pending without a production completion adapter", err)
	}
	operation, err = store.GetMCPConnectorOperation(ctx, owner.ID, submitted.Task.ID, connector.ID, operationKey)
	if err != nil || operation.Status != task.OperationSucceeded || provider.remoteCalls.Load() != 1 || model.callCount() != 2 {
		t.Fatalf("MCP recovery duplicated side effects or lost the result: operation=%#v err=%v provider_calls=%d model_calls=%d",
			operation, err, provider.remoteCalls.Load(), model.callCount())
	}
	details, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || details.Task.CheckpointThroughSequence <= recovered.CheckpointThroughSequence || details.Task.AgentGeneration <= recovered.AgentGeneration {
		t.Fatalf("recovery did not commit a new checkpoint and Agent generation: task=%#v err=%v", details.Task, err)
	}
}

type f01InterruptAfterMCPDispatch struct {
	inner       *app.CloudDSHToolBroker
	supervisor  *f01CapturingSupervisor
	interrupted atomic.Bool
}

func (tools *f01InterruptAfterMCPDispatch) Definitions(ctx context.Context, value task.Task) ([]app.AgentToolDefinition, error) {
	return tools.inner.Definitions(ctx, value)
}

func (tools *f01InterruptAfterMCPDispatch) Execute(ctx context.Context, value task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	result, err := tools.inner.Execute(ctx, value, call)
	if err == nil && strings.HasPrefix(call.Name, "mcp_") && tools.interrupted.CompareAndSwap(false, true) {
		tools.supervisor.child.mu.Lock()
		cmd := tools.supervisor.child.cmd
		tools.supervisor.child.mu.Unlock()
		if cmd == nil || cmd.Process == nil {
			return app.AgentToolResult{}, errors.New("dsh child is unavailable for restart test")
		}
		if err := cmd.Process.Kill(); err != nil {
			return app.AgentToolResult{}, err
		}
	}
	return result, err
}

func (tools *f01InterruptAfterMCPDispatch) Reconcile(ctx context.Context, value task.Task, call app.ModelToolCall, expected *string) (app.AgentToolResult, error) {
	return tools.inner.Reconcile(ctx, value, call, expected)
}

func (tools *f01InterruptAfterMCPDispatch) PauseRecovery(ctx context.Context, value task.Task) error {
	return tools.inner.PauseRecovery(ctx, value)
}

type f01LocalMCPProvider struct {
	server      *sdk.Server
	remoteCalls atomic.Int32
}

func newF01LocalMCPProvider() *f01LocalMCPProvider {
	provider := &f01LocalMCPProvider{server: sdk.NewServer(&sdk.Implementation{Name: "f01-local-mcp", Version: "1"}, nil)}
	provider.server.AddTool(&sdk.Tool{
		Name: "lookup", Description: "Look up a record.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		provider.remoteCalls.Add(1)
		return &sdk.CallToolResult{
			Content:           []sdk.Content{&sdk.TextContent{Text: "one record"}},
			StructuredContent: map[string]any{"record_id": "R-17"},
		}, nil
	})
	return provider
}

func (provider *f01LocalMCPProvider) session(ctx context.Context) (*sdk.ClientSession, func(), error) {
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := provider.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, nil, err
	}
	clientSession, err := sdk.NewClient(&sdk.Implementation{Name: "f01-local-client", Version: "1"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, nil, err
	}
	return clientSession, func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	}, nil
}

func (provider *f01LocalMCPProvider) ListTools(ctx context.Context, _ string, _ http.Header) ([]app.MCPConnectorToolDefinition, error) {
	session, closeSession, err := provider.session(ctx)
	if err != nil {
		return nil, err
	}
	defer closeSession()
	result, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	tools := make([]app.MCPConnectorToolDefinition, 0, len(result.Tools))
	for _, tool := range result.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, err
		}
		tools = append(tools, app.MCPConnectorToolDefinition{
			Name: tool.Name, Description: tool.Description, InputSchema: schema,
		})
	}
	return tools, nil
}

func (provider *f01LocalMCPProvider) CallTool(ctx context.Context, _ string, _ http.Header, name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	var values map[string]any
	if err := json.Unmarshal(arguments, &values); err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	session, closeSession, err := provider.session(ctx)
	if err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	defer closeSession()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: values})
	if err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	output := app.MCPConnectorToolResult{IsError: result.IsError}
	if result.StructuredContent != nil {
		output.StructuredContent, err = json.Marshal(result.StructuredContent)
		if err != nil {
			return app.MCPConnectorToolResult{}, err
		}
	}
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			output.Text = append(output.Text, text.Text)
		}
	}
	return output, nil
}

type f01MCPRestartModel struct {
	mu    sync.Mutex
	calls int
}

func (model *f01MCPRestartModel) callCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

func (model *f01MCPRestartModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.calls++
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}
	if model.calls == 1 {
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(call.RawRequest, &request); err != nil {
			return app.ModelResponse{}, err
		}
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "mcp_") {
				return app.ModelResponse{StopReason: "tool_calls", Usage: usage, ToolCalls: []app.ModelToolCall{{
					CallID: "f01-mcp-call-001", Name: tool.Name, Arguments: `{"query":"test record"}`,
				}}}, nil
			}
		}
		return app.ModelResponse{}, errors.New("MCP tool was not offered to the model")
	}
	if model.calls == 2 {
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(call.RawRequest, &request); err != nil {
			return app.ModelResponse{}, err
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == "f01-mcp-call-001" && len(message.Content) > 0 &&
				strings.Contains(message.Content[0].Text, "R-17") {
				return app.ModelResponse{Text: "Found record R-17.", StopReason: "stop", Usage: usage}, nil
			}
		}
		return app.ModelResponse{}, errors.New("recovered MCP result was not included in the next model request")
	}
	return app.ModelResponse{}, errors.New("unexpected dsh model call")
}

var _ app.MCPConnectorToolClient = (*f01LocalMCPProvider)(nil)
