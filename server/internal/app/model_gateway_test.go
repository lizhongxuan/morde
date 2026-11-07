package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type modelGatewayStoreTest struct {
	reservation ModelCallReservation
	settlement  ModelCallSettlement
	taskProject string
	scopeErr    error
	reserveN    int
	dispatchN   int
	abandonN    int
	settleN     int
	reserveErr  error
	dispatchErr error
	abandonErr  error
	deliveryErr error
	dispatch    bool
	settleErr   error
	settledCtx  error
}

func (s *modelGatewayStoreTest) ReserveModelCall(_ context.Context, value ModelCallReservation) error {
	s.reserveN++
	s.reservation = value
	return s.reserveErr
}

func (s *modelGatewayStoreTest) ModelTaskProjectID(_ context.Context, _ ModelTaskFence) (string, error) {
	return s.taskProject, s.scopeErr
}

func (s *modelGatewayStoreTest) BeginModelCallDispatch(_ context.Context, value ModelCallReservation) error {
	s.dispatchN++
	s.reservation, s.dispatch = value, true
	return s.dispatchErr
}

func (s *modelGatewayStoreTest) AbandonModelCallReservation(_ context.Context, value ModelCallReservation) error {
	s.abandonN++
	s.reservation = value
	return s.abandonErr
}

func (s *modelGatewayStoreTest) SettleModelCall(ctx context.Context, value ModelCallSettlement) error {
	s.settleN++
	s.settlement, s.settledCtx = value, ctx.Err()
	return s.settleErr
}

func (s *modelGatewayStoreTest) ValidateTaskModelDelivery(context.Context, ModelCallReservation) error {
	return s.deliveryErr
}

type modelGatewayResolverTest struct {
	owner, model string
	tools, image bool
	personal     bool
	endpoint     ModelEndpoint
}

func (r *modelGatewayResolverTest) ResolveGatewayEndpoint(_ context.Context, owner, model string, tools, image bool) (ModelGatewayEndpoint, error) {
	r.owner, r.model, r.tools, r.image = owner, model, tools, image
	endpoint := r.endpoint
	if endpoint == (ModelEndpoint{}) {
		endpoint = ModelEndpoint{BaseURL: "https://provider.example/v1", APIKey: "platform-key", ModelID: "test-model"}
	}
	return ModelGatewayEndpoint{Endpoint: endpoint, Version: time.Unix(1, 0), Personal: r.personal}, nil
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

func TestModelGatewayRejectsToolCallsNotAllowedByRequest(t *testing.T) {
	for _, test := range []struct {
		name         string
		tools        []ModelTool
		requiredTool string
		responseName string
		responseID   string
		arguments    string
	}{
		{name: "undeclared tool", tools: []ModelTool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}, responseName: "admin_delete"},
		{name: "no tools offered", responseName: "admin_delete"},
		{name: "tool conflicts with RequiredTool", tools: []ModelTool{
			{Name: "lookup", Parameters: []byte(`{"type":"object"}`)},
			{Name: "admin_delete", Parameters: []byte(`{"type":"object"}`)},
		}, requiredTool: "lookup", responseName: "admin_delete"},
		{name: "oversized tool call ID", tools: []ModelTool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}, responseName: "lookup", responseID: strings.Repeat("x", 257)},
		{name: "oversized tool arguments", tools: []ModelTool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}, responseName: "lookup", arguments: `{"value":"` + strings.Repeat("x", maxAgentToolArgsBytes) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			callID := test.responseID
			if callID == "" {
				callID = "call-1"
			}
			arguments := test.arguments
			if arguments == "" {
				arguments = `{}`
			}
			store := &modelGatewayStoreTest{}
			client := &modelGatewayClientTest{response: ModelResponse{
				ToolCalls: []ModelToolCall{{CallID: callID, Name: test.responseName, Arguments: arguments}},
				Usage:     ModelUsage{Known: true, InputTokens: 3, OutputTokens: 2},
			}}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
			if err != nil {
				t.Fatal(err)
			}
			call := ModelCall{Source: ModelCallChat,
				Principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
				ModelID:   "p1234567", CallID: "chat-call-tools-0001",
				Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, Tools: test.tools,
					RequiredTool: test.requiredTool, MaxOutputTokens: 10}}
			response, err := service.Complete(context.Background(), call)
			if domain.CodeOf(err) != domain.CodeUpstreamError || len(response.ToolCalls) != 0 || client.calls != 1 ||
				store.settleN != 1 || store.settlement.Unknown || store.settlement.Usage.InputTokens != 3 || store.settlement.Usage.OutputTokens != 2 {
				t.Fatalf("invalid provider tool call was returned or usage was lost: response=%#v settlement=%#v error=%v", response, store.settlement, err)
			}
		})
	}
}

func TestModelGatewayRejectsTaskToolCallsOutsideWorkerContract(t *testing.T) {
	fence := &ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		text       string
		stopReason string
		calls      []ModelToolCall
	}{
		{name: "call ID exceeds worker pattern", stopReason: "tool_calls", calls: []ModelToolCall{{CallID: strings.Repeat("x", 129), Name: "lookup", Arguments: `{}`}}},
		{name: "call ID contains unsupported character", stopReason: "tool_calls", calls: []ModelToolCall{{CallID: "call.id", Name: "lookup", Arguments: `{}`}}},
		{name: "duplicate call IDs", stopReason: "tool_calls", calls: []ModelToolCall{
			{CallID: "call-1", Name: "lookup", Arguments: `{}`},
			{CallID: "call-1", Name: "lookup", Arguments: `{}`},
		}},
		{name: "too many calls for worker", stopReason: "tool_calls", calls: func() []ModelToolCall {
			calls := make([]ModelToolCall, MaxDSHModelResponseToolCalls+1)
			for i := range calls {
				calls[i] = ModelToolCall{CallID: fmt.Sprintf("call-%d", i), Name: "lookup", Arguments: `{}`}
			}
			return calls
		}()},
		{name: "text exceeds worker limit", stopReason: "stop", text: strings.Repeat("x", MaxDSHModelResponseTextBytes+1)},
		{name: "empty final text", stopReason: "stop"},
		{name: "incomplete final response", stopReason: "length", text: "partial"},
		{name: "tool calls with text stop reason", stopReason: "stop", calls: []ModelToolCall{{CallID: "call-1", Name: "lookup", Arguments: `{}`}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &modelGatewayStoreTest{}
			client := &modelGatewayClientTest{response: ModelResponse{Text: test.text, ToolCalls: test.calls,
				StopReason: test.stopReason, Usage: ModelUsage{Known: true, InputTokens: 3, OutputTokens: 2}}}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
			if err != nil {
				t.Fatal(err)
			}
			call := ModelCall{Source: ModelCallTask, Task: fence, ModelID: "p1234567", CallID: callID,
				RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{"name":"lookup","parameters":{"type":"object"}}]}`)}
			response, err := service.Complete(context.Background(), call)
			if domain.CodeOf(err) != domain.CodeUpstreamError || len(response.ToolCalls) != 0 || store.settleN != 1 ||
				store.settlement.Unknown || store.settlement.Usage.InputTokens != 3 || store.settlement.Usage.OutputTokens != 2 {
				t.Fatalf("invalid Task tool response was returned or usage was lost: response=%#v settlement=%#v error=%v", response, store.settlement, err)
			}
		})
	}
}

func TestModelGatewayReservesStructuredInputOverheadBeforeDispatch(t *testing.T) {
	request := ModelRequest{
		Input: []ModelInput{
			{Type: "function_call", CallID: "call-1", Name: "lookup", Arguments: `{"query":"x"}`},
			{Type: "function_call_output", CallID: "call-1", Output: "found"},
			{Type: "message", Role: "user", Parts: []ModelContentPart{{Type: "text", Text: "hi"}}},
		},
		Tools:           []ModelTool{{Name: "lookup", Description: "Search", Parameters: json.RawMessage(`{"type":"object"}`), Strict: true}},
		RequiredTool:    "lookup",
		MaxOutputTokens: 10,
	}
	store := &modelGatewayStoreTest{reserveErr: domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallChat,
		Principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
		ModelID:   "p1234567", CallID: "chat-call-structured-01", Request: request}
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeQuotaExceeded || client.calls != 0 {
		t.Fatalf("quota rejection reached provider: %v, calls=%d", err, client.calls)
	}
	wantMinimum := int64(request.MaxOutputTokens + len(request.Instructions) + len(request.RequiredTool))
	itemCount := int64(len(request.Input) + len(request.Tools) + 2)
	for _, input := range request.Input {
		wantMinimum += int64(len(input.Type) + len(input.Role) + len(input.Text) + len(input.CallID) + len(input.Name) + len(input.Arguments) + len(input.Output))
		for _, part := range input.Parts {
			wantMinimum += int64(len(part.Type) + len(part.Text))
			itemCount++
		}
	}
	for _, tool := range request.Tools {
		wantMinimum += int64(len(tool.Name) + len(tool.Description) + len(tool.Parameters))
	}
	wantMinimum += itemCount * modelRequestItemTokenReserve
	if store.reservation.ReservedTokens < wantMinimum {
		t.Fatalf("reserved %d tokens for structured request, want at least %d", store.reservation.ReservedTokens, wantMinimum)
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
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeInvalidState || client.calls != 1 || store.abandonN != 1 {
		t.Fatalf("stale model call reached provider or kept its reservation: %v, calls %d, releases %d", err, client.calls, store.abandonN)
	}
	store.abandonErr = errors.New("ledger release failed")
	_, err = service.Complete(context.Background(), call)
	if !errors.Is(err, store.abandonErr) || store.abandonN != 2 || client.calls != 1 {
		t.Fatalf("release failure was hidden or provider was called: %v, calls %d, releases %d", err, client.calls, store.abandonN)
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
	personalClient := &modelGatewayClientTest{response: ModelResponse{Text: "done", Usage: ModelUsage{Known: true, InputTokens: 100, OutputTokens: 20}}}
	personalService, err := NewModelGatewayService(&modelGatewayStoreTest{}, &modelGatewayResolverTest{personal: true}, personalClient)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := personalService.Complete(context.Background(), call); err != nil || response.Text != "done" {
		t.Fatalf("personal model was rejected by a platform reservation estimate: %#v, %v", response, err)
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
	client := &modelGatewayClientTest{response: ModelResponse{Text: "ok", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
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
	if len(client.request.Input) != 1 || client.request.Input[0].Role != "user" || len(client.request.Input[0].Parts) != 1 ||
		client.request.Input[0].Parts[0].Text != "actual user message" || client.request.MaxOutputTokens != 16 ||
		!strings.Contains(client.request.Instructions, "You are Morde") || !strings.Contains(client.request.Instructions, "system") ||
		!strings.Contains(client.request.Instructions, "Do not introduce yourself as one of them") {
		t.Fatalf("provider request does not match dsh frame: %#v", client.request.Input)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(store.reservation.RequestDigest) != hex.EncodeToString(digest[:]) {
		t.Fatal("stored digest does not bind the raw dsh request")
	}
	if fence.AgentRootSessionID != "" {
		t.Fatalf("ModelGateway mutated caller-owned Task fence root session: %q", fence.AgentRootSessionID)
	}
	call.RawRequest = []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"image","attachment":{"id":"untrusted"}}]}],"tools":[]}`)
	if _, err := service.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeUnsupported || client.calls != 1 {
		t.Fatalf("unresolved image was not rejected before provider: %v", err)
	}
}

func TestTaskModelRequestReportsOversizeBeforeReservation(t *testing.T) {
	store := &modelGatewayStoreTest{}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"%s"}]}],"tools":[],"max_tokens":16}`, strings.Repeat("x", maxAgentInputBytes)))
	_, err = service.Complete(context.Background(), ModelCall{Source: ModelCallTask, Task: &fence,
		ModelID: "p1234567", CallID: callID, RawRequest: raw})
	if domain.CodeOf(err) != domain.CodeResultTooLarge || store.reserveN != 0 || store.dispatchN != 0 || client.calls != 0 {
		t.Fatalf("oversize task input was not rejected before reservation and dispatch: %v", err)
	}
}

func TestValidateModelCallDoesNotMutateSharedTaskFence(t *testing.T) {
	fence := &ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallTask, Task: fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
		Request:    ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 16}}
	const workers = 32
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, err := validateModelCall(call)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fence.AgentRootSessionID != "" {
		t.Fatalf("validation mutated caller-owned Task fence root session: %q", fence.AgentRootSessionID)
	}
}

func TestModelGatewayProjectTaskUsesAtomicBudgetPathAndFailsClosedWhenUnwired(t *testing.T) {
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: projectBudgetTestOwner, InitiatorID: projectBudgetTestMember,
		LeaseOwner: "worker-1", LeaseVersion: 1, AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"do work"}]}],"tools":[],"max_tokens":16}`)}
	store := &modelGatewayStoreTest{taskProject: projectBudgetTestID}
	budget, projectStore, _, rates := projectBudgetServiceFixture(t, project.RoleMember)
	rates.rate.Model = "test-model"
	client := &modelGatewayClientTest{response: ModelResponse{Text: "done", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client, budget)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := service.Complete(context.Background(), call); err != nil || response.Text != "done" {
		t.Fatalf("project Task model call = %#v, %v", response, err)
	}
	reserved := projectStore.reservation
	if store.reserveN != 0 || store.dispatchN != 1 || store.settleN != 0 || client.calls != 1 ||
		projectStore.reserveCalls != 1 || projectStore.settleCalls != 1 || reserved.ProjectID != projectBudgetTestID ||
		reserved.ActorID != projectBudgetTestMember || reserved.TaskID != fence.TaskID || reserved.AccountCall.OwnerID != projectBudgetTestMember ||
		reserved.AccountCall.Task == nil || reserved.AccountCall.Task.OwnerID != projectBudgetTestOwner ||
		projectStore.settlement.Unknown || projectStore.settlement.Usage != (project.Usage{InputTokens: 2, OutputTokens: 1}) {
		t.Fatalf("project budget path was not used atomically: account=%#v project=%#v", store, projectStore)
	}

	unwired := &modelGatewayStoreTest{taskProject: projectBudgetTestID}
	unwiredClient := &modelGatewayClientTest{}
	unwiredService, err := NewModelGatewayService(unwired, &modelGatewayResolverTest{}, unwiredClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwiredService.Complete(context.Background(), call); domain.CodeOf(err) != domain.CodeUnsupported ||
		unwired.reserveN != 0 || unwiredClient.calls != 0 {
		t.Fatalf("project call without budget integration was not rejected before dispatch: %v", err)
	}

	personalStore := &modelGatewayStoreTest{taskProject: projectBudgetTestID}
	personalClient := &modelGatewayClientTest{response: ModelResponse{Text: "done", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	personalService, err := NewModelGatewayService(personalStore, &modelGatewayResolverTest{personal: true}, personalClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personalService.Complete(context.Background(), call); err != nil || personalStore.reserveN != 1 ||
		!personalStore.reservation.PersonalModel || personalStore.reservation.ProjectID != projectBudgetTestID || personalClient.calls != 1 {
		t.Fatalf("personal project model should not require project billing: store=%#v calls=%d err=%v", personalStore, personalClient.calls, err)
	}
}

func TestTaskModelRequestDisablesReasoningOnlyForFirstPartyGPT6Luna(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint ModelEndpoint
		want     bool
	}{
		{name: "first-party Luna", endpoint: ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "test-key", ModelID: "gpt-6-luna"}, want: true},
		{name: "compatible endpoint", endpoint: ModelEndpoint{BaseURL: "https://provider.example/v1", APIKey: "test-key", ModelID: "gpt-6-luna"}},
		{name: "other OpenAI model", endpoint: ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "test-key", ModelID: "gpt-4o"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &modelGatewayClientTest{response: ModelResponse{Text: "ok", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
			service, err := NewModelGatewayService(&modelGatewayStoreTest{}, &modelGatewayResolverTest{endpoint: test.endpoint}, client)
			if err != nil {
				t.Fatal(err)
			}
			fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
				AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0,
				EngineVersion: "dsh-1", ProfileVersion: "v1"}
			callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
			if err != nil {
				t.Fatal(err)
			}
			call := ModelCall{Source: ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
				RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":16}`)}
			if _, err := service.Complete(context.Background(), call); err != nil {
				t.Fatal(err)
			}
			if client.request.DisableThinking != test.want {
				t.Fatalf("DisableThinking = %v; want %v", client.request.DisableThinking, test.want)
			}
		})
	}
}

func TestDSHToolImageBecomesAuthorizedModelObservation(t *testing.T) {
	raw := json.RawMessage(`{"provider":"morde","model":"p1234567","messages":[{"role":"assistant","content":[{"type":"tool-call","id":"call-1","name":"desktop_screenshot","arguments":"{}"}]},{"role":"tool","toolCallId":"call-1","content":[{"type":"text","text":"screenshot captured"},{"type":"image","attachment":{"id":"registered"}}]}],"tools":[]}`)
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
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

func TestDSHPrivateReasoningRemainsBoundToAssistantToolCall(t *testing.T) {
	raw := json.RawMessage(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"search"}]},{"role":"assistant","content":[{"type":"reasoning","text":"private reasoning"},{"type":"text","text":"Searching."},{"type":"tool-call","id":"call-1","name":"lookup","arguments":"{}"}]},{"role":"tool","toolCallId":"call-1","content":[{"type":"text","text":"found"}]}],"tools":[{"name":"lookup","description":"Find data","parameters":{"type":"object","properties":{}}}]}`)
	request, err := parseDSHModelRequest(context.Background(), ModelTaskFence{}, "p1234567", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 4 || request.Input[1].Role != "assistant" || len(request.Input[1].Parts) != 2 ||
		request.Input[1].Parts[0].Type != "reasoning" || request.Input[1].Parts[0].Text != "private reasoning" ||
		request.Input[2].Type != "function_call" || request.Input[3].Type != "function_call_output" {
		t.Fatalf("reasoning and tool call lost their ordering: %#v", request.Input)
	}
	if err := validateGatewayModelInput(request.Input, request.Tools); err != nil {
		t.Fatalf("valid assistant reasoning was rejected: %v", err)
	}
	request.Input[1].Role = "user"
	if err := validateGatewayModelInput(request.Input, request.Tools); err == nil {
		t.Fatal("reasoning was accepted in a user message")
	}
}

func TestModelCapabilityProbeDisablesThinkingOnlyForProbe(t *testing.T) {
	store := &modelGatewayStoreTest{}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "OK", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.CheckModelCapability(context.Background(), principal, "p1234567", "probe-thinking-001", "text"); err != nil {
		t.Fatal(err)
	}
	if !client.request.DisableThinking || client.request.MaxOutputTokens != 64 {
		t.Fatalf("capability probe did not disable thinking: %#v", client.request)
	}
	call := ModelCall{Source: ModelCallClient, Principal: principal, ModelID: "p1234567", CallID: "normal-thinking-001",
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "work"}}, MaxOutputTokens: 64}}
	if _, err := service.Complete(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if client.request.DisableThinking {
		t.Fatal("normal model task unexpectedly disabled thinking")
	}
}
