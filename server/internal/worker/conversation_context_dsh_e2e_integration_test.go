package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

type conversationContextScopedRuntimeControl struct {
	CloudDSHAccountRuntimeControl
	leaseOwner string
}

const conversationContextCompletionSteer = "Please include the word BLUE in the answer."
const conversationContextCompletionWriteCallID = "completion_race_write_once"
const conversationContextCompletionWriteToolName = "mcp_k1234567_abcdef123456"
const conversationContextCompletionWriteOutput = `{"record_id":"completion-race-record"}`
const conversationContextCompletionMemoryProof = "completion-race-test-proof"
const conversationContextCompletionIndependentContent = "Start another task for an independent request while the first result is being committed."
const conversationContextCompletionIndependentResult = "The independent request completed without interrupting the first task."
const conversationContextCompletionResultWindow = 2500 * time.Millisecond

type conversationContextCompletionRaceModel struct {
	mu                  sync.Mutex
	calls               []app.ModelCall
	independentTaskID   string
	independentStarted  chan struct{}
	independentContinue <-chan struct{}
}

func (m *conversationContextCompletionRaceModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	callNumber := len(m.calls)
	m.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 4}
	if callNumber == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: conversationContextCompletionWriteCallID,
			Name: conversationContextCompletionWriteToolName, Arguments: `{"record_name":"completion-race"}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	if callNumber == 2 {
		writeResult, found := f01ToolResult(call.RawRequest, conversationContextCompletionWriteCallID)
		if !found || !strings.Contains(writeResult, `"record_id":"completion-race-record"`) {
			return app.ModelResponse{}, errors.New("first completion request omitted the completed write result")
		}
		return app.ModelResponse{Text: "STALE COMPLETION RESULT", StopReason: "stop", Usage: usage}, nil
	}
	if callNumber == 4 {
		m.mu.Lock()
		independentTaskID := m.independentTaskID
		m.mu.Unlock()
		if call.Task == nil || independentTaskID == "" || call.Task.TaskID != independentTaskID {
			return app.ModelResponse{}, fmt.Errorf("independent request did not retain its own Task identity: task=%+v", call.Task)
		}
		if m.independentStarted != nil {
			select {
			case m.independentStarted <- struct{}{}:
			default:
			}
		}
		if m.independentContinue != nil {
			select {
			case <-m.independentContinue:
			case <-ctx.Done():
				return app.ModelResponse{}, ctx.Err()
			}
		}
		return app.ModelResponse{Text: conversationContextCompletionIndependentResult, StopReason: "stop", Usage: usage}, nil
	}
	writeResult, found := f01ToolResult(call.RawRequest, conversationContextCompletionWriteCallID)
	if callNumber != 3 || !strings.Contains(string(call.RawRequest), conversationContextCompletionSteer) || !found ||
		!strings.Contains(writeResult, `"record_id":"completion-race-record"`) {
		return app.ModelResponse{}, fmt.Errorf("recovered model call did not receive the pending steer: call=%d", callNumber)
	}
	return app.ModelResponse{Text: "The revised answer includes BLUE.", StopReason: "stop", Usage: usage}, nil
}

func (m *conversationContextCompletionRaceModel) snapshot() []app.ModelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]app.ModelCall(nil), m.calls...)
}

type conversationContextCompletionRaceTools struct {
	mu         sync.Mutex
	results    map[string]app.AgentToolResult
	executions int
	reconciles int
}

func (t *conversationContextCompletionRaceTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{{ModelTool: app.ModelTool{Name: conversationContextCompletionWriteToolName,
		Description: "Create one synthetic external record for the completion race test.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"record_name":{"type":"string"}},"required":["record_name"],"additionalProperties":false}`)}}}, nil
}

func (t *conversationContextCompletionRaceTools) AdmitToolCall(context.Context, task.Task, app.ModelToolCall,
	bool) (app.AgentToolCallAdmission, error) {
	return app.AgentToolCallAdmission{Status: app.AgentToolCallAdmitted}, nil
}

func (t *conversationContextCompletionRaceTools) Execute(_ context.Context, _ task.Task,
	call app.ModelToolCall) (app.AgentToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if call.CallID != conversationContextCompletionWriteCallID || call.Name != conversationContextCompletionWriteToolName {
		return app.AgentToolResult{}, fmt.Errorf("unexpected side-effecting call %q/%q", call.Name, call.CallID)
	}
	if t.results == nil {
		t.results = make(map[string]app.AgentToolResult)
	}
	if result, exists := t.results[call.CallID]; exists {
		return result, nil
	}
	t.executions++
	result := app.AgentToolResult{Status: app.AgentToolSucceeded, Output: conversationContextCompletionWriteOutput}
	t.results[call.CallID] = result
	return result, nil
}

func (t *conversationContextCompletionRaceTools) Reconcile(_ context.Context, _ task.Task,
	call app.ModelToolCall, _ *string) (app.AgentToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	result, exists := t.results[call.CallID]
	if !exists {
		return app.AgentToolResult{}, errors.New("side-effecting call has no durable result")
	}
	t.reconciles++
	return result, nil
}

func (*conversationContextCompletionRaceTools) InitialContext(context.Context, task.Task) (string, error) {
	return "Synthetic completion race context.\n" + app.CloudDSHMemoryProofPrefix + conversationContextCompletionMemoryProof, nil
}

func (*conversationContextCompletionRaceTools) ValidatePersonalMemoryProof(_ context.Context, _ task.Task, proof string,
	versions []app.CloudDSHMemoryVersion) error {
	if proof != conversationContextCompletionMemoryProof || len(versions) != 0 {
		return errors.New("completion-race test unexpectedly accessed personal memory")
	}
	return nil
}

func (*conversationContextCompletionRaceTools) CloudDSHPersonalMemoryProofEnabled() bool { return true }

func (t *conversationContextCompletionRaceTools) PauseRecovery(context.Context, task.Task) error {
	return nil
}

func (t *conversationContextCompletionRaceTools) snapshot() (executions, reconciles int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.executions, t.reconciles
}

type conversationContextCompletionGate struct {
	inner   CloudDSHCompleter
	started chan app.CloudDSHTaskCompletion
	release chan struct{}
	mu      sync.Mutex
	calls   []app.CloudDSHTaskCompletion
}

func (c *conversationContextCompletionGate) Complete(ctx context.Context, value task.Task,
	completion app.CloudDSHTaskCompletion) error {
	c.mu.Lock()
	c.calls = append(c.calls, completion)
	callNumber := len(c.calls)
	c.mu.Unlock()
	if callNumber == 1 {
		select {
		case c.started <- completion:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.inner.Complete(ctx, value, completion)
}

type conversationContextSteerRouteClassifier struct{}

func (conversationContextSteerRouteClassifier) ResolveConversationRouteModel(_ context.Context,
	claim app.ConversationRouteClaim) (string, error) {
	if claim.Input.ResultReviewID != "" {
		return "p1234567", nil
	}
	if claim.Input.ModelID == "" {
		return "", errors.New("route has no model id")
	}
	return claim.Input.ModelID, nil
}

func (conversationContextSteerRouteClassifier) ClassifyConversationRoute(_ context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	if value.ResultReview != nil {
		replies := make([]string, 0, len(value.ResultReview.Sources))
		for _, source := range value.ResultReview.Sources {
			switch source.Content {
			case "The revised answer includes BLUE.", conversationContextCompletionIndependentResult:
				replies = append(replies, source.Content)
			}
		}
		if len(replies) == 0 {
			replies = append(replies, "The revised answer includes BLUE.")
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: strings.Join(replies, " ")}, nil
	}
	if strings.Contains(claim.Message.Content, conversationContextCompletionIndependentContent) {
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}, nil
	}
	if claim.Input.ReferenceTaskID == "" && value.ReferenceTaskID == "" {
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}, nil
	}
	for _, candidate := range value.Candidates {
		if candidate.TaskID == value.ReferenceTaskID && candidate.Status == task.StatusRunning {
			return app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: candidate.TaskID,
				ExpectedRevision: candidate.Revision}, nil
		}
	}
	return app.ConversationRouteDecision{}, errors.New("referenced running Task is missing from route context")
}

func (c conversationContextScopedRuntimeControl) ListAccountAgentRuntimeStopCandidates(ctx context.Context, limit int) ([]app.AccountAgentRuntimeStopIdentity, error) {
	candidates, err := c.CloudDSHAccountRuntimeControl.ListAccountAgentRuntimeStopCandidates(ctx, limit)
	if err != nil {
		return nil, err
	}
	scoped := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Fence.LeaseOwner == c.leaseOwner {
			scoped = append(scoped, candidate)
		}
	}
	return scoped, nil
}

func (c conversationContextScopedRuntimeControl) ListPendingAccountAgentRuntimeStopAttestations(ctx context.Context, limit int,
	verifier app.AccountAgentRuntimeStopVerifier) ([]app.AccountAgentRuntimeStopAttestation, error) {
	attestations, err := c.CloudDSHAccountRuntimeControl.ListPendingAccountAgentRuntimeStopAttestations(ctx, limit, verifier)
	if err != nil {
		return nil, err
	}
	scoped := attestations[:0]
	for _, attestation := range attestations {
		if attestation.Fence.LeaseOwner == c.leaseOwner {
			scoped = append(scoped, attestation)
		}
	}
	return scoped, nil
}

func TestCloudDSHCompletionRaceRecoversPendingSteerOnSameSession(t *testing.T) {
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
		t.Fatalf("verify session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'completion race test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	initialConversation, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
		Title: "Completion race", IdempotencyKey: "completion-gate-conversation", ServiceInstanceID: serviceInstanceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	const clientMessageID = "completion-gate-initial"
	initialMessage, replayed, err := tasks.AcceptConversationMessage(ctx, principal, initialConversation.ID,
		app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: clientMessageID,
			Content: "Start a new task to answer briefly.", ModelID: "p1234567",
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 2000}, IdempotencyKey: clientMessageID})
	if err != nil || replayed {
		t.Fatalf("accept initial completion-race message: replayed=%t err=%v", replayed, err)
	}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: store, Actions: store,
		Models: conversationContextSteerRouteClassifier{}, WorkerID: "completion-gate-route-worker",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("create completion-race route worker: %v", err)
	}
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("route initial completion-race message: found=%t err=%v", found, err)
	}
	var initialTaskID string
	if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialMessage.ID).Scan(&initialTaskID); err != nil || initialTaskID == "" {
		t.Fatalf("initial route did not create a Task: id=%q err=%v", initialTaskID, err)
	}
	initialTask, err := store.GetTask(ctx, initialTaskID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	submitted := app.TaskSubmissionResult{Conversation: initialConversation, Message: initialMessage, Task: initialTask.Task}
	accountRuntimeControl, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real account Runtime subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	accountScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(accountScript); err != nil {
		t.Fatalf("locate real morde-cloud account Runtime entrypoint: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const supervisorID = "completion-gate-supervisor"
	var supervisorStopKey [32]byte
	supervisorStopKey[0] = 1
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorAuth, err := app.NewAgentStopAuthenticator(supervisorID, supervisorStopKey[:])
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
	supervisor := &f01RecoveringNodeAccountSupervisor{
		f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: accountScript},
		recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
	}
	defer func() {
		if command := supervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	runtimeManager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: accountRuntimeControl, Supervisor: supervisor, Guardian: &f01SignedNodeTaskGuardian{key: privateKey},
		StopVerifier: runtimeStopVerifier, TaskStopper: instances, LeaseOwner: "completion-gate-runtime", MaxTasks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(store, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	completionGate := &conversationContextCompletionGate{inner: toolBroker,
		started: make(chan app.CloudDSHTaskCompletion, 1), release: make(chan struct{})}
	independentModelStarted := make(chan struct{}, 1)
	independentModelRelease := make(chan struct{})
	model := &conversationContextCompletionRaceModel{independentStarted: independentModelStarted,
		independentContinue: independentModelRelease}
	tools := &conversationContextCompletionRaceTools{}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: supervisorID, SummarySealer: sealer, AccountRuntimes: runtimeManager, Instances: instances,
		Checkpoints: checkpoints, Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: tools, Completion: completionGate,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskWorker, err := NewTaskWorker(TaskWorkerOptions{Store: store, Executor: executor, WorkerID: "completion-gate-worker",
		LeaseDuration: 2 * time.Second, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1,
		Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	defer func() {
		stopWorker()
		select {
		case runErr := <-workerDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Errorf("TaskWorker.Run() = %v, want context canceled during cleanup", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()

	var firstCompletion app.CloudDSHTaskCompletion
	select {
	case firstCompletion = <-completionGate.started:
	case <-ctx.Done():
		t.Fatalf("old Task completion did not reach its final gate: %v", ctx.Err())
	}
	if firstCompletion.Result != "STALE COMPLETION RESULT" {
		t.Fatalf("final gate received %q, want stale pre-steer answer", firstCompletion.Result)
	}
	firstTask, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || firstTask.Task.Status != task.StatusRunning || firstTask.Task.Result != "" || firstTask.Task.AgentSessionID == "" {
		t.Fatalf("old completion escaped before pending-input gate: status=%q result=%q session=%q err=%v",
			firstTask.Task.Status, firstTask.Task.Result, firstTask.Task.AgentSessionID, err)
	}
	var staleReceipts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'
AND strpos(content,$2)>0`, submitted.Conversation.ID, "STALE COMPLETION RESULT").Scan(&staleReceipts); err != nil || staleReceipts != 0 {
		t.Fatalf("stale assistant receipt escaped the completion gate: count=%d err=%v", staleReceipts, err)
	}
	firstSessionID := firstTask.Task.AgentSessionID
	independentMessage, replayed, err := tasks.AcceptConversationMessage(ctx, principal, submitted.Conversation.ID,
		app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: "completion-gate-independent",
			PreviousClientMessageID: clientMessageID, Content: conversationContextCompletionIndependentContent, ModelID: submitted.Task.ModelID,
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 2000}, IdempotencyKey: "completion-gate-independent"})
	if err != nil || replayed {
		t.Fatalf("accept independent request during completion gate: replayed=%t err=%v", replayed, err)
	}
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("route independent request during completion gate: found=%t err=%v", found, err)
	}
	var independentTaskID, independentStatus, routeState, routeFailure, messageDeliveryStatus string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(task.id,''),COALESCE(task.status,''),route.state,route.failure_reason,
message.delivery_status
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
LEFT JOIN tasks task ON task.id=message.task_id WHERE message.id=$1`,
		independentMessage.ID).Scan(&independentTaskID, &independentStatus, &routeState, &routeFailure, &messageDeliveryStatus); err != nil ||
		independentTaskID == "" || independentTaskID == submitted.Task.ID || independentStatus != string(task.StatusQueued) {
		t.Fatalf("independent request did not remain a separate queued Task: task=%q status=%q route=%q/%q delivery=%q err=%v",
			independentTaskID, independentStatus, routeState, routeFailure, messageDeliveryStatus, err)
	}
	model.mu.Lock()
	model.independentTaskID = independentTaskID
	model.mu.Unlock()
	steer, replayed, err := tasks.AcceptConversationMessage(ctx, principal, submitted.Conversation.ID,
		app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: "completion-gate-steer",
			PreviousClientMessageID: "completion-gate-independent", ReferenceTaskID: submitted.Task.ID,
			Content: conversationContextCompletionSteer, ModelID: submitted.Task.ModelID,
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 2000}, IdempotencyKey: "completion-gate-steer"})
	if err != nil || replayed {
		t.Fatalf("accept completion-race steer: replayed=%t err=%v", replayed, err)
	}
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("route completion-race steer: found=%t err=%v", found, err)
	}
	var deliveryStatus, deliveryMode string
	if err := pool.QueryRow(ctx, `SELECT delivery_status,delivery_mode FROM conversation_messages WHERE id=$1`, steer.ID).
		Scan(&deliveryStatus, &deliveryMode); err != nil || deliveryStatus != "pending" || deliveryMode != string(app.AgentMessageDeliverySteer) {
		t.Fatalf("steer was not pending at completion gate: status=%q mode=%q err=%v", deliveryStatus, deliveryMode, err)
	}
	close(completionGate.release)

	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		completed, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Task.Status == task.StatusCompleted {
			if completed.Task.Result != "The revised answer includes BLUE." || completed.Task.AgentSessionID != firstSessionID {
				t.Fatalf("recovered completion = status %q result %q session %q; want revised result on same Session %q",
					completed.Task.Status, completed.Task.Result, completed.Task.AgentSessionID, firstSessionID)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Task did not recover and complete: %v", ctx.Err())
		case <-deadline.C:
			t.Fatalf("Task did not recover and complete: status=%q result=%q", completed.Task.Status, completed.Task.Result)
		case <-ticker.C:
		}
	}
	select {
	case <-independentModelStarted:
	case <-ctx.Done():
		t.Fatalf("independent Task did not start after the first Task completed: %v", ctx.Err())
	}
	independentTask, err := store.GetTask(ctx, independentTaskID, owner.ID)
	if err != nil || independentTask.Task.Status != task.StatusRunning || independentTask.Task.ID == submitted.Task.ID {
		t.Fatalf("independent Task state while its model call is held: task=%+v err=%v", independentTask.Task, err)
	}
	select {
	case <-time.After(conversationContextCompletionResultWindow):
	case <-ctx.Done():
		t.Fatalf("wait for the independent result aggregation window: %v", ctx.Err())
	}
	if _, err := store.QueueReadyConversationResultReviews(ctx, 100); err != nil {
		t.Fatalf("queue completed Task result for conversation delivery: %v", err)
	}
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("publish revised conversation result: found=%t err=%v", found, err)
	}
	modelCalls := model.snapshot()
	var thirdWriteResult string
	thirdToolResultFound := false
	if len(modelCalls) >= 3 {
		thirdWriteResult, thirdToolResultFound = f01ToolResult(modelCalls[2].RawRequest, conversationContextCompletionWriteCallID)
	}
	if len(modelCalls) != 4 || modelCalls[0].Task == nil || modelCalls[1].Task == nil || modelCalls[2].Task == nil || modelCalls[3].Task == nil ||
		modelCalls[0].Task.AgentSessionID != firstSessionID || modelCalls[1].Task.AgentSessionID != firstSessionID || modelCalls[2].Task.AgentSessionID != firstSessionID ||
		modelCalls[3].Task.TaskID != independentTaskID || modelCalls[3].Task.AgentSessionID == firstSessionID ||
		!strings.Contains(string(modelCalls[2].RawRequest), conversationContextCompletionSteer) || !thirdToolResultFound ||
		!strings.Contains(thirdWriteResult, conversationContextCompletionWriteOutput) {
		t.Fatalf("Task recovery did not reuse the DSH Session and apply steer: model calls=%d third_result_found=%t third_result=%s",
			len(modelCalls), thirdToolResultFound, thirdWriteResult)
	}
	if executions, reconciles := tools.snapshot(); executions != 1 {
		t.Fatalf("recovery repeated or skipped the side effect: executions=%d reconciliations=%d", executions, reconciles)
	}
	completionGate.mu.Lock()
	completionCalls := append([]app.CloudDSHTaskCompletion(nil), completionGate.calls...)
	completionGate.mu.Unlock()
	if len(completionCalls) != 2 || completionCalls[0].Result != "STALE COMPLETION RESULT" ||
		completionCalls[1].Result != "The revised answer includes BLUE." {
		t.Fatalf("completion deliveries = %#v; want stale attempt rejected, then revised result committed", completionCalls)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, steer.ID).Scan(&deliveryStatus); err != nil || deliveryStatus != "applied" {
		t.Fatalf("recovered Session did not claim the steer: status=%q err=%v", deliveryStatus, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'
AND strpos(content,$2)>0`, submitted.Conversation.ID, "STALE COMPLETION RESULT").Scan(&staleReceipts); err != nil || staleReceipts != 0 {
		t.Fatalf("stale assistant receipt was committed during recovery: count=%d err=%v", staleReceipts, err)
	}
	var revisedReceipts int
	if submitted.Task.CurrentRevision == nil {
		t.Fatal("completion-race Task has no current revision")
	}
	expectedReceiptStatus := fmt.Sprintf("- %s r%d：pending", submitted.Task.ID, *submitted.Task.CurrentRevision)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'
AND strpos(content,$2)>0
AND strpos(content,'结果摘要（主助手整理，不代表独立验收）：')>0
AND strpos(content,'Task 执行状态：completed')>0
AND strpos(content,'服务端验收状态（当前修订）：')>0
AND strpos(content,$3)>0`, submitted.Conversation.ID, "The revised answer includes BLUE.", expectedReceiptStatus).
		Scan(&revisedReceipts); err != nil || revisedReceipts != 1 {
		t.Fatalf("revised assistant result receipt count=%d err=%v; want exactly one wrapped receipt with Task status", revisedReceipts, err)
	}
	var publishedSources int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_sources WHERE task_id=$1 AND state='published'`,
		submitted.Task.ID).Scan(&publishedSources); err != nil || publishedSources != 1 {
		t.Fatalf("published revised result sources=%d err=%v; want one source for the Task", publishedSources, err)
	}
	close(independentModelRelease)
	independentDeadline := time.NewTimer(45 * time.Second)
	defer independentDeadline.Stop()
	for {
		completed, err := store.GetTask(ctx, independentTaskID, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Task.Status == task.StatusCompleted {
			if completed.Task.Result != conversationContextCompletionIndependentResult || completed.Task.ID == submitted.Task.ID {
				t.Fatalf("independent Task result = status %q result %q; want its own completed result",
					completed.Task.Status, completed.Task.Result)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("independent Task did not complete: %v", ctx.Err())
		case <-independentDeadline.C:
			t.Fatalf("independent Task did not complete: status=%q result=%q", completed.Task.Status, completed.Task.Result)
		case <-ticker.C:
		}
	}
	select {
	case <-time.After(conversationContextCompletionResultWindow):
	case <-ctx.Done():
		t.Fatalf("wait for the independent Task result aggregation window: %v", ctx.Err())
	}
	if _, err := store.QueueReadyConversationResultReviews(ctx, 100); err != nil {
		t.Fatalf("queue independent Task result for conversation delivery: %v", err)
	}
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("publish independent Task result: found=%t err=%v", found, err)
	}
	var independentReceipts, independentPublishedSources int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'
AND strpos(content,$2)>0`, submitted.Conversation.ID, conversationContextCompletionIndependentResult).Scan(&independentReceipts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_sources WHERE task_id=$1 AND state='published'`,
		independentTaskID).Scan(&independentPublishedSources); err != nil || independentReceipts != 1 || independentPublishedSources != 1 {
		t.Fatalf("independent result delivery receipts=%d published sources=%d err=%v; want one each",
			independentReceipts, independentPublishedSources, err)
	}
}

func TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real DSH subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	modelClient := &conversationContextE2EModelClient{}
	modelKey, err := sealer.Seal([]byte("local-synthetic-e2e-key"))
	if err != nil {
		t.Fatal(err)
	}
	// Three sub-preview-limit sources cross the 75% summary trigger without making an individual route message incomplete.
	window := 8_192
	model, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{ID: "p1234567", OwnerID: owner.ID,
		Name: "Local conversation context E2E", BaseURL: "https://models.example.invalid/v1", ModelName: "synthetic-e2e",
		ContextWindowTokens: &window, ContextWindowOverrideTokens: &window, ContextWindowSource: app.ModelContextWindowLegacyManual, APIKeyCipher: modelKey})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	model.Capabilities.Text, model.CheckedAt = true, &checkedAt
	if _, err := store.UpdateModelConnection(ctx, model); err != nil {
		t.Fatal(err)
	}
	connectionService, err := app.NewModelConnectionService(store, sealer, modelClient, []string{"models.example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, connectionService, modelClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
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
	credential := createF01Session(t, ctx, store, owner.ID)
	principal, err := credentials.VerifySession(ctx, credential)
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify local session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	mainConversation, _, err := conversations.EnsureMain(ctx, principal, app.EnsureMainConversationCommand{IdempotencyKey: "context-e2e-main"})
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conversationIDs := []string{mainConversation.ConversationID}
	for _, suffix := range []string{"one", "two"} {
		value, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
			Title: "Context E2E side chat " + suffix, IdempotencyKey: "context-e2e-side-" + suffix,
			ServiceInstanceID: serviceInstanceID,
		})
		if err != nil {
			t.Fatal(err)
		}
		conversationIDs = append(conversationIDs, value.ID)
	}
	accountRuntimes, err := app.NewAccountAgentRuntimeService(store)
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
	checkpoints, err := app.NewConversationCoordinatorCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	supervisors := make(map[string]*f01RecoveringNodeAccountSupervisor, len(conversationIDs)+1)
	managerRunCancels := make(map[string]context.CancelFunc, len(conversationIDs)+1)
	managerRunDone := make(map[string]<-chan struct{}, len(conversationIDs)+1)
	var managerRunWG sync.WaitGroup
	managerRunErrors := make(chan error, len(conversationIDs)+1)
	defer func() {
		for _, cancelManagerRun := range managerRunCancels {
			cancelManagerRun()
		}
		managerRunWG.Wait()
		close(managerRunErrors)
		for err := range managerRunErrors {
			t.Errorf("account Agent Runtime manager failed: %v", err)
		}
		for _, supervisor := range supervisors {
			if command := supervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}
	}()
	stopManagerRun := func(leaseOwner string) {
		if cancelManagerRun := managerRunCancels[leaseOwner]; cancelManagerRun != nil {
			cancelManagerRun()
			if done := managerRunDone[leaseOwner]; done != nil {
				<-done
			}
		}
	}
	makeCoordinator := func(conversationID, leaseOwner string) *CloudDSHConversationCoordinator {
		t.Helper()
		supervisor := &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: script},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
		supervisors[leaseOwner] = supervisor
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control:    conversationContextScopedRuntimeControl{CloudDSHAccountRuntimeControl: accountRuntimes, leaseOwner: leaseOwner},
			Supervisor: supervisor, Guardian: &f01NodeTaskGuardian{}, StopVerifier: stopVerifier,
			TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: leaseOwner, MaxTasks: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		managerRunCtx, cancelManagerRun := context.WithCancel(ctx)
		managerRunCancels[leaseOwner] = cancelManagerRun
		done := make(chan struct{})
		managerRunDone[leaseOwner] = done
		managerRunWG.Add(1)
		go func() {
			defer managerRunWG.Done()
			defer close(done)
			if err := manager.Run(managerRunCtx); managerRunCtx.Err() == nil {
				if err == nil {
					err = errors.New("manager stopped before fixture cleanup")
				}
				managerRunErrors <- fmt.Errorf("run account Agent Runtime manager %s: %w", leaseOwner, err)
			}
		}()
		coordinator, err := NewCloudDSHConversationCoordinator(CloudDSHConversationCoordinatorOptions{
			AccountRuntimes: manager, Models: gateway,
			Checkpoints: checkpoints, SummaryStore: store,
		})
		if err != nil {
			t.Fatal(err)
		}
		return coordinator
	}
	coordinators := make(map[string]*CloudDSHConversationCoordinator, len(conversationIDs))
	for index, conversationID := range conversationIDs {
		coordinators[conversationID] = makeCoordinator(conversationID, fmt.Sprintf("context-e2e-runtime-%d", index))
	}
	routeModels := &conversationContextE2ERouteModels{gateway: gateway, coordinators: coordinators}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: routeModels, SummaryStore: store, SummaryInputs: store, SummarySealer: sealer,
		WorkerID: "context-e2e-route-worker", LeaseDuration: 2 * time.Minute, PollInterval: time.Millisecond,
		MaxConcurrent: 4, Logger: zap.NewExample(),
	})
	if err != nil {
		t.Fatal(err)
	}
	lastClientMessageIDs := make(map[string]string, len(conversationIDs))
	acceptAndRoute := func(conversationID, content, clientID string) string {
		t.Helper()
		message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, conversationID, app.AcceptConversationMessageCommand{
			ServiceInstanceID: serviceInstanceID, ClientMessageID: clientID, IdempotencyKey: clientID,
			Content: content, PreviousClientMessageID: lastClientMessageIDs[conversationID], ModelID: model.ID,
			Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		})
		if err != nil || replayed {
			t.Fatalf("accept conversation message %q: replayed=%t err=%v", clientID, replayed, err)
		}
		found, err := routeWorker.RunOnce(ctx)
		if err != nil || !found {
			t.Fatalf("route conversation message %q: found=%t err=%v", clientID, found, err)
		}
		var state, failureReason string
		if err := pool.QueryRow(ctx, `SELECT state,failure_reason FROM conversation_message_routes WHERE message_id=$1`, message.ID).
			Scan(&state, &failureReason); err != nil || state != "routed" {
			t.Fatalf("message %q route state=%q failure_reason=%q err=%v", clientID, state, failureReason, err)
		}
		lastClientMessageIDs[conversationID] = clientID
		return message.ID
	}
	for chatIndex, conversationID := range conversationIDs {
		previousVersion := int64(0)
		for cycle := 1; cycle <= 3; cycle++ {
			for source := 1; source <= 3; source++ {
				prefix := fmt.Sprintf("E2E source chat=%d cycle=%d source=%d confirmed durable fact. ", chatIndex, cycle, source)
				content := prefix + strings.Repeat("x", 2040-len(prefix))
				acceptAndRoute(conversationID, content, fmt.Sprintf("context-source-%d-%d-%d", chatIndex, cycle, source))
			}
			acceptAndRoute(conversationID, fmt.Sprintf("E2E_COMPACTION_TRIGGER chat=%d cycle=%d", chatIndex, cycle),
				fmt.Sprintf("context-compact-%d-%d", chatIndex, cycle))
			version := conversationContextE2ESummaryVersion(t, ctx, pool, serviceInstanceID, owner.ID, conversationID)
			if version < int64(cycle) || version <= previousVersion {
				t.Fatalf("conversation %s compaction cycle %d produced summary version %d after %d; want an advancing summary",
					conversationID, cycle, version, previousVersion)
			}
			previousVersion = version
		}
	}
	summaryHeadsBeforeHistory := make(map[string]int64, len(conversationIDs))
	for _, conversationID := range conversationIDs {
		summaryHeadsBeforeHistory[conversationID] = conversationContextE2ESummaryVersion(t, ctx, pool,
			serviceInstanceID, owner.ID, conversationID)
	}
	var oldMainFenceRuntimeID string
	mainSupervisor := supervisors["context-e2e-runtime-0"]
	mainManager := coordinators[mainConversation.ConversationID].runtimes.(cloudDSHCoordinatorAccountRuntimeManager).manager
	mainSession, err := mainManager.Acquire(
		ctx, owner.ID, mainConversation.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	oldMainFence := mainSession.runtime.Fence
	oldMainFenceRuntimeID = oldMainFence.RuntimeID
	oldCommand := mainSupervisor.cmd
	if oldCommand == nil || oldCommand.Process == nil {
		t.Fatal("main conversation did not start a real Node account Runtime")
	}
	if err := oldCommand.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("simulate main DSH Runtime process restart: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- oldCommand.Wait() }()
	select {
	case waitErr := <-waitDone:
		if waitErr == nil || oldCommand.ProcessState == nil || oldCommand.ProcessState.ExitCode() != -1 {
			t.Fatalf("wait for stopped main DSH Runtime: %v", waitErr)
		}
	case <-ctx.Done():
		t.Fatalf("main DSH Runtime did not stop: %v", ctx.Err())
	}
	for deadline := time.Now().Add(2 * time.Second); !mainSession.closed() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !mainSession.closed() {
		t.Fatal("main account Runtime manager did not observe the stopped DSH process")
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, oldMainFence.ServiceInstanceID, owner.ID, oldMainFence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	stopped := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		current, found, err := accountRuntimes.Get(ctx, mainConversation.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if found && current.Fence == oldMainFence && current.State == app.AccountAgentRuntimeStopped {
			stopped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("old main account Runtime was not reaped by its lifecycle manager")
	}
	stopManagerRun("context-e2e-runtime-0")
	var bulkMessageIDs []string
	for index := 0; index < 840; index++ {
		prefix := fmt.Sprintf("Archive history row=%04d ", index)
		if index == 100 {
			prefix += "ORCHID-ARCHIVE-SENTINEL-8472 "
		}
		content := prefix + strings.Repeat("q", 10*1024-len(prefix))
		clientID := fmt.Sprintf("context-history-%04d", index)
		message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, mainConversation.ConversationID,
			app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: clientID, IdempotencyKey: clientID,
				Content: content, PreviousClientMessageID: lastClientMessageIDs[mainConversation.ConversationID], ModelID: model.ID,
				Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}})
		if err != nil || replayed {
			t.Fatalf("seed authorized long-history message %d: replayed=%t err=%v", index, replayed, err)
		}
		bulkMessageIDs = append(bulkMessageIDs, message.ID)
		lastClientMessageIDs[mainConversation.ConversationID] = clientID
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied',
created_at=clock_timestamp()-interval '1 hour' WHERE id=ANY($1::text[])`, bulkMessageIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET state='routed' WHERE message_id=ANY($1::text[])`, bulkMessageIDs); err != nil {
		t.Fatal(err)
	}
	var linkedHistoryRows int
	if err := pool.QueryRow(ctx, `WITH RECURSIVE chain(client_message_id,previous_id,depth) AS (
 SELECT client_message_id,previous_client_message_id,1 FROM conversation_message_routes
 WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$2 AND conversation_id=$3
  AND client_message_id='context-history-0839'
 UNION ALL
 SELECT r.client_message_id,r.previous_client_message_id,c.depth+1 FROM chain c
 JOIN conversation_message_routes r ON r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$2
  AND r.conversation_id=$3 AND r.client_message_id=c.previous_id WHERE c.depth<900
)
SELECT count(*) FILTER (WHERE client_message_id LIKE 'context-history-%') FROM chain`,
		serviceInstanceID, owner.ID, mainConversation.ConversationID).Scan(&linkedHistoryRows); err != nil || linkedHistoryRows != 840 {
		t.Fatalf("linked predecessor history rows=%d, want 840; err=%v", linkedHistoryRows, err)
	}
	var historyBytes int64
	if err := pool.QueryRow(ctx, `SELECT sum(octet_length(content)) FROM conversation_messages WHERE conversation_id=$1`, mainConversation.ConversationID).Scan(&historyBytes); err != nil {
		t.Fatal(err)
	}
	if historyBytes <= 8*1024*1024 {
		t.Fatalf("main conversation history is only %d bytes; want more than the 8 MiB restore threshold", historyBytes)
	}
	newMainCoordinator := makeCoordinator(mainConversation.ConversationID, "context-e2e-runtime-main-restarted")
	routeModels.coordinators[mainConversation.ConversationID] = newMainCoordinator
	mainManager = newMainCoordinator.runtimes.(cloudDSHCoordinatorAccountRuntimeManager).manager
	historyTaskMessageID := acceptAndRoute(mainConversation.ConversationID,
		conversationContextE2EHistoryTaskContent, "context-final-task-main")
	for index, conversationID := range conversationIDs[1:] {
		acceptAndRoute(conversationID, fmt.Sprintf("E2E_SIDE_CHAT_CONTINUES side=%d", index+1), fmt.Sprintf("context-side-after-restart-%d", index+1))
	}
	var expectedCompactions int
	var mainSummaryVersion int64
	for _, conversationID := range conversationIDs {
		got := conversationContextE2ESummaryVersion(t, ctx, pool, serviceInstanceID, owner.ID, conversationID)
		if conversationID == mainConversation.ConversationID {
			if got != summaryHeadsBeforeHistory[conversationID]+1 {
				t.Fatalf("main conversation summary head=%d, want one compaction after prior head %d",
					got, summaryHeadsBeforeHistory[conversationID])
			}
			mainSummaryVersion = got
		} else if got < summaryHeadsBeforeHistory[conversationID] ||
			got > summaryHeadsBeforeHistory[conversationID]+1 {
			// Each side chat may cross its own compaction threshold on the one
			// continuation message. Its summary must remain present and may
			// advance at most once; it must not inherit the main chat's history.
			t.Fatalf("side conversation %s summary head=%d, want prior head %d or one advance",
				conversationID, got, summaryHeadsBeforeHistory[conversationID])
		}
		expectedCompactions += int(got)
	}
	currentRuntime, found, err := accountRuntimes.Get(ctx, mainConversation.ConversationID)
	if err != nil || !found || currentRuntime.Fence.RuntimeID == oldMainFenceRuntimeID || currentRuntime.Fence.RuntimeGeneration != oldMainFence.RuntimeGeneration+1 {
		t.Fatalf("main account Runtime was not recovered after restart: runtime=%+v found=%t err=%v", currentRuntime, found, err)
	}
	if _, err := accountRuntimes.Renew(ctx, oldMainFence, app.MaxAgentLease); err == nil {
		t.Fatal("old main account Runtime fence remained usable after restart")
	}
	var settledCompactions, committedOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='settled'), count(*)
FROM model_call_ledger WHERE owner_id=$1 AND source='compaction'`, owner.ID).Scan(&settledCompactions, &committedOperations); err != nil || settledCompactions != expectedCompactions {
		t.Fatalf("DSH compaction model calls settled=%d, want %d; err=%v", settledCompactions, expectedCompactions, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_compaction_operations WHERE owner_id=$1 AND state='committed'`, owner.ID).Scan(&committedOperations); err != nil || committedOperations != expectedCompactions {
		t.Fatalf("committed DSH compaction operations=%d, want %d; err=%v", committedOperations, expectedCompactions, err)
	}
	var retainedResponses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_compaction_operations
WHERE owner_id=$1 AND state='committed' AND octet_length(response_ciphertext)>0`, owner.ID).Scan(&retainedResponses); err != nil || retainedResponses != 0 {
		t.Fatalf("committed compaction operations retain %d temporary model responses: %v", retainedResponses, err)
	}
	modelClient.mu.Lock()
	summaryCalls, routeCalls := modelClient.summaryCalls, modelClient.routeCalls
	modelClient.mu.Unlock()
	if summaryCalls != expectedCompactions || routeCalls != 39 {
		t.Fatalf("synthetic model calls: summaries=%d routes=%d, want %d summaries and 39 routes", summaryCalls, routeCalls, expectedCompactions)
	}
	var claimed task.Task
	claimed, found, err = store.ClaimNextTask(ctx, "context-e2e-history-task", time.Minute)
	if err != nil || !found || claimed.ConversationID != mainConversation.ConversationID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.Summary == nil || claimed.ConversationContext.Summary.Version != mainSummaryVersion {
		t.Fatalf("new Task did not receive the latest fixed summary: found=%t task=%+v err=%v", found, claimed, err)
	}
	var routedTaskID string
	if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, historyTaskMessageID).Scan(&routedTaskID); err != nil || routedTaskID != claimed.ID {
		t.Fatalf("authorized-history follow-up message was not linked to the claimed Task: linked=%q task=%q err=%v", routedTaskID, claimed.ID, err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close local Workspace blocks: %v", err)
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
	toolBroker, err := app.NewCloudDSHToolBroker(conversationContextE2ETaskStore{store}, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := toolBroker.Definitions(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	historyToolFound := false
	for _, definition := range definitions {
		historyToolFound = historyToolFound || definition.Name == "morde_current_conversation_history_search"
	}
	if !historyToolFound {
		t.Fatal("fixed-context Task did not expose the authorized original-history search tool")
	}
	stopKey := make([]byte, 32)
	stopKey[0] = 1
	stopAuthenticator, err := app.NewAgentStopAuthenticator("context-e2e-runtime-main-restarted", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	checkpointsForTask, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointObserver := &conversationContextE2ECheckpointObserver{
		CloudDSHCheckpoints: checkpointsForTask, targetSeen: make(chan struct{}),
	}
	const historyCallID = "context-e2e-history-agent-search"
	const historyAnswer = "此前消息中的归档标记是 ORCHID-ARCHIVE-SENTINEL-8472。"
	const historySteeredAnswer = historyAnswer + " 已按补充要求加入简短引言。"
	modelCalls := 0
	blockedModelCallStarted := make(chan struct{})
	resumeBlockedModelCall := make(chan struct{})
	var resumeBlockedModelCallOnce sync.Once
	resumeBlockedModel := func() { resumeBlockedModelCallOnce.Do(func() { close(resumeBlockedModelCall) }) }
	defer resumeBlockedModel()
	taskModels := &cloudDSHTestModels{complete: func(callCtx context.Context, call app.ModelCall) (app.ModelResponse, error) {
		modelCalls++
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
			return app.ModelResponse{}, fmt.Errorf("decode long-history DSH request: %w", err)
		}
		usage := app.ModelUsage{Known: true, InputTokens: 96, OutputTokens: 32}
		if modelCalls == 1 {
			searchOffered := false
			for _, definition := range request.Tools {
				searchOffered = searchOffered || definition.Name == "morde_current_conversation_history_search"
			}
			if !searchOffered || strings.Contains(string(call.RawRequest), "ORCHID-ARCHIVE-SENTINEL-8472") {
				return app.ModelResponse{}, errors.New("long-history request lacked the authorized search tool or contained the old fact already")
			}
			close(blockedModelCallStarted)
			select {
			case <-resumeBlockedModelCall:
			case <-callCtx.Done():
				return app.ModelResponse{}, callCtx.Err()
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: historyCallID, Name: "morde_current_conversation_history_search",
				Arguments: `{"query":"ORCHID-ARCHIVE-SENTINEL-8472","limit":1}`}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		resultSeen := false
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != historyCallID {
				continue
			}
			for _, part := range message.Content {
				resultSeen = resultSeen || strings.Contains(part.Text, "ORCHID-ARCHIVE-SENTINEL-8472")
			}
		}
		if modelCalls == 2 {
			supersededResponseSeen := false
			for _, message := range request.Messages {
				if message.Role != "assistant" {
					continue
				}
				for _, part := range message.Content {
					supersededResponseSeen = supersededResponseSeen || strings.Contains(part.Text, "A new task input arrived during this model call.")
				}
			}
			if !supersededResponseSeen || resultSeen || !strings.Contains(string(call.RawRequest), conversationContextE2EHistorySteerContent) {
				return app.ModelResponse{}, errors.New("DSH did not discard the superseded model response before resuming with the supplement")
			}
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: historyCallID, Name: "morde_current_conversation_history_search",
				Arguments: `{"query":"ORCHID-ARCHIVE-SENTINEL-8472","limit":1}`}}, StopReason: "tool_calls", Usage: usage}, nil
		}
		if modelCalls != 3 {
			return app.ModelResponse{}, fmt.Errorf("unexpected long-history DSH model call %d", modelCalls)
		}
		if !resultSeen || !strings.Contains(string(call.RawRequest), conversationContextE2EHistorySteerContent) {
			return app.ModelResponse{}, errors.New("DSH model request omitted the authorized history result or running-task supplement")
		}
		return app.ModelResponse{Text: historySteeredAnswer, StopReason: "stop", Usage: usage}, nil
	}}
	taskExecutor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "context-e2e-runtime-main-restarted", SummarySealer: sealer, AccountRuntimes: mainManager,
		Instances: instances, Checkpoints: checkpointObserver, Models: taskModels,
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, Tools: toolBroker, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskCtx, cancelTask := context.WithCancel(ctx)
	taskDone := make(chan error, 1)
	taskDonePending := true
	defer func() {
		resumeBlockedModel()
		cancelTask()
		if taskDonePending {
			select {
			case taskErr := <-taskDone:
				if taskErr != nil && !errors.Is(taskErr, context.Canceled) {
					t.Errorf("long-history Task returned while stopping after test cleanup: %v", taskErr)
				}
			case <-time.After(15 * time.Second):
				t.Error("long-history Task did not stop after the test ended")
			}
		}
	}()
	go func() { taskDone <- taskExecutor.ExecuteTask(taskCtx, claimed) }()
	select {
	case <-blockedModelCallStarted:
	case err := <-taskDone:
		taskDonePending = false
		t.Fatalf("long-history Task exited before its first model call: %v", err)
	case <-ctx.Done():
		t.Fatalf("long-history Task did not start its first model call: %v", ctx.Err())
	}
	var taskStateBeforeSteer string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, claimed.ID).Scan(&taskStateBeforeSteer); err != nil ||
		taskStateBeforeSteer != string(task.StatusRunning) {
		t.Fatalf("long-history Task was not running during the blocked model call: status=%q err=%v", taskStateBeforeSteer, err)
	}
	steerClientMessageID := "context-history-task-steer"
	steerMessage, replayed, err := tasks.AcceptConversationMessage(ctx, principal, mainConversation.ConversationID,
		app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: steerClientMessageID,
			IdempotencyKey: steerClientMessageID, Content: conversationContextE2EHistorySteerContent,
			PreviousClientMessageID: lastClientMessageIDs[mainConversation.ConversationID], ReferenceTaskID: claimed.ID,
			ModelID: model.ID, Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}})
	if err != nil || replayed {
		t.Fatalf("accept running-task supplement: replayed=%t err=%v", replayed, err)
	}
	checkpointObserver.mu.Lock()
	checkpointObserver.targetID = steerMessage.ID
	checkpointObserver.mu.Unlock()
	modelClient.mu.Lock()
	modelClient.steerTaskID = claimed.ID
	modelClient.mu.Unlock()
	lastClientMessageIDs[mainConversation.ConversationID] = steerClientMessageID
	if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
		t.Fatalf("route running-task supplement: found=%t err=%v", found, err)
	}
	modelClient.mu.Lock()
	steerCalls := modelClient.steerCalls
	modelClient.mu.Unlock()
	var linkedTaskID, deliveryStatus, deliveryMode, routeState, taskState, routeFailure string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),message.delivery_status,message.delivery_mode,route.state,COALESCE(task.status,''),route.failure_reason
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
LEFT JOIN tasks task ON task.id=message.task_id WHERE message.id=$1`, steerMessage.ID).
		Scan(&linkedTaskID, &deliveryStatus, &deliveryMode, &routeState, &taskState, &routeFailure); err != nil ||
		linkedTaskID != claimed.ID || deliveryStatus != "pending" || deliveryMode != string(app.AgentMessageDeliverySteer) ||
		routeState != "routed" || taskState != string(task.StatusRunning) || steerCalls == 0 {
		t.Fatalf("running-task supplement route: classifier_calls=%d task=%q delivery=%q mode=%q route=%q task_state=%q failure=%q err=%v",
			steerCalls, linkedTaskID, deliveryStatus, deliveryMode, routeState, taskState, routeFailure, err)
	}
	select {
	case <-checkpointObserver.targetSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("running-task supplement was not discovered by the DSH delivery poller")
	}
	time.Sleep(100 * time.Millisecond)
	resumeBlockedModel()
	var taskErr error
	select {
	case taskErr = <-taskDone:
	case <-time.After(30 * time.Second):
		t.Fatal("long-history Task did not finish after the model call resumed")
	}
	taskDonePending = false
	if taskErr != nil {
		t.Fatalf("execute long-history Task through DSH: %v", taskErr)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, steerMessage.ID).Scan(&deliveryStatus); err != nil || deliveryStatus != "applied" {
		t.Fatalf("DSH did not checkpoint the running-task supplement; delivery status=%q err=%v", deliveryStatus, err)
	}
	if modelCalls != 3 {
		t.Fatalf("long-history model calls=%d; want three model turns including stale-call recovery", modelCalls)
	}
	details, err := tasks.Get(ctx, principal, claimed.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != historySteeredAnswer {
		t.Fatalf("long-history DSH result was not completed: status=%q result=%q err=%v",
			details.Task.Status, details.Task.Result, err)
	}
	var pages, listed, foundSentinel int
	cursor := ""
	for pages < 20 {
		page, err := conversations.ListMessages(ctx, principal, mainConversation.ConversationID, app.PageRequest{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		listed += len(page.Items)
		for _, message := range page.Items {
			if strings.Contains(message.Content, "ORCHID-ARCHIVE-SENTINEL-8472") {
				foundSentinel++
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if pages < 2 || listed < 840 || foundSentinel != 1 {
		t.Fatalf("authorized original-history pagination: pages=%d listed=%d sentinel=%d", pages, listed, foundSentinel)
	}
	concurrentRouteBarrier := &conversationContextE2ERouteBarrier{expected: 4, ready: make(chan struct{})}
	modelClient.mu.Lock()
	modelClient.routeBarrier = concurrentRouteBarrier
	modelClient.mu.Unlock()
	concurrentMessageIDs := make([]string, 0, 4)
	for index := 0; index < 4; index++ {
		clientMessageID := fmt.Sprintf("context-concurrent-route-%d", index)
		message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, mainConversation.ConversationID,
			app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: clientMessageID,
				IdempotencyKey: clientMessageID, Content: fmt.Sprintf("DSH_CONCURRENT_ROUTE_%d", index), ModelID: model.ID,
				Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}})
		if err != nil || replayed {
			t.Fatalf("accept concurrent DSH route message %d: replayed=%t err=%v", index, replayed, err)
		}
		concurrentMessageIDs = append(concurrentMessageIDs, message.ID)
	}
	routeCtx, stopRoutes := context.WithTimeout(ctx, time.Minute)
	routeDone := make(chan error, 1)
	go func() { routeDone <- routeWorker.Run(routeCtx) }()
	routeTicker := time.NewTicker(10 * time.Millisecond)
	defer routeTicker.Stop()
	for {
		var routed int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_routes WHERE message_id=ANY($1::text[]) AND state='routed'`,
			concurrentMessageIDs).Scan(&routed); err != nil {
			stopRoutes()
			t.Fatal(err)
		}
		if routed == len(concurrentMessageIDs) {
			break
		}
		select {
		case <-routeCtx.Done():
			rows, err := pool.Query(ctx, `SELECT message_id,state,failure_reason,route_attempt FROM conversation_message_routes WHERE message_id=ANY($1::text[])`, concurrentMessageIDs)
			if err != nil {
				stopRoutes()
				t.Fatal(err)
			}
			var routeStates []string
			for rows.Next() {
				var messageID, state, failureReason string
				var attempts int
				if err := rows.Scan(&messageID, &state, &failureReason, &attempts); err != nil {
					rows.Close()
					stopRoutes()
					t.Fatal(err)
				}
				routeStates = append(routeStates, fmt.Sprintf("%s:%s:%s:%d", messageID, state, failureReason, attempts))
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				stopRoutes()
				t.Fatal(err)
			}
			rows.Close()
			stopRoutes()
			modelClient.mu.Lock()
			routeCalls := modelClient.routeCalls
			modelClient.mu.Unlock()
			concurrentRouteBarrier.mu.Lock()
			barrierArrived := concurrentRouteBarrier.arrived
			concurrentRouteBarrier.mu.Unlock()
			t.Fatalf("concurrent DSH messages routed before timeout: %d/%d, model calls=%d, barrier=%d, states=%v",
				routed, len(concurrentMessageIDs), routeCalls, barrierArrived, routeStates)
		case <-routeTicker.C:
		}
	}
	stopRoutes()
	if err := <-routeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop concurrent conversation route worker: %v", err)
	}
	concurrentRouteBarrier.mu.Lock()
	barrierArrived := concurrentRouteBarrier.arrived
	concurrentRouteBarrier.mu.Unlock()
	if barrierArrived < len(concurrentMessageIDs) {
		t.Fatalf("only %d of %d same-conversation DSH routes reached the model barrier", barrierArrived, len(concurrentMessageIDs))
	}
}

const conversationContextE2EHistoryTaskContent = "E2E_ROUTE_TO_TASK: find the earlier archive marker and report its value"
const conversationContextE2EHistorySteerContent = "E2E_STEER_SUPPLEMENT: include a brief introduction"

type conversationContextE2EModelClient struct {
	mu           sync.Mutex
	summaryCalls int
	routeCalls   int
	routeBarrier *conversationContextE2ERouteBarrier
	steerTaskID  string
	steerCalls   int
}

type conversationContextE2ERouteBarrier struct {
	mu       sync.Mutex
	expected int
	arrived  int
	ready    chan struct{}
}

func (b *conversationContextE2ERouteBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.expected {
		close(b.ready)
	}
	ready := b.ready
	b.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *conversationContextE2EModelClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	var currentMessage string
	for _, input := range request.Input {
		candidates := append([]string{input.Text}, make([]string, 0, len(input.Parts))...)
		for _, part := range input.Parts {
			candidates = append(candidates, part.Text)
		}
		for _, candidate := range candidates {
			var routeInput struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(candidate), &routeInput) == nil && routeInput.Message != "" {
				currentMessage = routeInput.Message
			}
		}
	}
	response := `{"action":"reply","reply":"已收到，继续保留在当前会话。","acknowledge":false}`
	var routeBarrier *conversationContextE2ERouteBarrier
	c.mu.Lock()
	if currentMessage == "" {
		c.summaryCalls++
		response = "持续会话摘要：保留已确认资料、决策与约束，后续继续从当前消息推进。"
	} else {
		c.routeCalls++
		if strings.Contains(currentMessage, "DSH_CONCURRENT_ROUTE_") {
			routeBarrier = c.routeBarrier
		}
		if currentMessage == conversationContextE2EHistoryTaskContent {
			response = `{"action":"new","file_delivery_intent":"none","acknowledge":false,"explicit_new":false}`
		}
		if currentMessage == conversationContextE2EHistorySteerContent && c.steerTaskID != "" {
			c.steerCalls++
			response = fmt.Sprintf(`{"action":"steer","task_id":%q}`, c.steerTaskID)
		}
	}
	c.mu.Unlock()
	if routeBarrier != nil {
		if err := routeBarrier.wait(ctx); err != nil {
			return app.ModelResponse{}, err
		}
	}
	return app.ModelResponse{Text: response, Usage: app.ModelUsage{InputTokens: 128, OutputTokens: 32, Known: true}, StopReason: "stop"}, nil
}

func (*conversationContextE2EModelClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true}, nil
}

type conversationContextE2ERouteModels struct {
	gateway      *app.ModelGatewayService
	coordinators map[string]*CloudDSHConversationCoordinator
}

type conversationContextE2ETaskStore struct{ *storepostgres.Store }

type conversationContextE2ECheckpointObserver struct {
	CloudDSHCheckpoints
	mu         sync.Mutex
	targetID   string
	targetSeen chan struct{}
	once       sync.Once
}

func (o *conversationContextE2ECheckpointObserver) PendingMessages(ctx context.Context,
	fence app.AgentCheckpointFence) ([]app.AgentConversationMessage, error) {
	messages, err := o.CloudDSHCheckpoints.PendingMessages(ctx, fence)
	if err != nil {
		return messages, err
	}
	o.mu.Lock()
	targetID := o.targetID
	o.mu.Unlock()
	for _, message := range messages {
		if message.ID == targetID && targetID != "" {
			o.once.Do(func() { close(o.targetSeen) })
			break
		}
	}
	return messages, nil
}

func (conversationContextE2ETaskStore) LoadDeferredTaskTarget(context.Context, task.Task) (app.DeferredTaskTarget, bool, error) {
	return app.DeferredTaskTarget{}, false, nil
}

func (m *conversationContextE2ERouteModels) ResolveConversationRouteModel(ctx context.Context, claim app.ConversationRouteClaim) (string, error) {
	return m.coordinators[claim.Message.ConversationID].ResolveConversationRouteModel(ctx, claim)
}

func (m *conversationContextE2ERouteModels) ResolveConversationRouteContextWindow(ctx context.Context,
	claim app.ConversationRouteClaim, modelID string) (int, bool, error) {
	return m.gateway.ResolveConversationRouteContextWindow(ctx, claim, modelID)
}

func (m *conversationContextE2ERouteModels) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	return m.coordinators[claim.Message.ConversationID].ClassifyConversationRoute(ctx, claim, value)
}

func (m *conversationContextE2ERouteModels) ResolveConversationSummaryModel(ctx context.Context,
	claim app.ConversationRouteClaim) (string, int, error) {
	return m.gateway.ResolveConversationSummaryModel(ctx, claim)
}

func (m *conversationContextE2ERouteModels) EstimateConversationSummaryTokenPressure(ctx context.Context,
	request CloudDSHConversationSummaryEstimateRequest) ([]int, error) {
	return m.coordinators[request.Claim.Message.ConversationID].EstimateConversationSummaryTokenPressure(ctx, request)
}

func (m *conversationContextE2ERouteModels) CompactConversationSummary(ctx context.Context,
	request CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error) {
	return m.coordinators[request.Claim.Message.ConversationID].CompactConversationSummary(ctx, request)
}

func conversationContextE2ESummaryVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	serviceInstanceID, ownerID, conversationID string) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT head_version FROM conversation_summary_heads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$2 AND conversation_id=$3 AND scope_kind='personal' AND scope_id=''`,
		serviceInstanceID, ownerID, conversationID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}
