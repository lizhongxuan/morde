package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestSummaryAndReviewRetryOnlyPredispatchReservationLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		source    ModelCallSource
		remaining int
		always    bool
		wantCalls int
		wantErr   domain.ErrorCode
	}{
		{name: "summary concurrency clears", source: ModelCallCompaction, remaining: 1, wantCalls: 2},
		{name: "review concurrency clears", source: ModelCallReview, remaining: 1, wantCalls: 2},
		{name: "summary wait is bounded", source: ModelCallCompaction, always: true, wantCalls: 4, wantErr: domain.CodeRateLimited},
		{name: "review wait is bounded", source: ModelCallReview, always: true, wantCalls: 4, wantErr: domain.CodeRateLimited},
		{name: "other sources do not retry", source: ModelCallProbe, always: true, wantCalls: 1, wantErr: domain.CodeRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &concurrencyLimitedModelGatewayStore{modelGatewayStoreTest: &modelGatewayStoreTest{},
				remaining: tc.remaining, always: tc.always}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = service.reserveModelCall(ctx, tc.source, false, "", ModelCallReservation{CallID: "reservation-id"}, project.Usage{})
			attempts := store.reservationAttempts()
			if len(attempts) != tc.wantCalls || (tc.wantErr == "" && err != nil) ||
				(tc.wantErr != "" && domain.CodeOf(err) != tc.wantErr) {
				t.Fatalf("attempts=%v err=%v, want calls=%d code=%s", attempts, err, tc.wantCalls, tc.wantErr)
			}
			for _, id := range attempts {
				if id != "reservation-id" {
					t.Fatalf("pre-dispatch reservation identity changed: %v", attempts)
				}
			}
		})
	}
}

func TestSummaryReservationWaitStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &concurrencyLimitedModelGatewayStore{modelGatewayStoreTest: &modelGatewayStoreTest{}, always: true}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	err = service.reserveModelCall(ctx, ModelCallCompaction, false, "", ModelCallReservation{CallID: "reservation-id"}, project.Usage{})
	if !errors.Is(err, context.Canceled) || len(store.reservationAttempts()) != 1 {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestModelConnectionErrorKeepsOnlyConfirmedProviderRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        *ModelClientError
		wantDelay  time.Duration
		wantBeyond bool
	}{
		{name: "provider rate limit", err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfter: 45 * time.Second}, wantDelay: 45 * time.Second},
		{name: "provider rate limit beyond horizon", err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfterExceedsDurableHorizon: true}, wantBeyond: true},
		{name: "negative delay", err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfter: -time.Second}},
		{name: "transport failure", err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "transport", RetryAfter: 45 * time.Second, RetryAfterExceedsDurableHorizon: true}},
		{name: "other provider failure", err: &ModelClientError{Kind: ModelErrorAuthentication, Stage: "provider_status", RetryAfter: 45 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *domain.Error
			if err := modelConnectionCheckError(tc.err); !errors.As(err, &got) || got.RetryAfter != tc.wantDelay ||
				(got.Details["retry_after_exceeds_durable_horizon"] == "true") != tc.wantBeyond {
				t.Fatalf("model error=%+v, want delay=%v beyond=%t", got, tc.wantDelay, tc.wantBeyond)
			}
		})
	}
}

// This fixture models the ledger's one-call identity rule, including its
// unknown settlement; a later request cannot redispatch the same identity.
type retrySummaryLedger struct {
	*summaryCapacityStoreTest
	reserved bool
}

func (s *retrySummaryLedger) ReserveModelCall(ctx context.Context, value ModelCallReservation) error {
	if s.reserved {
		return domain.NewError(domain.CodeConflict, "model call was already dispatched; reconcile its result")
	}
	s.reserved = true
	return s.summaryCapacityStoreTest.ReserveModelCall(ctx, value)
}

func TestConversationSummaryProviderFailureIsReturnedWithoutRedispatch(t *testing.T) {
	claim, _ := routeGatewayFixture()
	claim.Authorization = task.Authorization{Kind: "session", TokenID: "t1234567", Version: 1}
	scope, err := ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic summary input"))
	fence := ConversationSummaryModelFence{Claim: claim, Identity: scope, OperationID: operation,
		InputSHA256: digest[:], LeaseOwner: claim.LeaseOwner, LeaseVersion: claim.LeaseVersion,
		SessionID: "summary_session_1", ThroughSequence: 1, ContextWindowTokens: 4096}
	store := &retrySummaryLedger{summaryCapacityStoreTest: &summaryCapacityStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}}
	client := &modelGatewayClientTest{err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfter: 45 * time.Second}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	request := ModelRequest{Instructions: "Summarize authorized history.",
		Input: []ModelInput{{Type: "message", Role: "user", Text: "Summarize this conversation."}}, MaxOutputTokens: 64}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = service.CompleteConversationSummaryModel(ctx, fence, "p1234567", request)
	var modelErr *domain.Error
	if !errors.As(err, &modelErr) || modelErr.Code != domain.CodeRateLimited || modelErr.RetryAfter != 45*time.Second ||
		client.calls != 1 || !store.settlement.Unknown {
		t.Fatalf("first call: calls=%d settlement=%+v error=%v", client.calls, store.settlement, err)
	}
	_, err = service.CompleteConversationSummaryModel(ctx, fence, "p1234567", request)
	if domain.CodeOf(err) != domain.CodeConflict || client.calls != 1 {
		t.Fatalf("unknown call was redispatched: calls=%d err=%v", client.calls, err)
	}
}

func TestConversationSummaryDoesNotTrustProviderReservationLabel(t *testing.T) {
	claim, _ := routeGatewayFixture()
	claim.Authorization = task.Authorization{Kind: "session", TokenID: "t1234567", Version: 1}
	scope, err := ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic summary input"))
	fence := ConversationSummaryModelFence{Claim: claim, Identity: scope, OperationID: operation,
		InputSHA256: digest[:], LeaseOwner: claim.LeaseOwner, LeaseVersion: claim.LeaseVersion,
		SessionID: "summary_session_1", ThroughSequence: 1, ContextWindowTokens: 4096}
	store := &retrySummaryLedger{summaryCapacityStoreTest: &summaryCapacityStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}}
	providerErr := domain.NewError(domain.CodeRateLimited, "synthetic provider failure")
	providerErr.Details["reason"] = "model_concurrency_limit"
	providerErr.Details["stage"] = "reservation"
	client := &modelGatewayClientTest{err: providerErr}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	request := ModelRequest{Instructions: "Summarize authorized history.",
		Input: []ModelInput{{Type: "message", Role: "user", Text: "Summarize this conversation."}}, MaxOutputTokens: 64}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = service.CompleteConversationSummaryModel(ctx, fence, "p1234567", request)
	if !errors.Is(err, providerErr) || client.calls != 1 || !store.settlement.Unknown {
		t.Fatalf("provider error was treated as a safe pre-dispatch failure: calls=%d settlement=%+v err=%v",
			client.calls, store.settlement, err)
	}
}

type retryReviewLedger struct {
	*modelGatewayStoreTest
	reserved bool
}

func (s *retryReviewLedger) ModelTaskProjectIDForReview(context.Context, TaskValidationReviewModelFence) (string, error) {
	return "", nil
}

func (s *retryReviewLedger) RecoverTaskValidationReviewModelResponse(context.Context, ModelCallReservation) (TaskValidationReviewModelResponseRecord, bool, error) {
	if s.reserved {
		return TaskValidationReviewModelResponseRecord{}, false, domain.NewError(domain.CodeInvalidState, "review model call has no recoverable settled response")
	}
	return TaskValidationReviewModelResponseRecord{}, false, nil
}

func (s *retryReviewLedger) ReserveModelCall(ctx context.Context, value ModelCallReservation) error {
	if s.reserved {
		return domain.NewError(domain.CodeConflict, "model call was already dispatched; reconcile its result")
	}
	s.reserved = true
	return s.modelGatewayStoreTest.ReserveModelCall(ctx, value)
}

func TestTaskValidationReviewProviderFailureIsReturnedWithoutRedispatch(t *testing.T) {
	claim := TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567", TaskID: "q1234567",
		TaskRevision: 4, ResultEventSequence: 9, ResultSHA256: bytes.Repeat([]byte{0x12}, 32), ClaimGeneration: 3, LeaseOwner: "review-worker"}
	fence := TaskValidationReviewModelFence{Claim: claim, InitiatorID: claim.InitiatorID, SessionID: "review_session_1", ThroughSequence: 1}
	store := &retryReviewLedger{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfter: 45 * time.Second}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	raw := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"Review this task."}]}],"tools":[],"max_tokens":64}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = service.CompleteTaskValidationReviewDSHModel(ctx, fence, "p1234567", raw)
	var modelErr *domain.Error
	if !errors.As(err, &modelErr) || modelErr.Code != domain.CodeRateLimited || modelErr.RetryAfter != 45*time.Second ||
		client.calls != 1 || !store.settlement.Unknown {
		t.Fatalf("first call: calls=%d settlement=%+v error=%v", client.calls, store.settlement, err)
	}
	_, err = service.CompleteTaskValidationReviewDSHModel(ctx, fence, "p1234567", raw)
	if domain.CodeOf(err) != domain.CodeInvalidState || client.calls != 1 {
		t.Fatalf("unknown call was redispatched: calls=%d err=%v", client.calls, err)
	}
}
