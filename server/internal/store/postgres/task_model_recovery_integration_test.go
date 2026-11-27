package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func installTaskModelRecoveryDispatchFailure(t *testing.T, f routeActionsTestFixture) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.pool.Exec(ctx, `CREATE FUNCTION test_recovery_reject_model_dispatch() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected pre-dispatch failure'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.pool.Exec(ctx, `CREATE TRIGGER test_recovery_reject_model_dispatch
BEFORE UPDATE OF created_at ON model_call_ledger FOR EACH ROW
EXECUTE FUNCTION test_recovery_reject_model_dispatch()`)
	if err != nil {
		_, _ = f.store.pool.Exec(ctx, `DROP FUNCTION test_recovery_reject_model_dispatch()`)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_recovery_reject_model_dispatch ON model_call_ledger`)
		_, _ = f.store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_recovery_reject_model_dispatch()`)
	})
}

func confirmTaskModelRecoveryAgentStop(t *testing.T, store *Store, key []byte, fence app.AgentInstanceFence, nonce []byte) {
	t.Helper()
	serviceAuthenticator, err := app.NewAgentStopAuthenticator(fence.SupervisorID, key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceService(store, serviceAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	receipt := app.AgentStopReceipt{Fence: fence, SupervisorNonce: nonce, TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	if _, err := service.ConfirmStopped(context.Background(), receipt); err != nil {
		t.Fatalf("confirm stopped recovery Agent: %v", err)
	}
}

func createTaskModelRecoveryNoDispatch(t *testing.T) (routeActionsTestFixture, *app.TaskModelNoDispatchError, app.ModelCall, app.ModelTaskFence) {
	t.Helper()
	f, gateway, provider, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET root_checkpoint_through_seq=0,root_checkpoint_baseline_known=true
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	installTaskModelRecoveryDispatchFailure(t, f)
	_, err := gateway.Complete(ctx, call)
	var failure *app.TaskModelNoDispatchError
	if !errors.As(err, &failure) || provider.calls.Load() != 0 {
		t.Fatalf("gateway failure was not proven pre-dispatch: error=%v provider_calls=%d", err, provider.calls.Load())
	}
	var ledgerRows int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		f.ownerID, call.CallID).Scan(&ledgerRows); err != nil || ledgerRows != 0 {
		t.Fatalf("abandoned model ledger rows=%d err=%v", ledgerRows, err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 71
	nonceDigest := sha256.Sum256(nonce)
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET stop_nonce_digest=$2 WHERE task_id=$1 AND agent_generation=1`,
		fence.TaskID, nonceDigest[:]); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 29
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	stopFence := app.AgentInstanceFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
		SupervisorID: "revision-supervisor", LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
	receipt := app.AgentStopReceipt{Fence: stopFence, SupervisorNonce: nonce, TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	if _, err := instances.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm stopped Agent before scheduling recovery: %v", err)
	}
	return f, failure, call, fence
}

func taskModelRecoveryTestCallID(t *testing.T, fence app.ModelTaskFence, call app.ModelCall) string {
	t.Helper()
	value, err := app.TaskModelRecoveryCallID(fence.TaskID, fence.TaskRevision, call.CallID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPostgresTaskModelRecoveryPersistsAndConsumesOneStableAttempt(t *testing.T) {
	f, failure, call, fence := createTaskModelRecoveryNoDispatch(t)
	ctx := context.Background()
	details, err := f.store.GetTask(ctx, fence.TaskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("verified read-only toolset"))
	checkpointDigest := append([]byte(nil), details.Task.CheckpointDigest...)
	command := app.TaskModelRecoveryCommand{Task: details.Task, Failure: failure,
		CheckpointThroughSequence: details.Task.CheckpointThroughSequence, CheckpointDigest: checkpointDigest,
		ToolsetDigest: toolsetDigest[:], Auto: true}
	wantRecoveryCallID := taskModelRecoveryTestCallID(t, fence, call)
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command)
	if err != nil || !accepted || intent.State != task.ModelRecoveryScheduled || intent.FailedCallID != call.CallID ||
		intent.RecoveryCallID != wantRecoveryCallID || intent.FailedLeaseVersion != fence.LeaseVersion {
		t.Fatalf("schedule durable recovery: intent=%+v accepted=%t err=%v", intent, accepted, err)
	}
	var status, reason string
	var leaseOwner string
	var leaseVersion, eventSequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT status,waiting_reason,lease_owner,lease_version,event_seq FROM tasks WHERE id=$1`, fence.TaskID).
		Scan(&status, &reason, &leaseOwner, &leaseVersion, &eventSequence); err != nil || status != string(task.StatusQueued) ||
		reason != taskModelRecoveryScheduledReason || leaseOwner != "" || leaseVersion != fence.LeaseVersion+1 || eventSequence != intent.EventSequence {
		t.Fatalf("queued recovery Task state=%s/%s lease=%s/%d event=%d intent=%+v err=%v",
			status, reason, leaseOwner, leaseVersion, eventSequence, intent, err)
	}
	projected, err := f.store.GetTask(ctx, fence.TaskID, f.ownerID)
	if err != nil || projected.Task.ModelRecovery == nil || projected.Task.ModelRecovery.RecoveryCallID != wantRecoveryCallID {
		t.Fatalf("GetTask did not project recovery schedule: task=%+v err=%v", projected.Task, err)
	}
	replay, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command)
	if err != nil || !accepted || replay.RecoveryCallID != intent.RecoveryCallID || replay.EventSequence != intent.EventSequence {
		t.Fatalf("schedule idempotent replay: intent=%+v accepted=%t err=%v", replay, accepted, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "recovery-worker", time.Minute)
	if err != nil || !found || claimed.ID != fence.TaskID || claimed.ModelRecovery == nil ||
		claimed.ModelRecovery.State != task.ModelRecoveryScheduled || claimed.ModelRecovery.RecoveryCallID != wantRecoveryCallID {
		t.Fatalf("claim did not carry durable recovery plan: task=%+v found=%t err=%v", claimed, found, err)
	}
	key := make([]byte, 32)
	key[0] = 31
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 32
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-2",
		AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion},
		SupervisorNonce: nonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed || lease.Fence.AgentGeneration != fence.AgentGeneration+1 {
		t.Fatalf("start recovery Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *claimed.ModelRecovery, lease.Fence); err != nil {
		t.Fatalf("consume first recovery attempt: %v", err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *claimed.ModelRecovery, lease.Fence); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second recovery begin = %v, want conflict", err)
	}
	stored, found, err := f.store.GetTaskModelRecovery(ctx, fence.TaskID, f.ownerID, *details.Task.CurrentRevision)
	if err != nil || !found || stored.State != task.ModelRecoveryAttempting || stored.AttemptStartedAt == nil ||
		stored.RecoveryCallID != wantRecoveryCallID || stored.ToolsetDigest == nil {
		t.Fatalf("attempting recovery record=%+v found=%t err=%v", stored, found, err)
	}
	confirmTaskModelRecoveryAgentStop(t, f.store, key, lease.Fence, nonce)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	reclaimed, found, err := f.store.ClaimNextTask(ctx, "recovery-worker-restart", time.Minute)
	if err != nil || !found || reclaimed.ID != fence.TaskID || reclaimed.ModelRecovery == nil ||
		reclaimed.ModelRecovery.State != task.ModelRecoveryAttempting || reclaimed.ModelRecovery.RecoveryCallID != wantRecoveryCallID {
		t.Fatalf("restart claim did not carry consumed recovery state: task=%+v found=%t err=%v", reclaimed, found, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *reclaimed.ModelRecovery, lease.Fence); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("restart claim attempted a second dispatch: %v", err)
	}
}

func TestPostgresTaskModelRecoveryUsesDistinctLedgerIDAndReplaysSettledResponse(t *testing.T) {
	f, gateway, failedProvider, fence, failedCall := revisionGatewayFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET root_checkpoint_through_seq=0,root_checkpoint_baseline_known=true
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	failedProvider.err = &app.ModelClientError{Kind: app.ModelErrorRateLimited, Stage: "provider_status", ConfirmedNoUsage: true}
	_, err := gateway.Complete(ctx, failedCall)
	var failure *app.TaskModelRetryableFailureError
	if !errors.As(err, &failure) || failedProvider.calls.Load() != 1 {
		t.Fatalf("provider rejection did not produce bounded retry evidence: error=%v calls=%d", err, failedProvider.calls.Load())
	}
	var failedState string
	var failedInput, failedOutput int64
	if err := f.store.pool.QueryRow(ctx, `SELECT state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, fence.InitiatorID, failedCall.CallID).
		Scan(&failedState, &failedInput, &failedOutput); err != nil || failedState != "settled" || failedInput != 0 || failedOutput != 0 {
		t.Fatalf("known-zero provider rejection ledger=%s/%d/%d err=%v", failedState, failedInput, failedOutput, err)
	}
	var failedResponseRows int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		fence.InitiatorID, failedCall.CallID).Scan(&failedResponseRows); err != nil || failedResponseRows != 0 {
		t.Fatalf("rejected model call unexpectedly has a response row: count=%d err=%v", failedResponseRows, err)
	}

	nonce := make([]byte, 32)
	nonce[0] = 81
	nonceDigest := sha256.Sum256(nonce)
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET stop_nonce_digest=$2 WHERE task_id=$1 AND agent_generation=$3`,
		fence.TaskID, nonceDigest[:], fence.AgentGeneration); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 82
	stopFence := app.AgentInstanceFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
		SupervisorID: "revision-supervisor", LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
	confirmTaskModelRecoveryAgentStop(t, f.store, key, stopFence, nonce)
	details, err := f.store.GetTask(ctx, fence.TaskID, fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("validated public web read-only tools"))
	command := app.TaskModelRecoveryCommand{Task: details.Task, Failure: failure,
		CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
		CheckpointDigest:          append([]byte(nil), details.Task.CheckpointDigest...),
		ToolsetDigest:             toolsetDigest[:], Auto: true}
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command)
	if err != nil || !accepted || intent.FailedCallID != failedCall.CallID || intent.RecoveryCallID == failedCall.CallID {
		t.Fatalf("schedule safe retry with distinct identity: intent=%+v accepted=%t err=%v", intent, accepted, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "recovery-rejection-worker", time.Minute)
	if err != nil || !found || claimed.ModelRecovery == nil {
		t.Fatalf("claim scheduled recovery: task=%+v found=%t err=%v", claimed, found, err)
	}
	serviceAuthenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, serviceAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	recoveryNonce := make([]byte, 32)
	recoveryNonce[0] = 83
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-recovery-2",
		AgentSessionID: claimed.AgentSessionID, SupervisorID: "revision-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion},
		SupervisorNonce: recoveryNonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start recovery generation: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, *claimed.ModelRecovery, lease.Fence); err != nil {
		t.Fatalf("consume the single recovery attempt: %v", err)
	}

	recoveryFence := fence
	recoveryFence.AgentInstanceID, recoveryFence.AgentGeneration = lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration
	recoveryFence.LeaseOwner, recoveryFence.LeaseVersion = lease.Fence.LeaseOwner, lease.Fence.LeaseVersion
	recoveryFence.AgentRootSessionID = recoveryFence.AgentSessionID
	recoveryFence.RecoveryCallID = intent.RecoveryCallID
	response := app.ModelResponse{Text: "recovered result", StopReason: "stop", ProviderReportedModel: "test-model",
		Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}
	recoveryProvider := &revisionProviderTest{response: &response}
	var connectionVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	recoveryGateway, err := app.NewModelGatewayService(f.store, revisionModelResolverTest{version: connectionVersion}, recoveryProvider)
	if err != nil {
		t.Fatal(err)
	}
	setTaskModelResponseSealer(t, recoveryGateway)
	recoveryCall := app.ModelCall{Source: app.ModelCallTask, Task: &recoveryFence, ModelID: failedCall.ModelID,
		CallID: failedCall.CallID, RawRequest: append([]byte(nil), failedCall.RawRequest...)}
	got, err := recoveryGateway.Complete(ctx, recoveryCall)
	if err != nil || got.Text != response.Text || recoveryProvider.calls.Load() != 1 {
		t.Fatalf("controlled model attempt did not deliver result: response=%+v calls=%d err=%v", got, recoveryProvider.calls.Load(), err)
	}
	got, err = recoveryGateway.Complete(ctx, recoveryCall)
	if err != nil || got.Text != response.Text || recoveryProvider.calls.Load() != 1 {
		t.Fatalf("settled recovery response was not replayed without another provider call: response=%+v calls=%d err=%v",
			got, recoveryProvider.calls.Load(), err)
	}
	var failedLedgerCount, recoveryLedgerCount, recoveredResponseCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2),
(SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$3),
(SELECT count(*) FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$3)`,
		fence.InitiatorID, failedCall.CallID, intent.RecoveryCallID).Scan(&failedLedgerCount, &recoveryLedgerCount, &recoveredResponseCount); err != nil ||
		failedLedgerCount != 1 || recoveryLedgerCount != 1 || recoveredResponseCount != 1 {
		t.Fatalf("failure/recovery ledgers were reused or response was not persisted: failed=%d recovery=%d responses=%d err=%v",
			failedLedgerCount, recoveryLedgerCount, recoveredResponseCount, err)
	}
}

func TestPostgresTaskModelRecoveryManualRetryIsAtomicAndIdempotent(t *testing.T) {
	f, failure, call, fence := createTaskModelRecoveryNoDispatch(t)
	ctx := context.Background()
	details, err := f.store.GetTask(ctx, fence.TaskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("verified read-only toolset"))
	command := app.TaskModelRecoveryCommand{Task: details.Task, Failure: failure,
		CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
		CheckpointDigest:          append([]byte(nil), details.Task.CheckpointDigest...), ToolsetDigest: toolsetDigest[:], Auto: false}
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command)
	if err != nil || !accepted || intent.State != task.ModelRecoveryRetryAvailable {
		t.Fatalf("persist manual retry eligibility: intent=%+v accepted=%t err=%v", intent, accepted, err)
	}
	var status, reason string
	if err := f.store.pool.QueryRow(ctx, `SELECT status,waiting_reason FROM tasks WHERE id=$1`, fence.TaskID).Scan(&status, &reason); err != nil ||
		status != string(task.StatusWaitingUser) || reason != taskModelRecoveryAvailableReason {
		t.Fatalf("manual recovery Task state=%s/%s err=%v", status, reason, err)
	}
	retry := app.TaskModelRecoveryRetryCommand{TaskID: fence.TaskID, OwnerID: f.ownerID, InitiatorID: fence.InitiatorID,
		TaskRevision: *details.Task.CurrentRevision, AuthorizationVersion: details.Task.Authorization.Version, EventSequence: intent.EventSequence}
	hash := sha256.Sum256([]byte("exact manual retry request"))
	idempotency := app.IdempotencyRecord{OwnerID: fence.InitiatorID, Operation: "task.model_recovery.retry", Key: "recovery-retry-01",
		RequestHash: hash[:], ResourceID: fence.TaskID}
	type outcome struct {
		result app.TaskModelRecoveryRetryResult
		err    error
	}
	results := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < 2; index++ {
		go func() {
			start.Wait()
			result, err := f.store.RequestTaskModelRecoveryRetry(ctx, idempotency, retry)
			results <- outcome{result: result, err: err}
		}()
	}
	start.Done()
	acceptedCount, replayCount := 0, 0
	for index := 0; index < 2; index++ {
		result := <-results
		if result.err != nil || result.result.Intent.State != task.ModelRecoveryScheduled ||
			result.result.Intent.RecoveryCallID != taskModelRecoveryTestCallID(t, fence, call) {
			t.Fatalf("manual retry result=%+v err=%v", result.result, result.err)
		}
		if result.result.Replayed {
			replayCount++
		} else {
			acceptedCount++
		}
	}
	if acceptedCount != 1 || replayCount != 1 {
		t.Fatalf("concurrent manual retry outcomes: accepted=%d replayed=%d", acceptedCount, replayCount)
	}
	var eventAfterRetry int64
	if err := f.store.pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, fence.TaskID).Scan(&eventAfterRetry); err != nil || eventAfterRetry != intent.EventSequence+1 {
		t.Fatalf("manual retry event sequence=%d want=%d err=%v", eventAfterRetry, intent.EventSequence+1, err)
	}
	var keyCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`,
		fence.InitiatorID, idempotency.Operation, idempotency.Key).Scan(&keyCount); err != nil || keyCount != 1 {
		t.Fatalf("manual retry idempotency records=%d err=%v", keyCount, err)
	}
	if _, err := f.store.RequestTaskModelRecoveryRetry(ctx, idempotency, retry); err != nil {
		t.Fatalf("same-key retry replay after scheduling: %v", err)
	}
	stale := retry
	stale.EventSequence = intent.EventSequence
	other := idempotency
	other.Key = "recovery-retry-02"
	staleHash := sha256.Sum256([]byte("stale manual retry request"))
	other.RequestHash = staleHash[:]
	if _, err := f.store.RequestTaskModelRecoveryRetry(ctx, other, stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale retry event = %v, want conflict", err)
	}
}

func TestPostgresTaskModelRecoveryRejectsChangedToolsetAndCheckpoint(t *testing.T) {
	f, failure, _, fence := createTaskModelRecoveryNoDispatch(t)
	ctx := context.Background()
	details, err := f.store.GetTask(ctx, fence.TaskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest := sha256.Sum256([]byte("toolset A"))
	command := app.TaskModelRecoveryCommand{Task: details.Task, Failure: failure, CheckpointThroughSequence: 0,
		CheckpointDigest: append([]byte(nil), details.Task.CheckpointDigest...), ToolsetDigest: toolsetDigest[:], Auto: true}
	invalidToolset := command
	invalidToolset.ToolsetDigest = nil
	if _, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, invalidToolset); err == nil || accepted {
		t.Fatalf("missing toolset digest accepted: accepted=%t err=%v", accepted, err)
	}
	if _, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command); err != nil || !accepted {
		t.Fatalf("valid recovery schedule: accepted=%t err=%v", accepted, err)
	}
	changedToolset := sha256.Sum256([]byte("toolset B"))
	command.ToolsetDigest = changedToolset[:]
	if _, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command); err == nil || accepted {
		t.Fatalf("changed toolset accepted: accepted=%t err=%v", accepted, err)
	}
	command.ToolsetDigest = toolsetDigest[:]
	changedCheckpoint := sha256.Sum256([]byte("changed checkpoint"))
	command.CheckpointDigest = changedCheckpoint[:]
	if _, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, command); err == nil || accepted {
		t.Fatalf("changed checkpoint accepted: accepted=%t err=%v", accepted, err)
	}
}
