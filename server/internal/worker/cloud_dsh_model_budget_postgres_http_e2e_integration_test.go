package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type modelBudgetPostgresHTTPProviderRequest struct {
	Model           string                         `json:"model"`
	MaxOutputTokens int                            `json:"max_output_tokens"`
	Input           []modelBudgetPostgresHTTPInput `json:"input"`
}

type modelBudgetPostgresHTTPInput struct {
	Role    string                           `json:"role"`
	Content []modelBudgetPostgresHTTPContent `json:"content"`
}

type modelBudgetPostgresHTTPContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type modelBudgetPostgresHTTPResponse struct {
	Model        string `json:"model"`
	Text         string `json:"text"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type modelBudgetPostgresHTTPClient struct {
	endpoint string
	client   *http.Client
	mu       sync.Mutex
	maxCaps  []int
}

func (client *modelBudgetPostgresHTTPClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	payload := modelBudgetPostgresHTTPProviderRequest{Model: request.Endpoint.ModelID, MaxOutputTokens: request.MaxOutputTokens}
	for _, input := range request.Input {
		if input.Type != "message" {
			return app.ModelResponse{}, errors.New("budget E2E provider received a non-message input")
		}
		item := modelBudgetPostgresHTTPInput{Role: input.Role}
		if input.Text != "" {
			item.Content = append(item.Content, modelBudgetPostgresHTTPContent{Type: "input_text", Text: input.Text})
		}
		for _, part := range input.Parts {
			if part.Type != "text" {
				return app.ModelResponse{}, errors.New("budget E2E provider received a non-text input part")
			}
			item.Content = append(item.Content, modelBudgetPostgresHTTPContent{Type: "input_text", Text: part.Text})
		}
		payload.Input = append(payload.Input, item)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return app.ModelResponse{}, err
	}
	requestHTTP, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return app.ModelResponse{}, err
	}
	requestHTTP.Header.Set("Authorization", "Bearer "+request.Endpoint.APIKey)
	requestHTTP.Header.Set("Content-Type", "application/json")
	responseHTTP, err := client.client.Do(requestHTTP)
	if err != nil {
		return app.ModelResponse{}, err
	}
	defer func() { _ = responseHTTP.Body.Close() }()
	if responseHTTP.StatusCode != http.StatusOK {
		return app.ModelResponse{}, fmt.Errorf("budget E2E fake provider returned HTTP %d", responseHTTP.StatusCode)
	}
	var response modelBudgetPostgresHTTPResponse
	if err := json.NewDecoder(io.LimitReader(responseHTTP.Body, 1<<20)).Decode(&response); err != nil {
		return app.ModelResponse{}, err
	}
	client.mu.Lock()
	client.maxCaps = append(client.maxCaps, request.MaxOutputTokens)
	client.mu.Unlock()
	return app.ModelResponse{Text: response.Text, StopReason: "stop", ProviderReportedModel: response.Model,
		Usage: app.ModelUsage{Known: true, InputTokens: response.InputTokens, OutputTokens: response.OutputTokens}}, nil
}

func (*modelBudgetPostgresHTTPClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected model capability check in budget E2E test")
}

type modelBudgetPostgresHTTPPlannerCapture struct {
	inner    CloudDSHModelBudgetPlanner
	mu       sync.Mutex
	requests []CloudDSHModelBudgetPlanRequest
	plans    []CloudDSHModelBudgetPlan
	failures []modelBudgetPostgresHTTPPlannerFailure
}

type modelBudgetPostgresHTTPPlannerFailure struct {
	throughSeq int64
	dshCallID  string
	inputBytes int
	purpose    string
	code       string
	reason     string
	message    string
}

type budgetCompactionRouteModels struct{ contextWindow int }

type modelBudgetCompactionCheckpointObserver struct {
	CloudDSHCheckpoints
	mu       sync.Mutex
	watchers map[string]chan struct{}
	events   []string
}

func (observer *modelBudgetCompactionCheckpointObserver) Save(ctx context.Context,
	command app.AgentCheckpointCommand) (bool, error) {
	var payload struct {
		Events []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
	}
	_ = json.Unmarshal(command.Payload, &payload)
	observer.mu.Lock()
	for _, event := range payload.Events {
		if strings.HasPrefix(event.Type, "compaction/") {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(event.Data, &fields)
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			observer.events = append(observer.events, event.Type+":"+strings.Join(keys, ","))
		}
	}
	observer.mu.Unlock()
	return observer.CloudDSHCheckpoints.Save(ctx, command)
}

func (observer *modelBudgetCompactionCheckpointObserver) Watch(messageID string) <-chan struct{} {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.watchers == nil {
		observer.watchers = make(map[string]chan struct{})
	}
	seen := make(chan struct{})
	observer.watchers[messageID] = seen
	return seen
}

func (observer *modelBudgetCompactionCheckpointObserver) PendingMessages(ctx context.Context,
	fence app.AgentCheckpointFence) ([]app.AgentConversationMessage, error) {
	messages, err := observer.CloudDSHCheckpoints.PendingMessages(ctx, fence)
	if err != nil {
		return messages, err
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, message := range messages {
		if seen, ok := observer.watchers[message.ID]; ok {
			close(seen)
			delete(observer.watchers, message.ID)
		}
	}
	return messages, nil
}

func (models budgetCompactionRouteModels) ResolveConversationRouteModel(ctx context.Context, claim app.ConversationRouteClaim) (string, error) {
	return (conversationContextSteerRouteClassifier{}).ResolveConversationRouteModel(ctx, claim)
}

func (models budgetCompactionRouteModels) ResolveConversationRouteContextWindow(context.Context, app.ConversationRouteClaim, string) (int, bool, error) {
	return models.contextWindow, true, nil
}

func (models budgetCompactionRouteModels) ResolveTaskContextWindow(context.Context, task.Task) (*int, error) {
	contextWindow := models.contextWindow
	return &contextWindow, nil
}

func (budgetCompactionRouteModels) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	return (conversationContextSteerRouteClassifier{}).ClassifyConversationRoute(ctx, claim, value)
}

func (capture *modelBudgetPostgresHTTPPlannerCapture) PrepareModelBudget(ctx context.Context,
	request CloudDSHModelBudgetPlanRequest) (CloudDSHModelBudgetPlan, error) {
	plan, err := capture.inner.PrepareModelBudget(ctx, request)
	if err != nil {
		failure := modelBudgetPostgresHTTPPlannerFailure{throughSeq: request.ThroughSeq, dshCallID: request.DSHCallID,
			inputBytes: len(request.Request), code: string(domain.CodeOf(err)), message: err.Error()}
		var config struct {
			Purpose string `json:"purpose"`
		}
		_ = json.Unmarshal(request.Config, &config)
		failure.purpose = config.Purpose
		var modelErr *domain.Error
		if errors.As(err, &modelErr) {
			failure.reason, _ = modelErr.Details["reason"].(string)
		}
		capture.mu.Lock()
		capture.failures = append(capture.failures, failure)
		capture.mu.Unlock()
		return CloudDSHModelBudgetPlan{}, err
	}
	request.Request = append(json.RawMessage(nil), request.Request...)
	request.Config = append(json.RawMessage(nil), request.Config...)
	request.ToolHistory = append(json.RawMessage(nil), request.ToolHistory...)
	capture.mu.Lock()
	capture.requests = append(capture.requests, request)
	capture.plans = append(capture.plans, plan)
	capture.mu.Unlock()
	return plan, nil
}

func (capture *modelBudgetPostgresHTTPPlannerCapture) SupportsTaskModelBudgetAdmission(ctx context.Context, value task.Task) (bool, error) {
	return capture.inner.SupportsTaskModelBudgetAdmission(ctx, value)
}

type modelBudgetPostgresHTTPModels struct {
	*app.ModelGatewayService
	mu                           sync.Mutex
	contextWindow                int
	contextWindowCalls           atomic.Int32
	attempts                     []app.ModelCall
	calls                        []app.ModelCall
	first                        app.ModelResponse
	replay                       app.ModelResponse
	lastErr                      error
	cancelBeforeNextGateway      bool
	afterNoDispatch              func(app.ModelCall, error) error
	replayAfterNextGatewayError  bool
	afterGatewayErrorAndReplay   func(app.ModelCall, error, error) error
	beforeNextGateway            func(app.ModelCall) error
	retryAfterNextGatewayError   bool
	afterGatewayErrorBeforeRetry func(app.ModelCall, error) error
}

func (models *modelBudgetPostgresHTTPModels) ResolveTaskContextWindow(context.Context, task.Task) (*int, error) {
	models.contextWindowCalls.Add(1)
	contextWindow := models.contextWindow
	if contextWindow <= 0 {
		return nil, nil
	}
	return &contextWindow, nil
}

func (models *modelBudgetPostgresHTTPModels) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	models.mu.Lock()
	models.attempts = append(models.attempts, call)
	cancelBeforeGateway := models.cancelBeforeNextGateway
	models.cancelBeforeNextGateway = false
	afterNoDispatch := models.afterNoDispatch
	models.afterNoDispatch = nil
	replayAfterError := models.replayAfterNextGatewayError
	models.replayAfterNextGatewayError = false
	afterErrorAndReplay := models.afterGatewayErrorAndReplay
	models.afterGatewayErrorAndReplay = nil
	beforeGateway := models.beforeNextGateway
	models.beforeNextGateway = nil
	retryAfterError := models.retryAfterNextGatewayError
	models.retryAfterNextGatewayError = false
	afterErrorBeforeRetry := models.afterGatewayErrorBeforeRetry
	models.afterGatewayErrorBeforeRetry = nil
	models.mu.Unlock()
	attemptCtx := ctx
	var cancel context.CancelFunc
	if cancelBeforeGateway {
		attemptCtx, cancel = context.WithCancel(ctx)
		cancel()
	}
	if beforeGateway != nil {
		if err := beforeGateway(call); err != nil {
			models.mu.Lock()
			models.lastErr = err
			models.mu.Unlock()
			return app.ModelResponse{}, err
		}
	}
	response, err := models.ModelGatewayService.Complete(attemptCtx, call)
	if cancelBeforeGateway {
		cancel()
		if err == nil {
			failure := errors.New("cancelled pre-dispatch Gateway call unexpectedly succeeded")
			models.mu.Lock()
			models.lastErr = failure
			models.mu.Unlock()
			return app.ModelResponse{}, failure
		}
		if afterNoDispatch != nil {
			if checkErr := afterNoDispatch(call, err); checkErr != nil {
				models.mu.Lock()
				models.lastErr = errors.Join(err, checkErr)
				models.mu.Unlock()
				return app.ModelResponse{}, models.lastErr
			}
		}
		// The canceled child context was only for this Gateway attempt. The
		// DSH/Worker call context remains live, so retry the same signed call.
		response, err = models.ModelGatewayService.Complete(ctx, call)
	}
	if err != nil && retryAfterError {
		var retryCheckErr error
		if afterErrorBeforeRetry != nil {
			retryCheckErr = afterErrorBeforeRetry(call, err)
		}
		if retryCheckErr != nil {
			err = errors.Join(err, retryCheckErr)
		} else {
			// Retry only after the test's injected storage fault is removed. The
			// same signed DSH model call and budget identity must remain usable.
			response, err = models.ModelGatewayService.Complete(context.WithoutCancel(ctx), call)
		}
	}
	if err != nil {
		if replayAfterError {
			_, replayErr := models.ModelGatewayService.Complete(context.WithoutCancel(ctx), call)
			if afterErrorAndReplay != nil {
				if checkErr := afterErrorAndReplay(call, err, replayErr); checkErr != nil {
					err = errors.Join(err, checkErr)
				}
			}
		}
		models.mu.Lock()
		models.lastErr = err
		models.mu.Unlock()
		return app.ModelResponse{}, err
	}
	replayCall := call
	replayCall.RawRequest = append(json.RawMessage(nil), call.RawRequest...)
	if call.Task != nil {
		fence := *call.Task
		replayCall.Task = &fence
	}
	replay, err := models.ModelGatewayService.Complete(ctx, replayCall)
	if err != nil || replay.Text != response.Text || replay.Usage != response.Usage {
		failure := errors.New("settled model response replay did not match the first provider result")
		models.mu.Lock()
		models.lastErr = errors.Join(err, failure)
		models.mu.Unlock()
		return app.ModelResponse{}, models.lastErr
	}
	models.mu.Lock()
	models.calls = append(models.calls, call)
	models.first = response
	models.replay = replay
	models.mu.Unlock()
	return response, nil
}

func TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,100000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatalf("configure synthetic account model budget: %v", err)
	}

	var providerCalls atomic.Int32
	const (
		providerModel = "synthetic-budget-e2e-v1"
		apiKey        = "synthetic-budget-e2e-only"
		outputCap     = 3072
	)
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/responses" ||
			request.Header.Get("Authorization") != "Bearer "+apiKey ||
			!bytes.HasPrefix([]byte(request.Header.Get("Content-Type")), []byte("application/json")) {
			http.Error(writer, "unexpected synthetic provider request", http.StatusBadRequest)
			return
		}
		var body modelBudgetPostgresHTTPProviderRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic provider request", http.StatusBadRequest)
			return
		}
		hasUserText := false
		for _, input := range body.Input {
			if input.Role != "user" {
				continue
			}
			for _, content := range input.Content {
				if content.Type == "input_text" && content.Text != "" {
					hasUserText = true
				}
			}
		}
		if body.Model != providerModel || body.MaxOutputTokens != outputCap || !hasUserText {
			http.Error(writer, "request did not preserve its signed model cap and input", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"synthetic-budget-e2e-v1","text":"Synthetic DSH budget path verified.","input_tokens":8,"output_tokens":6}`)
	}))
	defer provider.Close()
	providerClient := &modelBudgetPostgresHTTPClient{endpoint: provider.URL + "/responses", client: provider.Client()}

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
		t.Fatalf("verify synthetic owner session: principal=%#v err=%v", principal, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, modelMaxOutput := 32768, 8192
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Budget E2E synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: providerModel, APIKey: apiKey,
		ContextWindowOverrideTokens: &contextWindow, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic model route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatalf("enable synthetic text-only route: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, providerClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: outputCap, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "dsh-pg-http-e2e-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: connection.BaseURL, Model: providerModel, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}

	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: connection.ID, Objective: "Return the synthetic budget E2E verification sentence.",
		Budget: task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 50000}, IdempotencyKey: "budget-e2e-submit-001",
	})
	if err != nil {
		t.Fatalf("submit synthetic budget E2E Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-e2e-task-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != submission.Task.ID {
		t.Fatalf("claim synthetic budget E2E Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the actual DSH runtime cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, runtimeScript,
		"budget-e2e-supervisor", bytes.Repeat([]byte{0x42}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	plannerCapture := &modelBudgetPostgresHTTPPlannerCapture{inner: planner}
	models := &modelBudgetPostgresHTTPModels{ModelGatewayService: gateway}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "budget-e2e-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: models, ModelBudgetPlanner: plannerCapture,
		ToolCapabilities: cloudDSHTestToolCapability{allowed: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	executionErr := executor.ExecuteTask(ctx, claimed)
	if !errors.Is(executionErr, ErrCloudDSHCompletionPending) {
		var modelFailure cloudDSHModelResultError
		diagnostic := app.AgentModelFailureDiagnostic{}
		if errors.As(executionErr, &modelFailure) {
			diagnostic = modelFailure.ModelFailureDiagnostic()
		}
		models.mu.Lock()
		attemptCount, gatewayCallCount, gatewayErr := len(models.attempts), len(models.calls), models.lastErr
		var gatewayAttempt app.ModelCall
		if attemptCount > 0 {
			gatewayAttempt = models.attempts[0]
		}
		models.mu.Unlock()
		plannerCapture.mu.Lock()
		plannerCallCount, plannerPlanCount := len(plannerCapture.requests), len(plannerCapture.plans)
		var plannerRequest CloudDSHModelBudgetPlanRequest
		var plannerPlan CloudDSHModelBudgetPlan
		if plannerCallCount > 0 {
			plannerRequest = plannerCapture.requests[0]
		}
		if plannerPlanCount > 0 {
			plannerPlan = plannerCapture.plans[0]
		}
		plannerCapture.mu.Unlock()
		providerClient.mu.Lock()
		providerCaps := append([]int(nil), providerClient.maxCaps...)
		providerClient.mu.Unlock()
		var taskState string
		taskDetails, taskErr := store.GetTask(ctx, claimed.ID, owner.ID)
		if taskErr == nil {
			taskState = string(taskDetails.Task.Status)
		}
		var dbPlanID, dbCallID, dbDSHCallID, dbBudgetCallID, dbLedgerState string
		var dbThroughSeq int64
		dbLedgerErr := pool.QueryRow(ctx, `SELECT preparation.plan_id,preparation.call_id,preparation.dsh_call_id,
preparation.budget_call_id,preparation.through_sequence,ledger.state FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.task_id=$2 AND preparation.source='task'`, owner.ID, claimed.ID).
			Scan(&dbPlanID, &dbCallID, &dbDSHCallID, &dbBudgetCallID, &dbThroughSeq, &dbLedgerState)
		gatewayThrough, gatewaySession := int64(-1), ""
		if gatewayAttempt.Task != nil {
			gatewayThrough, gatewaySession = gatewayAttempt.Task.ThroughSequence, gatewayAttempt.Task.AgentSessionID
		}
		t.Fatalf("DSH Task execution = %v (%T), diagnostic=%+v, attempts/gateway-success=%d/%d gateway_error=%v planner_calls/plans=%d/%d planner_identity={call:%q,dsh:%q,budget:%q,through:%d,session:%q,plan:%q} gateway_identity={call:%q,ledger:%q,budget:%q,through:%d,session:%q,plan:%q} postgres_identity={call:%q,dsh:%q,budget:%q,through:%d,plan:%q,state:%q,err:%v} provider_http/caps=%d/%v task_state=%q task_error=%v; want successful model result awaiting separate Task completion",
			executionErr, executionErr, diagnostic, attemptCount, gatewayCallCount, gatewayErr,
			plannerCallCount, plannerPlanCount, plannerRequest.CallID, plannerRequest.DSHCallID, plannerRequest.CallID, plannerRequest.ThroughSeq,
			plannerRequest.SessionID, plannerPlan.PlanID, gatewayAttempt.CallID, gatewayAttempt.LedgerCallID, gatewayAttempt.BudgetCallID,
			gatewayThrough, gatewaySession, gatewayAttempt.BudgetPlanID, dbCallID, dbDSHCallID, dbBudgetCallID, dbThroughSeq,
			dbPlanID, dbLedgerState, dbLedgerErr,
			providerCalls.Load(), providerCaps, taskState, taskErr)
	}

	plannerCapture.mu.Lock()
	preparedRequests := append([]CloudDSHModelBudgetPlanRequest(nil), plannerCapture.requests...)
	preparedPlans := append([]CloudDSHModelBudgetPlan(nil), plannerCapture.plans...)
	plannerCapture.mu.Unlock()
	if len(preparedRequests) != 1 || len(preparedPlans) != 1 || preparedPlans[0].PlanID == "" || preparedPlans[0].MaxTokens != outputCap {
		t.Fatalf("DSH prepare requests/plans = %d/%#v, want one PostgreSQL plan capped at %d", len(preparedRequests), preparedPlans, outputCap)
	}
	preparedRequest := preparedRequests[0]
	if digest, ok := cloudDSHModelBudgetRequestDigest(preparedRequest.Request); !ok || digest != preparedRequest.RequestDigest {
		t.Fatalf("DSH prepare digest does not bind its exact provider request: valid=%t", ok)
	}
	var preparedProjection map[string]json.RawMessage
	if err := json.Unmarshal(preparedRequest.Request, &preparedProjection); err != nil {
		t.Fatalf("decode DSH prepare projection: %v", err)
	}
	if len(preparedProjection) != 4 || preparedProjection["provider"] == nil || preparedProjection["model"] == nil ||
		preparedProjection["messages"] == nil || preparedProjection["tools"] == nil || preparedProjection["max_tokens"] != nil {
		t.Fatalf("DSH prepare projection did not remain cap-free: fields=%d", len(preparedProjection))
	}

	models.mu.Lock()
	capturedCalls := append([]app.ModelCall(nil), models.calls...)
	firstResponse, replayResponse := models.first, models.replay
	models.mu.Unlock()
	if len(capturedCalls) != 1 {
		t.Fatalf("final DSH model frame reached ModelGateway %d times, want once", len(capturedCalls))
	}
	modelCall := capturedCalls[0]
	if modelCall.Task == nil {
		t.Fatal("final DSH model frame omitted its Task identity")
	}
	preparedLedgerID, err := app.DSHModelCallID(preparedRequest.Fence.TaskID, preparedRequest.SessionID, preparedRequest.ThroughSeq)
	if err != nil || preparedLedgerID != preparedRequest.DSHCallID {
		t.Fatalf("prepared DSH call ID %q does not match its Task/session/through identity %q: err=%v",
			preparedRequest.DSHCallID, preparedLedgerID, err)
	}
	finalDSHCallID, err := app.DSHModelCallID(modelCall.Task.TaskID, modelCall.Task.AgentSessionID, modelCall.Task.ThroughSequence)
	if err != nil {
		t.Fatalf("derive final DSH call ID from the final model fence: %v", err)
	}
	if modelCall.BudgetPlanID != preparedPlans[0].PlanID || modelCall.BudgetCallID != preparedRequest.CallID ||
		modelCall.BudgetRequestDigest != preparedRequest.RequestDigest || modelCall.LedgerCallID != preparedRequest.DSHCallID ||
		modelCall.CallID != finalDSHCallID {
		t.Fatalf("final DSH model frame identity changed its prepared budget binding: prepared={plan:%q,budget:%q,ledger:%q,dsh:%q,through:%d} final={plan:%q,budget:%q,request_digest:%q,ledger:%q,dsh:%q,through:%d}",
			preparedPlans[0].PlanID, preparedRequest.CallID, preparedRequest.DSHCallID, preparedRequest.DSHCallID, preparedRequest.ThroughSeq,
			modelCall.BudgetPlanID, modelCall.BudgetCallID, modelCall.BudgetRequestDigest, modelCall.LedgerCallID, modelCall.CallID, modelCall.Task.ThroughSequence)
	}
	var finalProjection struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(modelCall.RawRequest, &finalProjection); err != nil || finalProjection.MaxTokens != outputCap {
		t.Fatalf("final DSH request max_tokens=%d err=%v, want %d", finalProjection.MaxTokens, err, outputCap)
	}
	if firstResponse.Text != "Synthetic DSH budget path verified." || replayResponse.Text != firstResponse.Text || replayResponse.Usage != firstResponse.Usage {
		t.Fatal("settled Gateway replay did not return the same synthetic result and usage")
	}

	providerClient.mu.Lock()
	modelClientCaps := append([]int(nil), providerClient.maxCaps...)
	providerClient.mu.Unlock()
	if len(modelClientCaps) != 1 || modelClientCaps[0] != outputCap || providerCalls.Load() != 1 {
		t.Fatalf("Gateway/provider calls and caps = %d/%v, want one provider dispatch at %d", providerCalls.Load(), modelClientCaps, outputCap)
	}

	var ledgerState, ledgerCallID, preparationDSHCallID string
	var budgetCallID string
	var sendTokens, inputReservedTokens, reservedTokens, actualInputTokens, actualOutputTokens int64
	var preparationConsumed bool
	if err := pool.QueryRow(ctx, `SELECT ledger.state,preparation.call_id,preparation.dsh_call_id,preparation.budget_call_id,
preparation.send_tokens,preparation.input_reserved_tokens,ledger.reserved_tokens,ledger.input_tokens,ledger.output_tokens,
(preparation.consumed_at IS NOT NULL)
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
	WHERE preparation.owner_id=$1 AND preparation.task_id=$2 AND preparation.source='task'`, owner.ID, submission.Task.ID).
		Scan(&ledgerState, &ledgerCallID, &preparationDSHCallID, &budgetCallID, &sendTokens, &inputReservedTokens, &reservedTokens,
			&actualInputTokens, &actualOutputTokens, &preparationConsumed); err != nil {
		t.Fatalf("read settled budget E2E ledger: %v", err)
	}
	var preparationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_budget_preparations WHERE owner_id=$1 AND task_id=$2 AND source='task'`,
		owner.ID, submission.Task.ID).Scan(&preparationCount); err != nil || preparationCount != 1 {
		t.Fatalf("Task budget preparation count=%d err=%v, want exactly one", preparationCount, err)
	}
	if ledgerState != "settled" || !preparationConsumed || sendTokens != int64(outputCap) || reservedTokens != inputReservedTokens+sendTokens ||
		actualInputTokens != 8 || actualOutputTokens != 6 || budgetCallID != modelCall.BudgetCallID ||
		ledgerCallID != modelCall.LedgerCallID || ledgerCallID != preparedRequest.DSHCallID || preparationDSHCallID != preparedRequest.DSHCallID {
		t.Fatalf("settled ledger identity/state/cap/usage mismatch: state=%q consumed=%t send=%d reserved=%d input_hold=%d usage=%d/%d budget_call=%q ledger_call=%q preparation_dsh=%q final_dsh=%q",
			ledgerState, preparationConsumed, sendTokens, reservedTokens, inputReservedTokens, actualInputTokens, actualOutputTokens,
			budgetCallID, ledgerCallID, preparationDSHCallID, modelCall.CallID)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("settled replay dispatched to the local HTTP provider again: calls=%d", providerCalls.Load())
	}
}

func TestCloudDSHModelBudgetPostgresHTTPUsesTaskHermesCompactionSummaryStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,1000000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatalf("configure synthetic account model budget: %v", err)
	}

	const (
		providerModel = "synthetic-budget-compaction-e2e-v1"
		apiKey        = "synthetic-budget-compaction-only"
		outputCap     = 14_000
		summaryCap    = 64
		contextWindow = 16_384
		routeWindow   = 131_072
		followupCount = 100
	)
	var providerCalls atomic.Int32
	type providerGate struct {
		started chan struct{}
		resume  chan struct{}
		once    sync.Once
	}
	newProviderGate := func() *providerGate {
		return &providerGate{started: make(chan struct{}), resume: make(chan struct{})}
	}
	var gateMu sync.Mutex
	firstProviderGate := newProviderGate()
	nextProviderGate := firstProviderGate
	var providerGates []*providerGate
	armProviderGate := func() *providerGate {
		gate := newProviderGate()
		gateMu.Lock()
		nextProviderGate = gate
		providerGates = append(providerGates, gate)
		gateMu.Unlock()
		return gate
	}
	providerGates = append(providerGates, firstProviderGate)
	resumeProviderGate := func(gate *providerGate) { gate.once.Do(func() { close(gate.resume) }) }
	defer func() {
		gateMu.Lock()
		gates := append([]*providerGate(nil), providerGates...)
		gateMu.Unlock()
		for _, gate := range gates {
			resumeProviderGate(gate)
		}
	}()
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCall := providerCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/responses" ||
			request.Header.Get("Authorization") != "Bearer "+apiKey ||
			!bytes.HasPrefix([]byte(request.Header.Get("Content-Type")), []byte("application/json")) {
			http.Error(writer, "unexpected synthetic provider request", http.StatusBadRequest)
			return
		}
		var body modelBudgetPostgresHTTPProviderRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic provider request", http.StatusBadRequest)
			return
		}
		hasUserText := false
		for _, input := range body.Input {
			if input.Role != "user" {
				continue
			}
			for _, content := range input.Content {
				hasUserText = hasUserText || content.Type == "input_text" && content.Text != ""
			}
		}
		if body.Model != providerModel || body.MaxOutputTokens <= 0 || body.MaxOutputTokens > outputCap || !hasUserText {
			http.Error(writer, "synthetic DSH request did not preserve its model cap and user input", http.StatusBadRequest)
			return
		}
		if body.MaxOutputTokens > summaryCap {
			gateMu.Lock()
			gate := nextProviderGate
			nextProviderGate = nil
			gateMu.Unlock()
			if gate != nil {
				close(gate.started)
				select {
				case <-gate.resume:
				case <-request.Context().Done():
					return
				}
			}
		}
		if providerCall < 1 {
			http.Error(writer, "unreachable synthetic provider call state", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"synthetic-budget-compaction-e2e-v1","text":"Synthetic DSH budget path verified.","input_tokens":8,"output_tokens":6}`)
	}))
	defer provider.Close()
	providerClient := &modelBudgetPostgresHTTPClient{endpoint: provider.URL + "/responses", client: provider.Client()}

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
		t.Fatalf("verify synthetic owner session: principal=%#v err=%v", principal, err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x35}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindowTokens := contextWindow
	modelMaxOutput := outputCap
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Task Hermes compaction synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: providerModel, APIKey: apiKey,
		ContextWindowOverrideTokens: &contextWindowTokens, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic compaction model route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatalf("enable synthetic text-only route: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, providerClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: summaryCap, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: summaryCap, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: summaryCap, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "dsh-task-compaction-e2e-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: connection.BaseURL, Model: providerModel, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}

	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	initialConversation, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
		Title: "Task Hermes compaction E2E", IdempotencyKey: "budget-compaction-conversation",
		ServiceInstanceID: serviceInstanceID,
	})
	if err != nil {
		t.Fatalf("create synthetic compaction conversation: %v", err)
	}
	const initialClientMessageID = "budget-compaction-initial"
	initialMessage, replayed, err := tasks.AcceptConversationMessage(ctx, principal, initialConversation.ID,
		app.AcceptConversationMessageCommand{
			ServiceInstanceID: serviceInstanceID, ClientMessageID: initialClientMessageID,
			IdempotencyKey: initialClientMessageID, Content: "Preserve the opening requirement, use no external tools, and answer briefly after reading the prior notes.",
			ModelID: connection.ID, Budget: task.Budget{ActiveSeconds: 180, ToolCalls: 1, ModelTokens: task.MaxModelTokens},
		})
	if err != nil || replayed {
		t.Fatalf("accept synthetic compaction initial message: replayed=%t err=%v", replayed, err)
	}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: budgetCompactionRouteModels{contextWindow: routeWindow},
		WorkerID: "budget-compaction-route-worker", LeaseDuration: time.Minute, PollInterval: time.Millisecond,
		MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create synthetic compaction conversation route worker: %v", err)
	}
	if routed, routeErr := routeWorker.RunOnce(ctx); routeErr != nil || !routed {
		t.Fatalf("route synthetic compaction initial message: routed=%t err=%v", routed, routeErr)
	}
	var taskID string
	if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialMessage.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("initial conversation route did not create a Task: task=%q err=%v", taskID, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-compaction-task-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.ConversationID != initialConversation.ID {
		t.Fatalf("claim synthetic compaction E2E Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the actual DSH runtime cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, runtimeScript,
		"budget-compaction-supervisor", bytes.Repeat([]byte{0x36}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointObserver := &modelBudgetCompactionCheckpointObserver{CloudDSHCheckpoints: checkpoints}
	planner, err := NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	plannerCapture := &modelBudgetPostgresHTTPPlannerCapture{inner: planner}
	models := &modelBudgetPostgresHTTPModels{ModelGatewayService: gateway, contextWindow: contextWindow}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "budget-compaction-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpointObserver, Models: models, ModelBudgetPlanner: plannerCapture,
		ToolCapabilities: cloudDSHTestToolCapability{allowed: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskCtx, cancelTask := context.WithCancel(ctx)
	taskDone := make(chan error, 1)
	taskDonePending := true
	defer func() {
		gateMu.Lock()
		gates := append([]*providerGate(nil), providerGates...)
		gateMu.Unlock()
		for _, gate := range gates {
			resumeProviderGate(gate)
		}
		cancelTask()
		if taskDonePending {
			select {
			case taskErr := <-taskDone:
				if taskErr != nil && !errors.Is(taskErr, context.Canceled) {
					t.Errorf("compaction Task returned while stopping after test cleanup: %v", taskErr)
				}
			case <-time.After(15 * time.Second):
				t.Error("compaction Task did not stop after the test ended")
			}
		}
	}()
	go func() { taskDone <- executor.ExecuteTask(taskCtx, claimed) }()
	select {
	case <-firstProviderGate.started:
	case taskErr := <-taskDone:
		taskDonePending = false
		t.Fatalf("compaction Task exited before its first provider call: %v", taskErr)
	case <-ctx.Done():
		t.Fatalf("compaction Task did not reach its first provider call: %v", ctx.Err())
	}
	var taskStateBeforeFollowups string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, claimed.ID).Scan(&taskStateBeforeFollowups); err != nil ||
		taskStateBeforeFollowups != string(task.StatusRunning) {
		t.Fatalf("compaction Task was not running while the provider call was blocked: status=%q err=%v", taskStateBeforeFollowups, err)
	}
	var followupMessageIDs []string
	previousClientMessageID := initialClientMessageID
	budget := task.Budget{ActiveSeconds: 180, ToolCalls: 1, ModelTokens: task.MaxModelTokens}
	const followupsPerBatch = 1
	currentProviderGate := firstProviderGate
	for batchStart := 1; batchStart <= followupCount; batchStart += followupsPerBatch {
		batchEnd := min(batchStart+followupsPerBatch-1, followupCount)
		for index := batchStart; index <= batchEnd; index++ {
			clientMessageID := fmt.Sprintf("budget-compaction-followup-%02d", index)
			content := fmt.Sprintf("followup-%02d preserve its detail: %s", index, strings.Repeat("history ", 34))
			message, replayed, acceptErr := tasks.AcceptConversationMessage(ctx, principal, claimed.ConversationID,
				app.AcceptConversationMessageCommand{
					ServiceInstanceID: serviceInstanceID, ClientMessageID: clientMessageID,
					IdempotencyKey: clientMessageID, PreviousClientMessageID: previousClientMessageID,
					ReferenceTaskID: claimed.ID, Content: content, ModelID: connection.ID, Budget: budget,
				})
			if acceptErr != nil || replayed {
				t.Fatalf("accept running Task followup %d: replayed=%t err=%v", index, replayed, acceptErr)
			}
			followupMessageIDs = append(followupMessageIDs, message.ID)
			varSeen := checkpointObserver.Watch(message.ID)
			routeDeadline := time.Now().Add(30 * time.Second)
			for {
				routed, routeErr := routeWorker.RunOnce(ctx)
				if routeErr != nil {
					t.Fatalf("route running Task followup %d: %v", index, routeErr)
				}
				var linkedTaskID, deliveryStatus, deliveryMode, routeState, failureReason string
				if err := pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),message.delivery_status,message.delivery_mode,route.state,route.failure_reason
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.id=$1`, message.ID).
					Scan(&linkedTaskID, &deliveryStatus, &deliveryMode, &routeState, &failureReason); err != nil {
					t.Fatalf("inspect routed running Task followup %d: %v", index, err)
				}
				if linkedTaskID == claimed.ID && deliveryStatus == "pending" && deliveryMode == string(app.AgentMessageDeliverySteer) && routeState == "routed" {
					break
				}
				if routeState == "failed" || routeState == "rejected" || routeState == "waiting_user" {
					t.Fatalf("running Task followup %d route ended state=%q mode=%q failure_bytes=%d",
						index, routeState, deliveryMode, len(failureReason))
				}
				if time.Now().After(routeDeadline) {
					t.Fatalf("running Task followup %d route state task=%q delivery=%q mode=%q route=%q failure_bytes=%d; RunOnce found=%t",
						index, linkedTaskID, deliveryStatus, deliveryMode, routeState, len(failureReason), routed)
				}
				if !routed {
					time.Sleep(20 * time.Millisecond)
				}
			}
			previousClientMessageID = clientMessageID
			if index == batchEnd {
				select {
				case <-varSeen:
				case taskErr := <-taskDone:
					taskDonePending = false
					t.Fatalf("compaction Task exited before DSH discovered followup %d: %v", index, taskErr)
				case <-time.After(30 * time.Second):
					t.Fatalf("DSH message-delivery poller did not discover synthetic Task followup %d", index)
				case <-ctx.Done():
					t.Fatalf("DSH message-delivery poller did not discover synthetic Task followup %d: %v", index, ctx.Err())
				}
			}
		}
		if batchEnd < followupCount {
			nextProviderGate := armProviderGate()
			resumeProviderGate(currentProviderGate)
			currentProviderGate = nextProviderGate
			select {
			case <-currentProviderGate.started:
			case taskErr := <-taskDone:
				taskDonePending = false
				plannerCapture.mu.Lock()
				planFailures := append([]modelBudgetPostgresHTTPPlannerFailure(nil), plannerCapture.failures...)
				preparedRequests := append([]CloudDSHModelBudgetPlanRequest(nil), plannerCapture.requests...)
				preparedPlans := append([]CloudDSHModelBudgetPlan(nil), plannerCapture.plans...)
				plannerCapture.mu.Unlock()
				prepared := make([]string, 0, len(preparedRequests))
				for requestIndex, request := range preparedRequests {
					var config struct {
						Purpose string `json:"purpose"`
					}
					_ = json.Unmarshal(request.Config, &config)
					prepared = append(prepared, fmt.Sprintf("through=%d purpose=%q input_bytes=%d max_tokens=%d",
						request.ThroughSeq, config.Purpose, len(request.Request), preparedPlans[requestIndex].MaxTokens))
				}
				checkpointObserver.mu.Lock()
				compactionEvents := append([]string(nil), checkpointObserver.events...)
				checkpointObserver.mu.Unlock()
				t.Fatalf("compaction Task exited before provider gate for followup batch %d: %v plan_failures=%+v prepared=%v compaction_events=%v context_window_resolves=%d",
					batchEnd+1, taskErr, planFailures, prepared, compactionEvents, models.contextWindowCalls.Load())
			case <-time.After(30 * time.Second):
				t.Fatalf("compaction Task did not reach provider gate for followup batch %d", batchEnd+1)
			case <-ctx.Done():
				t.Fatalf("compaction Task did not reach provider gate for followup batch %d: %v", batchEnd+1, ctx.Err())
			}
		} else {
			resumeProviderGate(currentProviderGate)
		}
	}
	executionErr := <-taskDone
	taskDonePending = false
	if !errors.Is(executionErr, ErrCloudDSHCompletionPending) {
		models.mu.Lock()
		lastErr, attempts := models.lastErr, len(models.attempts)
		models.mu.Unlock()
		plannerCapture.mu.Lock()
		planCount := len(plannerCapture.plans)
		planFailures := append([]modelBudgetPostgresHTTPPlannerFailure(nil), plannerCapture.failures...)
		plannerCapture.mu.Unlock()
		t.Fatalf("Task compaction DSH execution = %v gateway_error=%v attempts=%d plans=%d plan_failures=%+v; want successful result awaiting separate Task completion",
			executionErr, lastErr, attempts, planCount, planFailures)
	}
	var appliedFollowups int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE task_id=$1 AND id=ANY($2::text[]) AND delivery_status='applied'`, claimed.ID, followupMessageIDs).Scan(&appliedFollowups); err != nil || appliedFollowups != followupCount {
		t.Fatalf("applied running Task followups=%d, want %d; err=%v", appliedFollowups, followupCount, err)
	}
	plannerCapture.mu.Lock()
	preparedRequests := append([]CloudDSHModelBudgetPlanRequest(nil), plannerCapture.requests...)
	preparedPlans := append([]CloudDSHModelBudgetPlan(nil), plannerCapture.plans...)
	plannerCapture.mu.Unlock()
	models.mu.Lock()
	capturedCalls := append([]app.ModelCall(nil), models.calls...)
	models.mu.Unlock()
	if len(preparedRequests) < 2 || len(preparedRequests) != len(preparedPlans) || len(capturedCalls) == 0 {
		t.Fatalf("Task DSH prepare/plan/Gateway call counts = %d/%d/%d; want prepared root and compaction calls plus a successful root call",
			len(preparedRequests), len(preparedPlans), len(capturedCalls))
	}
	compactionCount := 0
	compactionCallIDs := make(map[string]struct{})
	planByCallID := make(map[string]CloudDSHModelBudgetPlan, len(preparedRequests))
	for index, request := range preparedRequests {
		planByCallID[request.CallID] = preparedPlans[index]
		var config struct {
			Purpose string `json:"purpose"`
		}
		if err := json.Unmarshal(request.Config, &config); err != nil {
			t.Fatalf("decode prepare config %q: %v", request.CallID, err)
		}
		if config.Purpose == "compaction" {
			compactionCount++
			compactionCallIDs[request.CallID] = struct{}{}
		} else if config.Purpose != "" {
			t.Fatalf("Task DSH prepare %q has unexpected purpose %q", request.CallID, config.Purpose)
		}
	}
	if compactionCount < 2 {
		purposes := make([]string, 0, len(preparedRequests))
		for _, request := range preparedRequests {
			var config struct {
				Purpose string `json:"purpose"`
			}
			_ = json.Unmarshal(request.Config, &config)
			purposes = append(purposes, fmt.Sprintf("through=%d purpose=%q input_bytes=%d", request.ThroughSeq, config.Purpose, len(request.Request)))
		}
		t.Fatalf("real Node DSH emitted %d Task compaction prepares; want repeated compaction; purposes=%v", compactionCount, purposes)
	}

	callsByBudgetID := make(map[string]app.ModelCall, len(capturedCalls))
	for _, call := range capturedCalls {
		if call.Task == nil || call.Task.TaskID != claimed.ID || call.BudgetCallID == "" {
			t.Fatalf("final DSH Gateway call lost its Task/budget identity: task=%v budget_call=%q", call.Task != nil, call.BudgetCallID)
		}
		callsByBudgetID[call.BudgetCallID] = call
		plan, ok := planByCallID[call.BudgetCallID]
		if !ok || call.BudgetPlanID != plan.PlanID {
			t.Fatalf("Gateway call %q is not bound to its prepared plan: plan=%q prepared=%q", call.BudgetCallID, call.BudgetPlanID, plan.PlanID)
		}
	}
	type settledPreparation struct {
		planID, callID, dshCallID, budgetCallID, stage, ledgerState                string
		sendTokens, inputReservedTokens, reservedTokens, inputTokens, outputTokens int64
		consumed                                                                   bool
	}
	rows, err := pool.Query(ctx, `SELECT preparation.plan_id,preparation.call_id,preparation.dsh_call_id,
preparation.budget_call_id,preparation.stage,preparation.send_tokens,preparation.input_reserved_tokens,
ledger.reserved_tokens,ledger.input_tokens,ledger.output_tokens,preparation.consumed_at IS NOT NULL,ledger.state
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.task_id=$2 AND preparation.source='task'`, owner.ID, claimed.ID)
	if err != nil {
		t.Fatalf("query settled Task model budget preparations: %v", err)
	}
	preparations := make(map[string]settledPreparation, len(preparedRequests))
	for rows.Next() {
		var row settledPreparation
		if err := rows.Scan(&row.planID, &row.callID, &row.dshCallID, &row.budgetCallID, &row.stage,
			&row.sendTokens, &row.inputReservedTokens, &row.reservedTokens, &row.inputTokens, &row.outputTokens,
			&row.consumed, &row.ledgerState); err != nil {
			rows.Close()
			t.Fatalf("scan settled Task budget preparation: %v", err)
		}
		preparations[row.budgetCallID] = row
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate settled Task budget preparations: %v", err)
	}
	rows.Close()
	settledCompactions := 0
	settledCaps := make(map[int]int)
	settledCount := 0
	for _, request := range preparedRequests {
		row, ok := preparations[request.CallID]
		if !ok {
			if _, compaction := compactionCallIDs[request.CallID]; compaction {
				t.Fatalf("PostgreSQL did not persist DSH compaction prepare %q", request.CallID)
			}
			continue // A routed user message may safely cancel a prepared, not-yet-dispatched normal step.
		}
		call, ok := callsByBudgetID[request.CallID]
		if row.planID != planByCallID[request.CallID].PlanID || row.budgetCallID != request.CallID ||
			row.callID != request.DSHCallID || row.dshCallID != request.DSHCallID ||
			row.sendTokens != int64(planByCallID[request.CallID].MaxTokens) {
			t.Fatalf("PostgreSQL/DSH budget binding or settlement mismatch for %q: row=%+v worker_gateway_call=%t",
				request.CallID, row, ok)
		}
		if _, compaction := compactionCallIDs[request.CallID]; compaction {
			if row.stage != "summary" || row.ledgerState != "settled" || !row.consumed {
				t.Fatalf("Task compaction prepare %q did not settle at summary stage: row=%+v", request.CallID, row)
			}
			settledCompactions++
		} else if ok && (call.LedgerCallID != request.DSHCallID || call.BudgetRequestDigest != request.RequestDigest) {
			t.Fatalf("Worker Gateway call is not bound to the prepared DSH identity for %q: call=%+v", request.CallID, call)
		}
		if row.ledgerState != "settled" {
			if _, compaction := compactionCallIDs[request.CallID]; compaction {
				t.Fatalf("Task compaction prepare %q has unsettled ledger state %q", request.CallID, row.ledgerState)
			}
			continue
		}
		if !row.consumed || row.reservedTokens != row.inputReservedTokens+row.sendTokens || row.inputTokens != 8 || row.outputTokens != 6 {
			t.Fatalf("PostgreSQL settlement mismatch for %q: row=%+v", request.CallID, row)
		}
		settledCount++
		settledCaps[planByCallID[request.CallID].MaxTokens]++
	}
	if settledCompactions != compactionCount {
		t.Fatalf("settled Task compaction ledger rows=%d, compaction prepares=%d", settledCompactions, compactionCount)
	}
	providerClient.mu.Lock()
	providerCaps := append([]int(nil), providerClient.maxCaps...)
	providerClient.mu.Unlock()
	if providerCalls.Load() < int32(compactionCount) || len(providerCaps) != settledCount || len(capturedCalls) > followupCount+2 {
		t.Fatalf("provider/settled model request counts = %d/%d; settled DSH prepares=%d, Worker Gateway calls=%d, caps=%v; want every settled preparation represented by a provider response and repeated compaction dispatch",
			providerCalls.Load(), len(providerCaps), settledCount, len(capturedCalls), providerCaps)
	}
	expectedCaps := settledCaps
	actualCaps := make(map[int]int, len(providerCaps))
	for _, cap := range providerCaps {
		actualCaps[cap]++
	}
	if len(expectedCaps) != len(actualCaps) {
		t.Fatalf("provider/plan output cap sets differ: provider=%v plans=%v", actualCaps, expectedCaps)
	}
	for cap, count := range expectedCaps {
		if actualCaps[cap] != count {
			t.Fatalf("provider cap %d was dispatched %d times, prepared plan count=%d; provider=%v plans=%v",
				cap, actualCaps[cap], count, actualCaps, expectedCaps)
		}
	}
	if actualCaps[summaryCap] < compactionCount {
		t.Fatalf("fake provider saw %d summary-cap requests for %d DSH compaction prepares; provider caps=%v",
			actualCaps[summaryCap], compactionCount, actualCaps)
	}
	for index, call := range capturedCalls {
		var projection struct {
			MaxTokens int `json:"max_tokens"`
		}
		plan := planByCallID[call.BudgetCallID]
		if err := json.Unmarshal(call.RawRequest, &projection); err != nil || projection.MaxTokens != plan.MaxTokens {
			t.Fatalf("Gateway/plan cap[%d]=%d/%d err=%v", index, projection.MaxTokens, plan.MaxTokens, err)
		}
	}
}

func TestCloudDSHModelBudgetPostgresHTTPUsesVerifiedModelCapAboveLegacyLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,100000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatalf("configure synthetic account model budget: %v", err)
	}

	const (
		providerModel = "synthetic-budget-large-cap-v1"
		apiKey        = "synthetic-budget-large-cap-only"
		outputCap     = 40_000
	)
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/responses" ||
			request.Header.Get("Authorization") != "Bearer "+apiKey ||
			!bytes.HasPrefix([]byte(request.Header.Get("Content-Type")), []byte("application/json")) {
			http.Error(writer, "unexpected synthetic provider request", http.StatusBadRequest)
			return
		}
		var body modelBudgetPostgresHTTPProviderRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic provider request", http.StatusBadRequest)
			return
		}
		hasUserText := false
		for _, input := range body.Input {
			if input.Role != "user" {
				continue
			}
			for _, content := range input.Content {
				hasUserText = hasUserText || content.Type == "input_text" && content.Text != ""
			}
		}
		if body.Model != providerModel || body.MaxOutputTokens != outputCap || !hasUserText {
			http.Error(writer, "request did not preserve its signed model cap and input", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		// A short response keeps this protocol test within the currently deployed
		// response-text limit; this case proves the signed request cap only.
		_, _ = io.WriteString(writer, `{"model":"synthetic-budget-large-cap-v1","text":"Signed large-cap request verified.","input_tokens":8,"output_tokens":12}`)
	}))
	defer provider.Close()
	providerClient := &modelBudgetPostgresHTTPClient{endpoint: provider.URL + "/responses", client: provider.Client()}

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
		t.Fatalf("verify synthetic owner session: principal=%#v err=%v", principal, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, modelMaxOutput := 128_000, 64_000
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Budget large-cap synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: providerModel, APIKey: apiKey,
		ContextWindowOverrideTokens: &contextWindow, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic model route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatalf("enable synthetic text-only route: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, providerClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: outputCap, MinimumTokens: 128},
		{Stage: "tool_step", GoalTokens: outputCap, MinimumTokens: 128},
		{Stage: "subtask", GoalTokens: outputCap, MinimumTokens: 128},
		{Stage: "summary", GoalTokens: outputCap, MinimumTokens: 128},
		{Stage: "long_form", GoalTokens: outputCap, MinimumTokens: 128},
		{Stage: "text_continuation", GoalTokens: outputCap, MinimumTokens: 128},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "dsh-large-cap-e2e-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: connection.BaseURL, Model: providerModel, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: connection.ID, Objective: "Return the synthetic large-cap verification sentence.",
		Budget: task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 50_000}, IdempotencyKey: "budget-large-cap-submit-001",
	})
	if err != nil {
		t.Fatalf("submit synthetic large-cap E2E Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-large-cap-task-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != submission.Task.ID {
		t.Fatalf("claim synthetic large-cap E2E Task: found=%t task=%q err=%v", found, claimed.ID, err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the actual DSH runtime cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, runtimeScript,
		"budget-large-cap-supervisor", bytes.Repeat([]byte{0x62}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	plannerCapture := &modelBudgetPostgresHTTPPlannerCapture{inner: planner}
	models := &modelBudgetPostgresHTTPModels{ModelGatewayService: gateway}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "budget-large-cap-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: models, ModelBudgetPlanner: plannerCapture,
		ToolCapabilities: cloudDSHTestToolCapability{allowed: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteTask(ctx, claimed); !errors.Is(err, ErrCloudDSHCompletionPending) {
		models.mu.Lock()
		lastErr, attempts := models.lastErr, len(models.attempts)
		models.mu.Unlock()
		plannerCapture.mu.Lock()
		planCount := len(plannerCapture.plans)
		plannerCapture.mu.Unlock()
		t.Fatalf("large-cap DSH execution = %v, gateway_error=%v attempts=%d plans=%d; want successful result awaiting separate Task completion",
			err, lastErr, attempts, planCount)
	}

	plannerCapture.mu.Lock()
	preparedRequests := append([]CloudDSHModelBudgetPlanRequest(nil), plannerCapture.requests...)
	preparedPlans := append([]CloudDSHModelBudgetPlan(nil), plannerCapture.plans...)
	plannerCapture.mu.Unlock()
	if len(preparedRequests) != 1 || len(preparedPlans) != 1 || preparedPlans[0].MaxTokens != outputCap {
		t.Fatalf("large-cap DSH prepare requests/plans = %d/%#v, want one plan capped at %d", len(preparedRequests), preparedPlans, outputCap)
	}
	models.mu.Lock()
	calls := append([]app.ModelCall(nil), models.calls...)
	models.mu.Unlock()
	if len(calls) != 1 || calls[0].Task == nil || calls[0].BudgetPlanID != preparedPlans[0].PlanID ||
		calls[0].BudgetCallID != preparedRequests[0].CallID {
		t.Fatalf("large-cap final DSH calls = %d, want one call bound to its PostgreSQL plan", len(calls))
	}
	var finalProjection struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(calls[0].RawRequest, &finalProjection); err != nil || finalProjection.MaxTokens != outputCap {
		t.Fatalf("large-cap final DSH max_tokens=%d err=%v, want %d", finalProjection.MaxTokens, err, outputCap)
	}
	providerClient.mu.Lock()
	providerCaps := append([]int(nil), providerClient.maxCaps...)
	providerClient.mu.Unlock()
	if providerCalls.Load() != 1 || len(providerCaps) != 1 || providerCaps[0] != outputCap {
		t.Fatalf("large-cap fake provider calls/caps = %d/%v, want one HTTP request at %d", providerCalls.Load(), providerCaps, outputCap)
	}

	var ledgerState string
	var sendTokens, inputReservedTokens, reservedTokens, actualInputTokens, actualOutputTokens int64
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT ledger.state,preparation.send_tokens,preparation.input_reserved_tokens,
ledger.reserved_tokens,ledger.input_tokens,ledger.output_tokens,(preparation.consumed_at IS NOT NULL)
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.task_id=$2 AND preparation.source='task'`, owner.ID, submission.Task.ID).
		Scan(&ledgerState, &sendTokens, &inputReservedTokens, &reservedTokens, &actualInputTokens, &actualOutputTokens, &consumed); err != nil {
		t.Fatalf("read settled large-cap budget ledger: %v", err)
	}
	if ledgerState != "settled" || !consumed || sendTokens != int64(outputCap) ||
		reservedTokens != inputReservedTokens+sendTokens || actualInputTokens != 8 || actualOutputTokens != 12 {
		t.Fatalf("large-cap ledger state/plan/usage mismatch: state=%q consumed=%t send=%d reserved=%d input_hold=%d usage=%d/%d",
			ledgerState, consumed, sendTokens, reservedTokens, inputReservedTokens, actualInputTokens, actualOutputTokens)
	}
	models.mu.Lock()
	firstResponse, replayResponse := models.first, models.replay
	models.mu.Unlock()
	if firstResponse.Text != "Signed large-cap request verified." || replayResponse.Text != firstResponse.Text ||
		replayResponse.Usage != firstResponse.Usage || providerCalls.Load() != 1 {
		t.Fatal("large-cap settled replay did not return the saved response without another provider request")
	}
}

func TestCloudDSHModelBudgetPostgresHTTPFailureEdges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,100000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatalf("configure synthetic account model budget: %v", err)
	}

	const (
		providerModel = "synthetic-budget-edge-v1"
		apiKey        = "synthetic-budget-edge-only"
		outputCap     = 3072
	)
	const (
		providerOK int32 = iota
		providerHTTPFailure
		providerTimeout
		providerCancel
	)
	var providerMode atomic.Int32
	var providerCalls atomic.Int32
	var cancelMu sync.Mutex
	var cancelTask context.CancelFunc
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/responses" ||
			request.Header.Get("Authorization") != "Bearer "+apiKey ||
			!bytes.HasPrefix([]byte(request.Header.Get("Content-Type")), []byte("application/json")) {
			http.Error(writer, "unexpected synthetic provider request", http.StatusBadRequest)
			return
		}
		var body modelBudgetPostgresHTTPProviderRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic provider request", http.StatusBadRequest)
			return
		}
		hasUserText := false
		for _, input := range body.Input {
			if input.Role != "user" {
				continue
			}
			for _, content := range input.Content {
				hasUserText = hasUserText || content.Type == "input_text" && content.Text != ""
			}
		}
		if body.Model != providerModel || body.MaxOutputTokens != outputCap || !hasUserText {
			http.Error(writer, "request did not preserve its signed model cap and input", http.StatusBadRequest)
			return
		}
		switch providerMode.Load() {
		case providerHTTPFailure:
			http.Error(writer, "synthetic upstream unavailable", http.StatusServiceUnavailable)
		case providerTimeout:
			<-request.Context().Done()
		case providerCancel:
			cancelMu.Lock()
			stopTask := cancelTask
			cancelMu.Unlock()
			if stopTask != nil {
				stopTask()
			}
			<-request.Context().Done()
		default:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"model":"synthetic-budget-edge-v1","text":"Synthetic DSH budget edge verified.","input_tokens":8,"output_tokens":6}`)
		}
	}))
	defer provider.Close()
	providerHTTPClient := provider.Client()
	providerHTTPClient.Timeout = 250 * time.Millisecond
	providerClient := &modelBudgetPostgresHTTPClient{endpoint: provider.URL + "/responses", client: providerHTTPClient}

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
		t.Fatalf("verify synthetic owner session: principal=%#v err=%v", principal, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, providerClient, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, modelMaxOutput := 32768, 8192
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Budget edge synthetic route", BaseURL: "https://api.openai.com/v1", ModelName: providerModel, APIKey: apiKey,
		ContextWindowOverrideTokens: &contextWindow, ContextWindowOverridePresent: true,
		MaxOutputOverrideTokens: &modelMaxOutput, MaxOutputOverridePresent: true,
	})
	if err != nil {
		t.Fatalf("create synthetic model route: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatalf("enable synthetic text-only route: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, providerClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: outputCap, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: outputCap, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "dsh-pg-http-edge-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: connection.BaseURL, Model: providerModel, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the actual DSH runtime cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, runtimeScript,
		"budget-edge-supervisor", bytes.Repeat([]byte{0x52}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	plannerCapture := &modelBudgetPostgresHTTPPlannerCapture{inner: planner}
	models := &modelBudgetPostgresHTTPModels{ModelGatewayService: gateway}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "budget-edge-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: models, ModelBudgetPlanner: plannerCapture,
		ToolCapabilities: cloudDSHTestToolCapability{allowed: false},
	})
	if err != nil {
		t.Fatal(err)
	}

	runTask := func(key string, budget task.Budget, cancelOnProvider bool) (app.ModelCall, error) {
		submission, submitErr := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
			ModelID: connection.ID, Objective: "Reply with the synthetic budget edge verification sentence.",
			Budget: budget, IdempotencyKey: key,
		})
		if submitErr != nil {
			return app.ModelCall{}, fmt.Errorf("submit edge Task %q: %w", key, submitErr)
		}
		claimed, found, claimErr := store.ClaimNextTask(ctx, "budget-edge-task-worker", 2*time.Minute)
		if claimErr != nil || !found || claimed.ID != submission.Task.ID {
			return app.ModelCall{}, fmt.Errorf("claim edge Task %q: found=%t task=%q err=%w", key, found, claimed.ID, claimErr)
		}
		taskCtx, cancelTaskCtx := context.WithTimeout(ctx, 60*time.Second)
		if cancelOnProvider {
			cancelMu.Lock()
			cancelTask = cancelTaskCtx
			cancelMu.Unlock()
		}
		executionErr := executor.ExecuteTask(taskCtx, claimed)
		cancelTaskCtx()
		cancelMu.Lock()
		cancelTask = nil
		cancelMu.Unlock()
		models.mu.Lock()
		defer models.mu.Unlock()
		if len(models.attempts) == 0 {
			return app.ModelCall{}, fmt.Errorf("edge Task %q reached no Gateway attempts", key)
		}
		return models.attempts[len(models.attempts)-1], executionErr
	}

	assertLedger := func(call app.ModelCall, wantState string) error {
		if call.Task == nil || call.LedgerCallID == "" || call.BudgetPlanID == "" {
			return fmt.Errorf("captured DSH call is missing Task/budget identity: task=%v ledger=%q plan=%q", call.Task != nil, call.LedgerCallID, call.BudgetPlanID)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
			owner.ID, call.LedgerCallID).Scan(&state); err != nil {
			return fmt.Errorf("read model ledger %q: %w", call.LedgerCallID, err)
		}
		if state != wantState {
			return fmt.Errorf("model ledger %q state=%q, want %q", call.LedgerCallID, state, wantState)
		}
		return nil
	}

	providerMode.Store(providerOK)
	models.mu.Lock()
	models.cancelBeforeNextGateway = true
	models.afterNoDispatch = func(call app.ModelCall, callErr error) error {
		if callErr == nil {
			return errors.New("canceled pre-dispatch Gateway attempt had no error")
		}
		if err := assertLedger(call, "prepared"); err != nil {
			return err
		}
		if calls := providerCalls.Load(); calls != 0 {
			return fmt.Errorf("provider was contacted before the recoverable retry: calls=%d", calls)
		}
		return nil
	}
	models.mu.Unlock()
	call, executionErr := runTask("budget-edge-pre-dispatch-cancel-001", task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 50000}, false)
	if !errors.Is(executionErr, ErrCloudDSHCompletionPending) {
		t.Fatalf("pre-dispatch cancel/retry Task result=%v, want successful model result awaiting Task completion", executionErr)
	}
	if err := assertLedger(call, "settled"); err != nil || providerCalls.Load() != 1 {
		t.Fatalf("pre-dispatch cancellation did not recover safely: ledger_error=%v provider_calls=%d", err, providerCalls.Load())
	}

	unknownCases := []struct {
		name            string
		mode            int32
		cancelOnRequest bool
	}{
		{name: "provider timeout", mode: providerTimeout},
		{name: "provider HTTP failure", mode: providerHTTPFailure},
		{name: "Task cancellation after provider dispatch", mode: providerCancel, cancelOnRequest: true},
	}
	for index, testCase := range unknownCases {
		t.Run(testCase.name, func(t *testing.T) {
			providerMode.Store(testCase.mode)
			callsBefore := providerCalls.Load()
			models.mu.Lock()
			models.replayAfterNextGatewayError = true
			models.afterGatewayErrorAndReplay = func(call app.ModelCall, callErr, replayErr error) error {
				if callErr == nil || replayErr == nil {
					return fmt.Errorf("provider failure/replay errors = %v/%v, want both errors", callErr, replayErr)
				}
				if err := assertLedger(call, "unknown"); err != nil {
					return err
				}
				if calls := providerCalls.Load(); calls != callsBefore+1 {
					return fmt.Errorf("unknown call/replay provider dispatch count=%d, want exactly one new HTTP request", calls-callsBefore)
				}
				return nil
			}
			models.mu.Unlock()
			budget := task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 50000}
			key := fmt.Sprintf("budget-edge-unknown-%03d", index+1)
			call, executionErr := runTask(key, budget, testCase.cancelOnRequest)
			if executionErr == nil {
				t.Fatal("DSH Task succeeded after an unknown provider result")
			}
			if err := assertLedger(call, "unknown"); err != nil {
				t.Fatal(err)
			}
			if calls := providerCalls.Load(); calls != callsBefore+1 {
				t.Fatalf("unknown provider outcome caused %d HTTP requests, want exactly one", calls-callsBefore)
			}
		})
	}

	const dispatchFaultSQL = `CREATE OR REPLACE FUNCTION fail_model_budget_dispatch_test() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' THEN
    RAISE EXCEPTION 'injected prepared-to-dispatched failure';
  END IF;
  IF TG_OP = 'DELETE' AND OLD.state = 'prepared' THEN
    RAISE EXCEPTION 'injected prepared reservation release failure';
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER fail_model_budget_dispatch_test
BEFORE UPDATE OF state ON model_call_ledger
FOR EACH ROW EXECUTE FUNCTION fail_model_budget_dispatch_test();
CREATE TRIGGER fail_model_budget_release_test
BEFORE DELETE ON model_call_ledger
FOR EACH ROW EXECUTE FUNCTION fail_model_budget_dispatch_test();`
	const dropDispatchFaultSQL = `DROP TRIGGER IF EXISTS fail_model_budget_dispatch_test ON model_call_ledger;
DROP TRIGGER IF EXISTS fail_model_budget_release_test ON model_call_ledger;
DROP FUNCTION IF EXISTS fail_model_budget_dispatch_test();`
	dropDispatchFault := func() error {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, err := pool.Exec(cleanupCtx, dropDispatchFaultSQL)
		return err
	}
	t.Cleanup(func() {
		if err := dropDispatchFault(); err != nil {
			t.Errorf("drop injected model dispatch fault: %v", err)
		}
	})
	callsBefore := providerCalls.Load()
	providerMode.Store(providerOK)
	models.mu.Lock()
	models.beforeNextGateway = func(app.ModelCall) error {
		if _, err := pool.Exec(ctx, dispatchFaultSQL); err != nil {
			return fmt.Errorf("install model dispatch fault trigger: %w", err)
		}
		return nil
	}
	models.retryAfterNextGatewayError = true
	models.afterGatewayErrorBeforeRetry = func(call app.ModelCall, firstErr error) error {
		if firstErr == nil {
			return errors.New("injected dispatch failure did not reach Gateway.Complete")
		}
		if err := assertLedger(call, "prepared"); err != nil {
			return fmt.Errorf("dispatch transaction failure changed prepared ledger: %w", err)
		}
		var consumed bool
		if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM model_budget_preparations
WHERE owner_id=$1 AND source='task' AND call_id=$2 AND plan_id=$3`, owner.ID, call.LedgerCallID, call.BudgetPlanID).Scan(&consumed); err != nil {
			return fmt.Errorf("read failed dispatch preparation consumption: %w", err)
		}
		if consumed {
			return errors.New("failed dispatch transaction consumed its prepared budget plan")
		}
		if calls := providerCalls.Load(); calls != callsBefore {
			return fmt.Errorf("provider HTTP count after dispatch transaction failure=%d, want %d", calls, callsBefore)
		}
		if err := dropDispatchFault(); err != nil {
			return fmt.Errorf("remove injected model dispatch fault before retry: %w", err)
		}
		return nil
	}
	models.mu.Unlock()

	call, executionErr = runTask("budget-edge-dispatch-transaction-failure-001",
		task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 50000}, false)
	if !errors.Is(executionErr, ErrCloudDSHCompletionPending) {
		models.mu.Lock()
		lastErr := models.lastErr
		models.mu.Unlock()
		t.Fatalf("dispatch transaction failure/retry Task result=%v, gateway error=%v, provider calls=%d; want successful model result awaiting Task completion",
			executionErr, lastErr, providerCalls.Load())
	}
	if err := assertLedger(call, "settled"); err != nil || providerCalls.Load() != callsBefore+1 {
		t.Fatalf("dispatch transaction retry did not settle exactly once: ledger_error=%v provider_calls=%d before=%d",
			err, providerCalls.Load(), callsBefore)
	}
}
