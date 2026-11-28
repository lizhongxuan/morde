package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
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
	model := &m06StructuredClarificationModel{taskID: submitted.Task.ID, ownerID: owner.ID}
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
	broker.calls = append(broker.calls, m06ClarificationToolCall{callID: call.CallID, name: call.Name,
		reconciled: reconciled, result: result, err: err})
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

type m06StructuredClarificationModel struct {
	mu      sync.Mutex
	taskID  string
	ownerID string
	calls   []m06StructuredClarificationModelCall
}

func (model *m06StructuredClarificationModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (model *m06StructuredClarificationModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	if call.Task == nil || call.Task.TaskID != model.taskID || call.Task.OwnerID != model.ownerID {
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
	defer model.mu.Unlock()
	model.calls = append(model.calls, m06StructuredClarificationModelCall{task: *call.Task})
	switch len(model.calls) {
	case 1:
		askUserOffered := false
		for _, tool := range request.Tools {
			askUserOffered = askUserOffered || tool.Name == "ask_user"
		}
		if !askUserOffered {
			return app.ModelResponse{}, errors.New("real Broker did not offer the locked ask_user tool")
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: m06StructuredClarificationCallID,
			Name: "ask_user", Arguments: m06StructuredClarificationArgs}}, StopReason: "tool_calls",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
	case 2:
		answerSeen := false
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != m06StructuredClarificationCallID {
				continue
			}
			for _, content := range message.Content {
				answerSeen = answerSeen || strings.Contains(content.Text, `"option_ids":["staging"]`)
			}
		}
		if !answerSeen {
			return app.ModelResponse{}, errors.New("recovered DSH request did not receive the persisted structured answer")
		}
		return app.ModelResponse{Text: m06StructuredClarificationFinal, StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 9, OutputTokens: 4}}, nil
	default:
		return app.ModelResponse{}, fmt.Errorf("unexpected synthetic model round %d", len(model.calls))
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
