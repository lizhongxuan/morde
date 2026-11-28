package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	correlatedLogQueueSize = 512
	correlatedLogBatchSize = 64
	correlatedLogMaxBytes  = 8 << 10
	correlatedLogTextLimit = 2048
)

var (
	correlatedLogTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	correlatedLogSpanIDPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	correlatedLogSafeIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,255}$`)
	correlatedLogServicePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	correlatedLogSecretPattern  = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|authorization|cookie)\s*[:=]\s*)[^\s,;]+|([?&](?:token|key|secret|password)=)[^&\s]+|\bsk-[A-Za-z0-9_-]{16,}\b`)
)

const insertCorrelatedLogSQL = `INSERT INTO observability_logs
(trace_id,span_id,request_id,task_id,service,severity,message,attributes,occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9)`

const purgeCorrelatedLogsSQL = `WITH expired AS (
 SELECT ctid FROM observability_logs WHERE occurred_at < $1
 ORDER BY occurred_at,id LIMIT $2 FOR UPDATE SKIP LOCKED
)
DELETE FROM observability_logs logs USING expired WHERE logs.ctid=expired.ctid`

type correlatedLogRecord struct {
	traceID    string
	spanID     string
	requestID  string
	taskID     string
	service    string
	severity   string
	message    string
	attributes []byte
	occurredAt time.Time
}

type CorrelatedLogHealth struct {
	Status         string     `json:"status"`
	DroppedRecords uint64     `json:"dropped_records"`
	FailedBatches  uint64     `json:"failed_batches"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	ReasonCode     string     `json:"reason_code,omitempty"`
	InternalError  string     `json:"-"`
	Shutdown       bool       `json:"shutdown"`
}

// PostgresCorrelatedLogStore writes only bounded, trace-correlated structured
// logs. It reuses the Portal pool and never makes logging wait for PostgreSQL.
type PostgresCorrelatedLogStore struct {
	pool   spanDatabase
	logger *zap.Logger
	queue  chan correlatedLogRecord
	stop   chan struct{}
	done   chan struct{}

	stateMu sync.Mutex
	closed  bool
	stopOne sync.Once

	healthMu sync.RWMutex
	health   CorrelatedLogHealth
}

// NewPostgresCorrelatedLogStore starts a single bounded writer over the
// existing Portal pool. The supplied logger should not include this store's
// core, preventing export errors from recursively enqueueing themselves.
func NewPostgresCorrelatedLogStore(pool *pgxpool.Pool, logger *zap.Logger) (*PostgresCorrelatedLogStore, error) {
	if pool == nil || logger == nil {
		return nil, errors.New("postgres correlated log store requires a pool and logger")
	}
	return newPostgresCorrelatedLogStore(pool, logger), nil
}

func newPostgresCorrelatedLogStore(pool spanDatabase, logger *zap.Logger) *PostgresCorrelatedLogStore {
	store := &PostgresCorrelatedLogStore{pool: pool, logger: logger, queue: make(chan correlatedLogRecord, correlatedLogQueueSize),
		stop: make(chan struct{}), done: make(chan struct{}), health: CorrelatedLogHealth{Status: "available"}}
	go store.run()
	return store
}

// NewCorrelatedLogCore mirrors safe, trace-correlated entries after writing
// them to the configured local logger.
func NewCorrelatedLogCore(next zapcore.Core, store *PostgresCorrelatedLogStore) zapcore.Core {
	return &correlatedLogCore{next: next, store: store}
}

type correlatedLogCore struct {
	next   zapcore.Core
	store  *PostgresCorrelatedLogStore
	fields []zapcore.Field
}

func (c *correlatedLogCore) Enabled(level zapcore.Level) bool { return c.next.Enabled(level) }

func (c *correlatedLogCore) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	merged = append(merged, c.fields...)
	merged = append(merged, fields...)
	return &correlatedLogCore{next: c.next.With(fields), store: c.store, fields: merged}
}

func (c *correlatedLogCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *correlatedLogCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	writeErr := c.next.Write(entry, fields)
	if c.store != nil {
		merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
		merged = append(merged, c.fields...)
		merged = append(merged, fields...)
		c.store.enqueue(entry, merged)
	}
	return writeErr
}

func (c *correlatedLogCore) Sync() error { return c.next.Sync() }

func (s *PostgresCorrelatedLogStore) enqueue(entry zapcore.Entry, fields []zapcore.Field) {
	record, correlated, rejected := makeCorrelatedLogRecord(entry, fields)
	if !correlated {
		return
	}
	if rejected {
		s.recordDrop(1)
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		s.recordDrop(1)
		return
	}
	select {
	case s.queue <- record:
	default:
		s.recordDrop(1)
	}
}

func makeCorrelatedLogRecord(entry zapcore.Entry, fields []zapcore.Field) (correlatedLogRecord, bool, bool) {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		if !correlatedLogFieldAllowed(field.Key) {
			continue
		}
		field.AddTo(encoder)
	}
	values := encoder.Fields
	traceID, ok := values["trace_id"].(string)
	if !ok || traceID == "" {
		return correlatedLogRecord{}, false, false
	}
	if !correlatedLogTraceIDPattern.MatchString(traceID) || entry.Time.IsZero() || !utf8.ValidString(entry.Message) {
		return correlatedLogRecord{}, true, true
	}
	spanID, _ := values["span_id"].(string)
	if spanID != "" && !correlatedLogSpanIDPattern.MatchString(spanID) {
		return correlatedLogRecord{}, true, true
	}
	requestID := safeCorrelatedLogID(values["request_id"])
	taskID := safeCorrelatedLogID(values["task_id"])
	attributes := make(map[string]any)
	for _, key := range []string{"response_id", "user_id", "conversation_id", "message_id", "agent_session_id", "parent_agent_session_id", "model_call_id", "tool_call_id", "attempt", "delivery_id", "claim_generation", "error_code", "error_type", "failure_code", "stage", "status", "duration_ms", "route", "method", "host_id", "operation", "source", "source_kind"} {
		value, exists := values[key]
		if !exists {
			continue
		}
		var safe any
		var ok bool
		if key == "response_id" {
			if candidate := safeCorrelatedLogID(value); candidate != "" {
				safe, ok = candidate, true
			}
		} else {
			safe, ok = safeCorrelatedLogValue(key, value)
		}
		if ok {
			attributes[key] = safe
		}
	}
	if value, exists := values["error"]; exists {
		if safe, ok := safeCorrelatedLogValue("error", value); ok {
			attributes["error"] = safe
		}
	}
	encoded, err := json.Marshal(attributes)
	if err != nil || len(encoded) > correlatedLogMaxBytes || len(entry.Message) > correlatedLogTextLimit {
		return correlatedLogRecord{}, true, true
	}
	message := redactCorrelatedLogText(entry.Message)
	if message == "" {
		return correlatedLogRecord{}, true, true
	}
	service := safeCorrelatedLogService(values["service"])
	if service == "" {
		service = "morde-portal"
	}
	return correlatedLogRecord{traceID: traceID, spanID: spanID, requestID: requestID, taskID: taskID,
		service: service, severity: entry.Level.String(), message: message, attributes: encoded, occurredAt: entry.Time.UTC()}, true, false
}

func correlatedLogFieldAllowed(key string) bool {
	switch key {
	case "trace_id", "span_id", "request_id", "response_id", "task_id", "service", "error",
		"user_id", "conversation_id", "message_id", "agent_session_id", "parent_agent_session_id",
		"model_call_id", "tool_call_id", "attempt", "delivery_id", "claim_generation", "error_code", "error_type", "failure_code", "stage",
		"status", "duration_ms", "route", "method", "host_id", "operation", "source", "source_kind":
		return true
	default:
		return false
	}
}

func safeCorrelatedLogID(value any) string {
	text, ok := value.(string)
	if !ok || !correlatedLogSafeIDPattern.MatchString(text) {
		return ""
	}
	return text
}

func safeCorrelatedLogService(value any) string {
	text, ok := value.(string)
	if !ok || len(text) > 128 || !correlatedLogServicePattern.MatchString(text) {
		return ""
	}
	return text
}

func safeCorrelatedLogValue(key string, value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		if len(typed) > 1024 || !utf8.ValidString(typed) {
			return nil, false
		}
		clean := redactCorrelatedLogText(typed)
		if key != "error" && clean != typed {
			return nil, false
		}
		return clean, true
	case int:
		return typed, true
	case int8:
		return typed, true
	case int16:
		return typed, true
	case int32:
		return typed, true
	case int64:
		return typed, true
	case uint:
		return typed, true
	case uint8:
		return typed, true
	case uint16:
		return typed, true
	case uint32:
		return typed, true
	case uint64:
		return typed, true
	case bool:
		return typed, true
	case float32:
		return typed, true
	case float64:
		return typed, true
	default:
		return nil, false
	}
}

func redactCorrelatedLogText(value string) string {
	value = correlatedLogSecretPattern.ReplaceAllString(value, "[已隐藏凭据]")
	return strings.TrimSpace(value)
}

func (s *PostgresCorrelatedLogStore) run() {
	defer close(s.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]correlatedLogRecord, 0, correlatedLogBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil {
			s.recordFailure(err)
			s.logger.Warn("write correlated observability logs failed", zap.Error(err))
		} else {
			s.recordSuccess()
		}
		batch = batch[:0]
	}
	for {
		select {
		case record := <-s.queue:
			batch = append(batch, record)
			if len(batch) >= correlatedLogBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.stop:
			for {
				select {
				case record := <-s.queue:
					batch = append(batch, record)
					if len(batch) >= correlatedLogBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *PostgresCorrelatedLogStore) writeBatch(records []correlatedLogRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin correlated log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(insertCorrelatedLogSQL, record.traceID, record.spanID, record.requestID, record.taskID,
			record.service, record.severity, record.message, record.attributes, record.occurredAt)
	}
	results := tx.SendBatch(ctx, batch)
	for range records {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("insert correlated log batch: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close correlated log batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit correlated log batch: %w", err)
	}
	return nil
}

// PurgeExpired removes a bounded page of expired correlated logs.
func (s *PostgresCorrelatedLogStore) PurgeExpired(ctx context.Context, cutoff time.Time, limit int32) (int64, error) {
	if s == nil || s.pool == nil || ctx == nil || cutoff.IsZero() || limit < 1 || limit > 10000 {
		return 0, errors.New("correlated log retention request is invalid")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(cleanupCtx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin correlated log retention transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(cleanupCtx) }()
	result, err := tx.Exec(cleanupCtx, purgeCorrelatedLogsSQL, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired correlated logs: %w", err)
	}
	if err := tx.Commit(cleanupCtx); err != nil {
		return 0, fmt.Errorf("commit correlated log retention: %w", err)
	}
	return result.RowsAffected(), nil
}

// Shutdown stops accepting new records and drains the bounded queue.
func (s *PostgresCorrelatedLogStore) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("correlated log shutdown context is invalid")
	}
	s.stateMu.Lock()
	s.closed = true
	s.stopOne.Do(func() { close(s.stop) })
	s.stateMu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Health returns bounded status and queue-loss information for the admin API.
func (s *PostgresCorrelatedLogStore) Health() CorrelatedLogHealth {
	if s == nil {
		return CorrelatedLogHealth{Status: "unavailable", ReasonCode: "log_store_unavailable"}
	}
	s.healthMu.RLock()
	health := s.health
	s.healthMu.RUnlock()
	s.stateMu.Lock()
	health.Shutdown = s.closed
	s.stateMu.Unlock()
	return health
}

func (s *PostgresCorrelatedLogStore) recordDrop(count uint64) {
	s.healthMu.Lock()
	s.health.DroppedRecords += count
	s.health.Status, s.health.ReasonCode = "partial", "log_queue_dropped"
	s.healthMu.Unlock()
}

func (s *PostgresCorrelatedLogStore) recordFailure(err error) {
	s.healthMu.Lock()
	s.health.FailedBatches++
	s.health.Status, s.health.ReasonCode = "partial", "log_write_failed"
	s.health.InternalError = err.Error()
	s.healthMu.Unlock()
}

func (s *PostgresCorrelatedLogStore) recordSuccess() {
	now := time.Now().UTC()
	s.healthMu.Lock()
	s.health.LastSuccessAt = &now
	s.health.InternalError = ""
	if s.health.DroppedRecords == 0 && s.health.FailedBatches == 0 {
		s.health.Status, s.health.ReasonCode = "available", ""
	}
	s.healthMu.Unlock()
}
