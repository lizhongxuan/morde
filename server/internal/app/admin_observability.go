package app

import (
	"context"
	"encoding/json"
	"time"
)

type AdminObservabilitySource struct {
	Status    string     `json:"status"`
	Reason    *string    `json:"reason"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type AdminObservabilityExportHealth struct {
	Status        string
	ReasonCode    string
	FailedBatches uint64
	LastSuccessAt *time.Time
}

type AdminObservabilityOverview struct {
	SpansSource   AdminObservabilitySource `json:"spans_source"`
	EventsSource  AdminObservabilitySource `json:"events_source"`
	RequestCount  int64                    `json:"request_count"`
	ErrorCount    int64                    `json:"error_count"`
	P95DurationMS float64                  `json:"p95_duration_ms"`
}

type AdminObservabilityTraceQuery struct {
	TraceID   string
	RequestID string
	TaskID    string
	Query     string
	Limit     int
	Cursor    string
}

type AdminObservabilityTraceSummary struct {
	TraceID    string     `json:"trace_id"`
	RequestID  *string    `json:"request_id"`
	TaskID     *string    `json:"task_id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Status     string     `json:"status"`
	SpanCount  int64      `json:"span_count"`
	ErrorCount int64      `json:"error_count"`
}

type AdminObservabilityTracePage struct {
	Items      []AdminObservabilityTraceSummary `json:"items"`
	NextCursor *string                          `json:"next_cursor"`
}

type AdminObservabilitySpan struct {
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	StartAt      time.Time       `json:"start_at"`
	EndAt        time.Time       `json:"end_at"`
	Attributes   json.RawMessage `json:"attributes"`
	Events       json.RawMessage `json:"events"`
	DroppedCount int             `json:"-"`
}

type AdminObservabilityTraceDetail struct {
	TraceID string                   `json:"trace_id"`
	Spans   []AdminObservabilitySpan `json:"spans"`
	Source  AdminObservabilitySource `json:"coverage"`
}

type AdminObservabilityAgentSession struct {
	SessionID       string  `json:"session_id"`
	ParentSessionID *string `json:"parent_session_id"`
	StepID          *string `json:"step_id"`
	Role            string  `json:"role"`
	ThroughSequence int64   `json:"-"`
}

type AdminObservabilityTraceSessions struct {
	TaskID   *string                          `json:"task_id"`
	OwnerID  *string                          `json:"owner_id,omitempty"`
	Sessions []AdminObservabilityAgentSession `json:"agent_sessions"`
	Source   AdminObservabilitySource         `json:"coverage"`
}

type AdminObservabilityReader interface {
	GetAdminObservabilityOverview(context.Context) (AdminObservabilityOverview, error)
	ListAdminObservabilityTraces(context.Context, AdminObservabilityTraceQuery) (AdminObservabilityTracePage, error)
	GetAdminObservabilityTrace(context.Context, string) (AdminObservabilityTraceDetail, error)
	GetAdminObservabilityTraceSessions(context.Context, string) (AdminObservabilityTraceSessions, error)
}
