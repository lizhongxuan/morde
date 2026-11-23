package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestPostgresTaskAgentRequestReconciliationPersistsUntilExactResolution(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	revision := *details.Task.CurrentRevision
	messageID := setTaskAgentReconciliationMessagePending(t, f.store, lease.Fence.TaskID, lease.Fence.OwnerID, true)
	first := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseTool, app.TaskAgentRequestKindToolCall, "call_timeout_first", strings.Repeat("a", 64), 1)
	second := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseCheckpoint, app.TaskAgentRequestKindCheckpointWrite, "checkpoint_17", strings.Repeat("b", 64), 2)
	for _, value := range []app.TaskAgentRequestReconciliation{first, second} {
		if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, value); err != nil || !accepted {
			t.Fatalf("record pending reconciliation %+v: accepted=%t err=%v", value, accepted, err)
		}
	}
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, second); err != nil || !accepted {
		t.Fatalf("repeat pending reconciliation: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)

	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: revision,
		Kind: app.TaskAgentActivityKindHeartbeat, Phase: second.Phase, ActivityID: second.ActivityID,
		ObservationSequence: 3,
	}); err != nil || !accepted {
		t.Fatalf("record fresh heartbeat: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: revision,
		Kind: app.TaskAgentActivityKindActivity, Phase: second.Phase, State: "failed", ActivityID: second.ActivityID,
		ObservationSequence: 4,
	}); err != nil || !accepted {
		t.Fatalf("record failed activity: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: revision,
		Kind: app.TaskAgentActivityKindStatus, Status: "idle", ObservationSequence: 5,
	}); err != nil || !accepted {
		t.Fatalf("record idle status: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)

	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration); err != nil {
		t.Fatal(err)
	}
	receipt := signAgentLifecycleReceipt(t, f.key, lease.Fence, f.request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm previous generation stopped: %v", err)
	}
	replacement := f.request
	replacement.Fence.AgentInstanceID = "agent-request-reconciliation-next"
	replacement.SupervisorNonce = make([]byte, 32)
	replacement.SupervisorNonce[0] = 3
	next, replayed, err := f.service.Start(ctx, replacement)
	if err != nil || replayed || next.Fence.AgentGeneration != lease.Fence.AgentGeneration+1 {
		t.Fatalf("start next Agent generation: fence=%+v replayed=%t err=%v", next.Fence, replayed, err)
	}
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: next.Fence, AgentSessionID: next.Fence.AgentSessionID, TaskRevision: revision,
		Kind: app.TaskAgentActivityKindStatus, Status: "idle", ObservationSequence: 1,
	}); err != nil || !accepted {
		t.Fatalf("record next-generation idle status: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)

	first.Fence = next.Fence
	first.ObservationSequence = 0 // Resolution is fenced by request identity, not observation ordering.
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, first); err != nil || !resolved {
		t.Fatalf("resolve first request while another remains: resolved=%t err=%v", resolved, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	second.Fence = next.Fence
	second.ObservationSequence = 0
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, second); err != nil || !resolved {
		t.Fatalf("resolve last request: resolved=%t err=%v", resolved, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "cleared", 1)

	second.ObservationSequence = 2
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, second); err != nil || !accepted {
		t.Fatalf("accept late duplicate timeout as inert: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "cleared", 1)
	var states []string
	rows, err := f.store.pool.Query(ctx, `SELECT state FROM task_agent_request_reconciliations
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 ORDER BY request_kind`, lease.Fence.TaskID,
		lease.Fence.AgentSessionID, revision)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(states) != 2 || states[0] != "resolved" || states[1] != "resolved" {
		t.Fatalf("request reconciliation states=%v, want both resolved", states)
	}
}

func TestPostgresTaskAgentRequestReconciliationRejectsWrongDigestAndFence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	revision := *details.Task.CurrentRevision
	messageID := setTaskAgentReconciliationMessagePending(t, f.store, lease.Fence.TaskID, lease.Fence.OwnerID, true)
	value := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseSubagent, app.TaskAgentRequestKindChildCreate, "call_child_create", strings.Repeat("c", 64), 1)
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, value); err != nil || !accepted {
		t.Fatalf("record request reconciliation: accepted=%t err=%v", accepted, err)
	}
	wrongDigest := value
	wrongDigest.RequestDigest = strings.Repeat("d", 64)
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, wrongDigest); err != nil || resolved {
		t.Fatalf("wrong digest resolved request: resolved=%t err=%v", resolved, err)
	}
	wrongFence := value
	wrongFence.Fence.AuthorizationVersion++
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, wrongFence); err != nil || accepted {
		t.Fatalf("wrong fence recorded request: accepted=%t err=%v", accepted, err)
	}
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, wrongFence); err != nil || resolved {
		t.Fatalf("wrong fence resolved request: resolved=%t err=%v", resolved, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	var pendingCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_request_reconciliations WHERE task_id=$1 AND state='pending'`,
		lease.Fence.TaskID).Scan(&pendingCount); err != nil || pendingCount != 1 {
		t.Fatalf("pending request count=%d err=%v, want 1", pendingCount, err)
	}
}

func TestPostgresTaskAgentRequestReconciliationCreatesNoticeAfterPendingInputArrives(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	revision := *details.Task.CurrentRevision
	messageID := setTaskAgentReconciliationMessagePending(t, f.store, lease.Fence.TaskID, lease.Fence.OwnerID, false)
	value := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseCheckpoint, app.TaskAgentRequestKindCheckpointRead, "checkpoint_read_0", strings.Repeat("e", 64), 1)
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, value); err != nil || !accepted {
		t.Fatalf("record request without pending input: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "", 0)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	details, err = f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, details.Task, time.Minute); err != nil {
		t.Fatalf("check pending request after input arrived: %v", err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
}

func TestPostgresTaskAgentRequestReconciliationNoticePersistsUntilExactResolution(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	revision := *details.Task.CurrentRevision
	messageID := setTaskAgentReconciliationMessagePending(t, f.store, lease.Fence.TaskID, lease.Fence.OwnerID, true)
	value := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseTool, app.TaskAgentRequestKindToolCall, "call_unresolved_after_message", strings.Repeat("f", 64), 1)
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, value); err != nil || !accepted {
		t.Fatalf("record pending request: accepted=%t err=%v", accepted, err)
	}
	second := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID, revision,
		app.TaskAgentRequestPhaseTool, app.TaskAgentRequestKindToolCall, "second_call_unresolved_after_message", strings.Repeat("a", 64), 2)
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, second); err != nil || !accepted {
		t.Fatalf("record second pending request: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	details, err = f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, details.Task, time.Minute); err != nil {
		t.Fatalf("check request after message left pending: %v", err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: revision,
		Kind: app.TaskAgentActivityKindActivity, Phase: "model", State: "started", ActivityID: "next_model_call",
		ObservationSequence: 3,
	}); err != nil || !accepted {
		t.Fatalf("record later agent progress: accepted=%t err=%v", accepted, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_agent_request_reconciliations
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 AND request_kind=$4 AND activity_id=$5 AND request_digest=$6`,
		lease.Fence.TaskID, value.AgentSessionID, revision, value.RequestKind, value.ActivityID, value.RequestDigest).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("message leaving pending incorrectly resolved request: state=%q", state)
	}
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, value); err != nil || !resolved {
		t.Fatalf("resolve first exact request after message left pending: resolved=%t err=%v", resolved, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "active", 1)
	if resolved, err := f.store.ResolveTaskAgentRequestReconciliation(ctx, second); err != nil || !resolved {
		t.Fatalf("resolve second exact request after message left pending: resolved=%t err=%v", resolved, err)
	}
	assertTaskAgentRequestReconciliationNotice(t, f.store, lease.Fence.TaskID, messageID, "cleared", 1)
	var summary string
	if err := f.store.pool.QueryRow(ctx, `SELECT s.summary FROM task_agent_stall_notices n
JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id
WHERE n.task_id=$1 AND n.message_id=$2 AND n.notice_kind='request_reconciliation'`,
		lease.Fence.TaskID, messageID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "精确对账结果") || !strings.Contains(summary, "已不在待处理队列中") {
		t.Fatalf("resolved notice summary=%q, want exact-result and message-resolved wording", summary)
	}
}

func newTaskAgentRequestReconciliation(fence app.AgentInstanceFence, sessionID string, revision int64,
	phase, requestKind, activityID, digest string, sequence int64) app.TaskAgentRequestReconciliation {
	return app.TaskAgentRequestReconciliation{Fence: fence, AgentSessionID: sessionID, TaskRevision: revision,
		Phase: phase, RequestKind: requestKind, ActivityID: activityID, RequestDigest: digest,
		ObservationSequence: sequence}
}

func setTaskAgentReconciliationMessagePending(t *testing.T, store *Store, taskID, ownerID string, pending bool) string {
	t.Helper()
	var messageID string
	if err := store.pool.QueryRow(context.Background(), `SELECT id FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND role='user' ORDER BY sequence,id LIMIT 1`, taskID, ownerID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	status := "applied"
	if pending {
		status = "pending"
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE conversation_messages SET delivery_status=$2 WHERE id=$1`, messageID, status); err != nil {
		t.Fatal(err)
	}
	return messageID
}

func assertTaskAgentRequestReconciliationNotice(t *testing.T, store *Store, taskID, messageID, state string, count int) {
	t.Helper()
	var gotCount int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_agent_stall_notices
WHERE task_id=$1 AND message_id=$2 AND notice_kind='request_reconciliation'`, taskID, messageID).Scan(&gotCount); err != nil {
		t.Fatal(err)
	}
	if gotCount != count {
		t.Fatalf("reconciliation notice count=%d, want %d", gotCount, count)
	}
	if count == 0 {
		return
	}
	var storedState, stepStatus, summary string
	if err := store.pool.QueryRow(context.Background(), `SELECT n.state,s.status,s.summary
FROM task_agent_stall_notices n JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id
WHERE n.task_id=$1 AND n.message_id=$2 AND n.notice_kind='request_reconciliation'`, taskID, messageID).
		Scan(&storedState, &stepStatus, &summary); err != nil {
		t.Fatal(err)
	}
	if storedState != state {
		t.Fatalf("reconciliation notice state=%q, want %q", storedState, state)
	}
	if state == "active" && (stepStatus != "running" || !strings.Contains(summary, "待对账")) {
		t.Fatalf("active reconciliation step status/summary=(%q,%q), want running with pending-reconciliation wording", stepStatus, summary)
	}
	if state == "cleared" && stepStatus != "succeeded" {
		t.Fatalf("cleared reconciliation step status=%q, want succeeded", stepStatus)
	}
}
