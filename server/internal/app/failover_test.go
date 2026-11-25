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
		t.Fatalf("AllFailoverReasons() returned %d reasons, want 31", len(reasons))
	}
	seen := make(map[FailoverReason]struct{}, len(reasons))
	for _, reason := range reasons {
		if _, exists := seen[reason]; exists {
			t.Fatalf("AllFailoverReasons() lists %q twice", reason)
		}
		seen[reason] = struct{}{}
		strategy := FailoverStrategyFor(reason)
		if strategy.Retryable && strategy.MaxAttempts < 1 {
			t.Fatalf("FailoverStrategyFor(%q) is retryable with %d attempts", reason, strategy.MaxAttempts)
		}
		if strategy.AfterExhaustion == "" {
			t.Fatalf("FailoverStrategyFor(%q) has no after-exhaustion action", reason)
		}
	}
	// An unlisted reason never silently passes: it gets one retry, then a
	// method switch.
	strategy := FailoverStrategyFor(FailoverReason("not_a_reason"))
	if !strategy.Retryable || strategy.MaxAttempts != 1 || strategy.AfterExhaustion != FailoverActionSwitchMethod {
		t.Fatalf("FailoverStrategyFor(unlisted) = %+v, want one retry then switch_method", strategy)
	}
}

func TestJitteredBackoffBounds(t *testing.T) {
	cases := []struct {
		name    string
		attempt int64
		jitter  float64
		wantMin time.Duration
		wantMax time.Duration
	}{
		{name: "first retry is the base delay", attempt: 0, jitter: 0, wantMin: 5 * time.Second, wantMax: 5 * time.Second},
		{name: "second retry doubles", attempt: 1, jitter: 0, wantMin: 10 * time.Second, wantMax: 10 * time.Second},
		{name: "third retry doubles again", attempt: 2, jitter: 0, wantMin: 20 * time.Second, wantMax: 20 * time.Second},
		{name: "full jitter adds at most 25 percent", attempt: 0, jitter: 1, wantMin: 5 * time.Second, wantMax: 6*time.Second + 250*time.Millisecond},
		{name: "backoff never exceeds the max", attempt: 10, jitter: 1, wantMin: 120 * time.Second, wantMax: 120 * time.Second},
		{name: "negative attempt counts as zero", attempt: -3, jitter: 0, wantMin: 5 * time.Second, wantMax: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JitteredBackoff(tc.attempt, ModelRetryBaseDelay, ModelRetryMaxDelay, tc.jitter)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("JitteredBackoff(%d, jitter=%v) = %v, want [%v, %v]", tc.attempt, tc.jitter, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestDecideModelRetry(t *testing.T) {
	cases := []struct {
		name       string
		reason     FailoverReason
		attempt    int64
		retryAfter time.Duration
		wantAction FailoverAction
		wantDelay  time.Duration
		exhausted  bool
	}{
		{name: "rate limited first retry waits the base delay", reason: FailoverReasonRateLimited, attempt: 0, wantAction: FailoverActionRetry, wantDelay: 5 * time.Second},
		{name: "rate limited second retry doubles", reason: FailoverReasonRateLimited, attempt: 1, wantAction: FailoverActionRetry, wantDelay: 10 * time.Second},
		{name: "rate limited third retry doubles again", reason: FailoverReasonRateLimited, attempt: 2, wantAction: FailoverActionRetry, wantDelay: 20 * time.Second},
		{name: "rate limited after 3 retries switches method", reason: FailoverReasonRateLimited, attempt: 3, wantAction: FailoverActionSwitchMethod, exhausted: true},
		{name: "retry-after wins over backoff", reason: FailoverReasonRateLimited, attempt: 0, retryAfter: 30 * time.Second, wantAction: FailoverActionRetry, wantDelay: 30 * time.Second},
		{name: "retry-after rounds up to whole seconds", reason: FailoverReasonRateLimited, attempt: 0, retryAfter: 1500 * time.Millisecond, wantAction: FailoverActionRetry, wantDelay: 2 * time.Second},
		{name: "authentication failure is terminal at once", reason: FailoverReasonAuthenticationRejected, attempt: 0, wantAction: FailoverActionTerminal, exhausted: true},
		{name: "quota exceeded is terminal at once", reason: FailoverReasonQuotaExceeded, attempt: 0, wantAction: FailoverActionTerminal, exhausted: true},
		{name: "context limit is terminal at once", reason: FailoverReasonContextLengthExceeded, attempt: 0, wantAction: FailoverActionTerminal, exhausted: true},
		{name: "tool failure hands to another agent after one retry", reason: FailoverReasonToolFailed, attempt: 0, wantAction: FailoverActionRetry, wantDelay: 5 * time.Second},
		{name: "tool failure exhausted switches agent", reason: FailoverReasonToolFailed, attempt: 1, wantAction: FailoverActionSwitchAgent, exhausted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := DecideModelRetry(tc.reason, tc.attempt, tc.retryAfter, 0)
			if decision.Action != tc.wantAction {
				t.Fatalf("DecideModelRetry() action = %v, want %v", decision.Action, tc.wantAction)
			}
			if decision.Exhausted != tc.exhausted {
				t.Fatalf("DecideModelRetry() exhausted = %v, want %v", decision.Exhausted, tc.exhausted)
			}
			if tc.wantAction == FailoverActionRetry && decision.Delay != tc.wantDelay {
				t.Fatalf("DecideModelRetry() delay = %v, want %v", decision.Delay, tc.wantDelay)
			}
		})
	}
}

func TestApplyTaskModelFailureLadderAgentDepth(t *testing.T) {
	// Tool failure exhausts into a handover; depth 1 allows it, depth 0
	// turns it into terminal instead.
	allowed := ApplyTaskModelFailureLadder(FailoverReasonToolFailed, 1, 0, 0, 1)
	if allowed.Action != FailoverActionSwitchAgent {
		t.Fatalf("ApplyTaskModelFailureLadder(depth 1) action = %v, want switch_agent", allowed.Action)
	}
	denied := ApplyTaskModelFailureLadder(FailoverReasonToolFailed, 1, 0, 0, 0)
	if denied.Action != FailoverActionTerminal || !denied.Exhausted {
		t.Fatalf("ApplyTaskModelFailureLadder(depth 0) = %+v, want terminal and exhausted", denied)
	}
	if got := ModelRetryMaxAgentDepth; got != 1 {
		t.Fatalf("ModelRetryMaxAgentDepth = %d, want 1 (default, pending the depth decision)", got)
	}
}

func TestClassifyModelError(t *testing.T) {
	rateLimited := &domain.Error{Code: domain.CodeRateLimited, Details: map[string]any{"reason": "rate_limited", "stage": "provider_status"}, RetryAfter: time.Second}
	if got := ClassifyModelError(rateLimited); got != FailoverReasonRateLimited {
		t.Fatalf("ClassifyModelError(rate limited) = %v, want rate_limited", got)
	}
	clientErr := &ModelClientError{Kind: ModelErrorTimeout, Stage: "provider_status"}
	if got := ClassifyModelError(clientErr); got != FailoverReasonTimeout {
		t.Fatalf("ClassifyModelError(timeout client error) = %v, want timeout", got)
	}
	quotaErr := &domain.Error{Code: domain.CodeQuotaExceeded, Details: map[string]any{}}
	if got := ClassifyModelError(quotaErr); got != FailoverReasonQuotaExceeded {
		t.Fatalf("ClassifyModelError(quota) = %v, want quota_exceeded", got)
	}
	if got := ClassifyModelError(errors.New("connection refused")); got != FailoverReasonConnectionRefused {
		t.Fatalf("ClassifyModelError(connection refused) = %v, want connection_refused", got)
	}
	if got := ClassifyModelError(errors.New("something entirely new")); got != FailoverReasonUnknown {
		t.Fatalf("ClassifyModelError(unrecognized) = %v, want unknown", got)
	}
	if got := ClassifyModelError(nil); got != FailoverReasonUnknown {
		t.Fatalf("ClassifyModelError(nil) = %v, want unknown", got)
	}
}

func TestLadderExhaustedUserMessage(t *testing.T) {
	message := LadderExhaustedMessage(FailoverReasonTimeout, ModelRetryMaxRetries)
	if !strings.Contains(message, "网络超时") {
		t.Fatalf("LadderExhaustedMessage() = %q, want it to name the cause", message)
	}
	if !strings.Contains(message, "3") {
		t.Fatalf("LadderExhaustedMessage() = %q, want it to state the retry count", message)
	}
	for _, banned := range []string{"stack", "goroutine", "panic", "Error:", "at ", ".go:"} {
		if strings.Contains(message, banned) {
			t.Fatalf("LadderExhaustedMessage() = %q, must not contain %q", message, banned)
		}
	}
	if !strings.Contains(message, "不需要做什么") {
		t.Fatalf("LadderExhaustedMessage() = %q, must tell the user no action is needed", message)
	}
	terminal := LadderExhaustedMessage(FailoverReasonAuthenticationRejected, 0)
	if strings.Contains(terminal, "stack") || strings.Contains(terminal, "Error:") {
		t.Fatalf("LadderExhaustedMessage(terminal) = %q, must not leak internals", terminal)
	}
}
