package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestPostgresApprovedTaskOperationBindingAllowsExactDispatchAndReplay(t *testing.T) {
	fixture := newApprovedTaskOperationBindingFixture(t)
	ctx := context.Background()
	// An approval answered before its deadline remains valid when dispatch
	// happens after that deadline.
	if _, err := fixture.store.pool.Exec(ctx, `UPDATE task_pending_requests SET created_at=clock_timestamp()-interval '3 minutes',
responded_at=clock_timestamp()-interval '2 minutes', expires_at=clock_timestamp()-interval '1 minute'
WHERE task_id=$1 AND id=$2 AND version=$3`,
		fixture.task.ID, fixture.pending.ID, fixture.pending.Version); err != nil {
		t.Fatal(err)
	}

	created, replayed, err := fixture.store.CreateOperation(ctx, fixture.operation, fixture.task.Lease.Owner)
	if err != nil || replayed || created.Status != task.OperationPrepared {
		t.Fatalf("CreateOperation() = %#v replayed=%t err=%v", created, replayed, err)
	}
	loaded, err := fixture.store.GetOperation(ctx, fixture.ownerID, fixture.task.ID, fixture.operation.Key)
	if err != nil || !sameTaskOperationIdentityBinding(loaded, fixture.operation) {
		t.Fatalf("GetOperation() lost approval identity: operation=%#v err=%v", loaded, err)
	}

	replay := fixture.operation
	replay.ID = mustID(t, domain.NewOperationID)
	if _, replayed, err := fixture.store.CreateOperation(ctx, replay, fixture.task.Lease.Owner); err != nil || !replayed {
		t.Fatalf("exact CreateOperation() replay = replayed=%t err=%v", replayed, err)
	}

	var responderID string
	if err := fixture.store.pool.QueryRow(ctx, `SELECT responded_by_user_id FROM task_pending_requests WHERE task_id=$1 AND id=$2 AND version=$3`,
		fixture.task.ID, fixture.pending.ID, fixture.pending.Version).Scan(&responderID); err != nil || responderID != fixture.ownerID {
		t.Fatalf("pending approval responder = %q err=%v; want acting user %q", responderID, err, fixture.ownerID)
	}

	dispatched, err := fixture.store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: fixture.task.ID, OperationKey: fixture.operation.Key, LeaseOwner: fixture.task.Lease.Owner,
		LeaseVersion: fixture.task.Lease.Version, RuntimeGeneration: fixture.host.RuntimeGeneration,
		RuntimeInstanceID: fixture.host.RuntimeInstanceID, ControlVersion: fixture.host.ControlVersion,
		RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || dispatched.Status != task.OperationDispatched || !sameTaskOperationIdentityBinding(dispatched, fixture.operation) {
		t.Fatalf("approved operation dispatch = %#v err=%v", dispatched, err)
	}
}

func TestPostgresTaskOperationRequiresExactApprovedBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *taskApprovalOperationFixture)
	}{
		{name: "wrong CallID", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			f.operation.CallID = "different-call-id"
			f.operation.Key = app.AgentOperationKey(f.operation.CallID)
		}},
		{name: "wrong request hash", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			f.operation.RequestHash = hashBytes("different operation hash")
			f.operation.ApprovalRequestHash = append([]byte(nil), f.operation.RequestHash...)
		}},
		{name: "wrong Task revision", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			revision := *f.operation.TaskRevision + 1
			f.operation.TaskRevision = &revision
		}},
		{name: "wrong runtime fence", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			if _, err := f.store.pool.Exec(context.Background(), `UPDATE task_pending_requests SET control_version=control_version+1 WHERE task_id=$1 AND id=$2 AND version=$3`,
				f.task.ID, f.pending.ID, f.pending.Version); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "rejected", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			if _, err := f.store.pool.Exec(context.Background(), `UPDATE task_pending_requests SET status='rejected', decision='reject' WHERE task_id=$1 AND id=$2 AND version=$3`,
				f.task.ID, f.pending.ID, f.pending.Version); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "expired", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			if _, err := f.store.pool.Exec(context.Background(), `UPDATE task_pending_requests SET created_at=clock_timestamp()-interval '3 minutes',
expires_at=clock_timestamp()-interval '1 second', responded_at=clock_timestamp()
WHERE task_id=$1 AND id=$2 AND version=$3`,
				f.task.ID, f.pending.ID, f.pending.Version); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "approved but unlinked version", mutate: func(t *testing.T, f *taskApprovalOperationFixture) {
			version := f.pending.Version + 1
			f.operation.ApprovalVersion = &version
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newApprovedTaskOperationBindingFixture(t)
			test.mutate(t, fixture)
			_, _, err := fixture.store.CreateOperation(context.Background(), fixture.operation, fixture.task.Lease.Owner)
			if err == nil {
				t.Fatal("CreateOperation() accepted a nonmatching approval binding")
			}
			if _, err := fixture.store.GetOperation(context.Background(), fixture.ownerID, fixture.task.ID, fixture.operation.Key); domain.CodeOf(err) != domain.CodeNotFound {
				t.Fatalf("rejected operation was persisted: GetOperation() error=%v", err)
			}
		})
	}
}

func TestPostgresTaskOperationDispatchRejectsRevisionChangedAfterApproval(t *testing.T) {
	fixture := newApprovedTaskOperationBindingFixture(t)
	ctx := context.Background()
	if _, _, err := fixture.store.CreateOperation(ctx, fixture.operation, fixture.task.Lease.Owner); err != nil {
		t.Fatalf("CreateOperation(): %v", err)
	}
	if _, err := fixture.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=current_revision+1, desired_revision=desired_revision+1 WHERE id=$1`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: fixture.task.ID, OperationKey: fixture.operation.Key, LeaseOwner: fixture.task.Lease.Owner,
		LeaseVersion: fixture.task.Lease.Version, RuntimeGeneration: fixture.host.RuntimeGeneration,
		RuntimeInstanceID: fixture.host.RuntimeInstanceID, ControlVersion: fixture.host.ControlVersion,
		RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched,
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch after Task revision change error=%v; want conflict", err)
	}
	loaded, loadErr := fixture.store.GetOperation(ctx, fixture.ownerID, fixture.task.ID, fixture.operation.Key)
	if loadErr != nil || loaded.Status != task.OperationPrepared {
		t.Fatalf("stale operation crossed dispatch boundary: operation=%#v err=%v", loaded, loadErr)
	}
}

type taskApprovalOperationFixture struct {
	store     *Store
	ownerID   string
	host      host.Host
	task      task.Task
	pending   task.PendingRequest
	operation task.Operation
}

func newApprovedTaskOperationBindingFixture(t *testing.T) *taskApprovalOperationFixture {
	t.Helper()
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	hostValue = setAgentTestHostRuntime(t, store, hostValue, "ri_task_approval_binding")
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "approval-binding-submit", "approve exact Host action"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "approval-binding-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v found=%t err=%v", claimed, found, err)
	}
	claimed = startTaskRevisionForApprovalBinding(t, store, owner.ID, claimed.ID)
	if claimed.CurrentRevision == nil || claimed.DesiredRevision == nil || *claimed.CurrentRevision != *claimed.DesiredRevision {
		t.Fatalf("Task revision was not started before approval fixture: current=%v desired=%v", claimed.CurrentRevision, claimed.DesiredRevision)
	}
	var stepSequence int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, claimed.ID).Scan(&stepSequence); err != nil {
		t.Fatal(err)
	}
	stepID, pendingID := mustID(t, domain.NewStepID), mustID(t, domain.NewPendingRequestID)
	requestHash := hashBytes("approved exact Host action")
	expiresAt := time.Now().UTC().Add(time.Hour)
	version := int64(1)
	revision := *claimed.CurrentRevision
	pending := task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: version, TaskRevision: &revision,
		Kind: "approval", Title: "Approve exact Host action", Object: "workspace command",
		Content: `{"command":"echo approved"}`, Impact: "Executes one Host command.",
		CallID: "call-task-approval-binding", ToolName: "shell.exec", RequestHash: requestHash,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, Status: task.PendingRequestPending, ExpiresAt: &expiresAt,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage:          task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps:          []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: stepSequence, Kind: "tool", Status: task.StepWaitingUser, Title: pending.Title, Summary: pending.Content}},
		Event:          task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
		PendingRequest: &pending,
	}); err != nil {
		t.Fatalf("CommitTaskProgress() for approval: %v", err)
	}
	response := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "approval-binding-response",
		RequestHash: hashBytes("approve exact Host action"), ResourceID: claimed.ID, RelatedID: pending.ID,
	}
	if err := store.RespondToPendingRequest(ctx, response, claimed.ID, pending.ID, pending.Version, task.PendingDecisionApprove); err != nil {
		t.Fatalf("RespondToPendingRequest(): %v", err)
	}
	resumed, found, err := store.ClaimNextTask(ctx, "approval-binding-resume-worker", time.Minute)
	if err != nil || !found || resumed.ID != claimed.ID {
		t.Fatalf("claim approved Task = %#v found=%t err=%v", resumed, found, err)
	}
	claimedLease := resumed.Lease
	resumedDetails, err := store.GetTask(ctx, resumed.ID, owner.ID)
	if err != nil {
		t.Fatalf("reload claimed Task after approval: %v", err)
	}
	resumed = resumedDetails.Task
	if resumed.Lease.Owner != claimedLease.Owner || resumed.Lease.Version != claimedLease.Version ||
		resumed.CurrentRevision == nil || resumed.DesiredRevision == nil || *resumed.CurrentRevision != *resumed.DesiredRevision {
		t.Fatalf("approved Task claim lost its revision or lease: task=%#v", resumed)
	}
	approvalVersion := pending.Version
	operationID := mustID(t, domain.NewOperationID)
	operation := task.Operation{
		ID: operationID, TaskID: resumed.ID, Key: app.AgentOperationKey(pending.CallID), RequestHash: append([]byte(nil), requestHash...),
		TaskRevision: &revision, CallID: pending.CallID, ToolName: pending.ToolName,
		ApprovalID: pending.ID, ApprovalVersion: &approvalVersion, ApprovalRequestHash: append([]byte(nil), requestHash...),
		HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, LeaseVersion: resumed.Lease.Version, RuntimeLeaseVersion: resumed.Lease.Version,
		Status: task.OperationPrepared,
	}
	return &taskApprovalOperationFixture{store: store, ownerID: owner.ID, host: hostValue, task: resumed, pending: pending, operation: operation}
}

func startTaskRevisionForApprovalBinding(t *testing.T, store *Store, ownerID, taskID string) task.Task {
	t.Helper()
	ctx := context.Background()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordTaskRevisionStartedInTx(ctx, tx, taskID); err != nil {
		t.Fatalf("record started Task revision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Task revision start: %v", err)
	}
	details, err := store.GetTask(ctx, taskID, ownerID)
	if err != nil {
		t.Fatalf("reload Task after revision start: %v", err)
	}
	return details.Task
}
