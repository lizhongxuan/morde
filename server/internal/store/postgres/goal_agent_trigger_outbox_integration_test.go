package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain/personal"
)

func TestPostgresGoalAgentFollowupOutboxRespectsOptInAndCurrentGoal(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	goalID := "goal-auto-followup-1"

	apply := func(mutationID, objectType, objectID string, revision int64, body string) {
		t.Helper()
		mutation := personalObjectMutationForTest(t, owner.ID, mutationID, objectType, objectID, revision, body, false)
		if _, err := store.ApplyPersonalMutation(ctx, principal, mutation); err != nil {
			t.Fatalf("apply %s revision %d: %v", objectID, revision, err)
		}
	}
	goalBody := func(step1, step2 string) string {
		return `{"title":"Project launch","description":"Finish launch","status":"active","steps":[{"id":"step_1","title":"Prepare release","status":"` + step1 + `","note":"Review checklist"},{"id":"step_2","title":"Publish release","status":"` + step2 + `"}]}`
	}
	apply("goal-auto-create-1", personal.ObjectTypeGoal, goalID, 0, goalBody("pending", "pending"))
	apply("goal-auto-block-off-1", personal.ObjectTypeGoal, goalID, 1, goalBody("blocked", "pending"))
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM goal_agent_trigger_events WHERE owner_id=$1`, owner.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("events with opt-in missing = %d, %v; want 0", count, err)
	}

	apply("goal-auto-opt-in-1", personal.ObjectTypeSkillSetting, "builtin_goal_followups", 0, `{"enabled":true}`)
	blockedStep2 := goalBody("blocked", "blocked")
	apply("goal-auto-block-on-1", personal.ObjectTypeGoal, goalID, 2, blockedStep2)
	blockedMutation := personalObjectMutationForTest(t, owner.ID, "goal-auto-block-on-1", personal.ObjectTypeGoal, goalID, 2, blockedStep2, false)
	if receipt, err := store.ApplyPersonalMutation(ctx, principal, blockedMutation); err != nil || !receipt.Replay {
		t.Fatalf("replay blocked Goal update = %#v, %v; want idempotent replay", receipt, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM goal_agent_trigger_events WHERE owner_id=$1`, owner.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events after newly blocked step = %d, %v; want 1", count, err)
	}

	// Automatic follow-ups stay durable but wait while the account runtime is off.
	if events, err := store.ClaimGoalAgentTriggerEvents(ctx, "test-goal-worker", 2*time.Minute, 10); err != nil || len(events) != 0 {
		t.Fatalf("claim while owner execution is disabled = %d, %v; want 0", len(events), err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled',epoch=epoch+1 WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner execution for trigger test: %v", err)
	}
	events, err := store.ClaimGoalAgentTriggerEvents(ctx, "test-goal-worker", 2*time.Minute, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("claim Goal trigger = %d, %v; want 1", len(events), err)
	}
	if duplicate, err := store.ClaimGoalAgentTriggerEvents(ctx, "other-goal-worker", 2*time.Minute, 10); err != nil || len(duplicate) != 0 {
		t.Fatalf("concurrent claim duplicated a leased Goal trigger: %#v, %v", duplicate, err)
	}
	if events[0].Authorization.Kind != "session" || events[0].Authorization.TokenID != principal.TokenID ||
		len(events[0].Authorization.Scopes) != 0 || events[0].Authorization.ClientInstallationID != "" {
		t.Fatalf("trigger authorization snapshot is not minimal: %#v", events[0].Authorization)
	}
	loaded, ready, err := store.LoadGoalAgentTriggerEvent(ctx, events[0])
	if err != nil || !ready || loaded.GoalTitle != "Project launch" || loaded.StepTitle != "Publish release" || loaded.StepNote != "" {
		t.Fatalf("load authorized trigger = %#v, %v, %v", loaded, ready, err)
	}

	// Turning the consent off after claim prevents schedule creation.
	apply("goal-auto-opt-out-1", personal.ObjectTypeSkillSetting, "builtin_goal_followups", 1, `{"enabled":false}`)
	if _, ready, err := store.LoadGoalAgentTriggerEvent(ctx, events[0]); err != nil || ready {
		t.Fatalf("load after opt-out = ready %v, err %v; want false", ready, err)
	}
	if ok, err := store.SkipGoalAgentTriggerEvent(ctx, events[0], "setting_disabled_or_goal_changed"); err != nil || !ok {
		t.Fatalf("skip opted-out trigger = %v, %v", ok, err)
	}

	// A subsequent explicit opt-in creates a fresh trigger only for a new
	// transition to blocked; that event becomes stale if the step is unblocked.
	apply("goal-auto-opt-in-2", personal.ObjectTypeSkillSetting, "builtin_goal_followups", 2, `{"enabled":true}`)
	apply("goal-auto-unblock-1", personal.ObjectTypeGoal, goalID, 3, goalBody("blocked", "pending"))
	apply("goal-auto-reblock-1", personal.ObjectTypeGoal, goalID, 4, goalBody("blocked", "blocked"))
	events, err = store.ClaimGoalAgentTriggerEvents(ctx, "test-goal-worker", 2*time.Minute, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("claim second Goal trigger = %d, %v; want 1", len(events), err)
	}
	second := events[0]
	if second.GoalRevision != 5 || second.StepID != "step_2" {
		t.Fatalf("second trigger identity = revision %d step %q; want revision 5 step_2", second.GoalRevision, second.StepID)
	}
	apply("goal-auto-unblock-2", personal.ObjectTypeGoal, goalID, 5, goalBody("blocked", "pending"))
	if _, ready, err := store.LoadGoalAgentTriggerEvent(ctx, second); err != nil || ready {
		t.Fatalf("load after Goal revision changed = ready %v, err %v; want false", ready, err)
	}
	if ok, err := store.SkipGoalAgentTriggerEvent(ctx, second, "setting_disabled_or_goal_changed"); err != nil || !ok {
		t.Fatalf("skip stale Goal trigger = %v, %v", ok, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM goal_agent_trigger_events WHERE owner_id=$1 AND status='skipped'`, owner.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("terminal stale/opt-out triggers = %d, %v; want 2", count, err)
	}
}
