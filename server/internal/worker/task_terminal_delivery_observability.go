package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"morde/server/internal/app"
)

const taskTerminalDeliveryObservabilityLookupTimeout = 100 * time.Millisecond

type taskTerminalDeliveryLogScope struct {
	ctx    context.Context
	span   trace.Span
	logger *zap.Logger
}

func (w *TaskTerminalDeliveryWorker) startPollLogScope(ctx context.Context) taskTerminalDeliveryLogScope {
	traceCtx, span := w.tracerProvider().Tracer("morde/server/internal/worker").Start(ctx,
		"morde.task.terminal_delivery.poll", trace.WithAttributes(attribute.String("morde.operation", "terminal_delivery_poll")))
	traceID := span.SpanContext().TraceID().String()
	spanID := span.SpanContext().SpanID().String()
	if !span.SpanContext().IsValid() {
		traceID = fallbackTerminalDeliveryTraceID(w.options.WorkerID, strconv.FormatInt(time.Now().UnixNano(), 10))
		spanID = ""
	}
	fields := []zap.Field{zap.String("trace_id", traceID), zap.String("operation", "task_terminal_delivery_poll")}
	if spanID != "" {
		fields = append(fields, zap.String("span_id", spanID))
	}
	return taskTerminalDeliveryLogScope{ctx: traceCtx, span: span, logger: w.options.Logger.With(fields...)}
}

func (w *TaskTerminalDeliveryWorker) startDeliveryLogScope(ctx context.Context,
	claim app.TaskTerminalDeliveryClaim, operation string) taskTerminalDeliveryLogScope {
	requestID := "unavailable"
	// This stable response correlation ID survives retries; the final assistant message ID is logged separately.
	responseID := "terminal_response_" + strconv.FormatInt(claim.DeliveryID, 10)
	parentCtx := ctx
	originStatus := "missing"
	var lookupErr error
	if w.options.ObservabilityContext != nil && claim.TaskID != "" {
		lookupCtx, cancel := context.WithTimeout(ctx, taskTerminalDeliveryObservabilityLookupTimeout)
		origin, found, err := w.options.ObservabilityContext.GetTaskObservabilityContext(lookupCtx, claim.TaskID)
		cancel()
		lookupErr = err
		if err != nil {
			originStatus = "lookup_failed"
		} else if found {
			if spanContext, ok := cloudDSHOriginSpanContext(origin); ok && origin.RequestID != "" && origin.MessageID != "" {
				parentCtx = trace.ContextWithRemoteSpanContext(ctx, spanContext)
				requestID = origin.RequestID
				originStatus = "linked"
			} else {
				originStatus = "invalid"
			}
		}
	} else if w.options.ObservabilityContext == nil {
		originStatus = "reader_unavailable"
	}

	attributes := []attribute.KeyValue{
		attribute.String("morde.task_id", claim.TaskID),
		attribute.Int64("morde.delivery_id", claim.DeliveryID),
		attribute.String("morde.response_id", responseID),
		attribute.String("morde.model_call_id", claim.ModelCallID),
		attribute.String("morde.operation", "terminal_delivery_"+operation),
		attribute.String("morde.source", claim.SourceKind),
	}
	if requestID != "unavailable" {
		attributes = append(attributes, attribute.String("morde.request_id", requestID))
	}
	traceCtx, span := w.tracerProvider().Tracer("morde/server/internal/worker").Start(parentCtx,
		"morde.task.terminal_delivery", trace.WithAttributes(attributes...))
	spanContext := span.SpanContext()
	traceID := spanContext.TraceID().String()
	spanID := spanContext.SpanID().String()
	if !spanContext.IsValid() {
		if parentSpanContext := trace.SpanContextFromContext(parentCtx); parentSpanContext.IsValid() {
			traceID = parentSpanContext.TraceID().String()
		} else {
			traceID = fallbackTerminalDeliveryTraceID(claim.TaskID, responseID)
		}
		spanID = ""
	}
	fields := []zap.Field{
		zap.String("trace_id", traceID),
		zap.String("request_id", requestID),
		zap.String("response_id", responseID),
		zap.String("task_id", claim.TaskID),
		zap.Int64("delivery_id", claim.DeliveryID),
		zap.Int64("claim_generation", claim.ClaimGeneration),
		zap.String("model_call_id", claim.ModelCallID),
		zap.String("source_kind", claim.SourceKind),
		zap.String("operation", "task_terminal_delivery_"+operation),
	}
	if spanID != "" {
		fields = append(fields, zap.String("span_id", spanID))
	}
	logger := w.options.Logger.With(fields...)
	if originStatus != "linked" {
		warningFields := []zap.Field{zap.String("stage", "origin_trace_context"), zap.String("status", originStatus)}
		if lookupErr != nil {
			warningFields = append(warningFields, zap.String("error_type", typeName(lookupErr)))
		}
		logger.Warn("task terminal delivery origin trace context is unavailable", warningFields...)
		span.AddEvent("task_terminal_delivery_origin_trace_unavailable", trace.WithAttributes(
			attribute.String("morde.status", originStatus)))
	}
	return taskTerminalDeliveryLogScope{ctx: traceCtx, span: span, logger: logger}
}

func (s taskTerminalDeliveryLogScope) finish(err error) {
	if s.span == nil {
		return
	}
	if err != nil {
		s.span.SetStatus(codes.Error, "terminal delivery operation failed")
	} else {
		s.span.SetStatus(codes.Ok, "")
	}
	s.span.End()
}

func (w *TaskTerminalDeliveryWorker) tracerProvider() trace.TracerProvider {
	if w.options.TracerProvider != nil {
		return w.options.TracerProvider
	}
	return noop.NewTracerProvider()
}

func fallbackTerminalDeliveryTraceID(values ...string) string {
	seed := "morde-task-terminal-delivery"
	for _, value := range values {
		seed += ":" + value
	}
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:16])
}

func typeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
