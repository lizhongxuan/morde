package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestTaskGetClassifiesOnlyCurrentAuthorizedRequestsAsUserAction(t *testing.T) {
	now := time.Now().UTC()
	revision := int64(3)
	requestRevision := revision
	currentApproval := task.PendingRequest{
		ID: "r1234567", TaskID: "q1234567", TaskRevision: &requestRevision, Version: 2, Kind: "approval",
		Status: task.PendingRequestPending, CreatedAt: now,
	}
	currentClarification := task.ClarificationRequest{
		ID: "r7654321", TaskID: "q1234567", Version: 1, ModelStepID: "s1234567", CallID: "morde-question",
		Question: "Which folder should I use?", Status: task.ClarificationPending, CreatedAt: now,
	}
	clarificationStepID := "s2468135"

	tests := []struct {
		name        string
		details     TaskDetails
		denyControl bool
		wantKind    TaskAttentionKind
		wantReason  string
		wantAction  *TaskAttentionAction
	}{
		{
			name: "current approval", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "approval_required"), PendingRequest: &currentApproval},
			wantKind: TaskAttentionNeedsUserAction, wantReason: "approval_required", wantAction: actionPointer(TaskAttentionActionApprove),
		},
		{
			name: "current clarification", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "clarification_required"),
				Steps: []task.Step{
					{ID: "s1234567", TaskID: "q1234567", Sequence: 1, Kind: "model", Status: task.StepSucceeded, TaskRevision: &revision},
					{ID: clarificationStepID, TaskID: "q1234567", Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, TaskRevision: &revision},
				}, ClarificationRequests: []task.ClarificationRequest{currentClarification}},
			wantKind: TaskAttentionNeedsUserAction, wantReason: "clarification_required", wantAction: actionPointer(TaskAttentionActionClarify),
		},
		{
			name: "stale approval revision", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "approval_required"),
				PendingRequest: func() *task.PendingRequest {
					stale := currentApproval
					old := revision - 1
					stale.TaskRevision = &old
					return &stale
				}()},
			wantKind: TaskAttentionInterrupted, wantReason: "unknown",
		},
		{
			name: "unauthorized approval", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "approval_required"), PendingRequest: &currentApproval},
			denyControl: true, wantKind: TaskAttentionInterrupted, wantReason: "unknown",
		},
		{
			name: "approval missing current revision", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "approval_required"),
				PendingRequest: func() *task.PendingRequest { legacy := currentApproval; legacy.TaskRevision = nil; return &legacy }()},
			wantKind: TaskAttentionInterrupted, wantReason: "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "current clarification" {
				test.details.Task.CurrentStep = clarificationStepID
			}
			authorizer := &taskApprovalScopeAuthorizer{denyControlHost: "h9999999"}
			if test.denyControl {
				authorizer.denyControlHost = "h1234567"
			}
			service := &TaskService{executionAuthorizer: authorizer}
			if test.name == "unauthorized approval" {
				test.details.Task.HostID = "h1234567"
			}
			store := &taskStoreStub{details: test.details}
			service.store = store
			attention, err := service.Get(context.Background(), taskTestPrincipal(), "q1234567")
			if err != nil {
				t.Fatal(err)
			}
			got := attention.Attention
			if got == nil || got.Kind != test.wantKind || got.Reason != test.wantReason || got.EventSequence != test.details.Task.EventSequence {
				t.Fatalf("Task attention = %#v, want kind=%q reason=%q event_sequence=%d", got, test.wantKind, test.wantReason, test.details.Task.EventSequence)
			}
			if (got.Action == nil) != (test.wantAction == nil) || got.Action != nil && *got.Action != *test.wantAction {
				t.Fatalf("Task attention action = %v, want %v", got.Action, test.wantAction)
			}
		})
	}
}

func TestTaskAttentionKeepsUnknownOutcomesAndNoScheduleConservative(t *testing.T) {
	revision := int64(1)
	service := &TaskService{}
	tests := []struct {
		name    string
		details TaskDetails
		want    TaskAttentionKind
	}{
		{name: "unresolved recovery", details: TaskDetails{Task: attentionTask(revision, task.StatusWaitingUser, "dsh_recovery_unresolved")}, want: TaskAttentionResultUnverified},
		{name: "unknown step result", details: TaskDetails{Task: attentionTask(revision, task.StatusFailed, ""), Steps: []task.Step{{TaskRevision: &revision, Status: task.StepUnknown}}}, want: TaskAttentionResultUnverified},
		{name: "ordinary interrupted task", details: TaskDetails{Task: attentionTask(revision, task.StatusFailed, "")}, want: TaskAttentionInterrupted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attention := service.taskAttention(context.Background(), taskTestPrincipal(), test.details)
			if attention == nil || attention.Kind != test.want || attention.Action != nil {
				t.Fatalf("Task attention = %#v, want %q with no action", attention, test.want)
			}
		})
	}
	if attention := service.taskAttention(context.Background(), taskTestPrincipal(), TaskDetails{
		Task: attentionTask(revision, task.StatusRunning, ""),
	}); attention != nil {
		t.Fatalf("running Task without a persisted recovery schedule was labeled as recovery: %#v", attention)
	}
}

func TestTaskAttentionRequiresPersistedCurrentRecoveryForRetryOrRecovering(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       task.ModelRecoveryState
		status      task.Status
		actorID     string
		denyControl bool
		staleEvent  bool
		wantKind    TaskAttentionKind
		wantAction  *TaskAttentionAction
	}{
		{name: "scheduled", state: task.ModelRecoveryScheduled, status: task.StatusQueued, actorID: "u1234567", wantKind: TaskAttentionRecovering},
		{name: "retry available", state: task.ModelRecoveryRetryAvailable, status: task.StatusWaitingUser, actorID: "u1234567",
			wantKind: TaskAttentionPausedRetryable, wantAction: actionPointer(TaskAttentionActionRetry)},
		{name: "retry is not authorized for another reader", state: task.ModelRecoveryRetryAvailable, status: task.StatusWaitingUser,
			actorID: "u7654321", wantKind: TaskAttentionPausedRetryable},
		{name: "retry control revoked", state: task.ModelRecoveryRetryAvailable, status: task.StatusWaitingUser, actorID: "u1234567",
			denyControl: true, wantKind: TaskAttentionPausedRetryable},
		{name: "retry event is stale", state: task.ModelRecoveryRetryAvailable, status: task.StatusWaitingUser, actorID: "u1234567",
			staleEvent: true, wantKind: TaskAttentionInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			revision := int64(4)
			value := attentionTask(revision, test.status, "task_model_recovery_retry_available")
			value.DesiredRevision = &revision
			value.EventSequence = 20
			value.Authorization.Version = 3
			if test.staleEvent {
				value.EventSequence++
			}
			intent := attentionRecoveryIntent(value, test.state)
			intent.EventSequence = 20
			if !ValidTaskModelRecoveryIntent(intent) {
				t.Fatalf("test recovery intent is invalid: %+v", intent)
			}
			store := &taskAttentionRecoveryStore{taskStoreStub: &taskStoreStub{details: TaskDetails{Task: value}}, intent: intent, found: true}
			authorizer := &taskApprovalScopeAuthorizer{denyControlHost: "h9999999"}
			value.HostID = "h1234567"
			store.details.Task.HostID = value.HostID
			if test.denyControl {
				authorizer.denyControlHost = value.HostID
			}
			service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
			principal := taskTestPrincipal()
			principal.UserID = test.actorID
			details, err := service.Get(context.Background(), principal, value.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := details.Attention
			if got == nil || got.Kind != test.wantKind || got.EventSequence != details.Task.EventSequence {
				t.Fatalf("Task attention = %#v, want %q at event %d", got, test.wantKind, details.Task.EventSequence)
			}
			if (got.Action == nil) != (test.wantAction == nil) || got.Action != nil && *got.Action != *test.wantAction {
				t.Fatalf("Task recovery action = %v, want %v", got.Action, test.wantAction)
			}
			if !ValidTaskAttention(got, details.Task.EventSequence) {
				t.Fatalf("current recovery attention failed its contract: %+v", got)
			}
		})
	}
	value := attentionTask(4, task.StatusWaitingUser, "dsh_recovery_unresolved")
	value.DesiredRevision = value.CurrentRevision
	service := &TaskService{store: &taskStoreStub{details: TaskDetails{Task: value}}}
	if attention := service.taskAttention(context.Background(), identity.Principal{UserID: value.OwnerID}, TaskDetails{Task: value}); attention == nil || attention.Kind != TaskAttentionResultUnverified || attention.Action != nil {
		t.Fatalf("missing persistent schedule exposed retry action: %#v", attention)
	}
}

type taskAttentionRecoveryStore struct {
	*taskStoreStub
	intent task.ModelRecoveryIntent
	found  bool
}

func (s *taskAttentionRecoveryStore) GetTaskModelRecovery(_ context.Context, taskID, ownerID string, revision int64) (task.ModelRecoveryIntent, bool, error) {
	if s.intent.TaskID != taskID || s.intent.OwnerID != ownerID || s.intent.TaskRevision != revision {
		return task.ModelRecoveryIntent{}, false, nil
	}
	return s.intent, s.found, nil
}

func attentionRecoveryIntent(value task.Task, state task.ModelRecoveryState) task.ModelRecoveryIntent {
	callID, _ := DSHModelCallID(value.ID, "session_123", 5)
	recoveryCallID, _ := TaskModelRecoveryCallID(value.ID, *value.CurrentRevision, callID, 5)
	return task.ModelRecoveryIntent{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		TaskRevision: *value.CurrentRevision, AuthorizationVersion: value.Authorization.Version, AgentInstanceID: "agent_123",
		AgentGeneration: 2, AgentSessionID: "session_123", FailedLeaseOwner: "worker-1", FailedLeaseVersion: 4,
		EngineVersion: "engine", ProfileVersion: "profile", FailedCallID: callID, FailedModelID: "p1234567",
		FailedRequestDigest: make([]byte, 32), FailedThroughSequence: 5,
		CheckpointThroughSequence: 5, CheckpointDigest: make([]byte, 32), ToolsetDigest: make([]byte, 32),
		RecoveryCallID: recoveryCallID, State: state, EventSequence: value.EventSequence}
}

func TestValidTaskAttentionBindsActionToCurrentEvent(t *testing.T) {
	action := TaskAttentionActionApprove
	valid := &TaskAttention{Kind: TaskAttentionNeedsUserAction, Reason: "approval_required", Action: &action, EventSequence: 7}
	if !ValidTaskAttention(valid, 7) || ValidTaskAttention(valid, 8) || ValidTaskAttention(nil, -1) {
		t.Fatal("Task attention accepted a stale event sequence or a negative cursor")
	}
	invalid := *valid
	invalid.Action = nil
	if ValidTaskAttention(&invalid, 7) {
		t.Fatal("actionable approval without an action was accepted")
	}
	invalid = *valid
	invalid.Reason = "provider_timeout"
	if ValidTaskAttention(&invalid, 7) {
		t.Fatal("internal failure detail was accepted as a public attention reason")
	}
}

func attentionTask(revision int64, status task.Status, waitingReason string) task.Task {
	currentRevision := revision
	return task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", HostID: "", ConversationID: "c1234567",
		Status: status, WaitingReason: waitingReason, EventSequence: 19, CurrentRevision: &currentRevision}
}

func actionPointer(value TaskAttentionAction) *TaskAttentionAction { return &value }
