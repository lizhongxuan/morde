package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskStepRetrySuccessPreservesFailureAndEvidence(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	setAgentTestHostRuntime(t, store, hostValue, "runtime-step-retry-success")
	if err := store.RecordHostReplicaHeartbeat(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatalf("record Host Runtime heartbeat: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "step-retry-success", "retry one failed safe tool call")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Host-bound retry Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "step-retry-success-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim retry Task: found=%t task=%+v err=%v", found, claimed, err)
	}

	const supervisorID = "step-retry-success-supervisor"
	supervisorKey := bytes.Repeat([]byte{0x4d}, 32)
	authenticator, err := app.NewAgentStopAuthenticator(supervisorID, supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	firstNonce := bytes.Repeat([]byte{0x27}, 32)
	first, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: "agent-step-retry-success-1", AgentSessionID: "session-step-retry-success",
			SupervisorID: supervisorID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: firstNonce, LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed || first.Fence.AgentGeneration != 1 {
		t.Fatalf("start initial Task Agent: fence=%+v replayed=%t err=%v", first.Fence, replayed, err)
	}

	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := hashBytes("file_read:{\"path\":\"example.txt\"}")
	sourceStepID := mustID(t, domain.NewStepID)
	sourceDetail, sourcePlain, sourceDetailDigest := sealRetryTestStepDetail(t, sealer, owner.ID, claimed.ID,
		sourceStepID, 0, "The call was not dispatched.")
	sourceStep := task.Step{ID: sourceStepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepFailed,
		Title: "Read selected file", Summary: "The call was not dispatched."}
	sourcePayload, err := json.Marshal(map[string]any{"step_id": sourceStepID, "sequence": 1, "kind": "tool",
		"status": string(task.StepFailed), "title": sourceStep.Title, "summary": sourceStep.Summary})
	if err != nil {
		t.Fatal(err)
	}
	usage := claimed.Usage
	usage.ToolCalls++
	sourceEvent, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: sourceStepID, Usage: usage, Steps: []task.Step{sourceStep},
		Event: task.Event{TaskID: claimed.ID, Type: "task.step", Payload: sourcePayload},
		ToolCall: &app.TaskToolCallProgress{
			AgentSessionID: first.Fence.AgentSessionID, AgentInstanceID: first.Fence.AgentInstanceID,
			AgentGeneration: first.Fence.AgentGeneration, AuthorizationVersion: first.Fence.AuthorizationVersion,
			TaskRevision: 0, CallKey: "tool-call-step-retry-success-source", ToolName: "file_read",
			RequestDigest: requestDigest, Phase: "prepare", FailureReason: "not_dispatched", Retryable: true,
			DetailState: "available", DetailCiphertext: sourceDetail, DetailSHA256: sourceDetailDigest,
			DetailSizeBytes: int32(len(sourcePlain)),
		},
	})
	if err != nil || sourceEvent.Sequence < 1 {
		t.Fatalf("persist source failed step and detail: event=%+v err=%v", sourceEvent, err)
	}

	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: sourceStepID, Result: "The initial run stopped before dispatch.", Usage: usage,
		Event: task.Event{TaskID: claimed.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); err != nil {
		t.Fatalf("complete Task with a retryable failed step: %v", err)
	}
	checkpointDigest := hashBytes("step-retry-success-checkpoint")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET current_revision=0,desired_revision=0,checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, claimed.ID, checkpointDigest); err != nil {
		t.Fatalf("prepare completed Task checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, claimed.ID, first.Fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("prepare Agent Session checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,
engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,$4,$5,$5,0,0,$6,'sealed',$7,$8,$9,$10)`, claimed.ID, owner.ID, first.Fence.AgentInstanceID,
		first.Fence.AgentGeneration, first.Fence.AgentSessionID, checkpointDigest, first.Fence.EngineVersion,
		first.Fence.ProfileVersion, first.Fence.AuthorizationVersion, first.Fence.LeaseVersion); err != nil {
		t.Fatalf("persist recovery checkpoint: %v", err)
	}
	if _, err := instances.ConfirmStopped(ctx, signAgentLifecycleReceipt(t, supervisorKey, first.Fence, firstNonce)); err != nil {
		t.Fatalf("record initial Agent stop evidence: %v", err)
	}

	attemptStepID := mustID(t, domain.NewStepID)
	request := app.TaskStepRetryRequest{TaskID: claimed.ID, StepID: sourceStepID, ExpectedTaskRevision: 0,
		ExpectedAttempt: 1, AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.step.retry", Key: "retry-success",
		RequestHash: hashBytes("task.step.retry:retry-success"), RelatedID: sourceStepID}
	retry, err := store.RequestTaskStepRetry(ctx, record, request)
	if err != nil || retry.Intent.AttemptStepID != attemptStepID || retry.Intent.SourceStepID != sourceStepID {
		t.Fatalf("request safe Task step retry: intent=%+v err=%v", retry.Intent, err)
	}

	retryClaim, found, err := store.ClaimNextTask(ctx, "step-retry-success-worker", time.Minute)
	if err != nil || !found || retryClaim.ID != claimed.ID || retryClaim.StepRetry == nil || retryClaim.CurrentRevision == nil {
		t.Fatalf("claim queued retry Task: found=%t task=%+v err=%v", found, retryClaim, err)
	}
	claimedIntent := retryClaim.StepRetry
	if *retryClaim.CurrentRevision != retry.Intent.TaskRevision || claimedIntent.TaskID != retry.Intent.TaskID ||
		claimedIntent.OwnerID != retry.Intent.OwnerID || claimedIntent.TaskRevision != retry.Intent.TaskRevision ||
		claimedIntent.Attempt != retry.Intent.Attempt || claimedIntent.SourceStepID != retry.Intent.SourceStepID ||
		claimedIntent.AttemptStepID != retry.Intent.AttemptStepID || claimedIntent.RetryCallID != retry.Intent.RetryCallID {
		t.Fatalf("claimed retry intent=%+v revision=%v; want request intent=%+v", claimedIntent, retryClaim.CurrentRevision, retry.Intent)
	}
	retryNonce := bytes.Repeat([]byte{0x29}, 32)
	retryAgent, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: retryClaim.ID, OwnerID: retryClaim.OwnerID, InitiatorID: retryClaim.InitiatorID,
			AgentInstanceID: "agent-step-retry-success-2", AgentSessionID: claimedIntent.AgentSessionID,
			SupervisorID: supervisorID, LeaseOwner: retryClaim.Lease.Owner, LeaseVersion: retryClaim.Lease.Version,
			AuthorizationVersion: retryClaim.Authorization.Version, EngineVersion: first.Fence.EngineVersion,
			ProfileVersion: first.Fence.ProfileVersion},
		SupervisorNonce: retryNonce, LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed || retryAgent.Fence.AgentGeneration != 2 {
		t.Fatalf("start retry Task Agent: fence=%+v replayed=%t err=%v", retryAgent.Fence, replayed, err)
	}
	if err := store.ConsumeTaskStepRetryIntent(ctx, *claimedIntent, retryAgent.Fence); err != nil {
		t.Fatalf("consume retry intent at the execution fence: %v", err)
	}

	retryDetail, retryPlain, retryDetailDigest := sealRetryTestStepDetail(t, sealer, owner.ID, claimed.ID,
		attemptStepID, 0, "The selected file contents were read successfully.")
	retryStep := task.Step{ID: attemptStepID, TaskID: claimed.ID, Sequence: 2, Kind: "tool", Status: task.StepSucceeded,
		Title: "Read selected file", Summary: "The retry read completed."}
	retryPayload, err := json.Marshal(map[string]any{"step_id": attemptStepID, "sequence": 2, "kind": "tool",
		"status": string(task.StepSucceeded), "title": retryStep.Title, "summary": retryStep.Summary})
	if err != nil {
		t.Fatal(err)
	}
	retryUsage := retryClaim.Usage
	retryUsage.ToolCalls++
	retryEvent, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: retryClaim.ID, LeaseOwner: retryClaim.Lease.Owner, LeaseVersion: retryClaim.Lease.Version,
		Status: task.StatusRunning, CurrentStep: attemptStepID, Usage: retryUsage, Steps: []task.Step{retryStep},
		Event: task.Event{TaskID: retryClaim.ID, Type: "task.step", Payload: retryPayload},
		ToolCall: &app.TaskToolCallProgress{
			AgentSessionID: retryAgent.Fence.AgentSessionID, AgentInstanceID: retryAgent.Fence.AgentInstanceID,
			AgentGeneration: retryAgent.Fence.AgentGeneration, AuthorizationVersion: retryAgent.Fence.AuthorizationVersion,
			TaskRevision: 0, CallKey: claimedIntent.RetryCallID, ToolName: "file_read",
			RequestDigest: requestDigest, Phase: "succeeded", DetailState: "available",
			DetailCiphertext: retryDetail, DetailSHA256: retryDetailDigest, DetailSizeBytes: int32(len(retryPlain)),
		},
	})
	if err != nil || retryEvent.Sequence <= sourceEvent.Sequence {
		t.Fatalf("persist successful retry step and detail: event=%+v err=%v", retryEvent, err)
	}

	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil {
		t.Fatalf("read retried Task: %v", err)
	}
	var source, attempt *task.Step
	for index := range details.Steps {
		step := &details.Steps[index]
		switch step.ID {
		case sourceStepID:
			source = step
		case attemptStepID:
			attempt = step
		}
	}
	if source == nil || source.Status != task.StepFailed || source.RetryOfStepID != "" ||
		source.FailureReason != "not_dispatched" || !source.Retryable || source.DetailState != "available" {
		t.Fatalf("source failure was overwritten or lost evidence: %+v", source)
	}
	if attempt == nil || attempt.Status != task.StepSucceeded || attempt.RetryOfStepID != sourceStepID || attempt.Attempt != 2 ||
		attempt.DetailState != "available" || attempt.ToolCallKey != request.RetryCallID {
		t.Fatalf("successful retry lost source relation or evidence: %+v", attempt)
	}
	var retryOf string
	var attemptNumber int64
	if err := pool.QueryRow(ctx, `SELECT retry_of_step_id,attempt_no FROM task_steps WHERE task_id=$1 AND id=$2`,
		claimed.ID, attemptStepID).Scan(&retryOf, &attemptNumber); err != nil || retryOf != sourceStepID || attemptNumber != 2 {
		t.Fatalf("persisted retry_of_step_id/attempt=%q/%d err=%v", retryOf, attemptNumber, err)
	}

	storedSource, err := store.GetTaskStepDetail(ctx, owner.ID, claimed.ID, sourceStepID)
	if err != nil || storedSource.EventSequence < 1 || len(storedSource.Ciphertext) == 0 {
		t.Fatalf("read source step detail: detail=%+v err=%v", storedSource, err)
	}
	storedAttempt, err := store.GetTaskStepDetail(ctx, owner.ID, claimed.ID, attemptStepID)
	if err != nil || storedAttempt.EventSequence < 1 || len(storedAttempt.Ciphertext) == 0 {
		t.Fatalf("read retry step detail: detail=%+v err=%v", storedAttempt, err)
	}
	for _, evidence := range []struct {
		stepID     string
		detail     app.TaskStepDetailRecord
		wantResult string
	}{{sourceStepID, storedSource, "The call was not dispatched."},
		{attemptStepID, storedAttempt, "The selected file contents were read successfully."}} {
		plain, err := sealer.OpenWithAAD(evidence.detail.Ciphertext,
			aadForTaskStepDetail(owner.ID, claimed.ID, evidence.stepID, evidence.detail.TaskRevision))
		if err != nil {
			t.Fatalf("open %s step detail: %v", evidence.stepID, err)
		}
		var document app.TaskStepDetailDocument
		if err := json.Unmarshal(plain, &document); err != nil || document.Object != "example.txt" || document.Result != evidence.wantResult {
			t.Fatalf("read %s detail document=%+v err=%v", evidence.stepID, document, err)
		}
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 100)
	if err != nil {
		t.Fatalf("list retry Task events: %v", err)
	}
	seenFailedSource, seenSuccessfulAttempt := false, false
	for _, event := range events {
		if event.Type != "task.step" {
			continue
		}
		var payload struct {
			StepID string `json:"step_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode Task step event %d: %v", event.Sequence, err)
		}
		if payload.StepID == sourceStepID && payload.Status == string(task.StepFailed) {
			seenFailedSource = true
		}
		if payload.StepID == attemptStepID && payload.Status == string(task.StepSucceeded) {
			seenSuccessfulAttempt = true
		}
	}
	if !seenFailedSource || !seenSuccessfulAttempt {
		t.Fatalf("failed source/successful retry events readable=%t/%t", seenFailedSource, seenSuccessfulAttempt)
	}
}

func TestPostgresSameOwnerTaskEventPagesDoNotMix(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	commitStep := func(taskID, worker, marker string) {
		t.Helper()
		claimed, found, err := store.ClaimNextTask(ctx, worker, time.Minute)
		if err != nil || !found || claimed.ID != taskID {
			t.Fatalf("claim Task %s: found=%t task=%+v err=%v", taskID, found, claimed, err)
		}
		stepID := mustID(t, domain.NewStepID)
		payload, err := json.Marshal(map[string]any{"step_id": stepID, "marker": marker})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: taskID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			Status: task.StatusRunning, CurrentStep: stepID,
			Steps: []task.Step{{ID: stepID, TaskID: taskID, Sequence: 1, Kind: "model", Status: task.StepRunning,
				Title: marker, Summary: marker}},
			Event: task.Event{TaskID: taskID, Type: "task.step", Payload: payload},
		}); err != nil {
			t.Fatalf("commit %s event: %v", marker, err)
		}
	}

	firstSubmission := newTestTaskSubmission(t, owner.ID, "", "same-owner-event-a", "Task A")
	first, err := store.SubmitTask(ctx, firstSubmission)
	if err != nil {
		t.Fatalf("submit first Task: %v", err)
	}
	commitStep(first.Task.ID, "same-owner-event-worker-a", "task-a-private-marker")
	secondSubmission := newTestTaskSubmission(t, owner.ID, "", "same-owner-event-b", "Task B")
	second, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatalf("submit second Task: %v", err)
	}
	commitStep(second.Task.ID, "same-owner-event-worker-b", "task-b-private-marker")

	assertOnlyMarker := func(taskID, wantMarker, forbiddenMarker string) {
		t.Helper()
		events, err := store.ListTaskEvents(ctx, owner.ID, taskID, 0, 100)
		if err != nil {
			t.Fatalf("list events for Task %s: %v", taskID, err)
		}
		found := false
		for _, event := range events {
			if event.TaskID != taskID {
				t.Fatalf("Task %s event page contains event for Task %s", taskID, event.TaskID)
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode Task %s event %d: %v", taskID, event.Sequence, err)
			}
			if payload["marker"] == forbiddenMarker {
				t.Fatalf("Task %s event page leaked marker %q", taskID, forbiddenMarker)
			}
			if payload["marker"] == wantMarker {
				found = true
			}
		}
		if !found {
			t.Fatalf("Task %s event page did not contain marker %q", taskID, wantMarker)
		}
	}
	assertOnlyMarker(first.Task.ID, "task-a-private-marker", "task-b-private-marker")
	assertOnlyMarker(second.Task.ID, "task-b-private-marker", "task-a-private-marker")
}

func sealRetryTestStepDetail(t *testing.T, sealer *app.Sealer, ownerID, taskID, stepID string,
	revision int64, result string) ([]byte, []byte, []byte) {
	t.Helper()
	document := app.TaskStepDetailDocument{Object: "example.txt", Input: "Read the selected file.", Result: result}
	plain, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.SealWithAAD(plain, aadForTaskStepDetail(ownerID, taskID, stepID, revision))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(plain)
	return ciphertext, plain, digest[:]
}
