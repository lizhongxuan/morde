package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestMakeCorrelatedLogRecordAllowListsAndRedactsFields(t *testing.T) {
	entry := zapcore.Entry{Time: time.Date(2026, 11, 27, 1, 2, 3, 0, time.UTC), Level: zapcore.ErrorLevel, Message: "request failed"}
	fields := []zapcore.Field{
		zap.String("trace_id", "0123456789abcdef0123456789abcdef"),
		zap.String("span_id", "0123456789abcdef"),
		zap.String("request_id", "req-123"),
		zap.String("task_id", "task-123"),
		zap.String("route", "/api/tasks/:id"),
		zap.String("error", "provider returned token=secret-value"),
		zap.String("password", "must-not-be-stored"),
		zap.String("authorization", "Bearer secret-value"),
	}
	record, correlated, rejected := makeCorrelatedLogRecord(entry, fields)
	if !correlated || rejected {
		t.Fatalf("correlated=%v rejected=%v", correlated, rejected)
	}
	if record.traceID != "0123456789abcdef0123456789abcdef" || record.spanID != "0123456789abcdef" ||
		record.requestID != "req-123" || record.taskID != "task-123" || record.severity != "error" || record.service != "morde-portal" {
		t.Fatalf("log identity = %+v", record)
	}
	if strings.Contains(record.message, "secret-value") || strings.Contains(string(record.attributes), "must-not-be-stored") ||
		strings.Contains(string(record.attributes), "authorization") || !strings.Contains(string(record.attributes), "[已隐藏凭据]") {
		t.Fatalf("log record did not redact credentials: message=%q attributes=%s", record.message, record.attributes)
	}
	var attributes map[string]any
	if err := json.Unmarshal(record.attributes, &attributes); err != nil {
		t.Fatal(err)
	}
	if attributes["route"] != "/api/tasks/:id" {
		t.Fatalf("allowlisted route missing: %#v", attributes)
	}
}
func TestCorrelatedLogCoreDrainsTraceLogsAndIgnoresUncorrelatedLogs(t *testing.T) {
	database := &fakeSpanDatabase{tx: &fakeSpanTx{results: &fakeBatchResults{}}}
	store := newPostgresCorrelatedLogStore(database, zap.NewNop())
	base := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	core := NewCorrelatedLogCore(base, store)
	logger := zap.New(core).With(zap.String("trace_id", "0123456789abcdef0123456789abcdef"), zap.String("request_id", "req-1"))
	logger.Info("http request", zap.String("span_id", "0123456789abcdef"))
	zap.New(core).Info("uncorrelated startup message")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if database.tx.batchSize != 1 || !database.tx.committed || database.tx.execCalls != 0 {
		t.Fatalf("shutdown did not flush exactly one correlated record: %+v", database.tx)
	}
	if health := store.Health(); health.Status != "available" || !health.Shutdown || health.LastSuccessAt == nil {
		t.Fatalf("unexpected log store health: %+v", health)
	}
}

func TestCorrelatedLogStoreReportsQueueDropWithoutBlocking(t *testing.T) {
	database := &blockingCorrelatedLogDatabase{started: make(chan struct{}, 1), release: make(chan struct{}), tx: &fakeSpanTx{results: &fakeBatchResults{}}}
	store := newPostgresCorrelatedLogStore(database, zap.NewNop())
	entry := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "correlated request"}
	fields := []zapcore.Field{zap.String("trace_id", "0123456789abcdef0123456789abcdef")}
	store.enqueue(entry, fields)
	select {
	case <-database.started:
	case <-time.After(2 * time.Second):
		t.Fatal("log writer did not start the database flush")
	}
	store.queue = make(chan correlatedLogRecord, 1)
	store.enqueue(entry, fields)
	store.enqueue(entry, fields)
	close(database.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if health := store.Health(); health.Status != "partial" || health.DroppedRecords != 1 || health.ReasonCode != "log_queue_dropped" {
		t.Fatalf("queue loss was not reported: %+v", health)
	}
}

func TestCorrelatedLogStoreReportsDatabaseFailure(t *testing.T) {
	store := newPostgresCorrelatedLogStore(&fakeSpanDatabase{beginErr: errors.New("database unavailable")}, zap.NewNop())
	store.enqueue(zapcore.Entry{Time: time.Now(), Level: zapcore.ErrorLevel, Message: "request failed"},
		[]zapcore.Field{zap.String("trace_id", "0123456789abcdef0123456789abcdef")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if health := store.Health(); health.Status != "partial" || health.FailedBatches != 1 || health.ReasonCode != "log_write_failed" {
		t.Fatalf("database loss was not reported: %+v", health)
	}
}

func TestMakeCorrelatedLogRecordRejectsMalformedTraceAndOversizedMessage(t *testing.T) {
	base := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "ok"}
	_, correlated, rejected := makeCorrelatedLogRecord(base, []zapcore.Field{zap.String("trace_id", "not-a-trace")})
	if !correlated || !rejected {
		t.Fatalf("malformed trace correlated=%v rejected=%v", correlated, rejected)
	}
	base.Message = strings.Repeat("x", correlatedLogTextLimit+1)
	_, correlated, rejected = makeCorrelatedLogRecord(base, []zapcore.Field{zap.String("trace_id", "0123456789abcdef0123456789abcdef")})
	if !correlated || !rejected {
		t.Fatalf("oversized message correlated=%v rejected=%v", correlated, rejected)
	}
	_, correlated, rejected = makeCorrelatedLogRecord(base, []zapcore.Field{zap.String("message", "no trace field")})
	if correlated || rejected {
		t.Fatalf("uncorrelated log correlated=%v rejected=%v", correlated, rejected)
	}
}

func TestPostgresCorrelatedLogStoreIntegration(t *testing.T) {
	pool := newTemporaryTracingPool(t)
	store, err := NewPostgresCorrelatedLogStore(pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	base := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	logger := zap.New(NewCorrelatedLogCore(base, store)).With(
		zap.String("trace_id", "0123456789abcdef0123456789abcdef"),
		zap.String("span_id", "0123456789abcdef"),
		zap.String("request_id", "request-42"),
		zap.String("task_id", "task-42"),
	)
	logger.Error("model request failed", zap.String("stage", "model"), zap.String("error", "token=do-not-persist"))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	var message, severity, traceID, spanID, requestID, taskID string
	var attributes []byte
	if err := pool.QueryRow(context.Background(), `SELECT message,severity,trace_id,span_id,request_id,task_id,attributes::text
FROM observability_logs WHERE trace_id=$1`, "0123456789abcdef0123456789abcdef").Scan(
		&message, &severity, &traceID, &spanID, &requestID, &taskID, &attributes); err != nil {
		t.Fatal(err)
	}
	if message != "model request failed" || severity != "error" || traceID != "0123456789abcdef0123456789abcdef" ||
		spanID != "0123456789abcdef" || requestID != "request-42" || taskID != "task-42" ||
		strings.Contains(string(attributes), "do-not-persist") || !strings.Contains(string(attributes), "[已隐藏凭据]") {
		t.Fatalf("stored correlated log is incomplete or unsafe: message=%q severity=%q attributes=%s", message, severity, attributes)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO observability_logs
(trace_id,service,severity,message,occurred_at) VALUES ($1,'morde-portal','info','expired',$2)`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now().UTC().Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.PurgeExpired(context.Background(), time.Now().UTC().Add(-7*24*time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("purge expired logs = %d, err=%v", deleted, err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM observability_logs WHERE message='expired'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired logs remaining=%d, err=%v", count, err)
	}
}

type blockingCorrelatedLogDatabase struct {
	started chan struct{}
	release chan struct{}
	tx      *fakeSpanTx
}

func (database *blockingCorrelatedLogDatabase) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	select {
	case database.started <- struct{}{}:
	default:
	}
	select {
	case <-database.release:
		return database.tx, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
