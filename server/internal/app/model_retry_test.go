package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func rateLimitedModelError() error {
	return &ModelClientError{Kind: ModelErrorRateLimited}
}

func TestRetryTaskModelCallRetriesTransientFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		failures   []error
		wantCalls  int
		wantSleeps []time.Duration
	}{
		{
			name:       "rate limited twice then succeeds",
			failures:   []error{rateLimitedModelError(), rateLimitedModelError()},
			wantCalls:  3,
			wantSleeps: []time.Duration{5 * time.Second, 10 * time.Second},
		},
		{
			name:       "timeout twice then succeeds",
			failures:   []error{&ModelClientError{Kind: ModelErrorTimeout}, &ModelClientError{Kind: ModelErrorTimeout}},
			wantCalls:  3,
			wantSleeps: []time.Duration{5 * time.Second, 10 * time.Second},
		},
		{
			name:       "invalid response twice then succeeds",
			failures:   []error{&ModelClientError{Kind: ModelErrorInvalidResponse}, &ModelClientError{Kind: ModelErrorInvalidResponse}},
			wantCalls:  3,
			wantSleeps: []time.Duration{5 * time.Second, 10 * time.Second},
		},
		{
			name:       "retry after wins over backoff",
			failures:   []error{&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 90 * time.Second}},
			wantCalls:  2,
			wantSleeps: []time.Duration{90 * time.Second},
		},
		{
			name:       "retry after rounds up to whole seconds",
			failures:   []error{&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 1500 * time.Millisecond}},
			wantCalls:  2,
			wantSleeps: []time.Duration{2 * time.Second},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var sleeps []time.Duration
			response, err := retryTaskModelCall(context.Background(), 0, func(context.Context) (ModelResponse, error) {
				calls++
				if calls <= len(test.failures) {
					return ModelResponse{}, test.failures[calls-1]
				}
				return ModelResponse{Text: "ok", StopReason: "stop"}, nil
			}, func(_ context.Context, delay time.Duration) error {
				sleeps = append(sleeps, delay)
				return nil
			})
			if err != nil || response.Text != "ok" || calls != test.wantCalls {
				t.Fatalf("response=%+v calls=%d err=%v", response, calls, err)
			}
			if len(sleeps) != len(test.wantSleeps) {
				t.Fatalf("sleeps=%v want=%v", sleeps, test.wantSleeps)
			}
			for i, want := range test.wantSleeps {
				if sleeps[i] != want {
					t.Fatalf("sleeps[%d]=%v want=%v", i, sleeps[i], want)
				}
			}
		})
	}
}

func TestRetryTaskModelCallExhaustion(t *testing.T) {
	for _, test := range []struct {
		name         string
		failures     []error
		wantCalls    int
		wantSleeps   []time.Duration
		wantReason   FailoverReason
		wantAttempts int64
	}{
		{
			name:         "persistent rate limit retries three times",
			failures:     []error{rateLimitedModelError(), rateLimitedModelError(), rateLimitedModelError(), rateLimitedModelError(), rateLimitedModelError()},
			wantCalls:    4,
			wantSleeps:   []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second},
			wantReason:   FailoverReasonRateLimited,
			wantAttempts: 3,
		},
		{
			name:         "authentication fails at once",
			failures:     []error{&ModelClientError{Kind: ModelErrorAuthentication}},
			wantCalls:    1,
			wantReason:   FailoverReasonAuthenticationRejected,
			wantAttempts: 0,
		},
		{
			name:         "unknown retries once",
			failures:     []error{errors.New("something odd happened"), errors.New("something odd happened")},
			wantCalls:    2,
			wantSleeps:   []time.Duration{5 * time.Second},
			wantReason:   FailoverReasonUnknown,
			wantAttempts: 1,
		},
		{
			name:         "invalid response retries twice",
			failures:     []error{&ModelClientError{Kind: ModelErrorInvalidResponse}, &ModelClientError{Kind: ModelErrorInvalidResponse}, &ModelClientError{Kind: ModelErrorInvalidResponse}},
			wantCalls:    3,
			wantSleeps:   []time.Duration{5 * time.Second, 10 * time.Second},
			wantReason:   FailoverReasonInvalidResponse,
			wantAttempts: 2,
		},
		{
			name:         "retry after past the total cap exhausts at once",
			failures:     []error{&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 601 * time.Second}},
			wantCalls:    1,
			wantReason:   FailoverReasonRateLimited,
			wantAttempts: 0,
		},
		{
			name: "second retry after past the total cap exhausts after one sleep",
			failures: []error{
				&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 400 * time.Second},
				&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 300 * time.Second},
			},
			wantCalls:    2,
			wantSleeps:   []time.Duration{400 * time.Second},
			wantReason:   FailoverReasonRateLimited,
			wantAttempts: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var sleeps []time.Duration
			_, err := retryTaskModelCall(context.Background(), 0, func(context.Context) (ModelResponse, error) {
				calls++
				if calls <= len(test.failures) {
					return ModelResponse{}, test.failures[calls-1]
				}
				return ModelResponse{Text: "ok", StopReason: "stop"}, nil
			}, func(_ context.Context, delay time.Duration) error {
				sleeps = append(sleeps, delay)
				return nil
			})
			if !IsModelRetryExhausted(err) {
				t.Fatalf("error=%v, want a spent ladder", err)
			}
			var exhausted *ModelRetryExhaustedError
			if !errors.As(err, &exhausted) {
				t.Fatalf("error=%v, want *ModelRetryExhaustedError", err)
			}
			if calls != test.wantCalls || exhausted.Reason != test.wantReason || exhausted.Attempts != test.wantAttempts {
				t.Fatalf("calls=%d reason=%s attempts=%d, want calls=%d reason=%s attempts=%d",
					calls, exhausted.Reason, exhausted.Attempts, test.wantCalls, test.wantReason, test.wantAttempts)
			}
			if len(sleeps) != len(test.wantSleeps) {
				t.Fatalf("sleeps=%v want=%v", sleeps, test.wantSleeps)
			}
			for i, want := range test.wantSleeps {
				if sleeps[i] != want {
					t.Fatalf("sleeps[%d]=%v want=%v", i, sleeps[i], want)
				}
			}
			// The exhausted error unwraps to the last provider failure.
			if exhausted.LastErr == nil || exhausted.LastErr == exhausted {
				t.Fatalf("exhausted error does not carry the last failure: %+v", exhausted)
			}
		})
	}
}

func TestRetryTaskModelCallContextEndsMidLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := retryTaskModelCall(ctx, 0, func(context.Context) (ModelResponse, error) {
		calls++
		return ModelResponse{}, rateLimitedModelError()
	}, func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	if err == nil || IsModelRetryExhausted(err) {
		t.Fatalf("error=%v, want the context error, not an exhaustion", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestRetryTaskModelCallRejectsBadInput(t *testing.T) {
	_, err := retryTaskModelCall(context.Background(), 0, nil, sleepModelRetry)
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("error=%v, want invalid_argument", err)
	}
	_, err = retryTaskModelCall(nil, 0, func(context.Context) (ModelResponse, error) {
		return ModelResponse{}, nil
	}, sleepModelRetry)
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("error=%v, want invalid_argument", err)
	}
}

func TestModelRetryAfter(t *testing.T) {
	if got := modelRetryAfter(&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 45 * time.Second}); got != 45*time.Second {
		t.Fatalf("client retry-after=%v, want 45s", got)
	}
	err := domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
	err.RetryAfter = 30 * time.Second
	if got := modelRetryAfter(err); got != 30*time.Second {
		t.Fatalf("domain retry-after=%v, want 30s", got)
	}
	if got := modelRetryAfter(errors.New("plain failure")); got != 0 {
		t.Fatalf("plain retry-after=%v, want 0", got)
	}
}

func TestModelRetryExhaustedDomainError(t *testing.T) {
	exhausted := &ModelRetryExhaustedError{Reason: FailoverReasonRateLimited, Attempts: 3, LastErr: rateLimitedModelError()}
	err := modelRetryExhaustedDomainError(exhausted)
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("code=%s, want rate_limited", domain.CodeOf(err))
	}
	if !IsModelRetryExhausted(err) {
		t.Fatalf("error=%v, want the exhausted marker preserved", err)
	}
	var clientErr *ModelClientError
	if !errors.As(err, &clientErr) || clientErr.Kind != ModelErrorRateLimited {
		t.Fatalf("error=%v, want the provider error preserved", err)
	}
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) || domainErr.Details["reason"] != string(FailoverReasonRateLimited) || domainErr.Details["attempts"] != int64(3) {
		t.Fatalf("error details=%v, want reason and attempts", domainErr.Details)
	}
}

// scriptedModelClientTest fails per its script, then succeeds: one entry
// per call, a nil entry meaning success with the fixed response.
type scriptedModelClientTest struct {
	calls    int
	script   []error
	response ModelResponse
}

func (c *scriptedModelClientTest) Complete(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	c.calls++
	if c.calls-1 < len(c.script) && c.script[c.calls-1] != nil {
		return ModelResponse{}, c.script[c.calls-1]
	}
	return c.response, nil
}

func (c *scriptedModelClientTest) Check(context.Context, ModelEndpoint) (ModelCapabilities, error) {
	return ModelCapabilities{}, errors.New("unexpected check")
}

func summaryRetryFence(t *testing.T) ConversationSummaryModelFence {
	t.Helper()
	claim, _ := routeGatewayFixture()
	claim.Authorization = task.Authorization{Kind: "session", TokenID: "t1234567", Version: 1}
	identity, err := ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	inputDigest := sha256.Sum256([]byte("authorized summary input"))
	return ConversationSummaryModelFence{Claim: claim, Identity: identity, OperationID: operationID,
		InputSHA256: inputDigest[:], LeaseOwner: claim.LeaseOwner, LeaseVersion: claim.LeaseVersion,
		SessionID: "summary_session_1", ThroughSequence: 1, ContextWindowTokens: 4096}
}

func TestTaskModelRetryCompactionRetriesTransientFailures(t *testing.T) {
	store := &summaryCapacityStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &scriptedModelClientTest{
		script:   []error{rateLimitedModelError(), rateLimitedModelError()},
		response: ModelResponse{Text: "short summary", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}},
	}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	var sleeps []time.Duration
	service.modelRetrySleep = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}
	response, err := service.CompleteConversationSummaryModel(context.Background(), summaryRetryFence(t), "p1234567", ModelRequest{
		Instructions:    "Summarize authorized history.",
		Input:           []ModelInput{{Type: "message", Role: "user", Text: "Summarize this conversation."}},
		MaxOutputTokens: 64,
	})
	if err != nil || response.Text != "short summary" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if client.calls != 3 {
		t.Fatalf("client calls=%d, want 3 (one provider call per ladder step)", client.calls)
	}
	if len(sleeps) != 2 || sleeps[0] < 5*time.Second || sleeps[0] > 6250*time.Millisecond ||
		sleeps[1] < 10*time.Second || sleeps[1] > 12500*time.Millisecond {
		t.Fatalf("sleeps=%v, want [5s..6.25s 10s..12.5s] (production jitter adds up to 25%%)", sleeps)
	}
	// One reservation, one dispatch, one settlement for the whole retried call.
	if store.reserveN != 1 || store.dispatchN != 1 || store.settleN != 1 {
		t.Fatalf("reserve=%d dispatch=%d settle=%d, want 1 each", store.reserveN, store.dispatchN, store.settleN)
	}
}

func TestTaskModelRetryCompactionExhaustionIsTerminal(t *testing.T) {
	store := &summaryCapacityStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &scriptedModelClientTest{
		script:   []error{rateLimitedModelError(), rateLimitedModelError(), rateLimitedModelError(), rateLimitedModelError()},
		response: ModelResponse{Text: "short summary", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}},
	}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	service.modelRetrySleep = func(_ context.Context, _ time.Duration) error { return nil }
	_, err = service.CompleteConversationSummaryModel(context.Background(), summaryRetryFence(t), "p1234567", ModelRequest{
		Instructions:    "Summarize authorized history.",
		Input:           []ModelInput{{Type: "message", Role: "user", Text: "Summarize this conversation."}},
		MaxOutputTokens: 64,
	})
	if err == nil || !IsModelRetryExhausted(err) {
		t.Fatalf("error=%v, want a spent ladder", err)
	}
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("code=%s, want rate_limited", domain.CodeOf(err))
	}
	if client.calls != 4 {
		t.Fatalf("client calls=%d, want 4 (initial call plus three retries)", client.calls)
	}
	if store.settleN != 1 {
		t.Fatalf("settle=%d, want 1 (the spent call still settles once, as unknown)", store.settleN)
	}
}

// reviewRetryStoreTest is the compaction store fake plus the two review
// interfaces Complete needs for a review call.
type reviewRetryStoreTest struct{ *modelGatewayStoreTest }

func (*reviewRetryStoreTest) RecoverTaskValidationReviewModelResponse(context.Context, ModelCallReservation) (TaskValidationReviewModelResponseRecord, bool, error) {
	return TaskValidationReviewModelResponseRecord{}, false, nil
}

func (*reviewRetryStoreTest) ModelTaskProjectIDForReview(context.Context, TaskValidationReviewModelFence) (string, error) {
	return "", nil
}

func reviewRetryFence(t *testing.T) TaskValidationReviewModelFence {
	t.Helper()
	digest := sha256.Sum256([]byte("verified result"))
	claim := TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: "q1234567", TaskRevision: 4, ResultEventSequence: 9, ResultSHA256: digest[:], ClaimGeneration: 2,
		LeaseOwner: "worker-a"}
	return TaskValidationReviewModelFence{Claim: claim, InitiatorID: claim.InitiatorID, SessionID: "review-session-1", ThroughSequence: 3}
}

func TestTaskModelRetryReviewRetriesTransientFailures(t *testing.T) {
	store := &reviewRetryStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &scriptedModelClientTest{
		script: []error{&ModelClientError{Kind: ModelErrorTimeout}},
		response: ModelResponse{Text: `{"verdict":"pass"}`, StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3},
			ProviderReportedModel: "deepseek-flash"},
	}
	// The review response must resolve to a catalogued model family: use a
	// DeepSeek catalog entry for both the endpoint and the reported model.
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true,
		endpoint: ModelEndpoint{BaseURL: "https://api.deepseek.com", APIKey: "platform-key", ModelID: "deepseek-flash"}}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)
	var sleeps []time.Duration
	service.modelRetrySleep = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}
	fence := reviewRetryFence(t)
	callID, err := DSHTaskValidationReviewModelCallID(fence)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Complete(context.Background(), ModelCall{Source: ModelCallReview, Review: &fence,
		ModelID: "p1234567", CallID: callID, RawRequest: []byte(`{"content":"review"}`),
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "Review the result."}}, MaxOutputTokens: 64}})
	if err != nil || response.Text != `{"verdict":"pass"}` {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if client.calls != 2 {
		t.Fatalf("client calls=%d, want 2 (one retry after the timeout)", client.calls)
	}
	if len(sleeps) != 1 || sleeps[0] < 5*time.Second || sleeps[0] > 6250*time.Millisecond {
		t.Fatalf("sleeps=%v, want [5s..6.25s] (production jitter adds up to 25%%)", sleeps)
	}
	if store.reserveN != 1 || store.dispatchN != 1 || store.settleN != 1 {
		t.Fatalf("reserve=%d dispatch=%d settle=%d, want 1 each", store.reserveN, store.dispatchN, store.settleN)
	}
}
