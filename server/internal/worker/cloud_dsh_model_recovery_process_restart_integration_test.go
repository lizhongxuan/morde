package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

const (
	t07ProcessRecoveryHelperPrefix    = "MORDE_T07_PROCESS_RECOVERY_HELPER_"
	t07ProcessRecoveryHelperModeEnv   = t07ProcessRecoveryHelperPrefix + "MODE"
	t07ProcessRecoveryHelperDSNEnv    = t07ProcessRecoveryHelperPrefix + "DSN"
	t07ProcessRecoveryHelperDatabase  = t07ProcessRecoveryHelperPrefix + "DATABASE"
	t07ProcessRecoveryHelperTaskEnv   = t07ProcessRecoveryHelperPrefix + "TASK_ID"
	t07ProcessRecoveryHelperOwnerEnv  = t07ProcessRecoveryHelperPrefix + "OWNER_ID"
	t07ProcessRecoveryHelperWorkerEnv = t07ProcessRecoveryHelperPrefix + "WORKER_ID"
	t07ProcessRecoveryHelperSignalEnv = t07ProcessRecoveryHelperPrefix + "SIGNAL"
	t07ProcessRecoveryHelperCallsEnv  = t07ProcessRecoveryHelperPrefix + "CALLS"
	t07ProcessRecoveryWorkerOne       = "t07-recovery-process-worker-one"
	t07ProcessRecoveryWorkerTwo       = "t07-recovery-process-worker-two"
)

type t07ProcessRecoverySignal struct {
	Event           string `json:"event"`
	TaskID          string `json:"task_id"`
	OwnerID         string `json:"owner_id"`
	WorkerID        string `json:"worker_id"`
	LeaseVersion    int64  `json:"lease_version"`
	AgentGeneration int64  `json:"agent_generation"`
	ThroughSequence int64  `json:"through_sequence"`
	RecoveryState   string `json:"recovery_state,omitempty"`
	ModelCalls      int    `json:"model_calls"`
	SearchCalls     int    `json:"search_calls"`
	Error           string `json:"error,omitempty"`
}

type t07ProcessRecoveryCall struct {
	Mode              string `json:"mode"`
	CallID            string `json:"call_id"`
	TaskID            string `json:"task_id"`
	RecoveryCallID    string `json:"recovery_call_id,omitempty"`
	RecoveryDSHCallID string `json:"recovery_dsh_call_id,omitempty"`
	LeaseVersion      int64  `json:"lease_version"`
	AgentGeneration   int64  `json:"agent_generation"`
	ThroughSequence   int64  `json:"through_sequence"`
	SawSearchResult   bool   `json:"saw_search_result"`
	ErrorType         string `json:"error_type,omitempty"`
}

type t07ProcessRecoveryProvider struct {
	mode                string
	mu                  sync.Mutex
	calls               int
	lastSawSearchResult bool
}

func (p *t07ProcessRecoveryProvider) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	p.mu.Lock()
	p.calls++
	callNumber := p.calls
	p.mu.Unlock()

	sawSearchResult := false
	for _, input := range request.Input {
		if input.Type == "function_call_output" && input.CallID == t07RecoverySearchCallID &&
			strings.Contains(input.Output, "recovery.example.test/source") {
			sawSearchResult = true
		}
	}
	p.mu.Lock()
	p.lastSawSearchResult = sawSearchResult
	p.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 4}
	switch p.mode {
	case "schedule":
		if callNumber == 1 && !sawSearchResult && toolOffered(request.Tools, "morde_web_search") {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: t07RecoverySearchCallID,
				Name: "morde_web_search", Arguments: `{"query":"synthetic public recovery evidence"}`}},
				StopReason: "tool_calls", Usage: usage, ProviderReportedModel: "t07-process-recovery-model"}, nil
		}
		if callNumber == 2 && sawSearchResult {
			return app.ModelResponse{}, &app.ModelClientError{Kind: app.ModelErrorRateLimited,
				Stage: "provider_status", ConfirmedNoUsage: true}
		}
	case "resume":
		if callNumber == 1 && sawSearchResult {
			return app.ModelResponse{Text: t07RecoveryFinalResult, StopReason: "stop", Usage: usage,
				ProviderReportedModel: "t07-process-recovery-model"}, nil
		}
	}
	return app.ModelResponse{}, fmt.Errorf("unexpected synthetic %s provider call %d (search result=%t)",
		p.mode, callNumber, sawSearchResult)
}

func (p *t07ProcessRecoveryProvider) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true, Tools: true, ModelID: "t07-process-recovery-model"}, nil
}

type t07ProcessRecoveryRecordingGateway struct {
	inner    *app.ModelGatewayService
	provider *t07ProcessRecoveryProvider
	mode     string
	calls    string
	mu       sync.Mutex
	count    int
}

func (g *t07ProcessRecoveryRecordingGateway) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	record := t07ProcessRecoveryCall{Mode: g.mode, CallID: call.CallID}
	if call.Task != nil {
		record.TaskID = call.Task.TaskID
		record.RecoveryCallID = call.Task.RecoveryCallID
		record.RecoveryDSHCallID = call.Task.RecoveryDSHCallID
		record.LeaseVersion = call.Task.LeaseVersion
		record.AgentGeneration = call.Task.AgentGeneration
		record.ThroughSequence = call.Task.ThroughSequence
	}
	response, err := g.inner.Complete(ctx, call)
	g.provider.mu.Lock()
	record.SawSearchResult = g.provider.lastSawSearchResult
	g.provider.mu.Unlock()
	if err != nil {
		record.ErrorType = fmt.Sprintf("%T", err)
	}
	if writeErr := g.appendCall(record); writeErr != nil {
		if err != nil {
			return app.ModelResponse{}, fmt.Errorf("record model call: %v; original provider error type %T", writeErr, err)
		}
		return app.ModelResponse{}, fmt.Errorf("record model call: %w", writeErr)
	}
	return response, err
}

func (g *t07ProcessRecoveryRecordingGateway) appendCall(record t07ProcessRecoveryCall) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := os.OpenFile(g.calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	g.count++
	return nil
}

func (g *t07ProcessRecoveryRecordingGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.count
}

type t07ProcessRecoveryExecutor struct {
	inner      *CloudDSHExecutor
	store      *storepostgres.Store
	mode       string
	taskID     string
	ownerID    string
	workerID   string
	signalPath string
	calls      *t07ProcessRecoveryRecordingGateway
	search     *t07RecoverySearchAdapter
	stopWorker context.CancelFunc
}

func (e *t07ProcessRecoveryExecutor) ExecuteTask(ctx context.Context, claimed task.Task) error {
	err := e.inner.ExecuteTask(ctx, claimed)
	details, readErr := e.store.GetTask(context.Background(), e.taskID, e.ownerID)
	signal := t07ProcessRecoverySignal{TaskID: e.taskID, OwnerID: e.ownerID, WorkerID: e.workerID,
		LeaseVersion: claimed.Lease.Version, AgentGeneration: claimed.AgentGeneration,
		ThroughSequence: claimed.CheckpointThroughSequence, ModelCalls: e.calls.callCount(), SearchCalls: e.search.callCount()}
	if readErr != nil {
		signal.Event = "failed"
		signal.Error = "could not read durable Task state after execution"
	} else if details.Task.ModelRecovery != nil {
		signal.RecoveryState = string(details.Task.ModelRecovery.State)
		signal.AgentGeneration = details.Task.AgentGeneration
		signal.ThroughSequence = details.Task.ModelRecovery.CheckpointThroughSequence
	}

	if e.mode == "schedule" {
		if readErr == nil && err != nil && details.Task.Status == task.StatusQueued &&
			details.Task.ModelRecovery != nil && details.Task.ModelRecovery.State == task.ModelRecoveryScheduled &&
			details.Task.Lease.Owner == "" {
			signal.Event = "recovery_scheduled"
		} else {
			signal.Event = "failed"
			signal.Error = "first Worker did not persist a safe queued recovery"
			if err != nil {
				signal.Error += fmt.Sprintf(" (%T)", err)
			}
		}
		if writeErr := t07WriteProcessRecoverySignal(e.signalPath, signal); writeErr != nil {
			return fmt.Errorf("write recovery schedule signal: %w", writeErr)
		}
		// The parent kills this actual Worker process after checking the durable
		// schedule, so the next Worker must reconstruct all recovery state from
		// PostgreSQL rather than this process's memory.
		select {}
	}

	if e.mode == "resume" {
		if readErr == nil && err == nil && details.Task.Status == task.StatusCompleted &&
			details.Task.Result == t07RecoveryFinalResult && details.Task.ConversationID != "" {
			signal.Event = "completed"
		} else {
			signal.Event = "failed"
			signal.Error = "restarted Worker did not complete the recovered Task"
			if err != nil {
				signal.Error += fmt.Sprintf(" (%T)", err)
			}
		}
		if writeErr := t07WriteProcessRecoverySignal(e.signalPath, signal); writeErr != nil {
			return fmt.Errorf("write recovery completion signal: %w", writeErr)
		}
		e.stopWorker()
	}
	return err
}

// TestCloudDSHModelRecoverySurvivesWorkerProcessRestart proves that a real
// Worker process can die after a read-only search result and a confirmed
// no-usage model failure have been durably scheduled, and a fresh process can
// resume from PostgreSQL without re-dispatching the search or duplicating the
// final delivery. All model and search responses are local synthetic fixtures.
func TestCloudDSHModelRecoverySurvivesWorkerProcessRestart(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable isolated Task execution: %v", err)
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
		t.Fatalf("verify isolated session: principal=%+v err=%v", principal, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x6a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	modelKey, err := sealer.Seal([]byte("t07-process-recovery-test-only-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	window := 32_768
	connection, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: "p7654321", OwnerID: owner.ID, Name: "T07 process-recovery synthetic model",
		BaseURL: "https://models.example.invalid/v1", ModelName: "t07-process-recovery-model",
		ContextWindowTokens: &window, ContextWindowOverrideTokens: &window,
		ContextWindowSource: app.ModelContextWindowLegacyManual, APIKeyCipher: modelKey,
	})
	if err != nil {
		t.Fatalf("create isolated synthetic model connection: %v", err)
	}
	checkedAt := time.Now().UTC()
	connection.Capabilities = app.ModelCapabilities{Text: true, Tools: true, ModelID: "t07-process-recovery-model"}
	connection.CheckedAt = &checkedAt
	if _, err := store.UpdateModelConnection(ctx, connection); err != nil {
		t.Fatalf("enable checked synthetic model capabilities: %v", err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p7654321", Objective: "Search the public web for the synthetic recovery source and report its finding.",
		Budget:         task.Budget{ActiveSeconds: 90, ToolCalls: 6, ModelTokens: 50_000},
		IdempotencyKey: "t07-process-recovery-e2e-001",
	})
	if err != nil {
		t.Fatalf("submit isolated recovery Task: %v", err)
	}

	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("isolated PostgreSQL database name is empty")
	}
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	callsPath := filepath.Join(t.TempDir(), "model-calls.jsonl")
	firstSignalPath := filepath.Join(t.TempDir(), "worker-one.json")
	first := startT07ProcessRecoveryWorker(t, dsn, databaseName, submitted.Task.ID, owner.ID,
		t07ProcessRecoveryWorkerOne, "schedule", firstSignalPath, callsPath)
	firstSignal := waitT07ProcessRecoverySignal(t, ctx, first, firstSignalPath)
	if firstSignal.Event != "recovery_scheduled" || firstSignal.TaskID != submitted.Task.ID ||
		firstSignal.OwnerID != owner.ID || firstSignal.WorkerID != t07ProcessRecoveryWorkerOne ||
		firstSignal.LeaseVersion < 1 || firstSignal.AgentGeneration < 1 || firstSignal.RecoveryState != string(task.ModelRecoveryScheduled) ||
		firstSignal.ModelCalls != 2 || firstSignal.SearchCalls != 1 {
		t.Fatalf("first Worker process did not durably schedule the expected read-only recovery: %+v", firstSignal)
	}
	firstDetails, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || firstDetails.Task.Status != task.StatusQueued || firstDetails.Task.ModelRecovery == nil ||
		firstDetails.Task.ModelRecovery.State != task.ModelRecoveryScheduled || firstDetails.Task.Lease.Owner != "" ||
		firstDetails.Task.CheckpointThroughSequence != firstDetails.Task.ModelRecovery.CheckpointThroughSequence ||
		len(firstDetails.Task.ModelRecovery.CheckpointDigest) != 32 || len(firstDetails.Task.ModelRecovery.ToolsetDigest) != 32 {
		t.Fatalf("durable recovery schedule before process termination: task=%+v err=%v", firstDetails.Task, err)
	}
	if firstSignal.ThroughSequence != firstDetails.Task.ModelRecovery.CheckpointThroughSequence {
		t.Fatalf("first Worker signal checkpoint=%d differs from durable recovery checkpoint=%d",
			firstSignal.ThroughSequence, firstDetails.Task.ModelRecovery.CheckpointThroughSequence)
	}

	if err := first.killGroup(); err != nil {
		t.Fatalf("kill first Go Worker and DSH runtime process group: %v", err)
	}
	if err := first.wait(ctx); err == nil {
		t.Fatal("first Worker process exited successfully after forced termination")
	}
	// Runtime recovery is fenced by the deterministic local Guardian used by
	// this test. Expiring the lease after killing the old process models the
	// restart boundary without bypassing task/model/tool authorization checks.
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE conversation_id=$1`, submitted.Conversation.ID); err != nil {
		t.Fatalf("expire only the dead DSH Runtime lease: %v", err)
	}
	stillScheduled, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || stillScheduled.Task.Status != task.StatusQueued || stillScheduled.Task.ModelRecovery == nil ||
		stillScheduled.Task.ModelRecovery.State != task.ModelRecoveryScheduled ||
		stillScheduled.Task.ModelRecovery.CheckpointThroughSequence != firstDetails.Task.ModelRecovery.CheckpointThroughSequence {
		t.Fatalf("process termination changed the durable recovery fence: task=%+v err=%v", stillScheduled.Task, err)
	}

	secondSignalPath := filepath.Join(t.TempDir(), "worker-two.json")
	second := startT07ProcessRecoveryWorker(t, dsn, databaseName, submitted.Task.ID, owner.ID,
		t07ProcessRecoveryWorkerTwo, "resume", secondSignalPath, callsPath)
	secondSignal := waitT07ProcessRecoverySignal(t, ctx, second, secondSignalPath)
	if secondSignal.Event != "completed" || secondSignal.TaskID != submitted.Task.ID || secondSignal.OwnerID != owner.ID ||
		secondSignal.WorkerID != t07ProcessRecoveryWorkerTwo || secondSignal.LeaseVersion <= firstSignal.LeaseVersion ||
		secondSignal.AgentGeneration <= firstSignal.AgentGeneration || secondSignal.ModelCalls != 1 || secondSignal.SearchCalls != 0 {
		t.Fatalf("fresh Worker process did not safely resume and complete the same Task: %+v", secondSignal)
	}
	if err := second.wait(ctx); err != nil {
		t.Fatalf("recovered Worker process exited with an error: %v", err)
	}

	completed, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != t07RecoveryFinalResult ||
		completed.Task.ConversationID != submitted.Conversation.ID {
		t.Fatalf("recovered Task terminal state=%+v err=%v", completed.Task, err)
	}
	var searchOperations int
	var searchStatus, searchResult string
	if err := pool.QueryRow(ctx, `SELECT count(*),max(status),max(result) FROM task_mcp_connector_operations
		WHERE task_id=$1 AND owner_id=$2 AND tool_name='morde_web_search'`, submitted.Task.ID, owner.ID).
		Scan(&searchOperations, &searchStatus, &searchResult); err != nil || searchOperations != 1 ||
		searchStatus != string(task.OperationSucceeded) || !strings.Contains(searchResult, "recovery.example.test/source") {
		t.Fatalf("durable public search operation count=%d status=%q result=%q err=%v",
			searchOperations, searchStatus, searchResult, err)
	}
	callRecords := readT07ProcessRecoveryCalls(t, callsPath)
	if len(callRecords) != 3 || callRecords[0].Mode != "schedule" || callRecords[1].Mode != "schedule" ||
		callRecords[2].Mode != "resume" || callRecords[0].SawSearchResult || !callRecords[1].SawSearchResult ||
		!callRecords[2].SawSearchResult || callRecords[0].ErrorType != "" || callRecords[1].ErrorType == "" ||
		callRecords[2].ErrorType != "" || callRecords[2].TaskID != submitted.Task.ID ||
		callRecords[2].RecoveryCallID == "" || callRecords[2].RecoveryDSHCallID == "" ||
		callRecords[2].RecoveryCallID == callRecords[1].CallID || callRecords[2].CallID != callRecords[2].RecoveryDSHCallID ||
		callRecords[2].LeaseVersion != secondSignal.LeaseVersion || callRecords[2].AgentGeneration <= callRecords[1].AgentGeneration {
		t.Fatalf("synthetic model call history did not prove one fenced recovery using the persisted search result: %+v", callRecords)
	}

	messages, err := store.ListMessages(ctx, owner.ID, submitted.Conversation.ID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatalf("list original conversation after recovery: %v", err)
	}
	assistantResults := 0
	for _, message := range messages.Items {
		if message.Role != "assistant" || message.TaskID != submitted.Task.ID {
			continue
		}
		assistantResults++
		if message.ConversationID != submitted.Conversation.ID || message.Content != t07RecoveryFinalResult {
			t.Fatalf("recovered assistant result was delivered to an unrelated conversation or Task: %+v", message)
		}
	}
	if assistantResults != 1 {
		t.Fatalf("original conversation has %d assistant results for recovered Task, want exactly one", assistantResults)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 100)
	if err != nil {
		t.Fatalf("list recovered Task events: %v", err)
	}
	resultEvents := 0
	for _, event := range events {
		if event.Type == "task.result" {
			resultEvents++
		}
	}
	if resultEvents != 1 {
		t.Fatalf("recovered Task has %d terminal result events, want exactly one", resultEvents)
	}
}

// TestCloudDSHModelRecoveryProcessRestartHelper is a real TaskWorker helper
// process. In schedule mode it stays alive after persisting the recovery plan
// so the parent can SIGKILL the Go Worker together with its local DSH runtime.
func TestCloudDSHModelRecoveryProcessRestartHelper(t *testing.T) {
	mode := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperModeEnv))
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode != "schedule" && mode != "resume" {
		t.Fatalf("unknown T07 process-recovery helper mode %q", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	defer cancel()
	dsn := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperDSNEnv))
	databaseName := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperDatabase))
	taskID := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperTaskEnv))
	ownerID := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperOwnerEnv))
	workerID := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperWorkerEnv))
	signalPath := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperSignalEnv))
	callsPath := strings.TrimSpace(os.Getenv(t07ProcessRecoveryHelperCallsEnv))
	if dsn == "" || databaseName == "" || taskID == "" || ownerID == "" || workerID == "" || signalPath == "" || callsPath == "" {
		t.Fatal("T07 process-recovery helper configuration is incomplete")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !m09MCPUnknownLocalPostgresHost(config.ConnConfig.Host) {
		t.Fatal("T07 process-recovery helper refuses a non-local PostgreSQL host")
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect Worker helper to isolated local PostgreSQL: %v", err)
	}
	defer pool.Close()
	store := storepostgres.New(pool)
	if _, err := store.GetTask(ctx, taskID, ownerID); err != nil {
		t.Fatalf("load isolated recovery Task for Worker helper: %v", err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x6a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	provider := &t07ProcessRecoveryProvider{mode: mode}
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
	models := &t07ProcessRecoveryRecordingGateway{inner: gateway, provider: provider, mode: mode, calls: callsPath}

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
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close isolated workspace block store: %v", err)
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
	instances, accountRuntimes, _ := m09MCPUnknownConversationRuntime(t, store)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: m09MCPUnknownSupervisorID, AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: models, ToolCapabilities: gateway, Tools: tools, Completion: broker,
	})
	if err != nil {
		t.Fatal(err)
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	wrappedExecutor := &t07ProcessRecoveryExecutor{inner: executor, store: store, mode: mode,
		taskID: taskID, ownerID: ownerID, workerID: workerID, signalPath: signalPath,
		calls: models, search: search, stopWorker: stopWorker}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: wrappedExecutor, WorkerID: workerID, LeaseDuration: 30 * time.Second,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create PostgreSQL-backed T07 Worker: %v", err)
	}
	if err := worker.Run(workerCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("T07 process-recovery Worker exited with %v, want controlled cancellation", err)
	}
}

type t07ProcessRecoveryWorker struct {
	cmd      *exec.Cmd
	output   string
	waitDone chan error
	waited   bool
	waitErr  error
}

func startT07ProcessRecoveryWorker(t *testing.T, dsn, databaseName, taskID, ownerID, workerID, mode,
	signalPath, callsPath string) *t07ProcessRecoveryWorker {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCloudDSHModelRecoveryProcessRestartHelper$", "-test.timeout=140s", "-test.v")
	env := make([]string, 0, len(os.Environ())+8)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, t07ProcessRecoveryHelperPrefix) {
			continue
		}
		env = append(env, value)
	}
	for name, value := range map[string]string{
		t07ProcessRecoveryHelperModeEnv: mode, t07ProcessRecoveryHelperDSNEnv: dsn,
		t07ProcessRecoveryHelperDatabase: databaseName, t07ProcessRecoveryHelperTaskEnv: taskID,
		t07ProcessRecoveryHelperOwnerEnv: ownerID, t07ProcessRecoveryHelperWorkerEnv: workerID,
		t07ProcessRecoveryHelperSignalEnv: signalPath, t07ProcessRecoveryHelperCallsEnv: callsPath,
	} {
		env = append(env, name+"="+value)
	}
	command.Env = env
	outputPath := signalPath + ".log"
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("create Worker helper output log: %v", err)
	}
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &t07ProcessRecoveryWorker{cmd: command, output: outputPath, waitDone: make(chan error, 1)}
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatalf("start PostgreSQL-backed DSH Worker helper: %v", err)
	}
	go func() {
		waitErr := command.Wait()
		_ = output.Close()
		process.waitDone <- waitErr
	}()
	t.Cleanup(func() {
		if !process.waited {
			_ = process.killGroup()
			_ = process.wait(context.Background())
		}
	})
	return process
}

func waitT07ProcessRecoverySignal(t *testing.T, ctx context.Context, process *t07ProcessRecoveryWorker,
	signalPath string) t07ProcessRecoverySignal {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(signalPath)
		if err == nil {
			var signal t07ProcessRecoverySignal
			if err := json.Unmarshal(data, &signal); err != nil {
				t.Fatalf("decode Worker process-recovery signal: %v", err)
			}
			return signal
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read Worker process-recovery signal: %v", err)
		}
		select {
		case processErr := <-process.waitDone:
			process.waited, process.waitErr = true, processErr
			// The child can atomically publish its signal and exit between the
			// initial file check and select. Recheck after observing process exit
			// before treating it as an early failure.
			if data, readErr := os.ReadFile(signalPath); readErr == nil {
				var signal t07ProcessRecoverySignal
				if decodeErr := json.Unmarshal(data, &signal); decodeErr != nil {
					t.Fatalf("decode final Worker process-recovery signal: %v", decodeErr)
				}
				return signal
			}
			log, _ := os.ReadFile(process.output)
			t.Fatalf("Worker helper exited before its state signal: %v; output=%s", processErr, log)
		case <-ctx.Done():
			log, _ := os.ReadFile(process.output)
			t.Fatalf("wait for Worker process-recovery signal: %v; output=%s", ctx.Err(), log)
		case <-ticker.C:
		}
	}
}

func (process *t07ProcessRecoveryWorker) killGroup() error {
	if process.waited || process.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (process *t07ProcessRecoveryWorker) wait(ctx context.Context) error {
	if process.waited {
		return process.waitErr
	}
	select {
	case err := <-process.waitDone:
		process.waited, process.waitErr = true, err
		return err
	case <-ctx.Done():
		_ = process.killGroup()
		err := <-process.waitDone
		process.waited, process.waitErr = true, err
		return ctx.Err()
	}
}

func t07WriteProcessRecoverySignal(path string, signal t07ProcessRecoverySignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}

func readT07ProcessRecoveryCalls(t *testing.T, path string) []t07ProcessRecoveryCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synthetic model-call history: %v", err)
	}
	var calls []t07ProcessRecoveryCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call t07ProcessRecoveryCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("decode synthetic model-call history: %v", err)
		}
		calls = append(calls, call)
	}
	return calls
}
