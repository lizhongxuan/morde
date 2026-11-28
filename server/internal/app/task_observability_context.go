package app

import "context"

// TaskObservabilityContext identifies the accepted user request that started a
// task. It contains correlation identifiers only, never message content.
type TaskObservabilityContext struct {
	OriginTraceID string
	OriginSpanID  string
	RequestID     string
	MessageID     string
}

// TaskObservabilityContextReader resolves the original accepted request for a
// task. A missing context is not an execution failure.
type TaskObservabilityContextReader interface {
	GetTaskObservabilityContext(context.Context, string) (TaskObservabilityContext, bool, error)
}

// TaskAttemptTraceLinker durably associates one task claim generation with its
// original accepted request and exported attempt span.
type TaskAttemptTraceLinker interface {
	LinkTaskAttemptTrace(context.Context, string, int64, TaskObservabilityContext, string, string) error
}
