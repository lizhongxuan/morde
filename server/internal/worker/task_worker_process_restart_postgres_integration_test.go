package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	storepostgres "morde/server/internal/store/postgres"
)

const (
	m09TaskWorkerHelperModeEnv     = "MORDE_M09_TASK_WORKER_HELPER_MODE"
	m09TaskWorkerHelperDSNEnv      = "MORDE_M09_TASK_WORKER_HELPER_DSN"
	m09TaskWorkerHelperDatabaseEnv = "MORDE_M09_TASK_WORKER_HELPER_DATABASE"
	m09TaskWorkerHelperTaskIDEnv   = "MORDE_M09_TASK_WORKER_HELPER_TASK_ID"
	m09TaskWorkerHelperWorkerIDEnv = "MORDE_M09_TASK_WORKER_HELPER_WORKER_ID"
	m09TaskWorkerHelperSignalEnv   = "MORDE_M09_TASK_WORKER_HELPER_SIGNAL"

	m09TaskWorkerLeaseDuration = 3 * time.Second
)

type m09TaskWorkerHelperSignal struct {
	Event        string `json:"event"`
	TaskID       string `json:"task_id"`
	WorkerID     string `json:"worker_id"`
	LeaseVersion int64  `json:"lease_version"`
}

type m09TaskWorkerSubprocess struct {
	cmd      *exec.Cmd
	waitDone chan error
	waited   bool
	waitErr  error
}

func TestTaskWorkerProcessRestartRecoversClaimedTask(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL TaskWorker process restart integration was not run")
	}
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL for local PostgreSQL validation")
	}
	if !m09TaskWorkerLocalPostgresHost(baseConfig.ConnConfig.Host) {
		t.Skip("CH_TEST_DATABASE_URL must point to local PostgreSQL; refusing a non-local host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatalf("create TaskWorker restart test audit service: %v", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatalf("create TaskWorker restart test credentials: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify TaskWorker restart test owner session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create TaskWorker restart test Task service: %v", err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "synthetic-local-model", Objective: "recover after a TaskWorker service process restart",
		Budget:         task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100},
		IdempotencyKey: "m09-worker-process-restart-001",
	})
	if err != nil {
		t.Fatalf("submit synthetic TaskWorker restart task: %v", err)
	}

	databaseName := pool.Config().ConnConfig.Database
	if databaseName == "" {
		t.Fatal("temporary PostgreSQL fixture did not expose its isolated database name")
	}
	firstSignalPath := filepath.Join(t.TempDir(), "worker-one.json")
	firstWorkerID := "m09-process-worker-one"
	firstProcess := startM09TaskWorkerSubprocess(t, dsn, databaseName, submitted.Task.ID, firstWorkerID, "claimed", firstSignalPath)
	firstSignal := waitM09TaskWorkerSignal(t, ctx, firstProcess, firstSignalPath)
	if firstSignal.Event != "claimed" || firstSignal.TaskID != submitted.Task.ID || firstSignal.WorkerID != firstWorkerID || firstSignal.LeaseVersion < 1 {
		t.Fatalf("first TaskWorker helper signal = %+v; want the submitted Task claimed by %q", firstSignal, firstWorkerID)
	}
	firstDetails, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || firstDetails.Task.Status != task.StatusRunning || firstDetails.Task.Lease.Owner != firstWorkerID ||
		firstDetails.Task.Lease.Version != firstSignal.LeaseVersion || firstDetails.Task.Lease.ExpiresAt == nil {
		t.Fatalf("first process claim was not persisted: task=%+v err=%v", firstDetails.Task, err)
	}

	if err := firstProcess.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill first TaskWorker service process: %v", err)
	}
	if err := firstProcess.wait(ctx); err == nil {
		t.Fatal("first TaskWorker service process exited successfully after forced termination")
	}
	if err := waitM09TaskLeaseExpiry(ctx, pool, submitted.Task.ID); err != nil {
		t.Fatalf("wait for first TaskWorker lease to expire: %v", err)
	}

	secondWorkerID := "m09-process-worker-two"
	secondSignalPath := filepath.Join(t.TempDir(), "worker-two.json")
	secondProcess := startM09TaskWorkerSubprocess(t, dsn, databaseName, submitted.Task.ID, secondWorkerID, "complete", secondSignalPath)
	secondSignal := waitM09TaskWorkerSignal(t, ctx, secondProcess, secondSignalPath)
	if secondSignal.Event != "completed" || secondSignal.TaskID != submitted.Task.ID || secondSignal.WorkerID != secondWorkerID ||
		secondSignal.LeaseVersion <= firstSignal.LeaseVersion {
		t.Fatalf("recovery TaskWorker helper signal = %+v; want the same Task claimed by a newer lease from %q", secondSignal, secondWorkerID)
	}
	if err := secondProcess.wait(ctx); err != nil {
		t.Fatalf("recovery TaskWorker service process failed: %v", err)
	}

	details, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != m09TaskWorkerRestartResult ||
		details.Task.Lease.Owner != "" || details.Task.Lease.Version != secondSignal.LeaseVersion || details.Task.Lease.ExpiresAt != nil {
		t.Fatalf("Task after Go TaskWorker process restart = status %q result %q lease=%+v err=%v",
			details.Task.Status, details.Task.Result, details.Task.Lease, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 100)
	if err != nil {
		t.Fatalf("list Task events after Go TaskWorker process restart: %v", err)
	}
	completionEvents := 0
	for _, event := range events {
		if event.Type != "task.result" {
			continue
		}
		completionEvents++
		var payload map[string]string
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["result"] != m09TaskWorkerRestartResult {
			t.Fatalf("persisted Task completion event = %+v payload=%q err=%v", event, event.Payload, err)
		}
	}
	if completionEvents != 1 {
		t.Fatalf("persisted Task completion event count = %d, want exactly one; events=%+v", completionEvents, events)
	}
}

const m09TaskWorkerRestartResult = "completed-after-go-taskworker-process-restart"

func TestTaskWorkerProcessRestartHelper(t *testing.T) {
	mode := os.Getenv(m09TaskWorkerHelperModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode != "claimed" && mode != "complete" {
		t.Fatalf("unknown TaskWorker helper mode %q", mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	dsn := os.Getenv(m09TaskWorkerHelperDSNEnv)
	databaseName := os.Getenv(m09TaskWorkerHelperDatabaseEnv)
	taskID := os.Getenv(m09TaskWorkerHelperTaskIDEnv)
	workerID := os.Getenv(m09TaskWorkerHelperWorkerIDEnv)
	signalPath := os.Getenv(m09TaskWorkerHelperSignalEnv)
	if strings.TrimSpace(dsn) == "" || databaseName == "" || taskID == "" || workerID == "" || signalPath == "" {
		t.Fatal("TaskWorker helper process configuration is incomplete")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse TaskWorker helper database configuration")
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect TaskWorker helper to isolated local PostgreSQL database: %v", err)
	}
	defer pool.Close()
	store := storepostgres.New(pool)

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	executor := taskExecutorFunc(func(executionCtx context.Context, claimed task.Task) error {
		if claimed.ID != taskID || claimed.Lease.Owner != workerID || claimed.Lease.Version < 1 {
			return fmt.Errorf("TaskWorker helper received unexpected Task claim: id=%q lease=%+v", claimed.ID, claimed.Lease)
		}
		if mode == "claimed" {
			if err := writeM09TaskWorkerSignal(signalPath, m09TaskWorkerHelperSignal{
				Event: "claimed", TaskID: claimed.ID, WorkerID: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			}); err != nil {
				return fmt.Errorf("write TaskWorker claim signal: %w", err)
			}
			<-executionCtx.Done()
			return executionCtx.Err()
		}

		payload, err := json.Marshal(map[string]string{"result": m09TaskWorkerRestartResult})
		if err != nil {
			return fmt.Errorf("marshal TaskWorker restart result: %w", err)
		}
		if _, err := store.CommitTaskProgress(executionCtx, app.TaskProgress{
			TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			Status: task.StatusCompleted, Result: m09TaskWorkerRestartResult,
			Event: task.Event{Type: "task.result", Payload: payload},
		}); err != nil {
			return fmt.Errorf("commit TaskWorker restart result: %w", err)
		}
		if err := writeM09TaskWorkerSignal(signalPath, m09TaskWorkerHelperSignal{
			Event: "completed", TaskID: claimed.ID, WorkerID: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		}); err != nil {
			return fmt.Errorf("write TaskWorker completion signal: %w", err)
		}
		stopWorker()
		return nil
	})
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: workerID, LeaseDuration: m09TaskWorkerLeaseDuration,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create TaskWorker helper: %v", err)
	}
	if err := worker.Run(workerCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("TaskWorker helper Run() = %v, want context cancellation", err)
	}
}

func m09TaskWorkerLocalPostgresHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "/")
}

func startM09TaskWorkerSubprocess(t *testing.T, dsn, databaseName, taskID, workerID, mode, signalPath string) *m09TaskWorkerSubprocess {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve current Go test binary: %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestTaskWorkerProcessRestartHelper$", "-test.timeout=75s")
	command.Env = []string{
		m09TaskWorkerHelperModeEnv + "=" + mode,
		m09TaskWorkerHelperDSNEnv + "=" + dsn,
		m09TaskWorkerHelperDatabaseEnv + "=" + databaseName,
		m09TaskWorkerHelperTaskIDEnv + "=" + taskID,
		m09TaskWorkerHelperWorkerIDEnv + "=" + workerID,
		m09TaskWorkerHelperSignalEnv + "=" + signalPath,
	}
	process := &m09TaskWorkerSubprocess{cmd: command, waitDone: make(chan error, 1)}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatalf("start TaskWorker helper process: %v", err)
	}
	go func() { process.waitDone <- command.Wait() }()
	t.Cleanup(func() {
		if !process.waited {
			_ = process.kill()
			_ = process.wait(context.Background())
		}
	})
	return process
}

func (process *m09TaskWorkerSubprocess) kill() error {
	if process.waited || process.cmd.Process == nil {
		return nil
	}
	if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (process *m09TaskWorkerSubprocess) wait(ctx context.Context) error {
	if process.waited {
		return process.waitErr
	}
	select {
	case err := <-process.waitDone:
		process.waited = true
		process.waitErr = err
		return err
	case <-ctx.Done():
		_ = process.kill()
		err := <-process.waitDone
		process.waited = true
		process.waitErr = err
		return ctx.Err()
	}
}

func (process *m09TaskWorkerSubprocess) exited() (bool, error) {
	if process.waited {
		return true, process.waitErr
	}
	select {
	case err := <-process.waitDone:
		process.waited = true
		process.waitErr = err
		return true, err
	default:
		return false, nil
	}
}

func waitM09TaskWorkerSignal(t *testing.T, ctx context.Context, process *m09TaskWorkerSubprocess, path string) m09TaskWorkerHelperSignal {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var signal m09TaskWorkerHelperSignal
			if err := json.Unmarshal(data, &signal); err != nil {
				t.Fatalf("decode TaskWorker helper signal: %v", err)
			}
			return signal
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read TaskWorker helper signal: %v", err)
		}
		if exited, waitErr := process.exited(); exited {
			t.Fatalf("TaskWorker helper exited before signaling: %v", waitErr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for TaskWorker helper signal: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeM09TaskWorkerSignal(path string, signal m09TaskWorkerHelperSignal) error {
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

func waitM09TaskLeaseExpiry(ctx context.Context, pool *pgxpool.Pool, taskID string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at <= clock_timestamp() FROM tasks WHERE id=$1`, taskID).Scan(&expired); err != nil {
			return fmt.Errorf("read Task lease expiry: %w", err)
		}
		if expired {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
