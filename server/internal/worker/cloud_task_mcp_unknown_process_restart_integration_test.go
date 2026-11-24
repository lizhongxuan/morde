package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	m09MCPUnknownHelperModeEnv       = "MORDE_M09_MCP_UNKNOWN_HELPER_MODE"
	m09MCPUnknownHelperDSNEnv        = "MORDE_M09_MCP_UNKNOWN_HELPER_DSN"
	m09MCPUnknownHelperDatabaseEnv   = "MORDE_M09_MCP_UNKNOWN_HELPER_DATABASE"
	m09MCPUnknownHelperTaskIDEnv     = "MORDE_M09_MCP_UNKNOWN_HELPER_TASK_ID"
	m09MCPUnknownHelperOwnerIDEnv    = "MORDE_M09_MCP_UNKNOWN_HELPER_OWNER_ID"
	m09MCPUnknownHelperConnectorEnv  = "MORDE_M09_MCP_UNKNOWN_HELPER_CONNECTOR_ID"
	m09MCPUnknownHelperSignalEnv     = "MORDE_M09_MCP_UNKNOWN_HELPER_SIGNAL"
	m09MCPUnknownHelperCallsEnv      = "MORDE_M09_MCP_UNKNOWN_HELPER_MODEL_CALLS"
	m09MCPUnknownHelperEffectsEnv    = "MORDE_M09_MCP_UNKNOWN_HELPER_SIDE_EFFECTS"
	m09MCPUnknownCallID              = "m09-mcp-unknown-call-001"
	m09MCPUnknownWorkerID            = "m09-mcp-unknown-process-worker"
	m09MCPUnknownSupervisorID        = "m09-mcp-unknown-process-supervisor"
	m09MCPUnknownHelperLeaseDuration = 3 * time.Second
)

type m09MCPUnknownProcessSignal struct {
	Event                     string `json:"event"`
	TaskID                    string `json:"task_id"`
	WorkerID                  string `json:"worker_id"`
	LeaseVersion              int64  `json:"lease_version"`
	AgentGeneration           int64  `json:"agent_generation"`
	CheckpointThroughSequence int64  `json:"checkpoint_through_sequence"`
	NodePID                   int    `json:"node_pid"`
	ModelCalls                int64  `json:"model_calls"`
	ConnectorSideEffects      int64  `json:"connector_side_effects"`
	TaskStatus                string `json:"task_status"`
	WaitingReason             string `json:"waiting_reason"`
	OperationStatus           string `json:"operation_status"`
	Error                     string `json:"error,omitempty"`
}

type m09MCPUnknownProcess struct {
	cmd        *exec.Cmd
	outputPath string
	waitDone   chan error
	waited     bool
	waitErr    error
}

type m09MCPUnknownRuntimeSupervisor struct {
	*f01CapturingSupervisor
	key ed25519.PrivateKey
}

func (supervisor *m09MCPUnknownRuntimeSupervisor) RecoverAccountRuntimeStopped(ctx context.Context,
	fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	supervisor.mu.Lock()
	startedHere := supervisor.fence == fence && supervisor.nonceSHA256 == nonceSHA256
	supervisor.mu.Unlock()
	if startedHere {
		return supervisor.f01CapturingSupervisor.RecoverAccountRuntimeStopped(ctx, fence, nonceSHA256)
	}
	if err := ctx.Err(); err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	// The test parent kills the first Go Worker and its Node process group
	// before expiring the persisted Runtime lease. This deterministic local
	// Guardian can therefore issue the restart proof without the old process's
	// in-memory supervisor handle.
	nonce, err := hex.DecodeString(nonceSHA256)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	encodedFence, err := json.Marshal(fence)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	identity := sha256.Sum256(append(append(encodedFence, 0), nonce...))
	proof := app.AccountAgentRuntimeStopAttestation{
		Fence: fence, ContainerName: "morde-account-runtime-" + hex.EncodeToString(identity[:]),
		NonceSHA256: nonceSHA256, ContainerID: strings.Repeat("a", 64),
		ExpiresAt: time.Now().Add(-time.Second), DestroyedAt: time.Now().UTC(),
		ProofSHA256: strings.Repeat("e", 64), Destroyed: true,
	}
	payload, err := app.AccountAgentRuntimeStopAttestationSigningPayload(proof)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	proof.Signature = ed25519.Sign(supervisor.key, payload)
	return proof, nil
}

// TestCloudTaskMCPUnknownOperationSurvivesNodeAndTaskWorkerRestart proves that
// an MCP dispatch whose result is lost with both its DSH Node and TaskWorker
// remains unknown after a fresh process resumes the PostgreSQL-backed Task.
func TestCloudTaskMCPUnknownOperationSurvivesNodeAndTaskWorkerRestart(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL MCP process-restart integration was not run")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for local PostgreSQL validation")
	}
	if !m09MCPUnknownLocalPostgresHost(config.ConnConfig.Host) {
		t.Skip("CH_TEST_DATABASE_URL must point to local PostgreSQL; refusing a non-local host")
	}

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
		t.Fatalf("verify MCP process-restart test session: principal=%#v err=%v", principal, err)
	}
	connectorService, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	connector, err := connectorService.Create(ctx, principal, app.CreateMCPConnectorCommand{
		Name: "Local MCP process-restart test", EndpointURL: "https://connector.example.test/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	connector, err = connectorService.SetEnabled(ctx, principal, connector.ID, true,
		app.MCPConnectorRevisionCommand{ExpectedRevision: connector.Revision})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Look up the record using the selected connector.",
		Budget:          task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000},
		MCPConnectorIDs: []string{connector.ID}, IdempotencyKey: "m09-mcp-unknown-process-restart-001",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pin and approve the exact call before the worker subprocess begins. The
	// provider below is entirely local and does not contact an external service.
	claimed, found, err := store.ClaimNextTask(ctx, "m09-mcp-unknown-approval-worker", 30*time.Second)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim MCP approval task: task=%+v found=%t err=%v", claimed, found, err)
	}
	claimed, approvalAgent := startF01MCPTestAgent(t, ctx, store, claimed, "m09-unknown-approval")
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close MCP process-restart Workspace block store: %v", err)
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
	approvalProvider := &m09MCPUnknownLocalConnector{}
	approvalBroker := m09MCPUnknownNewBroker(t, store, runtimeTools, connectorService, approvalProvider)
	if _, err := approvalBroker.Definitions(ctx, claimed); err != nil {
		t.Fatalf("load selected MCP definitions: %v", err)
	}
	discoverF01MCPTool(t, ctx, approvalBroker, claimed, connector.ID)
	approvalCall := f01SelectedMCPCall(t, m09MCPUnknownCallID, connector.ID, "process restart")
	approval, err := approvalBroker.Execute(ctx, claimed, approvalCall)
	if err != nil || approval.Status != app.AgentToolNeedsUser {
		t.Fatalf("request approval for exact synthetic MCP operation: result=%+v err=%v", approval, err)
	}
	approvalAgent.stop(t, ctx)
	approvalDetails, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || approvalDetails.PendingRequest == nil {
		t.Fatalf("load exact MCP approval request: pending=%+v err=%v", approvalDetails.PendingRequest, err)
	}
	if err := tasks.RespondToPendingRequest(ctx, principal, submitted.Task.ID, approvalDetails.PendingRequest.ID,
		approvalDetails.PendingRequest.Version, task.PendingDecisionApprove, "m09-mcp-unknown-approved-call-001"); err != nil {
		t.Fatalf("approve exact MCP operation: %v", err)
	}

	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("temporary PostgreSQL fixture did not expose its isolated database name")
	}
	signalPath := filepath.Join(t.TempDir(), "worker-one.json")
	modelCallsPath := filepath.Join(t.TempDir(), "model-calls.txt")
	connectorEffectsPath := filepath.Join(t.TempDir(), "connector-side-effects.txt")
	firstProcess := startM09MCPUnknownProcess(t, dsn, databaseName, submitted.Task.ID, owner.ID, connector.ID,
		"dispatch", signalPath, modelCallsPath, connectorEffectsPath)
	firstSignal := waitM09MCPUnknownSignal(t, ctx, firstProcess, signalPath)
	if firstSignal.Event != "mcp_dispatched" || firstSignal.TaskID != submitted.Task.ID || firstSignal.WorkerID != m09MCPUnknownWorkerID ||
		firstSignal.LeaseVersion < 1 || firstSignal.AgentGeneration < 1 || firstSignal.NodePID < 1 ||
		firstSignal.ConnectorSideEffects != 1 || firstSignal.ModelCalls != 1 {
		t.Fatalf("first process dispatch signal = %+v; want one side effect and one model call inside a live DSH Node", firstSignal)
	}
	firstDetails, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || firstDetails.Task.Status != task.StatusRunning || firstDetails.Task.Lease.Owner != m09MCPUnknownWorkerID ||
		firstDetails.Task.Lease.Version != firstSignal.LeaseVersion || firstDetails.Task.AgentGeneration != firstSignal.AgentGeneration ||
		firstDetails.Task.AgentSessionID == "" || firstDetails.Task.CheckpointThroughSequence < 0 || len(firstDetails.Task.CheckpointDigest) != 32 ||
		firstDetails.Task.Usage.ToolCalls != 1 {
		t.Fatalf("Task state while first MCP call is in flight: task=%+v err=%v", firstDetails.Task, err)
	}
	operationKey := f01OperationKey(m09MCPUnknownCallID)
	operation, err := store.GetMCPConnectorOperation(ctx, owner.ID, submitted.Task.ID, connector.ID, operationKey)
	if err != nil || operation.Status != task.OperationDispatched || operation.LeaseVersion != firstSignal.LeaseVersion ||
		m09MCPUnknownReadCounter(t, connectorEffectsPath) != 1 {
		t.Fatalf("first process durable MCP dispatch = operation=%+v err=%v", operation, err)
	}
	if err := firstProcess.killGroup(); err != nil {
		t.Fatalf("terminate first Go TaskWorker and DSH Node process group: %v", err)
	}
	if err := firstProcess.wait(ctx); err == nil {
		t.Fatal("first Go TaskWorker process exited successfully after forced process-group termination")
	}

	// Model both process restarts by expiring only their PostgreSQL leases. The
	// operation remains dispatched until the fresh worker reconciles its ledger.
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE conversation_id=$1`, submitted.Conversation.ID); err != nil {
		t.Fatalf("expire account DSH Runtime lease after process restart: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatalf("expire TaskWorker lease after process restart: %v", err)
	}
	operation, err = store.GetMCPConnectorOperation(ctx, owner.ID, submitted.Task.ID, connector.ID, operationKey)
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("MCP operation changed before recovery: operation=%+v err=%v", operation, err)
	}

	secondSignalPath := filepath.Join(t.TempDir(), "worker-two.json")
	secondProcess := startM09MCPUnknownProcess(t, dsn, databaseName, submitted.Task.ID, owner.ID, connector.ID,
		"resume", secondSignalPath, modelCallsPath, connectorEffectsPath)
	secondSignal := waitM09MCPUnknownSignal(t, ctx, secondProcess, secondSignalPath)
	if secondSignal.Event != "waiting_unknown" || secondSignal.TaskID != submitted.Task.ID || secondSignal.TaskStatus != string(task.StatusWaitingUser) ||
		secondSignal.WaitingReason != "mcp_result_unknown" || secondSignal.OperationStatus != string(task.OperationUnknown) ||
		secondSignal.AgentGeneration <= firstSignal.AgentGeneration || secondSignal.ModelCalls != 1 || secondSignal.ConnectorSideEffects != 1 {
		t.Fatalf("restarted TaskWorker/DSH Node signal = %+v; want waiting unknown without another call", secondSignal)
	}
	if err := secondProcess.wait(ctx); err != nil {
		t.Fatalf("recovered Go TaskWorker process failed: %v", err)
	}

	details, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil {
		t.Fatalf("read Task after both process restarts: %v", err)
	}
	operation, err = store.GetMCPConnectorOperation(ctx, owner.ID, submitted.Task.ID, connector.ID, operationKey)
	if err != nil {
		t.Fatalf("read MCP operation after both process restarts: %v", err)
	}
	if details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "mcp_result_unknown" ||
		details.Task.Result != "" || details.Task.Lease.Owner != "" || details.Task.Usage.ToolCalls != 1 || details.Task.Budget.ToolCalls != 5 ||
		details.Task.AgentGeneration <= firstSignal.AgentGeneration || operation.Status != task.OperationUnknown ||
		strings.Contains(operation.Result, "SIDE_EFFECT_RECORD") || m09MCPUnknownReadCounter(t, modelCallsPath) != 1 ||
		m09MCPUnknownReadCounter(t, connectorEffectsPath) != 1 {
		t.Fatalf("unknown MCP recovery released budget, redispatched, called the model, or fabricated a result: task=%+v operation=%+v model_calls=%d connector_side_effects=%d",
			details.Task, operation, m09MCPUnknownReadCounter(t, modelCallsPath), m09MCPUnknownReadCounter(t, connectorEffectsPath))
	}
	unknownStep := false
	for _, step := range details.Steps {
		if step.ToolCallKey == m09MCPUnknownCallID {
			unknownStep = step.Status == task.StepUnknown
		}
	}
	if !unknownStep {
		t.Fatalf("persisted MCP Task step was not left unknown: steps=%+v", details.Steps)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 100)
	if err != nil {
		t.Fatalf("list Task events after unknown MCP recovery: %v", err)
	}
	for _, event := range events {
		if event.Type == "task.result" {
			t.Fatalf("unknown MCP operation produced a fabricated terminal Task result: event=%+v", event)
		}
	}
}

// TestCloudTaskMCPUnknownProcessRestartHelper runs the actual PostgreSQL-backed
// TaskWorker in a child Go process and starts its normal DSH Node runtime.
func TestCloudTaskMCPUnknownProcessRestartHelper(t *testing.T) {
	mode := os.Getenv(m09MCPUnknownHelperModeEnv)
	t.Logf("MCP restart helper mode=%q args=%q", mode, os.Args)
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode != "dispatch" && mode != "resume" {
		t.Fatalf("unknown MCP process-restart helper mode %q", mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	dsn := os.Getenv(m09MCPUnknownHelperDSNEnv)
	databaseName := os.Getenv(m09MCPUnknownHelperDatabaseEnv)
	taskID := os.Getenv(m09MCPUnknownHelperTaskIDEnv)
	ownerID := os.Getenv(m09MCPUnknownHelperOwnerIDEnv)
	connectorID := os.Getenv(m09MCPUnknownHelperConnectorEnv)
	signalPath := os.Getenv(m09MCPUnknownHelperSignalEnv)
	modelCallsPath := os.Getenv(m09MCPUnknownHelperCallsEnv)
	connectorEffectsPath := os.Getenv(m09MCPUnknownHelperEffectsEnv)
	if strings.TrimSpace(dsn) == "" || databaseName == "" || taskID == "" || ownerID == "" || connectorID == "" ||
		signalPath == "" || modelCallsPath == "" || connectorEffectsPath == "" {
		t.Fatal("MCP process-restart helper configuration is incomplete")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse MCP process-restart helper database configuration")
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect MCP process-restart helper to isolated local PostgreSQL: %v", err)
	}
	defer pool.Close()
	store := storepostgres.New(pool)

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x39}, 32))
	if err != nil {
		t.Fatal(err)
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
	connectorService, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close MCP process-restart helper Workspace block store: %v", err)
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
	provider := &m09MCPUnknownLocalConnector{
		sideEffectsPath: connectorEffectsPath, started: make(chan struct{}, 1), block: mode == "dispatch",
	}
	broker := m09MCPUnknownNewBroker(t, store, runtimeTools, connectorService, provider)
	model := &m09MCPUnknownRestartModel{connectorID: connectorID, callsPath: modelCallsPath}
	instances, accountRuntimes, supervisor := m09MCPUnknownConversationRuntime(t, store)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: m09MCPUnknownSupervisorID,
		AccountRuntimes: accountRuntimes, Instances: instances, Checkpoints: checkpoints, Models: model, Tools: broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: m09MCPUnknownWorkerID, LeaseDuration: m09MCPUnknownHelperLeaseDuration,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	runDone := make(chan error, 1)
	go func() { runDone <- worker.Run(workerCtx) }()

	if mode == "dispatch" {
		select {
		case <-provider.started:
		case workerErr := <-runDone:
			t.Fatalf("TaskWorker stopped before dispatching the synthetic MCP operation: %v", workerErr)
		case <-ctx.Done():
			t.Fatalf("wait for synthetic MCP dispatch: %v", ctx.Err())
		}
		details, err := store.GetTask(ctx, taskID, ownerID)
		if err != nil {
			t.Fatalf("read Task after synthetic MCP dispatch: %v", err)
		}
		supervisor.child.mu.Lock()
		command := supervisor.child.cmd
		nodePID := 0
		if command != nil && command.Process != nil {
			nodePID = command.Process.Pid
		}
		supervisor.child.mu.Unlock()
		operation, err := store.GetMCPConnectorOperation(ctx, ownerID, taskID, connectorID, f01OperationKey(m09MCPUnknownCallID))
		if err != nil {
			t.Fatalf("read MCP operation after provider side effect: %v", err)
		}
		if nodePID < 1 || operation.Status != task.OperationDispatched || model.callCount(t) != 1 ||
			m09MCPUnknownReadCounter(t, connectorEffectsPath) != 1 {
			t.Fatalf("TaskWorker dispatch evidence: node_pid=%d task=%+v operation=%+v model_calls=%d connector_side_effects=%d",
				nodePID, details.Task, operation, model.callCount(t), m09MCPUnknownReadCounter(t, connectorEffectsPath))
		}
		if err := m09MCPUnknownWriteSignal(signalPath, m09MCPUnknownProcessSignal{
			Event: "mcp_dispatched", TaskID: taskID, WorkerID: details.Task.Lease.Owner, LeaseVersion: details.Task.Lease.Version,
			AgentGeneration: details.Task.AgentGeneration, CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
			NodePID: nodePID, ModelCalls: model.callCount(t), ConnectorSideEffects: m09MCPUnknownReadCounter(t, connectorEffectsPath),
			TaskStatus: string(details.Task.Status),
		}); err != nil {
			t.Fatalf("write first process MCP dispatch signal: %v", err)
		}
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		details, err := store.GetTask(ctx, taskID, ownerID)
		if err != nil {
			t.Fatalf("read Task after process restart: %v", err)
		}
		if details.Task.Status == task.StatusWaitingUser && details.Task.WaitingReason == "mcp_result_unknown" {
			operation, err := store.GetMCPConnectorOperation(ctx, ownerID, taskID, connectorID, f01OperationKey(m09MCPUnknownCallID))
			if err != nil {
				t.Fatalf("read reconciled unknown MCP operation: %v", err)
			}
			supervisor.child.mu.Lock()
			command := supervisor.child.cmd
			nodePID := 0
			if command != nil && command.Process != nil {
				nodePID = command.Process.Pid
			}
			supervisor.child.mu.Unlock()
			if operation.Status != task.OperationUnknown || model.callCount(t) != 1 ||
				m09MCPUnknownReadCounter(t, connectorEffectsPath) != 1 || details.Task.AgentGeneration < 1 || nodePID < 1 {
				t.Fatalf("recovered TaskWorker evidence: task=%+v operation=%+v node_pid=%d model_calls=%d connector_side_effects=%d",
					details.Task, operation, nodePID, model.callCount(t), m09MCPUnknownReadCounter(t, connectorEffectsPath))
			}
			if err := m09MCPUnknownWriteSignal(signalPath, m09MCPUnknownProcessSignal{
				Event: "waiting_unknown", TaskID: taskID, WorkerID: details.Task.Lease.Owner, LeaseVersion: details.Task.Lease.Version,
				AgentGeneration: details.Task.AgentGeneration, CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
				NodePID: nodePID, ModelCalls: model.callCount(t), ConnectorSideEffects: m09MCPUnknownReadCounter(t, connectorEffectsPath),
				TaskStatus: string(details.Task.Status), WaitingReason: details.Task.WaitingReason, OperationStatus: string(operation.Status),
			}); err != nil {
				t.Fatalf("write recovered unknown MCP signal: %v", err)
			}
			stopWorker()
			if workerErr := <-runDone; !errors.Is(workerErr, context.Canceled) {
				t.Fatalf("stop recovered TaskWorker = %v, want context cancellation", workerErr)
			}
			return
		}
		if details.Task.Status == task.StatusCompleted || details.Task.Status == task.StatusFailed ||
			details.Task.WaitingReason != "" && details.Task.WaitingReason != "approval_required" {
			operation, operationErr := store.GetMCPConnectorOperation(ctx, ownerID, taskID, connectorID, f01OperationKey(m09MCPUnknownCallID))
			events, eventsErr := store.ListTaskEvents(ctx, ownerID, taskID, 0, 100)
			t.Fatalf("restarted Task reached a fabricated or unrelated state: task=%+v operation=%+v operation_error=%v events=%+v events_error=%v model_calls=%d connector_side_effects=%d dsh_frames=%s",
				details.Task, operation, operationErr, events, eventsErr, model.callCount(t),
				m09MCPUnknownReadCounter(t, connectorEffectsPath), supervisor.output())
		}
		select {
		case workerErr := <-runDone:
			t.Fatalf("restarted TaskWorker stopped before durable unknown state: %v", workerErr)
		case <-deadline.C:
			operation, operationErr := store.GetMCPConnectorOperation(ctx, ownerID, taskID, connectorID, f01OperationKey(m09MCPUnknownCallID))
			events, eventsErr := store.ListTaskEvents(ctx, ownerID, taskID, 0, 100)
			t.Fatalf("restarted TaskWorker did not persist unknown MCP state: task=%+v operation=%+v operation_error=%v events=%+v events_error=%v model_calls=%d connector_side_effects=%d dsh_frames=%s",
				details.Task, operation, operationErr, events, eventsErr, model.callCount(t), m09MCPUnknownReadCounter(t, connectorEffectsPath), supervisor.output())
		case <-ctx.Done():
			t.Fatalf("wait for recovered unknown MCP state: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

type m09MCPUnknownLocalConnector struct {
	sideEffectsPath string
	started         chan struct{}
	block           bool
}

func (*m09MCPUnknownLocalConnector) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return []app.MCPConnectorToolDefinition{{
		Name: "lookup", Description: "Look up a local synthetic record.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}}, nil
}

func (client *m09MCPUnknownLocalConnector) CallTool(ctx context.Context, _ string, _ http.Header, _ string,
	_ json.RawMessage) (app.MCPConnectorToolResult, error) {
	if client.sideEffectsPath != "" {
		if _, err := m09MCPUnknownIncrementCounter(client.sideEffectsPath); err != nil {
			return app.MCPConnectorToolResult{}, fmt.Errorf("record local MCP side effect: %w", err)
		}
	}
	if client.started != nil {
		select {
		case client.started <- struct{}{}:
		default:
		}
	}
	if client.block {
		<-ctx.Done()
		return app.MCPConnectorToolResult{}, ctx.Err()
	}
	return app.MCPConnectorToolResult{
		StructuredContent: json.RawMessage(`{"record_id":"SIDE_EFFECT_RECORD"}`),
		Text:              []string{"local synthetic side effect completed"},
	}, nil
}

func m09MCPUnknownNewBroker(t *testing.T, store *storepostgres.Store, runtimeTools *app.RuntimeToolExecutor,
	connectorService *app.MCPConnectorService, provider app.MCPConnectorToolClient) *app.CloudDSHToolBroker {
	t.Helper()
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x29}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalFiles, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatal(err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, runtimeTools, connectorService, provider,
		recordCapture, personalData, personalSync, goals, personalFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

type m09MCPUnknownRestartModel struct {
	connectorID string
	callsPath   string
}

func (model *m09MCPUnknownRestartModel) callCount(t *testing.T) int64 {
	t.Helper()
	return m09MCPUnknownReadCounter(t, model.callsPath)
}

func (model *m09MCPUnknownRestartModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	callNumber, err := m09MCPUnknownIncrementCounter(model.callsPath)
	if err != nil {
		return app.ModelResponse{}, fmt.Errorf("record synthetic local model call: %w", err)
	}
	if callNumber != 1 {
		return app.ModelResponse{}, errors.New("unexpected model call after the connector result became unknown")
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode synthetic local model tool definitions: %w", err)
	}
	for _, tool := range request.Tools {
		if tool.Name != "mcp_call" {
			continue
		}
		arguments, err := json.Marshal(map[string]any{
			"connector_id": model.connectorID, "tool_name": "lookup", "arguments": map[string]string{"query": "process restart"},
		})
		if err != nil {
			return app.ModelResponse{}, fmt.Errorf("encode synthetic MCP tool call: %w", err)
		}
		return app.ModelResponse{StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2},
			ToolCalls: []app.ModelToolCall{{CallID: m09MCPUnknownCallID, Name: tool.Name, Arguments: string(arguments)}}}, nil
	}
	return app.ModelResponse{}, errors.New("MCP tool was not offered to the synthetic model")
}

func m09MCPUnknownConversationRuntime(t *testing.T, store *storepostgres.Store) (*app.AgentInstanceService,
	*CloudDSHAccountRuntimeManager, *m09MCPUnknownRuntimeSupervisor) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real DSH subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	seed := sha256.Sum256([]byte("m09 MCP process-restart test-only Guardian key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	stopKey := bytes.Repeat([]byte{0x41}, 32)
	supervisorAuth, err := app.NewAgentStopAuthenticator(m09MCPUnknownSupervisorID, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator(m09MCPUnknownSupervisorID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	stopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &m09MCPUnknownRuntimeSupervisor{
		f01CapturingSupervisor: &f01CapturingSupervisor{
			child: &cloudDSHTestSupervisor{node: node, account: filepath.Join(filepath.Dir(script), "bin-account.mjs")}, key: privateKey,
		},
		key: privateKey,
	}
	guardian := &f01ConversationRuntimeGuardian{key: privateKey}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: stopVerifier,
		TaskStopper: instances, LeaseOwner: m09MCPUnknownSupervisorID, MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		supervisor.child.mu.Lock()
		defer supervisor.child.mu.Unlock()
		if command := supervisor.child.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	return instances, manager, supervisor
}

func startM09MCPUnknownProcess(t *testing.T, dsn, databaseName, taskID, ownerID, connectorID, mode,
	signalPath, modelCallsPath, connectorEffectsPath string) *m09MCPUnknownProcess {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCloudTaskMCPUnknownProcessRestartHelper$", "-test.timeout=80s", "-test.v")
	command.Env = m09MCPUnknownHelperEnvironment(map[string]string{
		m09MCPUnknownHelperModeEnv: mode, m09MCPUnknownHelperDSNEnv: dsn,
		m09MCPUnknownHelperDatabaseEnv: databaseName, m09MCPUnknownHelperTaskIDEnv: taskID,
		m09MCPUnknownHelperOwnerIDEnv: ownerID, m09MCPUnknownHelperConnectorEnv: connectorID,
		m09MCPUnknownHelperSignalEnv: signalPath, m09MCPUnknownHelperCallsEnv: modelCallsPath,
		m09MCPUnknownHelperEffectsEnv: connectorEffectsPath,
	})
	outputPath := signalPath + ".log"
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("create MCP helper process log: %v", err)
	}
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &m09MCPUnknownProcess{cmd: command, outputPath: outputPath, waitDone: make(chan error, 1)}
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatalf("start PostgreSQL-backed MCP TaskWorker helper process: %v", err)
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

func m09MCPUnknownHelperEnvironment(values map[string]string) []string {
	filtered := make([]string, 0, len(os.Environ())+len(values))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "MORDE_M09_MCP_UNKNOWN_HELPER_") {
			continue
		}
		filtered = append(filtered, value)
	}
	for name, value := range values {
		filtered = append(filtered, name+"="+value)
	}
	return filtered
}

func (process *m09MCPUnknownProcess) killGroup() error {
	if process.waited || process.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (process *m09MCPUnknownProcess) wait(ctx context.Context) error {
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

func waitM09MCPUnknownSignal(t *testing.T, ctx context.Context, process *m09MCPUnknownProcess, path string) m09MCPUnknownProcessSignal {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var signal m09MCPUnknownProcessSignal
			if err := json.Unmarshal(data, &signal); err != nil {
				t.Fatalf("decode MCP process-restart helper signal: %v", err)
			}
			return signal
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read MCP process-restart helper signal: %v", err)
		}
		select {
		case processErr := <-process.waitDone:
			process.waited, process.waitErr = true, processErr
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				var signal m09MCPUnknownProcessSignal
				if err := json.Unmarshal(data, &signal); err != nil {
					t.Fatalf("decode MCP process-restart helper signal after process exit: %v", err)
				}
				return signal
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("read MCP process-restart helper signal after process exit: %v", readErr)
			}
			output, _ := os.ReadFile(process.outputPath)
			t.Fatalf("MCP process-restart helper exited before signaling: %v; output=%s", processErr, output)
		case <-ctx.Done():
			t.Fatalf("wait for MCP process-restart helper signal: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func m09MCPUnknownWriteSignal(path string, signal m09MCPUnknownProcessSignal) error {
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

func m09MCPUnknownIncrementCounter(path string) (int64, error) {
	current := int64(0)
	data, err := os.ReadFile(path)
	if err == nil {
		current, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("decode local counter: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	current++
	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, []byte(strconv.FormatInt(current, 10)), 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return 0, err
	}
	return current, nil
}

func m09MCPUnknownReadCounter(t *testing.T, path string) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read local process-restart counter: %v", err)
	}
	count, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("decode local process-restart counter: %v", err)
	}
	return count
}

func m09MCPUnknownLocalPostgresHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "/")
}
