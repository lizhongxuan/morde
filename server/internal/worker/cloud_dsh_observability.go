package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const cloudDSHObservabilityLookupTimeout = 100 * time.Millisecond
const cloudDSHObservabilityWriteTimeout = 500 * time.Millisecond
const cloudDSHObservabilityLookupAttempts = 3
const cloudDSHObservabilityLookupRetryDelay = 20 * time.Millisecond

func (e *CloudDSHExecutor) tracerOrNoop() trace.Tracer {
	if e != nil && e.tracer != nil {
		return e.tracer
	}
	return noop.NewTracerProvider().Tracer("morde/server/internal/worker")
}

func cloudDSHOriginSpanContext(value app.TaskObservabilityContext) (trace.SpanContext, bool) {
	traceID, err := trace.TraceIDFromHex(value.OriginTraceID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	spanID, err := trace.SpanIDFromHex(value.OriginSpanID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	return spanContext, spanContext.IsValid()
}

func (e *CloudDSHExecutor) startTaskAttemptTrace(ctx context.Context, value task.Task) (context.Context, trace.Span) {
	var origin app.TaskObservabilityContext
	var originSpanContext trace.SpanContext
	originFound := false
	if e.options.ObservabilityContext != nil {
		for attempt := 0; attempt < cloudDSHObservabilityLookupAttempts; attempt++ {
			lookupCtx, cancel := context.WithTimeout(ctx, cloudDSHObservabilityLookupTimeout)
			resolved, found, err := e.options.ObservabilityContext.GetTaskObservabilityContext(lookupCtx, value.ID)
			cancel()
			if err != nil {
				e.options.Logger.Warn("task origin trace context is unavailable", zap.String("task_id", value.ID),
					zap.String("reason", "context_lookup_failed"))
				break
			}
			if found {
				if spanContext, ok := cloudDSHOriginSpanContext(resolved); ok && resolved.RequestID != "" && resolved.MessageID != "" {
					origin, originSpanContext, originFound = resolved, spanContext, true
				} else {
					e.options.Logger.Warn("task origin trace context is invalid", zap.String("task_id", value.ID),
						zap.String("reason", "context_validation_failed"))
				}
				break
			}
			if attempt+1 < cloudDSHObservabilityLookupAttempts {
				timer := time.NewTimer(cloudDSHObservabilityLookupRetryDelay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					attempt = cloudDSHObservabilityLookupAttempts
				case <-timer.C:
				}
			}
		}
	}

	attributes := []attribute.KeyValue{
		attribute.String("morde.task_id", value.ID),
		attribute.String("morde.user_id", value.OwnerID),
		attribute.String("morde.conversation_id", value.ConversationID),
		attribute.String("morde.model_id", value.ModelID),
		attribute.Int64("morde.attempt", value.Lease.Version),
		attribute.String("morde.operation", "task_attempt"),
		attribute.String("morde.source", "dsh"),
	}
	if originFound {
		attributes = append(attributes, attribute.String("morde.request_id", origin.RequestID),
			attribute.String("morde.message_id", origin.MessageID))
	}
	options := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithAttributes(attributes...)}
	if originFound {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: originSpanContext,
			Attributes: []attribute.KeyValue{attribute.String("morde.link.kind", "task_input")}}))
	}
	attemptCtx, span := e.tracerOrNoop().Start(ctx, "morde.task.attempt", options...)
	if !originFound {
		if e.options.ObservabilityContext != nil {
			span.AddEvent("task_origin_trace_missing", trace.WithAttributes(attribute.String("morde.status", "partial")))
			e.options.Logger.Warn("task origin trace context is missing", zap.String("task_id", value.ID),
				zap.String("reason", "trace_link_not_available"))
		}
		return attemptCtx, span
	}
	if e.options.AttemptTraceLinker == nil || !span.SpanContext().IsValid() {
		span.AddEvent("task_attempt_trace_link_persistence_unavailable", trace.WithAttributes(attribute.String("morde.status", "unavailable")))
		return attemptCtx, span
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(attemptCtx), cloudDSHObservabilityWriteTimeout)
	err := e.options.AttemptTraceLinker.LinkTaskAttemptTrace(writeCtx, value.ID, value.Lease.Version, origin,
		span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String())
	cancel()
	if err != nil {
		span.AddEvent("task_attempt_trace_link_persistence_failed", trace.WithAttributes(attribute.String("morde.status", "failed")))
		e.options.Logger.Warn("task attempt trace link could not be persisted", zap.String("task_id", value.ID),
			zap.Int64("attempt", value.Lease.Version), zap.String("reason", "trace_link_write_failed"))
	}
	return attemptCtx, span
}

func (e *CloudDSHExecutor) startSessionTrace(parent context.Context, value task.Task, state *cloudDSHSessionState) {
	if state == nil {
		return
	}
	attributes := []attribute.KeyValue{
		attribute.String("morde.task_id", value.ID),
		attribute.String("morde.user_id", value.OwnerID),
		attribute.String("morde.conversation_id", value.ConversationID),
		attribute.String("morde.agent_session_id", state.identity.SessionID),
		attribute.Int64("morde.attempt", value.Lease.Version),
		attribute.String("morde.operation", "agent_session"),
		attribute.String("morde.source", "dsh"),
	}
	if state.parentSessionID != "" {
		attributes = append(attributes, attribute.String("morde.parent_agent_session_id", state.parentSessionID),
			attribute.String("morde.phase", "subagent"))
	}
	state.traceCtx, state.traceSpan = e.tracerOrNoop().Start(parent, "morde.agent.session", trace.WithAttributes(attributes...))
}

func (e *CloudDSHExecutor) startModelCallTrace(parent context.Context, value task.Task, state *cloudDSHSessionState,
	modelCall app.ModelCall) (context.Context, trace.Span) {
	base := parent
	var attributes []attribute.KeyValue
	attributes = append(attributes, attribute.String("morde.task_id", value.ID),
		attribute.String("morde.user_id", value.OwnerID), attribute.String("morde.conversation_id", value.ConversationID),
		attribute.String("morde.model_id", value.ModelID), attribute.String("morde.call_id", modelCall.CallID),
		attribute.Int64("morde.attempt", value.Lease.Version), attribute.String("morde.operation", "model_call"),
		attribute.String("morde.phase", "model"), attribute.String("morde.source", "dsh"))
	if state != nil {
		if state.traceSpan != nil {
			base = trace.ContextWithSpan(parent, state.traceSpan)
		}
		attributes = append(attributes, attribute.String("morde.agent_session_id", state.identity.SessionID))
		if modelCall.Task != nil && modelCall.Task.ThroughSequence >= 0 {
			attributes = append(attributes, attribute.Int64("morde.event_sequence", modelCall.Task.ThroughSequence))
		}
	}
	return e.tracerOrNoop().Start(base, "morde.model.call", trace.WithAttributes(attributes...))
}

func (e *CloudDSHExecutor) startToolCallTrace(parent context.Context, value task.Task, state *cloudDSHSessionState,
	toolName, callID string, eventSequence int64) (context.Context, trace.Span) {
	base := parent
	attributes := []attribute.KeyValue{
		attribute.String("morde.task_id", value.ID), attribute.String("morde.user_id", value.OwnerID),
		attribute.String("morde.conversation_id", value.ConversationID), attribute.String("morde.tool_name", toolName),
		attribute.String("morde.call_id", callID), attribute.Int64("morde.attempt", value.Lease.Version),
		attribute.String("morde.operation", "tool_call"), attribute.String("morde.phase", "tool"),
		attribute.String("morde.source", "dsh"),
	}
	if state != nil {
		if state.traceSpan != nil {
			base = trace.ContextWithSpan(parent, state.traceSpan)
		}
		attributes = append(attributes, attribute.String("morde.agent_session_id", state.identity.SessionID))
	}
	if eventSequence >= 0 {
		attributes = append(attributes, attribute.Int64("morde.event_sequence", eventSequence))
	}
	return e.tracerOrNoop().Start(base, "morde.tool.call", trace.WithAttributes(attributes...))
}

func cloudDSHEndSpan(span trace.Span, status string, cause error) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.String("morde.status", status))
	if cause != nil {
		span.SetStatus(codes.Error, status)
		span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", fmt.Sprintf("%T", cause))))
	} else if status == "failed" {
		span.SetStatus(codes.Error, status)
	} else {
		span.SetStatus(codes.Ok, status)
	}
	span.End()
}

// cloudDSHEndSpanOnPanic belongs in a small scoped function next to a span's
// operation. It closes the span without logging panic contents, then preserves
// the existing panic behavior.
func cloudDSHEndSpanOnPanic(span trace.Span) {
	if recovered := recover(); recovered != nil {
		cloudDSHEndSpan(span, "failed", errors.New("instrumented dsh operation panicked"))
		panic(recovered)
	}
}

func cloudDSHAttemptSpanStatus(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, ErrCloudDSHWaitingForUser):
		return "waiting_user"
	default:
		return "failed"
	}
}

func cloudDSHTraceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
