package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestPostgresTaskAgentStallNoticeWaitsForKnownStaleActivityAndClearsOnce(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND role='user' ORDER BY sequence,id LIMIT 1`,
		lease.Fence.TaskID, lease.Fence.OwnerID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	value := details.Task
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("unknown activity snapshot: %v", err)
	}
	var notices int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_stall_notices WHERE task_id=$1`, value.ID).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("unknown activity emitted a stall notice: count=%d err=%v", notices, err)
	}
	var revision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, value.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	record := func(observation app.TaskAgentActivityObservation) {
		t.Helper()
		observation.Fence = lease.Fence
		observation.AgentSessionID = lease.Fence.AgentSessionID
		observation.TaskRevision = revision
		accepted, err := f.store.RecordTaskAgentActivity(ctx, observation)
		if err != nil || !accepted {
			t.Fatalf("record activity %+v: accepted=%t err=%v", observation, accepted, err)
		}
	}
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindStatus, Status: "running", ObservationSequence: 1})
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("activity without heartbeat: %v", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_stall_notices WHERE task_id=$1`, value.ID).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("activity with unknown heartbeat emitted a stall notice: count=%d err=%v", notices, err)
	}
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindHeartbeat, ObservationSequence: 2})
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET activity_heartbeat_at=clock_timestamp()-interval '2 seconds'
WHERE task_id=$1 AND session_id=$2`, value.ID, lease.Fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("stale activity: %v", err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("repeat stale activity check: %v", err)
	}
	var stepID, state string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id,step_id,state FROM task_agent_stall_notices WHERE task_id=$1`, value.ID).
		Scan(&messageID, &stepID, &state); err != nil || state != "active" {
		t.Fatalf("persisted stall notice = (%q,%q), err=%v", state, stepID, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND payload->>'step_id'=$2`, value.ID, stepID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("repeat stale check duplicated the active event: count=%d err=%v", notices, err)
	}
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindHeartbeat, ObservationSequence: 3})
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("clear recovered activity: %v", err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("repeat recovered activity check: %v", err)
	}
	var stepStatus, summary string
	if err := f.store.pool.QueryRow(ctx, `SELECT n.state,s.status,s.summary
FROM task_agent_stall_notices n JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id WHERE n.task_id=$1`, value.ID).
		Scan(&state, &stepStatus, &summary); err != nil || state != "cleared" || stepStatus != "succeeded" || summary != "Agent 已恢复活动观察；任务仍按原状态继续。" {
		t.Fatalf("recovered notice = (%q,%q,%q), err=%v", state, stepStatus, summary, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND payload->>'step_id'=$2`, value.ID, stepID).Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("notice creation/clear events = %d, err=%v", notices, err)
	}
	var status string
	if err := f.store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, value.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("stall observation changed Task state to %q, err=%v", status, err)
	}
}

func TestPostgresTaskAgentActivityProgressClearsStallNoticeImmediately(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND role='user' ORDER BY sequence,id LIMIT 1`,
		lease.Fence.TaskID, lease.Fence.OwnerID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	value := details.Task
	var revision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, value.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	record := func(observation app.TaskAgentActivityObservation) {
		t.Helper()
		observation.Fence = lease.Fence
		observation.AgentSessionID = lease.Fence.AgentSessionID
		observation.TaskRevision = revision
		if accepted, err := f.store.RecordTaskAgentActivity(ctx, observation); err != nil || !accepted {
			t.Fatalf("record activity %+v: accepted=%t err=%v", observation, accepted, err)
		}
	}
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindStatus, Status: "running", ObservationSequence: 1})
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindHeartbeat, ObservationSequence: 2})
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET activity_heartbeat_at=clock_timestamp()-interval '2 seconds'
WHERE task_id=$1 AND session_id=$2`, value.ID, lease.Fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
		t.Fatalf("create stall notice: %v", err)
	}
	record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindActivity, Phase: "model",
		State: "started", ActivityID: "model-1", ObservationSequence: 3})
	var state, status, summary string
	if err := f.store.pool.QueryRow(ctx, `SELECT n.state,s.status,s.summary
FROM task_agent_stall_notices n JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id WHERE n.task_id=$1`, value.ID).
		Scan(&state, &status, &summary); err != nil || state != "cleared" || status != "succeeded" || summary != "Agent 已恢复活动观察；任务仍按原状态继续。" {
		t.Fatalf("activity progress did not clear notice immediately: (%q,%q,%q), err=%v", state, status, summary, err)
	}
}
