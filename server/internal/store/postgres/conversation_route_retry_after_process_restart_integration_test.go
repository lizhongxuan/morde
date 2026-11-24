package postgres

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/worker"
)

const (
	m09RouteProcessModeEnv     = "MORDE_M09_ROUTE_PROCESS_MODE"
	m09RouteProcessDSNEnv      = "MORDE_M09_ROUTE_PROCESS_DSN"
	m09RouteProcessDatabaseEnv = "MORDE_M09_ROUTE_PROCESS_DATABASE"
	m09RouteProcessMessageEnv  = "MORDE_M09_ROUTE_PROCESS_MESSAGE_ID"
	m09RouteProcessWorkerEnv   = "MORDE_M09_ROUTE_PROCESS_WORKER_ID"
	m09RouteProcessSignalEnv   = "MORDE_M09_ROUTE_PROCESS_SIGNAL"

	m09RouteRetryAfter        = 5 * time.Minute
	m09RouteRetryAfterHorizon = 48 * time.Hour
)

type m09RouteProcessSignal struct {
	Mode          string `json:"mode"`
	MessageID     string `json:"message_id"`
	WorkerID      string `json:"worker_id"`
	Found         bool   `json:"found"`
	ProviderCalls int32  `json:"provider_calls"`
}

func TestConversationRouteRetryAfterWaitSurvivesGoWorkerProcessRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := m09LocalRouteTestDSN(t)
	store, pool, initialClaim := m09RouteProcessFixture(t, ctx)
	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("temporary PostgreSQL fixture did not expose its isolated database name")
	}

	firstWorkerID := "m09-route-wait-worker-one"
	first := m09RunRouteWorkerProcess(t, ctx, dsn, databaseName, initialClaim.Message.ID, firstWorkerID, "wait-first")
	if first.Mode != "wait-first" || first.MessageID != initialClaim.Message.ID || first.WorkerID != firstWorkerID ||
		!first.Found || first.ProviderCalls != 1 {
		t.Fatalf("first route process result = %+v; want one provider rate-limit call", first)
	}

	var scheduledState string
	var scheduledAttempt int64
	var availableAfter time.Time
	var dueAtLeastFourMinutesAway, leaseIsClear bool
	if err := pool.QueryRow(ctx, `SELECT state,route_attempt,available_after,
available_after>clock_timestamp()+interval '4 minutes',(lease_owner='' AND lease_expires_at IS NULL)
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).Scan(
		&scheduledState, &scheduledAttempt, &availableAfter, &dueAtLeastFourMinutesAway, &leaseIsClear); err != nil {
		t.Fatalf("read persisted Retry-After state before Go process restart: %v", err)
	}
	if scheduledState != "pending" || scheduledAttempt != initialClaim.RouteAttempt+1 ||
		!dueAtLeastFourMinutesAway || !leaseIsClear {
		t.Fatalf("rate-limited route before restart: state=%q attempt=%d available_after=%s at_least_four_minutes=%t lease_clear=%t",
			scheduledState, scheduledAttempt, availableAfter, dueAtLeastFourMinutesAway, leaseIsClear)
	}
	m09AssertRouteCallRejected(t, ctx, store, initialClaim.Message.ID, initialClaim.RouteAttempt)

	secondWorkerID := "m09-route-wait-worker-two"
	second := m09RunRouteWorkerProcess(t, ctx, dsn, databaseName, initialClaim.Message.ID, secondWorkerID, "wait-probe")
	if second.Mode != "wait-probe" || second.MessageID != initialClaim.Message.ID || second.WorkerID != secondWorkerID ||
		second.Found || second.ProviderCalls != 0 {
		t.Fatalf("restarted waiting route process result = %+v; want no claim and no provider call", second)
	}

	var finalState string
	var finalAttempt int64
	var persistedAvailableAfter time.Time
	var dueAfterRestart, finalLeaseIsClear bool
	if err := pool.QueryRow(ctx, `SELECT state,route_attempt,available_after,
available_after>clock_timestamp(),(lease_owner='' AND lease_expires_at IS NULL)
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).Scan(
		&finalState, &finalAttempt, &persistedAvailableAfter, &dueAfterRestart, &finalLeaseIsClear); err != nil {
		t.Fatalf("read persisted Retry-After state after Go process restart: %v", err)
	}
	if finalState != "pending" || finalAttempt != scheduledAttempt || !persistedAvailableAfter.Equal(availableAfter) ||
		!dueAfterRestart || !finalLeaseIsClear {
		t.Fatalf("route wait changed across Go process restart: state=%q attempt=%d before=%s after=%s due=%t lease_clear=%t",
			finalState, finalAttempt, availableAfter, persistedAvailableAfter, dueAfterRestart, finalLeaseIsClear)
	}
	var routeCalls int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_route_model_calls
WHERE message_id=$1 AND phase='normal'`, initialClaim.Message.ID).Scan(&routeCalls); err != nil || routeCalls != 1 {
		t.Fatalf("route model call count after process restart = %d, want one; err=%v", routeCalls, err)
	}
}

func TestConversationRouteRetryAfterBeyondDurableHorizonStaysFailedAcrossGoWorkerProcessRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := m09LocalRouteTestDSN(t)
	store, pool, initialClaim := m09RouteProcessFixture(t, ctx)
	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("temporary PostgreSQL fixture did not expose its isolated database name")
	}

	firstWorkerID := "m09-route-horizon-worker-one"
	first := m09RunRouteWorkerProcess(t, ctx, dsn, databaseName, initialClaim.Message.ID, firstWorkerID, "horizon-first")
	if first.Mode != "horizon-first" || first.MessageID != initialClaim.Message.ID || first.WorkerID != firstWorkerID ||
		!first.Found || first.ProviderCalls != 1 {
		t.Fatalf("first over-horizon route process result = %+v; want one provider rate-limit call", first)
	}

	var failedState, failureReason string
	var failedAttempt int64
	var leaseIsClear bool
	if err := pool.QueryRow(ctx, `SELECT state,failure_reason,route_attempt,(lease_owner='' AND lease_expires_at IS NULL)
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).Scan(
		&failedState, &failureReason, &failedAttempt, &leaseIsClear); err != nil {
		t.Fatalf("read persisted over-horizon route state before Go process restart: %v", err)
	}
	if failedState != "failed" || failureReason == "" || failedAttempt != initialClaim.RouteAttempt || !leaseIsClear {
		t.Fatalf("over-horizon route before restart: state=%q reason=%q attempt=%d lease_clear=%t",
			failedState, failureReason, failedAttempt, leaseIsClear)
	}
	m09AssertRouteCallRejected(t, ctx, store, initialClaim.Message.ID, initialClaim.RouteAttempt)

	secondWorkerID := "m09-route-horizon-worker-two"
	second := m09RunRouteWorkerProcess(t, ctx, dsn, databaseName, initialClaim.Message.ID, secondWorkerID, "horizon-probe")
	if second.Mode != "horizon-probe" || second.MessageID != initialClaim.Message.ID || second.WorkerID != secondWorkerID ||
		second.Found || second.ProviderCalls != 0 {
		t.Fatalf("restarted failed route process result = %+v; want no claim and no provider call", second)
	}

	var finalState, finalFailureReason string
	var finalAttempt int64
	var finalLeaseIsClear bool
	if err := pool.QueryRow(ctx, `SELECT state,failure_reason,route_attempt,(lease_owner='' AND lease_expires_at IS NULL)
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).Scan(
		&finalState, &finalFailureReason, &finalAttempt, &finalLeaseIsClear); err != nil {
		t.Fatalf("read persisted over-horizon route state after Go process restart: %v", err)
	}
	if finalState != "failed" || finalFailureReason != failureReason || finalAttempt != failedAttempt || !finalLeaseIsClear {
		t.Fatalf("failed route changed across Go process restart: state=%q reason=%q attempt=%d lease_clear=%t",
			finalState, finalFailureReason, finalAttempt, finalLeaseIsClear)
	}
	var routeCalls int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_route_model_calls
WHERE message_id=$1 AND phase='normal'`, initialClaim.Message.ID).Scan(&routeCalls); err != nil || routeCalls != 1 {
		t.Fatalf("route model call count after failed-route process restart = %d, want one; err=%v", routeCalls, err)
	}
}

func TestConversationRouteRetryAfterProcessRestartHelper(t *testing.T) {
	mode := os.Getenv(m09RouteProcessModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := os.Getenv(m09RouteProcessDSNEnv)
	databaseName := os.Getenv(m09RouteProcessDatabaseEnv)
	messageID := os.Getenv(m09RouteProcessMessageEnv)
	workerID := os.Getenv(m09RouteProcessWorkerEnv)
	signalPath := os.Getenv(m09RouteProcessSignalEnv)
	if strings.TrimSpace(dsn) == "" || databaseName == "" || messageID == "" || workerID == "" || signalPath == "" {
		t.Fatal("route worker helper configuration is incomplete")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse local route worker helper database configuration")
	}
	if !m09LocalPostgresHost(config.ConnConfig.Host) {
		t.Fatal("route worker helper refuses a non-local PostgreSQL host")
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("connect route worker helper to isolated local PostgreSQL database")
	}
	defer pool.Close()
	store := New(pool)

	client := &routeModelClientTest{reply: `{"action":"reply","task_id":"","question":"","reply":"local route process restart fixture"}`}
	switch mode {
	case "wait-first":
		client.err = &app.ModelClientError{Kind: app.ModelErrorRateLimited, Stage: "provider_status", RetryAfter: m09RouteRetryAfter}
	case "horizon-first":
		client.err = &app.ModelClientError{Kind: app.ModelErrorRateLimited, Stage: "provider_status", RetryAfter: m09RouteRetryAfterHorizon}
	case "wait-probe", "horizon-probe":
	default:
		t.Fatalf("unknown route worker helper mode %q", mode)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("create local route test sealer: %v", err)
	}
	models, err := app.NewModelConnectionService(store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatalf("create local route model connection service: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, models, client)
	if err != nil {
		t.Fatalf("create local route model gateway: %v", err)
	}
	routeWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: gateway, WorkerID: workerID,
		LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create local ConversationRouteWorker: %v", err)
	}
	found, err := routeWorker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run local ConversationRouteWorker once: %v", err)
	}
	wantFound := mode == "wait-first" || mode == "horizon-first"
	wantProviderCalls := int32(0)
	if wantFound {
		wantProviderCalls = 1
	}
	if found != wantFound || client.calls.Load() != wantProviderCalls {
		t.Fatalf("route worker helper mode %q: found=%t provider_calls=%d; want found=%t calls=%d",
			mode, found, client.calls.Load(), wantFound, wantProviderCalls)
	}
	if err := writeM09RouteProcessSignal(signalPath, m09RouteProcessSignal{
		Mode: mode, MessageID: messageID, WorkerID: workerID, Found: found, ProviderCalls: client.calls.Load(),
	}); err != nil {
		t.Fatalf("write route worker helper signal: %v", err)
	}
}

func m09RouteProcessFixture(t *testing.T, ctx context.Context) (*Store, *pgxpool.Pool, app.ConversationRouteClaim) {
	t.Helper()
	store, _, _, claim, _ := routeModelFixture(t, false, &routeModelClientTest{})
	if err := store.ReleaseConversationRoute(ctx, claim); err != nil {
		t.Fatalf("release fixture route before process test: %v", err)
	}
	return store, store.pool, claim
}

func m09LocalRouteTestDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL route process restart integration was not run")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for local PostgreSQL validation")
	}
	if !m09LocalPostgresHost(config.ConnConfig.Host) {
		t.Skip("CH_TEST_DATABASE_URL must point to local PostgreSQL; refusing a non-local host")
	}
	return dsn
}

func m09LocalPostgresHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "/")
}

func m09RunRouteWorkerProcess(t *testing.T, ctx context.Context, dsn, databaseName, messageID, workerID, mode string) m09RouteProcessSignal {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve current Go test binary: %v", err)
	}
	signalPath := filepath.Join(t.TempDir(), mode+".json")
	command := exec.CommandContext(ctx, executable, "-test.run=^TestConversationRouteRetryAfterProcessRestartHelper$", "-test.timeout=35s")
	command.Env = []string{
		m09RouteProcessModeEnv + "=" + mode,
		m09RouteProcessDSNEnv + "=" + dsn,
		m09RouteProcessDatabaseEnv + "=" + databaseName,
		m09RouteProcessMessageEnv + "=" + messageID,
		m09RouteProcessWorkerEnv + "=" + workerID,
		m09RouteProcessSignalEnv + "=" + signalPath,
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		t.Fatalf("run route worker helper process %q: %v", mode, err)
	}
	data, err := os.ReadFile(signalPath)
	if err != nil {
		t.Fatalf("read route worker helper signal for mode %q: %v", mode, err)
	}
	var signal m09RouteProcessSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		t.Fatalf("decode route worker helper signal for mode %q: %v", mode, err)
	}
	return signal
}

func writeM09RouteProcessSignal(path string, signal m09RouteProcessSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func m09AssertRouteCallRejected(t *testing.T, ctx context.Context, store *Store, messageID string, attempt int64) {
	t.Helper()
	var routeState, ledgerState string
	if err := store.pool.QueryRow(ctx, `SELECT route_call.state,ledger.state
FROM conversation_route_model_calls route_call
JOIN model_call_ledger ledger ON ledger.owner_id=route_call.actor_id
 AND ledger.source=route_call.source AND ledger.call_id=route_call.call_id
WHERE route_call.message_id=$1 AND route_call.attempt=$2 AND route_call.phase='normal'`, messageID, attempt).
		Scan(&routeState, &ledgerState); err != nil || routeState != "rejected" || ledgerState != "rejected" {
		t.Fatalf("route model rejection evidence: route=%q ledger=%q err=%v", routeState, ledgerState, err)
	}
}
