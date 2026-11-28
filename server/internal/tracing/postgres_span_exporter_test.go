package tracing

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"morde/server/migrations"
)

func TestMakeSpanRecordStoresOnlyAllowlistedMetadata(t *testing.T) {
	span := endedTestSpan(t, func(span trace.Span) {
		span.SetAttributes(
			attribute.String("http.request.method", "POST"),
			attribute.String("http.route", "/api/tasks/:id"),
			attribute.String("morde.task_id", "q1234567"),
			attribute.String("error.type", "model output includes secret text"),
			attribute.String("url.full", "https://example.test/path?token=secret"),
			attribute.String("morde.prompt", "private reasoning"),
		)
		span.AddEvent("exception", trace.WithAttributes(
			attribute.String("exception.type", "timeout"),
			attribute.String("exception.message", "private response"),
		))
		span.SetStatus(codes.Error, "sensitive error description")
	})

	record, err := makeSpanRecord(span)
	if err != nil {
		t.Fatal(err)
	}
	if record.traceID == "" || record.spanID == "" || record.name != "GET /api/tasks/:id" || record.status != "error" {
		t.Fatalf("unexpected span identity: %+v", record)
	}
	var attributes map[string]any
	if err := json.Unmarshal(record.attributes, &attributes); err != nil {
		t.Fatal(err)
	}
	if attributes["http.route"] != "/api/tasks/:id" || attributes["morde.task_id"] != "q1234567" || attributes["http.request.method"] != "POST" || len(attributes) != 3 {
		t.Fatalf("stored attributes = %#v", attributes)
	}
	var events []spanEvent
	if err := json.Unmarshal(record.events, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Attributes) != `{"exception.type":"timeout"}` {
		t.Fatalf("stored events = %#v", events)
	}
	for _, payload := range [][]byte{record.attributes, record.resource, record.links, record.events} {
		if strings.Contains(string(payload), "secret") || strings.Contains(string(payload), "private") {
			t.Fatalf("sensitive span content was retained: %s", payload)
		}
	}
}

func TestMakeSpanRecordPreservesBoundedModelBudgetShadowEvents(t *testing.T) {
	span := endedTestSpan(t, func(span trace.Span) {
		span.AddEvent("model_budget_shadow_plan", trace.WithAttributes(
			attribute.String("morde.budget.route_fingerprint", "sha256:"+strings.Repeat("a", 64)),
			attribute.String("morde.budget.strategy_version", "shadow-v1"),
			attribute.String("morde.budget.stage", "tool_step"),
			attribute.String("morde.budget.outcome", "calculated"),
			attribute.String("morde.budget.block_reason", ""),
			attribute.StringSlice("morde.budget.limiting_dimensions", []string{"task_budget", "context_window"}),
			attribute.Int64("morde.budget.goal_tokens", 4096),
			attribute.Int64("morde.budget.minimum_tokens", 512),
			attribute.Int64("morde.budget.safe_tokens", 3072),
			attribute.Int64("morde.budget.send_tokens", 3072),
			attribute.Int64("morde.budget.current_output_cap", 2048),
			attribute.String("morde.prompt", "must be filtered"),
			attribute.String("url.full", "https://private-provider.example/secret"),
		))
		span.AddEvent("model_budget_shadow_result", trace.WithAttributes(
			attribute.String("morde.budget.stage", "tool_step"),
			attribute.Bool("morde.budget.usage_known", true),
			attribute.Int64("morde.budget.actual_output_tokens", 2187),
			attribute.String("morde.budget.stop_reason", "stop"),
			attribute.String("morde.budget.failure_class", "none"),
		))
	})

	record, err := makeSpanRecord(span)
	if err != nil {
		t.Fatal(err)
	}
	var events []spanEvent
	if err := json.Unmarshal(record.events, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || len(events[0].Attributes) > maxNestedAttrsBytes {
		t.Fatalf("model budget shadow events = %d, plan payload bytes=%d", len(events), len(events[0].Attributes))
	}
	var plan map[string]any
	if err := json.Unmarshal(events[0].Attributes, &plan); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"morde.budget.route_fingerprint", "morde.budget.strategy_version", "morde.budget.stage",
		"morde.budget.goal_tokens", "morde.budget.minimum_tokens", "morde.budget.safe_tokens", "morde.budget.send_tokens",
		"morde.budget.current_output_cap", "morde.budget.limiting_dimensions"} {
		if _, ok := plan[key]; !ok {
			t.Fatalf("model budget shadow plan did not preserve %q: %s", key, events[0].Attributes)
		}
	}
	if strings.Contains(string(record.events), "private-provider") || strings.Contains(string(record.events), "must be filtered") {
		t.Fatalf("shadow trace persisted raw route or prompt: %s", record.events)
	}
}

func TestEncodeAttributesEnforcesAllowlistAndValueBounds(t *testing.T) {
	values := []attribute.KeyValue{
		attribute.String("http.route", "/"+strings.Repeat("a", maxAttributeStringBytes)),
		attribute.String("morde.prompt", "must be filtered"),
		attribute.String("morde.task_id", "q1234567"),
	}
	encoded, err := encodeAttributes(values, spanAttributeList)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result["morde.task_id"] != "q1234567" {
		t.Fatalf("bounded attributes = %#v", result)
	}
	if allowedAttribute("exception.message", eventAttributeList) || allowedAttribute("url.full", spanAttributeList) {
		t.Fatal("sensitive keys unexpectedly passed the allowlist")
	}
	if safeMetadataText("user asked me to reveal a secret", maxAttributeStringBytes) || safeSpanName("user asked me to reveal a secret") {
		t.Fatal("free-form text was accepted as span metadata")
	}
	if !safeSpanName("GET /api/tasks/:id") || !safeSpanName("morde.tool.execute") {
		t.Fatal("valid operation names were rejected")
	}
}

func TestMakeSpanRecordClassifiesInvalidSpanNames(t *testing.T) {
	span := endedTestSpan(t, func(span trace.Span) { span.SetName("GET /api/private?query=value") })
	if _, err := makeSpanRecord(span); !errors.Is(err, errInvalidSpanName) {
		t.Fatalf("invalid span name error = %v, want errInvalidSpanName", err)
	}
	if got := exportErrorReason(errInvalidSpanName); got != "invalid_span_name" {
		t.Fatalf("invalid span name health reason = %q", got)
	}
	if got := exportErrorReason(errInvalidSpanIdentity); got != "invalid_span_identity" {
		t.Fatalf("invalid span identity health reason = %q", got)
	}
}

func TestEncodeAttributesEnforcesAggregatePayloadBounds(t *testing.T) {
	keys := []string{
		"http.request.method", "http.route", "http.response.status_code", "http.request.body.size", "http.response.body.size",
		"network.protocol.version", "error.type", "exception.type", "exception.escaped", "morde.request_id",
		"morde.user_id", "morde.conversation_id", "morde.message_id", "morde.task_id", "morde.agent_session_id",
		"morde.parent_agent_session_id", "morde.model_id", "morde.provider_model", "morde.tool_name", "morde.call_id",
		"morde.attempt", "morde.operation", "morde.failure_code", "morde.status", "morde.phase",
		"morde.task_revision", "morde.event_sequence", "morde.source",
	}
	values := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		values = append(values, attribute.String(key, strings.Repeat("x", maxAttributeStringBytes)))
	}

	encoded, err := encodeAttributes(values, spanAttributeList)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxSpanAttributesBytes {
		t.Fatalf("encoded span attributes use %d bytes; maximum is %d", len(encoded), maxSpanAttributesBytes)
	}

	encoded, err = encodeAttributes(values, eventAttributeList)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxNestedAttrsBytes {
		t.Fatalf("encoded nested attributes use %d bytes; maximum is %d", len(encoded), maxNestedAttrsBytes)
	}
}

func TestSafeAttributeValueBoundsAndRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value attribute.Value
		want  bool
	}{
		{name: "bool", value: attribute.BoolValue(true), want: true},
		{name: "integer", value: attribute.Int64Value(12), want: true},
		{name: "finite float", value: attribute.Float64Value(1.25), want: true},
		{name: "nonfinite float", value: attribute.Float64Value(math.NaN())},
		{name: "safe string", value: attribute.StringValue("morde.tool.run"), want: true},
		{name: "freeform string", value: attribute.StringValue("private prompt text")},
		{name: "long string", value: attribute.StringValue(strings.Repeat("a", maxAttributeStringBytes+1))},
		{name: "bool slice", value: attribute.BoolSliceValue([]bool{true, false}), want: true},
		{name: "integer slice", value: attribute.Int64SliceValue([]int64{1, 2}), want: true},
		{name: "float slice", value: attribute.Float64SliceValue([]float64{1, 2}), want: true},
		{name: "nonfinite float slice", value: attribute.Float64SliceValue([]float64{math.Inf(1)})},
		{name: "string slice", value: attribute.StringSliceValue([]string{"tool.run", "trace_id"}), want: true},
		{name: "freeform string slice", value: attribute.StringSliceValue([]string{"tool.run", "user prompt"})},
		{name: "oversized bool slice", value: attribute.BoolSliceValue(make([]bool, 33))},
		{name: "unsupported value", value: attribute.Value{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := safeAttributeValue(test.value)
			if got != test.want {
				t.Fatalf("safeAttributeValue(%s) = %v, want %v", test.name, got, test.want)
			}
		})
	}
}

func TestEncodeLinksAndEventsApplyCountAndPrivacyCaps(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	links := []sdktrace.Link{{}}
	for range maxSpanLinks + 1 {
		links = append(links, sdktrace.Link{SpanContext: spanContext, Attributes: []attribute.KeyValue{
			attribute.String("morde.link.kind", "task-child"),
			attribute.String("morde.prompt", "private text"),
		}})
	}
	encodedLinks, droppedLinks, err := encodeLinks(links)
	if err != nil {
		t.Fatal(err)
	}
	var decodedLinks []spanLink
	if err := json.Unmarshal(encodedLinks, &decodedLinks); err != nil {
		t.Fatal(err)
	}
	if len(decodedLinks) != maxSpanLinks-1 || droppedLinks != 3 {
		t.Fatalf("links count/dropped = %d/%d", len(decodedLinks), droppedLinks)
	}
	if string(decodedLinks[0].Attributes) != `{"morde.link.kind":"task-child"}` {
		t.Fatalf("stored link attributes = %s", decodedLinks[0].Attributes)
	}

	events := make([]sdktrace.Event, 0, maxSpanEvents+2)
	events = append(events, sdktrace.Event{Name: "free form prompt text", Time: time.Now()})
	for range maxSpanEvents + 1 {
		events = append(events, sdktrace.Event{Name: "exception", Time: time.Now(), Attributes: []attribute.KeyValue{
			attribute.String("exception.type", "timeout"),
			attribute.String("exception.message", "private response"),
		}})
	}
	encodedEvents, droppedEvents, err := encodeEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	var decodedEvents []spanEvent
	if err := json.Unmarshal(encodedEvents, &decodedEvents); err != nil {
		t.Fatal(err)
	}
	if len(decodedEvents) != maxSpanEvents-1 || droppedEvents != 3 {
		t.Fatalf("event count/dropped = %d/%d", len(decodedEvents), droppedEvents)
	}
	for _, event := range decodedEvents {
		if strings.Contains(string(event.Attributes), "private") || string(event.Attributes) != `{"exception.type":"timeout"}` {
			t.Fatalf("stored event attributes = %s", event.Attributes)
		}
	}
}

func TestExporterRejectsOversizedBatchesBeforeDatabaseAccess(t *testing.T) {
	database := &fakeSpanDatabase{}
	exporter, err := newPostgresSpanExporter(database, exporterLimits{
		maxBatch: 1, maxSpanBytes: maxSpanPayloadBytes, maxBatchBytes: maxBatchPayloadBytes, timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	span := endedTestSpan(t, nil)
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span, span}); err == nil || !strings.Contains(err.Error(), "maximum is 1") {
		t.Fatalf("oversized batch error = %v", err)
	}

	exporter.maxSpanBytes = 1
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span}); err == nil || !strings.Contains(err.Error(), "payload limit") {
		t.Fatalf("oversized span error = %v", err)
	}
	if database.tx != nil {
		t.Fatal("oversized span reached the database")
	}

	record, err := makeSpanRecord(span)
	if err != nil {
		t.Fatal(err)
	}
	exporter.maxSpanBytes = record.payloadBytes
	exporter.maxBatch = 2
	exporter.maxBatchBytes = record.payloadBytes*2 - 1
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span, span}); err == nil || !strings.Contains(err.Error(), "span batch exceeds") {
		t.Fatalf("oversized total payload error = %v", err)
	}
	if database.tx != nil {
		t.Fatal("oversized payload batch reached the database")
	}
}

func TestExporterRejectsCanceledContextBeforeDatabaseAccess(t *testing.T) {
	database := &fakeSpanDatabase{}
	exporter, err := newPostgresSpanExporter(database, defaultExporterLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{endedTestSpan(t, nil)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export error = %v", err)
	}
	if database.tx != nil || exporter.Health().ReasonCode != "export_cancelled" {
		t.Fatalf("canceled export reached the database or lost health state: %+v", exporter.Health())
	}
	if _, err := exporter.PurgeExpiredSpans(ctx, time.Now(), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled purge error = %v", err)
	}
}

func TestExporterConstructorsValidateDependencies(t *testing.T) {
	if _, err := NewPostgresSpanExporterWithLogger(nil, zap.NewNop()); err == nil {
		t.Fatal("nil pool was accepted")
	}
	pool := newUnconnectedPool(t)
	if _, err := NewPostgresSpanExporterWithLogger(pool, nil); err == nil {
		t.Fatal("nil logger was accepted")
	}
	if _, err := newPostgresSpanExporter(&fakeSpanDatabase{}, exporterLimits{}); err == nil {
		t.Fatal("invalid limits were accepted")
	}
	if _, err := newPostgresSpanExporter(&fakeSpanDatabase{}, defaultExporterLimits(), nil); err == nil {
		t.Fatal("nil injected logger was accepted")
	}
	if _, err := newPostgresSpanExporter(&fakeSpanDatabase{}, defaultExporterLimits(), zap.NewNop(), zap.NewNop()); err == nil {
		t.Fatal("multiple injected loggers were accepted")
	}
}

func TestExporterWritesBatchAtomicallyAndHandlesDatabaseFailures(t *testing.T) {
	span := endedTestSpan(t, nil)
	database := &fakeSpanDatabase{}
	exporter, err := newPostgresSpanExporter(database, defaultExporterLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span, span}); err != nil {
		t.Fatal(err)
	}
	if database.tx == nil || database.tx.batchSize != 2 || !database.tx.committed || !database.tx.results.closed || !database.tx.rolledBack {
		t.Fatalf("batch transaction = %+v", database.tx)
	}
	if got := exporter.Health(); got.Status != "healthy" || got.LastSuccessAt == nil {
		t.Fatalf("successful export health = %+v", got)
	}

	tests := []struct {
		name   string
		db     *fakeSpanDatabase
		reason string
	}{
		{name: "begin deadline", db: &fakeSpanDatabase{beginErr: context.DeadlineExceeded}, reason: "export_timeout"},
		{name: "batch execution", db: &fakeSpanDatabase{tx: &fakeSpanTx{results: &fakeBatchResults{execErr: errors.New("insert failed")}}}, reason: "span_export_failed"},
		{name: "batch close", db: &fakeSpanDatabase{tx: &fakeSpanTx{results: &fakeBatchResults{closeErr: errors.New("close failed")}}}, reason: "span_export_failed"},
		{name: "commit", db: &fakeSpanDatabase{tx: &fakeSpanTx{commitErr: errors.New("commit failed"), results: &fakeBatchResults{}}}, reason: "span_export_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exporter, err := newPostgresSpanExporter(test.db, defaultExporterLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span}); err == nil {
				t.Fatal("expected export failure")
			}
			if got := exporter.Health(); got.FailedBatches != 1 || got.ReasonCode != test.reason {
				t.Fatalf("failed export health = %+v", got)
			}
			if test.db.tx != nil && !test.db.tx.rolledBack {
				t.Fatal("failed export did not roll back its transaction")
			}
		})
	}
}

func TestPurgeExpiredSpansUsesBoundedBatch(t *testing.T) {
	database := &fakeSpanDatabase{tx: &fakeSpanTx{execRows: []int64{3, 2, 1}}}
	exporter, err := newPostgresSpanExporter(database, defaultExporterLimits())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("test", 3600))
	deleted, err := exporter.PurgeExpiredSpans(context.Background(), cutoff, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 6 || len(database.tx.execQueries) != 3 || len(database.tx.execArgs) != 3 {
		t.Fatalf("purge result/query/args = %d, %#v, %#v", deleted, database.tx.execQueries, database.tx.execArgs)
	}
	if database.tx.execQueries[0] != pruneSpansSQL || database.tx.execQueries[1] != pruneTraceLinksSQL ||
		database.tx.execQueries[2] != pruneTaskTraceLinksSQL || !database.tx.committed || !database.tx.rolledBack {
		t.Fatalf("purge did not atomically remove spans and trace links: %+v", database.tx)
	}
	for _, args := range database.tx.execArgs {
		if got, ok := args[0].(time.Time); !ok || !got.Equal(cutoff.UTC()) {
			t.Fatalf("purge cutoff = %#v", args[0])
		}
		if limit, ok := args[1].(int32); !ok || limit != 10000 {
			t.Fatalf("purge limit = %#v", args[1])
		}
	}
	if !strings.Contains(pruneSpansSQL, "SKIP LOCKED") || !strings.Contains(pruneTraceLinksSQL, "SKIP LOCKED") ||
		!strings.Contains(pruneTaskTraceLinksSQL, "SKIP LOCKED") {
		t.Fatal("purge queries do not allow concurrent bounded cleanup")
	}

	if _, err := exporter.PurgeExpiredSpans(context.Background(), time.Time{}, 1); err == nil {
		t.Fatal("zero cutoff was accepted")
	}
	if _, err := exporter.PurgeExpiredSpans(context.Background(), cutoff, 10001); err == nil {
		t.Fatal("oversized purge limit was accepted")
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.PurgeExpiredSpans(context.Background(), cutoff, 1); !errors.Is(err, errSpanExporterClosed) {
		t.Fatalf("purge after shutdown error = %v", err)
	}
	if _, err := exporter.PurgeExpiredSpans(context.Background(), cutoff, 1); !errors.Is(err, errSpanExporterClosed) {
		t.Fatalf("purge after shutdown error = %v", err)
	}

	database = &fakeSpanDatabase{tx: &fakeSpanTx{execErrors: map[int]error{1: context.DeadlineExceeded}}}
	exporter, err = newPostgresSpanExporter(database, defaultExporterLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.PurgeExpiredSpans(context.Background(), cutoff, 10); !errors.Is(err, context.DeadlineExceeded) || !database.tx.rolledBack {
		t.Fatalf("purge database error = %v", err)
	}
}

func TestExporterShutdownDoesNotOwnPoolAndIsIdempotent(t *testing.T) {
	pool := newUnconnectedPool(t)
	exporter, err := NewPostgresSpanExporter(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatalf("exporter opened or closed a shared pool connection: total=%d", pool.Stat().TotalConns())
	}
	if err := exporter.ExportSpans(context.Background(), nil); err == nil || !errors.Is(err, errSpanExporterClosed) {
		t.Fatalf("export after shutdown error = %v", err)
	}
}

func TestExporterShutdownWaitsForInFlightOperations(t *testing.T) {
	exporter, err := newPostgresSpanExporter(&fakeSpanDatabase{}, defaultExporterLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.beginOperation(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := exporter.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with operation in flight = %v", err)
	}
	if err := exporter.beginOperation(); !errors.Is(err, errSpanExporterClosed) {
		t.Fatalf("operation started during shutdown = %v", err)
	}
	exporter.endOperation()
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after operation drained: %v", err)
	}
}

func TestExporterHealthSnapshotIsSafeAndTracksRecovery(t *testing.T) {
	var nilExporter *PostgresSpanExporter
	if health := nilExporter.Health(); health.Status != "unavailable" || health.ReasonCode != "exporter_unavailable" {
		t.Fatalf("nil exporter health = %+v", health)
	}
	if err := nilExporter.Shutdown(context.Background()); err == nil {
		t.Fatal("nil exporter shutdown was accepted")
	}
	if err := nilExporter.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("nil exporter accepted a batch")
	}
	if _, err := nilExporter.PurgeExpiredSpans(context.Background(), time.Now(), 1); err == nil {
		t.Fatal("nil exporter accepted a retention request")
	}

	exporter, err := NewPostgresSpanExporter(newUnconnectedPool(t))
	if err != nil {
		t.Fatal(err)
	}

	exporter.recordFailure(errors.New("database SQL diagnostic"))
	health := exporter.Health()
	if health.Status != "unavailable" || health.ReasonCode != "span_export_failed" || health.FailedBatches != 1 || health.InternalError != "database SQL diagnostic" {
		t.Fatalf("failure health = %+v", health)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "database SQL diagnostic") {
		t.Fatalf("internal diagnostic leaked into JSON: %s", encoded)
	}

	if health.LastFailureAt == nil {
		t.Fatal("failure timestamp was not set")
	}
	originalFailureTime := *health.LastFailureAt
	*health.LastFailureAt = time.Time{}
	if got := exporter.Health().LastFailureAt; got == nil || !got.Equal(originalFailureTime) {
		t.Fatal("health snapshot exposed mutable internal timestamp storage")
	}

	exporter.recordSuccess()
	if got := exporter.Health(); got.Status != "healthy" || got.ReasonCode != "" || got.FailedBatches != 1 {
		t.Fatalf("recovered health = %+v", got)
	}
	exporter.recordFailure(errors.New("second failure"))
	if got := exporter.Health(); got.Status != "partial" || got.FailedBatches != 2 {
		t.Fatalf("partial health = %+v", got)
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exporter.Health(); got.Status != "unavailable" || !got.Shutdown {
		t.Fatalf("shutdown health = %+v", got)
	}
}

func TestSpanInsertIgnoresDuplicateSpanIdentity(t *testing.T) {
	if !strings.Contains(insertSpanSQL, "ON CONFLICT (trace_id,span_id) DO NOTHING") {
		t.Fatalf("span insert lacks idempotent key handling: %s", insertSpanSQL)
	}
}

func TestPostgresSpanExporterPersistsIdempotentlyAndPurgesExpiredSpans(t *testing.T) {
	pool := newTemporaryTracingPool(t)
	exporter, err := NewPostgresSpanExporter(pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exporter.Shutdown(context.Background()) })

	oldSpan := endedTestSpanAt(t, time.Now().UTC().Add(-8*24*time.Hour), nil)
	newSpan := endedTestSpan(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{oldSpan, newSpan}); err != nil {
		t.Fatalf("export two span batch: %v", err)
	}

	const duplicateExporters = 8
	var workers sync.WaitGroup
	workerErrors := make(chan error, duplicateExporters)
	for range duplicateExporters {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{oldSpan}); err != nil {
				workerErrors <- err
			}
		}()
	}
	workers.Wait()
	close(workerErrors)
	for err := range workerErrors {
		t.Errorf("concurrent duplicate export: %v", err)
	}

	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM observability_spans").Scan(&stored); err != nil {
		t.Fatal("query stored span count")
	}
	if stored != 2 {
		t.Fatalf("stored span count = %d, want 2 after concurrent duplicate exports", stored)
	}

	deleted, err := exporter.PurgeExpiredSpans(ctx, time.Now().UTC().Add(-7*24*time.Hour), 10000)
	if err != nil {
		t.Fatalf("purge old spans: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purge deleted %d spans; want 1", deleted)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM observability_spans").Scan(&stored); err != nil {
		t.Fatal("query stored span count after purge")
	}
	if stored != 1 {
		t.Fatalf("stored span count after purge = %d, want 1", stored)
	}
}

func endedTestSpan(t *testing.T, mutate func(trace.Span)) sdktrace.ReadOnlySpan {
	t.Helper()
	return endedTestSpanAt(t, time.Now().UTC(), mutate)
}

func endedTestSpanAt(t *testing.T, start time.Time, mutate func(trace.Span)) sdktrace.ReadOnlySpan {
	t.Helper()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	ctx, span := provider.Tracer("morde/test").Start(context.Background(), "GET /api/tasks/:id",
		trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(start))
	_ = ctx
	if mutate != nil {
		mutate(span)
	}
	span.End(trace.WithTimestamp(start.Add(time.Millisecond)))
	readOnly, ok := span.(sdktrace.ReadOnlySpan)
	if !ok {
		t.Fatal("recording span does not implement sdktrace.ReadOnlySpan")
	}
	_ = provider.Shutdown(context.Background())
	return readOnly
}

func defaultExporterLimits() exporterLimits {
	return exporterLimits{maxBatch: maxExportBatchSpans, maxSpanBytes: maxSpanPayloadBytes,
		maxBatchBytes: maxBatchPayloadBytes, timeout: defaultExportTimeout}
}

type fakeSpanDatabase struct {
	beginErr error
	tx       *fakeSpanTx
}

func (database *fakeSpanDatabase) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	if database.beginErr != nil {
		return nil, database.beginErr
	}
	if database.tx == nil {
		database.tx = &fakeSpanTx{results: &fakeBatchResults{}}
	}
	if database.tx.results == nil {
		database.tx.results = &fakeBatchResults{}
	}
	return database.tx, nil
}

type fakeSpanTx struct {
	results     *fakeBatchResults
	batchSize   int
	execRows    []int64
	execErrors  map[int]error
	execCalls   int
	execQueries []string
	execArgs    [][]any
	commitErr   error
	committed   bool
	rolledBack  bool
}

func (tx *fakeSpanTx) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected nested transaction")
}
func (tx *fakeSpanTx) Commit(context.Context) error {
	tx.committed = true
	return tx.commitErr
}
func (tx *fakeSpanTx) Rollback(context.Context) error {
	tx.rolledBack = true
	if tx.committed {
		return pgx.ErrTxClosed
	}
	return nil
}
func (*fakeSpanTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("unexpected copy")
}
func (tx *fakeSpanTx) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	tx.batchSize = batch.Len()
	return tx.results
}
func (*fakeSpanTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }
func (*fakeSpanTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("unexpected prepare")
}
func (tx *fakeSpanTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	call := tx.execCalls
	tx.execCalls++
	tx.execQueries = append(tx.execQueries, query)
	tx.execArgs = append(tx.execArgs, append([]any(nil), args...))
	if err := tx.execErrors[call]; err != nil {
		return pgconn.CommandTag{}, err
	}
	rows := int64(0)
	if call < len(tx.execRows) {
		rows = tx.execRows[call]
	}
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(rows, 10)), nil
}
func (*fakeSpanTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (*fakeSpanTx) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (*fakeSpanTx) Conn() *pgx.Conn                                  { return nil }

type fakeBatchResults struct {
	execErr  error
	closeErr error
	closed   bool
}

func (results *fakeBatchResults) Exec() (pgconn.CommandTag, error) {
	if results.execErr != nil {
		return pgconn.CommandTag{}, results.execErr
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (*fakeBatchResults) Query() (pgx.Rows, error) { return nil, errors.New("unexpected query") }
func (*fakeBatchResults) QueryRow() pgx.Row        { return nil }
func (results *fakeBatchResults) Close() error {
	results.closed = true
	return results.closeErr
}

func newTemporaryTracingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; PostgreSQL span exporter integration was not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse CH_TEST_DATABASE_URL")
	}
	host := config.ConnConfig.Host
	if host != "" && host != "localhost" && !filepath.IsAbs(host) {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			t.Skip("CH_TEST_DATABASE_URL is not a local PostgreSQL server; integration test was not run")
		}
	}

	adminPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("connect to local PostgreSQL test server")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		adminPool.Close()
		t.Fatal("generate temporary database name")
	}
	databaseName := fmt.Sprintf("morde_trace_test_%x", suffix)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE \""+databaseName+"\""); err != nil {
		adminPool.Close()
		t.Fatal("create temporary tracing test database")
	}

	testConfig := config.Copy()
	testConfig.ConnConfig.Database = databaseName
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE \""+databaseName+"\" WITH (FORCE)")
		adminPool.Close()
		t.Fatal("connect to temporary tracing test database")
	}
	if err := migrateTemporaryTracingDatabase(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE \""+databaseName+"\" WITH (FORCE)")
		adminPool.Close()
		t.Fatal("migrate temporary tracing test database")
	}
	t.Cleanup(func() {
		testPool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP DATABASE \""+databaseName+"\" WITH (FORCE)"); err != nil {
			t.Error("drop temporary tracing test database")
		}
		adminPool.Close()
	})
	return testPool
}

func migrateTemporaryTracingDatabase(ctx context.Context, pool *pgxpool.Pool) (retErr error) {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithLogger(goose.NopLogger()), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, provider.Close()) }()
	_, err = provider.Up(ctx)
	return err
}

func newUnconnectedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://localhost:1/morde?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
