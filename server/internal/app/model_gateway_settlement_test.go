package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type revisionSettlementStoreTest struct {
	*modelGatewayStoreTest
	allowed       atomic.Bool
	configuration atomic.Bool
	checks        chan struct{}
	cancelBegin   context.CancelCauseFunc
}

func (s *revisionSettlementStoreTest) BeginModelCallDispatch(ctx context.Context, call ModelCallReservation) error {
	if err := s.modelGatewayStoreTest.BeginModelCallDispatch(ctx, call); err != nil {
		return err
	}
	if s.cancelBegin != nil {
		s.cancelBegin(context.Canceled)
	}
	return nil
}

func (s *revisionSettlementStoreTest) CheckRevisionSettlement(context.Context, ModelTaskFence) (bool, error) {
	select {
	case s.checks <- struct{}{}:
	default:
	}
	return s.allowed.Load(), nil
}

func (s *revisionSettlementStoreTest) ValidateTaskModelRecoveryConfiguration(context.Context, ModelCallReservation) error {
	if !s.configuration.Load() {
		return errors.New("model authorization changed")
	}
	return nil
}

type revisionSettlementClientTest struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *revisionSettlementClientTest) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	c.calls.Add(1)
	close(c.started)
	select {
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	case <-c.release:
		if request.OnTextDelta != nil {
			if err := request.OnTextDelta("obsolete delta"); err != nil {
				return ModelResponse{}, err
			}
		}
		return ModelResponse{Text: "obsolete answer", StopReason: "stop",
			Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}, nil
	}
}

func (*revisionSettlementClientTest) Check(context.Context, ModelEndpoint) (ModelCapabilities, error) {
	return ModelCapabilities{}, errors.New("unexpected capability check")
}

func revisionSettlementFixture(t *testing.T) (*ModelGatewayService, *revisionSettlementStoreTest, *revisionSettlementClientTest, ModelCall) {
	t.Helper()
	store := &revisionSettlementStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, checks: make(chan struct{}, 10)}
	store.allowed.Store(true)
	store.configuration.Store(true)
	client := &revisionSettlementClientTest{started: make(chan struct{}), release: make(chan struct{})}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	fence := ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AuthorizationVersion: 1,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{Source: ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":16}`)}
	return service, store, client, call
}

func waitForRevisionSettlementCheck(t *testing.T, checks <-chan struct{}) {
	t.Helper()
	select {
	case <-checks:
	case <-time.After(5 * time.Second):
		t.Fatal("revision settlement witness was not checked")
	}
}

func TestTaskModelRevisionCancellationSettlesUsageWithoutDeliveringOutput(t *testing.T) {
	service, store, client, call := revisionSettlementFixture(t)
	lifecycle, stopService := context.WithCancel(context.Background())
	defer stopService()
	taskCtx, cancelTask := context.WithCancelCause(context.Background())
	explicit, cancelExplicit := context.WithCancel(context.Background())
	defer cancelExplicit()
	ctx := WithModelExplicitCancellation(WithModelTaskLifecycle(taskCtx, lifecycle), explicit)
	var delivered atomic.Int32
	call.Request.OnTextDelta = func(string) error { delivered.Add(1); return nil }
	type outcome struct {
		response ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() { response, err := service.Complete(ctx, call); done <- outcome{response, err} }()
	<-client.started
	cancelTask(ErrTaskRevisionInterrupted)
	waitForRevisionSettlementCheck(t, store.checks)
	close(client.release)
	result := <-done
	if !errors.Is(result.err, context.Canceled) || result.response.Text != "" || len(result.response.ToolCalls) != 0 || delivered.Load() != 0 ||
		store.settleN != 1 || store.settlement.Unknown || !store.settlement.Usage.Known ||
		store.settlement.Usage.InputTokens != 8 || store.settlement.Usage.OutputTokens != 3 {
		t.Fatalf("revision result = %#v, settlement=%#v, delivered=%d err=%v", result.response, store.settlement, delivered.Load(), result.err)
	}
}

func TestTaskModelCancellationFailsClosedUnlessRevisionRemainsAuthorized(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		step  string
	}{
		{name: "manual stop", cause: context.Canceled},
		{name: "missing witness", cause: ErrTaskRevisionInterrupted, step: "deny"},
		{name: "witness revoked", cause: ErrTaskRevisionInterrupted, step: "revoke"},
		{name: "model revoked", cause: ErrTaskRevisionInterrupted, step: "model"},
		{name: "service stopped", cause: ErrTaskRevisionInterrupted, step: "service"},
		{name: "service stopped before task", step: "service_first"},
		{name: "explicit model cancel", cause: ErrTaskRevisionInterrupted, step: "explicit"},
		{name: "missing explicit signal", cause: ErrTaskRevisionInterrupted, step: "missing"},
		{name: "missing service lifecycle", cause: ErrTaskRevisionInterrupted, step: "no_lifecycle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, client, call := revisionSettlementFixture(t)
			lifecycle, stopService := context.WithCancel(context.Background())
			defer stopService()
			taskCtx, cancelTask := context.WithCancelCause(context.Background())
			defer cancelTask(context.Canceled)
			ctx := taskCtx
			if test.step != "no_lifecycle" {
				ctx = WithModelTaskLifecycle(ctx, lifecycle)
			}
			explicit, cancelExplicit := context.WithCancel(context.Background())
			defer cancelExplicit()
			if test.step != "missing" {
				ctx = WithModelExplicitCancellation(ctx, explicit)
			}
			if test.step == "deny" {
				store.allowed.Store(false)
			}
			done := make(chan error, 1)
			go func() { _, err := service.Complete(ctx, call); done <- err }()
			<-client.started
			if test.step == "service_first" {
				stopService()
			} else {
				cancelTask(test.cause)
			}
			if test.step == "revoke" || test.step == "model" || test.step == "service" || test.step == "explicit" {
				waitForRevisionSettlementCheck(t, store.checks)
				switch test.step {
				case "revoke":
					store.allowed.Store(false)
				case "model":
					store.configuration.Store(false)
				case "service":
					stopService()
				case "explicit":
					cancelExplicit()
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || !store.settlement.Unknown {
					t.Fatalf("unsafe cancellation not held: settlement=%#v err=%v", store.settlement, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("provider continued after cancellation or revoked authorization")
			}
		})
	}
}

func TestTaskModelRevisionSettlementKeepsOriginalDeadline(t *testing.T) {
	_, store, _, call := revisionSettlementFixture(t)
	lifecycle, stopService := context.WithCancel(context.Background())
	defer stopService()
	explicit, cancelExplicit := context.WithCancel(context.Background())
	defer cancelExplicit()
	taskCtx, cancelTask := context.WithCancelCause(context.Background())
	ctx := WithModelExplicitCancellation(WithModelTaskLifecycle(taskCtx, lifecycle), explicit)
	original, cancelOriginal := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelOriginal()
	provider, finish := taskModelProviderContext(ctx, original, ModelCallReservation{Task: call.Task}, store)
	defer finish()
	cancelTask(ErrTaskRevisionInterrupted)
	waitForRevisionSettlementCheck(t, store.checks)
	select {
	case <-provider.Done():
		if !errors.Is(provider.Err(), context.DeadlineExceeded) {
			t.Fatalf("provider stopped for unexpected reason: %v", provider.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("revision settlement exceeded the original provider deadline")
	}
}

func TestTaskModelChecksDeliveryAndReleasesNeverSentCall(t *testing.T) {
	t.Run("revision changed before delivery", func(t *testing.T) {
		service, store, client, call := revisionSettlementFixture(t)
		store.deliveryErr = domain.NewError(domain.CodeUnauthenticated, "task model execution fence is unavailable")
		close(client.release)
		response, err := service.Complete(context.Background(), call)
		if domain.CodeOf(err) != domain.CodeUnauthenticated || response.Text != "" || store.settlement.Unknown || !store.settlement.Usage.Known {
			t.Fatalf("stale provider output reached Task: %#v settlement=%#v err=%v", response, store.settlement, err)
		}
	})
	t.Run("cancel before provider", func(t *testing.T) {
		service, store, client, call := revisionSettlementFixture(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		store.cancelBegin = cancel
		_, err := service.Complete(ctx, call)
		if !errors.Is(err, context.Canceled) || store.abandonN != 1 || store.settleN != 0 || client.calls.Load() != 0 {
			t.Fatalf("never-sent request was not released: calls=%d releases=%d settled=%d err=%v", client.calls.Load(), store.abandonN, store.settleN, err)
		}
	})
}
