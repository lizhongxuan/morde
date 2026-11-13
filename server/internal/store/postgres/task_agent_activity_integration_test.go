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
	record := func(sequence int64, status string) (bool, error) {
		return f.store.RecordTaskAgentActivity(ctx, app.TaskAgentActivityObservation{
			Fence: lease.Fence, Status: status, ObservationSequence: sequence,
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
	if accepted, err := record(2, "idle"); err != nil || accepted {
		t.Fatalf("out-of-order status was not ignored: accepted=%t err=%v", accepted, err)
	}
	var status string
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT activity_status,activity_sequence FROM task_agent_sessions
WHERE task_id=$1 AND session_id=$2`, lease.Fence.TaskID, lease.Fence.AgentSessionID).Scan(&status, &sequence); err != nil {
		t.Fatal(err)
	}
	if status != "running" || sequence != 3 {
		t.Fatalf("stored activity=(%s,%d), want (running,3)", status, sequence)
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
			Fence: fence, Status: "idle", ObservationSequence: 4,
		})
		if err != nil || accepted {
			t.Fatalf("accepted %s activity fence: accepted=%t err=%v", name, accepted, err)
		}
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET lease_expires_at=$3
WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if accepted, err := record(4, "idle"); err != nil || accepted {
		t.Fatalf("expired agent lease accepted activity: accepted=%t err=%v", accepted, err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatalf("load task after agent lease expiry: %v", err)
	}
	if len(details.Agents) == 0 || details.Agents[0].Status != "unknown" || details.Agents[0].ObservedAt != nil {
		t.Fatalf("expired agent activity was presented as live: %+v", details.Agents)
	}
}
