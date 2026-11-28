package worker

import (
	"context"
	"regexp"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHTraceContextReaderStub struct {
	value app.TaskObservabilityContext
	found bool
	err   error
	calls int
}

func (s *cloudDSHTraceContextReaderStub) GetTaskObservabilityContext(context.Context, string) (app.TaskObservabilityContext, bool, error) {
	s.calls++
	return s.value, s.found, s.err
}

type cloudDSHAttemptTraceLinkerStub struct {
	taskID  string
	attempt int64
	origin  app.TaskObservabilityContext
	traceID string
	spanID  string
	calls   int
}

func (s *cloudDSHAttemptTraceLinkerStub) LinkTaskAttemptTrace(_ context.Context, taskID string, attempt int64,
	origin app.TaskObservabilityContext, traceID, spanID string) error {
	s.taskID, s.attempt, s.origin, s.traceID, s.spanID = taskID, attempt, origin, traceID, spanID
	s.calls++
	return nil
}

func TestCloudDSHAttemptAndSessionTraceLinkage(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	originContext, originSpan := provider.Tracer("test/http").Start(context.Background(), "http.request")
	origin := app.TaskObservabilityContext{OriginTraceID: originSpan.SpanContext().TraceID().String(),
		OriginSpanID: originSpan.SpanContext().SpanID().String(), RequestID: "request_123", MessageID: "m1234567"}
	reader := &cloudDSHTraceContextReaderStub{value: origin, found: true}
	linker := &cloudDSHAttemptTraceLinkerStub{}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Logger: zap.NewNop(),
		ObservabilityContext: reader, AttemptTraceLinker: linker}, tracer: provider.Tracer("morde/worker-test")}
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", ConversationID: "c1234567", ModelID: "p1234567",
		Lease: task.Lease{Version: 9}}

	attemptCtx, attemptSpan := executor.startTaskAttemptTrace(originContext, value)
	if !attemptSpan.SpanContext().IsValid() || attemptSpan.SpanContext().TraceID() == originSpan.SpanContext().TraceID() {
		t.Fatal("task attempt did not start a new trace root")
	}
	state := newCloudDSHSessionState(app.AgentInstanceFence{}, cloudDSHIdentity{TaskID: value.ID, OwnerID: value.OwnerID,
		SessionID: "session_root"}, "", "", nil, false, false, -1, nil)
	executor.startSessionTrace(attemptCtx, value, state)
	if !state.traceSpan.SpanContext().IsValid() || state.traceSpan.SpanContext().TraceID() != attemptSpan.SpanContext().TraceID() {
		t.Fatal("root session span was not created under the task attempt")
	}
	traceparent := cloudDSHTraceparent(trace.ContextWithSpan(attemptCtx, state.traceSpan))
	if !regexp.MustCompile(`^00-[a-f0-9]{32}-[a-f0-9]{16}-[a-f0-9]{2}$`).MatchString(traceparent) {
		t.Fatalf("runtime traceparent = %q", traceparent)
	}
	modelCtx, modelSpan := executor.startModelCallTrace(attemptCtx, value, state, app.ModelCall{CallID: "model_call_1",
		Task: &app.ModelTaskFence{ThroughSequence: 4}})
	_, toolSpan := executor.startToolCallTrace(modelCtx, value, state, "file_read", "tool_call_1", 5)
	cloudDSHEndSpan(toolSpan, "succeeded", nil)
	cloudDSHEndSpan(modelSpan, "completed", nil)
	cloudDSHEndSpan(state.traceSpan, "completed", nil)
	cloudDSHEndSpan(attemptSpan, "completed", nil)
	originSpan.End()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := exporter.GetSpans()
	byName := make(map[string]tracetest.SpanStub, len(spans))
	for _, span := range spans {
		byName[span.Name] = span
	}
	attempt, ok := byName["morde.task.attempt"]
	if !ok || attempt.Parent.IsValid() || len(attempt.Links) != 1 ||
		attempt.Links[0].SpanContext.TraceID() != originSpan.SpanContext().TraceID() ||
		attempt.Links[0].SpanContext.SpanID() != originSpan.SpanContext().SpanID() {
		t.Fatalf("attempt span does not link to origin independently of its parent: %#v", attempt)
	}
	if session, ok := byName["morde.agent.session"]; !ok || session.Parent.SpanID() != attempt.SpanContext.SpanID() {
		t.Fatalf("session span is not a child of the attempt span: %#v", session)
	}
	if model, ok := byName["morde.model.call"]; !ok || model.Parent.SpanID() != state.traceSpan.SpanContext().SpanID() {
		t.Fatalf("model span is not associated with the DSH session: %#v", model)
	}
	if tool, ok := byName["morde.tool.call"]; !ok || tool.Parent.SpanID() != state.traceSpan.SpanContext().SpanID() {
		t.Fatalf("tool span is not associated with the DSH session: %#v", tool)
	}
	if linker.calls != 1 || linker.taskID != value.ID || linker.attempt != value.Lease.Version ||
		linker.traceID != attempt.SpanContext.TraceID().String() || linker.spanID != attempt.SpanContext.SpanID().String() ||
		linker.origin != origin {
		t.Fatalf("attempt linker call = %#v", linker)
	}
}

func TestCloudDSHMissingOriginTraceRetriesAndUsesNoopWhenNoProvider(t *testing.T) {
	reader := &cloudDSHTraceContextReaderStub{}
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSyncer(exporter))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Logger: zap.NewNop(), ObservabilityContext: reader},
		tracer: provider.Tracer("morde/worker-test")}
	ctx, span := executor.startTaskAttemptTrace(context.Background(), task.Task{ID: "q1234567", OwnerID: "u1234567",
		ConversationID: "c1234567", ModelID: "p1234567", Lease: task.Lease{Version: 2}})
	if reader.calls != cloudDSHObservabilityLookupAttempts {
		t.Fatalf("origin lookup attempts = %d, want %d", reader.calls, cloudDSHObservabilityLookupAttempts)
	}
	if got := cloudDSHTraceparent(ctx); !regexp.MustCompile(`^00-[a-f0-9]{32}-[a-f0-9]{16}-[a-f0-9]{2}$`).MatchString(got) {
		t.Fatalf("attempt context traceparent is invalid: %q", got)
	}
	cloudDSHEndSpan(span, "completed", nil)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || len(spans[0].Links) != 0 || len(spans[0].Events) != 1 ||
		spans[0].Events[0].Name != "task_origin_trace_missing" {
		t.Fatalf("missing source coverage was not recorded accurately: %#v", spans)
	}

	noProviderExecutor, _, _, _, _, _ := cloudDSHFixture(t)
	_, noProviderSpan := noProviderExecutor.tracer.Start(context.Background(), "no-provider")
	if noProviderSpan.SpanContext().IsValid() {
		t.Fatal("nil TracerProvider unexpectedly used a global provider")
	}
	if got := cloudDSHTraceparent(trace.ContextWithSpan(context.Background(), noProviderSpan)); got != "" {
		t.Fatalf("no-op provider propagated traceparent %q", got)
	}
	noProviderSpan.End()
}
