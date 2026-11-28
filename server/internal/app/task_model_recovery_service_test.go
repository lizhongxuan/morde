package app

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestRetryTaskModelRecoveryUsesCurrentPersistedIntentAndReturnsQueuedTask(t *testing.T) {
	revision := int64(4)
	value := attentionTask(revision, task.StatusWaitingUser, "task_model_recovery_retry_available")
	value.DesiredRevision = &revision
	value.EventSequence = 20
	value.HostID = "h1234567"
	value.Authorization.Version = 3
	intent := attentionRecoveryIntent(value, task.ModelRecoveryRetryAvailable)
	intent.EventSequence = 20
	store := &taskModelRecoveryRetryStoreStub{taskStoreStub: &taskStoreStub{details: TaskDetails{Task: value}}, intent: intent, found: true}
	authorizer := &taskApprovalScopeAuthorizer{denyControlHost: "h9999999"}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
	principal := taskTestPrincipal()

	details, err := service.RetryTaskModelRecovery(context.Background(), principal, value.ID, revision, value.EventSequence, "retry-model-001")
	if err != nil {
		t.Fatal(err)
	}
	if store.retryCalls != 1 || store.retryCommand.TaskID != value.ID || store.retryCommand.OwnerID != value.OwnerID ||
		store.retryCommand.InitiatorID != value.InitiatorID || store.retryCommand.TaskRevision != revision ||
		store.retryCommand.AuthorizationVersion != value.Authorization.Version || store.retryCommand.EventSequence != value.EventSequence ||
		store.retryRecord.OwnerID != principal.UserID || store.retryRecord.Operation != "task.model_recovery.retry" ||
		store.retryRecord.Key != "retry-model-001" || len(store.retryRecord.RequestHash) != 32 ||
		details.Task.Status != task.StatusQueued || details.Task.EventSequence != 21 || details.Attention == nil ||
		details.Attention.Kind != TaskAttentionRecovering || details.Attention.EventSequence != details.Task.EventSequence {
		t.Fatalf("retry request/result mismatch: store=%+v details=%+v", store, details)
	}

	other := principal
	other.UserID = "u7654321"
	if _, err := service.RetryTaskModelRecovery(context.Background(), other, value.ID, revision, value.EventSequence, "retry-model-002"); domain.CodeOf(err) != domain.CodeNotFound || store.retryCalls != 1 {
		t.Fatalf("another account retried task model recovery: calls=%d err=%v", store.retryCalls, err)
	}
}

func TestTaskModelRecoveryCommandAcceptsOnlyKnownZeroUsageEvidenceForCurrentRevision(t *testing.T) {
	revision := int64(8)
	value := attentionTask(revision, task.StatusRunning, "")
	value.DesiredRevision = &revision
	value.ExecutionLocation = "server"
	value.Lease = task.Lease{Owner: "worker-1", Version: 5}
	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = "agent_123", 3, "session_123"
	value.AgentEngineVersion, value.AgentProfileVersion = "engine", "profile"
	value.Authorization.Version = 4
	value.CheckpointThroughSequence, value.CheckpointDigest = 6, make([]byte, 32)
	fence := ModelTaskFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, TaskRevision: revision,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version, AgentInstanceID: value.AgentInstanceID,
		AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID, AgentRootSessionID: value.AgentSessionID,
		AuthorizationVersion: value.Authorization.Version, ThroughSequence: value.CheckpointThroughSequence,
		EngineVersion: value.AgentEngineVersion, ProfileVersion: value.AgentProfileVersion}
	failedCallID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("request"))
	failure := &TaskModelRetryableFailureError{evidence: TaskModelRecoveryFailureEvidence{Kind: TaskModelRecoveryFailureProviderReject,
		TaskFence: fence, CallID: failedCallID, ModelID: "p1234567", RequestDigest: digest[:], UsageKnown: true,
		ProviderErrorKind: ModelErrorRateLimited}}
	command := TaskModelRecoveryCommand{Task: value, Failure: failure, CheckpointThroughSequence: value.CheckpointThroughSequence,
		CheckpointDigest: value.CheckpointDigest, ToolsetDigest: make([]byte, 32), Auto: true}
	if err := ValidateTaskModelRecoveryCommand(command); err != nil {
		t.Fatalf("confirmed transient zero-usage failure was rejected: %v", err)
	}
	invalid := *failure
	invalid.evidence.ProviderErrorKind = ModelErrorTimeout
	command.Failure = &invalid
	if err := ValidateTaskModelRecoveryCommand(command); err == nil {
		t.Fatal("ambiguous timeout was accepted for automatic recovery")
	}
	invalid = *failure
	invalid.evidence.UsageKnown = false
	command.Failure = &invalid
	if err := ValidateTaskModelRecoveryCommand(command); err == nil {
		t.Fatal("unknown usage was accepted for automatic recovery")
	}
	invalid = *failure
	invalid.evidence.TaskFence.TaskRevision++
	command.Failure = &invalid
	if err := ValidateTaskModelRecoveryCommand(command); err == nil {
		t.Fatal("stale recovery evidence was accepted for a different Task revision")
	}
}

type taskModelRecoveryRetryStoreStub struct {
	*taskStoreStub
	intent       task.ModelRecoveryIntent
	found        bool
	retryCommand TaskModelRecoveryRetryCommand
	retryRecord  IdempotencyRecord
	retryCalls   int
	beginCalls   int
	beginFence   AgentInstanceFence
}

func (s *taskModelRecoveryRetryStoreStub) GetTaskModelRecovery(_ context.Context, taskID, ownerID string, revision int64) (task.ModelRecoveryIntent, bool, error) {
	if s.intent.TaskID != taskID || s.intent.OwnerID != ownerID || s.intent.TaskRevision != revision {
		return task.ModelRecoveryIntent{}, false, nil
	}
	return s.intent, s.found, nil
}

func (s *taskModelRecoveryRetryStoreStub) RequestTaskModelRecoveryRetry(_ context.Context, idempotency IdempotencyRecord,
	command TaskModelRecoveryRetryCommand) (TaskModelRecoveryRetryResult, error) {
	s.retryCalls++
	s.retryRecord, s.retryCommand = idempotency, command
	s.intent.State = task.ModelRecoveryScheduled
	s.intent.EventSequence = 21
	s.details.Task.Status, s.details.Task.EventSequence = task.StatusQueued, 21
	return TaskModelRecoveryRetryResult{Intent: s.intent}, nil
}

func (*taskModelRecoveryRetryStoreStub) ScheduleTaskModelRecovery(context.Context, TaskModelRecoveryCommand) (task.ModelRecoveryIntent, bool, error) {
	return task.ModelRecoveryIntent{}, false, nil
}

func (s *taskModelRecoveryRetryStoreStub) BeginTaskModelRecoveryAttempt(_ context.Context, _ task.ModelRecoveryIntent, fence AgentInstanceFence) error {
	s.beginCalls++
	s.beginFence = fence
	return nil
}

func (s *taskModelRecoveryRetryStoreStub) GetTask(_ context.Context, _, _ string) (TaskDetails, error) {
	return s.details, nil
}

var _ TaskModelRecoveryStore = (*taskModelRecoveryRetryStoreStub)(nil)
var _ TaskExecutionAuthorizer = (*taskApprovalScopeAuthorizer)(nil)

func TestBeginTaskModelRecoveryAttemptAllowsNewAgentGeneration(t *testing.T) {
	value := attentionTask(4, task.StatusQueued, "")
	value.Authorization.Version = 3
	intent := attentionRecoveryIntent(value, task.ModelRecoveryScheduled)
	intent.EventSequence = 21
	if !ValidTaskModelRecoveryIntent(intent) {
		t.Fatalf("test recovery intent is invalid: %+v", intent)
	}
	store := &taskModelRecoveryRetryStoreStub{taskStoreStub: &taskStoreStub{}, intent: intent, found: true}
	broker := &CloudDSHToolBroker{store: store}
	fence := AgentInstanceFence{TaskID: intent.TaskID, OwnerID: intent.OwnerID, InitiatorID: intent.InitiatorID,
		AgentInstanceID: "agent_456", AgentGeneration: intent.AgentGeneration + 1, AgentSessionID: intent.AgentSessionID,
		SupervisorID: "supervisor_1", LeaseOwner: "worker-next", LeaseVersion: intent.FailedLeaseVersion + 1,
		AuthorizationVersion: intent.AuthorizationVersion, EngineVersion: intent.EngineVersion, ProfileVersion: intent.ProfileVersion}

	if err := broker.BeginTaskModelRecoveryAttempt(context.Background(), intent, fence); err != nil {
		t.Fatalf("new fenced Agent generation could not start scheduled recovery: %v", err)
	}
	if store.beginCalls != 1 || store.beginFence != fence {
		t.Fatalf("recovery attempt was not delegated with current fence: calls=%d fence=%+v", store.beginCalls, store.beginFence)
	}

	staleFence := fence
	staleFence.AgentGeneration = intent.AgentGeneration
	staleFence.AgentInstanceID = intent.AgentInstanceID
	if err := broker.BeginTaskModelRecoveryAttempt(context.Background(), intent, staleFence); err == nil || store.beginCalls != 1 {
		t.Fatalf("failed Agent generation was allowed to consume recovery: calls=%d err=%v", store.beginCalls, err)
	}
}
