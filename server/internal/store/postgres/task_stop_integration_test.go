package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskPauseResumeStopLifecycle(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "stop-pause-task", "pause this task"))
	if err != nil {
		t.Fatal(err)
	}

	pauseID := mustID(t, domain.NewTaskActionID)
	pauseRecord := taskActionIdempotency(owner.ID, task.ActionPause, "pause-action-key", submitted.Task.ID, task.StatusQueued, pauseID)
	pausedRequest, err := store.RequestTaskAction(ctx, pauseRecord, submitted.Task.ID, task.ActionPause, task.StatusQueued, pauseID)
	if err != nil || pausedRequest.Task.Status != task.StatusPausing || pausedRequest.Action != task.ActionPause || len(pausedRequest.UnsettledOperationIDs) != 0 {
		t.Fatalf("pause request = %#v, err=%v", pausedRequest, err)
	}
	if pausedRequest.Task.Lease.Owner != "" || pausedRequest.Task.Lease.Version != submitted.Task.Lease.Version+1 {
		t.Fatalf("pause did not fence the task lease: %#v", pausedRequest.Task.Lease)
	}

	claim, found, err := store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != pauseID || claim.Request.Status != task.StopRequestPending || claim.Task.Status != task.StatusPausing || len(claim.Operations) != 0 {
		t.Fatalf("pause stop claim = %#v, found=%t, err=%v", claim, found, err)
	}
	paused, err := store.CompleteTaskStop(ctx, submitted.Task.ID, pauseID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || paused.Status != task.StatusPaused {
		t.Fatalf("complete pause = %#v, err=%v", paused, err)
	}
	details, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || details.StopRequest == nil || details.StopRequest.ID != pauseID || details.StopRequest.Status != task.StopRequestCompleted {
		t.Fatalf("paused task details = %#v, err=%v", details.StopRequest, err)
	}
	stalePauseID := mustID(t, domain.NewTaskActionID)
	stalePauseRecord := taskActionIdempotency(owner.ID, task.ActionPause, "stale-pause-key", submitted.Task.ID, task.StatusQueued, stalePauseID)
	if _, err := store.RequestTaskAction(ctx, stalePauseRecord, submitted.Task.ID, task.ActionPause, task.StatusQueued, stalePauseID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale expected task status error = %v", err)
	}

	resumeID := mustID(t, domain.NewTaskActionID)
	resumeRecord := taskActionIdempotency(owner.ID, task.ActionResume, "resume-action-key", submitted.Task.ID, task.StatusPaused, resumeID)
	resumed, err := store.RequestTaskAction(ctx, resumeRecord, submitted.Task.ID, task.ActionResume, task.StatusPaused, resumeID)
	if err != nil || resumed.Task.Status != task.StatusQueued || resumed.Action != task.ActionResume {
		t.Fatalf("resume request = %#v, err=%v", resumed, err)
	}

	// A retry receives a fresh generated request ID from the service, while the
	// idempotency row must retain and replay the original stop ID.
	retryID := mustID(t, domain.NewTaskActionID)
	retryRecord := pauseRecord
	retryRecord.ResourceID = retryID
	replayed, err := store.RequestTaskAction(ctx, retryRecord, submitted.Task.ID, task.ActionPause, task.StatusQueued, retryID)
	if err != nil || replayed.Action != task.ActionPause || replayed.Task.Status != task.StatusQueued {
		t.Fatalf("pause idempotent replay after resume = %#v, err=%v", replayed, err)
	}
	var stopRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_stop_requests WHERE task_id=$1`, submitted.Task.ID).Scan(&stopRows); err != nil || stopRows != 1 {
		t.Fatalf("stop request rows after replay = %d, err=%v", stopRows, err)
	}
}

func TestPostgresTaskStopRecoversDispatchedMCPConnectorOperation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "stop-recover-mcp-operation")
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("stop-recover-dispatched", "update"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare MCP operation = %#v replay=%t err=%v", prepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}

	stopID := mustID(t, domain.NewTaskActionID)
	stopRecord := taskActionIdempotency(owner.ID, task.ActionCancel, "stop-recover-mcp-action", fixture.running.ID, task.StatusRunning, stopID)
	requested, err := store.RequestTaskAction(ctx, stopRecord, fixture.running.ID, task.ActionCancel, task.StatusRunning, stopID)
	if err != nil || requested.Task.Status != task.StatusCancelling || len(requested.UnsettledOperationIDs) != 1 {
		t.Fatalf("stop request = %#v err=%v", requested, err)
	}
	stopFenced, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || stopFenced.Status != task.OperationDispatched || !stopFenced.CancelRequested {
		t.Fatalf("dispatched operation after stop request = %#v err=%v", stopFenced, err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, `{"late":true}`); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("late MCP result after stop request was accepted: %v", err)
	}

	claim, found, err := store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != stopID {
		t.Fatalf("claim stop = %#v found=%t err=%v", claim, found, err)
	}
	stillDispatched, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || stillDispatched.Status != task.OperationDispatched || !stillDispatched.CancelRequested {
		t.Fatalf("dispatched operation before safe-after recovery = %#v err=%v", stillDispatched, err)
	}
	if _, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stop completed before the safe-after barrier: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || cancelled.Status != task.StatusCancelled {
		t.Fatalf("complete stop with unknown MCP operation = %#v err=%v", cancelled, err)
	}
	unknownAfterStop, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || unknownAfterStop.Status != task.OperationUnknown || !unknownAfterStop.CancelRequested || unknownAfterStop.Result == "" {
		t.Fatalf("unknown operation audit record after stop = %#v err=%v", unknownAfterStop, err)
	}
}

func TestPostgresTaskStopRecoversRevokedDispatchedMCPConnectorOperation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "stop-recover-revoked-mcp")
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("stop-recover-revoked", "update"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare MCP operation = %#v replay=%t err=%v", prepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevokeTaskMCPConnectorGrant(ctx, owner.ID, fixture.running.ID, fixture.connectorID, 1); err != nil {
		t.Fatalf("revoke MCP connector grant: %v", err)
	}
	revoked, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || revoked.Status != task.OperationDispatched || !revoked.CancelRequested || revoked.Result == "" {
		t.Fatalf("dispatched operation after grant revocation = %#v err=%v", revoked, err)
	}

	stopID := mustID(t, domain.NewTaskActionID)
	stopRecord := taskActionIdempotency(owner.ID, task.ActionPause, "stop-recover-revoked-action", fixture.running.ID, task.StatusRunning, stopID)
	requested, err := store.RequestTaskAction(ctx, stopRecord, fixture.running.ID, task.ActionPause, task.StatusRunning, stopID)
	if err != nil || requested.Task.Status != task.StatusPausing || len(requested.UnsettledOperationIDs) != 1 {
		t.Fatalf("pause request = %#v err=%v", requested, err)
	}
	claim, found, err := store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != stopID {
		t.Fatalf("claim stop = %#v found=%t err=%v", claim, found, err)
	}
	stillDispatched, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || stillDispatched.Status != task.OperationDispatched || !stillDispatched.CancelRequested || stillDispatched.Result != revoked.Result {
		t.Fatalf("revoked dispatched operation before safe-after recovery = %#v err=%v", stillDispatched, err)
	}
	if _, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stop completed before the safe-after barrier: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopID); err != nil {
		t.Fatal(err)
	}
	paused, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || paused.Status != task.StatusPaused {
		t.Fatalf("complete pause with revoked MCP operation outcome unknown = %#v err=%v", paused, err)
	}
	unknown, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || unknown.Status != task.OperationUnknown || !unknown.CancelRequested || unknown.Result != revoked.Result {
		t.Fatalf("revoked unknown operation after stop finalization = %#v err=%v", unknown, err)
	}
}

func TestPostgresTaskStopWaitsForAgentStopReceipt(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed {
		t.Fatalf("start active agent: lease=%#v replayed=%t err=%v", lease, replayed, err)
	}
	cancelID := mustID(t, domain.NewTaskActionID)
	stopRecord := taskActionIdempotency(f.request.Fence.OwnerID, task.ActionCancel, "cancel-agent-key", f.request.Fence.TaskID, task.StatusRunning, cancelID)
	if _, err := f.store.RequestTaskAction(ctx, stopRecord, f.request.Fence.TaskID, task.ActionCancel, task.StatusRunning, cancelID); err != nil {
		t.Fatalf("request task cancellation: %v", err)
	}
	claim, found, err := f.store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != cancelID {
		t.Fatalf("claim task stop: claim=%#v found=%t err=%v", claim, found, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, cancelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, claim.Task.ID, cancelID, "stop-worker", claim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("task stop completed without an Agent receipt: %v", err)
	}
	receipt := signAgentLifecycleReceipt(t, f.key, lease.Fence, f.request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm Agent stop: %v", err)
	}
	cancelled, err := f.store.CompleteTaskStop(ctx, claim.Task.ID, cancelID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || cancelled.Status != task.StatusCancelled {
		t.Fatalf("complete confirmed stop: task=%#v err=%v", cancelled, err)
	}
}

func TestPostgresTaskCancelSettlesOperationsAndPendingApproval(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_stop_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	hostValue.RuntimeInstanceID = "ri_stop_test"
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "stop-cancel-task", "cancel this task"))
	if err != nil {
		t.Fatal(err)
	}
	running, found, err := store.ClaimNextTask(ctx, "task-worker", time.Minute)
	if err != nil || !found || running.ID != submitted.Task.ID {
		t.Fatalf("claim task = %#v, found=%t, err=%v", running, found, err)
	}

	operationKeys := []string{"dispatched", "prepared", "unknown"}
	operationStatuses := []task.OperationStatus{task.OperationDispatched, task.OperationPrepared, task.OperationUnknown}
	operations := make([]task.Operation, 0, len(operationStatuses))
	for i, status := range operationStatuses {
		operation := task.Operation{
			ID: mustID(t, domain.NewOperationID), TaskID: running.ID, Key: operationKeys[i],
			RequestHash: hashBytes("stop-op-" + string(status)), HostID: hostValue.ID,
			WorkspaceID: "w1234567", BindingVersion: 9,
			RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
			ControlVersion: hostValue.ControlVersion, LeaseVersion: running.Lease.Version,
			RuntimeLeaseVersion: running.Lease.Version, Status: task.OperationPrepared,
		}
		created, replayed, err := store.CreateOperation(ctx, operation, "task-worker")
		if err != nil || replayed {
			t.Fatalf("create operation: %#v replay=%t err=%v", created, replayed, err)
		}
		if status != task.OperationPrepared {
			if _, err := store.pool.Exec(ctx, `UPDATE task_operations SET status=$2 WHERE id=$1`, operation.ID, string(status)); err != nil {
				t.Fatal(err)
			}
		}
		operations = append(operations, created)
	}

	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	pending := &task.PendingRequest{
		ID: pendingID, TaskID: running.ID, Version: 1, Kind: "approval", Title: "Approve action",
		Object: "external change", Content: "Approve this action?", Impact: "This changes external state.",
		CallID: "call-stop-test", ToolName: "external_action", RequestHash: hashBytes("pending-stop-test"),
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, Status: task.PendingRequestPending,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: running.ID, LeaseOwner: running.Lease.Owner, LeaseVersion: running.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage:          task.Usage{ActiveSeconds: 2, ToolCalls: 1},
		Steps:          []task.Step{{ID: stepID, TaskID: running.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser, Title: pending.Title, Summary: pending.Content}},
		Event:          task.Event{TaskID: running.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
		PendingRequest: pending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET runtime_lease_safe_after=clock_timestamp()+interval '1 minute' WHERE id=$1`, running.ID); err != nil {
		t.Fatal(err)
	}

	cancelID := mustID(t, domain.NewTaskActionID)
	cancelRecord := taskActionIdempotency(owner.ID, task.ActionCancel, "cancel-action-key", running.ID, task.StatusWaitingUser, cancelID)
	cancelling, err := store.RequestTaskAction(ctx, cancelRecord, running.ID, task.ActionCancel, task.StatusWaitingUser, cancelID)
	if err != nil || cancelling.Task.Status != task.StatusCancelling || cancelling.Task.Lease.Owner != "" || len(cancelling.UnsettledOperationIDs) != 3 {
		t.Fatalf("cancel request = %#v, err=%v", cancelling, err)
	}
	if cancelling.Task.Lease.Version != running.Lease.Version+1 {
		t.Fatalf("cancel did not fence task lease: %#v", cancelling.Task.Lease)
	}
	var pendingStatus, pendingDecision string
	if err := store.pool.QueryRow(ctx, `SELECT status, decision FROM task_pending_requests WHERE id=$1`, pendingID).Scan(&pendingStatus, &pendingDecision); err != nil || pendingStatus != string(task.PendingRequestCancelled) || pendingDecision != "" {
		t.Fatalf("pending request after cancel = %q/%q err=%v", pendingStatus, pendingDecision, err)
	}

	claim, found, err := store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found || claim.Request.ID != cancelID || claim.Task.Status != task.StatusCancelling || len(claim.Operations) != 3 {
		t.Fatalf("cancel stop claim = %#v, found=%t, err=%v", claim, found, err)
	}
	for i, operation := range claim.Operations {
		if operation.ID != operations[i].ID || operation.RuntimeLeaseVersion != running.Lease.Version {
			t.Fatalf("stop operation fence was rewritten: got %#v expected id=%s runtime lease=%d", operation, operations[i].ID, running.Lease.Version)
		}
		if operation.WorkspaceID != "w1234567" || operation.BindingVersion != 9 {
			t.Fatalf("stop recovery lost operation Workspace binding: %#v", operation)
		}
	}
	settlementsByKey := map[string]task.OperationStatus{
		"prepared": task.OperationCancelled, "dispatched": task.OperationSucceeded, "unknown": task.OperationCancelled,
	}
	for _, operation := range claim.Operations {
		if operation.Key == "unknown" {
			continue
		}
		if err := store.SettleTaskStopOperation(ctx, running.ID, cancelID, "stop-worker", claim.Request.LeaseVersion, operation.ID, settlementsByKey[operation.Key], "settled by runtime", false); err != nil {
			t.Fatalf("settle %s: %v", operation.ID, err)
		}
	}
	var unknownOperationID string
	for _, operation := range claim.Operations {
		if operation.Key == "unknown" {
			unknownOperationID = operation.ID
			break
		}
	}
	if err := store.SettleTaskStopOperation(ctx, running.ID, cancelID, "stop-worker", claim.Request.LeaseVersion, unknownOperationID, task.OperationCancelled, "not found", true); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("not-found settlement before safe_after = %v", err)
	}
	if _, err := store.CompleteTaskStop(ctx, running.ID, cancelID, "stop-worker", claim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("cancel finalized before safe_after: %v", err)
	}
	var runtimeLeaseVersion int64
	if err := store.pool.QueryRow(ctx, `SELECT runtime_lease_version FROM task_operations WHERE task_id=$1 ORDER BY operation_key LIMIT 1`, running.ID).Scan(&runtimeLeaseVersion); err != nil || runtimeLeaseVersion != running.Lease.Version {
		t.Fatalf("operation runtime_lease_version = %d, err=%v", runtimeLeaseVersion, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, cancelID); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleTaskStopOperation(ctx, running.ID, cancelID, "stop-worker", claim.Request.LeaseVersion, unknownOperationID, task.OperationCancelled, "not found", true); err != nil {
		t.Fatalf("not-found settlement after database safe_after: %v", err)
	}
	cancelled, err := store.CompleteTaskStop(ctx, running.ID, cancelID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || cancelled.Status != task.StatusCancelled {
		t.Fatalf("complete cancel = %#v, err=%v", cancelled, err)
	}
	details, err := store.GetTask(ctx, running.ID, owner.ID)
	if err != nil || details.StopRequest == nil || details.StopRequest.Status != task.StopRequestCompleted || len(details.StopOperationIDs) != 0 {
		t.Fatalf("cancelled task stop state = %#v operations=%v err=%v", details.StopRequest, details.StopOperationIDs, err)
	}
	for _, operation := range claim.Operations {
		stored, err := store.GetOperation(ctx, owner.ID, running.ID, operation.Key)
		if err != nil || stored.Status != settlementsByKey[operation.Key] || stored.RuntimeLeaseVersion != running.Lease.Version {
			t.Fatalf("stored settled operation = %#v err=%v", stored, err)
		}
	}
}

func TestPostgresPendingResponseAdvancesTaskLeaseVersion(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "pending-version-task", "wait for approval"))
	if err != nil {
		t.Fatal(err)
	}
	pendingID := mustID(t, domain.NewPendingRequestID)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', lease_version=8, waiting_reason='approval_required' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash,
 runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,1,'approval','Approve change','remote change','Approve this operation?','It changes remote state.','call-pending-version','remote_action',$3,0,'ri_pending_version',0,'pending')`,
		pendingID, submitted.Task.ID, hashBytes("pending-version-request")); err != nil {
		t.Fatal(err)
	}
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "pending-version-response",
		RequestHash: hashBytes("pending-version-response"), ResourceID: submitted.Task.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, record, submitted.Task.ID, pendingID, 1, task.PendingDecisionApprove); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.Lease.Version != 9 {
		t.Fatalf("task after pending response = status %q lease %#v err=%v", details.Task.Status, details.Task.Lease, err)
	}
}

func TestPostgresCancelSettlesPendingClarification(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "clarification-cancel-submit", "perform task"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "clarification-cancel-worker", time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	modelStepID := mustID(t, domain.NewStepID)
	stepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "clarification_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{
			{ID: modelStepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask the model", Summary: "Model asked the user."},
			{ID: stepID, TaskID: claimed.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "Need a detail", Summary: "Which environment?"},
		},
		Event:                task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"clarification_required"}`)},
		ClarificationRequest: &task.ClarificationRequest{ID: requestID, TaskID: claimed.ID, Version: 1, ModelStepID: modelStepID, CallID: "call-cancel-clarification", Question: "Which environment?", Status: task.ClarificationPending},
	}); err != nil {
		t.Fatal(err)
	}
	cancelID := mustID(t, domain.NewTaskActionID)
	record := taskActionIdempotency(owner.ID, task.ActionCancel, "cancel-clarification-key", claimed.ID, task.StatusWaitingUser, cancelID)
	if _, err := store.RequestTaskAction(ctx, record, claimed.ID, task.ActionCancel, task.StatusWaitingUser, cancelID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_clarification_requests WHERE id=$1`, requestID).Scan(&status); err != nil || status != string(task.ClarificationCancelled) {
		t.Fatalf("clarification status after cancel = %q err=%v", status, err)
	}
	answerRecord := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.clarification.answer", Key: "answer-cancelled-clarification",
		RequestHash: hashBytes("answer-cancelled"), ResourceID: claimed.ID, RelatedID: requestID,
	}
	if err := store.AnswerClarificationRequest(ctx, answerRecord, claimed.ID, requestID, 1, "staging", "", ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("answer cancelled clarification error = %v", err)
	}
}

func taskActionIdempotency(ownerID string, action task.Action, key, taskID string, expected task.Status, actionID string) app.IdempotencyRecord {
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "task.action." + string(action), Key: key,
		RequestHash: hashBytes("task-action:" + string(action) + ":" + taskID + ":" + string(expected) + ":" + key),
		ResourceID:  actionID, RelatedID: taskID,
	}
}
