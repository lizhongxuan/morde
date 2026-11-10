package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type a10LocalModelStubRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions"`
	Prompt       string `json:"prompt"`
}

type a10LocalModelStubResponse struct {
	Text         string `json:"text"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type a10LocalHTTPModelClient struct {
	endpoint string
	client   *http.Client
}

func (client a10LocalHTTPModelClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	var prompt strings.Builder
	for _, input := range request.Input {
		if input.Text != "" {
			if prompt.Len() > 0 {
				prompt.WriteByte('\n')
			}
			prompt.WriteString(input.Text)
		}
		for _, part := range input.Parts {
			if part.Text == "" {
				continue
			}
			if prompt.Len() > 0 {
				prompt.WriteByte('\n')
			}
			prompt.WriteString(part.Text)
		}
	}
	payload, err := json.Marshal(a10LocalModelStubRequest{
		Model: request.Endpoint.ModelID, Instructions: request.Instructions, Prompt: prompt.String(),
	})
	if err != nil {
		return app.ModelResponse{}, err
	}
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return app.ModelResponse{}, err
	}
	call.Header.Set("Content-Type", "application/json")
	response, err := client.client.Do(call)
	if err != nil {
		return app.ModelResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return app.ModelResponse{}, fmt.Errorf("local model substitute returned HTTP %d", response.StatusCode)
	}
	var result a10LocalModelStubResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return app.ModelResponse{}, err
	}
	return app.ModelResponse{
		Text: result.Text, StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens},
	}, nil
}

func (a10LocalHTTPModelClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true, ModelID: "synthetic-local-v1"}, nil
}

type a10LocalGatewayResolver struct {
	endpoint app.ModelGatewayEndpoint
}

func (resolver a10LocalGatewayResolver) ResolveGatewayEndpoint(_ context.Context, ownerID, modelID string, _, _ bool) (app.ModelGatewayEndpoint, error) {
	if ownerID != "u1234567" || modelID != "p1234567" {
		return app.ModelGatewayEndpoint{}, errors.New("unexpected local model identity")
	}
	return resolver.endpoint, nil
}

func (resolver a10LocalGatewayResolver) ResolveProbeEndpoint(_ context.Context, _ identity.Principal, _ string) (app.ModelGatewayEndpoint, error) {
	return resolver.endpoint, nil
}

func (a10LocalGatewayResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, capabilities app.ModelCapabilities) (app.ModelCapabilities, error) {
	return capabilities, nil
}

type a10LocalGatewayStore struct {
	mu           sync.Mutex
	reservations []app.ModelCallReservation
	settlements  []app.ModelCallSettlement
	deliveries   []app.ModelCallReservation
}

func (store *a10LocalGatewayStore) ReserveModelCall(_ context.Context, value app.ModelCallReservation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.reservations = append(store.reservations, value)
	return nil
}

func (*a10LocalGatewayStore) BeginModelCallDispatch(context.Context, app.ModelCallReservation) error {
	return nil
}

func (*a10LocalGatewayStore) AbandonModelCallReservation(context.Context, app.ModelCallReservation) error {
	return nil
}

func (store *a10LocalGatewayStore) SettleModelCall(_ context.Context, value app.ModelCallSettlement) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.settlements = append(store.settlements, value)
	return nil
}

func (*a10LocalGatewayStore) ModelTaskProjectID(_ context.Context, fence app.ModelTaskFence) (string, error) {
	if fence.TaskID == "" {
		return "", errors.New("task model fence is missing")
	}
	return "", nil
}

func (store *a10LocalGatewayStore) ValidateTaskModelDelivery(_ context.Context, reservation app.ModelCallReservation) error {
	if reservation.Source != app.ModelCallTask || reservation.Task == nil || reservation.Task.TaskID == "" {
		return errors.New("task model delivery fence is missing")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.deliveries = append(store.deliveries, reservation)
	return nil
}

func TestCloudDSHUsesModelGatewayAndLocalSyntheticModelHTTP(t *testing.T) {
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
			Text: "A10 local model gateway result", InputTokens: 12, OutputTokens: 6,
		})
	}))
	defer modelServer.Close()

	endpoint := app.ModelGatewayEndpoint{
		Endpoint: app.ModelEndpoint{
			BaseURL: "https://models.invalid/v1", APIKey: "synthetic-test-only", ModelID: "synthetic-local-v1",
		},
		Version: time.Unix(1, 0), Personal: true,
	}
	gatewayStore := &a10LocalGatewayStore{}
	gateway, err := app.NewModelGatewayService(gatewayStore, a10LocalGatewayResolver{endpoint: endpoint}, a10LocalHTTPModelClient{
		endpoint: modelServer.URL + "/complete", client: modelServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	executor, _, _, _, fallbackModels, value := cloudDSHFixture(t)
	executor.options.Models = gateway
	executor.options.ToolCapabilities = gateway
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("DSH completion = %v, want verified result pending separate completion", err)
	}

	stubMu.Lock()
	observedRequests := append([]a10LocalModelStubRequest(nil), requests...)
	stubMu.Unlock()
	if len(observedRequests) != 1 || observedRequests[0].Model != "synthetic-local-v1" ||
		observedRequests[0].Instructions == "" || observedRequests[0].Prompt == "" {
		if len(observedRequests) == 1 {
			t.Fatalf("local synthetic model request omitted model input: model=%q instruction_bytes=%d prompt_bytes=%d",
				observedRequests[0].Model, len(observedRequests[0].Instructions), len(observedRequests[0].Prompt))
		}
		t.Fatalf("local synthetic model request count = %d", len(observedRequests))
	}
	if strings.Count(observedRequests[0].Instructions, "You are Morde, the user's AI assistant.") != 1 ||
		strings.Count(observedRequests[0].Instructions, "Morde task execution policy:") != 1 {
		t.Fatal("local synthetic model request duplicated or omitted the Morde identity or task policy")
	}
	if len(fallbackModels.calls) != 0 {
		t.Fatalf("DSH bypassed ModelGateway and called fallback models %d times", len(fallbackModels.calls))
	}

	gatewayStore.mu.Lock()
	defer gatewayStore.mu.Unlock()
	if len(gatewayStore.reservations) != 1 || len(gatewayStore.settlements) != 1 || len(gatewayStore.deliveries) != 1 {
		t.Fatalf("model gateway ledger: reservations=%d settlements=%d deliveries=%d",
			len(gatewayStore.reservations), len(gatewayStore.settlements), len(gatewayStore.deliveries))
	}
	reservation := gatewayStore.reservations[0]
	settlement := gatewayStore.settlements[0]
	if reservation.Source != app.ModelCallTask || reservation.Task == nil || reservation.Task.TaskID != value.ID ||
		reservation.ModelID != value.ModelID || reservation.OwnerID != value.InitiatorID ||
		settlement.Source != app.ModelCallTask || settlement.Unknown ||
		!settlement.Usage.Known || settlement.Usage.InputTokens != 12 || settlement.Usage.OutputTokens != 6 {
		t.Fatalf("task model gateway accounting was not preserved: reservation=%#v settlement=%#v", reservation, settlement)
	}
}
