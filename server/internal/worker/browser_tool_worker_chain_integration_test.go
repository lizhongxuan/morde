package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const (
	browserChainOpenCallID     = "morde-browser-open-call-001"
	browserChainSnapshotCallID = "morde-browser-snapshot-call-001"
	browserChainOpenOutput     = `{"target_id":"target_1234567","url":"https://example.test/","title":"Synthetic page"}`
	browserChainSnapshotOutput = `{"target_id":"target_1234567","url":"https://example.test/","title":"Synthetic page","text":"Synthetic page content."}`
	browserChainAnswer         = "I opened the page and read its content."
)

// TestDSHBrowserToolCompletesThroughWorkerAndRecordsOperationAndStep exercises
// a queued Task through DSH, two browser tools, and a synthetic model. No
// provider, live browser, Host, or external service is contacted.
func TestDSHBrowserToolCompletesThroughWorkerAndRecordsOperationAndStep(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	openTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_open_tab", Description: "Open a page in this Task's isolated browser.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
	}}
	snapshotTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_snapshot", Description: "Read the current page in this Task's isolated browser.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string"}},"required":["target_id"],"additionalProperties":false}`),
	}}
	broker := &browserChainBrokerStub{
		definitions: []app.AgentToolDefinition{openTool, snapshotTool},
		outputs: map[string]string{
			openTool.Name:     browserChainOpenOutput,
			snapshotTool.Name: browserChainSnapshotOutput,
		},
	}
	executor.options.Tools = broker
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Completion = completion
	model := &browserChainSyntheticModel{taskID: value.ID}
	models.complete = model.Complete

	workerID := "morde-browser-tool-worker"
	value.Lease.Owner = workerID
	claimed := false
	leaseStore := &taskLeaseStoreStub{
		claim: func(_ context.Context, gotWorkerID string, _ time.Duration) (task.Task, bool, error) {
			if gotWorkerID != workerID {
				return task.Task{}, false, errors.New("unexpected worker identity")
			}
			if claimed {
				return task.Task{}, false, nil
			}
			claimed = true
			return value, true, nil
		},
		renew: func(_ context.Context, taskID, gotWorkerID string, version int64, lease time.Duration) (task.Task, error) {
			if taskID != value.ID || gotWorkerID != workerID || version != value.Lease.Version {
				return task.Task{}, errors.New("unexpected Task lease renewal")
			}
			expiresAt := time.Now().Add(lease)
			value.Lease.ExpiresAt = &expiresAt
			return value, nil
		},
	}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: executor, WorkerID: workerID, LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()

	select {
	case <-completion.done:
	case <-ctx.Done():
		t.Fatalf("Task did not complete through DSH: %v", ctx.Err())
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after the synthetic Task completed")
	}

	if len(models.calls) != 3 {
		t.Fatalf("DSH model rounds = %d; want open-tab, snapshot, and final-answer rounds", len(models.calls))
	}
	if !model.openToolOffered || !model.snapshotToolOffered || !model.openResultSeen || !model.snapshotResultSeen {
		t.Fatalf("browser flow observations = %#v; want both tools and each exact result returned to the next model round", model)
	}
	if len(broker.calls) != 2 {
		t.Fatalf("browser Broker calls = %#v; want open-tab then snapshot", broker.calls)
	}
	wantCalls := []struct {
		id, name, args string
	}{
		{browserChainOpenCallID, openTool.Name, `{"url":"https://example.test/"}`},
		{browserChainSnapshotCallID, snapshotTool.Name, `{"target_id":"target_1234567"}`},
	}
	for i, want := range wantCalls {
		got := broker.calls[i]
		if got.TaskID != value.ID || got.OwnerID != value.OwnerID || got.InitiatorID != value.InitiatorID ||
			got.CallID != want.id || got.ToolName != want.name || got.Arguments != want.args {
			t.Fatalf("browser Broker call %d = %#v; want Task/owner/initiator-bound %s", i, got, want.name)
		}
		if got.Operation.TaskID != value.ID || got.Operation.Key != app.AgentOperationKey(want.id) ||
			got.Operation.Status != task.OperationSucceeded || got.Operation.Result != broker.outputs[want.name] {
			t.Fatalf("browser operation %d = %#v; want exact Task-bound tool result", i, got.Operation)
		}
		if got.Step.TaskID != value.ID || got.Step.Sequence != int64(i+1) || got.Step.Kind != "tool" ||
			got.Step.Status != task.StepSucceeded || got.Step.Title != want.name || got.Step.Summary != broker.outputs[want.name] {
			t.Fatalf("browser work-chain step %d = %#v; want ordered successful tool step", i, got.Step)
		}
	}
	if completion.completion.Result != browserChainAnswer || completion.task.ID != value.ID ||
		completion.completion.AgentSessionID == "" || completion.completion.CheckpointThroughSequence < 0 {
		t.Fatalf("completed DSH result = %#v for Task %#v; want final synthetic answer and checkpoint", completion.completion, completion.task)
	}
}

const (
	browserHandoffIDOpenCall     = "morde-browser-handoff-open-001"
	browserHandoffIDActivateCall = "morde-browser-handoff-activate-001"
	browserHandoffIDAskCall      = "morde-browser-handoff-ask-001"
	browserHandoffIDSnapshotCall = "morde-browser-handoff-snapshot-001"
	browserHandoffOpenOutput     = `{"target_id":"target_1234567","url":"https://example.test/login","title":"Sign in"}`
	browserHandoffActiveOutput   = `{"target_id":"target_1234567","active":true}`
	browserHandoffSnapshotOutput = `{"target_id":"target_1234567","url":"https://example.test/account","title":"Account","text":"Signed-in account page."}`
	browserHandoffQuestion       = "Please sign in in the browser, then confirm when the page is ready."
	browserHandoffAnswer         = "I have signed in. Continue."
	browserHandoffFinalAnswer    = "You are signed in. I read the account page successfully."
)

// TestDSHBrowserUserHandoffResumesSameTaskThroughWorker exercises a real local
// DSH process across a clarification pause and recovery. Browser operations,
// user confirmation, model responses, and Task lease claims are synthetic; no
// live browser, desktop input, provider, or external service is contacted.
func TestDSHBrowserUserHandoffResumesSameTaskThroughWorker(t *testing.T) {
	executor, supervisor, instanceStore, checkpoints, models, value := cloudDSHFixture(t)
	value.Budget.ToolCalls = 8
	openTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_open_tab", Description: "Open a page in this Task's isolated browser.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
	}}
	handoffTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_prepare_user_handoff", Description: "Activate this Task's browser for user sign-in.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string"}},"required":["target_id"],"additionalProperties":false}`),
	}}
	snapshotTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_snapshot", Description: "Read the current page in this Task's isolated browser.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string"}},"required":["target_id"],"additionalProperties":false}`),
	}}
	askUserTool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "ask_user", Description: "Ask the user for one missing detail. Use alone.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","minLength":1,"maxLength":4096}},"required":["question"],"additionalProperties":false}`),
	}}
	broker := &browserHandoffBrokerStub{
		definitions: []app.AgentToolDefinition{openTool, handoffTool, snapshotTool, askUserTool},
		waiting:     make(chan struct{}, 1),
		answer:      browserHandoffAnswer,
	}
	executor.options.Tools = broker
	checkpointStore := &browserHandoffCheckpointStore{CloudDSHCheckpoints: checkpoints}
	executor.options.Checkpoints = checkpointStore
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Completion = completion
	model := &browserHandoffSyntheticModel{taskID: value.ID, ownerID: value.OwnerID}
	models.complete = model.Complete

	workerID := "morde-browser-handoff-worker"
	value.Lease.Owner = workerID
	resumeReady := make(chan struct{})
	var claimMu sync.Mutex
	firstClaimed, resumedClaimed := false, false
	var resumed task.Task
	leaseStore := &taskLeaseStoreStub{
		claimWithOwnerLimit: func(ctx context.Context, gotWorkerID string, _ time.Duration, _ int) (task.Task, bool, error) {
			if gotWorkerID != workerID {
				return task.Task{}, false, errors.New("unexpected worker identity")
			}
			claimMu.Lock()
			if !firstClaimed {
				firstClaimed = true
				claimMu.Unlock()
				return value, true, nil
			}
			if resumed.ID == "" {
				claimMu.Unlock()
				select {
				case <-resumeReady:
				case <-ctx.Done():
					return task.Task{}, false, nil
				}
				claimMu.Lock()
			}
			if resumedClaimed {
				claimMu.Unlock()
				return task.Task{}, false, nil
			}
			resumedClaimed = true
			claimed := resumed
			claimMu.Unlock()
			return claimed, true, nil
		},
		renew: func(_ context.Context, taskID, gotWorkerID string, version int64, lease time.Duration) (task.Task, error) {
			claimMu.Lock()
			defer claimMu.Unlock()
			if taskID != value.ID || gotWorkerID != workerID {
				return task.Task{}, errors.New("unexpected Task lease renewal")
			}
			current := value
			if resumedClaimed {
				current = resumed
			}
			if version != current.Lease.Version {
				return task.Task{}, errors.New("unexpected Task lease version")
			}
			expiresAt := time.Now().Add(lease)
			current.Lease.ExpiresAt = &expiresAt
			if resumedClaimed {
				resumed = current
			}
			return current, nil
		},
	}
	executionErrs := make(chan error, 4)
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: &browserHandoffObservedExecutor{inner: executor, errors: executionErrs},
		WorkerID: workerID, LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()

	select {
	case <-broker.waiting:
	case <-ctx.Done():
		t.Fatalf("DSH did not pause for user confirmation: %v", ctx.Err())
	}
	pausedCalls := broker.snapshotCalls()
	if len(pausedCalls) != 3 || pausedCalls[0].ToolName != openTool.Name || pausedCalls[1].ToolName != handoffTool.Name ||
		pausedCalls[2].ToolName != askUserTool.Name || pausedCalls[2].Mode != "execute_waiting_user" ||
		pausedCalls[2].Status != app.AgentToolNeedsUser || pausedCalls[2].WaitingReason != "clarification_required" {
		t.Fatalf("tool calls before user confirmation = %#v; want open, activate handoff, then ask_user pause", pausedCalls)
	}
	if !model.snapshotObservations().handoffResultSeen {
		t.Fatal("model did not receive the activated browser handoff result before asking the user")
	}
	checkpoint, ok := checkpointStore.latest(value.ID)
	if !ok || checkpoint.Through < 0 || len(checkpoint.Digest) != sha256.Size {
		t.Fatalf("paused DSH checkpoint = %#v, present=%t; want a resumable checkpoint", checkpoint, ok)
	}
	claimMu.Lock()
	resumed = value
	resumed.Status = task.StatusRunning
	resumed.Lease.Version++
	expiresAt := time.Now().Add(time.Minute)
	resumed.Lease.ExpiresAt = &expiresAt
	resumed.AgentInstanceID = checkpoint.Fence.AgentInstanceID
	resumed.AgentGeneration = checkpoint.Fence.AgentGeneration
	resumed.AgentSessionID = checkpoint.Fence.AgentSessionID
	resumed.AgentEngineVersion = checkpoint.Fence.EngineVersion
	resumed.AgentProfileVersion = checkpoint.Fence.ProfileVersion
	resumed.CheckpointThroughSequence = checkpoint.Through
	resumed.CheckpointDigest = append([]byte(nil), checkpoint.Digest...)
	claimMu.Unlock()
	broker.confirmUser()
	close(resumeReady)

	select {
	case <-completion.done:
	case <-ctx.Done():
		claimCount, renewCount := leaseStore.counts()
		checkpoint, checkpointOK := checkpointStore.latest(value.ID)
		readCount, checkpointCount := checkpointStore.counts()
		var errorSummaries []string
		for {
			select {
			case executionErr := <-executionErrs:
				errorSummaries = append(errorSummaries, executionErr.Error())
			default:
				goto errorsCollected
			}
		}
	errorsCollected:
		instanceStore.mu.Lock()
		stoppedCount := instanceStore.stopped
		instanceStore.mu.Unlock()
		supervisor.mu.Lock()
		processStarted := supervisor.cmd != nil
		supervisor.mu.Unlock()
		t.Fatalf("resumed Task did not complete through DSH: %v; calls=%#v model=%#v claims=%d renewals=%d checkpoint_session=%s checkpoint_through=%d present=%t checkpoint_reads=%d checkpoint_writes=%d stopped_instances=%d process_started=%t execution_errors=%v",
			ctx.Err(), broker.snapshotCalls(), model.snapshotObservations(), claimCount, renewCount, checkpoint.Fence.AgentSessionID, checkpoint.Through,
			checkpointOK, readCount, checkpointCount, stoppedCount, processStarted, errorSummaries)
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after the synthetic Task completed")
	}

	calls := broker.snapshotCalls()
	wantCallIDs := []string{browserHandoffIDOpenCall, browserHandoffIDActivateCall, browserHandoffIDAskCall,
		browserHandoffIDOpenCall, browserHandoffIDActivateCall, browserHandoffIDAskCall, browserHandoffIDSnapshotCall}
	wantTools := []string{openTool.Name, handoffTool.Name, askUserTool.Name,
		openTool.Name, handoffTool.Name, askUserTool.Name, snapshotTool.Name}
	wantModes := []string{"execute", "execute", "execute_waiting_user",
		"reconcile_recorded", "reconcile_recorded", "reconcile_answered", "execute"}
	if len(calls) != len(wantTools) {
		t.Fatalf("Broker call sequence = %#v; want %d calls across the pause and resume", calls, len(wantTools))
	}
	for index, got := range calls {
		if got.TaskID != value.ID || got.OwnerID != value.OwnerID || got.InitiatorID != value.InitiatorID ||
			got.CallID != wantCallIDs[index] || got.ToolName != wantTools[index] || got.Mode != wantModes[index] {
			t.Fatalf("Broker call %d = %#v; want same Task/owner/initiator, call ID, tool, and phase", index, got)
		}
	}
	if calls[1].Status != app.AgentToolSucceeded || calls[1].Output != browserHandoffActiveOutput ||
		calls[2].WaitingReason != "clarification_required" || calls[5].Status != app.AgentToolSucceeded ||
		calls[5].Output != browserHandoffAnswer {
		t.Fatalf("handoff/clarification results = %#v, %#v, %#v; want activation, wait, and user answer", calls[1], calls[2], calls[5])
	}
	wantArguments := []string{
		`{"url":"https://example.test/login"}`,
		`{"target_id":"target_1234567"}`,
		`{"question":"Please sign in in the browser, then confirm when the page is ready."}`,
		`{"url":"https://example.test/login"}`,
		`{"target_id":"target_1234567"}`,
		`{"question":"Please sign in in the browser, then confirm when the page is ready."}`,
		`{"target_id":"target_1234567"}`,
	}
	for index, got := range calls {
		if got.Arguments != wantArguments[index] {
			t.Fatalf("Broker call %d arguments = %q; want %q", index, got.Arguments, wantArguments[index])
		}
	}
	if len(broker.operations) != 3 {
		t.Fatalf("browser operation ledger = %#v; want open, handoff, and snapshot only", broker.operations)
	}
	wantOperationCalls := []string{browserHandoffIDOpenCall, browserHandoffIDActivateCall, browserHandoffIDSnapshotCall}
	wantStepTitles := []string{openTool.Name, handoffTool.Name, "DSH model asked for clarification", "The Agent needs one detail", snapshotTool.Name}
	wantStepStatuses := []task.StepStatus{task.StepSucceeded, task.StepSucceeded, task.StepSucceeded, task.StepWaitingUser, task.StepSucceeded}
	steps := broker.snapshotSteps()
	if len(steps) != len(wantStepTitles) {
		t.Fatalf("work-chain steps = %#v; want browser steps and user clarification in order", steps)
	}
	for index, got := range steps {
		if got.TaskID != value.ID || got.Sequence != int64(index+1) || got.Status != wantStepStatuses[index] || got.Title != wantStepTitles[index] {
			t.Fatalf("work-chain step %d = %#v; want ordered Task-bound %q", index, got, wantStepTitles[index])
		}
	}
	operationIndex := 0
	for _, got := range broker.snapshotToolCalls() {
		if got.ToolName == askUserTool.Name {
			continue
		}
		wantCallID := wantOperationCalls[operationIndex]
		if got.Operation.TaskID != value.ID || got.Operation.Key != app.AgentOperationKey(wantCallID) ||
			got.Operation.Status != task.OperationSucceeded || got.Operation.Result == "" || got.Step.TaskID != value.ID {
			t.Fatalf("browser operation/step %d = operation %#v step %#v; want Task and call-ID binding", operationIndex, got.Operation, got.Step)
		}
		operationIndex++
	}
	modelState := model.snapshotObservations()
	if len(modelState.fences) != 5 || modelState.fences[0].TaskID != value.ID || modelState.fences[0].OwnerID != value.OwnerID ||
		modelState.fences[0].AgentSessionID == "" || modelState.fences[3].AgentSessionID != modelState.fences[0].AgentSessionID ||
		!modelState.confirmationSeen || !modelState.snapshotResultSeen || !modelState.finalAnswerSeen {
		t.Fatalf("model observations across Task recovery = %#v; want same Task/session, user answer, snapshot result, and final response", modelState)
	}
	if completion.task.ID != value.ID || completion.task.OwnerID != value.OwnerID || completion.completion.Result != browserHandoffFinalAnswer ||
		completion.completion.AgentSessionID != modelState.fences[0].AgentSessionID {
		t.Fatalf("completed DSH result = %#v for Task %#v; want final answer on same Task/session", completion.completion, completion.task)
	}
	if broker.recoveryPauses() != 0 {
		t.Fatalf("DSH recovery broker paused %d times during a confirmed user handoff", broker.recoveryPauses())
	}
	for _, definition := range broker.definitions {
		if strings.HasPrefix(definition.Name, "desktop_") || strings.HasPrefix(definition.Name, "computer_") {
			t.Fatalf("desktop input tool was exposed during browser handoff: %q", definition.Name)
		}
	}
	for _, call := range broker.snapshotCalls() {
		if call.ToolName == "desktop_click" || call.ToolName == "desktop_type" || call.ToolName == "desktop_keypress" || strings.Contains(call.Arguments, "target_other") {
			t.Fatalf("browser handoff unexpectedly used desktop input or another target: %#v", call)
		}
	}

	checkpointStore.mu.Lock()
	reads := append([]app.AgentCheckpointPageRequest(nil), checkpointStore.reads...)
	checkpointStore.mu.Unlock()
	if len(reads) == 0 {
		t.Fatal("resumed DSH run did not read the Task checkpoint")
	}
	for _, read := range reads {
		if read.Fence.TaskID != value.ID || read.Fence.OwnerID != value.OwnerID || read.Fence.InitiatorID != value.InitiatorID ||
			read.SessionID != modelState.fences[0].AgentSessionID {
			t.Fatalf("checkpoint read crossed the Task/owner/session fence: %#v", read)
		}
	}
}

type browserHandoffCheckpointStore struct {
	CloudDSHCheckpoints
	mu           sync.Mutex
	latestByTask map[string]app.AgentCheckpointCommand
	reads        []app.AgentCheckpointPageRequest
	writes       int
}

type browserHandoffObservedExecutor struct {
	inner  TaskExecutor
	errors chan<- error
}

func (e *browserHandoffObservedExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	err := e.inner.ExecuteTask(ctx, value)
	if err != nil {
		select {
		case e.errors <- err:
		default:
		}
	}
	return err
}

func (s *browserHandoffCheckpointStore) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	ok, err := s.CloudDSHCheckpoints.Save(ctx, command)
	if err != nil {
		return ok, err
	}
	s.mu.Lock()
	if s.latestByTask == nil {
		s.latestByTask = make(map[string]app.AgentCheckpointCommand)
	}
	copy := command
	copy.Digest = append([]byte(nil), command.Digest...)
	copy.Payload = append([]byte(nil), command.Payload...)
	s.latestByTask[command.Fence.TaskID] = copy
	s.writes++
	s.mu.Unlock()
	return ok, nil
}

func (s *browserHandoffCheckpointStore) ReadPage(ctx context.Context, request app.AgentCheckpointPageRequest) (app.AgentCheckpointPage, error) {
	page, err := s.CloudDSHCheckpoints.ReadPage(ctx, request)
	s.mu.Lock()
	s.reads = append(s.reads, request)
	s.mu.Unlock()
	return page, err
}

func (s *browserHandoffCheckpointStore) latest(taskID string) (app.AgentCheckpointCommand, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command, ok := s.latestByTask[taskID]
	return command, ok
}

func (s *browserHandoffCheckpointStore) counts() (reads, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads), s.writes
}

type browserHandoffToolCall struct {
	TaskID, OwnerID, InitiatorID string
	CallID, ToolName, Arguments  string
	Mode                         string
	Status                       app.AgentToolResultStatus
	WaitingReason, Output        string
	Operation                    task.Operation
	Step                         task.Step
}

type browserHandoffBrokerStub struct {
	mu                 sync.Mutex
	definitions        []app.AgentToolDefinition
	waiting            chan struct{}
	answer             string
	confirmed          bool
	calls              []browserHandoffToolCall
	toolCalls          []browserHandoffToolCall
	operations         []task.Operation
	steps              []task.Step
	recoveryPauseCalls int
}

func (b *browserHandoffBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *browserHandoffBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	return b.execute(claimed, call, false)
}

func (b *browserHandoffBrokerStub) Reconcile(_ context.Context, claimed task.Task, call app.ModelToolCall, checkpointOutput *string) (app.AgentToolResult, error) {
	entry := browserHandoffToolCall{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		CallID: call.CallID, ToolName: call.Name, Arguments: call.Arguments, Mode: "reconcile_recorded"}
	output := ""
	switch call.Name {
	case "browser_open_tab":
		if call.CallID != browserHandoffIDOpenCall || call.Arguments != `{"url":"https://example.test/login"}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected recovered browser_open_tab call: %#v", call)
		}
		output = browserHandoffOpenOutput
	case "browser_prepare_user_handoff":
		if call.CallID != browserHandoffIDActivateCall || call.Arguments != `{"target_id":"target_1234567"}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected recovered browser handoff call: %#v", call)
		}
		output = browserHandoffActiveOutput
	case "ask_user":
		if call.CallID != browserHandoffIDAskCall || call.Arguments != `{"question":"Please sign in in the browser, then confirm when the page is ready."}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected recovered ask_user call: %#v", call)
		}
		b.mu.Lock()
		confirmed := b.confirmed
		b.mu.Unlock()
		if !confirmed {
			return app.AgentToolResult{}, errors.New("user answer was not recorded before Task recovery")
		}
		entry.Mode = "reconcile_answered"
		output = b.answer
	default:
		return app.AgentToolResult{}, fmt.Errorf("unexpected recovered synthetic tool call %q", call.Name)
	}
	if checkpointOutput != nil && *checkpointOutput != output {
		return app.AgentToolResult{}, fmt.Errorf("checkpointed %s result differs from the synthetic operation result", call.Name)
	}
	entry.Status = app.AgentToolSucceeded
	entry.Output = output
	b.mu.Lock()
	b.calls = append(b.calls, entry)
	b.mu.Unlock()
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: output}, nil
}

func (b *browserHandoffBrokerStub) PauseRecovery(context.Context, task.Task) error {
	b.mu.Lock()
	b.recoveryPauseCalls++
	b.mu.Unlock()
	return nil
}

func (b *browserHandoffBrokerStub) recoveryPauses() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recoveryPauseCalls
}

func (b *browserHandoffBrokerStub) execute(claimed task.Task, call app.ModelToolCall, recovering bool) (app.AgentToolResult, error) {
	entry := browserHandoffToolCall{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		CallID: call.CallID, ToolName: call.Name, Arguments: call.Arguments, Mode: "execute"}
	if recovering {
		entry.Mode = "reconcile_answered"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	appendStep := func(kind string, status task.StepStatus, title, summary string) task.Step {
		step := task.Step{ID: fmt.Sprintf("s-browser-handoff-%d", len(b.steps)+1), TaskID: claimed.ID,
			Sequence: int64(len(b.steps) + 1), Kind: kind, Status: status, Title: title, Summary: summary, CreatedAt: time.Now()}
		b.steps = append(b.steps, step)
		return step
	}
	switch call.Name {
	case "browser_open_tab":
		if call.CallID != browserHandoffIDOpenCall || call.Arguments != `{"url":"https://example.test/login"}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected browser_open_tab call: %#v", call)
		}
		return b.recordBrowserSuccess(entry, browserHandoffOpenOutput, appendStep)
	case "browser_prepare_user_handoff":
		if call.CallID != browserHandoffIDActivateCall || call.Arguments != `{"target_id":"target_1234567"}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected browser handoff call: %#v", call)
		}
		return b.recordBrowserSuccess(entry, browserHandoffActiveOutput, appendStep)
	case "browser_snapshot":
		if call.CallID != browserHandoffIDSnapshotCall || call.Arguments != `{"target_id":"target_1234567"}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected browser_snapshot call: %#v", call)
		}
		return b.recordBrowserSuccess(entry, browserHandoffSnapshotOutput, appendStep)
	case "ask_user":
		if call.CallID != browserHandoffIDAskCall || call.Arguments != `{"question":"Please sign in in the browser, then confirm when the page is ready."}` {
			return app.AgentToolResult{}, fmt.Errorf("unexpected ask_user call: %#v", call)
		}
		if b.confirmed {
			return app.AgentToolResult{}, errors.New("answered clarification was executed instead of reconciled")
		}
		entry.Mode = "execute_waiting_user"
		entry.Status = app.AgentToolNeedsUser
		entry.WaitingReason = "clarification_required"
		entry.Output = browserHandoffQuestion
		entry.Step = appendStep("model", task.StepSucceeded, "DSH model asked for clarification", "The DSH model paused before continuing the task.")
		clarification := appendStep("clarification", task.StepWaitingUser, "The Agent needs one detail", browserHandoffQuestion)
		b.calls = append(b.calls, entry)
		b.steps[len(b.steps)-2] = entry.Step
		b.steps[len(b.steps)-1] = clarification
		select {
		case b.waiting <- struct{}{}:
		default:
		}
		return app.AgentToolResult{Status: app.AgentToolNeedsUser, WaitingReason: "clarification_required", Output: browserHandoffQuestion}, nil
	default:
		return app.AgentToolResult{}, fmt.Errorf("unexpected synthetic tool %q; desktop input is unavailable", call.Name)
	}
}

func (b *browserHandoffBrokerStub) recordBrowserSuccess(entry browserHandoffToolCall, output string,
	appendStep func(string, task.StepStatus, string, string) task.Step) (app.AgentToolResult, error) {
	entry.Status = app.AgentToolSucceeded
	entry.Output = output
	step := appendStep("tool", task.StepSucceeded, entry.ToolName, output)
	entry.Step = step
	entry.Operation = task.Operation{ID: "o-" + entry.CallID, TaskID: entry.TaskID,
		Key: app.AgentOperationKey(entry.CallID), Status: task.OperationSucceeded, Result: output}
	b.calls = append(b.calls, entry)
	b.toolCalls = append(b.toolCalls, entry)
	b.operations = append(b.operations, entry.Operation)
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: output}, nil
}

func (b *browserHandoffBrokerStub) confirmUser() {
	b.mu.Lock()
	b.confirmed = true
	b.mu.Unlock()
}

func (b *browserHandoffBrokerStub) snapshotCalls() []browserHandoffToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]browserHandoffToolCall(nil), b.calls...)
}

func (b *browserHandoffBrokerStub) snapshotToolCalls() []browserHandoffToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]browserHandoffToolCall(nil), b.toolCalls...)
}

func (b *browserHandoffBrokerStub) snapshotSteps() []task.Step {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]task.Step(nil), b.steps...)
}

type browserHandoffObservations struct {
	fences             []app.ModelTaskFence
	handoffResultSeen  bool
	confirmationSeen   bool
	snapshotResultSeen bool
	finalAnswerSeen    bool
}

type browserHandoffSyntheticModel struct {
	mu      sync.Mutex
	taskID  string
	ownerID string
	state   browserHandoffObservations
}

func (m *browserHandoffSyntheticModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID != m.taskID || call.Task.OwnerID != m.ownerID {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode browser handoff model request: %w", err)
	}
	toolOffered := func(name string) bool {
		for _, tool := range request.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	toolResultSeen := func(callID, exact string) bool {
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != callID {
				continue
			}
			for _, block := range message.Content {
				if block.Text == exact {
					return true
				}
			}
		}
		return false
	}
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.fences = append(m.state.fences, *call.Task)
	switch len(m.state.fences) {
	case 1:
		if !toolOffered("browser_open_tab") || !toolOffered("browser_prepare_user_handoff") || !toolOffered("browser_snapshot") || !toolOffered("ask_user") {
			return app.ModelResponse{}, errors.New("required browser and clarification tools were not offered")
		}
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "desktop_") || strings.HasPrefix(tool.Name, "computer_") {
				return app.ModelResponse{}, fmt.Errorf("desktop input tool %q was offered", tool.Name)
			}
		}
		return browserHandoffToolResponse(browserHandoffIDOpenCall, "browser_open_tab", `{"url":"https://example.test/login"}`, usage), nil
	case 2:
		if !toolResultSeen(browserHandoffIDOpenCall, browserHandoffOpenOutput) {
			return app.ModelResponse{}, errors.New("exact opened page result was not returned to the model")
		}
		return browserHandoffToolResponse(browserHandoffIDActivateCall, "browser_prepare_user_handoff", `{"target_id":"target_1234567"}`, usage), nil
	case 3:
		m.state.handoffResultSeen = toolResultSeen(browserHandoffIDActivateCall, browserHandoffActiveOutput)
		if !m.state.handoffResultSeen {
			return app.ModelResponse{}, errors.New("model did not receive the active handoff result before asking the user")
		}
		return browserHandoffToolResponse(browserHandoffIDAskCall, "ask_user", `{"question":"Please sign in in the browser, then confirm when the page is ready."}`, usage), nil
	case 4:
		m.state.confirmationSeen = toolResultSeen(browserHandoffIDAskCall, browserHandoffAnswer)
		if !m.state.confirmationSeen || call.Task.AgentSessionID == "" || call.Task.AgentSessionID != m.state.fences[0].AgentSessionID {
			return app.ModelResponse{}, errors.New("resumed model did not receive the user's confirmation in the original session")
		}
		return browserHandoffToolResponse(browserHandoffIDSnapshotCall, "browser_snapshot", `{"target_id":"target_1234567"}`, usage), nil
	case 5:
		m.state.snapshotResultSeen = toolResultSeen(browserHandoffIDSnapshotCall, browserHandoffSnapshotOutput)
		if !m.state.snapshotResultSeen {
			return app.ModelResponse{}, errors.New("exact browser snapshot was not returned to the final model round")
		}
		m.state.finalAnswerSeen = true
		return app.ModelResponse{Text: browserHandoffFinalAnswer, StopReason: "stop", Usage: usage}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic browser handoff model round %d", len(m.state.fences))
	}
}

func browserHandoffToolResponse(callID, name, arguments string, usage app.ModelUsage) app.ModelResponse {
	return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: callID, Name: name, Arguments: arguments}}, StopReason: "tool_calls", Usage: usage}
}

func (m *browserHandoffSyntheticModel) snapshotObservations() browserHandoffObservations {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	state.fences = append([]app.ModelTaskFence(nil), m.state.fences...)
	return state
}

type browserChainSyntheticModel struct {
	taskID              string
	calls               int
	openToolOffered     bool
	snapshotToolOffered bool
	openResultSeen      bool
	snapshotResultSeen  bool
}

func (m *browserChainSyntheticModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID != m.taskID {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	m.calls++
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode DSH browser request: %w", err)
	}
	toolOffered := func(name string) bool {
		for _, tool := range request.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	m.openToolOffered = m.openToolOffered || toolOffered("browser_open_tab")
	m.snapshotToolOffered = m.snapshotToolOffered || toolOffered("browser_snapshot")
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	switch m.calls {
	case 1:
		if !m.openToolOffered || !m.snapshotToolOffered {
			return app.ModelResponse{}, errors.New("isolated browser tools were not offered to the DSH model")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{
			CallID: browserChainOpenCallID, Name: "browser_open_tab", Arguments: `{"url":"https://example.test/"}`,
		}}, StopReason: "tool_calls", Usage: usage}, nil
	case 2:
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != browserChainOpenCallID {
				continue
			}
			for _, block := range message.Content {
				if block.Text == browserChainOpenOutput {
					m.openResultSeen = true
				}
			}
		}
		if !m.openResultSeen {
			return app.ModelResponse{}, errors.New("exact browser_open_tab result was not returned to the next model round")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{
			CallID: browserChainSnapshotCallID, Name: "browser_snapshot", Arguments: `{"target_id":"target_1234567"}`,
		}}, StopReason: "tool_calls", Usage: usage}, nil
	case 3:
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != browserChainSnapshotCallID {
				continue
			}
			for _, block := range message.Content {
				if block.Text == browserChainSnapshotOutput {
					m.snapshotResultSeen = true
				}
			}
		}
		if !m.snapshotResultSeen {
			return app.ModelResponse{}, errors.New("exact browser_snapshot result was not returned to the final model round")
		}
		return app.ModelResponse{Text: browserChainAnswer, StopReason: "stop", Usage: usage}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic model round %d", m.calls)
	}
}

type browserChainBrokerStub struct {
	definitions []app.AgentToolDefinition
	outputs     map[string]string
	calls       []browserChainCall
}

type browserChainCall struct {
	TaskID      string
	OwnerID     string
	InitiatorID string
	CallID      string
	ToolName    string
	Arguments   string
	Operation   task.Operation
	Step        task.Step
}

func (b *browserChainBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *browserChainBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	wantID, wantArgs := "", ""
	switch call.Name {
	case "browser_open_tab":
		wantID, wantArgs = browserChainOpenCallID, `{"url":"https://example.test/"}`
	case "browser_snapshot":
		wantID, wantArgs = browserChainSnapshotCallID, `{"target_id":"target_1234567"}`
	default:
		return app.AgentToolResult{}, fmt.Errorf("unexpected synthetic browser tool %q", call.Name)
	}
	if call.CallID != wantID || call.Arguments != wantArgs {
		return app.AgentToolResult{}, fmt.Errorf("unexpected synthetic browser call: %#v", call)
	}
	output := b.outputs[call.Name]
	stepSummary := strings.TrimSpace(output)
	operation := task.Operation{ID: "operation-" + call.CallID, TaskID: claimed.ID, Key: app.AgentOperationKey(call.CallID),
		Status: task.OperationSucceeded, Result: output}
	step := task.Step{ID: "step-" + call.CallID, TaskID: claimed.ID, Sequence: int64(len(b.calls) + 1), Kind: "tool",
		Status: task.StepSucceeded, Title: call.Name, Summary: stepSummary, CreatedAt: time.Now()}
	b.calls = append(b.calls, browserChainCall{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		CallID: call.CallID, ToolName: call.Name, Arguments: call.Arguments, Operation: operation, Step: step})
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: output}, nil
}

// TestDSHConcurrentBrowserTasksKeepToolResultsAndWorkChainsSeparated verifies
// two tasks from one conversation can use browser tools concurrently without
// crossing model history, operation records, work-chain steps, or final replies.
func TestDSHConcurrentBrowserTasksKeepToolResultsAndWorkChainsSeparated(t *testing.T) {
	executor, _, _, _, _, first := cloudDSHFixture(t)
	checkpoints := &browserConcurrentCheckpoints{commands: make(map[string][]app.AgentCheckpointCommand)}
	executor.options.Checkpoints = checkpoints
	second := first
	second.ID = "q2234567"
	second.Objective = "read page for task B"
	first.Objective = "read page for task A"
	firstExpiry, secondExpiry := time.Now().Add(time.Minute), time.Now().Add(time.Minute)
	first.Lease.Owner, second.Lease.Owner = "morde-browser-parallel-worker", "morde-browser-parallel-worker"
	first.Lease.ExpiresAt, second.Lease.ExpiresAt = &firstExpiry, &secondExpiry
	tasks := map[string]task.Task{first.ID: first, second.ID: second}
	instances := &browserConcurrentInstanceStore{sequence: make(map[string]int64), leases: make(map[app.AgentInstanceFence]app.AgentInstanceLease),
		stopped: make(map[app.AgentInstanceFence]bool), pending: make(map[string]app.AccountAgentTaskStopAttestation)}
	stopKey := bytes.Repeat([]byte{7}, 32)
	stopAuth, err := app.NewAgentStopAuthenticator("supervisor-test", stopKey)
	if err != nil {
		t.Fatalf("create concurrent Task stop authenticator: %v", err)
	}
	instanceService, err := app.NewAgentInstanceService(instances, stopAuth)
	if err != nil {
		t.Fatalf("create concurrent Task instance service: %v", err)
	}
	executor.options.Instances = instanceService
	executor.options.AccountRuntimes.options.TaskStopper = instances
	tool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "browser_open_tab", Description: "Open a page in this Task's isolated browser.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
	}}
	models := &browserConcurrentSyntheticModel{
		taskIDs: []string{first.ID, second.ID}, bothStarted: make(chan struct{}),
		calls: make(map[string]int), started: make(map[string]bool), resultSeen: make(map[string]bool),
	}
	broker := &browserConcurrentBrokerStub{tool: tool, calls: make(map[string]browserConcurrentCall)}
	completion := &browserConcurrentCompleterStub{done: make(chan struct{}, 2), completed: make(map[string]app.CloudDSHTaskCompletion)}
	executor.options.Models = models
	executor.options.Tools = broker
	executor.options.Completion = completion

	workerID := first.Lease.Owner
	var claimMu sync.Mutex
	claimIndex := 0
	leaseStore := &taskLeaseStoreStub{
		claimWithOwnerLimit: func(_ context.Context, gotWorkerID string, _ time.Duration, ownerLimit int) (task.Task, bool, error) {
			if gotWorkerID != workerID || ownerLimit != 2 {
				return task.Task{}, false, errors.New("unexpected concurrent Task worker fence")
			}
			claimMu.Lock()
			defer claimMu.Unlock()
			ordered := []task.Task{first, second}
			if claimIndex >= len(ordered) {
				return task.Task{}, false, nil
			}
			claimed := ordered[claimIndex]
			claimIndex++
			return claimed, true, nil
		},
		renew: func(_ context.Context, taskID, gotWorkerID string, version int64, lease time.Duration) (task.Task, error) {
			claimed, ok := tasks[taskID]
			if !ok || gotWorkerID != workerID || version != claimed.Lease.Version {
				return task.Task{}, errors.New("unexpected concurrent Task lease renewal")
			}
			expiresAt := time.Now().Add(lease)
			claimed.Lease.ExpiresAt = &expiresAt
			return claimed, nil
		},
	}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: executor, WorkerID: workerID, LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2,
	})
	if err != nil {
		t.Fatalf("create concurrent browser TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()
	for range 2 {
		select {
		case <-completion.done:
		case <-ctx.Done():
			claims, renewals := leaseStore.counts()
			t.Fatalf("concurrent browser Tasks did not complete: %v; claims=%d renewals=%d model_rounds={%s:%d,%s:%d} broker_calls=%d", ctx.Err(), claims, renewals,
				first.ID, models.roundCount(first.ID), second.ID, models.roundCount(second.ID), broker.count())
		}
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after both browser Tasks completed")
	}

	for _, taskID := range []string{first.ID, second.ID} {
		if models.roundCount(taskID) != 2 || !models.sawTaskResult(taskID) {
			t.Fatalf("Task %s model rounds/result = %d/%t; want one isolated tool-result round and one final answer", taskID, models.roundCount(taskID), models.sawTaskResult(taskID))
		}
		call, ok := broker.call(taskID)
		if !ok || call.callID != "browser-open-"+taskID || call.url != "https://"+taskID+".example.test/" ||
			call.ownerID != first.OwnerID || call.operation.TaskID != taskID ||
			call.operation.Key != app.AgentOperationKey(call.callID) || call.operation.Status != task.OperationSucceeded ||
			call.operation.Result != call.output || call.step.TaskID != taskID || call.step.Sequence != 1 ||
			call.step.Status != task.StepSucceeded || call.step.Title != tool.Name || call.step.Summary != call.output {
			t.Fatalf("Task %s browser operation/work-chain record = %#v; want a successful Task-isolated result", taskID, call)
		}
		result, ok := completion.result(taskID)
		if !ok || result.Result != "Read page for "+taskID+"." || result.AgentSessionID == "" {
			t.Fatalf("Task %s completion = %#v, present=%t; want its own final response", taskID, result, ok)
		}
	}
	if broker.count() != 2 || !models.bothTasksOverlapped() {
		t.Fatalf("browser Broker calls=%d; model overlap=%t; want two concurrent isolated Tasks", broker.count(), models.bothTasksOverlapped())
	}
}

type browserConcurrentSyntheticModel struct {
	mu          sync.Mutex
	taskIDs     []string
	bothStarted chan struct{}
	calls       map[string]int
	started     map[string]bool
	resultSeen  map[string]bool
}

func (m *browserConcurrentSyntheticModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || (call.Task.TaskID != m.taskIDs[0] && call.Task.TaskID != m.taskIDs[1]) {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received an unexpected Task fence: %#v", call.Task)
	}
	taskID := call.Task.TaskID
	m.mu.Lock()
	m.calls[taskID]++
	round := m.calls[taskID]
	if round == 1 {
		m.started[taskID] = true
		if len(m.started) == 2 {
			close(m.bothStarted)
		}
	}
	m.mu.Unlock()
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode concurrent browser Task request: %w", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	switch round {
	case 1:
		select {
		case <-m.bothStarted:
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		}
		for _, offered := range request.Tools {
			if offered.Name == "browser_open_tab" {
				return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "browser-open-" + taskID,
					Name: offered.Name, Arguments: `{"url":"https://` + taskID + `.example.test/"}`}},
					StopReason: "tool_calls", Usage: usage}, nil
			}
		}
		return app.ModelResponse{}, errors.New("browser_open_tab was not offered to both concurrent DSH Tasks")
	case 2:
		wantCallID := "browser-open-" + taskID
		wantOutput := concurrentBrowserOutput(taskID)
		for _, message := range request.Messages {
			if message.Role != "tool" {
				continue
			}
			if message.ToolCallID != wantCallID {
				return app.ModelResponse{}, fmt.Errorf("Task %s received another Task's browser call id %q", taskID, message.ToolCallID)
			}
			if len(message.Content) == 1 && message.Content[0].Text == wantOutput {
				m.mu.Lock()
				m.resultSeen[taskID] = true
				m.mu.Unlock()
			}
		}
		if !m.sawTaskResult(taskID) {
			return app.ModelResponse{}, fmt.Errorf("Task %s did not receive its own exact browser result", taskID)
		}
		return app.ModelResponse{Text: "Read page for " + taskID + ".", StopReason: "stop", Usage: usage}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("Task %s unexpectedly received model round %d", taskID, round)
	}
}

func (m *browserConcurrentSyntheticModel) roundCount(taskID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[taskID]
}

func (m *browserConcurrentSyntheticModel) sawTaskResult(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resultSeen[taskID]
}

func (m *browserConcurrentSyntheticModel) bothTasksOverlapped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started) == 2
}

type browserConcurrentBrokerStub struct {
	mu    sync.Mutex
	tool  app.AgentToolDefinition
	calls map[string]browserConcurrentCall
}

type browserConcurrentCheckpoints struct {
	mu       sync.Mutex
	commands map[string][]app.AgentCheckpointCommand
}

func (s *browserConcurrentCheckpoints) Save(_ context.Context, command app.AgentCheckpointCommand) (bool, error) {
	if !uniqueCloudDSHJSON(command.Payload) {
		return false, errors.New("concurrent browser checkpoint is invalid")
	}
	sum := sha256.Sum256(command.Payload)
	if !bytes.Equal(sum[:], command.Digest) {
		return false, errors.New("concurrent browser checkpoint digest is invalid")
	}
	s.mu.Lock()
	s.commands[command.Fence.AgentSessionID] = append(s.commands[command.Fence.AgentSessionID], command)
	s.mu.Unlock()
	return false, nil
}

func (s *browserConcurrentCheckpoints) ReadPage(_ context.Context, request app.AgentCheckpointPageRequest) (app.AgentCheckpointPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands[request.SessionID] {
		if command.From != request.From || command.Through > request.Through {
			continue
		}
		if command.Through == request.Through && !bytes.Equal(command.Digest, request.ThroughDigest) {
			return app.AgentCheckpointPage{}, errors.New("concurrent browser Task checkpoint changed")
		}
		return app.AgentCheckpointPage{From: command.From, Through: command.Through,
			Digest: append([]byte(nil), command.Digest...), Payload: append([]byte(nil), command.Payload...)}, nil
	}
	return app.AgentCheckpointPage{}, errors.New("concurrent browser Task checkpoint was not found")
}

func (*browserConcurrentCheckpoints) PendingMessages(context.Context, app.AgentCheckpointFence) ([]app.AgentConversationMessage, error) {
	return nil, nil
}

// The general cloudDSHTestInstanceStore models a single active instance. This
// test needs distinct, simultaneously active task fences inside one account
// Runtime, as the production instance store supports.
type browserConcurrentInstanceStore struct {
	mu       sync.Mutex
	nextGen  int64
	sequence map[string]int64
	leases   map[app.AgentInstanceFence]app.AgentInstanceLease
	stopped  map[app.AgentInstanceFence]bool
	pending  map[string]app.AccountAgentTaskStopAttestation
}

func (s *browserConcurrentInstanceStore) StartAgentInstance(_ context.Context, request app.AgentInstanceStart) (app.AgentInstanceLease, bool, error) {
	if err := request.Validate(); err != nil || request.RuntimeFence == nil {
		return app.AgentInstanceLease{}, false, errors.New("invalid concurrent browser Task start")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextGen++
	fence := request.Fence
	fence.AgentGeneration = s.nextGen
	conversationID := request.RuntimeFence.ConversationID
	s.sequence[conversationID]++
	lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: request.RuntimeFence, RuntimeSequence: s.sequence[conversationID],
		ExpiresAt: time.Now().Add(request.LeaseDuration)}
	s.leases[fence] = lease
	return lease, false, nil
}

func (s *browserConcurrentInstanceStore) RenewAgentInstance(_ context.Context, fence app.AgentInstanceFence, duration time.Duration) (app.AgentInstanceLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[fence]
	if !ok || s.stopped[fence] {
		return app.AgentInstanceLease{}, errors.New("concurrent browser Task lease is inactive")
	}
	lease.ExpiresAt = time.Now().Add(duration)
	s.leases[fence] = lease
	return lease, nil
}

func (*browserConcurrentInstanceStore) ConfirmAgentStopped(context.Context, app.VerifiedAgentStop) (bool, error) {
	return false, nil
}

func (s *browserConcurrentInstanceStore) ConfirmTaskStopped(_ context.Context, attestation app.AccountAgentTaskStopAttestation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence := attestation.TaskFence
	if !attestation.Destroyed || s.leases[fence].Fence != fence {
		return false, errors.New("concurrent browser Task stop fence is invalid")
	}
	s.stopped[fence] = true
	s.pending[attestation.TaskFence.TaskID] = attestation
	return false, nil
}

func (s *browserConcurrentInstanceStore) ListPendingAccountAgentTaskStopAttestations(_ context.Context, _ int) ([]app.AccountAgentTaskStopAttestation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attestations := make([]app.AccountAgentTaskStopAttestation, 0, len(s.pending))
	for _, attestation := range s.pending {
		attestations = append(attestations, attestation)
	}
	return attestations, nil
}

func (s *browserConcurrentInstanceStore) MarkAccountAgentTaskStopAttestationAcknowledged(_ context.Context, attestation app.AccountAgentTaskStopAttestation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, attestation.TaskFence.TaskID)
	return false, nil
}

type browserConcurrentCall struct {
	callID    string
	url       string
	ownerID   string
	output    string
	operation task.Operation
	step      task.Step
}

func (b *browserConcurrentBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{b.tool}, nil
}

func (b *browserConcurrentBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	wantCallID := "browser-open-" + claimed.ID
	wantURL := "https://" + claimed.ID + ".example.test/"
	var arguments struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil || call.Name != b.tool.Name ||
		call.CallID != wantCallID || arguments.URL != wantURL {
		return app.AgentToolResult{}, fmt.Errorf("Task %s received another Task's browser call: %#v", claimed.ID, call)
	}
	output := concurrentBrowserOutput(claimed.ID)
	b.mu.Lock()
	b.calls[claimed.ID] = browserConcurrentCall{
		callID: call.CallID, url: arguments.URL, ownerID: claimed.OwnerID, output: output,
		operation: task.Operation{ID: "operation-" + call.CallID, TaskID: claimed.ID,
			Key: app.AgentOperationKey(call.CallID), Status: task.OperationSucceeded, Result: output},
		step: task.Step{ID: "step-" + call.CallID, TaskID: claimed.ID, Sequence: 1, Kind: "tool",
			Status: task.StepSucceeded, Title: call.Name, Summary: output, CreatedAt: time.Now()},
	}
	b.mu.Unlock()
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: output}, nil
}

func concurrentBrowserOutput(taskID string) string {
	return `{"target_id":"target_` + taskID + `","url":"https://` + taskID + `.example.test/","title":"Page for ` + taskID + `"}`
}

func (b *browserConcurrentBrokerStub) call(taskID string) (browserConcurrentCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call, ok := b.calls[taskID]
	return call, ok
}

func (b *browserConcurrentBrokerStub) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

type browserConcurrentCompleterStub struct {
	mu        sync.Mutex
	done      chan struct{}
	completed map[string]app.CloudDSHTaskCompletion
}

func (c *browserConcurrentCompleterStub) Complete(_ context.Context, value task.Task, completion app.CloudDSHTaskCompletion) error {
	c.mu.Lock()
	c.completed[value.ID] = completion
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
	return nil
}

func (c *browserConcurrentCompleterStub) result(taskID string) (app.CloudDSHTaskCompletion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result, ok := c.completed[taskID]
	return result, ok
}
