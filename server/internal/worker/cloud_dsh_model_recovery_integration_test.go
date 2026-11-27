package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

const (
	t07RecoverySearchCallID = "t07_public_search_001"
	t07RecoverySearchQuery  = "synthetic public recovery evidence"
	t07RecoverySearchResult = `{"results":[{"title":"Synthetic recovery source","url":"https://recovery.example.test/source","snippet":"Durable synthetic search evidence."}]}`
	t07RecoveryFinalResult  = "The synthetic source confirms the durable finding. [Source](https://recovery.example.test/source)."
	t07RecoveryMemoryProof  = "t07-recovery-synthetic-memory-proof"
)

// TestCloudDSHModelRecoveryResumesAfterSettledSearch exercises the real local
// DSH runtime, PostgreSQL task/checkpoint/model ledgers, and the worker claim
// transition. The provider and public search adapter are synthetic and local.
func TestCloudDSHModelRecoveryResumesAfterSettledSearch(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic Task execution: %v", err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic user session: principal=%+v err=%v", principal, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	provider := &t07RecoveryProvider{}
	modelKey, err := sealer.Seal([]byte("t07-local-synthetic-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	window := 32_768
	connection, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: "p1234567", OwnerID: owner.ID, Name: "T07 local synthetic model",
		BaseURL: "https://models.example.invalid/v1", ModelName: "t07-synthetic-model",
		ContextWindowTokens: &window, ContextWindowOverrideTokens: &window,
		ContextWindowSource: app.ModelContextWindowLegacyManual, APIKeyCipher: modelKey,
	})
	if err != nil {
		t.Fatalf("create local synthetic model connection: %v", err)
	}
	checkedAt := time.Now().UTC()
	connection.Capabilities = app.ModelCapabilities{Text: true, Tools: true, ModelID: "t07-synthetic-model"}
	connection.CheckedAt = &checkedAt
	if _, err := store.UpdateModelConnection(ctx, connection); err != nil {
		t.Fatalf("enable checked text/tool capabilities for synthetic model: %v", err)
	}
	modelConnections, err := app.NewModelConnectionService(store, sealer, provider, []string{"models.example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, modelConnections, provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	models := &t07RecoveryRecordingGateway{inner: gateway}

	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Search the public web for the synthetic recovery source and report its finding.",
		Budget:         task.Budget{ActiveSeconds: 90, ToolCalls: 6, ModelTokens: 50_000},
		IdempotencyKey: "t07-model-recovery-postgres-e2e",
	})
	if err != nil {
		t.Fatalf("submit synthetic recovery Task: %v", err)
	}

	blocks, err := workspacefs.Open(t.TempDir() + "/workspace-blocks")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close synthetic Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	search := &t07RecoverySearchAdapter{}
	if err := broker.SetBuiltinSearch(search); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskModelRecoveryAutoEnabled(true); err != nil {
		t.Fatal(err)
	}
	tools := &t07RecoveryTools{CloudDSHToolBroker: broker, pool: pool}

	node, dshScript := f01NodeRuntimePaths(t)
	const supervisorID = "t07-model-recovery-supervisor"
	stopKey := bytes.Repeat([]byte{0x62}, 32)
	instances, accountRuntimes, supervisor := newF01ConversationRuntime(t, store, node, dshScript, supervisorID, stopKey)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	makeExecutor := func() *CloudDSHExecutor {
		t.Helper()
		executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
			SupervisorID: supervisorID, AccountRuntimes: accountRuntimes, Instances: instances,
			Checkpoints: checkpoints, Models: models, ToolCapabilities: gateway,
			Tools: tools, Completion: broker,
		})
		if err != nil {
			t.Fatalf("create synthetic recovery Cloud DSH executor: %v", err)
		}
		return executor
	}

	firstWorkerID := "t07-model-recovery-worker-one"
	firstClaim, found, err := store.ClaimNextTask(ctx, firstWorkerID, time.Minute)
	if err != nil || !found || firstClaim.ID != submitted.Task.ID || firstClaim.ConversationID != submitted.Conversation.ID {
		t.Fatalf("first worker claim: task=%+v found=%t err=%v", firstClaim, found, err)
	}
	firstRunErr := makeExecutor().ExecuteTask(ctx, firstClaim)
	if firstRunErr == nil {
		t.Fatal("first DSH run unexpectedly completed after the synthetic provider rejection")
	}

	providerCalls := provider.snapshot()
	modelCalls := models.snapshot()
	if len(providerCalls) != 2 || len(modelCalls) != 2 || providerCalls[0].err != nil || providerCalls[1].err == nil {
		t.Fatalf("first DSH run error=%v startup=%s provider=%+v model_calls=%d", firstRunErr,
			supervisor.child.startupDiagnostic(firstRunErr), providerCalls, len(modelCalls))
	}
	if !providerCalls[1].sawSearchResult {
		t.Fatal("the model call that failed did not receive the settled public search result")
	}
	if modelCalls[0].call.Task == nil || modelCalls[1].call.Task == nil || firstClaim.ID != modelCalls[0].call.Task.TaskID ||
		modelCalls[0].call.Task.RecoveryCallID != "" || modelCalls[1].call.Task.RecoveryCallID != "" ||
		modelCalls[1].call.Task.ThroughSequence <= modelCalls[0].call.Task.ThroughSequence {
		t.Fatalf("initial DSH model calls did not advance after the search checkpoint: %#v", modelCalls)
	}

	var searchOperationCount int
	var searchOperationState, searchOperationResult string
	if err := pool.QueryRow(ctx, `SELECT count(*),max(status),max(result) FROM task_mcp_connector_operations
WHERE task_id=$1 AND owner_id=$2 AND tool_name='morde_web_search'`, submitted.Task.ID, owner.ID).
		Scan(&searchOperationCount, &searchOperationState, &searchOperationResult); err != nil || searchOperationCount != 1 ||
		searchOperationState != string(task.OperationSucceeded) || !strings.Contains(searchOperationResult, "recovery.example.test/source") {
		t.Fatalf("settled public search operation count=%d state=%q result=%q err=%v",
			searchOperationCount, searchOperationState, searchOperationResult, err)
	}
	var failedLedgerState string
	var failedInputTokens, failedOutputTokens int64
	if err := pool.QueryRow(ctx, `SELECT state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, owner.ID, modelCalls[1].call.CallID).
		Scan(&failedLedgerState, &failedInputTokens, &failedOutputTokens); err != nil || failedLedgerState != "settled" ||
		failedInputTokens != 0 || failedOutputTokens != 0 {
		t.Fatalf("transient model failure ledger state=%q usage=%d/%d err=%v", failedLedgerState,
			failedInputTokens, failedOutputTokens, err)
	}
	var failedResponseRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_model_call_responses
WHERE owner_id=$1 AND source='task' AND call_id=$2`, owner.ID, modelCalls[1].call.CallID).Scan(&failedResponseRows); err != nil || failedResponseRows != 0 {
		t.Fatalf("transient failed model call response rows=%d err=%v", failedResponseRows, err)
	}

	afterFailure, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || afterFailure.Task.Status != task.StatusQueued || afterFailure.Task.ModelRecovery == nil ||
		afterFailure.Task.ModelRecovery.State != task.ModelRecoveryScheduled ||
		afterFailure.Task.ModelRecovery.FailedCallID != modelCalls[1].call.CallID ||
		afterFailure.Task.ModelRecovery.CheckpointThroughSequence != modelCalls[1].call.Task.ThroughSequence ||
		afterFailure.Task.CheckpointThroughSequence != afterFailure.Task.ModelRecovery.CheckpointThroughSequence ||
		len(afterFailure.Task.ModelRecovery.CheckpointDigest) != 32 || len(afterFailure.Task.ModelRecovery.ToolsetDigest) != 32 {
		t.Fatalf("durable recovery schedule after confirmed failure: task=%+v err=%v", afterFailure.Task, err)
	}
	if firstClaim.Lease.Owner == afterFailure.Task.Lease.Owner || afterFailure.Task.Lease.Owner != "" {
		t.Fatalf("recovery scheduling did not release the first worker lease: first=%+v after=%+v",
			firstClaim.Lease, afterFailure.Task.Lease)
	}

	secondWorkerID := "t07-model-recovery-worker-two"
	secondClaim, found, err := store.ClaimNextTask(ctx, secondWorkerID, time.Minute)
	if err != nil || !found || secondClaim.ID != submitted.Task.ID || secondClaim.ModelRecovery == nil ||
		secondClaim.ModelRecovery.State != task.ModelRecoveryScheduled || secondClaim.Lease.Owner != secondWorkerID ||
		secondClaim.Lease.Version <= firstClaim.Lease.Version || secondClaim.ConversationID != submitted.Conversation.ID {
		t.Fatalf("second worker did not reclaim the same queued recovery Task: task=%+v found=%t err=%v", secondClaim, found, err)
	}
	if err := makeExecutor().ExecuteTask(ctx, secondClaim); err != nil {
		afterRecoveryFailure, _ := store.GetTask(ctx, submitted.Task.ID, owner.ID)
		recoveryState := "missing"
		if afterRecoveryFailure.Task.ModelRecovery != nil {
			recoveryState = string(afterRecoveryFailure.Task.ModelRecovery.State)
		}
		failureDetails := "no typed model-result diagnostic"
		var modelFailure cloudDSHModelResultError
		if errors.As(err, &modelFailure) {
			failureDetails = fmt.Sprintf("model_failure={stage=%s category=%s reason=%s tool=%s tool_call_id=%s}",
				modelFailure.stage, modelFailure.category, modelFailure.reasonCode, modelFailure.toolName, modelFailure.toolCallID)
		}
		calls := models.snapshot()
		callSummary := make([]string, 0, len(calls))
		gatewayErrors := make([]string, 0, len(calls))
		for index, recorded := range calls {
			call := recorded.call
			taskFence := "none"
			if call.Task != nil {
				taskFence = fmt.Sprintf("task=%s session=%s generation=%d through=%d revision=%d recovery_dsh=%s recovery_ledger=%s",
					call.Task.TaskID, call.Task.AgentSessionID, call.Task.AgentGeneration, call.Task.ThroughSequence,
					call.Task.TaskRevision, call.Task.RecoveryDSHCallID, call.Task.RecoveryCallID)
			}
			callSummary = append(callSummary, fmt.Sprintf("%d call_id=%s %s", index, call.CallID, taskFence))
			if recorded.err != "" {
				gatewayErrors = append(gatewayErrors, fmt.Sprintf("%d %s", index, recorded.err))
			}
		}
		t.Fatalf("recovery worker execution: %v; %s; persisted_recovery_state=%s; recovery_start_error=%s; provider=%+v; model_calls=%+v; gateway_errors=%v; runtime_frames=%v; admissions=%v; search_calls=%d",
			err, failureDetails, recoveryState, tools.recoveryStartFailure(), provider.snapshot(), callSummary, gatewayErrors,
			t07RecoveryRuntimeSummary([]byte(supervisor.output())), tools.admissionFailures(), search.callCount())
	}

	providerCalls = provider.snapshot()
	modelCalls = models.snapshot()
	if len(providerCalls) != 3 || len(modelCalls) != 3 || providerCalls[2].err != nil || !providerCalls[2].sawSearchResult {
		t.Fatalf("resumed DSH provider/model calls: provider=%+v model_calls=%d", providerCalls, len(modelCalls))
	}
	recoveryCall := modelCalls[2].call
	if recoveryCall.Task == nil || recoveryCall.Task.TaskID != submitted.Task.ID ||
		recoveryCall.Task.RecoveryCallID == "" || recoveryCall.Task.RecoveryDSHCallID == "" ||
		recoveryCall.CallID != recoveryCall.Task.RecoveryDSHCallID ||
		recoveryCall.Task.RecoveryCallID == modelCalls[1].call.CallID ||
		recoveryCall.Task.AgentSessionID != modelCalls[1].call.Task.AgentSessionID ||
		recoveryCall.Task.AgentGeneration <= modelCalls[1].call.Task.AgentGeneration ||
		recoveryCall.Task.LeaseVersion != secondClaim.Lease.Version {
		t.Fatalf("recovery model call did not resume the original root session under the new lease: %+v", recoveryCall)
	}
	if got := search.callCount(); got != 1 {
		t.Fatalf("public search dispatched %d times, want exactly once", got)
	}
	var finalSearchOperationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_mcp_connector_operations
WHERE task_id=$1 AND owner_id=$2 AND tool_name='morde_web_search' AND status='succeeded'`, submitted.Task.ID, owner.ID).
		Scan(&finalSearchOperationCount); err != nil || finalSearchOperationCount != 1 {
		t.Fatalf("successful public search operation count after recovery=%d err=%v", finalSearchOperationCount, err)
	}

	completed, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != t07RecoveryFinalResult ||
		completed.Task.ConversationID != submitted.Conversation.ID {
		t.Fatalf("completed recovery Task=%+v err=%v", completed.Task, err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, submitted.Conversation.ID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	assistantResults := 0
	for _, message := range messages.Items {
		if message.Role != conversation.RoleAssistant || message.TaskID != submitted.Task.ID {
			continue
		}
		assistantResults++
		if message.ConversationID != submitted.Conversation.ID || message.Content != t07RecoveryFinalResult {
			t.Fatalf("assistant result was delivered to the wrong conversation/task or with wrong content: %+v", message)
		}
	}
	if assistantResults != 1 {
		t.Fatalf("original conversation has %d assistant result messages for Task %s, want exactly one", assistantResults, submitted.Task.ID)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resultEvents := 0
	for _, event := range events {
		if event.Type == "task.result" {
			resultEvents++
		}
	}
	if resultEvents != 1 {
		t.Fatalf("Task has %d terminal result events, want exactly one", resultEvents)
	}
}

func requireT07LocalPostgres(t *testing.T) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local disposable PostgreSQL integration was not run")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for local PostgreSQL validation")
	}
	host := strings.Trim(strings.TrimSpace(config.ConnConfig.Host), "[]")
	if host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1" && !strings.HasPrefix(host, "/") {
		t.Fatalf("refusing non-local PostgreSQL host for recovery E2E: %q", host)
	}
}

type t07RecoveryProviderCall struct {
	sawSearchResult bool
	err             error
}

type t07RecoveryProvider struct {
	mu    sync.Mutex
	calls []t07RecoveryProviderCall
}

func (p *t07RecoveryProvider) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	p.mu.Lock()
	callNumber := len(p.calls) + 1
	p.mu.Unlock()
	sawSearch := false
	for _, input := range request.Input {
		if input.Type == "function_call_output" && input.CallID == t07RecoverySearchCallID &&
			strings.Contains(input.Output, "recovery.example.test/source") {
			sawSearch = true
		}
	}
	call := t07RecoveryProviderCall{sawSearchResult: sawSearch}
	usage := app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 4}
	var response app.ModelResponse
	switch callNumber {
	case 1:
		if !toolOffered(request.Tools, "morde_web_search") {
			call.err = errors.New("synthetic model did not receive public search tool")
			break
		}
		response = app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: t07RecoverySearchCallID,
			Name: "morde_web_search", Arguments: `{"query":"synthetic public recovery evidence"}`}},
			StopReason: "tool_calls", Usage: usage, ProviderReportedModel: "t07-synthetic-model"}
	case 2:
		if !sawSearch {
			call.err = errors.New("model request after search omitted the settled result")
			break
		}
		call.err = &app.ModelClientError{Kind: app.ModelErrorRateLimited, Stage: "provider_status", ConfirmedNoUsage: true}
	case 3:
		if !sawSearch {
			call.err = errors.New("recovered model request omitted the persisted search result")
			break
		}
		response = app.ModelResponse{Text: t07RecoveryFinalResult, StopReason: "stop", Usage: usage,
			ProviderReportedModel: "t07-synthetic-model"}
	default:
		call.err = fmt.Errorf("unexpected synthetic provider call %d", callNumber)
	}
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
	if call.err != nil {
		return app.ModelResponse{}, call.err
	}
	return response, nil
}

func (p *t07RecoveryProvider) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true, Tools: true, ModelID: "t07-synthetic-model"}, nil
}

func (p *t07RecoveryProvider) snapshot() []t07RecoveryProviderCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]t07RecoveryProviderCall(nil), p.calls...)
}

type t07RecoveryRecordedModelCall struct {
	call app.ModelCall
	err  string
}

type t07RecoveryRecordingGateway struct {
	inner *app.ModelGatewayService
	mu    sync.Mutex
	calls []t07RecoveryRecordedModelCall
}

func (g *t07RecoveryRecordingGateway) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	copyCall := call
	copyCall.RawRequest = append(json.RawMessage(nil), call.RawRequest...)
	if call.Task != nil {
		fence := *call.Task
		copyCall.Task = &fence
	}
	g.mu.Lock()
	index := len(g.calls)
	g.calls = append(g.calls, t07RecoveryRecordedModelCall{call: copyCall})
	g.mu.Unlock()
	response, err := g.inner.Complete(ctx, call)
	if err != nil {
		g.mu.Lock()
		g.calls[index].err = fmt.Sprintf("%T: %s", err, err.Error())
		g.mu.Unlock()
	}
	return response, err
}

func (g *t07RecoveryRecordingGateway) snapshot() []t07RecoveryRecordedModelCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]t07RecoveryRecordedModelCall(nil), g.calls...)
}

type t07RecoverySearchAdapter struct {
	mu    sync.Mutex
	calls int
}

func (*t07RecoverySearchAdapter) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return nil, errors.New("synthetic built-in search definitions are installed locally")
}

func (a *t07RecoverySearchAdapter) CallTool(ctx context.Context, endpoint string, headers http.Header,
	name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	if err := ctx.Err(); err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	var input struct {
		Query string `json:"query"`
	}
	if endpoint != "" || len(headers) != 0 || name != "searxng_web_search" ||
		json.Unmarshal(arguments, &input) != nil || input.Query != t07RecoverySearchQuery {
		return app.MCPConnectorToolResult{}, fmt.Errorf("unexpected synthetic search request %q %s", name, arguments)
	}
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return app.MCPConnectorToolResult{Text: []string{t07RecoverySearchResult}}, nil
}

func (a *t07RecoverySearchAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type t07RecoveryTools struct {
	*app.CloudDSHToolBroker
	mu              sync.Mutex
	admissionErrors []string
	recoveryError   string
	pool            *pgxpool.Pool
}

func (tools *t07RecoveryTools) BeginTaskModelRecoveryAttempt(ctx context.Context, intent task.ModelRecoveryIntent,
	fence app.AgentInstanceFence) error {
	err := tools.CloudDSHToolBroker.BeginTaskModelRecoveryAttempt(ctx, intent, fence)
	if err != nil {
		tools.mu.Lock()
		tools.recoveryError = err.Error()
		tools.mu.Unlock()
	}
	return err
}

func (tools *t07RecoveryTools) recoveryStartFailure() string {
	tools.mu.Lock()
	defer tools.mu.Unlock()
	return tools.recoveryError
}

func (tools *t07RecoveryTools) AdmitToolCall(ctx context.Context, value task.Task, call app.ModelToolCall,
	recovering bool) (app.AgentToolCallAdmission, error) {
	admission, err := tools.CloudDSHToolBroker.AdmitToolCall(ctx, value, call, recovering)
	if err != nil {
		storedFence := "unavailable"
		if call.TaskFence != nil && tools.pool != nil {
			var instance, rootSession, callSession string
			var generation, through, rootThrough int64
			if queryErr := tools.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,root_session_id,call_session_id,through_sequence,root_through_sequence
FROM task_agent_tool_call_admissions WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3`,
				call.TaskFence.TaskID, call.TaskFence.OwnerID, app.AgentOperationKey(call.CallID)).
				Scan(&instance, &generation, &rootSession, &callSession, &through, &rootThrough); queryErr == nil {
				storedFence = fmt.Sprintf("instance=%s generation=%d root=%s session=%s through=%d root_through=%d",
					instance, generation, rootSession, callSession, through, rootThrough)
			}
		}
		tools.mu.Lock()
		tools.admissionErrors = append(tools.admissionErrors,
			fmt.Sprintf("name=%s call_id=%s recovering=%t error=%v stored={%s} current={instance=%s generation=%d root=%s session=%s through=%d}",
				call.Name, call.CallID, recovering, err, storedFence, call.TaskFence.AgentInstanceID, call.TaskFence.AgentGeneration,
				call.TaskFence.AgentRootSessionID, call.TaskFence.AgentSessionID, call.TaskFence.ThroughSequence))
		tools.mu.Unlock()
	}
	return admission, err
}

func (tools *t07RecoveryTools) admissionFailures() []string {
	tools.mu.Lock()
	defer tools.mu.Unlock()
	return append([]string(nil), tools.admissionErrors...)
}

func (tools *t07RecoveryTools) Definitions(ctx context.Context, value task.Task) ([]app.AgentToolDefinition, error) {
	definitions, err := tools.CloudDSHToolBroker.Definitions(ctx, value)
	if err != nil {
		return nil, err
	}
	filtered := make([]app.AgentToolDefinition, 0, 2)
	for _, definition := range definitions {
		if definition.Name == "morde_web_search" || definition.Name == "morde_web_read" {
			filtered = append(filtered, definition)
		}
	}
	if len(filtered) != 2 {
		return nil, fmt.Errorf("expected both public search tools, got %d", len(filtered))
	}
	return filtered, nil
}

func (*t07RecoveryTools) InitialContext(context.Context, task.Task) (string, error) {
	return "Synthetic test context.\n" + app.CloudDSHMemoryProofPrefix + t07RecoveryMemoryProof, nil
}

func (*t07RecoveryTools) ValidatePersonalMemoryProof(_ context.Context, _ task.Task, proof string,
	versions []app.CloudDSHMemoryVersion) error {
	if proof != t07RecoveryMemoryProof || len(versions) != 0 {
		return errors.New("unexpected synthetic memory proof")
	}
	return nil
}

func (*t07RecoveryTools) CloudDSHPersonalMemoryProofEnabled() bool { return true }

func (*t07RecoveryTools) CloudDSHPersonalMemoryProofEnabledForTask(task.Task) bool { return true }

func toolOffered(tools []app.ModelTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func t07RecoveryRuntimeSummary(output []byte) []string {
	type frame struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Through   int64  `json:"through_seq"`
		Code      string `json:"code"`
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		CallID    string `json:"call_id"`
	}
	var summary []string
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var value frame
		if json.Unmarshal(line, &value) != nil {
			continue
		}
		switch value.Type {
		case "model", "tool_call", "protocol_error", "task_failed":
			summary = append(summary, fmt.Sprintf("type=%s id=%s through=%d call_id=%s code=%s task=%s session=%s",
				value.Type, value.ID, value.Through, value.CallID, value.Code, value.TaskID, value.SessionID))
		}
	}
	return summary
}
