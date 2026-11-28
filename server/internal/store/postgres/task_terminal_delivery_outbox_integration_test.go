package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskTerminalDeliveryOutboxRecordsFailureAndCancellation(t *testing.T) {
	t.Run("failure source is bound to the task result", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		stepID := mustID(t, domain.NewStepID)
		event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
			Status: task.StatusFailed, CurrentStep: stepID, Result: "Task stopped after a model error.",
			Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "model", Status: task.StepFailed,
				Title: "Run model", Summary: "The model call failed."}},
			Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed","reason":"provider_unavailable"}`)},
		})
		if err != nil {
			t.Fatalf("commit failed Task: %v", err)
		}
		assertTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID, event.Sequence, "task.result", "terminal_failure")
		if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			return enqueueTaskTerminalDeliveryInTx(ctx, tx, f.request.Fence.TaskID, event.Sequence, "task.result")
		}); err != nil {
			t.Fatalf("replay terminal delivery source: %v", err)
		}
		assertTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID, event.Sequence, "task.result", "terminal_failure")
	})

	t.Run("outbox insert failure rolls back terminal result", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		if _, err := f.store.pool.Exec(ctx, `CREATE FUNCTION test_reject_task_terminal_delivery() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced terminal delivery insert failure'; END $$`); err != nil {
			t.Fatalf("create outbox failure trigger function: %v", err)
		}
		if _, err := f.store.pool.Exec(ctx, `CREATE TRIGGER test_reject_task_terminal_delivery
BEFORE INSERT ON task_terminal_delivery_outbox FOR EACH ROW EXECUTE FUNCTION test_reject_task_terminal_delivery()`); err != nil {
			t.Fatalf("create outbox failure trigger: %v", err)
		}
		t.Cleanup(func() {
			_, _ = f.store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_reject_task_terminal_delivery ON task_terminal_delivery_outbox`)
			_, _ = f.store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_reject_task_terminal_delivery()`)
		})

		stepID := mustID(t, domain.NewStepID)
		_, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
			Status: task.StatusFailed, CurrentStep: stepID, Result: "Task stopped after a model error.",
			Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "model", Status: task.StepFailed,
				Title: "Run model", Summary: "The model call failed."}},
			Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed","reason":"provider_unavailable"}`)},
		})
		if err == nil {
			t.Fatal("terminal result committed despite a failed durable source insert")
		}
		var status string
		if err := f.store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, f.request.Fence.TaskID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		var resultEvents, deliveries int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, f.request.Fence.TaskID).Scan(&resultEvents); err != nil {
			t.Fatal(err)
		}
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1`, f.request.Fence.TaskID).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusRunning) || resultEvents != 0 || deliveries != 0 {
			t.Fatalf("failed terminal transaction left status=%q result_events=%d deliveries=%d", status, resultEvents, deliveries)
		}
	})

	t.Run("confirmed cancellation source is durable", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		taskID := f.request.Fence.TaskID
		stopID := mustID(t, domain.NewTaskActionID)
		record := taskActionIdempotency(f.request.Fence.OwnerID, task.ActionCancel, "terminal-outbox-cancel", taskID, task.StatusRunning, stopID)
		if _, err := f.store.RequestTaskAction(ctx, record, taskID, task.ActionCancel, task.StatusRunning, nil, stopID); err != nil {
			t.Fatalf("request cancellation: %v", err)
		}
		claim, found, err := f.store.ClaimNextTaskStop(ctx, "terminal-outbox-stop-worker", time.Minute)
		if err != nil || !found || claim.Request.ID != stopID {
			t.Fatalf("claim cancellation: found=%t claim=%+v err=%v", found, claim, err)
		}
		if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopID); err != nil {
			t.Fatal(err)
		}
		cancelled, err := f.store.CompleteTaskStop(ctx, taskID, stopID, claim.Request.LeaseOwner, claim.Request.LeaseVersion)
		if err != nil || cancelled.Status != task.StatusCancelled {
			t.Fatalf("complete cancellation: task=%+v err=%v", cancelled, err)
		}
		var sequence int64
		if err := f.store.pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, taskID).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		assertTaskTerminalDeliverySource(t, ctx, f.store, taskID, sequence, "task.status", "terminal_cancellation")
	})

	t.Run("pause does not enqueue terminal delivery", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		taskID := f.request.Fence.TaskID
		pauseID := mustID(t, domain.NewTaskActionID)
		record := taskActionIdempotency(f.request.Fence.OwnerID, task.ActionPause, "terminal-outbox-pause", taskID, task.StatusRunning, pauseID)
		if _, err := f.store.RequestTaskAction(ctx, record, taskID, task.ActionPause, task.StatusRunning, nil, pauseID); err != nil {
			t.Fatalf("request pause: %v", err)
		}
		claim, found, err := f.store.ClaimNextTaskStop(ctx, "terminal-outbox-pause-worker", time.Minute)
		if err != nil || !found || claim.Request.ID != pauseID {
			t.Fatalf("claim pause: found=%t claim=%+v err=%v", found, claim, err)
		}
		if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, pauseID); err != nil {
			t.Fatal(err)
		}
		paused, err := f.store.CompleteTaskStop(ctx, taskID, pauseID, claim.Request.LeaseOwner, claim.Request.LeaseVersion)
		if err != nil || paused.Status != task.StatusPaused {
			t.Fatalf("complete pause: task=%+v err=%v", paused, err)
		}
		var deliveries int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1`, taskID).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 0 {
			t.Fatalf("pause created %d terminal delivery rows, want none", deliveries)
		}
	})
}

func TestPostgresTaskTerminalDeliveryOutboxRecordsOnlyEligibleUnknownWaits(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		reason       string
		publicReason string
	}{
		{reason: "dsh_recovery_unresolved", publicReason: "reconciliation_required"},
		{reason: "mcp_result_unknown", publicReason: "unknown_outcome"},
		{reason: "hub_action_unknown", publicReason: "unknown_outcome"},
		{reason: "tool_result_unknown", publicReason: "unknown_outcome"},
		{reason: "target_result_unknown", publicReason: "unknown_outcome"},
	} {
		t.Run("eligible/"+test.reason, func(t *testing.T) {
			f := newAgentLifecycleFixture(t)
			sequence := commitTaskTerminalDeliveryWaiting(t, f, test.reason)
			if got := safeTaskTerminalWaitingReason(test.reason); got != test.publicReason {
				t.Fatalf("public wait reason=%q, want %q", got, test.publicReason)
			}
			assertTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID, sequence, "task.waiting_user", "waiting_unknown")
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				return enqueueTaskWaitingUnknownDeliveryInTx(ctx, tx, f.request.Fence.TaskID, sequence, "task.waiting_user")
			}); err != nil {
				t.Fatalf("replay durable wait source: %v", err)
			}
			assertTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID, sequence, "task.waiting_user", "waiting_unknown")
		})
	}

	t.Run("approval card suppresses source", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		stepID := mustID(t, domain.NewStepID)
		pendingID := mustID(t, domain.NewPendingRequestID)
		expiresAt := time.Now().UTC().Add(time.Hour)
		pending := &task.PendingRequest{
			ID: pendingID, TaskID: f.request.Fence.TaskID, Version: 1, Kind: "approval", Title: "Approve action",
			Object: "test action", Content: "Review the action?", Impact: "The action will run on the Host.",
			CallID: "waiting-approval-call", ToolName: "test_tool", RequestHash: make([]byte, 32),
			RuntimeGeneration: 1, RuntimeInstanceID: "runtime-instance-1", ControlVersion: 1,
			Status: task.PendingRequestPending, ExpiresAt: &expiresAt,
		}
		if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
			Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
			Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
				Title: pending.Title, Summary: pending.Content}},
			Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)}, PendingRequest: pending,
		}); err != nil {
			t.Fatalf("commit approval wait: %v", err)
		}
		assertNoTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID)
	})

	t.Run("clarification card suppresses source", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		modelStepID := mustID(t, domain.NewStepID)
		stepID := mustID(t, domain.NewStepID)
		clarification := &task.ClarificationRequest{
			ID: mustID(t, domain.NewPendingRequestID), TaskID: f.request.Fence.TaskID, Version: 1,
			ModelStepID: modelStepID, CallID: "waiting-clarification-call", Question: "Which environment should I use?",
			Status: task.ClarificationPending,
		}
		if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
			Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "clarification_required",
			Steps: []task.Step{
				{ID: modelStepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask the model", Summary: "The model asked a question."},
				{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser,
					Title: "The Agent needs one detail", Summary: clarification.Question},
			},
			Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"clarification_required"}`)}, ClarificationRequest: clarification,
		}); err != nil {
			t.Fatalf("commit clarification wait: %v", err)
		}
		assertNoTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID)
	})

	for _, test := range []struct {
		name      string
		reason    string
		eventType string
	}{
		{name: "approval", reason: "approval_required", eventType: "task.waiting_user"},
		{name: "clarification", reason: "clarification_required", eventType: "task.waiting_user"},
		{name: "revoked connector", reason: "mcp_connector_revoked", eventType: "task.waiting_user"},
		{name: "workspace input changed", reason: "workspace_input_changed", eventType: "task.waiting_user"},
		{name: "Hub workspace sync", reason: "task_hub_workspace_sync", eventType: "task.waiting_user"},
		{name: "workspace host sync", reason: "workspace_host_sync", eventType: "task.waiting_user"},
		{name: "tool call budget", reason: "tool_call_budget", eventType: "task.waiting_user"},
		{name: "model retry ladder", reason: "model_retry_ladder_exhausted", eventType: "task.waiting_user"},
		{name: "unsupported MCP result", reason: "mcp_result_unsupported", eventType: "task.waiting_user"},
		{name: "other event type", reason: "dsh_recovery_unresolved", eventType: "task.status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAgentLifecycleFixture(t)
			stepID := mustID(t, domain.NewStepID)
			if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
				TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
				Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: test.reason,
				Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
					Title: "Wait", Summary: "The task is waiting."}},
				Event: task.Event{Type: test.eventType, Payload: []byte(`{"reason":"` + test.reason + `"}`)},
			}); err != nil {
				t.Fatalf("commit non-delivery wait: %v", err)
			}
			assertNoTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID)
		})
	}

	for _, cardKind := range []string{"approval", "clarification"} {
		t.Run("active "+cardKind+" card suppresses unknown source", func(t *testing.T) {
			f := newAgentLifecycleFixture(t)
			insertPendingTaskTerminalDeliveryCard(t, ctx, f, cardKind)
			commitTaskTerminalDeliveryWaiting(t, f, "tool_result_unknown")
			assertNoTaskTerminalDeliverySource(t, ctx, f.store, f.request.Fence.TaskID)
		})
	}

	t.Run("source insert failure rolls back wait", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		if _, err := f.store.pool.Exec(ctx, `CREATE FUNCTION test_reject_task_waiting_delivery() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.source_kind='waiting_unknown' THEN RAISE EXCEPTION 'forced waiting delivery insert failure'; END IF;
  RETURN NEW;
END $$`); err != nil {
			t.Fatalf("create waiting delivery failure trigger function: %v", err)
		}
		if _, err := f.store.pool.Exec(ctx, `CREATE TRIGGER test_reject_task_waiting_delivery
BEFORE INSERT ON task_terminal_delivery_outbox FOR EACH ROW EXECUTE FUNCTION test_reject_task_waiting_delivery()`); err != nil {
			t.Fatalf("create waiting delivery failure trigger: %v", err)
		}
		t.Cleanup(func() {
			_, _ = f.store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_reject_task_waiting_delivery ON task_terminal_delivery_outbox`)
			_, _ = f.store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_reject_task_waiting_delivery()`)
		})

		stepID := mustID(t, domain.NewStepID)
		_, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
			Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "dsh_recovery_unresolved",
			Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "model", Status: task.StepUnknown,
				Title: "Recover task", Summary: "The model result is unresolved."}},
			Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"dsh_recovery_unresolved"}`)},
		})
		if err == nil {
			t.Fatal("waiting Task committed despite a failed durable source insert")
		}
		var status, waitingReason string
		if err := f.store.pool.QueryRow(ctx, `SELECT status,waiting_reason FROM tasks WHERE id=$1`, f.request.Fence.TaskID).
			Scan(&status, &waitingReason); err != nil {
			t.Fatal(err)
		}
		var waitingEvents, deliveries int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.waiting_user'`, f.request.Fence.TaskID).
			Scan(&waitingEvents); err != nil {
			t.Fatal(err)
		}
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1`, f.request.Fence.TaskID).
			Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusRunning) || waitingReason != "" || waitingEvents != 0 || deliveries != 0 {
			t.Fatalf("failed waiting transaction left status=%q reason=%q waiting_events=%d deliveries=%d", status, waitingReason, waitingEvents, deliveries)
		}
	})
}

func commitTaskTerminalDeliveryWaiting(t *testing.T, f agentLifecycleFixture, reason string) int64 {
	t.Helper()
	stepID := mustID(t, domain.NewStepID)
	var stepSequence int64
	if err := f.store.pool.QueryRow(context.Background(), `SELECT COALESCE(max(sequence),0)+1 FROM task_steps WHERE task_id=$1`,
		f.request.Fence.TaskID).Scan(&stepSequence); err != nil {
		t.Fatalf("read next Task step sequence: %v", err)
	}
	event, err := f.store.CommitTaskProgress(context.Background(), app.TaskProgress{
		TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: reason,
		Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: stepSequence, Kind: "tool", Status: task.StepUnknown,
			Title: "Review unresolved result", Summary: "The operation result is unresolved."}},
		Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"` + reason + `","call_id":"private-call-identifier","payload":"private-operation-payload"}`)},
	})
	if err != nil {
		t.Fatalf("commit waiting Task: %v", err)
	}
	return event.Sequence
}

func insertPendingTaskTerminalDeliveryCard(t *testing.T, ctx context.Context, f agentLifecycleFixture, kind string) {
	t.Helper()
	switch kind {
	case "approval":
		requestID := mustID(t, domain.NewPendingRequestID)
		requestHash := sha256.Sum256([]byte("active approval request"))
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,
 runtime_generation,runtime_instance_id,control_version,status)
VALUES ($1,$2,1,'approval','Review action','test object','Review this action','External action impact',
 'private-approval-call','test_tool',$3,0,'test-runtime',0,'pending')`, requestID, f.request.Fence.TaskID, requestHash[:]); err != nil {
			t.Fatalf("insert pending approval card: %v", err)
		}
	case "clarification":
		modelStepID := mustID(t, domain.NewStepID)
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,1,'model','succeeded','Model step','Stored model step')`, modelStepID, f.request.Fence.TaskID); err != nil {
			t.Fatalf("insert clarification model step: %v", err)
		}
		requestID := mustID(t, domain.NewPendingRequestID)
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_clarification_requests
(id,task_id,version,model_step_id,call_id,question,status)
VALUES ($1,$2,1,$3,'private-clarification-call','Which environment should I use?','pending')`,
			requestID, f.request.Fence.TaskID, modelStepID); err != nil {
			t.Fatalf("insert pending clarification card: %v", err)
		}
	default:
		t.Fatalf("unsupported pending card kind %q", kind)
	}
}

func assertNoTaskTerminalDeliverySource(t *testing.T, ctx context.Context, store *Store, taskID string) {
	t.Helper()
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1`, taskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ineligible waiting Task created %d terminal delivery source rows", count)
	}
}

func assertTaskTerminalDeliverySource(t *testing.T, ctx context.Context, store *Store, taskID string, sequence int64, eventType, sourceKind string) {
	t.Helper()
	var (
		ownerID, conversationID, initiatorID, gotKind, gotEventType, payload string
		taskRevision, authorizationVersion                                   int64
		storedDigest                                                         []byte
		state                                                                string
	)
	err := store.pool.QueryRow(ctx, `SELECT delivery.owner_id,delivery.conversation_id,delivery.initiator_id,
delivery.task_revision,delivery.authorization_version,delivery.source_kind,delivery.source_event_type,
delivery.source_sha256,delivery.state,event.payload::text
FROM task_terminal_delivery_outbox delivery
JOIN task_events event ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
WHERE delivery.task_id=$1 AND delivery.source_event_sequence=$2 AND delivery.source_event_type=$3`,
		taskID, sequence, eventType).Scan(&ownerID, &conversationID, &initiatorID, &taskRevision, &authorizationVersion,
		&gotKind, &gotEventType, &storedDigest, &state, &payload)
	if err != nil {
		t.Fatalf("read terminal delivery source: %v", err)
	}
	digest := sha256.Sum256([]byte(payload))
	if ownerID == "" || conversationID == "" || initiatorID == "" || taskRevision < 0 || authorizationVersion < 1 ||
		gotKind != sourceKind || gotEventType != eventType || state != "pending" || !bytes.Equal(storedDigest, digest[:]) {
		t.Fatalf("terminal delivery source mismatch: owner=%q conversation=%q initiator=%q revision=%d auth=%d kind=%q event=%q state=%q digest_matches=%t",
			ownerID, conversationID, initiatorID, taskRevision, authorizationVersion, gotKind, gotEventType, state, bytes.Equal(storedDigest, digest[:]))
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_event_sequence=$2 AND source_event_type=$3`, taskID, sequence, eventType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("terminal delivery source count=%d, want 1", count)
	}
}
