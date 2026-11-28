package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestAllFailoverReasonsCoveredByStrategyTable(t *testing.T) {
	reasons := AllFailoverReasons()
	if len(reasons) != 31 {
		t.Fatalf("failover reasons = %d, want 31 (hermes error_classifier.py:35)", len(reasons))
	}
	seen := make(map[FailoverReason]bool, len(reasons))
	for _, reason := range reasons {
		if seen[reason] {
			t.Fatalf("duplicate failover reason %q", reason)
		}
		seen[reason] = true
		strategy, ok := failoverReasonStrategies[reason]
		if !ok {
			t.Fatalf("failover reason %q has no strategy entry", reason)
		}
		if strategy.UserVisibleCase < 0 || strategy.UserVisibleCase > 4 {
			t.Fatalf("failover reason %q user visible case = %d, want 0-4 (§4.3)", reason, strategy.UserVisibleCase)
		}
	}
}

func TestFailoverStrategyGroups(t *testing.T) {
	// Transient: same-provider retry, no fallback, never user-visible alone.
	for _, reason := range []FailoverReason{FailoverTimeout, FailoverOverloaded, FailoverServerError} {
		strategy := FailoverStrategyFor(reason)
		if !strategy.Retryable || strategy.Fallback || strategy.NeedsRepair {
			t.Fatalf("transient reason %q strategy = %+v", reason, strategy)
		}
	}
	// Rate limit: retry, then fallback to another method/model.
	for _, reason := range []FailoverReason{FailoverRateLimit, FailoverUpstreamRateLimit} {
		strategy := FailoverStrategyFor(reason)
		if !strategy.Retryable || !strategy.Fallback {
			t.Fatalf("rate limit reason %q strategy = %+v", reason, strategy)
		}
	}
	// Repair-only: the ladder cannot fix the request by itself.
	for _, reason := range []FailoverReason{FailoverContextOverflow, FailoverPayloadTooLarge, FailoverImageTooLarge,
		FailoverImageCorrupt, FailoverLongContextTier, FailoverThinkingSignature, FailoverInvalidEncryptedContent,
		FailoverMultimodalToolContentUnsupported, FailoverReasoningMandatory, FailoverOAuthLongContextBetaForbidden,
		FailoverLlamaCppGrammarPattern, FailoverRoleAlternation} {
		strategy := FailoverStrategyFor(reason)
		if strategy.Retryable || strategy.Fallback || !strategy.NeedsRepair {
			t.Fatalf("repair reason %q strategy = %+v", reason, strategy)
		}
	}
	// Terminal: no retry, no fallback; exhaustion maps to a §4.3 case.
	for _, reason := range []FailoverReason{FailoverAuthPermanent, FailoverSSLCertVerification,
		FailoverProviderPolicyBlocked, FailoverDataPolicyDenied, FailoverContentPolicyBlocked,
		FailoverGuardrailBlocked, FailoverFormatError} {
		strategy := FailoverStrategyFor(reason)
		if strategy.Retryable || strategy.Fallback || strategy.NeedsRepair || strategy.UserVisibleCase == 0 {
			t.Fatalf("terminal reason %q strategy = %+v", reason, strategy)
		}
	}
	if strategy := FailoverStrategyFor(FailoverReason("not_a_reason")); strategy != FailoverStrategyFor(FailoverUnknown) {
		t.Fatalf("unknown reason did not fall back to the unknown strategy: %+v", strategy)
	}
}

func TestClassifyModelClientErrorKinds(t *testing.T) {
	cases := []struct {
		kind         ModelClientErrorKind
		providerCode string
		want         FailoverReason
	}{
		{ModelErrorAuthentication, "", FailoverAuth},
		{ModelErrorAccess, "", FailoverAuthPermanent},
		{ModelErrorRateLimited, "", FailoverRateLimit},
		{ModelErrorQuota, "", FailoverBilling},
		{ModelErrorInvalidRequest, "", FailoverFormatError},
		{ModelErrorUnavailable, "", FailoverServerError},
		{ModelErrorTimeout, "", FailoverTimeout},
		{ModelErrorTransport, "", FailoverTimeout},
		{ModelErrorInvalidResponse, "", FailoverFormatError},
		{ModelErrorEmptyResponse, "", FailoverIncompleteResponse},
		{ModelErrorTruncatedResponse, "", FailoverIncompleteResponse},
		{ModelErrorContentFiltered, "", FailoverGuardrailBlocked},
		// Provider-code refinement wins over the kind.
		{ModelErrorInvalidRequest, "context_length_exceeded", FailoverContextOverflow},
		{ModelErrorInvalidRequest, "model_not_found", FailoverModelNotFound},
		{ModelErrorRateLimited, "insufficient_quota", FailoverBilling},
		{ModelErrorUnavailable, "server_error", FailoverServerError},
		{ModelErrorRateLimited, "rate_limit_exceeded", FailoverRateLimit},
		{ModelErrorAccess, "invalid_api_key", FailoverAuth},
		{ModelErrorAccess, "permission_denied", FailoverAuthPermanent},
		{ModelErrorRateLimited, "1305", FailoverOverloaded},
		{ModelClientErrorKind("not_a_kind"), "", FailoverUnknown},
	}
	for _, tc := range cases {
		classification := ClassifyModelClientError(&ModelClientError{Kind: tc.kind, ProviderCode: tc.providerCode})
		if classification.Reason != tc.want || !classification.Known || classification.SourceKind != tc.kind {
			t.Fatalf("classify kind %q code %q = %+v, want reason %q", tc.kind, tc.providerCode, classification, tc.want)
		}
		if classification.Strategy != FailoverStrategyFor(tc.want) {
			t.Fatalf("classify kind %q strategy mismatch: %+v", tc.kind, classification.Strategy)
		}
	}
}

func TestClassifyModelClientErrorDomainError(t *testing.T) {
	routeErr := domain.NewError(domain.CodeRateLimited, "model connection check failed")
	routeErr.Details["reason"] = string(ModelErrorRateLimited)
	routeErr.Details["stage"] = "provider_status"
	routeErr.Details["provider_code"] = "rate_limit_exceeded"
	classification := ClassifyModelClientError(routeErr)
	if classification.Reason != FailoverRateLimit || !classification.Known {
		t.Fatalf("classify domain rate limit = %+v", classification)
	}

	contextErr := domain.NewError(domain.CodeResultTooLarge, "conversation route exceeds the provider context window")
	contextErr.Details["stage"] = "provider_status"
	contextErr.Details["provider_code"] = "context_length_exceeded"
	classification = ClassifyModelClientError(contextErr)
	if classification.Reason != FailoverContextOverflow || !classification.Known {
		t.Fatalf("classify domain context overflow without reason detail = %+v (want context_overflow)", classification)
	}

	plain := errors.New("boom")
	classification = ClassifyModelClientError(plain)
	if classification.Reason != FailoverUnknown || !classification.Strategy.Retryable || classification.Known {
		t.Fatalf("classify plain error = %+v, want unknown retryable fallback", classification)
	}
	if classification = ClassifyModelClientError(nil); classification.Reason != FailoverUnknown {
		t.Fatalf("classify nil error = %+v, want unknown", classification)
	}
}

func TestJitteredBackoffBounds(t *testing.T) {
	zero := func() float64 { return 0 }
	full := func() float64 { return 1 }
	cases := []struct {
		attempt  int
		wantLow  time.Duration
		wantHigh time.Duration
	}{
		{0, 5 * time.Second, 7500 * time.Millisecond},
		{1, 5 * time.Second, 7500 * time.Millisecond},
		{2, 10 * time.Second, 15 * time.Second},
		{3, 20 * time.Second, 30 * time.Second},
		{4, 40 * time.Second, 60 * time.Second},
		{10, 120 * time.Second, 180 * time.Second},
	}
	for _, tc := range cases {
		if got := JitteredBackoff(tc.attempt, ModelRetryBackoffBase, ModelRetryBackoffMax, ModelRetryJitterRatio, zero); got != tc.wantLow {
			t.Fatalf("backoff attempt %d with zero jitter = %v, want %v", tc.attempt, got, tc.wantLow)
		}
		if got := JitteredBackoff(tc.attempt, ModelRetryBackoffBase, ModelRetryBackoffMax, ModelRetryJitterRatio, full); got != tc.wantHigh {
			t.Fatalf("backoff attempt %d with full jitter = %v, want %v", tc.attempt, got, tc.wantHigh)
		}
	}
	if got := JitteredBackoff(1, ModelRetryBackoffBase, ModelRetryBackoffMax, ModelRetryJitterRatio, nil); got != 5*time.Second {
		t.Fatalf("backoff with nil rand = %v, want lower bound 5s", got)
	}
	if got := JitteredBackoff(1, 0, ModelRetryBackoffMax, ModelRetryJitterRatio, zero); got != 0 {
		t.Fatalf("backoff with zero base = %v, want 0", got)
	}
}

func TestModelRetryDelay(t *testing.T) {
	zero := func() float64 { return 0 }
	// Retry-After wins over backoff, capped at the caller's cap.
	if delay, ok := ModelRetryDelay(1, 30*time.Second, ModelRetryMaxRetryAfter, zero); !ok || delay != 30*time.Second {
		t.Fatalf("retry delay with Retry-After 30s = %v, %t", delay, ok)
	}
	if delay, ok := ModelRetryDelay(1, 900*time.Second, ModelRetryMaxRetryAfter, zero); !ok || delay != ModelRetryMaxRetryAfter {
		t.Fatalf("retry delay with Retry-After 900s = %v, %t, want capped at 600s", delay, ok)
	}
	// No Retry-After: jittered backoff per attempt.
	if delay, ok := ModelRetryDelay(2, 0, ModelRetryMaxRetryAfter, zero); !ok || delay != 10*time.Second {
		t.Fatalf("retry delay attempt 2 = %v, %t, want 10s", delay, ok)
	}
	// Budget spent: no fourth retry.
	if _, ok := ModelRetryDelay(ModelRetryMaxRetries+1, 0, ModelRetryMaxRetryAfter, zero); ok {
		t.Fatal("retry delay past max retries was allowed")
	}
	if _, ok := ModelRetryDelay(0, 0, ModelRetryMaxRetryAfter, zero); !ok {
		t.Fatal("retry delay attempt 0 was rejected instead of counting as the first attempt")
	}
}

func TestDecideModelRetry(t *testing.T) {
	zero := func() float64 { return 0 }
	transient := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorTimeout})
	decision := DecideModelRetry(transient, 0, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionRetry || decision.Delay != 5*time.Second || decision.Exhausted {
		t.Fatalf("transient first failure decision = %+v", decision)
	}
	decision = DecideModelRetry(transient, 2, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionRetry || decision.Delay != 20*time.Second {
		t.Fatalf("transient third retry decision = %+v, want retry after 20s", decision)
	}
	decision = DecideModelRetry(transient, 3, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionTerminal || !decision.Exhausted {
		t.Fatalf("transient spent decision = %+v, want terminal exhausted", decision)
	}

	rateLimited := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorRateLimited})
	decision = DecideModelRetry(rateLimited, 3, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionFallback || !decision.Exhausted {
		t.Fatalf("rate limit spent decision = %+v, want fallback", decision)
	}

	billing := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorQuota})
	decision = DecideModelRetry(billing, 0, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionFallback || !decision.Exhausted {
		t.Fatalf("billing first failure decision = %+v, want immediate fallback with no same-provider retry", decision)
	}

	overflow := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorInvalidRequest, ProviderCode: "context_length_exceeded"})
	decision = DecideModelRetry(overflow, 0, 0, ModelRetryMaxRetryAfter, zero)
	if decision.Action != ModelRetryActionTerminal || !decision.Exhausted || decision.UserVisibleCase != 4 {
		t.Fatalf("context overflow decision = %+v, want terminal case 4", decision)
	}

	blocked := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorInvalidRequest, ProviderCode: "content_filter"})
	if blocked.Reason != FailoverFormatError {
		t.Fatalf("unlisted provider code classified as %q, want kind-level format_error", blocked.Reason)
	}
}

func TestLadderExhaustedUserMessage(t *testing.T) {
	network := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorTimeout})
	message, ok := LadderExhaustedUserMessage(network, 3)
	if !ok || !strings.Contains(message, "3") || strings.Contains(message, "timeout") || strings.Contains(message, "stack") {
		t.Fatalf("network terminal message = %q, %t", message, ok)
	}
	auth := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorAuthentication})
	message, ok = LadderExhaustedUserMessage(auth, 3)
	if !ok || !strings.Contains(message, "授权") {
		t.Fatalf("auth terminal message = %q, %t", message, ok)
	}
	policy := ClassifyModelClientError(&ModelClientError{Kind: ModelErrorInvalidRequest, ProviderCode: "context_length_exceeded"})
	message, ok = LadderExhaustedUserMessage(policy, 3)
	if !ok || !strings.Contains(message, "做不下去") {
		t.Fatalf("overflow terminal message = %q, %t", message, ok)
	}
}
