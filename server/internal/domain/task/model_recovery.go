package task

import "time"

type ModelRecoveryState string

const (
	ModelRecoveryRetryAvailable ModelRecoveryState = "retry_available"
	ModelRecoveryScheduled      ModelRecoveryState = "scheduled"
	ModelRecoveryAttempting     ModelRecoveryState = "attempting"
	ModelRecoverySpent          ModelRecoveryState = "spent"
	ModelRecoveryCompleted      ModelRecoveryState = "completed"
	ModelRecoverySuperseded     ModelRecoveryState = "superseded"
)

// ModelRecoveryIntent is the durable, revision-fenced recovery record attached
// internally to Task snapshots. It is never serialized to clients.
type ModelRecoveryIntent struct {
	TaskID                    string
	OwnerID                   string
	InitiatorID               string
	TaskRevision              int64
	AuthorizationVersion      int64
	AgentInstanceID           string
	AgentGeneration           int64
	AgentSessionID            string
	FailedLeaseOwner          string
	FailedLeaseVersion        int64
	EngineVersion             string
	ProfileVersion            string
	FailedCallID              string
	FailedModelID             string
	FailedRequestDigest       []byte
	FailedThroughSequence     int64
	CheckpointThroughSequence int64
	CheckpointDigest          []byte
	ToolsetDigest             []byte
	RecoveryDSHCallID         string
	RecoveryCallID            string
	State                     ModelRecoveryState
	Automatic                 bool
	EventSequence             int64
	AttemptStartedAt          *time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}
