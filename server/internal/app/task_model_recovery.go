package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// TaskModelRecoveryCommand asks storage to atomically schedule one model call
// with bounded gateway failure evidence. ToolsetDigest is computed by the
// worker after validating the pinned, read-only checkpoint path.
type TaskModelRecoveryCommand struct {
	Task                      task.Task
	Failure                   TaskModelRecoveryFailure
	CheckpointThroughSequence int64
	CheckpointDigest          []byte
	ToolsetDigest             []byte
	Auto                      bool
}

// TaskModelRecoveryRetryCommand binds a user initiated recovery to the exact
// Task snapshot shown by the client.
type TaskModelRecoveryRetryCommand struct {
	TaskID               string
	OwnerID              string
	InitiatorID          string
	TaskRevision         int64
	AuthorizationVersion int64
	EventSequence        int64
}

type TaskModelRecoveryRetryResult struct {
	Intent   task.ModelRecoveryIntent
	Replayed bool
}

// TaskModelRecoveryStore owns the durable one-attempt schedule and Task queue
// transition. BeginTaskModelRecoveryAttempt consumes the single attempt before
// the worker contacts the model gateway.
type TaskModelRecoveryStore interface {
	ScheduleTaskModelRecovery(context.Context, TaskModelRecoveryCommand) (task.ModelRecoveryIntent, bool, error)
	RequestTaskModelRecoveryRetry(context.Context, IdempotencyRecord, TaskModelRecoveryRetryCommand) (TaskModelRecoveryRetryResult, error)
	DeferAutomaticTaskModelRecovery(context.Context, task.ModelRecoveryIntent, AgentInstanceFence) error
	BeginTaskModelRecoveryAttempt(context.Context, task.ModelRecoveryIntent, AgentInstanceFence) error
	GetTaskModelRecovery(context.Context, string, string, int64) (task.ModelRecoveryIntent, bool, error)
}

func TaskModelRecoveryCallID(taskID string, revision int64, failedCallID string, throughSequence int64) (string, error) {
	return taskModelRecoveryIdentity("morde-task-model-recovery-v1", "mrc_", taskID, revision, failedCallID, throughSequence)
}

// TaskModelRecoveryDSHCallID creates a new DSH model-frame identity for the
// one authorized recovery attempt. It is distinct from both the failed frame
// and the durable model-call ledger identity.
func TaskModelRecoveryDSHCallID(taskID string, revision int64, failedCallID string, throughSequence int64) (string, error) {
	return taskModelRecoveryIdentity("morde-task-model-recovery-dsh-v1", "mdr_", taskID, revision, failedCallID, throughSequence)
}

func taskModelRecoveryIdentity(domainVersion, prefix, taskID string, revision int64, failedCallID string, throughSequence int64) (string, error) {
	if !domain.ValidTaskID(taskID) || revision < 0 || throughSequence < 0 || !idempotencyKeyPattern.MatchString(failedCallID) {
		return "", domain.InvalidArgument("task_model_recovery", "recovery call identity is invalid")
	}
	digest := sha256.Sum256([]byte(domainVersion + "\n" + taskID + "\n" + strconv.FormatInt(revision, 10) +
		"\n" + failedCallID + "\n" + strconv.FormatInt(throughSequence, 10)))
	return prefix + hex.EncodeToString(digest[:]), nil
}

func ValidateTaskModelRecoveryCommand(value TaskModelRecoveryCommand) error {
	if value.Failure == nil || value.Task.ID == "" || value.Task.CurrentRevision == nil || value.Task.DesiredRevision == nil ||
		*value.Task.CurrentRevision < 0 || *value.Task.CurrentRevision != *value.Task.DesiredRevision ||
		value.Task.Status != task.StatusRunning || value.Task.ExecutionLocation != "server" ||
		len(value.CheckpointDigest) != sha256.Size || len(value.ToolsetDigest) != sha256.Size || value.CheckpointThroughSequence < 0 {
		return domain.InvalidArgument("task_model_recovery", "recovery command is invalid")
	}
	evidence := value.Failure.TaskModelRecoveryFailureEvidence()
	fence := evidence.TaskFence
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) ||
		!agentCheckpointIDPattern.MatchString(fence.AgentInstanceID) || !agentCheckpointIDPattern.MatchString(fence.AgentSessionID) ||
		fence.AgentRootSessionID != fence.AgentSessionID || fence.AgentGeneration < 1 || fence.LeaseOwner == "" ||
		fence.LeaseVersion < 1 || fence.AuthorizationVersion < 1 || fence.ThroughSequence < 0 ||
		fence.EngineVersion == "" || fence.ProfileVersion == "" || fence.TaskRevision != *value.Task.CurrentRevision ||
		fence.RecoveryCallID != "" || fence.RecoveryDSHCallID != "" ||
		!evidence.UsageKnown || evidence.InputTokens != 0 || evidence.OutputTokens != 0 ||
		!domain.ValidModelConnectionID(evidence.ModelID) || len(evidence.RequestDigest) != sha256.Size {
		return domain.InvalidArgument("task_model_recovery", "model dispatch proof is invalid")
	}
	expectedCallID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil || expectedCallID != evidence.CallID ||
		fence.TaskID != value.Task.ID || fence.OwnerID != value.Task.OwnerID || fence.InitiatorID != value.Task.InitiatorID ||
		fence.TaskRevision != *value.Task.CurrentRevision || fence.LeaseOwner != value.Task.Lease.Owner || fence.LeaseVersion != value.Task.Lease.Version ||
		fence.AgentInstanceID != value.Task.AgentInstanceID || fence.AgentGeneration != value.Task.AgentGeneration ||
		fence.AgentSessionID != value.Task.AgentSessionID || fence.AuthorizationVersion != value.Task.Authorization.Version ||
		fence.EngineVersion != value.Task.AgentEngineVersion || fence.ProfileVersion != value.Task.AgentProfileVersion ||
		value.CheckpointThroughSequence != fence.ThroughSequence || value.Task.CheckpointThroughSequence != value.CheckpointThroughSequence ||
		!equalTaskModelRecoveryDigest(value.CheckpointDigest, value.Task.CheckpointDigest) {
		return domain.InvalidArgument("task_model_recovery", "model dispatch proof does not match the current Task checkpoint")
	}
	switch evidence.Kind {
	case TaskModelRecoveryFailureNoDispatch:
		if evidence.ProviderErrorKind != "" {
			return domain.InvalidArgument("task_model_recovery", "no-dispatch proof contains provider failure details")
		}
	case TaskModelRecoveryFailureProviderReject:
		if evidence.ProviderErrorKind != ModelErrorRateLimited && evidence.ProviderErrorKind != ModelErrorUnavailable {
			return domain.InvalidArgument("task_model_recovery", "provider failure is not retryable")
		}
	default:
		return domain.InvalidArgument("task_model_recovery", "model recovery failure kind is invalid")
	}
	return nil
}

func ValidTaskModelRecoveryIntent(value task.ModelRecoveryIntent) bool {
	if !domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		value.TaskRevision < 0 || value.AuthorizationVersion < 1 || !agentCheckpointIDPattern.MatchString(value.AgentInstanceID) ||
		value.AgentGeneration < 1 || !agentCheckpointIDPattern.MatchString(value.AgentSessionID) || value.EngineVersion == "" ||
		value.ProfileVersion == "" || value.FailedLeaseOwner == "" || value.FailedLeaseVersion < 1 || !idempotencyKeyPattern.MatchString(value.FailedCallID) ||
		!domain.ValidModelConnectionID(value.FailedModelID) || len(value.FailedRequestDigest) != sha256.Size ||
		value.FailedThroughSequence < 0 || value.CheckpointThroughSequence != value.FailedThroughSequence ||
		len(value.CheckpointDigest) != sha256.Size || len(value.ToolsetDigest) != sha256.Size ||
		!idempotencyKeyPattern.MatchString(value.RecoveryCallID) || value.RecoveryCallID == value.FailedCallID || value.EventSequence < 1 {
		return false
	}
	expected, err := TaskModelRecoveryCallID(value.TaskID, value.TaskRevision, value.FailedCallID, value.FailedThroughSequence)
	if err != nil || expected != value.RecoveryCallID {
		return false
	}
	expectedDSHCallID, err := TaskModelRecoveryDSHCallID(value.TaskID, value.TaskRevision, value.FailedCallID, value.FailedThroughSequence)
	if err != nil || expectedDSHCallID != value.RecoveryDSHCallID || value.RecoveryDSHCallID == value.RecoveryCallID ||
		value.RecoveryDSHCallID == value.FailedCallID {
		return false
	}
	switch value.State {
	case task.ModelRecoveryRetryAvailable:
		return !value.Automatic && value.AttemptStartedAt == nil
	case task.ModelRecoveryScheduled:
		return value.AttemptStartedAt == nil
	case task.ModelRecoveryAttempting:
		return value.AttemptStartedAt != nil
	case task.ModelRecoverySpent, task.ModelRecoveryCompleted, task.ModelRecoverySuperseded:
		return true
	default:
		return false
	}
}

func equalTaskModelRecoveryDigest(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
