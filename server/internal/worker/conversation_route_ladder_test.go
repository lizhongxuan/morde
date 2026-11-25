package worker

import (
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestConversationRouteRateLimitDelayUsesRetryLadder(t *testing.T) {
	rateLimited := func(retryAfter time.Duration) error {
		return &domain.Error{Code: domain.CodeRateLimited,
			Details:    map[string]any{"reason": string(app.ModelErrorRateLimited), "stage": "provider_status"},
			RetryAfter: retryAfter}
	}
	cases := []struct {
		name        string
		attempt     int64
		retryAfter  time.Duration
		wantDelay   time.Duration
		wantConfirm bool
	}{
		{name: "first rate limit waits the base delay", attempt: 1, wantDelay: 5 * time.Second, wantConfirm: true},
		{name: "second rate limit doubles the delay", attempt: 2, wantDelay: 10 * time.Second, wantConfirm: true},
		{name: "third rate limit doubles again", attempt: 3, wantDelay: 20 * time.Second, wantConfirm: true},
		{name: "provider retry-after wins over backoff", attempt: 1, retryAfter: 45 * time.Second, wantDelay: 45 * time.Second, wantConfirm: true},
		{name: "ladder spent after 3 retries is not confirmed", attempt: 4, wantConfirm: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claim := app.ConversationRouteClaim{RouteAttempt: tc.attempt}
			delay, confirmed := conversationRouteRateLimitDelay(claim, rateLimited(tc.retryAfter))
			if confirmed != tc.wantConfirm {
				t.Fatalf("conversationRouteRateLimitDelay() confirmed = %v, want %v", confirmed, tc.wantConfirm)
			}
			// Jitter can only add up to 25%: the delay must be at least the
			// base step and below the next step.
			if tc.wantConfirm && (delay < tc.wantDelay || delay >= tc.wantDelay*2) {
				t.Fatalf("conversationRouteRateLimitDelay() delay = %v, want [%v, %v)", delay, tc.wantDelay, tc.wantDelay*2)
			}
		})
	}
	t.Run("non rate limited error is not confirmed", func(t *testing.T) {
		claim := app.ConversationRouteClaim{RouteAttempt: 1}
		if _, confirmed := conversationRouteRateLimitDelay(claim, &domain.Error{Code: domain.CodeUpstreamError, Details: map[string]any{}}); confirmed {
			t.Fatal("conversationRouteRateLimitDelay() must not confirm a non rate limited error")
		}
	})
	t.Run("retry-after beyond the durable horizon is confirmed with no delay", func(t *testing.T) {
		claim := app.ConversationRouteClaim{RouteAttempt: 1}
		delay, confirmed := conversationRouteRateLimitDelay(claim, rateLimited(25*time.Hour))
		if !confirmed || delay != 0 {
			t.Fatalf("conversationRouteRateLimitDelay() = (%v, %v), want (0, true)", delay, confirmed)
		}
	})
}

func TestConversationRouteRetryLadderExhausted(t *testing.T) {
	rateLimited := &domain.Error{Code: domain.CodeRateLimited,
		Details: map[string]any{"reason": string(app.ModelErrorRateLimited), "stage": "provider_status"}}
	if conversationRouteRetryLadderExhausted(app.ConversationRouteClaim{RouteAttempt: 3}, rateLimited) {
		t.Fatal("attempt 3 still has one retry left and must not report exhaustion")
	}
	if !conversationRouteRetryLadderExhausted(app.ConversationRouteClaim{RouteAttempt: 4}, rateLimited) {
		t.Fatal("attempt 4 has spent 3 retries and must report exhaustion")
	}
	if conversationRouteRetryLadderExhausted(app.ConversationRouteClaim{RouteAttempt: 4}, &domain.Error{Code: domain.CodeUpstreamError, Details: map[string]any{}}) {
		t.Fatal("a non rate limited error must not report ladder exhaustion")
	}
}
