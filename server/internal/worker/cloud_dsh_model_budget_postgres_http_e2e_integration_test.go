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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
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
}

func (capture *modelBudgetPostgresHTTPPlannerCapture) PrepareModelBudget(ctx context.Context,
	request CloudDSHModelBudgetPlanRequest) (CloudDSHModelBudgetPlan, error) {
	plan, err := capture.inner.PrepareModelBudget(ctx, request)
	if err != nil {
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

type modelBudgetPostgresHTTPModels struct {
	*app.ModelGatewayService
	mu       sync.Mutex
	attempts []app.ModelCall
	calls    []app.ModelCall
	first    app.ModelResponse
	replay   app.ModelResponse
	lastErr  error
}

func (models *modelBudgetPostgresHTTPModels) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	models.mu.Lock()
	models.attempts = append(models.attempts, call)
	models.mu.Unlock()
	response, err := models.ModelGatewayService.Complete(ctx, call)
	if err != nil {
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
		{Stage: "short_answer", GoalTokens: outputCap, MinimumTokens: 64},
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
