package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestPostgresTaskAgentActivityIsOrderedAndFenced(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var taskRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, lease.Fence.TaskID).Scan(&taskRevision); err != nil {
		t.Fatal(err)
	}
	record := func(sequence int64, status string) (bool, error) {
		return f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
			Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: taskRevision,
			Kind: app.TaskAgentActivityKindStatus, Status: status, ObservationSequence: sequence,
		})
	}
	if accepted, err := record(1, "idle"); err != nil || !accepted {
		t.Fatalf("record initial idle status: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := record(1, "idle"); err != nil || accepted {
		t.Fatalf("duplicate status was not ignored: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := record(3, "running"); err != nil || !accepted {
		t.Fatalf("record running status: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: taskRevision,
		Kind: app.TaskAgentActivityKindActivity, Phase: "tool", State: "waiting", ActivityID: "call_sensitive", ObservationSequence: 4,
	}); err != nil || !accepted {
		t.Fatalf("record waiting tool activity: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := record(2, "idle"); err != nil || accepted {
		t.Fatalf("out-of-order status was not ignored: accepted=%t err=%v", accepted, err)
	}
	var status string
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT activity_status,activity_sequence FROM task_agent_sessions
WHERE task_id=$1 AND session_id=$2`, lease.Fence.TaskID, lease.Fence.AgentSessionID).Scan(&status, &sequence); err != nil {
		t.Fatal(err)
	}
	if status != "running" || sequence != 4 {
		t.Fatalf("stored activity=(%s,%d), want (running,4)", status, sequence)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatalf("load task with current waiting activity: %v", err)
	}
	if len(details.Agents) == 0 || details.Agents[0].ActivityPhase != "tool" || details.Agents[0].ActivityState != "waiting" {
		t.Fatalf("current waiting activity was not projected: %+v", details.Agents)
	}
	var events int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events
WHERE task_id=$1 AND event_type='task.step' AND payload->>'phase'='agent_status'`, lease.Fence.TaskID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("agent activity events=%d, want 2 state transitions", events)
	}
	staleGeneration, wrongSession, revokedAuthorization := lease.Fence, lease.Fence, lease.Fence
	staleGeneration.AgentGeneration++
	wrongSession.AgentSessionID = "session_stale"
	revokedAuthorization.AuthorizationVersion++
	for name, fence := range map[string]app.AgentInstanceFence{
		"stale generation": staleGeneration, "wrong session": wrongSession, "stale authorization": revokedAuthorization,
	} {
		accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
			Fence: fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: taskRevision,
			Kind: app.TaskAgentActivityKindStatus, Status: "idle", ObservationSequence: 5,
		})
		if err != nil || accepted {
			t.Fatalf("accepted %s activity fence: accepted=%t err=%v", name, accepted, err)
		}
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET lease_expires_at=$3
WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if accepted, err := record(5, "idle"); err != nil || accepted {
		t.Fatalf("expired agent lease accepted activity: accepted=%t err=%v", accepted, err)
	}
	details, err = f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatalf("load task after agent lease expiry: %v", err)
	}
	if len(details.Agents) == 0 || details.Agents[0].Status != "unknown" || details.Agents[0].ObservedAt != nil ||
		details.Agents[0].ActivityPhase != "" || details.Agents[0].ActivityState != "" {
		t.Fatalf("expired agent activity was presented as live: %+v", details.Agents)
	}
}

func TestPostgresTaskAgentActivityProjectionRequiresCurrentRevision(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var taskRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, lease.Fence.TaskID).Scan(&taskRevision); err != nil {
		t.Fatal(err)
	}
	if accepted, err := f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
		Fence: lease.Fence, AgentSessionID: lease.Fence.AgentSessionID, TaskRevision: taskRevision,
		Kind: app.TaskAgentActivityKindActivity, Phase: "tool", State: "waiting", ActivityID: "call_sensitive", ObservationSequence: 1,
	}); err != nil || !accepted {
		t.Fatalf("record current waiting tool activity: accepted=%t err=%v", accepted, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET activity_revision=CASE WHEN activity_revision=0 THEN 1 ELSE activity_revision-1 END
WHERE task_id=$1 AND session_id=$2`, lease.Fence.TaskID, lease.Fence.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatalf("load task with stale activity revision: %v", err)
	}
	if len(details.Agents) == 0 || details.Agents[0].ActivityPhase != "" || details.Agents[0].ActivityState != "" {
		t.Fatalf("untrusted activity revision was projected: %+v", details.Agents)
	}
}

func TestPostgresTaskAgentActivityHeartbeatsDoNotAdvanceProgress(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var taskRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, lease.Fence.TaskID).Scan(&taskRevision); err != nil {
		t.Fatal(err)
	}
	record := func(observation app.TaskAgentActivityObservation) (bool, error) {
		observation.Fence = lease.Fence
		observation.AgentSessionID = lease.Fence.AgentSessionID
		observation.TaskRevision = taskRevision
		return f.store.RecordTaskAgentActivity(ctx, observation)
	}
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindStatus, Status: "running", ObservationSequence: 1}); err != nil || !accepted {
		t.Fatalf("record running status: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindActivity,
		Phase: "model", State: "waiting", ActivityID: "model-1", ObservationSequence: 2}); err != nil || !accepted {
		t.Fatalf("record model wait phase: accepted=%t err=%v", accepted, err)
	}
	var progressAt, observedAt time.Time
	var heartbeatPresent bool
	var phase, state, activityID string
	var revision int64
	read := func() {
		t.Helper()
		if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(activity_phase,''),COALESCE(activity_state,''),COALESCE(activity_id,''),activity_revision,
activity_progressed_at,activity_heartbeat_at IS NOT NULL,activity_observed_at FROM task_agent_sessions
WHERE task_id=$1 AND session_id=$2`, lease.Fence.TaskID, lease.Fence.AgentSessionID).
			Scan(&phase, &state, &activityID, &revision, &progressAt, &heartbeatPresent, &observedAt); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if phase != "model" || state != "waiting" || activityID != "model-1" || revision != taskRevision || progressAt.IsZero() || heartbeatPresent {
		t.Fatalf("persisted model activity = (%q,%q,%q,%d,%v,heartbeat=%t)", phase, state, activityID, revision, progressAt, heartbeatPresent)
	}
	firstProgressAt := progressAt
	time.Sleep(time.Millisecond)
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindHeartbeat,
		Phase: "model", ActivityID: "model-1", ObservationSequence: 3}); err != nil || !accepted {
		t.Fatalf("record model heartbeat: accepted=%t err=%v", accepted, err)
	}
	read()
	if progressAt != firstProgressAt || !heartbeatPresent || !observedAt.After(firstProgressAt) {
		t.Fatalf("heartbeat advanced progress or failed to refresh liveness: progress=%v heartbeat_present=%t observed=%v; first=%v",
			progressAt, heartbeatPresent, observedAt, firstProgressAt)
	}
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindActivity,
		Phase: "model", State: "completed", ActivityID: "model-1", ObservationSequence: 4}); err != nil || !accepted {
		t.Fatalf("record model completion: accepted=%t err=%v", accepted, err)
	}
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindStatus, Status: "idle", ObservationSequence: 5}); err != nil || !accepted {
		t.Fatalf("record idle status: accepted=%t err=%v", accepted, err)
	}
	read()
	if phase != "" || state != "" || activityID != "" || !progressAt.After(firstProgressAt) {
		t.Fatalf("idle activity projection = (%q,%q,%q,%v)", phase, state, activityID, progressAt)
	}
	if accepted, err := record(app.TaskAgentActivityObservation{Kind: app.TaskAgentActivityKindHeartbeat,
		Phase: "model", ActivityID: "model-1", ObservationSequence: 6}); err == nil || accepted {
		t.Fatalf("stale phase heartbeat accepted: accepted=%t err=%v", accepted, err)
	}
}
