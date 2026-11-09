package app

import (
	"context"
	"strconv"
	"time"

	"morde/server/internal/domain"
)

// TaskSandboxLifecycleStatus is the durable state of a sandbox owned by one
// Hostless Task. Creating and stopping are write-ahead states: an uncertain
// provider result must be reconciled, never replayed automatically.
type TaskSandboxLifecycleStatus string

const (
	TaskSandboxPrepared   TaskSandboxLifecycleStatus = "prepared"
	TaskSandboxCreating   TaskSandboxLifecycleStatus = "creating"
	TaskSandboxRunning    TaskSandboxLifecycleStatus = "running"
	TaskSandboxStopping   TaskSandboxLifecycleStatus = "stopping"
	TaskSandboxUnknown    TaskSandboxLifecycleStatus = "unknown"
	TaskSandboxTerminated TaskSandboxLifecycleStatus = "terminated"
)

// TaskSandboxLifecycle binds a single sandbox generation to the current Task
// lease and DSH Agent generation. It deliberately has no execution methods;
// callers must first pass the real Task/Agent authorization fences.
type TaskSandboxLifecycle struct {
	ID                   string
	TaskID               string
	OwnerID              string
	InitiatorID          string
	LeaseOwner           string
	LeaseVersion         int64
	AuthorizationVersion int64
	AgentInstanceID      string
	AgentGeneration      int64
	// NonceSHA256 binds Guardian evidence to the latest persisted stop challenge.
	// The raw challenge is short-lived and must only be sent to the dedicated
	// Task Sandbox Guardian; never persist it.
	NonceSHA256         string
	SandboxID           string
	SandboxGeneration   int64
	Status              TaskSandboxLifecycleStatus
	ExpiresAt           *time.Time
	UnknownCode         string
	TerminationEvidence []byte
	TerminatedAt        *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const (
	taskSandboxMetadataManagedBy  = "morde.managed-by"
	taskSandboxMetadataOwnerID    = "morde.owner-id"
	taskSandboxMetadataTaskID     = "morde.task-id"
	taskSandboxMetadataLifecycle  = "morde.task-lifecycle-id"
	taskSandboxMetadataGeneration = "morde.sandbox-generation"
)

// TaskSandboxLifecycleMetadata returns the immutable metadata used to find a
// uniquely-created sandbox again if the provider response is lost. Callers
// must never use these fields as a substitute for the Task/lease fences.
func TaskSandboxLifecycleMetadata(value TaskSandboxLifecycle) (map[string]string, error) {
	if !validTaskSandboxLifecycleMetadataFence(value) {
		return nil, domain.InvalidArgument("task_sandbox.metadata", "Task Sandbox metadata fence is invalid")
	}
	return map[string]string{
		taskSandboxMetadataManagedBy:  "morde",
		taskSandboxMetadataOwnerID:    value.OwnerID,
		taskSandboxMetadataTaskID:     value.TaskID,
		taskSandboxMetadataLifecycle:  value.ID,
		taskSandboxMetadataGeneration: strconv.FormatInt(value.SandboxGeneration, 10),
	}, nil
}

func validTaskSandboxLifecycleMetadataFence(value TaskSandboxLifecycle) bool {
	return domain.ValidOperationID(value.ID) && domain.ValidTaskID(value.TaskID) &&
		domain.ValidUserID(value.OwnerID) && domain.ValidUserID(value.InitiatorID) && value.OwnerID == value.InitiatorID &&
		!invalidCheckpointLabel(value.LeaseOwner, 255) && value.LeaseVersion > 0 && value.AuthorizationVersion > 0 &&
		agentCheckpointIDPattern.MatchString(value.AgentInstanceID) && value.AgentGeneration > 0 && value.SandboxGeneration > 0
}

// TaskSandboxMatchesLifecycle checks that a provider result carries every
// Morde identity marker for one specific Task sandbox generation.
func TaskSandboxMatchesLifecycle(sandbox Sandbox, lifecycle TaskSandboxLifecycle) bool {
	metadata, err := TaskSandboxLifecycleMetadata(lifecycle)
	if err != nil || !validTaskSandboxGuardianSandboxID(sandbox.ID) ||
		(lifecycle.SandboxID != "" && sandbox.ID != lifecycle.SandboxID) {
		return false
	}
	for key, expected := range metadata {
		if sandbox.Metadata[key] != expected {
			return false
		}
	}
	return true
}

// TaskSandboxLifecycleStore persists the write-ahead lifecycle around an
// external sandbox provider. Prepare may adopt only a still-prepared record
// after Task lease recovery. Creating/running/stopping/unknown records are
// never replayed or taken over by a newer Agent generation.
type TaskSandboxLifecycleStore interface {
	PrepareTaskSandboxLifecycle(context.Context, TaskSandboxLifecycle) (TaskSandboxLifecycle, bool, error)
	MarkTaskSandboxLifecycleCreating(context.Context, TaskSandboxLifecycle) (TaskSandboxLifecycle, error)
	BindTaskSandboxLifecycle(context.Context, TaskSandboxLifecycle, string, time.Time) (TaskSandboxLifecycle, error)
	MarkTaskSandboxLifecycleUnknown(context.Context, TaskSandboxLifecycle, string) (TaskSandboxLifecycle, error)
	RequestTaskSandboxStop(context.Context, TaskSandboxLifecycle) (TaskSandboxLifecycle, error)
	TerminateTaskSandbox(context.Context, TaskSandboxLifecycle, []byte) (TaskSandboxLifecycle, error)
	SetTaskSandboxGuardianStopChallenge(context.Context, TaskSandboxLifecycle, string) (TaskSandboxLifecycle, error)
	GetTaskSandboxLifecycle(context.Context, string, string, int64) (TaskSandboxLifecycle, error)
}
