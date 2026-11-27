package app

import (
	"context"
	"encoding/json"
	"time"
)

type AdminObservabilityLogQuery struct {
	TraceID   string
	RequestID string
	TaskID    string
	Limit     int
	Cursor    string
}

type AdminObservabilityLog struct {
	ID         int64           `json:"-"`
	Time       time.Time       `json:"time"`
	Severity   string          `json:"severity"`
	Service    string          `json:"service"`
	Message    string          `json:"message"`
	TraceID    string          `json:"trace_id"`
	SpanID     *string         `json:"span_id"`
	RequestID  *string         `json:"request_id"`
	TaskID     *string         `json:"task_id"`
	Attributes json.RawMessage `json:"attributes"`
}

type AdminObservabilityLogPage struct {
	Items      []AdminObservabilityLog  `json:"items"`
	NextCursor *string                  `json:"next_cursor"`
	Coverage   AdminObservabilitySource `json:"coverage"`
}

type AdminObservabilityLogReader interface {
	ListAdminObservabilityLogs(context.Context, AdminObservabilityLogQuery) (AdminObservabilityLogPage, error)
}

type AdminObservabilityLogHealth struct {
	Status         string
	DroppedRecords uint64
	FailedBatches  uint64
	ReasonCode     string
	LastSuccessAt  *time.Time
}

type AdminObservabilityLogHealthProvider interface {
	Health() AdminObservabilityLogHealth
}
