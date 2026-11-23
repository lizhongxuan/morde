package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

// The local DSH process is killed only after a depth-2 worker has committed a
// test effect to PostgreSQL and checkpointed its tool result. Recovery must
// restore the same parent/session/spawn keys and reconcile, not execute again.
func TestCloudDSHNestedCompletedEffectIsNotRepeatedAfterRestart(t *testing.T) {
	runNestedRecoveryScenario(t, false)
}

func TestCloudDSHNestedCancelledBackgroundWorkerDoesNotRestart(t *testing.T) {
	runNestedRecoveryScenario(t, true)
}

func runNestedRecoveryScenario(t *testing.T, cancelBackground bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE nested_test_effects (id bigserial PRIMARY KEY, task_id text NOT NULL, call_id text NOT NULL, result text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify session: %v", err)
	}
	schedules, err := app.NewScheduleService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	scheduled, err := schedules.Create(ctx, principal, app.CreateScheduleCommand{
		ModelID: "p1234567", Objective: "Coordinate a worker and report the committed local effect.",
		Timing:        schedule.Timing{Kind: schedule.TimingOnce, RunAt: dueAt.Format(time.RFC3339), TimeZone: "UTC"},
		Budget:        task.Budget{ActiveSeconds: 120, ToolCalls: 16, ModelTokens: 50000},
		MisfirePolicy: schedule.MisfireRunOnce, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
	}, "nested-recovery-e2e")
	if err != nil {
		t.Fatal(err)
	}
	processed, err := schedules.ProcessDue(ctx, time.Now().UTC(), 10)
	if err != nil || processed != 1 {
		t.Fatalf("queue scheduled Task: count=%d err=%v", processed, err)
	}
	runs, err := schedules.Runs(ctx, principal, scheduled.ID, app.PageRequest{Limit: 10})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].TaskID == nil {
		t.Fatalf("scheduled Task missing: %#v err=%v", runs.Items, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "nested-recovery-worker-one", 5*time.Minute)
	if err != nil || !found || claimed.ID != *runs.Items[0].TaskID {
		t.Fatalf("claim initial Task: found=%t err=%v", found, err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	const supervisorID = "nested-recovery-supervisor"
	supervisorAuth, err := app.NewAgentStopAuthenticator(supervisorID, bytes.Repeat([]byte{0x74}, 32))
	if err != nil {
		t.Fatal(err)
	}
	agentAuth, err := app.NewAgentGuardianStopAuthenticator(supervisorID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentAuth, taskAuth)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	observed := &f01ObservedCheckpoints{AgentCheckpointService: checkpoints}
	capture := &f07ObservedCheckpoints{f01ObservedCheckpoints: observed, bySession: make(map[string][]f07CheckpointEntry)}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	childObserver := &f01ObservedChildSessions{CloudDSHChildSessions: children}
	node, script := f01NodeRuntimePaths(t)
	accountScript := filepath.Join(filepath.Dir(script), "bin-account.mjs")
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	guardian := &f01SignedNodeTaskGuardian{key: privateKey}
	model := &nestedRecoveryModel{pool: pool, turns: make(map[string]int), effectSeen: make(chan struct{}), interrupt: make(chan struct{}),
		background: cancelBackground, coordinatorBlocked: make(chan struct{})}
	tools := &nestedRecoveryTools{pool: pool}
	makeSupervisor := func() *nestedRecoverySupervisor {
		return &nestedRecoverySupervisor{f01RecoveringNodeAccountSupervisor: &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey}},
			input: &nestedRecoveryInputCapture{}}
	}
	makeManager := func(supervisor CloudDSHAccountRuntimeSupervisor, lease string) *CloudDSHAccountRuntimeManager {
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: stopVerifier, TaskStopper: instances, LeaseOwner: lease, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	makeExecutor := func(manager *CloudDSHAccountRuntimeManager) *CloudDSHExecutor {
		executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
			ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: supervisorID,
			AccountRuntimes: manager, Instances: instances, Checkpoints: capture, Models: model, Tools: tools, Children: childObserver,
			Completion: &parentSiblingCancelCompletion{store: store},
		})
		if err != nil {
			t.Fatal(err)
		}
		return executor
	}
	firstSupervisor := makeSupervisor()
	firstManager := makeManager(firstSupervisor, "nested-recovery-runtime-one")
	defer nestedRecoveryStopSupervisor(firstSupervisor.f01RecoveringNodeAccountSupervisor)
	firstDone := make(chan error, 1)
	go func() { firstDone <- makeExecutor(firstManager).ExecuteTask(ctx, claimed) }()
	select {
	case <-model.effectSeen:
	case runErr := <-firstDone:
		t.Fatalf("first run ended before the effect was confirmed: %v; calls=%v", runErr, model.snapshot())
	case <-ctx.Done():
		t.Fatalf("worker did not confirm effect: %v; calls=%v", ctx.Err(), model.snapshot())
	}
	if cancelBackground {
		select {
		case <-model.coordinatorBlocked:
		case runErr := <-firstDone:
			t.Fatalf("coordinator stopped before cancellation: %v; calls=%v", runErr, model.snapshot())
		case <-ctx.Done():
			t.Fatalf("coordinator did not persist the background handle: %v", ctx.Err())
		}
	}
	initial, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || initial.Task.AgentSessionID == "" {
		t.Fatalf("root before restart: %#v err=%v", initial.Task, err)
	}
	rootID := initial.Task.AgentSessionID
	coordinators, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, rootID)
	if err != nil || len(coordinators) != 1 || coordinators[0].SpawnKey != "recovery-coordinator" || coordinators[0].Step.Status != task.StepRunning {
		t.Fatalf("coordinator before restart: %#v err=%v", coordinators, err)
	}
	workers, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinators[0].SessionID)
	if err != nil || len(workers) != 1 || workers[0].SpawnKey != "recovery-worker" || workers[0].Step.Status != task.StepRunning {
		t.Fatalf("worker before restart: %#v err=%v", workers, err)
	}
	workerID := workers[0].SessionID
	history, err := capture.rootHistory(workerID, workers[0].CheckpointThrough)
	if err != nil {
		t.Fatalf("read confirmed worker checkpoint: %v", err)
	}
	effectCall := history.calls["recovery-effect"]
	if effectCall == nil || !effectCall.callSeen || !effectCall.resultSeen || effectCall.resultText != nestedRecoveryEffectResult {
		t.Fatalf("worker effect result was not checkpointed before crash: %#v", effectCall)
	}
	var effects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM nested_test_effects WHERE task_id=$1`, claimed.ID).Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("expected one committed effect before restart: count=%d err=%v", effects, err)
	}
	if cancelBackground {
		cancelled, replayed, err := children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{
			Fence: f07AgentFence(initial.Task, supervisorID), ActorSessionID: coordinators[0].SessionID, SessionID: workerID, CallID: "nested-recovery-cancel",
		})
		if err != nil || replayed || !cancelled.CancelRequested || cancelled.Step.Status != task.StepRunning {
			t.Fatalf("background cancellation was not durably requested while running: %#v replayed=%t err=%v", cancelled, replayed, err)
		}
		persisted, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinators[0].SessionID)
		if err != nil || len(persisted) != 1 || !persisted[0].Background || !persisted[0].CancelRequested || persisted[0].Step.Status != task.StepRunning {
			t.Fatalf("cancelled worker was not running before crash: %#v err=%v", persisted, err)
		}
	}
	firstManager.mu.Lock()
	runtimeSession := firstManager.sessions[claimed.ConversationID]
	firstManager.mu.Unlock()
	if runtimeSession == nil {
		t.Fatal("first account runtime session is missing")
	}
	runtimeSession.mu.Lock()
	var taskPID int
	for fence, running := range runtimeSession.tasks {
		if fence.TaskID != claimed.ID {
			continue
		}
		running.mu.Lock()
		if running.guard != nil {
			taskPID = running.guard.ContainerPID
		}
		running.mu.Unlock()
	}
	runtimeSession.mu.Unlock()
	groupID, err := syscall.Getpgid(taskPID)
	if err != nil || taskPID < 2 || groupID != taskPID {
		t.Fatalf("test DSH task process is not independently isolated: pid=%d group=%d err=%v", taskPID, groupID, err)
	}
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill local DSH task process group: %v", err)
	}
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	for {
		err := syscall.Kill(-groupID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatalf("check DSH process group exit: %v", err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("local DSH task process group survived SIGKILL")
		}
	}
	ticker.Stop()
	deadline.Stop()
	oldProcess := firstSupervisor.cmd
	if oldProcess == nil || oldProcess.Process == nil {
		t.Fatal("real account Runtime process was not running")
	}
	if err := oldProcess.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	close(model.interrupt)
	select {
	case runErr := <-firstDone:
		if runErr == nil {
			t.Fatal("first run survived process interruption")
		}
	case <-ctx.Done():
		t.Fatalf("interrupted Executor did not stop: %v", ctx.Err())
	}
	waitOld := make(chan error, 1)
	go func() { waitOld <- oldProcess.Wait() }()
	select {
	case waitErr := <-waitOld:
		if waitErr == nil || oldProcess.ProcessState == nil || oldProcess.ProcessState.ExitCode() != -1 {
			t.Fatalf("old process did not exit from kill: %v", waitErr)
		}
		firstSupervisor.cmd = nil
	case <-ctx.Done():
		t.Fatalf("old process did not exit: %v", ctx.Err())
	}
	stopped, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	fence := runtimeSession.runtime.Fence
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4`, fence.ServiceInstanceID, owner.ID, fence.RuntimeID, fence.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND lease_owner=$2 AND lease_version=$3`, claimed.ID, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "nested-recovery-worker-two", 5*time.Minute)
	if err != nil || !found || recovered.ID != claimed.ID || recovered.AgentSessionID != rootID || recovered.CheckpointThroughSequence != stopped.Task.CheckpointThroughSequence {
		t.Fatalf("reclaim persisted root: found=%t task=%#v err=%v", found, recovered, err)
	}
	secondSupervisor := makeSupervisor()
	defer nestedRecoveryStopSupervisor(secondSupervisor.f01RecoveringNodeAccountSupervisor)
	secondManager := makeManager(secondSupervisor, "nested-recovery-runtime-two")
	runErr := makeExecutor(secondManager).ExecuteTask(ctx, recovered)
	if cancelBackground {
		persisted, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinators[0].SessionID)
		if err != nil || len(persisted) != 1 || persisted[0].SessionID != workerID || persisted[0].SpawnKey != workers[0].SpawnKey || !persisted[0].CancelRequested || persisted[0].Step.Status != task.StepUnknown {
			t.Fatalf("cancelled nested worker was not retired as unknown after replay: %#v err=%v runErr=%v creates=%v calls=%v", persisted, err, runErr, childObserver.snapshot(), model.snapshot())
		}
		replayed := false
		for _, entry := range childObserver.snapshot() {
			if strings.Contains(entry, "session="+workerID) && strings.Contains(entry, "replayed=true") {
				replayed = true
			}
		}
		unknownFrame := secondSupervisor.input.sawUnknownChild(workerID, coordinators[0].SessionID)
		if !replayed || !unknownFrame || runErr == nil || model.recoveredWorker.Load() || tools.executed.Load() != 1 {
			t.Fatalf("cancelled worker was not replayed as unknown or resumed: replayed=%t unknown_frame=%t runErr=%v resumed=%t executions=%d creates=%v calls=%v", replayed, unknownFrame, runErr, model.recoveredWorker.Load(), tools.executed.Load(), childObserver.snapshot(), model.snapshot())
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM nested_test_effects WHERE task_id=$1`, claimed.ID).Scan(&effects); err != nil || effects != 1 {
			t.Fatalf("cancelled worker repeated effect: count=%d err=%v", effects, err)
		}
		return
	}
	if runErr != nil {
		t.Fatalf("recovered nested Task did not finish: %v; calls=%v; checkpoints=%v", runErr, model.snapshot(), observed.snapshot())
	}
	completed, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != nestedRecoveryRootResult || completed.Task.AgentSessionID != rootID || completed.Task.AgentGeneration <= initial.Task.AgentGeneration {
		t.Fatalf("recovered root result: %#v err=%v", completed.Task, err)
	}
	newCoordinators, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, rootID)
	if err != nil || len(newCoordinators) != 1 || newCoordinators[0].SessionID != coordinators[0].SessionID ||
		newCoordinators[0].SpawnKey != coordinators[0].SpawnKey || newCoordinators[0].Step.Status != task.StepSucceeded {
		t.Fatalf("coordinator was not restored by parent and spawn key: %#v err=%v", newCoordinators, err)
	}
	newWorkers, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinators[0].SessionID)
	if err != nil || len(newWorkers) != 1 || newWorkers[0].SessionID != workerID || newWorkers[0].SpawnKey != workers[0].SpawnKey || newWorkers[0].Step.Status != task.StepSucceeded {
		t.Fatalf("worker was not restored by parent and spawn key: %#v err=%v", newWorkers, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM nested_test_effects WHERE task_id=$1`, claimed.ID).Scan(&effects); err != nil || effects != 1 || tools.executed.Load() != 1 || tools.reconciled.Load() < 1 {
		t.Fatalf("confirmed effect was repeated or not reconciled: rows=%d executions=%d reconciliations=%d err=%v", effects, tools.executed.Load(), tools.reconciled.Load(), err)
	}
}

// Capture the actual worker-to-DSH child-session response while still writing
// to the real local subprocess, rather than inferring it from stored status.
type nestedRecoverySupervisor struct {
	*f01RecoveringNodeAccountSupervisor
	input *nestedRecoveryInputCapture
}

func (s *nestedRecoverySupervisor) StartAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, nonce []byte, expires time.Time) (CloudDSHAccountRuntimeProcess, error) {
	process, err := s.f01RecoveringNodeAccountSupervisor.StartAccountRuntime(ctx, fence, nonce, expires)
	if err == nil {
		s.input.target = process.Input
		process.Input = s.input
	}
	return process, err
}

type nestedRecoveryInputCapture struct {
	target io.WriteCloser
	mu     sync.Mutex
	buffer []byte
	frames []json.RawMessage
}

func (c *nestedRecoveryInputCapture) Write(data []byte) (int, error) {
	n, err := c.target.Write(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = append(c.buffer, data[:n]...)
	for {
		index := bytes.IndexByte(c.buffer, '\n')
		if index < 0 {
			break
		}
		c.frames = append(c.frames, append([]byte(nil), c.buffer[:index]...))
		c.buffer = c.buffer[index+1:]
	}
	return n, err
}
func (c *nestedRecoveryInputCapture) Close() error { return c.target.Close() }
func (c *nestedRecoveryInputCapture) sawUnknownChild(sessionID, parentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, raw := range c.frames {
		var frame struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			SessionID string `json:"session_id"`
			ParentID  string `json:"parent_session_id"`
			Status    string `json:"step_status"`
		}
		if json.Unmarshal(raw, &frame) == nil && frame.Type == "child_session" && frame.CallID == "recovery-worker" && frame.SessionID == sessionID && frame.ParentID == parentID && frame.Status == "unknown" {
			return true
		}
	}
	return false
}

func nestedRecoveryStopSupervisor(supervisor *f01RecoveringNodeAccountSupervisor) {
	if command := supervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
		_ = command.Process.Kill()
		_ = command.Wait()
	}
}

const nestedRecoveryEffectResult = "one local effect committed"
const nestedRecoveryRootResult = "Verified: one local effect committed."

type nestedRecoveryTools struct {
	pool                 *pgxpool.Pool
	executed, reconciled atomic.Int32
}

func (*nestedRecoveryTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{{ModelTool: app.ModelTool{Name: "nested_effect_write", Description: "Commit one local PostgreSQL test effect.", Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)}}}, nil
}
func (t *nestedRecoveryTools) Execute(ctx context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	t.executed.Add(1)
	if call.Name != "nested_effect_write" {
		return app.AgentToolResult{}, fmt.Errorf("unexpected tool %q", call.Name)
	}
	if _, err := t.pool.Exec(ctx, `INSERT INTO nested_test_effects(task_id, call_id, result) VALUES($1,$2,$3)`, claimed.ID, call.CallID, nestedRecoveryEffectResult); err != nil {
		return app.AgentToolResult{}, err
	}
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: nestedRecoveryEffectResult}, nil
}
func (t *nestedRecoveryTools) Reconcile(ctx context.Context, claimed task.Task, call app.ModelToolCall, recorded *string) (app.AgentToolResult, error) {
	t.reconciled.Add(1)
	var output string
	if err := t.pool.QueryRow(ctx, `SELECT result FROM nested_test_effects WHERE task_id=$1 AND call_id=$2`, claimed.ID, call.CallID).Scan(&output); err != nil {
		return app.AgentToolResult{}, err
	}
	if recorded != nil && *recorded != output {
		return app.AgentToolResult{}, errors.New("checkpoint and local effect differ")
	}
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: output}, nil
}
func (*nestedRecoveryTools) PauseRecovery(context.Context, task.Task) error { return nil }
func (*nestedRecoveryTools) InitialContext(context.Context, task.Task) (string, error) {
	return "Synthetic context for a local scheduled Task.", nil
}

type nestedRecoveryModel struct {
	pool               *pgxpool.Pool
	mu                 sync.Mutex
	turns              map[string]int
	calls              []string
	effectSeen         chan struct{}
	interrupt          chan struct{}
	once               sync.Once
	background         bool
	coordinatorBlocked chan struct{}
	coordinatorOnce    sync.Once
	recoveredWorker    atomic.Bool
}

func (m *nestedRecoveryModel) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}
func (m *nestedRecoveryModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model Task is missing")
	}
	var parent, title string
	if err := m.pool.QueryRow(ctx, `SELECT COALESCE(s.parent_session_id,''),COALESCE(st.title,'') FROM task_agent_sessions s LEFT JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).Scan(&parent, &title); err != nil {
		return app.ModelResponse{}, err
	}
	m.mu.Lock()
	m.turns[call.Task.AgentSessionID]++
	turn := m.turns[call.Task.AgentSessionID]
	m.calls = append(m.calls, fmt.Sprintf("session=%s parent=%s title=%s turn=%d generation=%d", call.Task.AgentSessionID, parent, title, turn, call.Task.AgentGeneration))
	m.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	response := func(calls ...app.ModelToolCall) app.ModelResponse {
		return app.ModelResponse{ToolCalls: calls, StopReason: "tool_calls", Usage: usage}
	}
	if parent == "" {
		if turn == 1 {
			return response(app.ModelToolCall{CallID: "recovery-coordinator", Name: cloudDSHDelegateToolName, Arguments: `{"title":"recovery-coordinator","objective":"Run one worker and report the verified result.","tool_names":["nested_effect_write"]}`}), nil
		}
		output, ok := f01ToolResult(call.RawRequest, "recovery-coordinator")
		if !ok || !strings.Contains(output, nestedRecoveryEffectResult) {
			return app.ModelResponse{}, fmt.Errorf("root did not receive coordinator result: %q", output)
		}
		return app.ModelResponse{Text: nestedRecoveryRootResult, StopReason: "stop", Usage: usage}, nil
	}
	if parent == call.Task.AgentRootSessionID {
		if turn == 1 {
			arguments := `{"title":"recovery-worker","objective":"Commit one local effect and report it.","tool_names":["nested_effect_write"]}`
			if m.background {
				arguments = `{"title":"recovery-worker","objective":"Commit one local effect and report it.","tool_names":["nested_effect_write"],"run_in_background":true}`
			}
			return response(app.ModelToolCall{CallID: "recovery-worker", Name: cloudDSHDelegateToolName, Arguments: arguments}), nil
		}
		if m.background && call.Task.AgentGeneration == 1 {
			if _, err := nestedBackgroundHandle(call.RawRequest, "recovery-worker"); err != nil {
				return app.ModelResponse{}, err
			}
			m.coordinatorOnce.Do(func() { close(m.coordinatorBlocked) })
			select {
			case <-m.interrupt:
				return app.ModelResponse{}, errors.New("injected runtime interruption")
			case <-ctx.Done():
				return app.ModelResponse{}, ctx.Err()
			}
		}
		output, ok := f01ToolResult(call.RawRequest, "recovery-worker")
		if !ok || !strings.Contains(output, nestedRecoveryEffectResult) {
			return app.ModelResponse{}, fmt.Errorf("coordinator did not receive worker result: %q", output)
		}
		return app.ModelResponse{Text: "Coordinator verified: " + nestedRecoveryEffectResult, StopReason: "stop", Usage: usage}, nil
	}
	if title != "recovery-worker" {
		return app.ModelResponse{}, fmt.Errorf("unexpected nested session %q", title)
	}
	if call.Task.AgentGeneration > 1 {
		m.recoveredWorker.Store(true)
	}
	output, ok := f01ToolResult(call.RawRequest, "recovery-effect")
	if !ok {
		if turn != 1 {
			return app.ModelResponse{}, errors.New("restored worker lost its confirmed result")
		}
		return response(app.ModelToolCall{CallID: "recovery-effect", Name: "nested_effect_write", Arguments: `{}`}), nil
	}
	if output != nestedRecoveryEffectResult {
		return app.ModelResponse{}, fmt.Errorf("worker received invalid effect result %q", output)
	}
	if call.Task.AgentGeneration == 1 {
		m.once.Do(func() { close(m.effectSeen) })
		select {
		case <-m.interrupt:
			return app.ModelResponse{}, errors.New("injected runtime interruption")
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		}
	}
	return app.ModelResponse{Text: nestedRecoveryEffectResult, StopReason: "stop", Usage: usage}, nil
}
