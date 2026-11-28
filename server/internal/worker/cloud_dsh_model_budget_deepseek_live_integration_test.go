package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/model"
)

type liveDeepSeekBudgetModelClient struct {
	app.ModelClient
	mu        sync.Mutex
	calls     int
	maxCalls  int
	caps      []int
	last      liveDeepSeekBudgetModelResponseSummary
	summaries []liveDeepSeekBudgetModelResponseSummary
}

type liveDeepSeekBudgetModelResponseSummary struct {
	ProviderModel       string
	StopReason          string
	UsageKnown          bool
	InputTokens         int64
	OutputTokens        int64
	TextBytes           int
	TextRunes           int
	ReasoningBytes      int
	ToolCalls           int
	KnownTextTruncation bool
}

func (client *liveDeepSeekBudgetModelClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	client.mu.Lock()
	client.calls++
	callNumber := client.calls
	client.caps = append(client.caps, request.MaxOutputTokens)
	client.mu.Unlock()
	if client.maxCalls > 0 && callNumber > client.maxCalls {
		return app.ModelResponse{}, errors.New("live DeepSeek test provider call limit exceeded")
	}
	response, err := client.ModelClient.Complete(ctx, request)
	client.mu.Lock()
	client.last = liveDeepSeekBudgetModelResponseSummary{
		ProviderModel: response.ProviderReportedModel, StopReason: response.StopReason, UsageKnown: response.Usage.Known,
		InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens,
		TextBytes: len(response.Text), TextRunes: len([]rune(response.Text)),
		ReasoningBytes: len(response.Reasoning), ToolCalls: len(response.ToolCalls), KnownTextTruncation: response.KnownTextTruncation,
	}
	client.summaries = append(client.summaries, client.last)
	client.mu.Unlock()
	return response, err
}

func TestCloudDSHModelBudgetPostgresHTTPDeepSeekOutputAbove2048Settles(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("MORDE_TEST_DEEPSEEK_KEY"))
	if apiKey == "" {
		t.Skip("MORDE_TEST_DEEPSEEK_KEY is unset; live DeepSeek DSH/PostgreSQL test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,100000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, owner.ID); err != nil {
		t.Fatalf("configure live model account budget: %v", err)
	}

	const (
		providerModel = "deepseek-flash"
		outputCap     = 16384
	)
	responsesClient, err := model.NewResponsesClient([]string{"api.deepseek.com"}, os.Getenv("MORDE_TEST_MODEL_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	liveClient := &liveDeepSeekBudgetModelClient{ModelClient: responsesClient, maxCalls: 1}

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
		t.Fatalf("verify live model owner session: kind=%q err=%v", principal.Kind, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.NewModelConnectionService(store, sealer, liveClient, []string{"api.deepseek.com"})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "Live DeepSeek budget E2E", BaseURL: "https://api.deepseek.com", ModelName: providerModel, APIKey: apiKey,
	})
	if err != nil {
		t.Fatalf("create encrypted live DeepSeek route: %v", err)
	}
	if connection.MaxOutputTokens == nil || *connection.MaxOutputTokens < outputCap || connection.ContextWindowTokens == nil {
		t.Fatalf("DeepSeek route has insufficient verified capability metadata: max_output=%v context=%v source=%q",
			connection.MaxOutputTokens, connection.ContextWindowTokens, connection.MaxOutputSource)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, connection.ID); err != nil {
		t.Fatalf("enable verified text-only DeepSeek route: %v", err)
	}
	gateway, err := app.NewModelGatewayService(store, connections, liveClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: outputCap, MinimumTokens: 512},
		{Stage: "tool_step", GoalTokens: outputCap, MinimumTokens: 512},
		{Stage: "subtask", GoalTokens: outputCap, MinimumTokens: 512},
		{Stage: "summary", GoalTokens: outputCap, MinimumTokens: 512},
		{Stage: "long_form", GoalTokens: outputCap, MinimumTokens: 512},
		{Stage: "text_continuation", GoalTokens: outputCap, MinimumTokens: 512},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "live-deepseek-dsh-budget-e2e-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: connection.BaseURL, Model: providerModel, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	const objective = "请用中文写一篇约 2600 个汉字的技术科普文章，主题是分布式系统中的幂等、账本状态与崩溃恢复。分成 8 个小节，解释请求身份、幂等键、预算预留、模型派发、结算和未知结果，并用一个贯穿全文的例子说明。只写正文，不联网、不调用工具、不添加链接。按约定篇幅完整写完，不要用摘要代替。"
	submission, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: connection.ID, Objective: objective,
		Budget:         task.Budget{ActiveSeconds: 180, ToolCalls: 1, ModelTokens: 50_000},
		IdempotencyKey: "budget-live-deepseek-submit-001",
	})
	if err != nil {
		t.Fatalf("submit live DeepSeek budget E2E Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "budget-live-deepseek-task-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != submission.Task.ID {
		t.Fatalf("claim live DeepSeek budget E2E Task: found=%t task_matches=%t err=%v", found, claimed.ID == submission.Task.ID, err)
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
		"budget-live-deepseek-supervisor", bytes.Repeat([]byte{0x74}, 32))
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
		SupervisorID: "budget-live-deepseek-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
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
		liveClient.mu.Lock()
		responseSummary := liveClient.last
		liveClient.mu.Unlock()
		t.Fatalf("live DeepSeek DSH execution = %v, gateway_error=%v attempts=%d provider_response=%+v; want successful Task result awaiting separate completion",
			err, lastErr, attempts, responseSummary)
	}

	plannerCapture.mu.Lock()
	preparedRequests := append([]CloudDSHModelBudgetPlanRequest(nil), plannerCapture.requests...)
	preparedPlans := append([]CloudDSHModelBudgetPlan(nil), plannerCapture.plans...)
	plannerCapture.mu.Unlock()
	if len(preparedRequests) != 1 || len(preparedPlans) != 1 {
		t.Fatalf("live DeepSeek prepare requests/plans = %d/%d; want one long-form call", len(preparedRequests), len(preparedPlans))
	}
	plansByCallID := make(map[string]CloudDSHModelBudgetPlan, len(preparedPlans))
	for index, request := range preparedRequests {
		plan := preparedPlans[index]
		if plan.MaxTokens != outputCap || request.CallID == "" || plan.PlanID == "" {
			t.Fatalf("live DeepSeek plan[%d] = call_id=%q plan=%+v; want a signed %d-token plan", index, request.CallID, plan, outputCap)
		}
		plansByCallID[request.CallID] = plan
	}
	models.mu.Lock()
	calls := append([]app.ModelCall(nil), models.calls...)
	firstResponse, replayResponse := models.first, models.replay
	models.mu.Unlock()
	if len(calls) != 1 || len(calls) != len(preparedPlans) {
		t.Fatalf("live DeepSeek DSH calls/plans = %d/%d; want one settled call", len(calls), len(preparedPlans))
	}
	for index, call := range calls {
		plan, ok := plansByCallID[call.BudgetCallID]
		if !ok || call.Task == nil || call.Task.TaskID != claimed.ID || call.BudgetPlanID != plan.PlanID {
			t.Fatalf("live DeepSeek call[%d] is not bound to its Task and preparation: call=%+v plan=%+v", index, call, plan)
		}
		var projection struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(call.RawRequest, &projection); err != nil || projection.MaxTokens != outputCap {
			t.Fatalf("final DSH output cap[%d] = %d err=%v, want %d", index, projection.MaxTokens, err, outputCap)
		}
	}
	if !firstResponse.Usage.Known || firstResponse.StopReason != "stop" || replayResponse.Text != firstResponse.Text || replayResponse.Usage != firstResponse.Usage {
		t.Fatalf("DeepSeek final continuation/replay mismatch: output_tokens=%d text_bytes=%d stop=%q known=%t replay_matches=%t",
			firstResponse.Usage.OutputTokens, len(firstResponse.Text), firstResponse.StopReason, firstResponse.Usage.Known,
			replayResponse.Text == firstResponse.Text && replayResponse.Usage == firstResponse.Usage)
	}
	liveClient.mu.Lock()
	providerCalls := liveClient.calls
	providerCaps := append([]int(nil), liveClient.caps...)
	providerSummaries := append([]liveDeepSeekBudgetModelResponseSummary(nil), liveClient.summaries...)
	liveClient.mu.Unlock()
	var actualInputTokens, actualOutputTokens int64
	var visibleTextBytes, visibleTextRunes int
	for index, summary := range providerSummaries {
		if !summary.UsageKnown || summary.ToolCalls != 0 || summary.ProviderModel != providerModel || summary.OutputTokens < 0 || summary.OutputTokens > outputCap {
			t.Fatalf("DeepSeek continuation[%d] response summary is invalid: %+v", index, summary)
		}
		actualInputTokens += summary.InputTokens
		actualOutputTokens += summary.OutputTokens
		visibleTextBytes += summary.TextBytes
		visibleTextRunes += summary.TextRunes
	}
	capsMatch := len(providerCaps) == len(calls)
	for _, cap := range providerCaps {
		capsMatch = capsMatch && cap == outputCap
	}
	if providerCalls != 1 || len(providerSummaries) != 1 || providerCalls != len(calls) || !capsMatch ||
		actualOutputTokens <= 2048 || visibleTextRunes < 2200 {
		t.Fatalf("real provider calls/caps = %d/%v; usage=%d/%d visible_chars=%d bytes=%d; want one complete request at %d and >2048 output tokens",
			providerCalls, providerCaps, actualInputTokens, actualOutputTokens, visibleTextRunes, visibleTextBytes, outputCap)
	}

	rows, err := pool.Query(ctx, `SELECT preparation.budget_call_id,ledger.state,preparation.stage,preparation.send_tokens,preparation.input_reserved_tokens,
ledger.reserved_tokens,ledger.input_tokens,ledger.output_tokens,(preparation.consumed_at IS NOT NULL)
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
	WHERE preparation.owner_id=$1 AND preparation.task_id=$2 AND preparation.source='task'`, owner.ID, claimed.ID)
	if err != nil {
		t.Fatalf("read settled live DeepSeek budget ledger: %v", err)
	}
	defer rows.Close()
	var ledgerCalls, rootStageCalls, continuationStageCalls int
	var ledgerInputTokens, ledgerOutputTokens int64
	for rows.Next() {
		var callID, ledgerState, stage string
		var sendTokens, inputReservedTokens, reservedTokens, inputTokens, outputTokens int64
		var consumed bool
		if err := rows.Scan(&callID, &ledgerState, &stage, &sendTokens, &inputReservedTokens, &reservedTokens, &inputTokens, &outputTokens, &consumed); err != nil {
			t.Fatalf("scan settled live DeepSeek budget ledger: %v", err)
		}
		if ledgerState != "settled" || (stage != "root_text_only" && stage != "text_continuation") || !consumed || sendTokens != outputCap ||
			reservedTokens != inputReservedTokens+sendTokens || plansByCallID[callID].PlanID == "" {
			t.Fatalf("live DeepSeek ledger row mismatch: call=%q state=%q stage=%q consumed=%t send=%d reserved=%d input_hold=%d",
				callID, ledgerState, stage, consumed, sendTokens, reservedTokens, inputReservedTokens)
		}
		if stage == "root_text_only" {
			rootStageCalls++
		} else {
			continuationStageCalls++
		}
		ledgerCalls++
		ledgerInputTokens += inputTokens
		ledgerOutputTokens += outputTokens
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate settled live DeepSeek budget ledger: %v", err)
	}
	if ledgerCalls != len(calls) || rootStageCalls != 1 || continuationStageCalls != 0 ||
		ledgerInputTokens != actualInputTokens || ledgerOutputTokens != actualOutputTokens || ledgerOutputTokens <= 2048 {
		t.Fatalf("live DeepSeek ledger differs from provider usage: calls=%d/%d stages=%d/%d usage=%d/%d provider=%d/%d",
			ledgerCalls, len(calls), rootStageCalls, continuationStageCalls, ledgerInputTokens, ledgerOutputTokens, actualInputTokens, actualOutputTokens)
	}
	t.Logf("DSH + PostgreSQL + DeepSeek settled: calls=%d cap=%d output_tokens=%d visible_chars=%d input_tokens=%d",
		providerCalls, outputCap, actualOutputTokens, visibleTextRunes, actualInputTokens)
}
