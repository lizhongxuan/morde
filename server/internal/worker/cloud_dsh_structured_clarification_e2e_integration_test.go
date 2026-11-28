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

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

const (
	m06StructuredClarificationCallID = "m06_structured_ask_call_001"
	m06StructuredClarificationArgs   = `{"question":null,"questions":[{"id":"environment","prompt":"Which environment should I use?","type":"single_choice","required":true,"allow_skip":false,"options":[{"id":"staging","label":"Staging"},{"id":"production","label":"Production"}]}]}`
	m06StructuredClarificationFinal  = "I will use staging."
	m06StructuredClarificationProof  = "m06-synthetic-memory-proof"
)

// TestCloudDSHStructuredClarificationResumesAfterWorkerRestart exercises the
// real TaskWorker, local DSH account runtime, CloudDSHToolBroker, PostgreSQL
// checkpoint and clarification rows, and durable Task result delivery. Only
// the model response is synthetic; no provider or external service is used.
func TestCloudDSHStructuredClarificationResumesAfterWorkerRestart(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic Task execution: %v", err)
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
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic user session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Ask which environment to use, then confirm the selected environment.",
		Budget: task.Budget{ActiveSeconds: 90, ToolCalls: 4, ModelTokens: 4000}, IdempotencyKey: "m06-structured-clarification-worker-restart",
	})
	if err != nil {
		t.Fatalf("submit synthetic clarification Task: %v", err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(t.TempDir() + "/workspace-blocks")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close synthetic Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	observedBroker := &m06ObservedClarificationBroker{CloudDSHToolBroker: broker}

	node, dshScript := f01NodeRuntimePaths(t)
	const supervisorID = "m06-structured-clarification-supervisor"
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, dshScript, supervisorID,
		bytes.Repeat([]byte{0x6e}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	model := newM06StructuredClarificationModel()
	model.register(submitted.Task.ID, owner.ID, m06StructuredClarificationCallID, m06StructuredClarificationArgs,
		`"option_ids":["staging"]`, m06StructuredClarificationFinal)
	makeExecutor := func() *CloudDSHExecutor {
		t.Helper()
		executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
			SupervisorID: supervisorID, AccountRuntimes: accountRuntimes, Instances: instances,
			Checkpoints: checkpoints, Models: model, Tools: observedBroker, Completion: observedBroker,
		})
		if err != nil {
			t.Fatalf("create real local DSH executor: %v", err)
		}
		return executor
	}

	firstWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: makeExecutor(), WorkerID: "m06-clarification-worker-one",
		LeaseDuration: 10 * time.Second, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create first real TaskWorker: %v", err)
	}
	firstWorkerCtx, stopFirstWorker := context.WithCancel(ctx)
	firstWorkerDone := make(chan error, 1)
	go func() { firstWorkerDone <- firstWorker.Run(firstWorkerCtx) }()

	paused := waitM06ClarificationTask(t, ctx, store, submitted.Task.ID, owner.ID, task.StatusWaitingUser)
	if paused.Task.WaitingReason != "clarification_required" || paused.Task.Lease.Owner != "" ||
		paused.Task.AgentSessionID == "" || paused.Task.CheckpointThroughSequence < 0 || len(paused.Task.CheckpointDigest) != 32 ||
		len(paused.ClarificationRequests) != 1 {
		t.Fatalf("persisted structured clarification pause = task:%+v requests:%#v", paused.Task, paused.ClarificationRequests)
	}
	request := paused.ClarificationRequests[0]
	if request.Status != task.ClarificationPending || request.Answer != "" ||
		request.CallID != m06StructuredClarificationCallID || request.Version != 1 || len(request.Questions) != 1 ||
		request.Questions[0].ID != "environment" {
		t.Fatalf("durable structured clarification request = %#v", request)
	}
	if request.AnswerFormat != task.ClarificationAnswerStructured {
		t.Errorf("PostgreSQL pending structured clarification answer_format=%q; want %q", request.AnswerFormat,
			task.ClarificationAnswerStructured)
	}

	stopFirstWorker()
	select {
	case workerErr := <-firstWorkerDone:
		if !errors.Is(workerErr, context.Canceled) {
			t.Fatalf("first TaskWorker stopped with %v; want context cancellation", workerErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first TaskWorker did not stop after simulated worker restart")
	}

	answers := []task.ClarificationAnswer{{QuestionID: "environment", OptionIDs: []string{"staging"}}}
	if err := tasks.AnswerStructuredClarificationRequest(ctx, principal, submitted.Task.ID, request.ID, request.Version+1,
		answers, "m06-clarification-stale-version"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale structured answer error = %v; want conflict", err)
	}
	stillWaiting, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || stillWaiting.Task.Status != task.StatusWaitingUser || len(stillWaiting.ClarificationRequests) != 1 ||
		stillWaiting.ClarificationRequests[0].Status != task.ClarificationPending {
		t.Fatalf("stale answer changed durable clarification state: details=%+v err=%v", stillWaiting, err)
	}
	if err := tasks.AnswerStructuredClarificationRequest(ctx, principal, submitted.Task.ID, request.ID, request.Version,
		answers, "m06-clarification-valid-answer"); err != nil {
		t.Fatalf("persist structured clarification answer in PostgreSQL: %v", err)
	}
	answered, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || answered.Task.Status != task.StatusQueued || len(answered.ClarificationRequests) != 1 ||
		answered.ClarificationRequests[0].Status != task.ClarificationAnswered || answered.ClarificationRequests[0].RespondedAt == nil ||
		!strings.Contains(answered.ClarificationRequests[0].Answer, `"option_ids":["staging"]`) {
		t.Fatalf("durable structured answer state = task:%+v requests:%#v err=%v", answered.Task, answered.ClarificationRequests, err)
	}

	secondWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: makeExecutor(), WorkerID: "m06-clarification-worker-two",
		LeaseDuration: 10 * time.Second, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create restarted real TaskWorker: %v", err)
	}
	secondWorkerCtx, stopSecondWorker := context.WithCancel(ctx)
	secondWorkerDone := make(chan error, 1)
	go func() { secondWorkerDone <- secondWorker.Run(secondWorkerCtx) }()

	completed := waitM06ClarificationTask(t, ctx, store, submitted.Task.ID, owner.ID, task.StatusCompleted)
	if completed.Task.Result != m06StructuredClarificationFinal || completed.Task.ConversationID != submitted.Conversation.ID ||
		completed.Task.AgentSessionID != paused.Task.AgentSessionID {
		t.Fatalf("recovered structured clarification Task = status:%s result:%q session:%q conversation_match:%t",
			completed.Task.Status, completed.Task.Result, completed.Task.AgentSessionID,
			completed.Task.ConversationID == submitted.Conversation.ID)
	}
	stopSecondWorker()
	select {
	case workerErr := <-secondWorkerDone:
		if !errors.Is(workerErr, context.Canceled) {
			t.Fatalf("restarted TaskWorker stopped with %v; want context cancellation", workerErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restarted TaskWorker did not stop after Task completion")
	}

	toolCalls := observedBroker.snapshot()
	if len(toolCalls) != 2 || toolCalls[0].reconciled || !toolCalls[1].reconciled ||
		toolCalls[0].callID != m06StructuredClarificationCallID || toolCalls[1].callID != m06StructuredClarificationCallID ||
		toolCalls[0].name != "ask_user" || toolCalls[1].name != "ask_user" ||
		toolCalls[0].result.Status != app.AgentToolNeedsUser || toolCalls[1].result.Status != app.AgentToolSucceeded ||
		!strings.Contains(toolCalls[1].result.Output, `"option_ids":["staging"]`) {
		t.Fatalf("Broker did not reconcile the same original structured ask_user call exactly once: %#v", toolCalls)
	}
	modelCalls := model.snapshot()
	if len(modelCalls) != 2 || modelCalls[0].task.AgentSessionID == "" || modelCalls[0].task.AgentSessionID != modelCalls[1].task.AgentSessionID ||
		modelCalls[0].task.TaskID != submitted.Task.ID || modelCalls[1].task.TaskID != submitted.Task.ID {
		t.Fatalf("DSH model calls did not resume the same Task session: %#v", modelCalls)
	}

	clarificationRows := 0
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_clarification_requests WHERE task_id=$1 AND call_id=$2`,
		submitted.Task.ID, m06StructuredClarificationCallID).Scan(&clarificationRows); err != nil || clarificationRows != 1 {
		t.Fatalf("durable clarification rows=%d err=%v; want one row for the original DSH call", clarificationRows, err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, submitted.Conversation.ID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	assistantResults := 0
	for _, message := range messages.Items {
		if message.Role == conversation.RoleAssistant && message.TaskID == submitted.Task.ID {
			assistantResults++
			if message.Content != m06StructuredClarificationFinal {
				t.Fatalf("assistant result content=%q; want exactly the synthetic final answer", message.Content)
			}
		}
	}
	if assistantResults != 1 {
		t.Fatalf("conversation has %d assistant result messages for Task %s; want exactly one", assistantResults, submitted.Task.ID)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resultEvents := 0
	for _, event := range events {
		if event.Type == "task.result" {
			resultEvents++
		}
	}
	if resultEvents != 1 {
		t.Fatalf("Task has %d terminal result events; want exactly one", resultEvents)
	}
}

type m06ClarificationToolCall struct {
	taskID     string
	sessionID  string
	callID     string
	name       string
	reconciled bool
	result     app.AgentToolResult
	err        error
}

type m06ObservedClarificationBroker struct {
	*app.CloudDSHToolBroker
	mu    sync.Mutex
	calls []m06ClarificationToolCall
}

func (broker *m06ObservedClarificationBroker) Execute(ctx context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	result, err := broker.CloudDSHToolBroker.Execute(ctx, claimed, call)
	broker.record(call, false, result, err)
	return result, err
}

func (broker *m06ObservedClarificationBroker) Reconcile(ctx context.Context, claimed task.Task, call app.ModelToolCall,
	checkpointOutput *string) (app.AgentToolResult, error) {
	result, err := broker.CloudDSHToolBroker.Reconcile(ctx, claimed, call, checkpointOutput)
	broker.record(call, true, result, err)
	return result, err
}

func (broker *m06ObservedClarificationBroker) record(call app.ModelToolCall, reconciled bool, result app.AgentToolResult, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	value := m06ClarificationToolCall{callID: call.CallID, name: call.Name,
		reconciled: reconciled, result: result, err: err}
	if call.TaskFence != nil {
		value.taskID = call.TaskFence.TaskID
		value.sessionID = call.TaskFence.AgentSessionID
	}
	broker.calls = append(broker.calls, value)
}

func (broker *m06ObservedClarificationBroker) snapshot() []m06ClarificationToolCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]m06ClarificationToolCall(nil), broker.calls...)
}

func (*m06ObservedClarificationBroker) InitialContext(context.Context, task.Task) (string, error) {
	return "Synthetic local test context.\n" + app.CloudDSHMemoryProofPrefix + m06StructuredClarificationProof, nil
}

func (*m06ObservedClarificationBroker) ValidatePersonalMemoryProof(_ context.Context, _ task.Task, proof string,
	versions []app.CloudDSHMemoryVersion) error {
	if proof != m06StructuredClarificationProof || len(versions) != 0 {
		return errors.New("unexpected synthetic personal-memory proof")
	}
	return nil
}

func (*m06ObservedClarificationBroker) CloudDSHPersonalMemoryProofEnabled() bool { return true }

func (*m06ObservedClarificationBroker) CloudDSHPersonalMemoryProofEnabledForTask(task.Task) bool {
	return true
}

type m06StructuredClarificationModelCall struct {
	task app.ModelTaskFence
}

type m06StructuredClarificationTaskModel struct {
	ownerID       string
	callID        string
	arguments     string
	answerMarker  string
	finalResponse string
	rounds        int
}

type m06StructuredClarificationModel struct {
	mu                 sync.Mutex
	tasks              map[string]*m06StructuredClarificationTaskModel
	calls              []m06StructuredClarificationModelCall
	firstRoundGate     chan struct{}
	firstRoundExpected int
	firstRoundArrivals int
}

func newM06StructuredClarificationModel() *m06StructuredClarificationModel {
	return &m06StructuredClarificationModel{tasks: make(map[string]*m06StructuredClarificationTaskModel)}
}

func (model *m06StructuredClarificationModel) register(taskID, ownerID, callID, arguments, answerMarker, finalResponse string) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.tasks[taskID] = &m06StructuredClarificationTaskModel{ownerID: ownerID, callID: callID,
		arguments: arguments, answerMarker: answerMarker, finalResponse: finalResponse}
}

func (model *m06StructuredClarificationModel) requireConcurrentFirstRounds(expected int) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.firstRoundExpected = expected
	model.firstRoundGate = make(chan struct{})
}

func (model *m06StructuredClarificationModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (model *m06StructuredClarificationModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	if call.Task == nil {
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
		return app.ModelResponse{}, fmt.Errorf("decode synthetic model request: %w", err)
	}
	model.mu.Lock()
	state, ok := model.tasks[call.Task.TaskID]
	if !ok || call.Task.OwnerID != state.ownerID {
		model.mu.Unlock()
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	state.rounds++
	round := state.rounds
	config := *state
	model.calls = append(model.calls, m06StructuredClarificationModelCall{task: *call.Task})
	var firstRoundGate chan struct{}
	if round == 1 && model.firstRoundGate != nil {
		model.firstRoundArrivals++
		firstRoundGate = model.firstRoundGate
		if model.firstRoundArrivals == model.firstRoundExpected {
			close(model.firstRoundGate)
		}
	}
	model.mu.Unlock()
	if firstRoundGate != nil {
		select {
		case <-firstRoundGate:
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		}
	}
	switch round {
	case 1:
		askUserOffered := false
		for _, tool := range request.Tools {
			askUserOffered = askUserOffered || tool.Name == "ask_user"
		}
		if !askUserOffered {
			return app.ModelResponse{}, errors.New("real Broker did not offer the locked ask_user tool")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: config.callID,
			Name: "ask_user", Arguments: config.arguments}}, StopReason: "tool_calls",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
	case 2:
		answerSeen := false
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != config.callID {
				continue
			}
			for _, content := range message.Content {
				answerSeen = answerSeen || strings.Contains(content.Text, config.answerMarker)
			}
		}
		if !answerSeen {
			return app.ModelResponse{}, fmt.Errorf("recovered DSH request for Task %s did not receive the persisted answer %q",
				call.Task.TaskID, config.answerMarker)
		}
		return app.ModelResponse{Text: config.finalResponse, StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 9, OutputTokens: 4}}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic model round %d for Task %s", round, call.Task.TaskID)
	}
}

func (model *m06StructuredClarificationModel) snapshot() []m06StructuredClarificationModelCall {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]m06StructuredClarificationModelCall(nil), model.calls...)
}

func waitM06ClarificationTask(t *testing.T, ctx context.Context, store interface {
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}, taskID, ownerID string, status task.Status) app.TaskDetails {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		details, err := store.GetTask(ctx, taskID, ownerID)
		if err != nil {
			t.Fatalf("read Task %s while waiting for %s: %v", taskID, status, err)
		}
		if details.Task.Status == status {
			return details
		}
		if details.Task.Status == task.StatusFailed || details.Task.Status == task.StatusCancelled {
			t.Fatalf("Task stopped in %s while waiting for %s: %+v", details.Task.Status, status, details.Task)
		}
		if status != task.StatusWaitingUser && details.Task.Status == task.StatusWaitingUser &&
			details.Task.WaitingReason == "dsh_recovery_unresolved" {
			t.Fatalf("Task recovery became unresolved instead of reaching %s: %+v", status, details.Task)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Task %s did not reach %s: %v; last state=%s", taskID, status, ctx.Err(), details.Task.Status)
		case <-ticker.C:
		}
	}
}

type m06ClarificationHarness struct {
	store          *storepostgres.Store
	pool           *pgxpool.Pool
	owner          identity.User
	principal      identity.Principal
	tasks          *app.TaskService
	instances      *app.AgentInstanceService
	accountRuntime *CloudDSHAccountRuntimeManager
	checkpoints    *app.AgentCheckpointService
	broker         *m06ObservedClarificationBroker
	model          *m06StructuredClarificationModel
	supervisorID   string
}

func newM06ClarificationHarness(t *testing.T, ctx context.Context) *m06ClarificationHarness {
	t.Helper()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic Task execution: %v", err)
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
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic user session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(t.TempDir() + "/workspace-blocks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close synthetic Workspace blocks: %v", err)
		}
	})
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	observedBroker := &m06ObservedClarificationBroker{CloudDSHToolBroker: broker}
	node, dshScript := f01NodeRuntimePaths(t)
	const supervisorID = "m06-clarification-matrix-supervisor"
	instances, accountRuntime, _ := newF01ConversationRuntime(t, store, node, dshScript, supervisorID,
		bytes.Repeat([]byte{0x6e}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return &m06ClarificationHarness{
		store: store, pool: pool, owner: owner, principal: principal, tasks: tasks, instances: instances,
		accountRuntime: accountRuntime, checkpoints: checkpoints, broker: observedBroker,
		model: newM06StructuredClarificationModel(), supervisorID: supervisorID,
	}
}

func (harness *m06ClarificationHarness) submit(t *testing.T, ctx context.Context, idempotencyKey, objective string) app.TaskSubmissionResult {
	t.Helper()
	result, err := harness.tasks.Submit(ctx, harness.principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: objective,
		Budget: task.Budget{ActiveSeconds: 90, ToolCalls: 4, ModelTokens: 4000}, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatalf("submit synthetic clarification Task: %v", err)
	}
	return result
}

func (harness *m06ClarificationHarness) newWorker(t *testing.T, workerID string, maxConcurrent, maxPerOwner int,
	checkpoints CloudDSHCheckpoints) *TaskWorker {
	t.Helper()
	if checkpoints == nil {
		checkpoints = harness.checkpoints
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: harness.supervisorID, AccountRuntimes: harness.accountRuntime, Instances: harness.instances,
		Checkpoints: checkpoints, Models: harness.model, Tools: harness.broker, Completion: harness.broker,
	})
	if err != nil {
		t.Fatalf("create real local DSH executor: %v", err)
	}
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: harness.store, Executor: executor, WorkerID: workerID, LeaseDuration: 10 * time.Second,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: maxConcurrent,
		MaxConcurrentTasksPerOwner: maxPerOwner,
	})
	if err != nil {
		t.Fatalf("create real TaskWorker %s: %v", workerID, err)
	}
	return worker
}

func startM06Worker(t *testing.T, ctx context.Context, worker *TaskWorker) (context.CancelFunc, <-chan error) {
	t.Helper()
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func stopM06Worker(t *testing.T, cancel context.CancelFunc, done <-chan error, name string) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s stopped with %v; want context cancellation", name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not stop", name)
	}
}

func TestCloudDSHStructuredClarificationCancelDoesNotResumeAskUser(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	harness := newM06ClarificationHarness(t, ctx)
	submitted := harness.submit(t, ctx, "m06-cancel-waiting-clarification", "Ask for an environment and wait for an answer.")
	callID := "m06_cancel_ask_" + submitted.Task.ID
	harness.model.register(submitted.Task.ID, harness.owner.ID, callID, m06StructuredClarificationArgs,
		`"option_ids":["staging"]`, "This answer must never be delivered.")
	firstWorker := harness.newWorker(t, "m06-cancel-clarification-worker", 1, 1, nil)
	stopFirst, firstDone := startM06Worker(t, ctx, firstWorker)
	paused := waitM06ClarificationTask(t, ctx, harness.store, submitted.Task.ID, harness.owner.ID, task.StatusWaitingUser)
	if len(paused.ClarificationRequests) != 1 || paused.ClarificationRequests[0].Status != task.ClarificationPending ||
		paused.ClarificationRequests[0].CallID != callID {
		t.Fatalf("pending clarification before cancellation = %#v", paused.ClarificationRequests)
	}
	stopM06Worker(t, stopFirst, firstDone, "first TaskWorker")

	action, err := harness.tasks.RequestAction(ctx, harness.principal, submitted.Task.ID, task.ActionCancel,
		task.StatusWaitingUser, "m06-cancel-waiting-clarification-action")
	if err != nil || action.Task.Status != task.StatusCancelling {
		t.Fatalf("cancel waiting clarification Task = status:%s err:%v; want cancelling", action.Task.Status, err)
	}
	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: harness.store,
		Executor: taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
			return task.OperationCancelled, "no operation was dispatched before ask_user", false, nil
		}),
		WorkerID: "m06-cancel-clarification-stop-worker", LeaseDuration: time.Second, PollInterval: 10 * time.Millisecond,
		OperationTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create TaskStopWorker: %v", err)
	}
	stopCtx, stopStopWorker := context.WithCancel(ctx)
	stopDone := make(chan error, 1)
	go func() { stopDone <- stopWorker.Run(stopCtx) }()
	t.Cleanup(stopStopWorker)
	cancelled := waitM06ClarificationTask(t, ctx, harness.store, submitted.Task.ID, harness.owner.ID, task.StatusCancelled)
	stopStopWorker()
	select {
	case err := <-stopDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskStopWorker stopped with %v; want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskStopWorker did not stop")
	}
	if len(cancelled.ClarificationRequests) != 1 || cancelled.ClarificationRequests[0].Status != task.ClarificationCancelled {
		t.Fatalf("cancelled clarification request = %#v", cancelled.ClarificationRequests)
	}
	if err := harness.tasks.AnswerStructuredClarificationRequest(ctx, harness.principal, submitted.Task.ID,
		paused.ClarificationRequests[0].ID, paused.ClarificationRequests[0].Version,
		[]task.ClarificationAnswer{{QuestionID: "environment", OptionIDs: []string{"staging"}}},
		"m06-cancelled-clarification-answer"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("answer after cancellation error=%v; want conflict", err)
	}
	if calls := harness.broker.snapshot(); len(calls) != 1 || calls[0].reconciled || calls[0].callID != callID {
		t.Fatalf("cancelled Task resumed the original ask_user call: %#v", calls)
	}
	if calls := harness.model.snapshot(); len(calls) != 1 || calls[0].task.TaskID != submitted.Task.ID {
		t.Fatalf("cancelled Task reached the model %d times; want only its initial ask_user call", len(calls))
	}
	var requestStatus string
	if err := harness.pool.QueryRow(ctx, `SELECT status FROM task_clarification_requests WHERE task_id=$1 AND call_id=$2`,
		submitted.Task.ID, callID).Scan(&requestStatus); err != nil || requestStatus != string(task.ClarificationCancelled) {
		t.Fatalf("PostgreSQL clarification status=%q err=%v; want cancelled", requestStatus, err)
	}
}

func TestCloudDSHStructuredClarificationConcurrentTasksStayIsolated(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	harness := newM06ClarificationHarness(t, ctx)
	first := harness.submit(t, ctx, "m06-concurrent-clarification-one", "Ask for the environment for Task one.")
	second := harness.submit(t, ctx, "m06-concurrent-clarification-two", "Ask for the environment for Task two.")
	if first.Task.ID == second.Task.ID || first.Conversation.ID == second.Conversation.ID {
		t.Fatalf("concurrent Task identities are not isolated: first=%+v second=%+v", first.Task, second.Task)
	}
	firstCallID, secondCallID := "m06_concurrent_"+first.Task.ID, "m06_concurrent_"+second.Task.ID
	harness.model.register(first.Task.ID, harness.owner.ID, firstCallID, m06StructuredClarificationArgs,
		`"option_ids":["staging"]`, "Task one selected staging.")
	harness.model.register(second.Task.ID, harness.owner.ID, secondCallID, m06StructuredClarificationArgs,
		`"option_ids":["production"]`, "Task two selected production.")
	harness.model.requireConcurrentFirstRounds(2)
	firstWorker := harness.newWorker(t, "m06-concurrent-clarification-worker-one", 2, 2, nil)
	stopFirst, firstDone := startM06Worker(t, ctx, firstWorker)
	pausedFirst := waitM06ClarificationTask(t, ctx, harness.store, first.Task.ID, harness.owner.ID, task.StatusWaitingUser)
	pausedSecond := waitM06ClarificationTask(t, ctx, harness.store, second.Task.ID, harness.owner.ID, task.StatusWaitingUser)
	if pausedFirst.Task.AgentSessionID == pausedSecond.Task.AgentSessionID || len(pausedFirst.ClarificationRequests) != 1 ||
		len(pausedSecond.ClarificationRequests) != 1 || pausedFirst.ClarificationRequests[0].CallID != firstCallID ||
		pausedSecond.ClarificationRequests[0].CallID != secondCallID {
		t.Fatalf("parallel clarification sessions or calls crossed: first=%+v second=%+v", pausedFirst.Task, pausedSecond.Task)
	}
	stopM06Worker(t, stopFirst, firstDone, "concurrent TaskWorker before clarification answers")

	if err := harness.tasks.AnswerStructuredClarificationRequest(ctx, harness.principal, first.Task.ID,
		pausedFirst.ClarificationRequests[0].ID, pausedFirst.ClarificationRequests[0].Version,
		[]task.ClarificationAnswer{{QuestionID: "environment", OptionIDs: []string{"staging"}}},
		"m06-concurrent-clarification-answer-one"); err != nil {
		t.Fatalf("answer first concurrent clarification: %v", err)
	}
	if err := harness.tasks.AnswerStructuredClarificationRequest(ctx, harness.principal, second.Task.ID,
		pausedSecond.ClarificationRequests[0].ID, pausedSecond.ClarificationRequests[0].Version,
		[]task.ClarificationAnswer{{QuestionID: "environment", OptionIDs: []string{"production"}}},
		"m06-concurrent-clarification-answer-two"); err != nil {
		t.Fatalf("answer second concurrent clarification: %v", err)
	}
	secondWorker := harness.newWorker(t, "m06-concurrent-clarification-worker-two", 2, 2, nil)
	stopSecond, secondDone := startM06Worker(t, ctx, secondWorker)
	completedFirst := waitM06ClarificationTask(t, ctx, harness.store, first.Task.ID, harness.owner.ID, task.StatusCompleted)
	completedSecond := waitM06ClarificationTask(t, ctx, harness.store, second.Task.ID, harness.owner.ID, task.StatusCompleted)
	stopM06Worker(t, stopSecond, secondDone, "restarted concurrent TaskWorker")
	if completedFirst.Task.Result != "Task one selected staging." || completedSecond.Task.Result != "Task two selected production." ||
		completedFirst.Task.AgentSessionID != pausedFirst.Task.AgentSessionID || completedSecond.Task.AgentSessionID != pausedSecond.Task.AgentSessionID {
		t.Fatalf("parallel Task results/session recovery crossed: first=%+v second=%+v", completedFirst.Task, completedSecond.Task)
	}
	modelCalls := harness.model.snapshot()
	modelSessions := make(map[string][]string)
	for _, call := range modelCalls {
		modelSessions[call.task.TaskID] = append(modelSessions[call.task.TaskID], call.task.AgentSessionID)
	}
	if len(modelCalls) != 4 || len(modelSessions[first.Task.ID]) != 2 || len(modelSessions[second.Task.ID]) != 2 ||
		modelSessions[first.Task.ID][0] != modelSessions[first.Task.ID][1] ||
		modelSessions[second.Task.ID][0] != modelSessions[second.Task.ID][1] ||
		modelSessions[first.Task.ID][0] == modelSessions[second.Task.ID][0] {
		t.Fatalf("concurrent model request fences are not Task/session isolated: %#v", modelSessions)
	}
	toolCalls := harness.broker.snapshot()
	taskCalls := make(map[string][]m06ClarificationToolCall)
	for _, call := range toolCalls {
		taskCalls[call.taskID] = append(taskCalls[call.taskID], call)
	}
	if len(toolCalls) != 4 || len(taskCalls[first.Task.ID]) != 2 || len(taskCalls[second.Task.ID]) != 2 {
		t.Fatalf("parallel Broker call count/Task attribution = %#v", taskCalls)
	}
	for _, check := range []struct {
		taskID, callID, marker string
	}{
		{first.Task.ID, firstCallID, `"option_ids":["staging"]`},
		{second.Task.ID, secondCallID, `"option_ids":["production"]`},
	} {
		calls := taskCalls[check.taskID]
		if calls[0].reconciled || !calls[1].reconciled || calls[0].callID != check.callID || calls[1].callID != check.callID ||
			calls[0].sessionID == "" || calls[0].sessionID != calls[1].sessionID ||
			calls[1].result.Status != app.AgentToolSucceeded || !strings.Contains(calls[1].result.Output, check.marker) {
			t.Fatalf("Task %s Broker recovery crossed identity/answer: %#v", check.taskID, calls)
		}
	}
}

func TestCloudDSHLegacyAskUserCheckpointResumesAfterWorkerRestart(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	harness := newM06ClarificationHarness(t, ctx)
	submitted := harness.submit(t, ctx, "m06-legacy-ask-user-checkpoint", "Ask which environment to use before continuing.")
	callID := "m06_legacy_ask_" + submitted.Task.ID
	harness.model.register(submitted.Task.ID, harness.owner.ID, callID, `{"question":"Which environment should I use?"}`,
		"production", "The user selected production.")
	legacyCheckpoints := &m06LegacyAskUserCheckpointStore{CloudDSHCheckpoints: harness.checkpoints}
	firstWorker := harness.newWorker(t, "m06-legacy-clarification-worker-one", 1, 1, legacyCheckpoints)
	stopFirst, firstDone := startM06Worker(t, ctx, firstWorker)
	paused := waitM06ClarificationTask(t, ctx, harness.store, submitted.Task.ID, harness.owner.ID, task.StatusWaitingUser)
	if len(paused.ClarificationRequests) != 1 || paused.ClarificationRequests[0].CallID != callID ||
		paused.ClarificationRequests[0].AnswerFormat != task.ClarificationAnswerFreeText ||
		len(paused.ClarificationRequests[0].Questions) != 0 {
		t.Fatalf("legacy free-text ask_user state = %#v", paused.ClarificationRequests)
	}
	if writes := legacyCheckpoints.legacyWrites(); writes == 0 {
		t.Fatal("checkpoint fixture did not persist a pinned legacy ask_user schema into PostgreSQL")
	}
	stopM06Worker(t, stopFirst, firstDone, "first legacy-checkpoint TaskWorker")
	request := paused.ClarificationRequests[0]
	if err := harness.tasks.AnswerClarificationRequest(ctx, harness.principal, submitted.Task.ID, request.ID, request.Version,
		"production", "", "m06-legacy-clarification-answer"); err != nil {
		t.Fatalf("answer legacy clarification: %v", err)
	}
	secondWorker := harness.newWorker(t, "m06-legacy-clarification-worker-two", 1, 1, legacyCheckpoints)
	stopSecond, secondDone := startM06Worker(t, ctx, secondWorker)
	completed := waitM06ClarificationTask(t, ctx, harness.store, submitted.Task.ID, harness.owner.ID, task.StatusCompleted)
	stopM06Worker(t, stopSecond, secondDone, "restarted legacy-checkpoint TaskWorker")
	if completed.Task.Result != "The user selected production." || completed.Task.AgentSessionID != paused.Task.AgentSessionID {
		t.Fatalf("legacy checkpoint recovery changed Task/session: result=%q session=%q want_session=%q",
			completed.Task.Result, completed.Task.AgentSessionID, paused.Task.AgentSessionID)
	}
	if calls := harness.broker.snapshot(); len(calls) != 2 || calls[0].reconciled || !calls[1].reconciled ||
		calls[0].callID != callID || calls[1].callID != callID || calls[1].result.Status != app.AgentToolSucceeded ||
		calls[1].result.Output != "production" {
		t.Fatalf("legacy pinned ask_user call did not reconcile under its original CallID: %#v", calls)
	}
	if calls := harness.model.snapshot(); len(calls) != 2 || calls[0].task.AgentSessionID != calls[1].task.AgentSessionID {
		t.Fatalf("legacy checkpoint did not resume the original DSH session: %#v", calls)
	}
}

type m06LegacyAskUserCheckpointStore struct {
	CloudDSHCheckpoints
	mu               sync.Mutex
	legacyWriteCount int
}

func (checkpoints *m06LegacyAskUserCheckpointStore) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	legacyPayload, replacements, err := m06ReplaceCheckpointAskUserWithLegacy(command.Payload)
	if err != nil {
		return false, err
	}
	if replacements > 0 {
		command.Payload = legacyPayload
		digest := sha256.Sum256(legacyPayload)
		command.Digest = digest[:]
		checkpoints.mu.Lock()
		checkpoints.legacyWriteCount += replacements
		checkpoints.mu.Unlock()
	}
	return checkpoints.CloudDSHCheckpoints.Save(ctx, command)
}

func (checkpoints *m06LegacyAskUserCheckpointStore) legacyWritesCount() int {
	checkpoints.mu.Lock()
	defer checkpoints.mu.Unlock()
	return checkpoints.legacyWriteCount
}

func (checkpoints *m06LegacyAskUserCheckpointStore) legacyWrites() int {
	return checkpoints.legacyWritesCount()
}

func m06ReplaceCheckpointAskUserWithLegacy(payload []byte) ([]byte, int, error) {
	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		return nil, 0, fmt.Errorf("decode DSH checkpoint for legacy tool fixture: %w", err)
	}
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint["events"], &events); err != nil {
		return nil, 0, fmt.Errorf("decode DSH checkpoint events for legacy tool fixture: %w", err)
	}
	legacyTool, err := json.Marshal(cloudDSHToolPayload([]app.AgentToolDefinition{app.CloudDSHLegacyAskUserToolDefinition()})[0])
	if err != nil {
		return nil, 0, err
	}
	replacements := 0
	for _, event := range events {
		var eventType string
		if err := json.Unmarshal(event["type"], &eventType); err != nil || eventType != "request/header" {
			continue
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(event["data"], &data); err != nil {
			return nil, 0, fmt.Errorf("decode DSH request/header data: %w", err)
		}
		var header map[string]json.RawMessage
		if err := json.Unmarshal(data["header"], &header); err != nil {
			return nil, 0, fmt.Errorf("decode DSH request header: %w", err)
		}
		var tools []json.RawMessage
		if err := json.Unmarshal(header["tools"], &tools); err != nil {
			return nil, 0, fmt.Errorf("decode DSH pinned tools: %w", err)
		}
		changed := false
		for index, rawTool := range tools {
			var tool struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(rawTool, &tool); err != nil {
				return nil, 0, fmt.Errorf("decode DSH pinned tool: %w", err)
			}
			if tool.Name == "ask_user" {
				tools[index] = append(json.RawMessage(nil), legacyTool...)
				replacements++
				changed = true
			}
		}
		if !changed {
			continue
		}
		encodedTools, err := json.Marshal(tools)
		if err != nil {
			return nil, 0, err
		}
		header["tools"] = encodedTools
		encodedHeader, err := json.Marshal(header)
		if err != nil {
			return nil, 0, err
		}
		data["header"] = encodedHeader
		encodedData, err := json.Marshal(data)
		if err != nil {
			return nil, 0, err
		}
		event["data"] = encodedData
	}
	if replacements == 0 {
		return payload, 0, nil
	}
	encodedEvents, err := json.Marshal(events)
	if err != nil {
		return nil, 0, err
	}
	checkpoint["events"] = encodedEvents
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, 0, err
	}
	return encoded, replacements, nil
}
