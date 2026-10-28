package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelGatewayStoreTest struct {
	reservation ModelCallReservation
	settlement  ModelCallSettlement
	reserveErr  error
	dispatchErr error
	dispatch    bool
	settleErr   error
	settledCtx  error
}

func (s *modelGatewayStoreTest) ReserveModelCall(_ context.Context, value ModelCallReservation) error {
	s.reservation = value
	return s.reserveErr
}

func (s *modelGatewayStoreTest) BeginModelCallDispatch(_ context.Context, value ModelCallReservation) error {
	s.reservation, s.dispatch = value, true
	return s.dispatchErr
}

func (s *modelGatewayStoreTest) SettleModelCall(ctx context.Context, value ModelCallSettlement) error {
	s.settlement, s.settledCtx = value, ctx.Err()
	return s.settleErr
}

type modelGatewayResolverTest struct {
	owner, model string
	tools, image bool
}

func (r *modelGatewayResolverTest) ResolveGatewayEndpoint(_ context.Context, owner, model string, tools, image bool) (ModelGatewayEndpoint, error) {
	r.owner, r.model, r.tools, r.image = owner, model, tools, image
	return ModelGatewayEndpoint{Endpoint: ModelEndpoint{BaseURL: "https://provider.example/v1", APIKey: "platform-key", ModelID: "test-model"}, Version: time.Unix(1, 0)}, nil
}

func (r *modelGatewayResolverTest) ResolveProbeEndpoint(_ context.Context, principal identity.Principal, model string) (ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(context.Background(), principal.UserID, model, true, true)
}

func (*modelGatewayResolverTest) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, value ModelCapabilities) (ModelCapabilities, error) {
	return value, nil
}

type modelTaskImageResolverTest struct{}

func (modelTaskImageResolverTest) ResolveTaskModelImage(context.Context, ModelTaskFence, json.RawMessage) (ModelImage, error) {
	return ModelImage{MediaType: "image/jpeg", Data: []byte("jpeg")}, nil
}

type modelGatewayClientTest struct {
	request  ModelRequest
	response ModelResponse
	err      error
	calls    int
}

func (c *modelGatewayClientTest) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	c.calls++
	c.request = request
	if request.OnTextDelta != nil {
		if err := request.OnTextDelta("hello"); err != nil {
			return ModelResponse{}, err
		}
	}
	return c.response, c.err
}

func (*modelGatewayClientTest) Check(context.Context, ModelEndpoint) (ModelCapabilities, error) {
	return ModelCapabilities{}, errors.New("unexpected check")
}

func TestModelGatewayReservesSettlesAndReturnsToolCalls(t *testing.T) {
	store := &modelGatewayStoreTest{}
	resolver := &modelGatewayResolverTest{}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "hello", ToolCalls: []ModelToolCall{{CallID: "call-1", Name: "test", Arguments: `{}`}}, Usage: ModelUsage{Known: true, InputTokens: 3, OutputTokens: 4}}}
	service, err := NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	call := ModelCall{Source: ModelCallClient, Principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser,
		Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"},
		ModelID: "p1234567", CallID: "model-call-0001", RawRequest: []byte(`{"content":"private"}`),
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
			Tools: []ModelTool{{Name: "test", Parameters: []byte(`{"type":"object"}`)}}, OnTextDelta: func(delta string) error { text += delta; return nil }}}
	response, err := service.Complete(context.Background(), call)
	if err != nil || response.Text != "hello" || len(response.ToolCalls) != 1 || text != "hello" {
		t.Fatalf("response = %#v, stream = %q, error = %v", response, text, err)
	}
	digest := sha256.Sum256(call.RawRequest)
	if !store.dispatch || store.reservation.OwnerID != call.Principal.UserID || store.reservation.ProviderModel != "test-model" ||
		hex.EncodeToString(store.reservation.RequestDigest) != hex.EncodeToString(digest[:]) || !resolver.tools || resolver.image ||
		store.settlement.Unknown || store.settlement.Usage.InputTokens != 3 || store.settlement.Usage.OutputTokens != 4 ||
		client.request.Endpoint.APIKey != "platform-key" || client.calls != 1 {
		t.Fatalf("reservation/settlement mismatch: %#v %#v", store.reservation, store.settlement)
	}
}

func TestModelGatewayRejectsBeforeProviderAndRecordsUnknownOnCancellation(t *testing.T) {
	store := &modelGatewayStoreTest{reserveErr: domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallChat, Principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
		ModelID: "p1234567", CallID: "chat-call-0001", Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10}}
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeQuotaExceeded || client.calls != 0 {
		t.Fatalf("budget denial reached provider: %v, calls %d", err, client.calls)
	}
	store.reserveErr = nil
	store.dispatchErr = nil
	client.err = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Complete(ctx, call); !errors.Is(err, context.Canceled) || !store.settlement.Unknown || store.settledCtx != nil {
		t.Fatalf("cancel settlement = %#v, context error=%v, call error=%v", store.settlement, store.settledCtx, err)
	}
	store.dispatchErr = domain.NewError(domain.CodeInvalidState, "model call expired before dispatch")
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeInvalidState || client.calls != 1 {
		t.Fatalf("stale model call reached provider: %v, calls %d", err, client.calls)
	}
	call.Request.Endpoint.APIKey = "device-secret"
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeInvalidArgument || client.calls != 1 {
		t.Fatalf("device endpoint was not rejected: %v, calls %d", err, client.calls)
	}
}

func TestModelGatewayDoesNotReleaseToolsWhenUsageIsUnknownOrExceedsReservation(t *testing.T) {
	store := &modelGatewayStoreTest{}
	client := &modelGatewayClientTest{response: ModelResponse{ToolCalls: []ModelToolCall{{CallID: "call-1", Name: "tool", Arguments: `{}`}}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallChat, Principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
		ModelID: "p1234567", CallID: "chat-call-0001", Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10}}
	response, err := service.Complete(context.Background(), call)
	if domain.CodeOf(err) != domain.CodeInvalidState || len(response.ToolCalls) != 0 || !store.settlement.Unknown {
		t.Fatalf("unknown usage returned actionable tools: %#v, %v", response, err)
	}
	client.response.Usage = ModelUsage{Known: true, InputTokens: 100, OutputTokens: 20}
	response, err = service.Complete(context.Background(), call)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded || len(response.ToolCalls) != 0 || store.settlement.Unknown || store.settlement.Usage.InputTokens != 100 {
		t.Fatalf("over-budget usage returned actionable tools or lost usage: %#v, %v", response, err)
	}
}

func TestDSHModelCallIDIsStableAndBounded(t *testing.T) {
	first, err := DSHModelCallID("q1234567", "session-1", 0)
	if err != nil || first != "mdl_4fd2d15ac743c0f4912e61c3f4a19140c176c327e4da9b30697f73be97126308" {
		t.Fatalf("call ID = %q, %v", first, err)
	}
	if _, err := DSHModelCallID("bad\nid", "session-1", 0); err == nil {
		t.Fatal("unsafe task identity was accepted")
	}
}

func TestTaskModelRequestUsesTheCheckpointedDSHFrame(t *testing.T) {
	store := &modelGatewayStoreTest{}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "ok", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"system","id":"system-1","source":{"kind":"system-prompt"},"content":[{"type":"text","text":"system"}]},{"role":"user","content":[{"type":"text","text":"actual user message"}]}],"tools":[],"max_tokens":16}`)
	call := ModelCall{Source: ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID, RawRequest: raw,
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "ignored injected content"}}}}
	if _, err := service.Complete(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if len(client.request.Input) != 2 || client.request.Input[0].Role != "system" || len(client.request.Input[1].Parts) != 1 ||
		client.request.Input[1].Parts[0].Text != "actual user message" || client.request.MaxOutputTokens != 16 {
		t.Fatalf("provider request does not match dsh frame: %#v", client.request.Input)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(store.reservation.RequestDigest) != hex.EncodeToString(digest[:]) {
		t.Fatal("stored digest does not bind the raw dsh request")
	}
	call.RawRequest = []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"image","attachment":{"id":"untrusted"}}]}],"tools":[]}`)
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeUnsupported || client.calls != 1 {
		t.Fatalf("unresolved image was not rejected before provider: %v", err)
	}
}

func TestDSHToolImageBecomesAuthorizedModelObservation(t *testing.T) {
	raw := json.RawMessage(`{"provider":"morde","model":"p1234567","messages":[{"role":"assistant","content":[{"type":"tool-call","id":"call-1","name":"desktop_screenshot","arguments":"{}"}]},{"role":"tool","toolCallId":"call-1","content":[{"type":"text","text":"screenshot captured"},{"type":"image","attachment":{"id":"registered"}}]}],"tools":[]}`)
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 1,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	request, err := parseDSHModelRequest(context.Background(), fence, "p1234567", raw, modelTaskImageResolverTest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 3 || request.Input[0].Type != "function_call" || request.Input[1].Type != "function_call_output" ||
		request.Input[1].Output != "screenshot captured" || request.Input[2].Type != "message" || request.Input[2].Role != "user" ||
		len(request.Input[2].Parts) != 2 || request.Input[2].Parts[0].Type != "text" ||
		request.Input[2].Parts[1].Image == nil || string(request.Input[2].Parts[1].Image.Data) != "jpeg" {
		t.Fatalf("DSH tool image was not converted into a visual observation: %#v", request.Input)
	}
}
