package app

import (
	"context"
	"time"

	"morde/server/internal/domain/task"
)

// TaskSandboxCall is the durable receipt for one Hostless Task sandbox call.
// It is intentionally separate from task.Operation, whose identity is tied to
// a Host Runtime fence.
type TaskSandboxCall struct {
	ID                     string
	TaskID                 string
	OwnerID                string
	InitiatorID            string
	ProjectID              string
	WorkspaceID            string
	WorkspaceOwnerID       string
	CallID                 string
	LeaseOwner             string
	SandboxID              string
	InputRevision          int64
	WorkspaceCheckpoint    int64
	ProjectResourceVersion int64
	AuthorizationVersion   int64
	AgentInstanceID        string
	AgentGeneration        int64
	AgentSessionID         string
	AgentEngineVersion     string
	AgentProfileVersion    string
	LeaseVersion           int64
	SandboxGeneration      int64
	ContextDigest          []byte
	RequestHash            []byte
	Status                 task.OperationStatus
	ExitCode               *int32
	Output                 string
	ResultSHA256           []byte
	UnknownCode            string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// TaskSandboxCallStore records the dispatch boundary before any sandbox side
// effect. A dispatched call is never replayed; uncertain outcomes become
// unknown and must be reconciled before another call is admitted.
type TaskSandboxCallStore interface {
	PrepareTaskSandboxCall(context.Context, TaskSandboxCall) (TaskSandboxCall, bool, error)
	MarkTaskSandboxCallDispatched(context.Context, TaskSandboxCall, string, int64) (TaskSandboxCall, error)
	CompleteTaskSandboxCall(context.Context, TaskSandboxCall, task.OperationStatus, int32, string) (TaskSandboxCall, error)
	MarkTaskSandboxCallUnknown(context.Context, TaskSandboxCall, string) error
	GetTaskSandboxCall(context.Context, string, string, string) (TaskSandboxCall, error)
}
