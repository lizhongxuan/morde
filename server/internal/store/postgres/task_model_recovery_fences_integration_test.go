package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskModelRecoveryFenceFixture struct {
	store     *Store
	taskID    string
	ownerID   string
	intent    task.ModelRecoveryIntent
	fence     app.AgentInstanceFence
	stopKey   []byte
	stopNonce []byte
}

func newTaskModelRecoveryFenceFixture(t *testing.T) taskModelRecoveryFenceFixture {
	t.Helper()
	ctx := context.Background()
	f, failure, call, failedFence := createTaskModelRecoveryNoDispatch(t)
	details, err := f.store.GetTask(ctx, failedFence.TaskID, failedFence.OwnerID)
	if err != nil {
		t.Fatalf("read Task before scheduling fenced recovery: %v", err)
	}
	toolsetDigest := sha256.Sum256([]byte("verified read-only recovery toolset"))
	intent, accepted, err := f.store.ScheduleTaskModelRecovery(ctx, app.TaskModelRecoveryCommand{
		Task: details.Task, Failure: failure, CheckpointThroughSequence: details.Task.CheckpointThroughSequence,
		CheckpointDigest: append([]byte(nil), details.Task.CheckpointDigest...), ToolsetDigest: toolsetDigest[:], Auto: true,
	})
	if err != nil || !accepted || intent.State != task.ModelRecoveryScheduled {
		t.Fatalf("schedule fenced model recovery: intent=%+v accepted=%t err=%v", intent, accepted, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "recovery-fence-worker", time.Minute)
	if err != nil || !found || claimed.ID != details.Task.ID || claimed.ModelRecovery == nil ||
		claimed.ModelRecovery.State != task.ModelRecoveryScheduled {
		t.Fatalf("claim scheduled fenced recovery: task=%+v found=%t err=%v", claimed, found, err)
	}

	stopKey := make([]byte, 32)
	stopKey[0] = 0x73
	authenticator, err := app.NewAgentStopAuthenticator("revision-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	stopNonce := make([]byte, 32)
	stopNonce[0] = 0x74
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "agent-recovery-fence", AgentSessionID: claimed.AgentSessionID,
		SupervisorID: "revision-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: claimed.AgentEngineVersion,
		ProfileVersion: claimed.AgentProfileVersion,
	}, SupervisorNonce: stopNonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed || started.Fence.AgentGeneration <= intent.AgentGeneration {
		t.Fatalf("start fenced recovery Agent: lease=%+v replayed=%t err=%v", started, replayed, err)
	}
	if call.Task == nil || call.Task.TaskID != intent.TaskID {
		t.Fatalf("failed model call is not bound to recovery Task: call=%+v intent=%+v", call, intent)
	}
	return taskModelRecoveryFenceFixture{store: f.store, taskID: intent.TaskID,
		ownerID: intent.OwnerID, intent: intent, fence: started.Fence, stopKey: stopKey, stopNonce: stopNonce}
}

func (f taskModelRecoveryFenceFixture) assertNotAttempting(t *testing.T) {
	t.Helper()
	intent, found, err := f.store.GetTaskModelRecovery(context.Background(), f.taskID, f.ownerID, f.intent.TaskRevision)
	if err != nil || !found || intent.State != task.ModelRecoveryScheduled || intent.AttemptStartedAt != nil {
		t.Fatalf("rejected recovery changed durable attempt state: intent=%+v found=%t err=%v", intent, found, err)
	}
	var ledgerRows, responseRows int
	if err := f.store.pool.QueryRow(context.Background(), `SELECT
(SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2),
(SELECT count(*) FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2)`,
		f.ownerID, f.intent.RecoveryCallID).Scan(&ledgerRows, &responseRows); err != nil || ledgerRows != 0 || responseRows != 0 {
		t.Fatalf("rejected recovery dispatched or persisted a response: ledger=%d responses=%d err=%v", ledgerRows, responseRows, err)
	}
}

func TestPostgresTaskModelRecoveryBeginRejectsRevokedAuthorizationVersion(t *testing.T) {
	f := newTaskModelRecoveryFenceFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET auth_version=auth_version+1 WHERE id=$1 AND auth_version=$2`,
		f.taskID, f.intent.AuthorizationVersion); err != nil {
		t.Fatalf("revoke the scheduled recovery authorization version: %v", err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, f.intent, f.fence); err == nil {
		t.Fatal("recovery began after its authorization version was revoked")
	}
	f.assertNotAttempting(t)
}

func TestPostgresTaskModelRecoveryBeginRejectsChangedPinnedCheckpointHistory(t *testing.T) {
	f := newTaskModelRecoveryFenceFixture(t)
	ctx := context.Background()
	changedDigest := sha256.Sum256([]byte("test-only replacement for the scheduled checkpoint digest"))
	updated, err := f.store.pool.Exec(ctx, `UPDATE task_agent_checkpoints SET digest=$4
WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND agent_root_session_id=$3 AND through_seq=$5`,
		f.taskID, f.ownerID, f.intent.AgentSessionID, changedDigest[:], f.intent.CheckpointThroughSequence)
	if err != nil || updated.RowsAffected() != 1 {
		t.Fatalf("replace only the persisted pinned checkpoint digest for this negative fence fixture: rows=%d err=%v",
			updated.RowsAffected(), err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, f.intent, f.fence); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("recovery after pinned checkpoint history changed = %v, want conflict", err)
	}
	f.assertNotAttempting(t)
}

func TestPostgresTaskModelRecoveryBeginRejectsCancelledTask(t *testing.T) {
	f := newTaskModelRecoveryFenceFixture(t)
	ctx := context.Background()
	cancelID := mustID(t, domain.NewTaskActionID)
	record := taskActionIdempotency(f.ownerID, task.ActionCancel, "model-recovery-cancel-fence", f.taskID,
		task.StatusRunning, cancelID)
	requested, err := f.store.RequestTaskAction(ctx, record, f.taskID, task.ActionCancel, task.StatusRunning, nil, cancelID)
	if err != nil || requested.Task.Status != task.StatusCancelling {
		t.Fatalf("request cancellation of a claimed recovery Task: task=%+v err=%v", requested.Task, err)
	}
	confirmTaskModelRecoveryAgentStop(t, f.store, f.stopKey, f.fence, f.stopNonce)
	stopClaim, found, err := f.store.ClaimNextTaskStop(ctx, "recovery-cancel-stop-worker", time.Minute)
	if err != nil || !found || stopClaim.Request.ID != cancelID {
		t.Fatalf("claim recovery Task cancellation: claim=%+v found=%t err=%v", stopClaim, found, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, cancelID); err != nil {
		t.Fatalf("confirm the isolated cancellation fixture safe-after point: %v", err)
	}
	cancelled, err := f.store.CompleteTaskStop(ctx, f.taskID, cancelID, "recovery-cancel-stop-worker", stopClaim.Request.LeaseVersion)
	if err != nil || cancelled.Status != task.StatusCancelled {
		t.Fatalf("complete recovery Task cancellation: task=%+v err=%v", cancelled, err)
	}
	if err := f.store.BeginTaskModelRecoveryAttempt(ctx, f.intent, f.fence); err == nil {
		t.Fatal("recovery began after the Task was cancelled")
	}
	f.assertNotAttempting(t)
}
