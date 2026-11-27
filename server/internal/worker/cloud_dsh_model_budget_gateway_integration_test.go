package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
)

type cloudDSHHTTPBudgetProbe struct {
	mu       sync.Mutex
	requests []CloudDSHModelBudgetPlanRequest
	maxToken int
}

func (probe *cloudDSHHTTPBudgetProbe) PrepareModelBudget(_ context.Context, request CloudDSHModelBudgetPlanRequest) (CloudDSHModelBudgetPlan, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	request.Request = append(json.RawMessage(nil), request.Request...)
	probe.requests = append(probe.requests, request)
	return CloudDSHModelBudgetPlan{PlanID: "budget-plan-0001", MaxTokens: probe.maxToken}, nil
}

func TestCloudDSHModelBudgetAdmissionSetsLocalGatewayOutputCap(t *testing.T) {
	var (
		stubMu   sync.Mutex
		requests []a10LocalModelStubRequest
	)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/complete" || request.Header.Get("Authorization") != "" {
			http.Error(writer, "unexpected synthetic model request", http.StatusBadRequest)
			return
		}
		var body a10LocalModelStubRequest
		if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(writer, "invalid synthetic model request", http.StatusBadRequest)
			return
		}
		stubMu.Lock()
		requests = append(requests, body)
		stubMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(a10LocalModelStubResponse{
			Text: "A10 budget-admitted local result", InputTokens: 12, OutputTokens: 6,
		})
	}))
	defer modelServer.Close()

	endpoint := app.ModelGatewayEndpoint{
		Endpoint: app.ModelEndpoint{BaseURL: "https://models.invalid/v1", APIKey: "synthetic-test-only", ModelID: "synthetic-local-v1"},
		Version:  time.Unix(1, 0), Personal: true,
	}
	gatewayStore := &a10LocalGatewayStore{}
	gateway, err := app.NewModelGatewayService(gatewayStore, a10LocalGatewayResolver{endpoint: endpoint}, a10LocalHTTPModelClient{
		endpoint: modelServer.URL + "/complete", client: modelServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}

	executor, _, _, _, fallbackModels, value := cloudDSHFixture(t)
	value.Budget.ModelTokens = 50_000
	probe := &cloudDSHHTTPBudgetProbe{maxToken: 3_072}
	executor.options.Models = gateway
	executor.options.ToolCapabilities = gateway
	executor.options.ModelBudgetPlanner = probe
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("DSH completion = %v, want verified result pending separate completion", err)
	}

	stubMu.Lock()
	observedRequests := append([]a10LocalModelStubRequest(nil), requests...)
	stubMu.Unlock()
	if len(observedRequests) != 1 || observedRequests[0].MaxOutputTokens != probe.maxToken {
		t.Fatalf("local provider max_output_tokens = %#v, want one request capped at %d", observedRequests, probe.maxToken)
	}
	if len(fallbackModels.calls) != 0 {
		t.Fatalf("DSH bypassed ModelGateway and called fallback models %d times", len(fallbackModels.calls))
	}

	probe.mu.Lock()
	planned := append([]CloudDSHModelBudgetPlanRequest(nil), probe.requests...)
	probe.mu.Unlock()
	if len(planned) != 1 {
		t.Fatalf("budget prepare count = %d, want one", len(planned))
	}
	planRequest := planned[0]
	if planRequest.Fence.TaskID != value.ID || planRequest.Fence.OwnerID != value.OwnerID ||
		planRequest.Fence.InitiatorID != value.InitiatorID || planRequest.Fence.AgentSessionID == "" ||
		planRequest.ThroughSeq < 0 || planRequest.Turn < 0 || planRequest.Step < 0 ||
		planRequest.CallID == "" || len(planRequest.RequestDigest) != 64 {
		t.Fatalf("budget prepare did not carry its Task/session/step fence: %#v", planRequest)
	}
	if digest, ok := cloudDSHModelBudgetRequestDigest(planRequest.Request); !ok || digest != planRequest.RequestDigest {
		t.Fatalf("budget request digest does not bind the prepared provider input: digest=%q valid=%t", digest, ok)
	}
	var prepared map[string]json.RawMessage
	if err := json.Unmarshal(planRequest.Request, &prepared); err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 4 || prepared["provider"] == nil || prepared["model"] == nil ||
		prepared["messages"] == nil || prepared["tools"] == nil || prepared["max_tokens"] != nil {
		t.Fatalf("budget plan was not based on the cap-free canonical request: %s", planRequest.Request)
	}
}
