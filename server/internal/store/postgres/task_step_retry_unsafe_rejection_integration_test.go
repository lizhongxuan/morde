package postgres

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

type unsafeMCPRetryRequestFixture struct {
	ownerID     string
	task        task.Task
	sourceStep  string
	callID      string
	request     app.TaskStepRetryRequest
	record      app.IdempotencyRecord
	terminalRaw string
}

type unsafeRetryAdmissionSnapshot struct {
	taskStatus           string
	currentStep          string
	eventSequence        int64
	currentRevision      int64
	stepCount            int64
	retryStepCount       int64
	retryIntentCount     int64
	retryIdempotency     int64
	mcpOperationCount    int64
	mcpDispatchedCount   int64
	taskOperationCount   int64
	taskDispatchedCount  int64
	modelCallCount       int64
	modelDispatchedCount int64
}

func TestPostgresMCPRateLimitRetryRejectsUnsafeCurrentState(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	setAgentTestHostRuntime(t, store, hostValue, "mcp-rate-limit-retry-unsafe-runtime")
	if err := store.RecordHostReplicaHeartbeat(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatalf("record Host Runtime heartbeat: %v", err)
	}
	supervisorKey := bytes.Repeat([]byte{0x4d}, 32)
	const supervisorID = "mcp-rate-limit-retry-unsafe-supervisor"
	authenticator, err := app.NewAgentStopAuthenticator(supervisorID, supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*testing.T, unsafeMCPRetryRequestFixture)
	}{
		{name: "budget exhausted", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE tasks SET usage_tool_calls=budget_tool_calls WHERE id=$1`, f.task.ID); err != nil {
				t.Fatalf("exhaust Task tool-call budget: %v", err)
			}
		}},
		{name: "pending approval", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,runtime_generation,runtime_instance_id,control_version,status)
VALUES ($1,$2,1,'approval','Confirm connector call','Synthetic connector','Approval remains pending','Connector action',$3,'mcp_call',$4,0,'retry-test',0,'pending')`,
				mustID(t, domain.NewPendingRequestID), f.task.ID, f.callID, hashBytes("pending-approval")); err != nil {
				t.Fatalf("insert pending approval: %v", err)
			}
		}},
		{name: "pending clarification", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `INSERT INTO task_clarification_requests
(id,task_id,version,model_step_id,call_id,question,status)
VALUES ($1,$2,1,$3,$4,'Clarification remains pending','pending')`,
				mustID(t, domain.NewPendingRequestID), f.task.ID, f.sourceStep, "clarification-"+f.callID); err != nil {
				t.Fatalf("insert pending clarification: %v", err)
			}
		}},
		{name: "cancelled Task", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE tasks SET status='cancelled',finished_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL WHERE id=$1`, f.task.ID); err != nil {
				t.Fatalf("cancel Task before retry: %v", err)
			}
		}},
		{name: "stale Task revision", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE tasks SET current_revision=current_revision+1,desired_revision=desired_revision+1 WHERE id=$1`, f.task.ID); err != nil {
				t.Fatalf("advance Task revision past source failure: %v", err)
			}
		}},
		{name: "revoked authorization", mutate: func(t *testing.T, f unsafeMCPRetryRequestFixture) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, f.task.Authorization.TokenID, f.ownerID); err != nil {
				t.Fatalf("revoke Task authorization token: %v", err)
			}
		}},
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := seedUnsafeMCPRetryFixture(t, ctx, store, owner.ID, hostValue, instances, supervisorKey, index)
			tc.mutate(t, fixture)
			before := snapshotUnsafeRetryAdmission(t, ctx, pool, fixture)

			if _, err := store.RequestTaskStepRetry(ctx, fixture.record, fixture.request); err == nil {
				t.Fatal("RequestTaskStepRetry accepted unsafe Task state")
			}

			after := snapshotUnsafeRetryAdmission(t, ctx, pool, fixture)
			if after != before {
				t.Fatalf("rejected retry changed persistent state: before=%+v after=%+v", before, after)
			}
			var sourceStatus, sourceResult string
			if err := pool.QueryRow(ctx, `SELECT status,result FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`, fixture.task.ID, fixture.callID).
				Scan(&sourceStatus, &sourceResult); err != nil || sourceStatus != string(task.OperationFailed) || sourceResult != fixture.terminalRaw {
				t.Fatalf("source operation changed after rejection: status=%q result=%q err=%v", sourceStatus, sourceResult, err)
			}
		})
	}
}

func seedUnsafeMCPRetryFixture(t *testing.T, ctx context.Context, store *Store, ownerID string, hostValue host.Host,
	instances *app.AgentInstanceService, supervisorKey []byte, index int) unsafeMCPRetryRequestFixture {
	t.Helper()
	suffix := fmt.Sprintf("%02d", index+1)
	submission := newTestTaskSubmission(t, ownerID, hostValue.ID, "mcp-rate-limit-unsafe-"+suffix,
		"retry an explicitly terminal MCP rate limit")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit MCP retry Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "mcp-rate-limit-unsafe-worker-"+suffix, time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim MCP retry Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}
	nonce := bytes.Repeat([]byte{byte(index + 1)}, 32)
	fence := app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "agent-mcp-rate-limit-unsafe-" + suffix, AgentSessionID: "session-mcp-rate-limit-unsafe-" + suffix,
		SupervisorID: "mcp-rate-limit-retry-unsafe-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: fence, SupervisorNonce: nonce, LeaseDuration: 30 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start MCP retry Agent: replayed=%t err=%v", replayed, err)
	}
	claimed = startedTaskForUnsafeRetry(claimed, started.Fence)

	var revision int64
	if err := store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, claimed.ID).Scan(&revision); err != nil {
		t.Fatalf("read Task revision: %v", err)
	}
	checkpointDigest := hashBytes("mcp-rate-limit-retry-unsafe-checkpoint-" + suffix)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='failed',finished_at=clock_timestamp(),result='connector rate limit',
lease_owner='',lease_expires_at=NULL,current_revision=$2,desired_revision=$2,checkpoint_through_seq=0,checkpoint_digest=$3 WHERE id=$1`,
		claimed.ID, revision, checkpointDigest); err != nil {
		t.Fatalf("prepare failed Task checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, claimed.ID, started.Fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("prepare Agent Session checkpoint: %v", err)
	}
	if _, err := instances.ConfirmStopped(ctx, signAgentLifecycleReceipt(t, supervisorKey, started.Fence, nonce)); err != nil {
		t.Fatalf("confirm previous MCP Agent stopped: %v", err)
	}

	const connectorID = "k1234567"
	sourceStepID := mustID(t, domain.NewStepID)
	callID := "mcp-rate-limit-unsafe-call-" + suffix
	requestDigest := hashBytes(`mcp_call:{"query":"synthetic"}`)
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,
detail_state,attempt_no,failure_reason,retryable)
VALUES ($1,$2,1,'tool','failed','Call selected connector','Connector was rate limited',$3,$4,$5,'mcp_call',$6,'execute',
'not_recorded',1,'connector_rate_limited',true)`, sourceStepID, claimed.ID, started.Fence.AgentSessionID, revision, callID, requestDigest); err != nil {
		t.Fatalf("insert failed connector Task step: %v", err)
	}
	terminalRaw := `{"is_error":true,"structured_content":{"code":"connector_rate_limited","terminal_for_attempt":true,"retry_after_seconds":300}}`
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,result)
VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,'',0,$10,$11,$12,'mcp_call',$13,'failed',$14)`,
		mustID(t, domain.NewOperationID), claimed.ID, ownerID, connectorID, int64(1), int64(0), int64(1), started.Fence.LeaseVersion,
		claimed.HostID, revision, callID, "rate-limit-source-operation-"+suffix, requestDigest, terminalRaw); err != nil {
		t.Fatalf("insert failed connector operation evidence: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET updated_at=clock_timestamp()-interval '6 minutes'
WHERE task_id=$1 AND call_id=$2`, claimed.ID, callID); err != nil {
		t.Fatalf("expire connector Retry-After window: %v", err)
	}

	attemptStepID := mustID(t, domain.NewStepID)
	record := app.IdempotencyRecord{OwnerID: ownerID, Operation: "task.step.retry", Key: "mcp-retry-unsafe-" + suffix,
		RequestHash: hashBytes("task.step.retry:mcp-retry-unsafe-" + suffix), RelatedID: sourceStepID}
	request := app.TaskStepRetryRequest{TaskID: claimed.ID, StepID: sourceStepID, ExpectedTaskRevision: revision, ExpectedAttempt: 1,
		AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	return unsafeMCPRetryRequestFixture{ownerID: ownerID, task: claimed, sourceStep: sourceStepID,
		callID: callID, request: request, record: record, terminalRaw: terminalRaw}
}

func startedTaskForUnsafeRetry(claimed task.Task, fence app.AgentInstanceFence) task.Task {
	claimed.AgentInstanceID, claimed.AgentGeneration, claimed.AgentSessionID = fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID
	claimed.AgentEngineVersion, claimed.AgentProfileVersion = fence.EngineVersion, fence.ProfileVersion
	return claimed
}

func snapshotUnsafeRetryAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	fixture unsafeMCPRetryRequestFixture) unsafeRetryAdmissionSnapshot {
	t.Helper()
	var value unsafeRetryAdmissionSnapshot
	if err := pool.QueryRow(ctx, `SELECT status,COALESCE(current_step,''),event_seq,current_revision FROM tasks WHERE id=$1`, fixture.task.ID).
		Scan(&value.taskStatus, &value.currentStep, &value.eventSequence, &value.currentRevision); err != nil {
		t.Fatalf("read Task admission snapshot: %v", err)
	}
	queries := []struct {
		dest  *int64
		query string
		args  []any
	}{
		{&value.stepCount, `SELECT count(*) FROM task_steps WHERE task_id=$1`, []any{fixture.task.ID}},
		{&value.retryStepCount, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2`, []any{fixture.task.ID, fixture.sourceStep}},
		{&value.retryIntentCount, `SELECT count(*) FROM task_step_retry_intents WHERE task_id=$1 AND source_step_id=$2`, []any{fixture.task.ID, fixture.sourceStep}},
		{&value.retryIdempotency, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`, []any{fixture.ownerID, fixture.record.Operation, fixture.record.Key}},
		{&value.mcpOperationCount, `SELECT count(*) FROM task_mcp_connector_operations WHERE task_id=$1`, []any{fixture.task.ID}},
		{&value.mcpDispatchedCount, `SELECT count(*) FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched')`, []any{fixture.task.ID}},
		{&value.taskOperationCount, `SELECT count(*) FROM task_operations WHERE task_id=$1`, []any{fixture.task.ID}},
		{&value.taskDispatchedCount, `SELECT count(*) FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched')`, []any{fixture.task.ID}},
		{&value.modelCallCount, `SELECT count(*) FROM model_call_ledger WHERE task_id=$1 AND source='task'`, []any{fixture.task.ID}},
		{&value.modelDispatchedCount, `SELECT count(*) FROM model_call_ledger WHERE task_id=$1 AND source='task' AND state IN ('dispatched','unknown')`, []any{fixture.task.ID}},
	}
	for _, query := range queries {
		if err := pool.QueryRow(ctx, query.query, query.args...).Scan(query.dest); err != nil {
			t.Fatalf("read retry admission counts: %v", err)
		}
	}
	return value
}
