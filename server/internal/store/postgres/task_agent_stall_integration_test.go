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
	var noticeRevisionMatches, stepRevisionIsNull bool
	if err := f.store.pool.QueryRow(ctx, `SELECT n.task_revision=$2,s.task_revision IS NULL
FROM task_agent_stall_notices n JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id
WHERE n.task_id=$1 AND n.message_id=$3`, value.ID, revision, messageID).
		Scan(&noticeRevisionMatches, &stepRevisionIsNull); err != nil || !noticeRevisionMatches || !stepRevisionIsNull {
		t.Fatalf("stall notice revision ownership = notice_matches:%t step_revision_null:%t err:%v",
			noticeRevisionMatches, stepRevisionIsNull, err)
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

func TestPostgresTaskAgentModelCallStallUsesProgressDespiteFreshHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name                 string
		phase                string
		progressStale        bool
		heartbeatStale       bool
		terminal             bool
		child                bool
		idleStarted          bool
		preserveNoticeOnIdle bool
		wantNotice           bool
	}{
		{name: "within model timeout"},
		{name: "started call stalled", progressStale: true, wantNotice: true},
		{name: "terminal call does not stall", progressStale: true, terminal: true},
		{name: "child started call stalled", progressStale: true, child: true, wantNotice: true},
		{name: "child idle started snapshot remains unknown", progressStale: true, child: true, idleStarted: true},
		{name: "tool progress age alone does not stall", phase: "tool", progressStale: true},
		{name: "root idle started tool keeps existing notice unresolved", phase: "tool", heartbeatStale: true, preserveNoticeOnIdle: true, wantNotice: true},
		{name: "child idle started tool keeps existing notice unresolved", phase: "tool", heartbeatStale: true, child: true, preserveNoticeOnIdle: true, wantNotice: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			var revision int64
			if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, details.Task.ID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			activitySessionID := lease.Fence.AgentSessionID
			sequence := int64(0)
			record := func(kind, phase, state, activityID, status string) {
				t.Helper()
				sequence++
				accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
					Fence: lease.Fence, AgentSessionID: activitySessionID, TaskRevision: revision,
					Kind: kind, Phase: phase, State: state, ActivityID: activityID, Status: status,
					ObservationSequence: sequence,
				})
				if err != nil || !accepted {
					t.Fatalf("record activity %s/%s: accepted=%t err=%v", phase, state, accepted, err)
				}
			}
			if test.child {
				record(app.TaskAgentActivityKindStatus, "", "", "", "running")
				record(app.TaskAgentActivityKindHeartbeat, "", "", "", "")
				children, err := app.NewAgentChildSessionService(f.store)
				if err != nil {
					t.Fatal(err)
				}
				child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{
					Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: "model-stall-child",
					RootCheckpointThroughSequence: rootAgentCheckpointBaselineForTest(t, f.store, details.Task.ID,
						lease.Fence.OwnerID, lease.Fence.AgentSessionID),
					Arguments: []byte(`{"objective":"wait for model response"}`), Title: "Wait for model response",
					Summary: "Wait for a model response.", AllowedTools: []string{"file_read"},
				})
				if err != nil || replayed {
					t.Fatalf("create child session: child=%+v replayed=%t err=%v", child, replayed, err)
				}
				activitySessionID = child.SessionID
				sequence = 0
			}
			phase := test.phase
			if phase == "" {
				phase = "model"
			}
			record(app.TaskAgentActivityKindStatus, "", "", "", "running")
			record(app.TaskAgentActivityKindActivity, phase, "started", "activity-1", "")
			record(app.TaskAgentActivityKindHeartbeat, phase, "", "activity-1", "")
			if test.terminal {
				record(app.TaskAgentActivityKindActivity, phase, "completed", "activity-1", "")
			}
			if test.idleStarted && !test.preserveNoticeOnIdle {
				if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET activity_status='idle'
WHERE task_id=$1 AND session_id=$2`, details.Task.ID, activitySessionID); err != nil {
					t.Fatal(err)
				}
			}

			progressAge := int64(0)
			if test.progressStale {
				progressAge = (app.ModelCallTimeout + time.Second).Milliseconds()
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions
SET activity_progressed_at=clock_timestamp()-($3::bigint*interval '1 millisecond'),activity_heartbeat_at=clock_timestamp()
WHERE task_id=$1 AND session_id=$2`, details.Task.ID, activitySessionID, progressAge); err != nil {
				t.Fatal(err)
			}
			if test.heartbeatStale {
				if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions
SET activity_heartbeat_at=clock_timestamp()-interval '11 minutes' WHERE task_id=$1 AND session_id=$2`,
					details.Task.ID, activitySessionID); err != nil {
					t.Fatal(err)
				}
			} else {
				var heartbeatFresh bool
				if err := f.store.pool.QueryRow(ctx, `SELECT activity_heartbeat_at>clock_timestamp()-interval '1 minute'
FROM task_agent_sessions WHERE task_id=$1 AND session_id=$2`, details.Task.ID, activitySessionID).Scan(&heartbeatFresh); err != nil || !heartbeatFresh {
					t.Fatalf("heartbeat is not fresh: fresh=%t err=%v", heartbeatFresh, err)
				}
			}

			check := func() {
				t.Helper()
				if err := f.store.CheckTaskAgentStall(ctx, details.Task, 10*time.Minute); err != nil {
					t.Fatalf("CheckTaskAgentStall(): %v", err)
				}
			}
			check()
			var activeNotices int
			if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_stall_notices
WHERE task_id=$1 AND state='active'`, details.Task.ID).Scan(&activeNotices); err != nil {
				t.Fatal(err)
			}
			wantActive := 0
			if test.wantNotice {
				wantActive = 1
			}
			if activeNotices != wantActive {
				t.Fatalf("active stall notices=%d, want %d", activeNotices, wantActive)
			}
			if !test.wantNotice {
				return
			}
			if test.preserveNoticeOnIdle {
				if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET activity_status='idle',activity_heartbeat_at=clock_timestamp()
WHERE task_id=$1 AND session_id=$2`, details.Task.ID, activitySessionID); err != nil {
					t.Fatal(err)
				}
				check()
				if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_stall_notices
WHERE task_id=$1 AND state='active'`, details.Task.ID).Scan(&activeNotices); err != nil || activeNotices != 1 {
					t.Fatalf("unknown idle/started snapshot cleared notice: active=%d err=%v", activeNotices, err)
				}
				return
			}

			check()
			if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_stall_notices WHERE task_id=$1`, details.Task.ID).Scan(&activeNotices); err != nil || activeNotices != 1 {
				t.Fatalf("repeated stale check notices=%d, err=%v; want one notice", activeNotices, err)
			}
			record(app.TaskAgentActivityKindActivity, phase, "completed", "activity-1", "")
			var noticeState, stepStatus string
			if err := f.store.pool.QueryRow(ctx, `SELECT n.state,s.status FROM task_agent_stall_notices n
JOIN task_steps s ON s.task_id=n.task_id AND s.id=n.step_id WHERE n.task_id=$1`, details.Task.ID).Scan(&noticeState, &stepStatus); err != nil ||
				noticeState != "cleared" || stepStatus != "succeeded" {
				t.Fatalf("activity recovery notice=(%q,%q), err=%v", noticeState, stepStatus, err)
			}
		})
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
