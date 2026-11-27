package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestTaskModelGatewayReportsNoDispatchOnlyAfterReservationRelease(t *testing.T) {
	service, store, client, call := taskGatewayFixture(t,
		[]byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":16}`),
		ModelResponse{})
	dispatchErr := domain.NewError(domain.CodeInvalidState, "model call expired before dispatch")
	store.dispatchErr = dispatchErr

	_, err := service.Complete(context.Background(), call)
	var noDispatch *TaskModelNoDispatchError
	if !errors.As(err, &noDispatch) {
		t.Fatalf("confirmed undispatched call error = %T %v, want typed evidence", err, err)
	}
	evidence := noDispatch.TaskModelNoDispatchEvidence()
	if store.reservation.Task == nil || evidence.TaskFence != *store.reservation.Task || evidence.CallID != call.CallID || !evidence.UsageKnown ||
		evidence.InputTokens != 0 || evidence.OutputTokens != 0 || client.calls != 0 || store.abandonN != 1 {
		t.Fatalf("no-dispatch evidence or call counts are invalid: evidence=%+v provider=%d abandon=%d",
			evidence, client.calls, store.abandonN)
	}
	if strings.Contains(noDispatch.Error(), call.CallID) || strings.Contains(noDispatch.Error(), call.Task.OwnerID) ||
		strings.Contains(noDispatch.Error(), dispatchErr.Error()) {
		t.Fatalf("typed error exposed identity: %q", noDispatch.Error())
	}

	store.abandonErr = errors.New("reservation release could not be confirmed")
	_, err = service.Complete(context.Background(), call)
	noDispatch = nil
	if errors.As(err, &noDispatch) || !errors.Is(err, store.abandonErr) || client.calls != 0 {
		t.Fatalf("failed reservation release was treated as proven no-dispatch: error=%T %v provider=%d", err, err, client.calls)
	}

	store.dispatchErr = nil
	store.abandonErr = nil
	client.err = errors.New("provider response may have been accepted")
	_, err = service.Complete(context.Background(), call)
	noDispatch = nil
	if errors.As(err, &noDispatch) || client.calls != 1 || !store.settlement.Unknown {
		t.Fatalf("provider error was treated as no-dispatch: error=%T %v provider=%d settlement=%+v",
			err, err, client.calls, store.settlement)
	}
}

func TestTaskModelGatewayEmitsRetryableFailureOnlyAfterConfirmedZeroUsageSettlement(t *testing.T) {
	service, store, client, call := taskGatewayFixture(t,
		[]byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":16}`),
		ModelResponse{})
	call.Task.TaskRevision = 7
	call.Task.RecoveryCallID = "mrc_retry_123"
	client.err = &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", ConfirmedNoUsage: true}
	_, err := service.Complete(context.Background(), call)
	var retryable *TaskModelRetryableFailureError
	if !errors.As(err, &retryable) || client.calls != 1 || store.settleN != 1 || store.settlement.Unknown ||
		!store.settlement.Usage.Known || store.settlement.Usage.InputTokens != 0 || store.settlement.Usage.OutputTokens != 0 ||
		store.settlement.CallID != call.Task.RecoveryCallID || store.reservation.DSHCallID != call.CallID ||
		store.reservation.CallID != call.Task.RecoveryCallID || store.settlement.TaskCall != nil {
		t.Fatalf("confirmed retryable provider rejection was not settled under a separate attempt identity: error=%T %v settlement=%+v reservation=%+v",
			err, err, store.settlement, store.reservation)
	}
	evidence := retryable.TaskModelRecoveryFailureEvidence()
	if evidence.Kind != TaskModelRecoveryFailureProviderReject || evidence.ProviderErrorKind != ModelErrorRateLimited ||
		evidence.CallID != call.CallID || evidence.ModelID != call.ModelID || len(evidence.RequestDigest) != 32 || !evidence.UsageKnown {
		t.Fatalf("retry evidence is incomplete or unbounded: %+v", evidence)
	}

	store = &modelGatewayStoreTest{}
	client = &modelGatewayClientTest{err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status"}}
	service, err = NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	_, err = service.Complete(context.Background(), call)
	retryable = nil
	if errors.As(err, &retryable) || !store.settlement.Unknown {
		t.Fatalf("provider rejection without confirmed zero usage became retryable: error=%T %v settlement=%+v", err, err, store.settlement)
	}
}
