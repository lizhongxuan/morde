package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/dshguardian"
)

func TestCloudDSHCancellingParentChildLeavesSiblingTaskRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify Task session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Start a background child, then wait for cancellation.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 8, ModelTokens: 1000}, IdempotencyKey: "f01-parent-sibling-cancel-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ConversationID: parent.Conversation.ID, ModelID: "p1234567", Objective: "Finish independently after the parent is stopped.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 1000}, IdempotencyKey: "f01-parent-sibling-cancel-sibling",
	})
	if err != nil {
		t.Fatal(err)
	}

	node, script := f01NodeRuntimePaths(t)
	accountScript := filepath.Join(filepath.Dir(script), "bin-account.mjs")
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stopKey := bytes.Repeat([]byte{0x72}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorAuth, err := app.NewAgentStopAuthenticator("f01-parent-sibling-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator("f01-parent-sibling-supervisor", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointObserver := &f01ObservedCheckpoints{AgentCheckpointService: checkpoints}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	childObserver := &f01ObservedChildSessions{CloudDSHChildSessions: children}
	model := newParentSiblingCancelModel(parent.Task.ID, sibling.Task.ID)
	tools := &cancelRestartTools{definition: cloudDSHWriteTool()}
	guardian := &f01GatedSignedNodeTaskGuardian{
		f01SignedNodeTaskGuardian: &f01SignedNodeTaskGuardian{key: privateKey},
		taskID:                    parent.Task.ID,
		stopStarted:               make(chan struct{}),
		allowStop:                 make(chan struct{}),
	}
	var releaseParentStop sync.Once
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSupervisor := &f01RecoveringNodeAccountSupervisor{
		f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
		recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
	}
	defer func() {
		if command := runtimeSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	runtimeManager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: runtimeSupervisor, Guardian: guardian, StopVerifier: runtimeStopVerifier,
		TaskStopper: instances, LeaseOwner: "f01-parent-sibling-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	completionErrors := make(chan error, 1)
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: "f01-parent-sibling-supervisor",
		AccountRuntimes: runtimeManager,
		Instances:       instances, Checkpoints: checkpointObserver, Children: childObserver, Models: model, Tools: tools,
		Completion: &parentSiblingCancelCompletion{store: store, errors: completionErrors},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "f01-parent-sibling-worker", LeaseDuration: 3 * time.Second,
		PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store, Executor: taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
			return task.OperationCancelled, "stopped by integration test", false, nil
		}),
		WorkerID: "f01-parent-sibling-stop-worker", LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond,
		OperationTimeout: time.Second, RetryAfter: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	defer func() {
		cancelWorker()
		select {
		case runErr := <-workerDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Errorf("TaskWorker.Run() = %v, want context canceled during cleanup", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()
	stopCtx, cancelStop := context.WithCancel(context.Background())
	stopDone := make(chan error, 1)
	go func() { stopDone <- stopWorker.Run(stopCtx) }()
	defer func() {
		cancelStop()
		select {
		case runErr := <-stopDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Errorf("TaskStopWorker.Run() = %v, want context canceled during cleanup", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskStopWorker did not stop")
		}
	}()
	defer releaseParentStop.Do(func() { close(guardian.allowStop) })
	var activityMu sync.Mutex
	var activitySamples []string
	activityStop := make(chan struct{})
	defer close(activityStop)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-activityStop:
				return
			case <-ticker.C:
				diagnosticCtx, cancelDiagnostic := context.WithTimeout(context.Background(), time.Second)
				rows, err := pool.Query(diagnosticCtx, `SELECT pid,state,COALESCE(wait_event_type,''),COALESCE(wait_event,''),
pg_blocking_pids(pid)::text,left(query,240) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
AND state='active' AND wait_event_type='Lock' AND query NOT LIKE '%pg_stat_activity%' ORDER BY pid`)
				if err == nil {
					var found []string
					for rows.Next() {
						var pid int
						var state, waitType, waitEvent, blocking, query string
						if scanErr := rows.Scan(&pid, &state, &waitType, &waitEvent, &blocking, &query); scanErr != nil {
							found = append(found, "scan_error="+scanErr.Error())
							break
						}
						found = append(found, fmt.Sprintf("pid=%d state=%s wait=%s/%s blockers=%s query=%q", pid, state, waitType, waitEvent, blocking, query))
					}
					if rows.Err() != nil {
						found = append(found, "iterate_error="+rows.Err().Error())
					}
					rows.Close()
					if len(found) > 0 {
						activityMu.Lock()
						activitySamples = append(activitySamples, found...)
						if len(activitySamples) > 40 {
							activitySamples = append([]string(nil), activitySamples[len(activitySamples)-40:]...)
						}
						activityMu.Unlock()
					}
				}
				cancelDiagnostic()
			}
		}
	}()
	stateSnapshot := func() string {
		diagnosticCtx, cancelDiagnostic := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelDiagnostic()
		taskSnapshot := func(taskID string) string {
			var status, leaseOwner, agentID, sessionID string
			var leaseVersion, eventSeq, generation int64
			rowErr := pool.QueryRow(diagnosticCtx, `SELECT status,COALESCE(lease_owner,''),lease_version,event_seq,
COALESCE(agent_instance_id,''),COALESCE(agent_generation,0),COALESCE(agent_session_id,'') FROM tasks WHERE id=$1`, taskID).
				Scan(&status, &leaseOwner, &leaseVersion, &eventSeq, &agentID, &generation, &sessionID)
			var instanceState, instanceLeaseOwner, supervisorID, engineVersion, profileVersion string
			var instanceLeaseVersion, authorizationVersion int64
			instanceErr := pool.QueryRow(diagnosticCtx, `SELECT state,lease_owner,lease_version,supervisor_id,auth_version,engine_version,profile_version FROM task_agent_instances
WHERE task_id=$1 AND agent_generation=$2`, taskID, generation).
				Scan(&instanceState, &instanceLeaseOwner, &instanceLeaseVersion, &supervisorID, &authorizationVersion, &engineVersion, &profileVersion)
			details, detailsErr := store.GetTask(diagnosticCtx, taskID, owner.ID)
			checkpointHistory := func(checkpointSession string, through int64, digest []byte) string {
				if detailsErr != nil || instanceErr != nil || through < 0 || len(digest) != 32 {
					return fmt.Sprintf("session=%s unavailable through=%d digest_len=%d task_err=%v instance_err=%v",
						checkpointSession, through, len(digest), detailsErr, instanceErr)
				}
				fence := app.AgentInstanceFence{TaskID: taskID, OwnerID: owner.ID, InitiatorID: details.Task.InitiatorID,
					AgentInstanceID: agentID, AgentGeneration: generation, AgentSessionID: sessionID, SupervisorID: supervisorID,
					LeaseOwner: instanceLeaseOwner, LeaseVersion: instanceLeaseVersion, AuthorizationVersion: authorizationVersion,
					EngineVersion: engineVersion, ProfileVersion: profileVersion}
				if err := fence.Validate(); err != nil {
					return fmt.Sprintf("session=%s invalid_fence=%v", checkpointSession, err)
				}
				type checkpointEvent struct {
					Sequence int64  `json:"seq"`
					Type     string `json:"type"`
				}
				type checkpointBody struct {
					Events []checkpointEvent `json:"events"`
				}
				events := make([]string, 0)
				for from := int64(0); from <= through; {
					page, err := checkpoints.ReadPage(diagnosticCtx, app.AgentCheckpointPageRequest{
						Fence: fence, SessionID: checkpointSession, From: from, Through: through, ThroughDigest: digest,
					})
					if err != nil || page.From != from || page.Through < from {
						return fmt.Sprintf("session=%s through=%d events=%v read_error=%v", checkpointSession, through, events, err)
					}
					var body checkpointBody
					if err := json.Unmarshal(page.Payload, &body); err != nil || len(body.Events) == 0 {
						return fmt.Sprintf("session=%s through=%d events=%v decode_error=%v", checkpointSession, through, events, err)
					}
					for _, event := range body.Events {
						events = append(events, fmt.Sprintf("%d:%s", event.Sequence, event.Type))
					}
					from = page.Through + 1
				}
				return fmt.Sprintf("session=%s through=%d events=%v", checkpointSession, through, events)
			}
			rootHistory := checkpointHistory(sessionID, details.Task.CheckpointThroughSequence, details.Task.CheckpointDigest)
			var childSnapshot []string
			childRows, childErr := pool.Query(diagnosticCtx, `SELECT s.session_id,COALESCE(s.parent_session_id,''),s.step_id,
s.cancel_requested_at IS NOT NULL,st.status,COALESCE(st.summary,''),s.checkpoint_through_seq,s.checkpoint_digest
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 ORDER BY st.sequence`, taskID)
			if childErr == nil {
				for childRows.Next() {
					var childSession, parentSession, stepID, stepStatus, summary string
					var cancelRequested bool
					var checkpointThrough int64
					var checkpointDigest []byte
					if err := childRows.Scan(&childSession, &parentSession, &stepID, &cancelRequested, &stepStatus, &summary,
						&checkpointThrough, &checkpointDigest); err != nil {
						childSnapshot = append(childSnapshot, "scan_error="+err.Error())
						break
					}
					childHistory := checkpointHistory(childSession, checkpointThrough, checkpointDigest)
					childSnapshot = append(childSnapshot, fmt.Sprintf("session=%s parent=%s step=%s status=%s cancel=%t checkpoint=%d summary=%q history={%s}",
						childSession, parentSession, stepID, stepStatus, cancelRequested, checkpointThrough, summary, childHistory))
				}
				if err := childRows.Err(); err != nil {
					childSnapshot = append(childSnapshot, "iterate_error="+err.Error())
				}
				childRows.Close()
			}
			events, eventsErr := store.ListTaskEvents(diagnosticCtx, owner.ID, taskID, 0, 100)
			eventSnapshot := make([]string, 0, len(events))
			for _, event := range events {
				eventSnapshot = append(eventSnapshot, fmt.Sprintf("seq=%d task=%s type=%s payload=%s",
					event.Sequence, event.TaskID, event.Type, event.Payload))
			}
			return fmt.Sprintf("id=%s row=status:%s lease:%s/%d event_seq:%d agent:%s/%d/%s row_err=%v instance=state:%s lease:%s/%d supervisor:%s auth:%d engine:%s profile:%s err=%v root_history={%s} details=%#v details_err=%v children=%v child_err=%v events=%v events_err=%v",
				taskID, status, leaseOwner, leaseVersion, eventSeq, agentID, generation, sessionID, rowErr,
				instanceState, instanceLeaseOwner, instanceLeaseVersion, supervisorID, authorizationVersion, engineVersion, profileVersion, instanceErr,
				rootHistory, details, detailsErr, childSnapshot, childErr, eventSnapshot, eventsErr)
		}
		var runtimeState, runtimeID, runtimeLeaseOwner string
		var runtimeGeneration, runtimeSequence int64
		runtimeErr := pool.QueryRow(diagnosticCtx, `SELECT state,runtime_id,runtime_generation,runtime_sequence,lease_owner
FROM account_agent_runtimes WHERE owner_id=$1 ORDER BY updated_at DESC LIMIT 1`, owner.ID).
			Scan(&runtimeState, &runtimeID, &runtimeGeneration, &runtimeSequence, &runtimeLeaseOwner)
		model.mu.Lock()
		parentCalls, childCalls := model.parentCalls, model.childCalls
		model.mu.Unlock()
		command := runtimeSupervisor.cmd
		processState := "unavailable"
		if command != nil && command.Process != nil {
			processState = fmt.Sprintf("pid=%d process_state=%v", command.Process.Pid, command.ProcessState)
		}
		stderr := runtimeSupervisor.stderrSnapshot()
		var dbActivity []string
		activityRows, activityErr := pool.Query(diagnosticCtx, `SELECT pid,state,COALESCE(wait_event_type,''),COALESCE(wait_event,''),
pg_blocking_pids(pid)::text,left(query,240) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
AND state='active' AND wait_event_type='Lock' ORDER BY pid`)
		if activityErr == nil {
			for activityRows.Next() {
				var pid int
				var state, waitType, waitEvent, blocking, query string
				if err := activityRows.Scan(&pid, &state, &waitType, &waitEvent, &blocking, &query); err != nil {
					dbActivity = append(dbActivity, "scan_error="+err.Error())
					break
				}
				dbActivity = append(dbActivity, fmt.Sprintf("pid=%d state=%s wait=%s/%s blockers=%s query=%q", pid, state, waitType, waitEvent, blocking, query))
			}
			if err := activityRows.Err(); err != nil {
				dbActivity = append(dbActivity, "iterate_error="+err.Error())
			}
			activityRows.Close()
		}
		activityMu.Lock()
		observedActivity := append([]string(nil), activitySamples...)
		activityMu.Unlock()
		return fmt.Sprintf("parent={%s} sibling={%s} runtime=state:%s id:%s generation:%d seq:%d lease:%s err=%v node={%s stderr=%q} model_calls=%d/%d child_writes=%d child_tool_task_ids=%v checkpoint_events=%v child_session_calls=%v db_activity=%v db_activity_err=%v",
			taskSnapshot(parent.Task.ID), taskSnapshot(sibling.Task.ID), runtimeState, runtimeID, runtimeGeneration,
			runtimeSequence, runtimeLeaseOwner, runtimeErr, processState, stderr, parentCalls, childCalls, tools.writes.Load(), tools.taskIDsSnapshot(),
			checkpointObserver.snapshot(), childObserver.snapshot(), append(dbActivity, observedActivity...), activityErr)
	}

	select {
	case <-model.parentWaiting:
	case <-ctx.Done():
		t.Fatalf("parent Task did not reach its pending model turn: %s", stateSnapshot())
	}
	select {
	case <-model.childRunning:
	case <-ctx.Done():
		t.Fatalf("parent's background child did not reach its running model turn: %s", stateSnapshot())
	}
	select {
	case <-model.siblingWaiting:
	case <-ctx.Done():
		t.Fatalf("sibling Task did not start concurrently: %s", stateSnapshot())
	}
	parentBeforeStop, err := store.GetTask(ctx, parent.Task.ID, owner.ID)
	if err != nil || parentBeforeStop.Task.Status != task.StatusRunning {
		t.Fatalf("parent Task before cancellation = %s err=%v", parentBeforeStop.Task.Status, err)
	}
	siblingBeforeStop, err := store.GetTask(ctx, sibling.Task.ID, owner.ID)
	if err != nil || siblingBeforeStop.Task.Status != task.StatusRunning {
		t.Fatalf("sibling Task before parent cancellation = %s err=%v", siblingBeforeStop.Task.Status, err)
	}

	childSessionID := <-model.childSessionID
	if _, err := tasks.RequestAction(ctx, principal, parent.Task.ID, task.ActionCancel, task.StatusRunning, "f01-parent-sibling-cancel-action"); err != nil {
		t.Fatalf("request parent Task cancellation: %v", err)
	}
	if err := waitF01(t, ctx, func() (bool, error) {
		var status string
		var cancelRequested bool
		err := pool.QueryRow(ctx, `SELECT st.status,s.cancel_requested_at IS NOT NULL
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, parent.Task.ID, owner.ID, childSessionID).
			Scan(&status, &cancelRequested)
		return cancelRequested && status == string(task.StepRunning), err
	}, nil, nil); err != nil {
		t.Fatalf("parent cancellation did not atomically record child intent while keeping its step active: %v", err)
	}
	select {
	case <-model.childCancelled:
	case <-ctx.Done():
		t.Fatal("background child model call did not observe parent cancellation")
	}
	select {
	case <-guardian.stopStarted:
	case <-ctx.Done():
		t.Fatal("Guardian did not begin stopping the exact parent Task process")
	}
	var parentStatus, parentAgentState, childStatus string
	var childCancelRequested bool
	if err := pool.QueryRow(ctx, `SELECT t.status,i.state,st.status,s.cancel_requested_at IS NOT NULL
FROM tasks t JOIN task_agent_instances i ON i.task_id=t.id AND i.owner_id=t.owner_id
JOIN task_agent_sessions s ON s.task_id=t.id AND s.owner_id=t.owner_id
JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE t.id=$1 AND t.owner_id=$2 AND s.session_id=$3 AND i.agent_generation=t.agent_generation`,
		parent.Task.ID, owner.ID, childSessionID).Scan(&parentStatus, &parentAgentState, &childStatus, &childCancelRequested); err != nil {
		t.Fatalf("read fenced parent and child before Guardian stop proof: %v", err)
	}
	if parentStatus != string(task.StatusCancelling) || parentAgentState != "active" ||
		childStatus != string(task.StepRunning) || !childCancelRequested {
		t.Fatalf("before process stop proof: parent=%s agent=%s child=%s cancel_requested=%t; want cancelling/active/running/true",
			parentStatus, parentAgentState, childStatus, childCancelRequested)
	}
	releaseParentStop.Do(func() { close(guardian.allowStop) })
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, parent.Task.ID, owner.ID)
		return err == nil && current.Task.Status == task.StatusCancelled, err
	}, nil, nil); err != nil {
		t.Fatalf("parent Task did not reach cancelled status: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM task_agent_instances WHERE task_id=$1 ORDER BY agent_generation DESC LIMIT 1`, parent.Task.ID).
		Scan(&parentAgentState); err != nil || parentAgentState != "stopped" {
		t.Fatalf("parent Agent stop proof = %q err=%v; want stopped", parentAgentState, err)
	}
	select {
	case <-model.siblingCancelled:
		t.Fatal("cancelling parent cancelled the sibling's model request")
	case <-time.After(100 * time.Millisecond):
	}
	siblingAfterStop, err := store.GetTask(ctx, sibling.Task.ID, owner.ID)
	if err != nil || siblingAfterStop.Task.Status != task.StatusRunning {
		t.Fatalf("cancelling parent affected sibling Task: status=%s err=%v", siblingAfterStop.Task.Status, err)
	}

	close(model.siblingRelease)
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, sibling.Task.ID, owner.ID)
		return err == nil && current.Task.Status == task.StatusCompleted, err
	}, completionErrors, nil); err != nil {
		diagnosticCtx, cancelDiagnostic := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelDiagnostic()
		siblingDetails, detailErr := store.GetTask(diagnosticCtx, sibling.Task.ID, owner.ID)
		model.mu.Lock()
		parentCalls, childCalls := model.parentCalls, model.childCalls
		model.mu.Unlock()
		var siblingAgentState string
		_ = pool.QueryRow(diagnosticCtx, `SELECT state FROM task_agent_instances WHERE task_id=$1 ORDER BY agent_generation DESC LIMIT 1`, sibling.Task.ID).
			Scan(&siblingAgentState)
		siblingResultReturned := false
		select {
		case <-model.siblingReturned:
			siblingResultReturned = true
		default:
		}
		select {
		case <-model.siblingCancelled:
			t.Fatalf("sibling Task did not complete after parent cancellation: %v; status=%s reason=%s steps=%#v agent_state=%s sibling model was cancelled, result_returned=%t model_calls=%d/%d writes=%d details_error=%v",
				err, siblingDetails.Task.Status, siblingDetails.Task.WaitingReason, siblingDetails.Steps, siblingAgentState, siblingResultReturned, parentCalls, childCalls, tools.writes.Load(), detailErr)
		default:
		}
		t.Fatalf("sibling Task did not complete after parent cancellation: %v; status=%s reason=%s steps=%#v agent_state=%s result_returned=%t model_calls=%d/%d writes=%d details_error=%v",
			err, siblingDetails.Task.Status, siblingDetails.Task.WaitingReason, siblingDetails.Steps, siblingAgentState, siblingResultReturned, parentCalls, childCalls, tools.writes.Load(), detailErr)
	}
	messages, err := store.ListMessages(ctx, owner.ID, parent.Conversation.ID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundSiblingAnswer := false
	for _, message := range messages.Items {
		if message.TaskID == sibling.Task.ID && message.Role == conversation.RoleAssistant && message.Content == "Sibling completed independently." {
			foundSiblingAnswer = true
		}
		if message.TaskID == parent.Task.ID && message.Role == conversation.RoleAssistant {
			t.Fatalf("cancelled parent unexpectedly published an assistant result: %#v", message)
		}
	}
	if !foundSiblingAnswer {
		t.Fatalf("sibling assistant result is missing or routed to another Task: %#v", messages.Items)
	}
	var cancelEventStepID, settledEventStepID, cancelEventParentSessionID, settledEventParentSessionID string
	for _, taskID := range []string{parent.Task.ID, sibling.Task.ID} {
		events, err := store.ListTaskEvents(ctx, owner.ID, taskID, 0, 100)
		if err != nil || len(events) == 0 {
			t.Fatalf("Task %s events = %d err=%v", taskID, len(events), err)
		}
		sawResult := false
		sawCancelIntent := false
		sawChildSettlement := false
		for _, event := range events {
			if event.TaskID != taskID {
				t.Fatalf("event escaped its Task: expected %s, got %#v", taskID, event)
			}
			if event.Type == "task.result" && taskID == sibling.Task.ID {
				sawResult = true
			}
			if taskID == parent.Task.ID && event.Type == "task.step" {
				var payload struct {
					Phase           string `json:"phase"`
					SessionID       string `json:"session_id"`
					ParentSessionID string `json:"parent_session_id"`
					StepID          string `json:"step_id"`
					Status          string `json:"status"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("decode parent child lifecycle event: %v", err)
				}
				if payload.Phase == "subagent_cancel_requested" {
					sawCancelIntent = true
					cancelEventStepID, cancelEventParentSessionID = payload.StepID, payload.ParentSessionID
					if payload.SessionID != childSessionID || payload.StepID == "" || payload.ParentSessionID == "" {
						t.Fatalf("child cancel event has wrong session lineage: %#v", payload)
					}
				}
				if payload.Phase == "subagent_settled" {
					sawChildSettlement = true
					settledEventStepID, settledEventParentSessionID = payload.StepID, payload.ParentSessionID
					if payload.SessionID != childSessionID || payload.StepID == "" || payload.Status != string(task.StepUnknown) {
						t.Fatalf("child settlement event has wrong session or terminal status: %#v", payload)
					}
				}
			}
		}
		if taskID == sibling.Task.ID && !sawResult {
			t.Fatalf("sibling Task has no result event: %#v", events)
		}
		if taskID == parent.Task.ID && (!sawCancelIntent || !sawChildSettlement) {
			t.Fatalf("parent Task is missing correlated child lifecycle events: cancel_intent=%t settled=%t events=%#v",
				sawCancelIntent, sawChildSettlement, events)
		}
		if taskID == parent.Task.ID && (cancelEventStepID != settledEventStepID ||
			cancelEventParentSessionID != settledEventParentSessionID) {
			t.Fatalf("parent child lifecycle events do not refer to the same fenced child: intent=%s/%s settled=%s/%s",
				cancelEventStepID, cancelEventParentSessionID, settledEventStepID, settledEventParentSessionID)
		}
	}
	childStatus = ""
	childCancelRequested = false
	var childStepID, childParentSessionID string
	if err := pool.QueryRow(ctx, `SELECT st.status,s.cancel_requested_at IS NOT NULL,s.step_id,s.parent_session_id
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, parent.Task.ID, owner.ID, childSessionID).
		Scan(&childStatus, &childCancelRequested, &childStepID, &childParentSessionID); err != nil {
		t.Fatalf("read stopped child session: %v", err)
	}
	if childStepID != cancelEventStepID || childParentSessionID != cancelEventParentSessionID {
		t.Fatalf("child event lineage does not match the persisted child session: event=%s/%s DB=%s/%s",
			cancelEventStepID, cancelEventParentSessionID, childStepID, childParentSessionID)
	}
	if !childCancelRequested || childStatus != string(task.StepUnknown) {
		parentEvents, eventErr := store.ListTaskEvents(ctx, owner.ID, parent.Task.ID, 0, 100)
		childCancellationObserved := false
		select {
		case <-model.childCancelled:
			childCancellationObserved = true
		default:
		}
		t.Fatalf("parent child cancellation state = %s cancel_requested=%t child_model_cancelled=%t; want unknown with cancellation recorded; parent_events=%#v event_error=%v",
			childStatus, childCancelRequested, childCancellationObserved, parentEvents, eventErr)
	}
	if tools.writes.Load() != 1 || len(tools.taskIDsSnapshot()) != 1 || tools.taskIDsSnapshot()[0] != parent.Task.ID {
		t.Fatalf("child side effect routing = writes %d task IDs %v; want one write on parent Task", tools.writes.Load(), tools.taskIDsSnapshot())
	}
}

type f01GatedSignedNodeTaskGuardian struct {
	*f01SignedNodeTaskGuardian
	taskID      string
	stopStarted chan struct{}
	allowStop   chan struct{}
	startedOnce sync.Once
}

func (g *f01GatedSignedNodeTaskGuardian) StopTaskProcess(ctx context.Context,
	guard dshguardian.TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	if guard.TaskFence.TaskID == g.taskID {
		g.startedOnce.Do(func() { close(g.stopStarted) })
		<-g.allowStop
	}
	return g.f01SignedNodeTaskGuardian.StopTaskProcess(ctx, guard)
}

func TestCloudTaskWithCancelledPersonalChildFailsClosedAfterNodeRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify Task session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Answer in chat after a background child task is cancelled.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 8, ModelTokens: 1000}, IdempotencyKey: "f01-child-cancel-restart-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "f01-child-cancel-worker-old", 5*time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim initial Task: id=%s found=%t err=%v", claimed.ID, found, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stopKey := bytes.Repeat([]byte{0x64}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorAuth, err := app.NewAgentStopAuthenticator("f01-child-cancel-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator("f01-child-cancel-supervisor", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	checkpointService, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := &f01ObservedCheckpoints{AgentCheckpointService: checkpointService}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	node, script := f01NodeRuntimePaths(t)
	accountScript := filepath.Join(filepath.Dir(script), "bin-account.mjs")
	model := &cancelRestartModel{childCheckpointed: make(chan struct{}), interrupted: make(chan struct{})}
	tools := &cancelRestartTools{definition: cloudDSHWriteTool()}
	guardian := &f01SignedNodeTaskGuardian{key: privateKey}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	makeSupervisor := func() *f01RecoveringNodeAccountSupervisor {
		return &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
	}
	makeRuntimeManager := func(supervisor CloudDSHAccountRuntimeSupervisor, leaseOwner string) *CloudDSHAccountRuntimeManager {
		t.Helper()
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: runtimeStopVerifier,
			TaskStopper: instances, LeaseOwner: leaseOwner, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	firstRuntimeSupervisor := makeSupervisor()
	firstRuntimeManager := makeRuntimeManager(firstRuntimeSupervisor, "f01-child-cancel-runtime-old")
	defer func() {
		if command := firstRuntimeSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	firstExecutor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: "f01-child-cancel-supervisor", AccountRuntimes: firstRuntimeManager, Instances: instances, Checkpoints: checkpoints, Children: children, Models: model, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- firstExecutor.ExecuteTask(firstCtx, claimed) }()
	defer cancelFirst()

	select {
	case <-model.childCheckpointed:
	case <-ctx.Done():
		t.Fatalf("background child did not reach its persisted checkpoint: %v; root_calls=%d child_calls=%d writes=%d",
			ctx.Err(), model.callCount(1), model.childCallCount(1), tools.writes.Load())
	case err := <-firstDone:
		firstRuntimeManager.mu.Lock()
		runtimeSession := firstRuntimeManager.sessions[submitted.Conversation.ID]
		firstRuntimeManager.mu.Unlock()
		var runtimeFailure string
		if runtimeSession != nil {
			runtimeSession.mu.Lock()
			if runtimeSession.failed != nil {
				runtimeFailure = runtimeSession.failed.Error()
			}
			runtimeSession.mu.Unlock()
		}
		t.Fatalf("first Agent stopped before background child checkpoint: %v; checkpoint=%v runtime=%s root_calls=%d child_calls=%d writes=%d",
			err, checkpoints.lastError(), runtimeFailure, model.callCount(1), model.childCallCount(1), tools.writes.Load())
	}
	rootBeforeRestart, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil {
		t.Fatalf("read root recovery checkpoint: %v", err)
	}
	fence := app.AgentInstanceFence{TaskID: submitted.Task.ID, OwnerID: owner.ID, InitiatorID: rootBeforeRestart.Task.InitiatorID,
		AgentInstanceID: rootBeforeRestart.Task.AgentInstanceID, AgentGeneration: rootBeforeRestart.Task.AgentGeneration,
		AgentSessionID: rootBeforeRestart.Task.AgentSessionID, SupervisorID: "f01-child-cancel-supervisor",
		LeaseOwner: rootBeforeRestart.Task.Lease.Owner, LeaseVersion: rootBeforeRestart.Task.Lease.Version,
		AuthorizationVersion: rootBeforeRestart.Task.Authorization.Version,
		EngineVersion:        cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	if fence.AgentGeneration != 1 || fence.AgentSessionID == "" {
		t.Fatalf("initial Agent fence was not persisted: %#v", fence)
	}
	proofFound := false
	for _, event := range checkpoints.snapshot() {
		if strings.Contains(event, "task="+submitted.Task.ID+" session="+fence.AgentSessionID) &&
			strings.Contains(event, "source=plugin/morde-context proof=true") {
			proofFound = true
			break
		}
	}
	if !proofFound {
		t.Fatalf("root checkpoint is missing authenticated Morde context proof: events=%v", checkpoints.snapshot())
	}
	jobs, err := children.List(ctx, fence, fence.AgentSessionID)
	if err != nil || len(jobs) != 1 || !jobs[0].Background || jobs[0].Step.Status != task.StepRunning ||
		jobs[0].CheckpointThrough < 0 || len(jobs[0].CheckpointDigest) != 32 {
		t.Fatalf("running child checkpoint before cancel = %#v err=%v", jobs, err)
	}
	jobID := jobs[0].SessionID
	requested, replayed, err := children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{
		Fence: fence, ActorSessionID: fence.AgentSessionID, SessionID: jobID,
	})
	if err != nil || replayed || !requested.CancelRequested || requested.Step.Status != task.StepRunning {
		t.Fatalf("persist child cancellation: child=%#v replayed=%t err=%v", requested, replayed, err)
	}
	oldChildCheckpoint := append([]byte(nil), jobs[0].CheckpointDigest...)
	oldChildThrough := jobs[0].CheckpointThrough
	oldRootCheckpointThrough := rootBeforeRestart.Task.CheckpointThroughSequence
	oldRootCheckpointDigest := append([]byte(nil), rootBeforeRestart.Task.CheckpointDigest...)
	if oldRootCheckpointThrough < 0 || len(oldRootCheckpointDigest) != 32 {
		t.Fatalf("parent checkpoint was not durable before interruption: through=%d digest=%d", oldRootCheckpointThrough, len(oldRootCheckpointDigest))
	}

	oldProcess := firstRuntimeSupervisor.cmd
	if oldProcess == nil || oldProcess.Process == nil {
		t.Fatal("real Node account Runtime was not available for restart simulation")
	}
	if err := oldProcess.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop old Node process: %v", err)
	}
	model.interrupt()
	var interruptedErr error
	select {
	case runErr := <-firstDone:
		interruptedErr = runErr
		if runErr == nil || errors.Is(runErr, ErrCloudDSHCompletionPending) {
			t.Fatalf("interrupted Agent result = %v, want a recoverable interruption", runErr)
		}
	case <-ctx.Done():
		t.Fatalf("old Agent did not stop and persist its receipt: %v", ctx.Err())
	}
	waitOldRuntime := make(chan error, 1)
	go func() { waitOldRuntime <- oldProcess.Wait() }()
	select {
	case waitErr := <-waitOldRuntime:
		if waitErr == nil || oldProcess.ProcessState == nil || oldProcess.ProcessState.ExitCode() != -1 {
			t.Fatalf("wait for old account Runtime: %v", waitErr)
		}
		firstRuntimeSupervisor.cmd = nil
	case <-ctx.Done():
		t.Fatalf("old account Runtime did not exit: %v", ctx.Err())
	}
	var oldState string
	if err := pool.QueryRow(ctx, `SELECT state FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`,
		submitted.Task.ID, fence.AgentGeneration).Scan(&oldState); err != nil || oldState != "stopped" {
		t.Fatalf("old Agent stop proof state=%q err=%v interrupted=%v", oldState, err, interruptedErr)
	}
	var parentStatus, parentLeaseOwner string
	var pendingStopRequests int
	if err := pool.QueryRow(ctx, `SELECT status,lease_owner FROM tasks WHERE id=$1`, submitted.Task.ID).
		Scan(&parentStatus, &parentLeaseOwner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_stop_requests WHERE task_id=$1 AND status='pending'`, submitted.Task.ID).
		Scan(&pendingStopRequests); err != nil {
		t.Fatal(err)
	}
	if parentStatus != string(task.StatusRunning) || parentLeaseOwner == "" || pendingStopRequests != 0 {
		t.Fatalf("Runtime crash cancelled its Task: status=%s lease_owner_empty=%t pending_stops=%d",
			parentStatus, parentLeaseOwner == "", pendingStopRequests)
	}
	firstRuntimeManager.mu.Lock()
	oldRuntimeSession := firstRuntimeManager.sessions[submitted.Conversation.ID]
	firstRuntimeManager.mu.Unlock()
	if oldRuntimeSession == nil {
		t.Fatal("old account Runtime session is missing")
	}
	oldRuntimeFence := oldRuntimeSession.runtime.Fence
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		oldRuntimeFence.ServiceInstanceID, owner.ID, oldRuntimeFence.RuntimeID, oldRuntimeFence.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "f01-child-cancel-worker-new", 5*time.Minute)
	if err != nil || !found || recovered.ID != submitted.Task.ID || recovered.AgentSessionID != fence.AgentSessionID ||
		recovered.CheckpointThroughSequence != oldRootCheckpointThrough || !bytes.Equal(recovered.CheckpointDigest, oldRootCheckpointDigest) {
		t.Fatalf("claim recoverable parent session: task=%#v found=%t err=%v", recovered, found, err)
	}
	secondRuntimeSupervisor := makeSupervisor()
	secondRuntimeManager := makeRuntimeManager(secondRuntimeSupervisor, "f01-child-cancel-runtime-new")
	defer func() {
		if command := secondRuntimeSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	secondExecutor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: "f01-child-cancel-supervisor", AccountRuntimes: secondRuntimeManager, Instances: instances, Checkpoints: checkpoints, Children: children, Models: model, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondExecutor.ExecuteTask(ctx, recovered); !errors.Is(err, errCloudDSHPersonalMemoryChanged) {
		t.Fatalf("parent Agent recovery with an unknown personal child outcome = %v, want fail-closed personal-memory recovery; root_calls=%d child_calls=%d checkpoints=%v",
			err, model.callCount(fence.AgentGeneration+1), model.childCallCount(fence.AgentGeneration+1), checkpoints.snapshot())
	}
	if model.callCount(fence.AgentGeneration+1) != 0 || model.childCallCount(fence.AgentGeneration+1) != 0 ||
		tools.writes.Load() != 1 || tools.memoryRecoveryPauses.Load() != 1 {
		t.Fatalf("unsafe parent/child work replayed or recovery was not paused: root_calls=%d child_calls=%d writes=%d pauses=%d",
			model.callCount(fence.AgentGeneration+1), model.childCallCount(fence.AgentGeneration+1), tools.writes.Load(), tools.memoryRecoveryPauses.Load())
	}

	var childStatus, childSummary string
	var cancelRequested bool
	var childThrough int64
	var childDigest []byte
	if err := pool.QueryRow(ctx, `SELECT st.status,st.summary,s.cancel_requested_at IS NOT NULL,s.checkpoint_through_seq,s.checkpoint_digest
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, submitted.Task.ID, owner.ID, jobID).Scan(
		&childStatus, &childSummary, &cancelRequested, &childThrough, &childDigest); err != nil {
		t.Fatalf("read reconciled child job: %v", err)
	}
	if childStatus != string(task.StepUnknown) || !cancelRequested ||
		childSummary != "outcome unknown after cancellation and confirmed Agent restart" ||
		childThrough != oldChildThrough || !bytes.Equal(childDigest, oldChildCheckpoint) {
		t.Fatalf("cancelled child convergence status=%s summary=%q cancel=%t checkpoint=%d/%x; want unknown with its pinned checkpoint",
			childStatus, childSummary, cancelRequested, childThrough, childDigest)
	}
	parent, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || parent.Task.AgentGeneration != fence.AgentGeneration+1 || parent.Task.AgentSessionID != fence.AgentSessionID ||
		parent.Task.CheckpointThroughSequence != oldRootCheckpointThrough || !bytes.Equal(parent.Task.CheckpointDigest, oldRootCheckpointDigest) {
		t.Fatalf("failed-closed recovery changed the pinned parent checkpoint: task=%#v err=%v", parent.Task, err)
	}
}

// TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim uses a real
// PostgreSQL checkpoint/child store and the real local DSH subprocess. Its
// synthetic model has no external provider and its only application tool is
// deliberately never invoked. The first runtime is interrupted while a
// background child is running; a later lease owner resumes and settles it,
// then a further reclaim reads the durable child output from the parent's
// persisted cursor before completing.
func TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify Task session: principal=%#v err=%v", principal, err)
	}
	schedules, err := app.NewScheduleService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	scheduled, err := schedules.Create(ctx, principal, app.CreateScheduleCommand{
		ModelID: "p1234567", Objective: "Start a background child and review its result before reporting.",
		Timing:        schedule.Timing{Kind: schedule.TimingOnce, RunAt: dueAt.Format(time.RFC3339), TimeZone: "UTC"},
		Budget:        task.Budget{ActiveSeconds: 120, ToolCalls: 12, ModelTokens: 2000},
		MisfirePolicy: schedule.MisfireRunOnce, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
	}, "f07-background-output-reclaim-001")
	if err != nil {
		t.Fatalf("create synthetic scheduled Task: %v", err)
	}
	processed, err := schedules.ProcessDue(ctx, time.Now().UTC(), 10)
	if err != nil || processed != 1 {
		t.Fatalf("process synthetic schedule: processed=%d err=%v", processed, err)
	}
	runs, err := schedules.Runs(ctx, principal, scheduled.ID, app.PageRequest{Limit: 10})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].TaskID == nil || runs.Items[0].Status != schedule.RunQueued {
		t.Fatalf("scheduled Task was not queued: runs=%#v err=%v", runs.Items, err)
	}
	taskID := *runs.Items[0].TaskID
	claimed, found, err := store.ClaimNextTask(ctx, "f07-background-output-worker-one", 5*time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim initial Task: task=%#v found=%t err=%v", claimed, found, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x77}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stopKey := bytes.Repeat([]byte{0x78}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorID := "f07-background-output-supervisor"
	supervisorAuth, err := app.NewAgentStopAuthenticator(supervisorID, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator(supervisorID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	observedCheckpoints := &f01ObservedCheckpoints{AgentCheckpointService: checkpoints}
	checkpointCapture := &f07ObservedCheckpoints{f01ObservedCheckpoints: observedCheckpoints, bySession: make(map[string][]f07CheckpointEntry)}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	node, dshScript := f01NodeRuntimePaths(t)
	accountScript := filepath.Join(filepath.Dir(dshScript), "bin-account.mjs")
	model := newF07BackgroundOutputModel()
	tools := &f07SyntheticTool{}
	guardian := &f01SignedNodeTaskGuardian{key: privateKey}
	accountControl, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	makeSupervisor := func() *f01RecoveringNodeAccountSupervisor {
		t.Helper()
		return &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
	}
	makeRuntimeManager := func(supervisor CloudDSHAccountRuntimeSupervisor, leaseOwner string) *CloudDSHAccountRuntimeManager {
		t.Helper()
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: accountControl, Supervisor: supervisor, Guardian: guardian, StopVerifier: runtimeStopVerifier,
			TaskStopper: instances, LeaseOwner: leaseOwner, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	makeExecutor := func(manager *CloudDSHAccountRuntimeManager) *CloudDSHExecutor {
		t.Helper()
		executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
			ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: supervisorID,
			AccountRuntimes: manager, Instances: instances, Checkpoints: checkpointCapture,
			Models: model, Tools: tools, Children: children,
			Completion: &parentSiblingCancelCompletion{store: store},
		})
		if err != nil {
			t.Fatal(err)
		}
		return executor
	}

	firstSupervisor := makeSupervisor()
	firstManager := makeRuntimeManager(firstSupervisor, "f07-background-output-runtime-one")
	defer func() {
		if command := firstSupervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	firstExecutor := makeExecutor(firstManager)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	firstDone := make(chan error, 1)
	firstRuntimeError := make(chan error, 1)
	go func() {
		runErr := firstExecutor.ExecuteTask(firstCtx, claimed)
		firstDone <- runErr
		firstRuntimeError <- runErr
	}()
	select {
	case <-model.childStarted:
	case runErr := <-firstRuntimeError:
		t.Fatalf("first Agent stopped before the background child model call: %v", runErr)
	case <-ctx.Done():
		t.Fatalf("background child did not start: %v", ctx.Err())
	}
	var initialDetails app.TaskDetails
	var startupDiagnostic string
	startupCtx, cancelStartup := context.WithTimeout(ctx, 20*time.Second)
	defer cancelStartup()
	if err := waitF01(t, startupCtx, func() (bool, error) {
		current, err := store.GetTask(ctx, taskID, owner.ID)
		if err != nil {
			return false, err
		}
		initialDetails = current
		if current.Task.AgentGeneration != 1 || current.Task.AgentSessionID == "" || current.Task.CheckpointThroughSequence < 0 {
			startupDiagnostic = fmt.Sprintf("generation=%d session=%q checkpoint=%d events=%v",
				current.Task.AgentGeneration, current.Task.AgentSessionID, current.Task.CheckpointThroughSequence, observedCheckpoints.snapshot())
			return false, nil
		}
		rootToolResultPersisted := false
		for _, entry := range observedCheckpoints.snapshot() {
			if strings.Contains(entry, "session="+current.Task.AgentSessionID) && strings.Contains(entry, "type=tool/result") {
				rootToolResultPersisted = true
				break
			}
		}
		childRows, err := children.List(ctx, f07AgentFence(current.Task, supervisorID), current.Task.AgentSessionID)
		if err != nil {
			return false, err
		}
		startupDiagnostic = fmt.Sprintf("root_tool_result=%t children=%#v checkpoint_events=%v",
			rootToolResultPersisted, childRows, observedCheckpoints.snapshot())
		startupDiagnostic += fmt.Sprintf(" model_calls=%v", model.callSnapshot())
		return rootToolResultPersisted && len(childRows) == 1 && childRows[0].Background &&
			childRows[0].Step.Status == task.StepRunning && childRows[0].CheckpointThrough >= 0 &&
			len(childRows[0].CheckpointDigest) == 32, nil
	}, firstRuntimeError, nil); err != nil {
		t.Fatalf("parent delegation and running child checkpoint were not both persisted: %v; state: %s", err, startupDiagnostic)
	}
	childBeforeRestart, err := children.List(ctx, f07AgentFence(initialDetails.Task, supervisorID), initialDetails.Task.AgentSessionID)
	if err != nil || len(childBeforeRestart) != 1 || childBeforeRestart[0].Step.Status != task.StepRunning || childBeforeRestart[0].CancelRequested {
		t.Fatalf("background child before Node restart = %#v err=%v", childBeforeRestart, err)
	}
	rootSessionID := initialDetails.Task.AgentSessionID
	rootThroughBeforeRestart := initialDetails.Task.CheckpointThroughSequence
	rootDigestBeforeRestart := append([]byte(nil), initialDetails.Task.CheckpointDigest...)
	if rootThroughBeforeRestart < 0 || len(rootDigestBeforeRestart) != 32 {
		t.Fatalf("parent checkpoint is not pinned before restart: through=%d digest=%d", rootThroughBeforeRestart, len(rootDigestBeforeRestart))
	}
	oldProcess := firstSupervisor.cmd
	if oldProcess == nil || oldProcess.Process == nil {
		t.Fatal("real Node account Runtime was not available for restart simulation")
	}
	if err := oldProcess.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop first Node Runtime: %v", err)
	}
	model.interrupt()
	select {
	case runErr := <-firstDone:
		if runErr == nil || errors.Is(runErr, ErrCloudDSHCompletionPending) || errors.Is(runErr, errCloudDSHBackgroundOutputPending) {
			t.Fatalf("interrupted first Agent result = %v, want a recoverable Runtime interruption", runErr)
		}
	case <-ctx.Done():
		t.Fatalf("first Agent did not stop after Node restart: %v", ctx.Err())
	}
	waitOldRuntime := make(chan error, 1)
	go func() { waitOldRuntime <- oldProcess.Wait() }()
	select {
	case waitErr := <-waitOldRuntime:
		if waitErr == nil || oldProcess.ProcessState == nil || oldProcess.ProcessState.ExitCode() != -1 {
			t.Fatalf("wait for first Node Runtime: %v", waitErr)
		}
		firstSupervisor.cmd = nil
	case <-ctx.Done():
		t.Fatalf("first Node Runtime did not exit: %v", ctx.Err())
	}
	firstManager.mu.Lock()
	firstRuntimeSession := firstManager.sessions[claimed.ConversationID]
	firstManager.mu.Unlock()
	if firstRuntimeSession == nil {
		t.Fatal("first account Runtime session is missing")
	}
	oldRuntimeFence := firstRuntimeSession.runtime.Fence
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		oldRuntimeFence.ServiceInstanceID, owner.ID, oldRuntimeFence.RuntimeID, oldRuntimeFence.RuntimeGeneration); err != nil {
		t.Fatalf("expire first account Runtime lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE id=$1 AND lease_owner=$2 AND lease_version=$3`, taskID, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("expire first Task lease: %v", err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "f07-background-output-worker-two", 5*time.Minute)
	if err != nil || !found || recovered.ID != taskID || recovered.AgentSessionID != rootSessionID ||
		recovered.CheckpointThroughSequence != rootThroughBeforeRestart || !bytes.Equal(recovered.CheckpointDigest, rootDigestBeforeRestart) {
		t.Fatalf("claim parent after first lease expiry: task=%#v found=%t err=%v", recovered, found, err)
	}

	secondSupervisor := makeSupervisor()
	secondManager := makeRuntimeManager(secondSupervisor, "f07-background-output-runtime-two")
	defer func() {
		if command := secondSupervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	secondExecutor := makeExecutor(secondManager)
	if err := secondExecutor.ExecuteTask(ctx, recovered); !errors.Is(err, errCloudDSHBackgroundOutputPending) {
		failedDetails, detailsErr := store.GetTask(ctx, taskID, owner.ID)
		var generation, through int64
		var digest []byte
		var failedChildren []app.AgentChildSession
		var childrenErr error
		if detailsErr == nil {
			generation, through, digest = failedDetails.Task.AgentGeneration, failedDetails.Task.CheckpointThroughSequence, failedDetails.Task.CheckpointDigest
			failedChildren, childrenErr = children.List(ctx, f07AgentFence(failedDetails.Task, supervisorID), rootSessionID)
		}
		t.Fatalf("restart resumed child but skipped the pending parent output: err=%v task_generation=%d task_checkpoint=%d/%x details_err=%v children=%#v children_err=%v model_calls=%v",
			err, generation, through, digest,
			detailsErr, failedChildren, childrenErr, model.callSnapshot())
	}
	settledDetails, err := store.GetTask(ctx, taskID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	settledChildren, err := f07ReadPersistedChildren(ctx, pool, taskID, owner.ID, rootSessionID)
	if err != nil || len(settledChildren) != 1 || settledChildren[0].Step.Status != task.StepSucceeded ||
		settledChildren[0].Step.Summary != f07BackgroundOutputSummary || settledChildren[0].CheckpointThrough < 0 ||
		len(settledChildren[0].CheckpointDigest) != 32 || settledChildren[0].CancelRequested {
		t.Fatalf("background result was not durably settled after restart: children=%#v err=%v", settledChildren, err)
	}
	if settledDetails.Task.CheckpointThroughSequence <= rootThroughBeforeRestart || len(settledDetails.Task.CheckpointDigest) != 32 ||
		bytes.Equal(settledDetails.Task.CheckpointDigest, rootDigestBeforeRestart) {
		t.Fatalf("parent recovery did not durably continue its open turn before output consumption: before=%d/%x after=%d/%x",
			rootThroughBeforeRestart, rootDigestBeforeRestart, settledDetails.Task.CheckpointThroughSequence, settledDetails.Task.CheckpointDigest)
	}
	model.setJob(settledChildren[0].SessionID, settledChildren[0].CheckpointThrough)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE id=$1 AND lease_owner=$2 AND lease_version=$3`, settledDetails.Task.ID,
		settledDetails.Task.Lease.Owner, settledDetails.Task.Lease.Version); err != nil {
		t.Fatalf("expire pending-output Task lease: %v", err)
	}
	reclaimed, found, err := store.ClaimNextTask(ctx, "f07-background-output-worker-three", 5*time.Minute)
	if err != nil || !found || reclaimed.ID != taskID || reclaimed.AgentSessionID != rootSessionID ||
		reclaimed.CheckpointThroughSequence != settledDetails.Task.CheckpointThroughSequence || !bytes.Equal(reclaimed.CheckpointDigest, settledDetails.Task.CheckpointDigest) {
		t.Fatalf("claim parent for durable output continuation: task=%#v found=%t err=%v", reclaimed, found, err)
	}
	if err := secondExecutor.ExecuteTask(ctx, reclaimed); err != nil {
		t.Fatalf("consume settled background output and complete parent: %v", err)
	}
	completed, err := store.GetTask(ctx, taskID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != f07BackgroundParentSummary {
		t.Fatalf("parent result after background output continuation = status %s result=%q err=%v",
			completed.Task.Status, completed.Task.Result, err)
	}
	completedChildren, err := f07ReadPersistedChildren(ctx, pool, taskID, owner.ID, rootSessionID)
	if err != nil || len(completedChildren) != 1 || completedChildren[0].Step.Status != task.StepSucceeded ||
		completedChildren[0].Step.Summary != f07BackgroundOutputSummary || completedChildren[0].CheckpointThrough != settledChildren[0].CheckpointThrough ||
		!bytes.Equal(completedChildren[0].CheckpointDigest, settledChildren[0].CheckpointDigest) {
		t.Fatalf("child settlement changed while the parent consumed output: children=%#v err=%v", completedChildren, err)
	}
	rootHistory, err := checkpointCapture.rootHistory(rootSessionID, completed.Task.CheckpointThroughSequence)
	if err != nil {
		t.Fatalf("read completed parent output checkpoint: %v", err)
	}
	outputCall := rootHistory.calls[f07BackgroundOutputCallID]
	var outputResult struct {
		JobID      string `json:"job_id"`
		Status     string `json:"status"`
		NextCursor int64  `json:"next_cursor"`
		Output     string `json:"output"`
	}
	if outputCall == nil || outputCall.name != cloudDSHJobOutputToolName || !outputCall.callSeen || !outputCall.resultSeen ||
		json.Unmarshal([]byte(outputCall.resultText), &outputResult) != nil || outputResult.JobID != completedChildren[0].SessionID ||
		outputResult.Status != "succeeded" || outputResult.NextCursor != completedChildren[0].CheckpointThrough ||
		outputResult.Output != f07BackgroundOutputSummary || !model.outputConsumed.Load() || tools.calls.Load() != 0 {
		t.Fatalf("durable background output/cursor was not consumed exactly once: call=%#v result=%#v consumed=%t tool_calls=%d",
			outputCall, outputResult, model.outputConsumed.Load(), tools.calls.Load())
	}
	rootDefinitions := []app.AgentToolDefinition{tools.definition()}
	rootDefinitions = append(rootDefinitions, cloudDSHJobTools()...)
	rootDefinitions = append(rootDefinitions, cloudDSHDelegateTool([]app.AgentToolDefinition{tools.definition()}))
	continuations, waiting, err := cloudDSHBackgroundOutputContinuations(rootHistory, completedChildren,
		completed.Task.ID, rootSessionID, rootDefinitions)
	if err != nil || waiting || len(continuations) != 0 {
		t.Fatalf("fully consumed child output was not idempotently recognized: continuations=%#v waiting=%t err=%v", continuations, waiting, err)
	}
}

const (
	f07BackgroundOutputCallID  = "f07_read_background_output"
	f07BackgroundOutputSummary = "The child completed from its durable checkpoint."
	f07BackgroundParentSummary = "I reviewed the durable background result."
)

type f07SyntheticTool struct{ calls atomic.Int32 }

type f07CheckpointEntry struct {
	from, through int64
	digest        []byte
	payload       []byte
}

type f07ObservedCheckpoints struct {
	*f01ObservedCheckpoints
	mu        sync.Mutex
	bySession map[string][]f07CheckpointEntry
}

func (checkpoints *f07ObservedCheckpoints) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	checkpoints.mu.Lock()
	checkpoints.bySession[command.Fence.AgentSessionID] = append(checkpoints.bySession[command.Fence.AgentSessionID], f07CheckpointEntry{
		from: command.From, through: command.Through, digest: append([]byte(nil), command.Digest...), payload: append([]byte(nil), command.Payload...),
	})
	checkpoints.mu.Unlock()
	return checkpoints.f01ObservedCheckpoints.Save(ctx, command)
}

func (checkpoints *f07ObservedCheckpoints) rootHistory(sessionID string, through int64) (*cloudDSHCheckpointHistory, error) {
	checkpoints.mu.Lock()
	entries := append([]f07CheckpointEntry(nil), checkpoints.bySession[sessionID]...)
	checkpoints.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].from == entries[j].from {
			return entries[i].through < entries[j].through
		}
		return entries[i].from < entries[j].from
	})
	history := newCloudDSHCheckpointHistory(sessionID)
	for _, entry := range entries {
		if entry.through < history.nextSeq {
			continue
		}
		if err := history.append(entry.payload, entry.from, entry.through); err != nil {
			return nil, err
		}
	}
	if history.nextSeq != through+1 {
		return nil, fmt.Errorf("captured root checkpoint ends at %d; persisted task head is %d", history.nextSeq-1, through)
	}
	return history, nil
}

func f07ReadPersistedChildren(ctx context.Context, pool *pgxpool.Pool, taskID, ownerID, rootSessionID string) ([]app.AgentChildSession, error) {
	rows, err := pool.Query(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,st.id,st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.parent_session_id=$3 ORDER BY st.sequence`, taskID, ownerID, rootSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := make([]app.AgentChildSession, 0)
	for rows.Next() {
		var child app.AgentChildSession
		var allowedTools []byte
		var status string
		if err := rows.Scan(&child.TaskID, &child.SessionID, &child.ParentSessionID, &child.SpawnKey, &child.CheckpointThrough,
			&child.CheckpointDigest, &allowedTools, &child.Background, &child.CancelRequested, &child.Step.ID, &child.Step.Sequence,
			&child.Step.Kind, &status, &child.Step.Title, &child.Step.Summary, &child.Step.CreatedAt); err != nil {
			return nil, err
		}
		child.Step.TaskID, child.Step.Status = child.TaskID, task.StepStatus(status)
		if err := json.Unmarshal(allowedTools, &child.AllowedTools); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

func (tools *f07SyntheticTool) definition() app.AgentToolDefinition {
	return app.AgentToolDefinition{ModelTool: app.ModelTool{Name: "f07_test_noop", Description: "A test-only tool that must not execute.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)}}
}

func (tools *f07SyntheticTool) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{tools.definition()}, nil
}

func (tools *f07SyntheticTool) Execute(context.Context, task.Task, app.ModelToolCall) (app.AgentToolResult, error) {
	tools.calls.Add(1)
	return app.AgentToolResult{}, errors.New("synthetic no-op tool must not execute")
}

func (*f07SyntheticTool) InitialContext(context.Context, task.Task) (string, error) {
	return "Synthetic context for this integration test.", nil
}

type f07BackgroundOutputModel struct {
	mu             sync.Mutex
	rootCalls      map[int64]int
	childCalls     map[int64]int
	callLog        []string
	childStarted   chan struct{}
	interrupted    chan struct{}
	childOnce      sync.Once
	interruptOnce  sync.Once
	jobID          string
	cursor         int64
	outputConsumed atomic.Bool
}

func newF07BackgroundOutputModel() *f07BackgroundOutputModel {
	return &f07BackgroundOutputModel{rootCalls: make(map[int64]int), childCalls: make(map[int64]int),
		childStarted: make(chan struct{}), interrupted: make(chan struct{}), cursor: -1}
}

func (model *f07BackgroundOutputModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("synthetic model call is missing its Task fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}
	model.mu.Lock()
	model.callLog = append(model.callLog, fmt.Sprintf("gen=%d session=%s root=%s through=%d", call.Task.AgentGeneration,
		call.Task.AgentSessionID, call.Task.AgentRootSessionID, call.Task.ThroughSequence))
	model.mu.Unlock()
	if call.Task.AgentSessionID != call.Task.AgentRootSessionID {
		model.mu.Lock()
		model.childCalls[call.Task.AgentGeneration]++
		generation := call.Task.AgentGeneration
		model.mu.Unlock()
		if generation == 1 {
			model.childOnce.Do(func() { close(model.childStarted) })
			select {
			case <-ctx.Done():
				return app.ModelResponse{}, ctx.Err()
			case <-model.interrupted:
				return app.ModelResponse{}, errors.New("synthetic child request interrupted for restart")
			}
		}
		if generation == 2 {
			return app.ModelResponse{Text: f07BackgroundOutputSummary, StopReason: "stop", Usage: usage}, nil
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected child model generation %d", generation)
	}
	model.mu.Lock()
	model.rootCalls[call.Task.AgentGeneration]++
	callNumber := model.rootCalls[call.Task.AgentGeneration]
	model.mu.Unlock()
	switch call.Task.AgentGeneration {
	case 1:
		if callNumber == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "f07_spawn_background", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Restartable result","objective":"Return a concise result for the parent.","tool_names":[],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
	case 2:
		if callNumber == 1 {
			return app.ModelResponse{Text: "I will review the delegated result before finishing.", StopReason: "stop", Usage: usage}, nil
		}
	case 3:
		if callNumber == 1 {
			if !strings.Contains(string(call.RawRequest), "Internal continuation:") {
				return app.ModelResponse{}, errors.New("parent recovery omitted its trusted output continuation")
			}
			model.mu.Lock()
			jobID := model.jobID
			model.mu.Unlock()
			if jobID == "" {
				return app.ModelResponse{}, errors.New("settled child identity is unavailable")
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: f07BackgroundOutputCallID, Name: cloudDSHJobOutputToolName,
				Arguments: fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, jobID)}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 2 {
			outputText, ok := f01ToolResult(call.RawRequest, f07BackgroundOutputCallID)
			var result struct {
				JobID      string `json:"job_id"`
				Status     string `json:"status"`
				NextCursor int64  `json:"next_cursor"`
				Output     string `json:"output"`
			}
			model.mu.Lock()
			jobID, cursor := model.jobID, model.cursor
			model.mu.Unlock()
			if !ok || json.Unmarshal([]byte(outputText), &result) != nil || result.JobID != jobID || result.Status != "succeeded" ||
				result.NextCursor != cursor || result.Output != f07BackgroundOutputSummary {
				return app.ModelResponse{}, errors.New("parent did not receive the complete durable child output")
			}
			model.outputConsumed.Store(true)
			return app.ModelResponse{Text: f07BackgroundParentSummary, StopReason: "stop", Usage: usage}, nil
		}
	}
	return app.ModelResponse{}, fmt.Errorf("unexpected parent model generation/call %d/%d", call.Task.AgentGeneration, callNumber)
}

func (model *f07BackgroundOutputModel) setJob(jobID string, cursor int64) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.jobID, model.cursor = jobID, cursor
}

func (model *f07BackgroundOutputModel) callSnapshot() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]string(nil), model.callLog...)
}

func (model *f07BackgroundOutputModel) interrupt() {
	model.interruptOnce.Do(func() { close(model.interrupted) })
}

func f07AgentFence(value task.Task, supervisorID string) app.AgentInstanceFence {
	return app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: value.AgentInstanceID, AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID,
		SupervisorID: supervisorID, LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version,
		AuthorizationVersion: value.Authorization.Version, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
}

func f01NodeRuntimePaths(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real dsh subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	return node, script
}

type f01ObservedCheckpoints struct {
	*app.AgentCheckpointService
	mu     sync.Mutex
	err    error
	events []string
}

func (c *f01ObservedCheckpoints) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	type checkpointEvent struct {
		Sequence int64  `json:"seq"`
		Type     string `json:"type"`
		Data     struct {
			Source struct {
				Kind   string `json:"kind"`
				Plugin string `json:"plugin"`
			} `json:"source"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"data"`
	}
	var body struct {
		Events []checkpointEvent `json:"events"`
	}
	if json.Unmarshal(command.Payload, &body) == nil {
		c.mu.Lock()
		for _, event := range body.Events {
			proof := false
			for _, block := range event.Data.Content {
				proof = proof || strings.Contains(block.Text, app.CloudDSHMemoryProofPrefix)
			}
			c.events = append(c.events, fmt.Sprintf("task=%s session=%s seq=%d type=%s", command.Fence.TaskID,
				command.Fence.AgentSessionID, event.Sequence, event.Type)+fmt.Sprintf(" source=%s/%s proof=%t", event.Data.Source.Kind, event.Data.Source.Plugin, proof))
		}
		c.mu.Unlock()
	}
	replayed, err := c.AgentCheckpointService.Save(ctx, command)
	if err != nil {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
	}
	return replayed, err
}

func (c *f01ObservedCheckpoints) lastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *f01ObservedCheckpoints) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

type f01ObservedChildSessions struct {
	CloudDSHChildSessions
	mu    sync.Mutex
	calls []string
}

func (c *f01ObservedChildSessions) Create(ctx context.Context, command app.AgentChildSessionCommand) (app.AgentChildSession, bool, error) {
	entry := fmt.Sprintf("create_enter task=%s parent=%s spawn=%s", command.Fence.TaskID, command.ParentSessionID, command.SpawnKey)
	c.mu.Lock()
	c.calls = append(c.calls, entry)
	c.mu.Unlock()
	child, replayed, err := c.CloudDSHChildSessions.Create(ctx, command)
	c.mu.Lock()
	c.calls = append(c.calls, fmt.Sprintf("create_exit task=%s session=%s step=%s status=%s replayed=%t err=%v",
		child.TaskID, child.SessionID, child.Step.ID, child.Step.Status, replayed, err))
	c.mu.Unlock()
	return child, replayed, err
}

func (c *f01ObservedChildSessions) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

type cancelRestartModel struct {
	mu                sync.Mutex
	rootCalls         map[int64]int
	childCalls        map[int64]int
	childCheckpointed chan struct{}
	interrupted       chan struct{}
	childOnce         sync.Once
	interruptOnce     sync.Once
}

func (m *cancelRestartModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model call omitted its Agent fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID {
		m.mu.Lock()
		if m.rootCalls == nil {
			m.rootCalls = make(map[int64]int)
		}
		m.rootCalls[call.Task.AgentGeneration]++
		callNumber := m.rootCalls[call.Task.AgentGeneration]
		m.mu.Unlock()
		if call.Task.AgentGeneration > 1 {
			return app.ModelResponse{Text: "Parent model must not run while the cancelled child outcome is unknown.",
				StopReason: "stop", Usage: usage}, nil
		}
		if callNumber == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "spawn_cancelled_child", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Cancelable file job","objective":"Write the file, then wait for follow-up instructions.","tool_names":["file_write"],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 2 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "list_cancelled_jobs", Name: cloudDSHJobListToolName,
				Arguments: `{}`}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 3 {
			output, ok := f01ToolResult(call.RawRequest, "list_cancelled_jobs")
			var jobs []cloudDSHJobView
			if !ok || json.Unmarshal([]byte(output), &jobs) != nil || len(jobs) != 1 || jobs[0].JobID == "" {
				return app.ModelResponse{}, errors.New("cancel test job was not listed")
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "get_cancelled_job_001", Name: cloudDSHJobGetToolName,
				Arguments: fmt.Sprintf(`{"job_id":%q}`, jobs[0].JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		previousGet := fmt.Sprintf("get_cancelled_job_%03d", callNumber-3)
		output, ok := f01ToolResult(call.RawRequest, previousGet)
		var job cloudDSHJobView
		if !ok || json.Unmarshal([]byte(output), &job) != nil || job.JobID == "" ||
			(job.Status != "running" && job.Status != "cancelling") {
			return app.ModelResponse{}, errors.New("cancel test job status changed before the interruption")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: fmt.Sprintf("get_cancelled_job_%03d", callNumber-2),
			Name: cloudDSHJobGetToolName, Arguments: fmt.Sprintf(`{"job_id":%q}`, job.JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
	}

	m.mu.Lock()
	if m.childCalls == nil {
		m.childCalls = make(map[int64]int)
	}
	m.childCalls[call.Task.AgentGeneration]++
	callNumber := m.childCalls[call.Task.AgentGeneration]
	m.mu.Unlock()
	if call.Task.AgentGeneration > 1 {
		return app.ModelResponse{}, errors.New("cancelled child runtime was incorrectly resumed")
	}
	if callNumber == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "cancel_child_write", Name: "file_write",
			Arguments: `{"workspace_id":"w1234567","path":"cancel-check.txt","content":"write once"}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	m.childOnce.Do(func() { close(m.childCheckpointed) })
	select {
	case <-ctx.Done():
		return app.ModelResponse{}, ctx.Err()
	case <-m.interrupted:
		return app.ModelResponse{}, errors.New("test Runtime stopped during the model request")
	}
}

func (m *cancelRestartModel) interrupt() { m.interruptOnce.Do(func() { close(m.interrupted) }) }

func (m *cancelRestartModel) childCallCount(generation int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.childCalls[generation]
}

func (m *cancelRestartModel) callCount(generation int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootCalls[generation]
}

type cancelRestartTools struct {
	definition           app.AgentToolDefinition
	writes               atomic.Int32
	memoryRecoveryPauses atomic.Int32
	mu                   sync.Mutex
	taskIDs              []string
}

type parentSiblingCancelCompletionStore interface {
	CommitTaskProgress(context.Context, app.TaskProgress) (task.Event, error)
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}

type parentSiblingCancelCompletion struct {
	store  parentSiblingCancelCompletionStore
	errors chan<- error
}

func (completion *parentSiblingCancelCompletion) Complete(ctx context.Context, claimed task.Task, result app.CloudDSHTaskCompletion) (err error) {
	defer func() {
		if err != nil && completion.errors != nil {
			select {
			case completion.errors <- err:
			default:
			}
		}
	}()
	details, err := completion.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	sequence := int64(1)
	for _, step := range details.Steps {
		if step.Sequence >= sequence {
			sequence = step.Sequence + 1
		}
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"status": string(task.StatusCompleted), "source": "cloud_dsh_test"})
	if err != nil {
		return err
	}
	_, err = completion.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result.Result, Usage: claimed.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: sequence, Kind: "result", Status: task.StepSucceeded,
			Title: "Complete sibling task", Summary: "The sibling completed independently after its parent was stopped."}},
		PublishResultMessage: true,
		Event:                task.Event{TaskID: claimed.ID, Type: "task.result", Payload: payload},
	})
	return err
}

func (tools *cancelRestartTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{tools.definition}, nil
}

func (tools *cancelRestartTools) Execute(_ context.Context, value task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	if call.Name != "file_write" {
		return app.AgentToolResult{}, fmt.Errorf("unexpected test tool %q", call.Name)
	}
	tools.mu.Lock()
	tools.taskIDs = append(tools.taskIDs, value.ID)
	tools.mu.Unlock()
	tools.writes.Add(1)
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: `{"path":"cancel-check.txt","revision":1}`}, nil
}

func (*cancelRestartTools) InitialContext(context.Context, task.Task) (string, error) {
	return "Authorized test context.\n" + app.CloudDSHMemoryProofPrefix + "f01-test-personal-proof", nil
}

func (*cancelRestartTools) ValidatePersonalMemoryProof(_ context.Context, _ task.Task, proof string, _ []app.CloudDSHMemoryVersion) error {
	if proof != "f01-test-personal-proof" {
		return errors.New("test personal context proof is invalid")
	}
	return nil
}

func (*cancelRestartTools) CloudDSHPersonalMemoryProofEnabled() bool { return true }

func (tools *cancelRestartTools) PausePersonalMemoryRecovery(context.Context, task.Task) error {
	tools.memoryRecoveryPauses.Add(1)
	return nil
}

func (tools *cancelRestartTools) taskIDsSnapshot() []string {
	tools.mu.Lock()
	defer tools.mu.Unlock()
	return append([]string(nil), tools.taskIDs...)
}

type parentSiblingCancelModel struct {
	parentTaskID      string
	siblingTaskID     string
	parentCalls       int
	childCalls        int
	mu                sync.Mutex
	parentWaiting     chan struct{}
	childRunning      chan struct{}
	childSessionID    chan string
	siblingWaiting    chan struct{}
	siblingRelease    chan struct{}
	siblingReturned   chan struct{}
	siblingReturnOnce sync.Once
	parentCancelled   chan struct{}
	childCancelled    chan struct{}
	siblingCancelled  chan struct{}
	parentOnce        sync.Once
	childOnce         sync.Once
	siblingOnce       sync.Once
	childCancelOnce   sync.Once
	siblingCancelOnce sync.Once
}

func newParentSiblingCancelModel(parentTaskID, siblingTaskID string) *parentSiblingCancelModel {
	return &parentSiblingCancelModel{
		parentTaskID: parentTaskID, siblingTaskID: siblingTaskID,
		parentWaiting: make(chan struct{}), childRunning: make(chan struct{}), childSessionID: make(chan string, 1),
		siblingWaiting: make(chan struct{}), siblingRelease: make(chan struct{}), siblingReturned: make(chan struct{}),
		parentCancelled: make(chan struct{}), childCancelled: make(chan struct{}), siblingCancelled: make(chan struct{}),
	}
}

func (m *parentSiblingCancelModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model call omitted its Agent fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}
	if call.Task.TaskID == m.siblingTaskID {
		m.siblingOnce.Do(func() { close(m.siblingWaiting) })
		select {
		case <-ctx.Done():
			m.siblingCancelOnce.Do(func() { close(m.siblingCancelled) })
			return app.ModelResponse{}, ctx.Err()
		case <-m.siblingRelease:
			m.siblingReturnOnce.Do(func() { close(m.siblingReturned) })
			return app.ModelResponse{Text: "Sibling completed independently.", StopReason: "stop", Usage: usage}, nil
		}
	}
	if call.Task.TaskID != m.parentTaskID {
		return app.ModelResponse{}, errors.New("model call escaped its test Task")
	}
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID {
		m.mu.Lock()
		m.parentCalls++
		callNumber := m.parentCalls
		m.mu.Unlock()
		if callNumber == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "spawn_parent_child", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Cancelable child","objective":"Write one test file, then wait.","tool_names":["file_write"],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 2 {
			m.parentOnce.Do(func() { close(m.parentWaiting) })
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "list_parent_children", Name: cloudDSHJobListToolName,
				Arguments: `{}`}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		if callNumber == 3 {
			output, ok := f01ToolResult(call.RawRequest, "list_parent_children")
			var jobs []cloudDSHJobView
			if !ok || json.Unmarshal([]byte(output), &jobs) != nil || len(jobs) != 1 || jobs[0].JobID == "" {
				return app.ModelResponse{}, errors.New("parent's background child was not listed")
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "get_parent_child_001", Name: cloudDSHJobGetToolName,
				Arguments: fmt.Sprintf(`{"job_id":%q}`, jobs[0].JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		previousGet := fmt.Sprintf("get_parent_child_%03d", callNumber-3)
		output, ok := f01ToolResult(call.RawRequest, previousGet)
		var job cloudDSHJobView
		if !ok || json.Unmarshal([]byte(output), &job) != nil || job.JobID == "" ||
			(job.Status != "running" && job.Status != "cancelling") {
			return app.ModelResponse{}, errors.New("parent's background child stopped before the cancellation test")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: fmt.Sprintf("get_parent_child_%03d", callNumber-2),
			Name: cloudDSHJobGetToolName, Arguments: fmt.Sprintf(`{"job_id":%q}`, job.JobID)}}, StopReason: "tool_calls", Usage: usage}, nil
	}

	m.mu.Lock()
	m.childCalls++
	callNumber := m.childCalls
	m.mu.Unlock()
	if callNumber == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "parent_child_write", Name: "file_write",
			Arguments: `{"workspace_id":"w1234567","path":"cancel-check.txt","content":"write once"}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	m.childSessionID <- call.Task.AgentSessionID
	m.childOnce.Do(func() { close(m.childRunning) })
	<-ctx.Done()
	m.childCancelOnce.Do(func() { close(m.childCancelled) })
	return app.ModelResponse{}, ctx.Err()
}

func TestCloudDSHConcurrentOwnersKeepOutputMessagesAndEventsIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)

	type ownerTask struct {
		owner        identity.User
		conversation string
		taskID       string
		output       string
	}
	owners := []identity.User{createF01User(t, ctx, store), createF01User(t, ctx, store)}
	for _, owner := range owners {
		if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	ownerTasks := make([]ownerTask, 0, len(owners))
	for i, owner := range owners {
		principal, verifyErr := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
		if verifyErr != nil || principal.UserID != owner.ID || principal.Kind != identity.KindSession {
			t.Fatalf("verify owner %d Task session: principal=%#v err=%v", i, principal, verifyErr)
		}
		submission, submitErr := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
			ModelID: "p1234567", Objective: fmt.Sprintf("Answer in chat with the private marker for owner %d.", i+1),
			Budget:         task.Budget{ActiveSeconds: 120, ToolCalls: 2, ModelTokens: 1000},
			IdempotencyKey: fmt.Sprintf("f01-owner-isolation-%d", i+1),
		})
		if submitErr != nil {
			t.Fatalf("submit owner %d Task: %v", i, submitErr)
		}
		if submission.Task.Status != task.StatusQueued {
			t.Fatalf("owner %d Task status=%s file_delivery_intent=%s; want queued chat task", i,
				submission.Task.Status, submission.Task.FileDeliveryIntent)
		}
		ownerTasks = append(ownerTasks, ownerTask{owner: owner, conversation: submission.Conversation.ID,
			taskID: submission.Task.ID, output: fmt.Sprintf("private-output-owner-%d", i+1)})
	}

	node, script := f01NodeRuntimePaths(t)
	accountScript := filepath.Join(filepath.Dir(script), "bin-account.mjs")
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stopKey := bytes.Repeat([]byte{0x74}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorAuth, err := app.NewAgentStopAuthenticator("f01-owner-isolation-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator("f01-owner-isolation-supervisor", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	model := &f01ConcurrentOwnerOutputModel{owners: make(map[string]string), outputs: make(map[string]string),
		started: make(chan string, len(ownerTasks)), release: make(chan struct{})}
	for _, value := range ownerTasks {
		model.owners[value.taskID] = value.owner.ID
		model.outputs[value.taskID] = value.output
	}
	baseSupervisor := &f01RecoveringNodeAccountSupervisor{
		f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
		recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
	}
	runtimeSupervisor := &f01TrackedNodeAccountSupervisor{f01RecoveringNodeAccountSupervisor: baseSupervisor}
	defer func() {
		for _, command := range runtimeSupervisor.commandsSnapshot() {
			if command != nil && command.Process != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}
	}()
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	guardian := &f01SignedNodeTaskGuardian{key: privateKey}
	runtimeManager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: runtimeSupervisor, Guardian: guardian, StopVerifier: runtimeStopVerifier,
		TaskStopper: instances, LeaseOwner: "f01-owner-isolation-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: "f01-owner-isolation-supervisor",
		AccountRuntimes: runtimeManager,
		Instances:       instances, Checkpoints: checkpoints, Models: model,
		Completion: &f01OwnerIsolationCompletion{store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	executionErrors := make(chan error, len(ownerTasks))
	recorder := &f01OwnerIsolationRecordingExecutor{inner: executor, errors: executionErrors}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: recorder, WorkerID: "f01-owner-isolation-worker", LeaseDuration: 3 * time.Second,
		PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	workerDoneConsumed := false
	defer func() {
		cancelWorker()
		if workerDoneConsumed {
			return
		}
		select {
		case runErr := <-workerDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Errorf("TaskWorker.Run() = %v, want context canceled during cleanup", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()

	diagnostic := func() string {
		diagnosticCtx, cancelDiagnostic := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelDiagnostic()
		states := make([]string, 0, len(ownerTasks))
		for _, value := range ownerTasks {
			current, getErr := store.GetTask(diagnosticCtx, value.taskID, value.owner.ID)
			if getErr != nil {
				states = append(states, fmt.Sprintf("owner=%s task=%s get_error=%v", value.owner.ID, value.taskID, getErr))
				continue
			}
			events, eventsErr := store.ListTaskEvents(diagnosticCtx, value.owner.ID, value.taskID, 0, 100)
			eventDetails := make([]string, 0, len(events))
			for _, event := range events {
				eventDetails = append(eventDetails, fmt.Sprintf("%s:%s", event.Type, event.Payload))
			}
			states = append(states, fmt.Sprintf("owner=%s task=%s status=%s waiting=%s agent=%s/%d session=%s lease=%s/%d result=%q events=%v events_error=%v",
				value.owner.ID, value.taskID, current.Task.Status, current.Task.WaitingReason, current.Task.AgentInstanceID,
				current.Task.AgentGeneration, current.Task.AgentSessionID, current.Task.Lease.Owner, current.Task.Lease.Version,
				current.Task.Result, eventDetails, eventsErr))
		}
		processes := make([]string, 0)
		for _, command := range runtimeSupervisor.commandsSnapshot() {
			if command == nil || command.Process == nil {
				processes = append(processes, "unavailable")
				continue
			}
			processes = append(processes, fmt.Sprintf("pid=%d state=%v", command.Process.Pid, command.ProcessState))
		}
		var executionErrs []string
		for {
			select {
			case executionErr := <-executionErrors:
				executionErrs = append(executionErrs, executionErr.Error())
			default:
				return fmt.Sprintf("tasks=%v node_processes=%v node_stderr=%q execution_errors=%v", states, processes, runtimeSupervisor.stderrSnapshot(), executionErrs)
			}
		}
	}
	seen := make(map[string]bool, len(ownerTasks))
	for range ownerTasks {
		select {
		case taskID := <-model.started:
			if _, exists := model.owners[taskID]; !exists || seen[taskID] {
				t.Fatalf("unexpected or duplicate concurrent model request for Task %q", taskID)
			}
			seen[taskID] = true
		case executionErr := <-executionErrors:
			t.Fatalf("Task execution stopped before reaching the synthetic model: %v; %s", executionErr, diagnostic())
		case workerErr := <-workerDone:
			workerDoneConsumed = true
			t.Fatalf("TaskWorker returned before reaching the synthetic model: %v; %s", workerErr, diagnostic())
		case <-ctx.Done():
			t.Fatalf("both owners did not reach the synthetic model concurrently: seen=%v err=%v; %s", seen, ctx.Err(), diagnostic())
		}
	}
	if commands := runtimeSupervisor.commandsSnapshot(); len(commands) != len(ownerTasks) {
		t.Fatalf("local Node account Runtime processes=%d; want one per owner (%d)", len(commands), len(ownerTasks))
	}
	close(model.release)
	for _, value := range ownerTasks {
		if err := waitF01(t, ctx, func() (bool, error) {
			current, err := store.GetTask(ctx, value.taskID, value.owner.ID)
			return err == nil && current.Task.Status == task.StatusCompleted, err
		}, nil, nil); err != nil {
			t.Fatalf("owner %s Task did not complete: %v; %s", value.owner.ID, err, diagnostic())
		}
		current, err := store.GetTask(ctx, value.taskID, value.owner.ID)
		if err != nil || current.Task.Result != value.output {
			t.Fatalf("owner %s Task output=%q err=%v; want %q", value.owner.ID, current.Task.Result, err, value.output)
		}
		messages, err := store.ListMessages(ctx, value.owner.ID, value.conversation, app.PageRequest{Limit: 100})
		if err != nil {
			t.Fatalf("list owner %s conversation messages: %v", value.owner.ID, err)
		}
		assistantOutputs := make([]string, 0, 1)
		for _, message := range messages.Items {
			if message.Role != conversation.RoleAssistant {
				continue
			}
			if message.OwnerID != value.owner.ID || message.TaskID != value.taskID {
				t.Fatalf("assistant message crossed owner or Task boundary: %#v", message)
			}
			assistantOutputs = append(assistantOutputs, message.Content)
		}
		if len(assistantOutputs) != 1 || assistantOutputs[0] != value.output {
			t.Fatalf("owner %s assistant messages=%q; want only %q", value.owner.ID, assistantOutputs, value.output)
		}
		events, err := store.ListTaskEvents(ctx, value.owner.ID, value.taskID, 0, 100)
		if err != nil || len(events) == 0 {
			t.Fatalf("list owner %s Task events=%d err=%v", value.owner.ID, len(events), err)
		}
		resultEvents := 0
		otherOutput := ownerTasks[0].output
		if otherOutput == value.output {
			otherOutput = ownerTasks[1].output
		}
		for _, event := range events {
			if event.TaskID != value.taskID || bytes.Contains(event.Payload, []byte(otherOutput)) {
				t.Fatalf("Task event crossed owner boundary: task=%s event=%#v", value.taskID, event)
			}
			if event.Type == "task.result" {
				resultEvents++
			}
		}
		if resultEvents != 1 {
			t.Fatalf("owner %s result event count=%d; want 1", value.owner.ID, resultEvents)
		}
	}
	for i, value := range ownerTasks {
		other := ownerTasks[1-i]
		if _, err := store.GetTask(ctx, value.taskID, other.owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("cross-owner Task lookup returned err=%v; want not_found", err)
		}
		if _, err := store.ListMessages(ctx, other.owner.ID, value.conversation, app.PageRequest{Limit: 100}); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("cross-owner conversation lookup returned err=%v; want not_found", err)
		}
		if _, err := store.ListTaskEvents(ctx, other.owner.ID, value.taskID, 0, 100); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("cross-owner event lookup returned err=%v; want not_found", err)
		}
	}
}

type f01TrackedNodeAccountSupervisor struct {
	*f01RecoveringNodeAccountSupervisor
	mu       sync.Mutex
	commands []*exec.Cmd
}

func (s *f01TrackedNodeAccountSupervisor) StartAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonce []byte, expiresAt time.Time) (CloudDSHAccountRuntimeProcess, error) {
	process, err := s.f01RecoveringNodeAccountSupervisor.StartAccountRuntime(ctx, fence, nonce, expiresAt)
	if err != nil {
		return process, err
	}
	// Acquire serializes account Runtime starts. Capture each command so the
	// two local Node processes can both be reaped by this test.
	s.mu.Lock()
	s.commands = append(s.commands, s.cmd)
	s.mu.Unlock()
	return process, nil
}

func (s *f01TrackedNodeAccountSupervisor) commandsSnapshot() []*exec.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*exec.Cmd(nil), s.commands...)
}

type f01ConcurrentOwnerOutputModel struct {
	owners  map[string]string
	outputs map[string]string
	started chan string
	release chan struct{}
}

func (m *f01ConcurrentOwnerOutputModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model call omitted its Agent fence")
	}
	ownerID, ok := m.owners[call.Task.TaskID]
	output, hasOutput := m.outputs[call.Task.TaskID]
	if !ok || !hasOutput || call.Task.OwnerID != ownerID {
		return app.ModelResponse{}, errors.New("model call crossed the expected owner and Task mapping")
	}
	select {
	case m.started <- call.Task.TaskID:
	case <-ctx.Done():
		return app.ModelResponse{}, ctx.Err()
	}
	select {
	case <-m.release:
		return app.ModelResponse{Text: output, StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}}, nil
	case <-ctx.Done():
		return app.ModelResponse{}, ctx.Err()
	}
}

type f01OwnerIsolationCompletionStore interface {
	CommitTaskProgress(context.Context, app.TaskProgress) (task.Event, error)
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}

type f01OwnerIsolationRecordingExecutor struct {
	inner  TaskExecutor
	errors chan<- error
}

func (executor *f01OwnerIsolationRecordingExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	err := executor.inner.ExecuteTask(ctx, value)
	if err != nil {
		select {
		case executor.errors <- err:
		default:
		}
	}
	return err
}

type f01OwnerIsolationCompletion struct {
	store f01OwnerIsolationCompletionStore
}

func (completion *f01OwnerIsolationCompletion) Complete(ctx context.Context, claimed task.Task, result app.CloudDSHTaskCompletion) error {
	details, err := completion.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	sequence := int64(1)
	for _, step := range details.Steps {
		if step.Sequence >= sequence {
			sequence = step.Sequence + 1
		}
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"status": string(task.StatusCompleted), "source": "owner_isolation_test"})
	if err != nil {
		return err
	}
	_, err = completion.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result.Result, Usage: claimed.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: sequence, Kind: "result", Status: task.StepSucceeded,
			Title: "Complete owner isolation task", Summary: "Synthetic DSH output was isolated by account owner."}},
		PublishResultMessage: true,
		Event:                task.Event{TaskID: claimed.ID, Type: "task.result", Payload: payload},
	})
	return err
}
