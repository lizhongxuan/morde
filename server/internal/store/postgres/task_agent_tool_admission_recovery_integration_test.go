package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestDurableRecoveredToolAdmissionRebindsCheckpointUnderNewAgentGeneration(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-recovery-admission-rebind", false)
	callID := "call_s03_recovery_admission_rebind"
	call := fixture.call(callID, "web_search", `{"query":"settled recovery search"}`)
	if admission, err := fixture.route.store.AdmitAgentToolCall(ctx, call, false); err != nil || admission.Status != app.AgentToolCallAdmitted {
		t.Fatalf("initial tool-call admission = %+v err=%v", admission, err)
	}

	recovered := fixture.modelFence
	recovered.AgentInstanceID = "s03-agent-instance-recovery"
	recovered.AgentGeneration++
	recovered.ThroughSequence = 15
	if err := fixture.route.store.WithTx(ctx, func(tx pgx.Tx) error {
		return persistTaskAgentToolCallAdmission(ctx, tx, recovered, app.AgentOperationKey(callID),
			recovered.AgentRootSessionID, recovered.ThroughSequence, true)
	}); err != nil {
		t.Fatalf("rebind a durable recovered tool admission: %v", err)
	}

	var instance, rootSession, callSession string
	var generation, through, rootThrough int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,root_session_id,call_session_id,
through_sequence,root_through_sequence FROM task_agent_tool_call_admissions WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3`,
		fixture.task.ID, fixture.task.OwnerID, app.AgentOperationKey(callID)).
		Scan(&instance, &generation, &rootSession, &callSession, &through, &rootThrough); err != nil {
		t.Fatalf("read rebound tool admission: %v", err)
	}
	if instance != recovered.AgentInstanceID || generation != recovered.AgentGeneration || rootSession != recovered.AgentRootSessionID ||
		callSession != recovered.AgentSessionID || through != recovered.ThroughSequence || rootThrough != recovered.ThroughSequence {
		t.Fatalf("recovered tool admission fence = instance=%s generation=%d root=%s session=%s through=%d root_through=%d",
			instance, generation, rootSession, callSession, through, rootThrough)
	}

	for name, testCase := range map[string]struct {
		fence          app.ModelTaskFence
		allowRebind    bool
	}{
		"changed session remains rejected": {fence: func() app.ModelTaskFence {
			value := recovered
			value.AgentGeneration++
			value.AgentInstanceID = "s03-agent-instance-session-change"
			value.AgentSessionID = "session_s03_changed"
			value.ThroughSequence++
			return value
		}(), allowRebind: true},
		"same generation checkpoint change remains rejected": {fence: func() app.ModelTaskFence {
			value := recovered
			value.ThroughSequence++
			return value
		}(), allowRebind: true},
		"checkpoint change without durable recovery proof remains rejected": {fence: func() app.ModelTaskFence {
			value := recovered
			value.AgentGeneration++
			value.AgentInstanceID = "s03-agent-instance-unproven"
			value.ThroughSequence++
			return value
		}(), allowRebind: false},
	} {
		t.Run(name, func(t *testing.T) {
			err := fixture.route.store.WithTx(ctx, func(tx pgx.Tx) error {
				return persistTaskAgentToolCallAdmission(ctx, tx, testCase.fence, app.AgentOperationKey(callID),
					recovered.AgentRootSessionID, testCase.fence.ThroughSequence, testCase.allowRebind)
			})
			if domain.CodeOf(err) != domain.CodeIdempotencyConflict {
				t.Fatalf("unproven admission rebind error = %v; want idempotency conflict", err)
			}
		})
	}
}
