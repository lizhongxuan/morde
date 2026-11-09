package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type revisionBlockingModel struct {
	mu                sync.Mutex
	calls             []app.ModelCall
	started           chan struct{}
	firstNoted        chan struct{}
	firstCause        error
	lifecycleAlive    bool
	explicitStillOpen bool
}

func (*revisionBlockingModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return false, nil
}

func revisionPersonalBroker(t *testing.T, store *Store) *app.CloudDSHToolBroker {
	t.Helper()
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := app.NewPersonalAttachmentService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, &app.RuntimeToolExecutor{}, nil, nil, capture,
		personalData, personalSync, goals, attachments, nil)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func (m *revisionBlockingModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	first := len(m.calls) == 1
	m.mu.Unlock()
	if first {
		close(m.started)
		<-ctx.Done()
		m.mu.Lock()
		m.firstCause = context.Cause(ctx)
		lifecycle, explicit := app.ModelTaskLifecycle(ctx), app.ModelExplicitCancellation(ctx)
		m.lifecycleAlive = lifecycle != nil && lifecycle.Err() == nil
		m.explicitStillOpen = explicit != nil && explicit.Err() == nil
		m.mu.Unlock()
		if m.firstNoted != nil {
			close(m.firstNoted)
		}
		// Simulate a late provider response that must never reach the user.
		return app.ModelResponse{Text: "STALE ORIGINAL RESULT", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	}
	return app.ModelResponse{Text: "Updated result.", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
}

type revisionSideEffectModel struct {
	mu                sync.Mutex
	calls             []app.ModelCall
	started           chan struct{}
	firstNoted        chan struct{}
	firstCause        error
	lifecycleAlive    bool
	explicitStillOpen bool
}

func (*revisionSideEffectModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (m *revisionSideEffectModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	callNumber := len(m.calls)
	m.mu.Unlock()
	switch callNumber {
	case 1:
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "revision_memory_write_1", Name: "morde_personal_object_update",
			Arguments: `{"object_type":"memory","object_id":"global","base_revision":0,"body":"{\"content\":\"completed before revision\"}"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	case 2:
		close(m.started)
		<-ctx.Done()
		m.mu.Lock()
		m.firstCause = context.Cause(ctx)
		lifecycle, explicit := app.ModelTaskLifecycle(ctx), app.ModelExplicitCancellation(ctx)
		m.lifecycleAlive = lifecycle != nil && lifecycle.Err() == nil
		m.explicitStillOpen = explicit != nil && explicit.Err() == nil
		m.mu.Unlock()
		close(m.firstNoted)
		return app.ModelResponse{Text: "STALE ORIGINAL RESULT", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	default:
		return app.ModelResponse{Text: "Updated result.", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	}
}

func revisionTestExecutor(t *testing.T, store *Store, model worker.CloudDSHModels) *worker.CloudDSHExecutor {
	return revisionTestExecutorWithCheckpoints(t, store, model, nil)
}

func revisionTestExecutorWithCheckpoints(t *testing.T, store *Store, model worker.CloudDSHModels,
	wrap func(worker.CloudDSHCheckpoints) worker.CloudDSHCheckpoints, tools ...worker.CloudDSHTools) *worker.CloudDSHExecutor {
	return revisionTestExecutorWithOptions(t, store, model, wrap, nil, tools...)
}

func revisionTestExecutorWithOptions(t *testing.T, store *Store, model worker.CloudDSHModels,
	wrap func(worker.CloudDSHCheckpoints) worker.CloudDSHCheckpoints, configure func(*worker.CloudDSHExecutorOptions),
	tools ...worker.CloudDSHTools) *worker.CloudDSHExecutor {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stopAuth, err := app.NewAgentStopAuthenticator("revision-test-supervisor", bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	agentAuth, err := app.NewAgentGuardianStopAuthenticator("revision-test-supervisor", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, stopAuth, agentAuth, taskAuth)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpointService, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints worker.CloudDSHCheckpoints = checkpointService
	if wrap != nil {
		checkpoints = wrap(checkpoints)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &recordCloudDSHE2ESupervisor{node: node, script: script, key: privateKey}
	t.Cleanup(func() {
		supervisor.mu.Lock()
		defer supervisor.mu.Unlock()
		if supervisor.cmd != nil && supervisor.cmd.Process != nil {
			_ = supervisor.cmd.Process.Kill()
			_ = supervisor.cmd.Wait()
		}
	})
	manager, err := worker.NewCloudDSHAccountRuntimeManager(worker.CloudDSHAccountRuntimeManagerOptions{Control: control, Supervisor: supervisor,
		Guardian: &recordCloudDSHE2EGuardian{key: privateKey}, StopVerifier: verifier, TaskStopper: instances,
		LeaseOwner: "revision-test-supervisor", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	options := worker.CloudDSHExecutorOptions{SupervisorID: "revision-test-supervisor",
		AccountRuntimes: manager, Instances: instances, Checkpoints: checkpoints, Models: model, RevisionStops: store,
		ProjectContexts: store}
	if len(tools) != 0 {
		options.Tools = tools[0]
	}
	if configure != nil {
		configure(&options)
	}
	executor, err := worker.NewCloudDSHExecutor(options)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestConversationRevisionStopsRealAgentAndRestartsWithLatestMessages(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	service, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx := app.WithModelTaskLifecycle(service, service)
	f.authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl)}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: f.authorization.TokenID}
	personalData, err := app.NewPersonalMutationService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "revision-e2e-enable-memory", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	initial := f.receive(t, "revision-e2e-001", "Summarize the report", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	assertStarted := func(want bool) {
		t.Helper()
		details, err := f.store.GetTask(ctx, taskID, f.ownerID)
		if err != nil || details.CurrentRevisionStarted == nil || *details.CurrentRevisionStarted != want {
			t.Fatalf("current revision started projection: got=%v want=%t err=%v", details.CurrentRevisionStarted, want, err)
		}
	}
	assertStarted(false)
	var startsBeforeExecution int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_revision_starts WHERE task_id=$1`, taskID).Scan(&startsBeforeExecution); err != nil || startsBeforeExecution != 0 {
		t.Fatalf("queued task falsely recorded execution: count=%d err=%v", startsBeforeExecution, err)
	}
	first, found, err := f.store.ClaimNextTask(ctx, "revision-first-worker", time.Minute)
	if err != nil || !found || first.ID != taskID {
		t.Fatalf("claim initial task: found=%t err=%v", found, err)
	}
	model := &revisionSideEffectModel{started: make(chan struct{}), firstNoted: make(chan struct{})}
	executor := revisionTestExecutorWithCheckpoints(t, f.store, model, nil, revisionPersonalBroker(t, f.store))
	firstDone := make(chan error, 1)
	go func() { firstDone <- executor.ExecuteTask(ctx, first) }()
	select {
	case <-model.started:
	case err := <-firstDone:
		t.Fatalf("initial Agent stopped before model start: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("initial model did not start")
	}
	completedMutation, err := f.store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || completedMutation.Revision != 1 {
		t.Fatalf("initial personal mutation was not committed before revision: object=%+v err=%v", completedMutation, err)
	}
	var startsAtFirstTurn int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_revision_starts WHERE task_id=$1 AND revision=0`, taskID).Scan(&startsAtFirstTurn); err != nil || startsAtFirstTurn != 1 {
		t.Fatalf("initial turn start was not persisted: count=%d err=%v", startsAtFirstTurn, err)
	}
	assertStarted(true)
	var changes []string
	for index, text := range []string{"Only include revenue", "Use Chinese headings"} {
		message := f.receive(t, "revision-e2e-00"+string(rune('2'+index)), text, app.ConversationRoutingInput{ReferenceTaskID: taskID})
		snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, message)
		if err != nil || len(snapshot.Context.Candidates) != 1 {
			t.Fatalf("revision snapshot: %+v err=%v", snapshot, err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, message, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
			TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, message.Message.ID)
	}
	var fence app.ModelTaskFence
	fence.TaskID, fence.OwnerID, fence.InitiatorID = taskID, first.OwnerID, first.InitiatorID
	fence.LeaseOwner, fence.LeaseVersion = first.Lease.Owner, first.Lease.Version
	if err := f.store.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,auth_version,agent_engine_version,agent_profile_version
FROM tasks WHERE id=$1`, taskID).Scan(&fence.AgentInstanceID, &fence.AgentGeneration, &fence.AgentSessionID,
		&fence.AuthorizationVersion, &fence.EngineVersion, &fence.ProfileVersion); err != nil {
		t.Fatal(err)
	}
	fence.AgentRootSessionID = fence.AgentSessionID
	if proven, err := f.store.CheckRevisionSettlement(ctx, fence); err != nil || !proven {
		t.Fatalf("revision stop evidence missing: proven=%t err=%v", proven, err)
	}
	if proven, err := f.store.RevisionStopProven(ctx, first); err != nil || !proven {
		t.Fatalf("old task lease was not linked to revision stop: proven=%t err=%v", proven, err)
	}
	forged := fence
	forged.LeaseVersion++
	if proven, err := f.store.CheckRevisionSettlement(ctx, forged); err != nil || proven {
		t.Fatalf("forged old lease was accepted: proven=%t err=%v", proven, err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		t.Fatalf("revision started before stop proof: applied=%t err=%v", applied, err)
	}
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("interrupted original execution unexpectedly completed")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("old Agent did not stop after its lease was fenced")
	}
	select {
	case <-model.firstNoted:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted model did not observe its cancellation")
	}
	model.mu.Lock()
	cause, lifecycleAlive, explicitStillOpen := model.firstCause, model.lifecycleAlive, model.explicitStillOpen
	model.mu.Unlock()
	if !errors.Is(cause, app.ErrTaskRevisionInterrupted) || !lifecycleAlive || !explicitStillOpen {
		t.Fatalf("revision cancellation was not safely distinguished: cause=%v lifecycle=%t explicit_open=%t", cause, lifecycleAlive, explicitStillOpen)
	}
	var status, agentState, result string
	if err := f.store.pool.QueryRow(ctx, `SELECT t.status,i.state,t.result FROM tasks t JOIN task_agent_instances i
ON i.task_id=t.id AND i.agent_generation=t.agent_generation WHERE t.id=$1`, taskID).Scan(&status, &agentState, &result); err != nil ||
		status != "pausing" || agentState != "stopped" || strings.Contains(result, "STALE ORIGINAL RESULT") {
		t.Fatalf("old Agent stop or late result was not fenced: status=%s agent=%s result=%q err=%v", status, agentState, result, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.store.CheckRevisionSettlement(ctx, fence); err != nil || proven {
		t.Fatalf("revoked credential retained settlement permission: proven=%t err=%v", proven, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='cancelling' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.store.CheckRevisionSettlement(ctx, fence); err != nil || proven {
		t.Fatalf("manual stop retained settlement permission: proven=%t err=%v", proven, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='pausing' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.store.ClaimNextTaskStop(ctx, "revision-stop-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim stop: found=%t err=%v", found, err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply stopped revisions: applied=%t err=%v", applied, err)
	}
	assertStarted(false)
	second, found, err := f.store.ClaimNextTask(ctx, "revision-second-worker", time.Minute)
	if err != nil || !found || second.ID != taskID {
		t.Fatalf("claim revised task: found=%t err=%v", found, err)
	}
	if err := executor.ExecuteTask(ctx, second); !errors.Is(err, worker.ErrCloudDSHCompletionPending) {
		t.Fatalf("execute revised task: %v", err)
	}
	assertStarted(true)
	var initialStarts, skippedStarts, latestStarts int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revision=0),count(*) FILTER (WHERE revision=1),count(*) FILTER (WHERE revision=2)
FROM task_revision_starts WHERE task_id=$1`, taskID).Scan(&initialStarts, &skippedStarts, &latestStarts); err != nil ||
		initialStarts != 1 || skippedStarts != 0 || latestStarts != 1 {
		t.Fatalf("restarted task start markers are incorrect: initial=%d skipped=%d latest=%d err=%v", initialStarts, skippedStarts, latestStarts, err)
	}
	model.mu.Lock()
	calls := append([]app.ModelCall(nil), model.calls...)
	model.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("revisions made %d total model calls", len(calls))
	}
	var request struct {
		Messages []struct {
			ID      string `json:"id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(calls[2].RawRequest, &request); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, message := range request.Messages {
		for _, id := range changes {
			if message.ID == id {
				seen = append(seen, id)
			}
		}
		for _, block := range message.Content {
			if strings.Contains(block.Text, "STALE ORIGINAL RESULT") {
				t.Fatal("late old result reached revised model")
			}
		}
	}
	if strings.Join(seen, ",") != strings.Join(changes, ",") {
		t.Fatalf("revision inputs lost, duplicated or reordered: got=%v want=%v", seen, changes)
	}
	completedMutation, err = f.store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || completedMutation.Revision != 1 {
		t.Fatalf("completed tool side effect was repeated during revision recovery: object=%+v err=%v", completedMutation, err)
	}
	details, err := f.store.GetTask(ctx, taskID, f.ownerID)
	if err != nil || details.Task.Usage.ToolCalls != 1 {
		t.Fatalf("revision recovery duplicated the completed tool call: calls=%d err=%v", details.Task.Usage.ToolCalls, err)
	}
	var routed, delivered int
	if err := f.store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM conversation_message_routes WHERE message_id=ANY($1::text[]) AND state='routed'),
 (SELECT count(*) FROM conversation_messages WHERE id=ANY($1::text[]) AND delivery_status='applied')`, changes).Scan(&routed, &delivered); err != nil || routed != len(changes) || delivered != len(changes) {
		t.Fatalf("checkpoint did not acknowledge all revision messages: routed=%d delivered=%d err=%v", routed, delivered, err)
	}
}
