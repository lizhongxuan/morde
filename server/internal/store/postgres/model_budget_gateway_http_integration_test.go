package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/worker"
)

type budgetResponsesHTTPClient struct {
	endpoint string
	client   *http.Client
}

type budgetResponsesInput struct {
	Type    string                   `json:"type"`
	Role    string                   `json:"role"`
	Content []budgetResponsesContent `json:"content"`
}

type budgetResponsesContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type budgetResponsesRequest struct {
	Model           string                 `json:"model"`
	Instructions    string                 `json:"instructions,omitempty"`
	Input           []budgetResponsesInput `json:"input"`
	MaxOutputTokens int                    `json:"max_output_tokens"`
}

type budgetResponsesHTTPResponse struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (client budgetResponsesHTTPClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	payload := budgetResponsesRequest{
		Model: request.Endpoint.ModelID, Instructions: request.Instructions,
		MaxOutputTokens: request.MaxOutputTokens,
	}
	for _, input := range request.Input {
		if input.Type != "message" {
			return app.ModelResponse{}, fmt.Errorf("unexpected budget test input type %q", input.Type)
		}
		item := budgetResponsesInput{Type: "message", Role: input.Role}
		if input.Text != "" {
			item.Content = append(item.Content, budgetResponsesContent{Type: "input_text", Text: input.Text})
		}
		for _, part := range input.Parts {
			if part.Type != "text" {
				return app.ModelResponse{}, fmt.Errorf("unexpected budget test input part %q", part.Type)
			}
			item.Content = append(item.Content, budgetResponsesContent{Type: "input_text", Text: part.Text})
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
	requestHTTP.Header.Set("Authorization", "Bearer synthetic-only")
	requestHTTP.Header.Set("Content-Type", "application/json")
	responseHTTP, err := client.client.Do(requestHTTP)
	if err != nil {
		return app.ModelResponse{}, err
	}
	defer func() { _ = responseHTTP.Body.Close() }()
	if responseHTTP.StatusCode != http.StatusOK {
		return app.ModelResponse{}, fmt.Errorf("synthetic OpenAI-compatible provider returned HTTP %d", responseHTTP.StatusCode)
	}
	var response budgetResponsesHTTPResponse
	if err := json.NewDecoder(io.LimitReader(responseHTTP.Body, 1<<20)).Decode(&response); err != nil {
		return app.ModelResponse{}, err
	}
	var text string
	for _, output := range response.Output {
		if output.Type != "message" {
			continue
		}
		for _, content := range output.Content {
			if content.Type == "output_text" {
				text += content.Text
			}
		}
	}
	return app.ModelResponse{
		Text: text, StopReason: "stop", ProviderReportedModel: response.Model,
		Usage: app.ModelUsage{Known: response.Usage.InputTokens >= 0 && response.Usage.OutputTokens >= 0,
			InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens},
	}, nil
}

func (budgetResponsesHTTPClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, fmt.Errorf("unexpected model capability check")
}

func TestPostgresAdaptiveBudgetPlannerGatewayAndOpenAICompatibleHTTPUseOneOutputCap(t *testing.T) {
	fixture, _, _, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	store := fixture.store
	fence.AgentRootSessionID = fence.AgentSessionID

	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET base_url='https://models.invalid/v1',
context_window_tokens=32768,context_window_override_tokens=32768,context_window_source='legacy_manual',
context_window_resolved_at=NULL,context_window_catalog_version=NULL,max_output_tokens=8192,
max_output_override_tokens=8192,max_output_source='override',max_output_resolved_at=NULL,
max_output_catalog_version=NULL,max_output_source_url=NULL,updated_at=clock_timestamp() WHERE id=$1`, call.ModelID); err != nil {
		t.Fatalf("configure exact test route capacity: %v", err)
	}
	var connectionVersion time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, call.ModelID).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	contextWindow, maxOutput := 32768, 8192
	endpoint := app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://models.invalid/v1", APIKey: "synthetic-only", ModelID: "test"},
		Version: connectionVersion, Personal: true, ContextWindowTokens: &contextWindow,
		ContextWindowSource: app.ModelContextWindowLegacyManual, MaxOutputTokens: &maxOutput, MaxOutputSource: app.ModelMaxOutputOverride}
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" ||
			request.Header.Get("Authorization") != "Bearer synthetic-only" ||
			!strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
			http.Error(writer, "unexpected synthetic provider request", http.StatusBadRequest)
			return
		}
		var body budgetResponsesRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic provider request", http.StatusBadRequest)
			return
		}
		if body.Model != endpoint.Endpoint.ModelID || body.MaxOutputTokens != 3072 || len(body.Input) != 1 ||
			body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 ||
			body.Input[0].Content[0].Type != "input_text" || body.Input[0].Content[0].Text == "" {
			http.Error(writer, "request did not preserve its signed provider projection", http.StatusBadRequest)
			return
		}
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"completed","model":"synthetic-provider-v1","output":[{"type":"message","content":[{"type":"output_text","text":"Adaptive HTTP budget verified."}]}],"usage":{"input_tokens":8,"output_tokens":6}}`)
	}))
	defer provider.Close()
	client := budgetResponsesHTTPClient{endpoint: provider.URL + "/v1/responses", client: provider.Client()}
	gateway, err := app.NewModelGatewayService(store, adaptiveBudgetGatewayResolver{endpoint: endpoint}, client)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "short_answer", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: 3072, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "pg-http-integration-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: endpoint.Endpoint.BaseURL, Model: endpoint.Endpoint.ModelID, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}
	providerProjection := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"adaptive budget integration"}]}],"tools":[]}`)
	requestDigest := adaptiveBudgetProjectionDigest(t, providerProjection)
	planner, err := worker.NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	dshCallID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	budgetCallID := "budget-call-openai-http-0001"
	plan, err := planner.PrepareModelBudget(ctx, worker.CloudDSHModelBudgetPlanRequest{
		Fence: app.AgentInstanceFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
			AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, AuthorizationVersion: fence.AuthorizationVersion,
			EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion},
		TaskRevision: fence.TaskRevision, SessionID: fence.AgentSessionID, ModelID: call.ModelID,
		ThroughSeq: fence.ThroughSequence, Turn: 1, Step: 0, CallID: budgetCallID, DSHCallID: dshCallID,
		RequestDigest: requestDigest, Request: providerProjection,
	})
	if err != nil {
		t.Fatalf("prepare through the Worker planner adapter and PostgreSQL ledger: %v", err)
	}
	if plan.PlanID == "" || plan.MaxTokens != 3072 {
		t.Fatalf("prepared output plan = %#v, want a persisted plan with 3072 tokens", plan)
	}
	var finalRequest map[string]any
	if err := json.Unmarshal(providerProjection, &finalRequest); err != nil {
		t.Fatal(err)
	}
	finalRequest["max_tokens"] = plan.MaxTokens
	finalProjection, err := json.Marshal(finalRequest)
	if err != nil {
		t.Fatal(err)
	}
	call.Source = app.ModelCallTask
	call.Task = &fence
	call.CallID, err = app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call.RawRequest = finalProjection
	call.BudgetPlanID, call.BudgetCallID, call.BudgetRequestDigest = plan.PlanID, budgetCallID, requestDigest
	response, err := gateway.Complete(ctx, call)
	if err != nil {
		t.Fatalf("dispatch and settle through ModelGateway/PostgreSQL/HTTP: %v", err)
	}
	if response.Text != "Adaptive HTTP budget verified." || response.Usage.InputTokens != 8 || response.Usage.OutputTokens != 6 ||
		response.ProviderReportedModel != "synthetic-provider-v1" || providerCalls.Load() != 1 {
		t.Fatalf("gateway response/usage/provider calls = %#v/%d, want one successful HTTP dispatch", response, providerCalls.Load())
	}
	var ledgerState string
	var reservedTokens, outputTokens, inputReservedTokens, actualInputTokens, actualOutputTokens int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.state,ledger.reserved_tokens,preparation.send_tokens,
preparation.input_reserved_tokens,ledger.input_tokens,ledger.output_tokens
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fence.InitiatorID, call.CallID).
		Scan(&ledgerState, &reservedTokens, &outputTokens, &inputReservedTokens, &actualInputTokens, &actualOutputTokens); err != nil {
		t.Fatal(err)
	}
	if ledgerState != "settled" || outputTokens != int64(plan.MaxTokens) || reservedTokens != inputReservedTokens+outputTokens ||
		actualInputTokens != response.Usage.InputTokens || actualOutputTokens != response.Usage.OutputTokens || inputReservedTokens < actualInputTokens {
		t.Fatalf("settled ledger state/tokens = %s/%d (input=%d output=%d), usage=%+v",
			ledgerState, reservedTokens, inputReservedTokens, outputTokens, response.Usage)
	}
	replayed, err := gateway.Complete(ctx, call)
	if err != nil || replayed.Text != response.Text || providerCalls.Load() != 1 {
		t.Fatalf("settled response replay = %q, err=%v, provider HTTP calls=%d; want same result without redispatch",
			replayed.Text, err, providerCalls.Load())
	}
}
