package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	budgetCrashPortalBinaryEnv    = "MORDE_MODEL_BUDGET_CRASH_PORTAL_BINARY"
	budgetCrashWorkerID           = "model-budget-crash-worker"
	budgetCrashSupervisorID       = "model-budget-crash-supervisor"
	budgetCrashProviderModel      = "synthetic-budget-process-crash-v1"
	budgetCrashProviderAPIKey     = "synthetic-budget-process-crash-key"
	budgetCrashSignedOutputTokens = 3072
)

// TestPortalProcessCrashMarksDispatchedModelCallUnknownAndDoesNotReplay boots
// the real Portal binary against an isolated PostgreSQL database. Its local
// model proxy accepts CONNECT but withholds the tunnel response, giving the
// test a deterministic point after the ledger dispatch commit and before any
// provider HTTP request. After SIGKILL and Portal restart, the real reconciler
// marks the stale dispatch unknown and an API replay with the same key must not
// contact the proxy or provider again.
func TestPortalProcessCrashMarksDispatchedModelCallUnknownAndDoesNotReplay(t *testing.T) {
	if runtime.GOOS != "linux" && strings.TrimSpace(os.Getenv(budgetCrashPortalBinaryEnv)) == "" {
		t.Skip("the real Portal startup checks Linux /proc mount metadata; run this integration test on Linux")
	}
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local Portal process-crash test was not run")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for Portal process-crash validation")
	}
	if !budgetCrashLocalPostgresHost(config.ConnConfig.Host) {
		t.Skip("CH_TEST_DATABASE_URL must point to local PostgreSQL; refusing a non-local host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	if err := app.EnsureInitialAdmin(ctx, store); err != nil {
		t.Fatalf("seed the required isolated Portal administrator: %v", err)
	}
	owner := createF01User(t, ctx, store)
	portalDSN := budgetCrashDSNForDatabase(t, dsn, pool.Config().ConnConfig.Database)

	proxyEntered := make(chan struct{}, 1)
	var proxyConnects atomic.Int32
	var providerHTTPRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect && request.Host == "api.openai.com:443" {
			proxyConnects.Add(1)
			select {
			case proxyEntered <- struct{}{}:
			default:
			}
			// The model client waits for a successful CONNECT before sending TLS
			// and provider HTTP. Holding this handler lets the parent kill Portal
			// without putting a request on a provider connection.
			<-request.Context().Done()
			return
		}
		providerHTTPRequests.Add(1)
		http.Error(writer, "unexpected non-CONNECT provider request", http.StatusBadRequest)
	}))
	t.Cleanup(proxy.Close)
	openSandbox := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"items":[],"pagination":{"hasNextPage":false}}`)
	}))
	t.Cleanup(openSandbox.Close)

	dataKey := bytes.Repeat([]byte{0x31}, 32)
	sealer, err := app.NewSealer(dataKey)
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
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic Portal process-crash session: principal=%#v err=%v", principal, err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, budgetCrashProviderBarrier{}, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, modelMaxOutput := 32768, 8192
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Portal process-crash synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: budgetCrashProviderModel,
		APIKey: budgetCrashProviderAPIKey, ContextWindowOverrideTokens: &contextWindow, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic Portal process-crash route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	apiAddress := budgetCrashFreeLoopbackAddress(t)
	configPath := writeBudgetCrashPortalConfig(t, tempDir, portalDSN, proxy.URL, openSandbox.URL, apiAddress,
		budgetCrashFreeLoopbackAddress(t), budgetCrashFreeLoopbackAddress(t))
	portalBinary := buildBudgetCrashPortalBinary(t, ctx, tempDir)
	portalURL := "http://" + apiAddress
	apiHost := "portal.test" + strings.TrimPrefix(apiAddress, "127.0.0.1")
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}

	firstPortal := startBudgetCrashPortalProcess(t, portalBinary, configPath, filepath.Join(tempDir, "portal-first.log"))
	waitBudgetCrashPortalHealth(t, ctx, httpClient, portalURL, firstPortal)
	callID := "portal-process-crash-call-001"
	firstResponse := make(chan struct {
		status int
		err    error
	}, 1)
	go func() {
		status, _, requestErr := budgetCrashPortalModelRequest(httpClient, portalURL, apiHost, sessionCredential,
			connection.ID, callID)
		firstResponse <- struct {
			status int
			err    error
		}{status: status, err: requestErr}
	}()
	select {
	case <-proxyEntered:
	case <-ctx.Done():
		t.Fatalf("wait for Portal model client to enter local CONNECT barrier: %v", ctx.Err())
	}

	var dispatchState string
	if err := pool.QueryRow(ctx, `SELECT state FROM model_call_ledger
WHERE owner_id=$1 AND source='client' AND call_id=$2`, owner.ID, callID).Scan(&dispatchState); err != nil {
		t.Fatalf("read model ledger at Portal CONNECT barrier: %v", err)
	}
	if dispatchState != "dispatched" || proxyConnects.Load() != 1 || providerHTTPRequests.Load() != 0 {
		t.Fatalf("Portal call at CONNECT barrier: ledger=%q proxy CONNECTs=%d provider HTTP=%d; want dispatched, one CONNECT, and no provider HTTP",
			dispatchState, proxyConnects.Load(), providerHTTPRequests.Load())
	}
	if err := firstPortal.killGroup(); err != nil {
		t.Fatalf("SIGKILL the Portal process: %v", err)
	}
	if waitErr := firstPortal.wait(ctx); waitErr == nil {
		t.Fatal("Portal exited successfully; expected process-level SIGKILL")
	}
	select {
	case result := <-firstResponse:
		if result.err == nil {
			t.Fatalf("killed Portal model API request unexpectedly returned status %d", result.status)
		}
	case <-ctx.Done():
		t.Fatalf("wait for interrupted Portal API request to close: %v", ctx.Err())
	}

	// Stale only this synthetic client dispatch. This exercises the same
	// production recovery threshold without waiting two minutes in the test.
	if _, err := pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source='client' AND call_id=$2`, owner.ID, callID); err != nil {
		t.Fatalf("age the interrupted Portal dispatch for reconciliation: %v", err)
	}
	secondPortal := startBudgetCrashPortalProcess(t, portalBinary, configPath, filepath.Join(tempDir, "portal-second.log"))
	waitBudgetCrashPortalHealth(t, ctx, httpClient, portalURL, secondPortal)
	waitBudgetCrashLedgerState(t, ctx, pool, owner.ID, callID, "unknown")

	status, _, err := budgetCrashPortalModelRequest(httpClient, portalURL, apiHost, sessionCredential,
		connection.ID, callID)
	if err != nil {
		t.Fatalf("replay model API request after Portal recovery: %v", err)
	}
	if status < http.StatusBadRequest || proxyConnects.Load() != 1 || providerHTTPRequests.Load() != 0 {
		t.Fatalf("Portal replay after unknown recovery: status=%d proxy CONNECTs=%d provider HTTP=%d; want rejected replay and no second provider dispatch",
			status, proxyConnects.Load(), providerHTTPRequests.Load())
	}
	var finalState string
	if err := pool.QueryRow(ctx, `SELECT state FROM model_call_ledger
WHERE owner_id=$1 AND source='client' AND call_id=$2`, owner.ID, callID).Scan(&finalState); err != nil {
		t.Fatal(err)
	}
	if finalState != "unknown" {
		t.Fatalf("Portal replay changed recovered ledger state to %q; want unknown", finalState)
	}
}

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
		{Stage: "root_text_only", GoalTokens: budgetCrashSignedOutputTokens, MinimumTokens: 64},
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

func buildBudgetCrashPortalBinary(t *testing.T, ctx context.Context, directory string) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv(budgetCrashPortalBinaryEnv)); configured != "" {
		info, err := os.Stat(configured)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("configured Portal binary is not a regular file: %v", err)
		}
		return configured
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	serverRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	binaryPath := filepath.Join(directory, "portal-budget-crash-test")
	command := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./cmd/portal")
	command.Dir = serverRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build real Portal binary for process-crash integration: %v; output=%s", err, output)
	}
	return binaryPath
}

func startBudgetCrashPortalProcess(t *testing.T, binaryPath, configPath, outputPath string) *budgetCrashProcess {
	t.Helper()
	command := exec.Command(binaryPath, "serve", "--config", configPath)
	command.Env = budgetCrashHelperEnvironment(nil)
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &budgetCrashProcess{cmd: command, outputPath: outputPath, waitDone: make(chan error, 1)}
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatalf("start real Portal process: %v", err)
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

func waitBudgetCrashPortalHealth(t *testing.T, ctx context.Context, client *http.Client, baseURL string, process *budgetCrashProcess) {
	t.Helper()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case processErr := <-process.waitDone:
			process.waited, process.waitErr = true, processErr
			output, _ := os.ReadFile(process.outputPath)
			t.Fatalf("Portal exited before /healthz was ready: %v; output=%s", processErr, output)
		case <-deadline.C:
			_ = process.killGroup()
			processErr := process.wait(context.Background())
			output, _ := os.ReadFile(process.outputPath)
			t.Fatalf("Portal /healthz did not become ready: process=%v; output=%s", processErr, output)
		case <-ctx.Done():
			t.Fatalf("wait for Portal /healthz: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitBudgetCrashLedgerState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID, callID, expected string) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM model_call_ledger
WHERE owner_id=$1 AND source='client' AND call_id=$2`, ownerID, callID).Scan(&state); err == nil && state == expected {
			return
		} else if err != nil {
			t.Fatalf("read Portal-recovered model ledger: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Portal reconciliation state %q; last state=%q", expected, state)
		case <-ticker.C:
		}
	}
}

func budgetCrashPortalModelRequest(client *http.Client, baseURL, host, sessionCredential, modelID, callID string) (int, []byte, error) {
	body, err := json.Marshal(map[string]any{
		"model": modelID, "messages": []map[string]string{{"role": "user", "content": "Return one synthetic answer."}},
		"max_tokens": budgetCrashSignedOutputTokens,
	})
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/model/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Host = host
	request.Header.Set("Origin", "http://"+host)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", callID)
	request.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, data, err
}

func writeBudgetCrashPortalConfig(t *testing.T, directory, dsn, modelProxyURL, openSandboxURL, apiAddress, gatewayAddress, metricsAddress string) string {
	t.Helper()
	workspaceData := "/dev/shm"
	workspacePrefix := fmt.Sprintf("morde-budget-crash-%d", time.Now().UnixNano())
	workspaceRoot := filepath.Join(workspaceData, workspacePrefix+"-blocks")
	workspaceCurrent := filepath.Join(workspaceData, workspacePrefix+"-current")
	t.Cleanup(func() {
		_ = os.RemoveAll(workspaceRoot)
		_ = os.RemoveAll(workspaceCurrent)
	})
	dockerData := filepath.Join(directory, "docker-data")
	siteStorage := filepath.Join(directory, "site-storage")
	workbench := filepath.Join(directory, "workbench")
	portalWeb := filepath.Join(directory, "portal-web")
	for _, path := range []string{workspaceRoot, workspaceCurrent, dockerData, siteStorage, workbench, portalWeb} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create isolated Portal directory: %v", err)
		}
	}
	dataKeyPath := filepath.Join(directory, "data.key")
	cookieKeyPath := filepath.Join(directory, "cookie.key")
	if err := os.WriteFile(dataKeyPath, bytes.Repeat([]byte{0x31}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cookieKeyPath, bytes.Repeat([]byte{0x52}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	guardPath := filepath.Join(directory, "network-guard")
	if err := os.WriteFile(guardPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	serverRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	templates, err := filepath.Abs(filepath.Join(serverRoot, "..", "deploy", "single", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	dockerHost := "unix://" + filepath.Join(directory, "docker.sock")
	configuration := map[string]any{
		"instance": "budget-portal-process-crash-test",
		"listen":   map[string]any{"api": apiAddress, "gateway": gatewayAddress, "metrics": metricsAddress},
		"http": map[string]any{"read_header_timeout": "5s", "read_timeout": "30s", "write_timeout": "30s",
			"idle_timeout": "60s", "shutdown_timeout": "1s", "api_max_body_bytes": 1048576, "mcp_max_body_bytes": 1048576},
		"domains":  map[string]any{"scheme": "http", "portal": "portal.test", "host_suffix": "host.test", "cookie_secure": false},
		"database": map[string]any{"dsn": dsn, "max_conns": 16},
		"security": map[string]any{"data_key_file": dataKeyPath, "cookie_key_file": cookieKeyPath,
			"session_ttl": "1h", "ticket_ttl": "1m", "host_cookie_ttl": "1h", "login_rate_limit": "5/min"},
		"models": map[string]any{"allowed_hosts": []string{"api.openai.com"}, "proxy_url": modelProxyURL,
			"project_rates": []any{}, "output_budget_admission_enabled": false,
			"output_budget_shadow": map[string]any{"enabled": false, "sample_rate": float64(0)}},
		"orchestrator": map[string]any{"profiles": map[string]any{"container": map[string]any{
			"base_url": openSandboxURL, "api_key": "budget-portal-crash-test-key", "request_timeout": "1s",
			"network_guard_path": guardPath, "network_policy_enabled": false}}},
		"templates":  map[string]any{"dir": templates, "default": "std-1c2g"},
		"hosts":      map[string]any{"ready_timeout": "1s", "ready_poll_interval": "100ms", "runtime_portal_url": "https://portal.test"},
		"capacity":   map[string]any{"cpu_milli": 2000, "mem_mib": 4096},
		"reconciler": map[string]any{"interval": "100ms", "renew_before": "1s", "activity_flush_interval": "100ms"},
		"storage":    map[string]any{"driver": "docker", "docker_host": dockerHost, "docker_data_dir": dockerData, "min_free_bytes": 1},
		"cloud_dsh":  map[string]any{"enabled": false, "builtin_search": map[string]any{"enabled": false}},
		"workspace_storage": map[string]any{"root_dir": workspaceRoot, "max_workspace_bytes": 1048576,
			"max_owner_bytes": 1048576, "min_free_bytes": 1},
		"workspace_sync_node": map[string]any{"enabled": false, "project_upload_enabled": false,
			"agent_workspace_host_mount_enabled": false, "agent_workspace_live_directory_source_enabled": false,
			"data_root": workspaceData, "workspace_root": workspaceCurrent, "protected_storage_roots": []any{}},
		"site_storage": map[string]any{"root_dir": siteStorage, "max_release_bytes": 1048576, "max_owner_bytes": 1048576, "min_free_bytes": 1},
		"gateway": map[string]any{"workbench_dir": workbench, "upstream_cache_ttl": "30s", "credential_cache_ttl": "10s",
			"preview_port_min": 1024, "preview_port_deny": []int{8080, 5900}, "max_body_bytes": 1048576},
		"mcp":        map[string]any{"enabled": false, "max_output_bytes": 65536, "screenshot_max_width": 1280},
		"portal_web": map[string]any{"dir": portalWeb},
		"log":        map[string]any{"level": "error", "format": "json"},
	}
	contents, err := json.Marshal(configuration)
	if err != nil {
		t.Fatalf("encode isolated Portal configuration: %v", err)
	}
	configPath := filepath.Join(directory, "portal.yaml")
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatalf("write isolated Portal configuration: %v", err)
	}
	return configPath
}

func budgetCrashFreeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func budgetCrashDSNForDatabase(t *testing.T, dsn, databaseName string) string {
	t.Helper()
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse Portal crash-test database DSN: %v", err)
		}
		query := parsed.Query()
		query.Set("dbname", databaseName)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return strings.TrimSpace(dsn) + " dbname='" + databaseName + "'"
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
