package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
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

func TestPostgresTaskAgentStallWaitsWhenExactRootRuntimeLeaseExpires(t *testing.T) {
	f, value, runtime, _ := newTaskAgentRootRuntimeStallFixture(t)
	ctx := context.Background()
	stepID := addRunningTaskAgentStallStep(t, ctx, f.store, value.ID)
	noticeStepID := addActiveTaskAgentStallNotice(t, ctx, f.store, value)
	if _, err := f.store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND conversation_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		runtime.Fence.ServiceInstanceID, runtime.Fence.ConversationID, runtime.Fence.RuntimeID,
		runtime.Fence.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); !taskAgentRootRuntimeUnavailableTestSignal(err) {
		t.Fatalf("CheckTaskAgentStall() error = %v, want committed root Runtime recovery signal", err)
	}
	assertTaskAgentStallWaitingState(t, ctx, f.store, value.ID, stepID)
	var noticeState, noticeStepStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT notice.state,step.status FROM task_agent_stall_notices notice
JOIN task_steps step ON step.task_id=notice.task_id AND step.id=notice.step_id
WHERE notice.task_id=$1 AND notice.step_id=$2`, value.ID, noticeStepID).Scan(&noticeState, &noticeStepStatus); err != nil ||
		noticeState != "cleared" || noticeStepStatus != string(task.StepUnknown) {
		t.Fatalf("preexisting activity notice = (%q,%q), err=%v", noticeState, noticeStepStatus, err)
	}
}

func TestPostgresTaskAgentStallWaitsWhenRootRuntimeStopProofIsVerified(t *testing.T) {
	f, value, runtime, runtimeService := newTaskAgentRootRuntimeStallFixture(t)
	ctx := context.Background()
	stepID := addRunningTaskAgentStallStep(t, ctx, f.store, value.ID)
	if _, err := runtimeService.RequestStop(ctx, runtime.Fence); err != nil {
		t.Fatalf("request exact root Runtime stop: %v", err)
	}
	identity, err := runtimeService.StopIdentity(ctx, runtime.Fence.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAccountRuntimeStopProof(t, identity, privateKey)
	verifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if replayed, err := runtimeService.ConfirmGuardianStopped(ctx, proof, verifier); err != nil || replayed {
		t.Fatalf("persist verified root Runtime stop: replayed=%t err=%v", replayed, err)
	}
	if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); !taskAgentRootRuntimeUnavailableTestSignal(err) {
		t.Fatalf("CheckTaskAgentStall() error = %v, want committed root Runtime recovery signal", err)
	}
	assertTaskAgentStallWaitingState(t, ctx, f.store, value.ID, stepID)
}

func TestPostgresTaskAgentStallDoesNotWaitOnUnprovenOrMismatchedRuntime(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(t *testing.T, ctx context.Context, f agentLifecycleFixture, runtime app.AccountAgentRuntime)
	}{
		{name: "mismatched generation", edit: func(t *testing.T, ctx context.Context, f agentLifecycleFixture, runtime app.AccountAgentRuntime) {
			if _, err := f.store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET runtime_generation=runtime_generation+1
WHERE service_instance_id=$1::uuid AND conversation_id=$2 AND runtime_id=$3`,
				runtime.Fence.ServiceInstanceID, runtime.Fence.ConversationID, runtime.Fence.RuntimeID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid stopped proof", edit: func(t *testing.T, ctx context.Context, f agentLifecycleFixture, runtime app.AccountAgentRuntime) {
			if _, err := f.store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET state='stopped',lease_expires_at=NULL,
stopped_at=clock_timestamp(),guardian_stop_attestation=convert_to('invalid-proof','UTF8'),
guardian_stop_attestation_digest=sha256(convert_to('invalid-proof','UTF8'))
WHERE service_instance_id=$1::uuid AND conversation_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
				runtime.Fence.ServiceInstanceID, runtime.Fence.ConversationID, runtime.Fence.RuntimeID,
				runtime.Fence.RuntimeGeneration); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, value, runtime, _ := newTaskAgentRootRuntimeStallFixture(t)
			ctx := context.Background()
			stepID := addRunningTaskAgentStallStep(t, ctx, f.store, value.ID)
			test.edit(t, ctx, f, runtime)
			if err := f.store.CheckTaskAgentStall(ctx, value, time.Second); err != nil {
				t.Fatalf("CheckTaskAgentStall() error = %v, want no transition", err)
			}
			assertTaskAgentStallRunningState(t, ctx, f.store, value.ID, stepID)
		})
	}
}

func TestPostgresTaskAgentStallQueryFailureDoesNotWait(t *testing.T) {
	f, value, runtime, _ := newTaskAgentRootRuntimeStallFixture(t)
	ctx := context.Background()
	stepID := addRunningTaskAgentStallStep(t, ctx, f.store, value.ID)
	if _, err := f.store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND conversation_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		runtime.Fence.ServiceInstanceID, runtime.Fence.ConversationID, runtime.Fence.RuntimeID,
		runtime.Fence.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.store.CheckTaskAgentStall(canceled, value, time.Second); err == nil {
		t.Fatal("CheckTaskAgentStall() with a canceled query context succeeded")
	}
	assertTaskAgentStallRunningState(t, ctx, f.store, value.ID, stepID)
}

func newTaskAgentRootRuntimeStallFixture(t *testing.T) (agentLifecycleFixture, task.Task, app.AccountAgentRuntime,
	*app.AccountAgentRuntimeService) {
	t.Helper()
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 94
	runtime, replayed, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{
		OwnerID: f.request.Fence.OwnerID, ConversationID: runtimeTaskConversation(t, f.store, f.request.Fence.TaskID),
		RuntimeID: "task-agent-stall-runtime", LeaseOwner: "task-agent-stall-runtime-worker",
		SupervisorNonce: nonce, MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed {
		t.Fatalf("start root Agent Runtime: runtime=%+v replayed=%t err=%v", runtime, replayed, err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatalf("mark root Agent Runtime ready: %v", err)
	}
	f.request.RuntimeFence = &runtime.Fence
	lease, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed || lease.RuntimeFence == nil || *lease.RuntimeFence != runtime.Fence {
		t.Fatalf("start Runtime-bound Task Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	return f, details.Task, runtime, runtimeService
}

func addRunningTaskAgentStallStep(t *testing.T, ctx context.Context, store *Store, taskID string) string {
	t.Helper()
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, taskID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'tool','running','Pending operation','Waiting for operation result.')`, stepID, taskID, sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_step=$2 WHERE id=$1`, taskID, stepID); err != nil {
		t.Fatal(err)
	}
	return stepID
}

func addActiveTaskAgentStallNotice(t *testing.T, ctx context.Context, store *Store, value task.Task) string {
	t.Helper()
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, value.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'agent','running','任务仍在运行','Agent 活动仍待确认。')`, stepID, value.ID, sequence); err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND owner_id=$2
AND conversation_id=$3 AND role='user' ORDER BY sequence,id LIMIT 1`, value.ID, value.OwnerID,
		value.ConversationID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_stall_notices
(task_id,owner_id,message_id,step_id,task_revision,agent_generation,state,notice_kind)
VALUES ($1,$2,$3,$4,$5,$6,'active','activity_stall')`, value.ID, value.OwnerID, messageID, stepID,
		*value.CurrentRevision, value.AgentGeneration); err != nil {
		t.Fatal(err)
	}
	return stepID
}

func taskAgentRootRuntimeUnavailableTestSignal(err error) bool {
	var domainErr *domain.Error
	return errors.As(err, &domainErr) && domainErr.Code == domain.CodeInvalidState &&
		domainErr.Details["task_agent_stall"] == "root_runtime_unavailable"
}

func assertTaskAgentStallWaitingState(t *testing.T, ctx context.Context, store *Store, taskID, stepID string) {
	t.Helper()
	var status, waitingReason, leaseOwner, stepStatus string
	var leaseCleared bool
	if err := store.pool.QueryRow(ctx, `SELECT status,waiting_reason,lease_owner,lease_expires_at IS NULL
FROM tasks WHERE id=$1`, taskID).Scan(&status, &waitingReason, &leaseOwner, &leaseCleared); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusWaitingUser) || waitingReason != "dsh_recovery_unresolved" || leaseOwner != "" || !leaseCleared {
		t.Fatalf("recovery wait = status:%q reason:%q lease_owner:%q lease_cleared:%t", status, waitingReason, leaseOwner, leaseCleared)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_steps WHERE task_id=$1 AND id=$2`, taskID, stepID).Scan(&stepStatus); err != nil || stepStatus != string(task.StepUnknown) {
		t.Fatalf("unresolved current step = %q, err=%v", stepStatus, err)
	}
	var sources int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox source
JOIN task_events event ON event.task_id=source.task_id AND event.sequence=source.source_event_sequence
WHERE source.task_id=$1 AND source.source_kind='waiting_unknown' AND source.source_event_type='task.waiting_user'
  AND event.payload->>'reason'='dsh_recovery_unresolved'`, taskID).Scan(&sources); err != nil || sources != 1 {
		t.Fatalf("waiting_unknown delivery sources = %d, err=%v", sources, err)
	}
}

func assertTaskAgentStallRunningState(t *testing.T, ctx context.Context, store *Store, taskID, stepID string) {
	t.Helper()
	var status, waitingReason, leaseOwner, stepStatus string
	var leasePresent bool
	if err := store.pool.QueryRow(ctx, `SELECT status,waiting_reason,lease_owner,lease_expires_at IS NOT NULL
FROM tasks WHERE id=$1`, taskID).Scan(&status, &waitingReason, &leaseOwner, &leasePresent); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusRunning) || waitingReason != "" || leaseOwner == "" || !leasePresent {
		t.Fatalf("Task changed without proven root Runtime loss: status:%q reason:%q lease_owner:%q lease_present:%t", status, waitingReason, leaseOwner, leasePresent)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_steps WHERE task_id=$1 AND id=$2`, taskID, stepID).Scan(&stepStatus); err != nil || stepStatus != string(task.StepRunning) {
		t.Fatalf("unknown root Runtime changed current step to %q, err=%v", stepStatus, err)
	}
	var sources int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1 AND source_kind='waiting_unknown'`, taskID).Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("unknown root Runtime emitted waiting_unknown source: count=%d err=%v", sources, err)
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
