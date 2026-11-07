package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type textStreamingStoreTest struct {
	*modelGatewayStoreTest
	verified, replay, streamAtDispatch bool
	begins, finishes                   int
	capabilityErr                      error
}

func (s *textStreamingStoreTest) BeginModelTextStreamingProbe(context.Context, ModelCallReservation) (bool, error) {
	s.begins++
	if s.replay && s.verified {
		return true, nil
	}
	s.verified = false
	return false, nil
}

func (s *textStreamingStoreTest) FinishModelTextStreamingProbe(context.Context, ModelCallReservation) error {
	s.finishes++
	s.verified = true
	return nil
}

func (s *textStreamingStoreTest) BeginModelCallDispatchWithTextStreaming(ctx context.Context, request ModelCallReservation) (bool, error) {
	return s.streamAtDispatch, s.BeginModelCallDispatch(ctx, request)
}

func (s *textStreamingStoreTest) TaskTextStreamingCapability(context.Context, task.Task) (bool, error) {
	return s.verified, s.capabilityErr
}

type textStreamingProbeClientTest struct {
	calls        int
	noDeltas     bool
	wrongDelta   bool
	unknownUsage bool
	err          error
}

func (c *textStreamingProbeClientTest) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	c.calls++
	if c.err != nil {
		return ModelResponse{}, c.err
	}
	if request.OnTextDelta == nil {
		return ModelResponse{}, errors.New("probe did not request streaming")
	}
	if !c.noDeltas {
		delta := "OK"
		if c.wrongDelta {
			delta = "different"
		}
		if err := request.OnTextDelta(delta); err != nil {
			return ModelResponse{}, err
		}
	}
	return ModelResponse{Text: "OK", StopReason: "stop", Usage: ModelUsage{Known: !c.unknownUsage, InputTokens: 3, OutputTokens: 1}}, nil
}

func (*textStreamingProbeClientTest) Check(context.Context, ModelEndpoint) (ModelCapabilities, error) {
	return ModelCapabilities{}, errors.New("unexpected check")
}

func TestTextStreamingProbeRequiresObservedCompleteMeteredStream(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noDeltas     bool
		wrongDelta   bool
		unknownUsage bool
		err          error
		success      bool
	}{
		{name: "verified stream", success: true},
		{name: "no stream events", noDeltas: true},
		{name: "stream differs from final", wrongDelta: true},
		{name: "usage unknown", unknownUsage: true},
		{name: "provider failure", err: errors.New("synthetic failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &textStreamingStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, verified: true}
			client := &textStreamingProbeClientTest{noDeltas: tc.noDeltas, wrongDelta: tc.wrongDelta, unknownUsage: tc.unknownUsage, err: tc.err}
			gateway, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
			if err != nil {
				t.Fatal(err)
			}
			principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
			result, err := gateway.CheckModelCapability(context.Background(), principal, "p1234567", "stream-probe-001", "text_stream")
			if (err == nil) != tc.success || store.verified != tc.success || result.TextStream != tc.success ||
				store.begins != 1 || client.calls != 1 || store.finishes != boolInt(tc.success) || store.settleN != 1 {
				t.Fatalf("stream probe result=%+v err=%v verified=%t begins=%d finishes=%d calls=%d settlements=%d",
					result, err, store.verified, store.begins, store.finishes, client.calls, store.settleN)
			}
			if tc.success {
				store.replay = true
				result, err = gateway.CheckModelCapability(context.Background(), principal, "p1234567", "stream-probe-001", "text_stream")
				if err != nil || !result.TextStream || client.calls != 1 {
					t.Fatalf("verified replay caused another provider call: result=%+v err=%v calls=%d", result, err, client.calls)
				}
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestTaskStreamingDispatchFallsBackBeforeProviderCall(t *testing.T) {
	_, _, _, call := revisionSettlementFixture(t)
	for _, allowed := range []bool{false, true} {
		store := &textStreamingStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, streamAtDispatch: allowed}
		client := &modelGatewayClientTest{response: ModelResponse{Text: "hello", StopReason: "stop",
			Usage: ModelUsage{Known: true, InputTokens: 1, OutputTokens: 1}}}
		gateway, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
		if err != nil {
			t.Fatal(err)
		}
		var deltas int
		call.Request.OnTextDelta = func(string) error { deltas++; return nil }
		response, err := gateway.Complete(context.Background(), call)
		if err != nil || response.Text != "hello" || client.calls != 1 || (client.request.OnTextDelta != nil) != allowed || deltas != boolInt(allowed) {
			t.Fatalf("stream allowed=%t response=%+v err=%v calls=%d deltas=%d", allowed, response, err, client.calls, deltas)
		}
	}
}

func TestTaskStreamingCapabilityUnknownDoesNotEnableStream(t *testing.T) {
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", ModelID: "p1234567",
		Lease: task.Lease{Owner: "worker", Version: 1}}
	gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	if enabled, err := gateway.TaskTextStreamingCapability(context.Background(), value); err != nil || enabled {
		t.Fatalf("unknown streaming capability enabled: enabled=%t err=%v", enabled, err)
	}
	store := &textStreamingStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, verified: true,
		capabilityErr: domain.NewError(domain.CodeUnauthenticated, "authorization changed")}
	gateway, err = NewModelGatewayService(store, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.TaskTextStreamingCapability(context.Background(), value); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("streaming authorization error was hidden: %v", err)
	}
}
