package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	storepostgres "morde/server/internal/store/postgres"
)

const (
	budgetCrashHelperPrefix       = "MORDE_MODEL_BUDGET_CRASH_HELPER_"
	budgetCrashHelperModeEnv      = budgetCrashHelperPrefix + "MODE"
	budgetCrashHelperDSNEnv       = budgetCrashHelperPrefix + "DSN"
	budgetCrashHelperDatabaseEnv  = budgetCrashHelperPrefix + "DATABASE"
	budgetCrashHelperTaskIDEnv    = budgetCrashHelperPrefix + "TASK_ID"
	budgetCrashHelperModelIDEnv   = budgetCrashHelperPrefix + "MODEL_ID"
	budgetCrashHelperSignalEnv    = budgetCrashHelperPrefix + "SIGNAL"
	budgetCrashHelperCallEnv      = budgetCrashHelperPrefix + "CALL"
	budgetCrashWorkerID           = "model-budget-crash-worker"
	budgetCrashSupervisorID       = "model-budget-crash-supervisor"
	budgetCrashProviderModel      = "synthetic-budget-process-crash-v1"
	budgetCrashProviderAPIKey     = "synthetic-budget-process-crash-key"
	budgetCrashSignedOutputTokens = 3072
)

type budgetCrashProcessSignal struct {
	TaskID                  string `json:"task_id"`
	ProviderModel           string `json:"provider_model"`
	MaxOutputTokens         int    `json:"max_output_tokens"`
	ProviderHTTPRequestSent bool   `json:"provider_http_request_sent"`
}

type budgetCrashCallRecord struct {
	Task                app.ModelTaskFence `json:"task"`
	ModelID             string             `json:"model_id"`
	CallID              string             `json:"call_id"`
	LedgerCallID        string             `json:"ledger_call_id"`
	BudgetPlanID        string             `json:"budget_plan_id"`
	BudgetCallID        string             `json:"budget_call_id"`
	BudgetRequestDigest string             `json:"budget_request_digest"`
	RawRequest          json.RawMessage    `json:"raw_request"`
}

type budgetCrashProviderBarrier struct {
	taskID     string
	signalPath string
}

func (client budgetCrashProviderBarrier) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	signal := budgetCrashProcessSignal{
		TaskID: client.taskID, ProviderModel: request.Endpoint.ModelID, MaxOutputTokens: request.MaxOutputTokens,
		ProviderHTTPRequestSent: false,
	}
	if err := budgetCrashWriteJSON(client.signalPath, signal); err != nil {
		return app.ModelResponse{}, fmt.Errorf("write post-dispatch provider barrier signal: %w", err)
	}
	// ModelGateway calls ModelClient only after BeginModelCallDispatch commits.
	// Holding here gives the parent a deterministic kill point before any HTTP
	// request byte can be sent by this process.
	<-ctx.Done()
	return app.ModelResponse{}, context.Cause(ctx)
}

func (budgetCrashProviderBarrier) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected capability check in process-crash fixture")
}

type budgetCrashGatewayCapture struct {
	inner interface {
		Complete(context.Context, app.ModelCall) (app.ModelResponse, error)
	}
	callPath string
}

func (models budgetCrashGatewayCapture) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	record := budgetCrashCallRecord{
		ModelID: call.ModelID, CallID: call.CallID, LedgerCallID: call.LedgerCallID,
		BudgetPlanID: call.BudgetPlanID, BudgetCallID: call.BudgetCallID,
		BudgetRequestDigest: call.BudgetRequestDigest, RawRequest: append(json.RawMessage(nil), call.RawRequest...),
	}
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("budget call omitted Task fence before Gateway")
	}
	record.Task = *call.Task
	if err := budgetCrashWriteJSON(models.callPath, record); err != nil {
		return app.ModelResponse{}, fmt.Errorf("write pre-dispatch Task model call identity: %w", err)
	}
	return models.inner.Complete(ctx, call)
}

type budgetCrashProcess struct {
	cmd        *exec.Cmd
	outputPath string
	waitDone   chan error
	waited     bool
	waitErr    error
}

// TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP
// proves the prepared budget is durably marked dispatched before entering the
// provider client and that killing the real DSH Worker process group before
// HTTP does not allow the same call identity to dispatch on replay.
func TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL model budget crash test was not run")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for model budget process-crash validation")
	}
	if !budgetCrashLocalPostgresHost(config.ConnConfig.Host) {
		t.Skip("CH_TEST_DATABASE_URL must point to local PostgreSQL; refusing a non-local host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,100000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatal(err)
	}

	var providerHTTPCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerHTTPCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/responses" ||
			request.Header.Get("Authorization") != "Bearer "+budgetCrashProviderAPIKey {
			http.Error(writer, "unexpected model budget process-crash replay", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"synthetic-budget-process-crash-v1","text":"unexpected replay","input_tokens":8,"output_tokens":6}`)
	}))
	defer provider.Close()
	providerClient := &modelBudgetPostgresHTTPClient{endpoint: provider.URL + "/responses", client: provider.Client()}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
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
		t.Fatalf("verify synthetic process-crash test session: principal=%#v err=%v", principal, err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, modelMaxOutput := 32768, 8192
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Budget process-crash synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: budgetCrashProviderModel,
		APIKey: budgetCrashProviderAPIKey, ContextWindowOverrideTokens: &contextWindow, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic process-crash route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}

	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: connection.ID, Objective: "Return one short synthetic answer so the budget crash boundary is observable.",
		Budget:         task.Budget{ActiveSeconds: 120, ToolCalls: 2, ModelTokens: 50000},
		IdempotencyKey: "budget-process-crash-submit-001",
	})
	if err != nil {
		t.Fatalf("submit synthetic process-crash Task: %v", err)
	}

	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("isolated PostgreSQL fixture did not expose its database name")
	}
	privateDirectory := t.TempDir()
	signalPath := filepath.Join(privateDirectory, "provider-client-entered.json")
	callPath := filepath.Join(privateDirectory, "captured-model-call.json")
	process := startBudgetCrashProcess(t, dsn, databaseName, submitted.Task.ID, connection.ID, signalPath, callPath)

	signal := waitBudgetCrashSignal(t, ctx, process, signalPath)
	if signal.TaskID != submitted.Task.ID || signal.ProviderModel != budgetCrashProviderModel ||
		signal.MaxOutputTokens != budgetCrashSignedOutputTokens || signal.ProviderHTTPRequestSent {
		t.Fatalf("post-dispatch barrier signal = %+v; want the expected signed cap before provider HTTP", signal)
	}
	callRecord := readBudgetCrashCall(t, callPath)
	if callRecord.Task.TaskID != submitted.Task.ID || callRecord.ModelID != connection.ID ||
		callRecord.BudgetPlanID == "" || callRecord.BudgetCallID == "" ||
		callRecord.LedgerCallID == "" || callRecord.CallID == "" || len(callRecord.RawRequest) == 0 {
		t.Fatalf("captured adaptive model call is incomplete: task=%q model=%q plan_present=%t budget_call_present=%t ledger_present=%t dsh_present=%t raw_request_present=%t",
			callRecord.Task.TaskID, callRecord.ModelID, callRecord.BudgetPlanID != "", callRecord.BudgetCallID != "",
			callRecord.LedgerCallID != "", callRecord.CallID != "", len(callRecord.RawRequest) != 0)
	}
	var ledgerState string
	var preparationConsumed bool
	if err := pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, owner.ID, callRecord.LedgerCallID).
		Scan(&ledgerState, &preparationConsumed); err != nil {
		t.Fatalf("read dispatch state at provider-client boundary: %v", err)
	}
	if ledgerState != "dispatched" || !preparationConsumed || providerHTTPCalls.Load() != 0 {
		t.Fatalf("model call at deterministic kill boundary: state=%q consumed=%t HTTP calls=%d; want dispatched, consumed and zero HTTP requests",
			ledgerState, preparationConsumed, providerHTTPCalls.Load())
	}

	if err := process.killGroup(); err != nil {
		t.Fatalf("SIGKILL the DSH Worker and Node runtime process group: %v", err)
	}
	if waitErr := process.wait(ctx); waitErr == nil {
		t.Fatal("Worker helper exited successfully; expected process-level SIGKILL")
	}
	if providerHTTPCalls.Load() != 0 {
		t.Fatalf("provider received HTTP before or during process termination: calls=%d", providerHTTPCalls.Load())
	}
	staleAge := app.TaskModelCallTimeout + 3*time.Minute
	if _, err := pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-$3::double precision*interval '1 second'
WHERE owner_id=$1 AND source='task' AND call_id=$2`, owner.ID, callRecord.LedgerCallID, staleAge.Seconds()); err != nil {
		t.Fatalf("age interrupted dispatch for the stale model-call recovery pass: %v", err)
	}
	marked, err := store.MarkStaleModelCallsUnknown(ctx)
	if err != nil || marked != 1 {
		t.Fatalf("stale DSH model-call recovery marked=%d err=%v, want exactly the killed dispatch", marked, err)
	}
	var recoveredLedgerState string
	var recoveredTaskUsage *int64
	if err := pool.QueryRow(ctx, `SELECT ledger.state,task.usage_model_tokens
FROM model_call_ledger ledger JOIN tasks task ON task.id=ledger.task_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, owner.ID, callRecord.LedgerCallID).
		Scan(&recoveredLedgerState, &recoveredTaskUsage); err != nil {
		t.Fatalf("read recovered stale model call and Task usage: %v", err)
	}
	if recoveredLedgerState != "unknown" || recoveredTaskUsage != nil || providerHTTPCalls.Load() != 0 {
		t.Fatalf("Portal stale-call recovery: ledger=%q Task usage=%v provider HTTP calls=%d; want unknown, NULL usage, and zero HTTP requests",
			recoveredLedgerState, recoveredTaskUsage, providerHTTPCalls.Load())
	}

	// Reconstruct only durable call identity and request data in this process.
	// After the stale-call recovery contract marks the dispatch unknown, a fresh
	// Gateway must refuse the call before reaching the localhost HTTP endpoint;
	// it cannot safely infer whether a provider consumed the interrupted request.
	restartedConnections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	restartedGateway, err := app.NewModelGatewayService(store, restartedConnections, providerClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedGateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &callRecord.Task, ModelID: callRecord.ModelID,
		CallID: callRecord.CallID, LedgerCallID: callRecord.LedgerCallID, BudgetPlanID: callRecord.BudgetPlanID,
		BudgetCallID: callRecord.BudgetCallID, BudgetRequestDigest: callRecord.BudgetRequestDigest,
		RawRequest: append(json.RawMessage(nil), callRecord.RawRequest...)}
	if _, err := restartedGateway.Complete(ctx, call); err == nil {
		t.Fatal("reconstructed Gateway accepted the interrupted call without a settled provider response")
	}
	var finalLedgerState string
	if err := pool.QueryRow(ctx, `SELECT state FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, owner.ID, callRecord.LedgerCallID).Scan(&finalLedgerState); err != nil {
		t.Fatal(err)
	}
	var finalTaskUsage *int64
	if err := pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, submitted.Task.ID).Scan(&finalTaskUsage); err != nil {
		t.Fatal(err)
	}
	if finalLedgerState != "unknown" || finalTaskUsage != nil || providerHTTPCalls.Load() != 0 {
		t.Fatalf("process-crash replay changed unknown accounting: ledger=%q Task usage=%v provider HTTP calls=%d; want unknown, NULL usage, and zero replay",
			finalLedgerState, finalTaskUsage, providerHTTPCalls.Load())
	}
}

// This helper runs in a separate Go Worker process. Its real CloudDSHExecutor
// invokes the injected provider barrier only after ModelGateway has committed
// the prepared -> dispatched PostgreSQL transition.
func TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTPHelper(t *testing.T) {
	if os.Getenv(budgetCrashHelperModeEnv) == "" {
		t.Skip("helper process only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := strings.TrimSpace(os.Getenv(budgetCrashHelperDSNEnv))
	databaseName := strings.TrimSpace(os.Getenv(budgetCrashHelperDatabaseEnv))
	taskID := strings.TrimSpace(os.Getenv(budgetCrashHelperTaskIDEnv))
	modelID := strings.TrimSpace(os.Getenv(budgetCrashHelperModelIDEnv))
	signalPath := strings.TrimSpace(os.Getenv(budgetCrashHelperSignalEnv))
	callPath := strings.TrimSpace(os.Getenv(budgetCrashHelperCallEnv))
	if dsn == "" || databaseName == "" || taskID == "" || modelID == "" || signalPath == "" || callPath == "" {
		t.Fatal("model budget process-crash helper configuration is incomplete")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !budgetCrashLocalPostgresHost(config.ConnConfig.Host) {
		t.Fatal("model budget process-crash helper refuses a non-local PostgreSQL host")
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("connect helper to isolated local PostgreSQL database")
	}
	defer pool.Close()
	store := storepostgres.New(pool)
	claimed, found, err := store.ClaimNextTask(ctx, budgetCrashWorkerID, 2*time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim adaptive budget process-crash Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	barrier := budgetCrashProviderBarrier{taskID: taskID, signalPath: signalPath}
	connections, err := app.NewModelConnectionService(store, sealer, barrier, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, barrier)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	if err := budgetCrashInstallStrategy(gateway); err != nil {
		t.Fatal(err)
	}
	planner, err := NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; real DSH runtime process cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimeScript); err != nil {
		t.Fatalf("locate locked morde-cloud-runtime entrypoint: %v", err)
	}
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, runtimeScript,
		budgetCrashSupervisorID, bytes.Repeat([]byte{0x42}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: budgetCrashSupervisorID, AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: budgetCrashGatewayCapture{inner: gateway,
			callPath: strings.TrimSpace(os.Getenv(budgetCrashHelperCallEnv))},
		ModelBudgetPlanner: planner, ToolCapabilities: cloudDSHTestToolCapability{allowed: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteTask(ctx, claimed); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("DSH Worker left before provider dispatch barrier: %v", err)
	}
	t.Fatal("provider barrier returned; helper should remain alive until parent SIGKILL")
}

func budgetCrashInstallStrategy(gateway *app.ModelGatewayService) error {
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "short_answer", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
	}
	return gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{
		Version: "budget-process-crash-v1",
		Models:  []app.ModelOutputBudgetModelDefinition{{BaseURL: "https://api.openai.com/v1", Model: budgetCrashProviderModel, Stages: stages}},
	})
}

func startBudgetCrashProcess(t *testing.T, dsn, databaseName, taskID, modelID, signalPath, callPath string) *budgetCrashProcess {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable,
		"-test.run=^TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTPHelper$",
		"-test.timeout=90s", "-test.v")
	command.Env = budgetCrashHelperEnvironment(map[string]string{
		budgetCrashHelperModeEnv: "worker", budgetCrashHelperDSNEnv: dsn,
		budgetCrashHelperDatabaseEnv: databaseName, budgetCrashHelperTaskIDEnv: taskID,
		budgetCrashHelperModelIDEnv: modelID, budgetCrashHelperSignalEnv: signalPath,
		budgetCrashHelperCallEnv: callPath,
	})
	outputPath := signalPath + ".log"
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &budgetCrashProcess{cmd: command, outputPath: outputPath, waitDone: make(chan error, 1)}
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatalf("start PostgreSQL-backed DSH Worker helper process: %v", err)
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

func (process *budgetCrashProcess) killGroup() error {
	if process.waited || process.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (process *budgetCrashProcess) wait(ctx context.Context) error {
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

func waitBudgetCrashSignal(t *testing.T, ctx context.Context, process *budgetCrashProcess, path string) budgetCrashProcessSignal {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var signal budgetCrashProcessSignal
			if err := json.Unmarshal(data, &signal); err != nil {
				t.Fatalf("decode model budget process-crash signal: %v", err)
			}
			return signal
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read model budget process-crash signal: %v", err)
		}
		select {
		case processErr := <-process.waitDone:
			process.waited, process.waitErr = true, processErr
			output, _ := os.ReadFile(process.outputPath)
			t.Fatalf("DSH Worker helper exited before provider signal: %v; output=%s", processErr, output)
		case <-ctx.Done():
			t.Fatalf("wait for DSH Worker provider-client entry: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func readBudgetCrashCall(t *testing.T, path string) budgetCrashCallRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured adaptive model call: %v", err)
	}
	var record budgetCrashCallRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode captured adaptive model call: %v", err)
	}
	return record
}

func budgetCrashWriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
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

func budgetCrashHelperEnvironment(values map[string]string) []string {
	filtered := make([]string, 0, len(os.Environ())+len(values))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, budgetCrashHelperPrefix) {
			continue
		}
		filtered = append(filtered, value)
	}
	for name, value := range values {
		filtered = append(filtered, name+"="+value)
	}
	return filtered
}

func budgetCrashLocalPostgresHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "/")
}
