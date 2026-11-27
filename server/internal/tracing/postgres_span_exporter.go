package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

const (
	maxExportBatchSpans     = 128
	maxSpanPayloadBytes     = 64 << 10
	maxBatchPayloadBytes    = 4 << 20
	maxAttributeCount       = 64
	maxAttributeStringBytes = 1024
	maxSpanAttributesBytes  = 8 << 10
	maxResourceAttrsBytes   = 4 << 10
	maxNestedAttrsBytes     = 1024
	maxSpanEvents           = 32
	maxSpanLinks            = 16
	defaultExportTimeout    = 3 * time.Second
)

var (
	errSpanExporterClosed  = errors.New("postgres span exporter is shut down")
	errInvalidSpanIdentity = errors.New("span identity is invalid")
	errInvalidSpanName     = errors.New("span name is invalid")
)

const insertSpanSQL = `INSERT INTO observability_spans
(trace_id,span_id,parent_span_id,name,span_kind,start_time,end_time,status,scope_name,scope_version,
 attributes,resource,links,events,dropped_attributes,dropped_links,dropped_events)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb,$14::jsonb,$15,$16,$17)
ON CONFLICT (trace_id,span_id) DO NOTHING`

const pruneSpansSQL = `WITH expired AS (
 SELECT ctid FROM observability_spans
 WHERE end_time < $1
 ORDER BY end_time,trace_id,span_id
 LIMIT $2
 FOR UPDATE SKIP LOCKED
)
DELETE FROM observability_spans AS spans USING expired
WHERE spans.ctid=expired.ctid`

const pruneTraceLinksSQL = `WITH expired AS (
 SELECT ctid FROM observability_trace_links
 WHERE created_at < $1
 ORDER BY created_at,trace_id,span_id
 LIMIT $2
 FOR UPDATE SKIP LOCKED
)
DELETE FROM observability_trace_links AS links USING expired
WHERE links.ctid=expired.ctid`

const pruneTaskTraceLinksSQL = `WITH expired AS (
 SELECT ctid FROM observability_task_attempt_links
 WHERE created_at < $1
 ORDER BY created_at,task_id,attempt
 LIMIT $2
 FOR UPDATE SKIP LOCKED
)
DELETE FROM observability_task_attempt_links AS links USING expired
WHERE links.ctid=expired.ctid`

// PostgresSpanExporter stores low-sensitivity span metadata using the Portal's
// existing pgxpool. The SDK's BatchSpanProcessor provides the bounded async
// queue; this exporter bounds each batch and its serialized payload as well.
// It never owns or closes the shared pool.
type PostgresSpanExporter struct {
	pool             spanDatabase
	maxBatch         int
	maxSpanBytes     int
	maxBatchBytes    int
	timeout          time.Duration
	logger           *zap.Logger
	stateMu          sync.Mutex
	shutdown         bool
	activeOperations int
	drained          chan struct{}
	drainOnce        sync.Once
	health           SpanExporterHealth
}

type spanDatabase interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// SpanExporterHealth is an in-process health snapshot. InternalError is for
// server diagnostics only and must not be serialized into an admin response.
type SpanExporterHealth struct {
	Status        string     `json:"status"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	FailedBatches uint64     `json:"failed_batches"`
	ReasonCode    string     `json:"reason_code,omitempty"`
	InternalError string     `json:"-"`
	Shutdown      bool       `json:"shutdown"`
}

var _ sdktrace.SpanExporter = (*PostgresSpanExporter)(nil)

// NewPostgresSpanExporter constructs a bounded exporter over the existing
// Portal pool. It does not open another database connection pool.
func NewPostgresSpanExporter(pool *pgxpool.Pool) (*PostgresSpanExporter, error) {
	return NewPostgresSpanExporterWithLogger(pool, zap.NewNop())
}

// NewPostgresSpanExporterWithLogger constructs a bounded exporter with an
// injected logger. Export errors are logged locally; callers should expose
// only Health().ReasonCode to users, never InternalError or raw SQL errors.
func NewPostgresSpanExporterWithLogger(pool *pgxpool.Pool, logger *zap.Logger) (*PostgresSpanExporter, error) {
	if pool == nil {
		return nil, errors.New("postgres span exporter requires a pool")
	}
	if logger == nil {
		return nil, errors.New("postgres span exporter requires a logger")
	}
	return newPostgresSpanExporter(pool, exporterLimits{
		maxBatch: maxExportBatchSpans, maxSpanBytes: maxSpanPayloadBytes,
		maxBatchBytes: maxBatchPayloadBytes, timeout: defaultExportTimeout,
	}, logger)
}

type exporterLimits struct {
	maxBatch      int
	maxSpanBytes  int
	maxBatchBytes int
	timeout       time.Duration
}

func newPostgresSpanExporter(pool spanDatabase, limits exporterLimits, logger ...*zap.Logger) (*PostgresSpanExporter, error) {
	if pool == nil || limits.maxBatch < 1 || limits.maxSpanBytes < 1 || limits.maxBatchBytes < limits.maxSpanBytes || limits.timeout <= 0 {
		return nil, errors.New("postgres span exporter limits are invalid")
	}
	selectedLogger := zap.NewNop()
	if len(logger) > 1 || len(logger) == 1 && logger[0] == nil {
		return nil, errors.New("postgres span exporter logger is invalid")
	}
	if len(logger) == 1 {
		selectedLogger = logger[0]
	}
	return &PostgresSpanExporter{pool: pool, maxBatch: limits.maxBatch, maxSpanBytes: limits.maxSpanBytes,
		maxBatchBytes: limits.maxBatchBytes, timeout: limits.timeout, logger: selectedLogger,
		drained: make(chan struct{})}, nil
}

// ExportSpans atomically stores one bounded SDK batch. Duplicate span IDs are
// ignored so shutdown flushes or caller retries remain idempotent.
func (e *PostgresSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) (exportErr error) {
	if e == nil || e.pool == nil {
		return errors.New("postgres span exporter is unavailable")
	}
	if ctx == nil {
		exportErr = errors.New("postgres span exporter context is invalid")
		e.recordFailure(exportErr)
		return exportErr
	}
	if err := ctx.Err(); err != nil {
		e.recordFailure(err)
		return err
	}
	if err := e.beginOperation(); err != nil {
		e.recordFailure(err)
		return err
	}
	if len(spans) == 0 {
		e.endOperation()
		return nil
	}
	defer func() {
		if exportErr != nil {
			e.recordFailure(exportErr)
		} else {
			e.recordSuccess()
		}
		e.endOperation()
	}()
	if len(spans) > e.maxBatch {
		return fmt.Errorf("span batch contains %d spans; maximum is %d", len(spans), e.maxBatch)
	}

	exportCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	records := make([]spanRecord, 0, len(spans))
	batchBytes := 0
	for index, span := range spans {
		if span == nil {
			return fmt.Errorf("span at index %d is nil", index)
		}
		record, err := makeSpanRecord(span)
		if err != nil {
			return fmt.Errorf("encode span at index %d: %w", index, err)
		}
		if record.payloadBytes > e.maxSpanBytes {
			return fmt.Errorf("span at index %d exceeds the %d byte payload limit", index, e.maxSpanBytes)
		}
		batchBytes += record.payloadBytes
		if batchBytes > e.maxBatchBytes {
			return fmt.Errorf("span batch exceeds the %d byte payload limit", e.maxBatchBytes)
		}
		records = append(records, record)
	}

	// The transaction makes a batch all-or-none; callers can safely report a
	// failed export as a visible telemetry gap without partial duplicate rows.
	tx, err := e.pool.BeginTx(exportCtx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin span export transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(exportCtx) }()

	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(insertSpanSQL,
			record.traceID, record.spanID, record.parentSpanID, record.name, record.spanKind,
			record.startTime, record.endTime, record.status, record.scopeName, record.scopeVersion,
			record.attributes, record.resource, record.links, record.events,
			record.droppedAttributes, record.droppedLinks, record.droppedEvents)
	}
	results := tx.SendBatch(exportCtx, batch)
	for range records {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("insert span batch: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close span batch: %w", err)
	}
	if err := tx.Commit(exportCtx); err != nil {
		return fmt.Errorf("commit span export transaction: %w", err)
	}
	return nil
}

// PurgeExpiredSpans atomically removes up to limit old spans and trace links.
// Callers should use a seven-day cutoff; each table uses its indexed timestamp
// and the per-table limit is capped at 10000 rows.
func (e *PostgresSpanExporter) PurgeExpiredSpans(ctx context.Context, cutoff time.Time, limit int32) (int64, error) {
	if e == nil || e.pool == nil || ctx == nil || cutoff.IsZero() || limit < 1 || limit > 10000 {
		return 0, errors.New("span retention request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := e.beginOperation(); err != nil {
		return 0, err
	}
	defer e.endOperation()
	cleanupCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	tx, err := e.pool.BeginTx(cleanupCtx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin observability retention transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(cleanupCtx) }()
	spans, err := tx.Exec(cleanupCtx, pruneSpansSQL, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired spans: %w", err)
	}
	links, err := tx.Exec(cleanupCtx, pruneTraceLinksSQL, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired trace links: %w", err)
	}
	taskLinks, err := tx.Exec(cleanupCtx, pruneTaskTraceLinksSQL, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired Task trace links: %w", err)
	}
	if err := tx.Commit(cleanupCtx); err != nil {
		return 0, fmt.Errorf("commit observability retention transaction: %w", err)
	}
	return spans.RowsAffected() + links.RowsAffected() + taskLinks.RowsAffected(), nil
}

// Shutdown prevents new exports but leaves the shared pool open. The SDK
// BatchSpanProcessor is responsible for flushing before this method is called.
func (e *PostgresSpanExporter) Shutdown(ctx context.Context) error {
	if e == nil || ctx == nil {
		return errors.New("postgres span exporter shutdown context is invalid")
	}
	e.stateMu.Lock()
	e.shutdown = true
	if e.activeOperations == 0 {
		e.drainOnce.Do(func() { close(e.drained) })
	}
	drained := e.drained
	e.stateMu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Health returns counters and timestamps for admin overview assembly. The
// InternalError field is intentionally excluded from JSON serialization.
func (e *PostgresSpanExporter) Health() SpanExporterHealth {
	if e == nil {
		return SpanExporterHealth{Status: "unavailable", ReasonCode: "exporter_unavailable"}
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	health := e.health
	health.Shutdown = e.shutdown
	if health.LastSuccessAt != nil {
		lastSuccess := *health.LastSuccessAt
		health.LastSuccessAt = &lastSuccess
	}
	if health.LastFailureAt != nil {
		lastFailure := *health.LastFailureAt
		health.LastFailureAt = &lastFailure
	}
	if health.Shutdown || health.Status == "" {
		health.Status = "unavailable"
	}
	return health
}

func (e *PostgresSpanExporter) beginOperation() error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.shutdown {
		return errSpanExporterClosed
	}
	e.activeOperations++
	return nil
}

func (e *PostgresSpanExporter) endOperation() {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.activeOperations--
	if e.shutdown && e.activeOperations == 0 {
		e.drainOnce.Do(func() { close(e.drained) })
	}
}

func (e *PostgresSpanExporter) recordSuccess() {
	now := time.Now().UTC()
	e.stateMu.Lock()
	e.health.LastSuccessAt = &now
	e.health.Status = "healthy"
	e.health.ReasonCode = ""
	e.stateMu.Unlock()
}

func (e *PostgresSpanExporter) recordFailure(err error) {
	now := time.Now().UTC()
	reason := exportErrorReason(err)
	e.stateMu.Lock()
	e.health.FailedBatches++
	e.health.LastFailureAt = &now
	if e.health.LastSuccessAt == nil {
		e.health.Status = "unavailable"
	} else {
		e.health.Status = "partial"
	}
	e.health.ReasonCode = reason
	e.health.InternalError = err.Error()
	e.stateMu.Unlock()
	if e.logger != nil {
		e.logger.Error("postgres span export failed", zap.String("reason", reason), zap.Error(err))
	}
}

func exportErrorReason(err error) string {
	switch {
	case errors.Is(err, errInvalidSpanName):
		return "invalid_span_name"
	case errors.Is(err, errInvalidSpanIdentity):
		return "invalid_span_identity"
	case errors.Is(err, context.DeadlineExceeded):
		return "export_timeout"
	case errors.Is(err, context.Canceled):
		return "export_cancelled"
	case errors.Is(err, errSpanExporterClosed):
		return "exporter_shutdown"
	default:
		return "span_export_failed"
	}
}

type spanRecord struct {
	traceID           string
	spanID            string
	parentSpanID      string
	name              string
	spanKind          string
	startTime         time.Time
	endTime           time.Time
	status            string
	scopeName         string
	scopeVersion      string
	attributes        []byte
	resource          []byte
	links             []byte
	events            []byte
	droppedAttributes int
	droppedLinks      int
	droppedEvents     int
	payloadBytes      int
}

func makeSpanRecord(span sdktrace.ReadOnlySpan) (spanRecord, error) {
	spanContext := span.SpanContext()
	if !spanContext.IsValid() || span.EndTime().IsZero() || span.StartTime().IsZero() || span.EndTime().Before(span.StartTime()) {
		return spanRecord{}, errInvalidSpanIdentity
	}
	if span.Name() == "" || len(span.Name()) > 256 || !safeSpanName(span.Name()) {
		return spanRecord{}, errInvalidSpanName
	}
	attributes, err := encodeAttributes(span.Attributes(), spanAttributeList)
	if err != nil {
		return spanRecord{}, fmt.Errorf("encode span attributes: %w", err)
	}
	resourceAttributes := []attribute.KeyValue{}
	if span.Resource() != nil {
		resourceAttributes = span.Resource().Attributes()
	}
	resource, err := encodeAttributes(resourceAttributes, resourceAttributeList)
	if err != nil {
		return spanRecord{}, fmt.Errorf("encode span resource: %w", err)
	}
	links, droppedLinks, err := encodeLinks(span.Links())
	if err != nil {
		return spanRecord{}, fmt.Errorf("encode span links: %w", err)
	}
	events, droppedEvents, err := encodeEvents(span.Events())
	if err != nil {
		return spanRecord{}, fmt.Errorf("encode span events: %w", err)
	}
	scope := span.InstrumentationScope()
	if !safeScopeName(scope.Name) || !safeMetadataText(scope.Version, 64) {
		return spanRecord{}, errors.New("instrumentation scope is invalid")
	}
	parent := span.Parent()
	parentSpanID := ""
	if parent.IsValid() {
		parentSpanID = parent.SpanID().String()
	}
	status := "unset"
	switch span.Status().Code {
	case codes.Unset:
	case codes.Ok:
		status = "ok"
	case codes.Error:
		status = "error"
	default:
		return spanRecord{}, errors.New("span status is invalid")
	}

	record := spanRecord{
		traceID: spanContext.TraceID().String(), spanID: spanContext.SpanID().String(),
		parentSpanID: parentSpanID, name: span.Name(), spanKind: span.SpanKind().String(),
		startTime: span.StartTime().UTC(), endTime: span.EndTime().UTC(), status: status,
		scopeName: scope.Name, scopeVersion: scope.Version,
		attributes: attributes, resource: resource, links: links, events: events,
		droppedAttributes: span.DroppedAttributes(), droppedLinks: span.DroppedLinks() + droppedLinks,
		droppedEvents: span.DroppedEvents() + droppedEvents,
	}
	record.payloadBytes = len(attributes) + len(resource) + len(links) + len(events) + len(record.name) + len(record.scopeName) + len(record.scopeVersion)
	return record, nil
}

type attributeAllowList uint8

const (
	spanAttributeList attributeAllowList = iota
	resourceAttributeList
	eventAttributeList
	linkAttributeList
)

func encodeAttributes(values []attribute.KeyValue, allow attributeAllowList) ([]byte, error) {
	ordered := append([]attribute.KeyValue(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	attributes := make(map[string]any, min(len(ordered), maxAttributeCount))
	encodedBytes := 2 // braces
	for _, value := range ordered {
		key := string(value.Key)
		if !allowedAttribute(key, allow) {
			continue
		}
		if _, exists := attributes[key]; exists || len(attributes) >= maxAttributeCount {
			continue
		}
		encoded, ok := safeAttributeValue(value.Value)
		if !ok {
			continue
		}
		encodedValue, err := json.Marshal(encoded)
		if err != nil {
			return nil, fmt.Errorf("marshal attribute value: %w", err)
		}
		entryBytes := len(key) + len(encodedValue) + 4 // JSON key quotes, colon, and a comma
		if encodedBytes+entryBytes > attributePayloadLimit(allow) {
			continue
		}
		encodedBytes += entryBytes
		attributes[key] = encoded
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal attributes: %w", err)
	}
	return encoded, nil
}

func attributePayloadLimit(allow attributeAllowList) int {
	switch allow {
	case spanAttributeList:
		return maxSpanAttributesBytes
	case resourceAttributeList:
		return maxResourceAttrsBytes
	case eventAttributeList, linkAttributeList:
		return maxNestedAttrsBytes
	default:
		return 0
	}
}

func allowedAttribute(key string, allow attributeAllowList) bool {
	switch allow {
	case spanAttributeList:
		switch key {
		case "http.request.method", "http.route", "http.response.status_code", "http.request.body.size", "http.response.body.size",
			"network.protocol.version", "error.type", "exception.type", "exception.escaped", "morde.request_id",
			"morde.user_id", "morde.conversation_id", "morde.message_id", "morde.task_id", "morde.agent_session_id",
			"morde.parent_agent_session_id", "morde.model_id", "morde.provider_model", "morde.tool_name", "morde.call_id",
			"morde.attempt", "morde.operation", "morde.failure_code", "morde.status", "morde.phase",
			"morde.task_revision", "morde.event_sequence", "morde.source":
			return true
		}
	case resourceAttributeList:
		switch key {
		case "service.name", "service.version", "telemetry.sdk.language", "telemetry.sdk.name", "telemetry.sdk.version", "deployment.environment.name":
			return true
		}
	case eventAttributeList:
		switch key {
		case "exception.type", "exception.escaped", "morde.failure_code", "morde.status", "morde.phase",
			"morde.budget.route_fingerprint", "morde.budget.strategy_version", "morde.budget.stage", "morde.budget.outcome",
			"morde.budget.block_reason", "morde.budget.limiting_dimensions", "morde.budget.goal_tokens", "morde.budget.minimum_tokens",
			"morde.budget.safe_tokens", "morde.budget.send_tokens", "morde.budget.current_output_cap", "morde.budget.usage_known",
			"morde.budget.actual_output_tokens", "morde.budget.stop_reason", "morde.budget.failure_class":
			return true
		}
	case linkAttributeList:
		return key == "morde.link.kind"
	}
	return false
}

func safeAttributeValue(value attribute.Value) (any, bool) {
	switch value.Type() {
	case attribute.BOOL:
		return value.AsBool(), true
	case attribute.INT64:
		return value.AsInt64(), true
	case attribute.FLOAT64:
		float := value.AsFloat64()
		return float, !math.IsNaN(float) && !math.IsInf(float, 0)
	case attribute.STRING:
		text := value.AsString()
		return text, safeMetadataText(text, maxAttributeStringBytes)
	case attribute.BOOLSLICE:
		values := value.AsBoolSlice()
		return values, len(values) <= 32
	case attribute.INT64SLICE:
		values := value.AsInt64Slice()
		return values, len(values) <= 32
	case attribute.FLOAT64SLICE:
		values := value.AsFloat64Slice()
		if len(values) > 32 {
			return nil, false
		}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, false
			}
		}
		return values, true
	case attribute.STRINGSLICE:
		values := value.AsStringSlice()
		if len(values) > 32 {
			return nil, false
		}
		for _, value := range values {
			if !safeMetadataText(value, maxAttributeStringBytes) {
				return nil, false
			}
		}
		return values, true
	default:
		return nil, false
	}
}

func encodeLinks(values []sdktrace.Link) ([]byte, int, error) {
	limit := min(len(values), maxSpanLinks)
	links := make([]spanLink, 0, limit)
	dropped := len(values) - limit
	for _, value := range values[:limit] {
		ctx := value.SpanContext
		if !ctx.IsValid() {
			dropped++
			continue
		}
		attributes, err := encodeAttributes(value.Attributes, linkAttributeList)
		if err != nil {
			return nil, dropped, err
		}
		links = append(links, spanLink{TraceID: ctx.TraceID().String(), SpanID: ctx.SpanID().String(), Attributes: json.RawMessage(attributes)})
	}
	if links == nil {
		links = []spanLink{}
	}
	encoded, err := json.Marshal(links)
	return encoded, dropped, err
}

func encodeEvents(values []sdktrace.Event) ([]byte, int, error) {
	limit := min(len(values), maxSpanEvents)
	events := make([]spanEvent, 0, limit)
	dropped := len(values) - limit
	for _, value := range values[:limit] {
		if value.Name == "" || len(value.Name) > 128 || !safeSpanName(value.Name) {
			dropped++
			continue
		}
		attributes, err := encodeAttributes(value.Attributes, eventAttributeList)
		if err != nil {
			return nil, dropped, err
		}
		events = append(events, spanEvent{Name: value.Name, Time: value.Time.UTC(), Attributes: json.RawMessage(attributes)})
	}
	if events == nil {
		events = []spanEvent{}
	}
	encoded, err := json.Marshal(events)
	return encoded, dropped, err
}

type spanLink struct {
	TraceID    string          `json:"trace_id"`
	SpanID     string          `json:"span_id"`
	Attributes json.RawMessage `json:"attributes"`
}

type spanEvent struct {
	Name       string          `json:"name"`
	Time       time.Time       `json:"time"`
	Attributes json.RawMessage `json:"attributes"`
}

func safeSpanName(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "?=\r\n\x00") {
		return false
	}
	if method, route, hasRoute := strings.Cut(value, " "); hasRoute {
		if method == "" || route == "" || strings.Contains(route, " ") || route != "*" && !strings.HasPrefix(route, "/") || !safeOperationToken(method) || !safeOperationToken(route) {
			return false
		}
		return true
	}
	return safeOperationToken(value)
}

func safeOperationToken(value string) bool {
	for _, r := range value {
		isASCIIAlphaNumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isASCIIAlphaNumeric && !strings.ContainsRune("._-/:{}*", r) {
			return false
		}
	}
	return true
}

func safeMetadataText(value string, maxBytes int) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		isASCIIAlphaNumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isASCIIAlphaNumeric && !strings.ContainsRune("._-/:{}*", r) {
			return false
		}
	}
	return true
}

func safeScopeName(value string) bool {
	if len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		isASCIIAlphaNumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isASCIIAlphaNumeric && !strings.ContainsRune("._/-", r) {
			return false
		}
	}
	return true
}
