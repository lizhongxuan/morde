package postgres

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestTaskStepRetryReplayBindsPersistedSourceDigest(t *testing.T) {
	request := app.TaskStepRetryRequest{TaskID: "q1234567", StepID: "s1234567", ExpectedTaskRevision: 4, ExpectedAttempt: 1}
	digest := bytes.Repeat([]byte{0xa5}, 32)
	intent := task.StepRetryIntent{TaskID: request.TaskID, OwnerID: "u1234567", SourceStepID: request.StepID,
		TaskRevision: request.ExpectedTaskRevision, Attempt: request.ExpectedAttempt + 1, RequestDigest: digest}
	if !matchesTaskStepRetryReplay(intent, "u1234567", request, digest) {
		t.Fatal("replay did not match the persisted source digest")
	}
	if matchesTaskStepRetryReplay(intent, "u1234567", request, bytes.Repeat([]byte{0x5a}, 32)) {
		t.Fatal("replay matched a digest different from the persisted source")
	}
}

func TestPostgresTaskStepRetryMigration(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	var stepColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='task_steps'
AND column_name IN ('attempt_no','retry_of_step_id','failure_reason','retryable')`).Scan(&stepColumns); err != nil || stepColumns != 4 {
		t.Fatalf("Task step retry columns=%d err=%v; want four migrated columns", stepColumns, err)
	}
	var intentTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.task_step_retry_intents') IS NOT NULL`).Scan(&intentTable); err != nil || !intentTable {
		t.Fatalf("Task step retry intent table exists=%t err=%v", intentTable, err)
	}
	var pendingSourceUnique bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND tablename='task_step_retry_intents'
AND indexname='task_step_retry_intents_one_pending_source' AND indexdef LIKE '%UNIQUE%')`).Scan(&pendingSourceUnique); err != nil || !pendingSourceUnique {
		t.Fatalf("one-pending-retry-per-source unique index exists=%t err=%v", pendingSourceUnique, err)
	}
	var connectorCallLookupIndex bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND tablename='task_mcp_connector_operations'
AND indexname='task_mcp_connector_operations_call_id_lookup_idx')`).Scan(&connectorCallLookupIndex); err != nil || !connectorCallLookupIndex {
		t.Fatalf("connector call-id lookup index exists=%t err=%v", connectorCallLookupIndex, err)
	}
}

func TestPostgresTaskStepRetryConcurrentSameKeyCreatesOneAttempt(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	setAgentTestHostRuntime(t, store, hostValue, "runtime-step-retry-race")
	if err := store.RecordHostReplicaHeartbeat(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatalf("record Host Runtime heartbeat: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "step-retry-race", "retry one failed safe tool call")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Host-bound retry Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "step-retry-race-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim retry Task: found=%t task=%+v err=%v", found, claimed, err)
	}
	supervisorKey := bytes.Repeat([]byte{0x4d}, 32)
	authenticator, err := app.NewAgentStopAuthenticator("step-retry-supervisor", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: "agent-step-retry-race", AgentSessionID: "session-step-retry-race", SupervisorID: "step-retry-supervisor",
			LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
			EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: bytes.Repeat([]byte{0x27}, 32), LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed || started.Fence.AgentGeneration < 1 {
		t.Fatalf("start retry Task Agent: lease=%+v replayed=%t err=%v", started, replayed, err)
	}

	checkpointDigest := hashBytes("step-retry-race-checkpoint")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed',finished_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL,
current_revision=0,desired_revision=0,result='completed with a not-dispatched tool failure',checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, claimed.ID, checkpointDigest); err != nil {
		t.Fatalf("prepare completed Task checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, claimed.ID, started.Fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("prepare Agent Session checkpoint: %v", err)
	}
	sourceStepID := mustID(t, domain.NewStepID)
	requestDigest := hashBytes("file_read:{}")
	if _, err := pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,
detail_state,attempt_no,failure_reason,retryable)
VALUES ($1,$2,1,'tool','failed','Read selected file','The call was not dispatched',$3,0,'tool-call-step-retry-race',
'file_read',$4,'prepare','not_recorded',1,'not_dispatched',true)`,
		sourceStepID, claimed.ID, started.Fence.AgentSessionID, requestDigest); err != nil {
		t.Fatalf("insert source Task step: %v", err)
	}

	// Hold the retry Task update after its idempotency row, step and intent have
	// been written but before commit. This forces the second request to miss the
	// uncommitted idempotency row and then wait on the same Task row lock.
	const advisoryKey int64 = 91012177
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_pause_step_retry_task_update() RETURNS trigger
LANGUAGE plpgsql AS $retry$
BEGIN
  IF OLD.status='completed' AND NEW.status='queued' THEN
    PERFORM pg_advisory_xact_lock(91012177);
  END IF;
  RETURN NEW;
END;
$retry$`); err != nil {
		t.Fatalf("create deterministic retry race trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_pause_step_retry_task_update
BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION test_pause_step_retry_task_update()`); err != nil {
		t.Fatalf("create deterministic retry race trigger: %v", err)
	}
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire advisory lock connection: %v", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey); err != nil {
		t.Fatalf("hold retry race advisory lock: %v", err)
	}
	defer func() { _, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryKey) }()

	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.step.retry", Key: "retry-same-key-race",
		RequestHash: hashBytes("task.step.retry:task:source:revision:0:attempt:1"), RelatedID: sourceStepID}
	makeRequest := func() app.TaskStepRetryRequest {
		attemptStepID := mustID(t, domain.NewStepID)
		return app.TaskStepRetryRequest{TaskID: claimed.ID, StepID: sourceStepID, ExpectedTaskRevision: 0, ExpectedAttempt: 1,
			AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	}
	firstRequest, secondRequest := makeRequest(), makeRequest()
	type retryResult struct {
		value app.TaskStepRetryRecord
		err   error
	}
	results := make(chan retryResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		defer ready.Done()
		value, err := store.RequestTaskStepRetry(ctx, record, firstRequest)
		results <- retryResult{value: value, err: err}
	}()
	waitForRetryLock := func(queryPattern string) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, queryPattern).Scan(&waiting); err != nil {
				t.Fatalf("inspect PostgreSQL retry lock waiters: %v", err)
			}
			if waiting > 0 {
				return
			}
			select {
			case result := <-results:
				t.Fatalf("retry request returned before expected lock wait: value=%+v err=%v", result.value, result.err)
			case <-ctx.Done():
				t.Fatalf("timed out waiting for PostgreSQL retry lock %q", queryPattern)
			case <-ticker.C:
			}
		}
	}
	waitForRetryLock("%UPDATE tasks SET status='queued'%")
	go func() {
		defer ready.Done()
		value, err := store.RequestTaskStepRetry(ctx, record, secondRequest)
		results <- retryResult{value: value, err: err}
	}()
	waitForRetryLock("%SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE%")
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, advisoryKey); err != nil {
		t.Fatalf("release retry race advisory lock: %v", err)
	}
	ready.Wait()
	close(results)
	var values []app.TaskStepRetryRecord
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent same-key Task step retry: %v", result.err)
		}
		values = append(values, result.value)
	}
	if len(values) != 2 || values[0].Intent.AttemptStepID != values[1].Intent.AttemptStepID ||
		values[0].Intent.Attempt != 2 || values[1].Intent.Attempt != 2 || values[0].Intent.SourceStepID != sourceStepID || values[1].Intent.SourceStepID != sourceStepID ||
		values[0].Replayed == values[1].Replayed {
		t.Fatalf("concurrent retry results = %#v; want one new and one replay of the same attempt", values)
	}

	for _, assertion := range []struct {
		name  string
		query string
		args  []any
		want  int
	}{
		{name: "retry Task step", query: `SELECT count(*) FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2`, args: []any{claimed.ID, sourceStepID}, want: 1},
		{name: "retry intent", query: `SELECT count(*) FROM task_step_retry_intents WHERE task_id=$1 AND source_step_id=$2`, args: []any{claimed.ID, sourceStepID}, want: 1},
		{name: "idempotency reservation", query: `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`, args: []any{owner.ID, record.Operation, record.Key}, want: 1},
	} {
		var count int
		if err := pool.QueryRow(ctx, assertion.query, assertion.args...).Scan(&count); err != nil || count != assertion.want {
			t.Errorf("%s count=%d err=%v; want %d", assertion.name, count, err, assertion.want)
		}
	}
	var status, currentStep string
	if err := pool.QueryRow(ctx, `SELECT status,current_step FROM tasks WHERE id=$1`, claimed.ID).Scan(&status, &currentStep); err != nil ||
		status != "queued" || currentStep != values[0].Intent.AttemptStepID {
		t.Fatalf("retry Task status/current step=%q/%q err=%v", status, currentStep, err)
	}
}

func TestPostgresMCPConnectorRateLimitStepRetryWaitsForExactLedgerEvidence(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	setAgentTestHostRuntime(t, store, hostValue, "mcp-rate-limit-retry-runtime")
	if err := store.RecordHostReplicaHeartbeat(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatalf("record Host Runtime heartbeat: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "mcp-rate-limit-step-retry", "retry an explicitly terminal MCP rate limit")
	submission.Task.ExecutionLocation = "server"
	createdTask, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit MCP retry Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "mcp-rate-limit-retry-worker", time.Minute)
	if err != nil || !found || claimed.ID != createdTask.Task.ID {
		t.Fatalf("claim MCP retry Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}
	supervisorKey := bytes.Repeat([]byte{0x4d}, 32)
	authenticator, err := app.NewAgentStopAuthenticator("mcp-rate-limit-retry-supervisor", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: "agent-mcp-rate-limit-retry", AgentSessionID: "session-mcp-rate-limit-retry",
			SupervisorID: "mcp-rate-limit-retry-supervisor", LeaseOwner: claimed.Lease.Owner,
			LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
			EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: bytes.Repeat([]byte{0x27}, 32), LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed {
		t.Fatalf("start MCP retry Agent: replayed=%t err=%v", replayed, err)
	}
	f := selectedMCPAgentFixture{route: routeActionsTestFixture{store: store, ownerID: owner.ID}, task: claimed, fence: started.Fence}
	const connectorID = "k1234567"
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, f.task.ID).Scan(&revision); err != nil {
		t.Fatalf("read Task revision: %v", err)
	}
	checkpointDigest := hashBytes("mcp-rate-limit-retry-checkpoint")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='failed',finished_at=clock_timestamp(),result='connector rate limit',
lease_owner='',lease_expires_at=NULL,current_revision=$2,desired_revision=$2,checkpoint_through_seq=0,checkpoint_digest=$3 WHERE id=$1`,
		f.task.ID, revision, checkpointDigest); err != nil {
		t.Fatalf("prepare failed Task checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, f.task.ID, f.fence.AgentSessionID, checkpointDigest); err != nil {
		t.Fatalf("prepare Agent Session checkpoint: %v", err)
	}

	sourceStepID := mustID(t, domain.NewStepID)
	callID := "mcp-rate-limit-call-001"
	requestDigest := hashBytes("mcp_call:{\"query\":\"synthetic\"}")
	if _, err := f.route.store.pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,
detail_state,attempt_no,failure_reason,retryable)
VALUES ($1,$2,1,'tool','failed','Call selected connector','Connector was rate limited',$3,$4,$5,'mcp_call',$6,'execute',
'not_recorded',1,'connector_rate_limited',true)`, sourceStepID, f.task.ID, f.fence.AgentSessionID, revision, callID, requestDigest); err != nil {
		t.Fatalf("insert failed connector Task step: %v", err)
	}
	result := `{"is_error":true,"structured_content":{"code":"connector_rate_limited","terminal_for_attempt":true,"retry_after_seconds":300}}`
	if _, err := pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,result)
VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,'',0,$10,$11,$12,'mcp_call',$13,'failed',$14)`,
		mustID(t, domain.NewOperationID), f.task.ID, f.route.ownerID, connectorID, int64(1), int64(0), int64(1), f.task.Lease.Version, f.task.HostID, revision,
		callID, "rate-limit-source-operation", requestDigest, result); err != nil {
		t.Fatalf("insert failed connector operation evidence: %v", err)
	}

	details, err := f.route.store.GetTask(ctx, f.task.ID, f.route.ownerID)
	if err != nil {
		t.Fatalf("read failed Task: %v", err)
	}
	var projected *task.Step
	for i := range details.Steps {
		if details.Steps[i].ID == sourceStepID {
			projected = &details.Steps[i]
			break
		}
	}
	if projected == nil || projected.Retryable || projected.RetryAfterAt == nil || !projected.RetryAfterAt.After(time.Now()) {
		t.Fatalf("step retry projection before retry-after = %+v; want disabled with a future retry time", projected)
	}

	attemptStepID := mustID(t, domain.NewStepID)
	record := app.IdempotencyRecord{OwnerID: f.route.ownerID, Operation: "task.step.retry", Key: "mcp-retry-after-window",
		RequestHash: hashBytes("mcp-retry-after-window"), RelatedID: sourceStepID}
	request := app.TaskStepRetryRequest{TaskID: f.task.ID, StepID: sourceStepID, ExpectedTaskRevision: revision, ExpectedAttempt: 1,
		AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	if _, err := f.route.store.RequestTaskStepRetry(ctx, record, request); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("retry during Retry-After window err=%v; want conflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_mcp_connector_operations SET updated_at=clock_timestamp()-interval '6 minutes'
WHERE task_id=$1 AND call_id=$2`, f.task.ID, callID); err != nil {
		t.Fatalf("expire connector Retry-After window: %v", err)
	}
	details, err = f.route.store.GetTask(ctx, f.task.ID, f.route.ownerID)
	if err != nil {
		t.Fatalf("read Task after Retry-After window: %v", err)
	}
	for _, step := range details.Steps {
		if step.ID == sourceStepID && (!step.Retryable || step.RetryAfterAt == nil || step.RetryAfterAt.After(time.Now())) {
			t.Fatalf("step retry projection after Retry-After window = %+v; want enabled with an elapsed retry time", step)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,result)
VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,'',0,$10,$11,$12,'mcp_call',$13,'failed',$14)`,
		mustID(t, domain.NewOperationID), f.task.ID, f.route.ownerID, connectorID, int64(1), int64(0), int64(1), f.task.Lease.Version, f.task.HostID, revision,
		callID, "duplicate-call-id-evidence", requestDigest, result); err != nil {
		t.Fatalf("insert duplicate call-id evidence: %v", err)
	}
	if _, err := f.route.store.RequestTaskStepRetry(ctx, record, request); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("retry with duplicate ledger evidence err=%v; want conflict", err)
	}
	details, err = f.route.store.GetTask(ctx, f.task.ID, f.route.ownerID)
	if err != nil {
		t.Fatalf("read Task with duplicate call-id evidence: %v", err)
	}
	for _, step := range details.Steps {
		if step.ID == sourceStepID && step.Retryable {
			t.Fatalf("duplicate call-id evidence exposed an enabled retry: %+v", step)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM task_mcp_connector_operations WHERE task_id=$1 AND operation_key='duplicate-call-id-evidence'`, f.task.ID); err != nil {
		t.Fatalf("remove duplicate call-id evidence: %v", err)
	}

	unknownCallID := "mcp-unknown-unrelated-001"
	if _, err := pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status)
VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,'',0,$10,$11,$12,'mcp_call',$13,'unknown')`,
		mustID(t, domain.NewOperationID), f.task.ID, f.route.ownerID, connectorID, int64(1), int64(0), int64(1), f.task.Lease.Version, f.task.HostID, revision,
		unknownCallID, "unrelated-unknown-operation", hashBytes("unrelated request")); err != nil {
		t.Fatalf("insert unrelated unknown operation: %v", err)
	}
	if _, err := f.route.store.RequestTaskStepRetry(ctx, record, request); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("retry with unrelated unknown operation err=%v; want conflict", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`, f.task.ID, unknownCallID); err != nil {
		t.Fatalf("remove unrelated unknown operation: %v", err)
	}

	created, err := f.route.store.RequestTaskStepRetry(ctx, record, request)
	if err != nil || created.Replayed || created.Intent.AttemptStepID != attemptStepID || created.Intent.RetryCallID != request.RetryCallID {
		t.Fatalf("create verified connector retry: intent=%+v replayed=%t err=%v", created.Intent, created.Replayed, err)
	}
	replayResult, err := f.route.store.RequestTaskStepRetry(ctx, record, request)
	if err != nil || !replayResult.Replayed || replayResult.Intent.AttemptStepID != created.Intent.AttemptStepID {
		t.Fatalf("replay connector retry: intent=%+v replayed=%t err=%v", replayResult.Intent, replayResult.Replayed, err)
	}
	details, err = f.route.store.GetTask(ctx, f.task.ID, f.route.ownerID)
	if err != nil {
		t.Fatalf("read retried Task: %v", err)
	}
	if details.Task.Status != task.StatusQueued || details.Task.CurrentStep != attemptStepID {
		t.Fatalf("Task status/current step=%q/%q; want queued retry attempt %q", details.Task.Status, details.Task.CurrentStep, attemptStepID)
	}
	var sourceFound, attemptFound bool
	for _, step := range details.Steps {
		if step.ID == sourceStepID {
			sourceFound = true
			if step.Status != task.StepFailed || step.RetryOfStepID != "" {
				t.Fatalf("source failure was overwritten: %+v", step)
			}
		}
		if step.ID == attemptStepID {
			attemptFound = true
			if step.Status != task.StepQueued || step.RetryOfStepID != sourceStepID || step.Attempt != 2 {
				t.Fatalf("retry attempt lost its source binding: %+v", step)
			}
		}
	}
	if !sourceFound || !attemptFound {
		t.Fatalf("source/retry step present=%t/%t", sourceFound, attemptFound)
	}
	var retrySteps, intents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2`, f.task.ID, sourceStepID).Scan(&retrySteps); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_step_retry_intents WHERE task_id=$1 AND source_step_id=$2`, f.task.ID, sourceStepID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if retrySteps != 1 || intents != 1 {
		t.Fatalf("retry step/intent count=%d/%d; want one each", retrySteps, intents)
	}
}
