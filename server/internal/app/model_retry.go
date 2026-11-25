package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/domain"
)

// Task-level model retry (P3-1; compliance check §三第 6 项; enhancement
// plan E12; design proposal §16.5).
//
// Audit (2026-11-25, release base 80ada17 + P0/P1/P2): E12 shipped the
// ladder decision functions (DecideModelRetry, ApplyTaskModelFailureLadder,
// ClassifyModelError) and wired them into the routing worker only. No
// task-level model call retried: ModelGatewayService.Complete called the
// provider client exactly once, and both task-level callers
// (CompleteConversationSummaryModel for compaction and
// CompleteTaskValidationReviewDSHModel for review) called Complete once
// with no loop. The executor (pauseCloudDSHModelRecovery) only diverted
// never-retryable reasons to the terminal record; retryable reasons never
// consulted the ladder. There was also no cumulative delay cap anywhere
// (per-step cap 120s only; a provider Retry-After won uncapped).
//
// The retry loop lives here, inside Complete, around the provider client
// call and before settlement — not at the call sites re-entering Complete.
// A failed Complete attempt already settles its ledger row (unknown usage),
// and a second Complete with the same CallID is rejected at reservation
// ("model call was already dispatched"), so re-entering Complete could
// never retry. Retrying the raw client call keeps one reservation, one
// dispatch and one settlement per CallID.

// ModelRetryTotalDelayCap caps the total backoff a single task-level model
// call waits across all its retries. The per-step cap (120s) and the
// provider Retry-After still apply inside DecideModelRetry; when the next
// step would push the waited total past this cap, the ladder is spent
// instead of sleeping past it. The 45s dispatch timeout
// (ModelCallTimeout) bounds a call further in practice; this cap is the
// ladder's own ceiling.
const ModelRetryTotalDelayCap = 600 * time.Second

// ModelRetryExhaustedError is the terminal signal for a task-level model
// call whose retry ladder is spent. Reason is the classified failure,
// Attempts is the number of retries actually spent, and LastErr is the
// last provider error. It unwraps to LastErr, so error classification and
// domain codes underneath stay reachable through errors.As.
type ModelRetryExhaustedError struct {
	Reason   FailoverReason
	Attempts int64
	LastErr  error
}

func (e *ModelRetryExhaustedError) Error() string {
	if e == nil {
		return "model retry ladder exhausted"
	}
	return fmt.Sprintf("model retry ladder exhausted: %s after %d retries", e.Reason, e.Attempts)
}

func (e *ModelRetryExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.LastErr
}

// IsModelRetryExhausted reports whether err (or anything it wraps) is a
// spent task-level retry ladder.
func IsModelRetryExhausted(err error) bool {
	var exhausted *ModelRetryExhaustedError
	return errors.As(err, &exhausted)
}

// modelRetryAfter extracts the provider Retry-After from a model failure:
// the model client error carries it directly, and a domain error carries
// it on its RetryAfter field (see conversationRouteRateLimitDelay, which
// reads the same field for the routing ladder).
func modelRetryAfter(err error) time.Duration {
	var clientErr *ModelClientError
	if errors.As(err, &clientErr) && clientErr.RetryAfter > 0 {
		return clientErr.RetryAfter
	}
	var routeErr *domain.Error
	if errors.As(err, &routeErr) && routeErr.RetryAfter > 0 {
		return routeErr.RetryAfter
	}
	return 0
}

// sleepModelRetry waits for delay, or returns the context cause when the
// context ends first. It is the production sleep for the retry loop; the
// loop itself takes the sleep function so tests can record the delays
// without waiting.
func sleepModelRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// retryTaskModelCall runs one task-level model call through the E12 ladder:
// classify the failure, wait the ladder delay (Retry-After wins, per-step
// cap 120s, cumulative cap ModelRetryTotalDelayCap), and retry the same
// call. jitter must be in [0, 1); anything else means no jitter. The
// caller supplies the attempt function and the sleep function; a nil
// context, call or sleep is rejected. On success the response is
// returned as-is; when the ladder is spent the returned error is a
// *ModelRetryExhaustedError wrapping the last failure. A context that
// ends mid-loop returns its own error, not an exhaustion: cancellation
// is not a spent ladder. Post-settlement response-shape failures (an
// invalid summary or review body) are not retried here — they are only
// known after settlement, and a second attempt would need a second
// reservation under the same CallID, which the ledger refuses.
func retryTaskModelCall(ctx context.Context, jitter float64, call func(context.Context) (ModelResponse, error),
	sleep func(context.Context, time.Duration) error) (ModelResponse, error) {
	if ctx == nil || call == nil || sleep == nil {
		return ModelResponse{}, domain.InvalidArgument("model_call", "task model retry requires a call and a sleep function")
	}
	if jitter < 0 || jitter >= 1 {
		jitter = 0
	}
	var attempts int64
	var waited time.Duration
	for {
		response, err := call(ctx)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return response, err
		}
		reason := ClassifyModelError(err)
		decision := DecideModelRetry(reason, attempts, modelRetryAfter(err), jitter)
		if decision.Exhausted {
			return response, &ModelRetryExhaustedError{Reason: reason, Attempts: attempts, LastErr: err}
		}
		if waited+decision.Delay > ModelRetryTotalDelayCap {
			return response, &ModelRetryExhaustedError{Reason: reason, Attempts: attempts, LastErr: err}
		}
		waited += decision.Delay
		if err := sleep(ctx, decision.Delay); err != nil {
			return response, err
		}
		attempts++
	}
}

// modelRetryExhaustedDomainError converts a spent ladder into the domain
// error Complete returns after settlement. The code mirrors
// modelConnectionCheckError (rate limit and timeout keep their codes, the
// rest are upstream errors); the details name the reason and the retries
// spent, and the exhausted error stays reachable through Unwrap, so
// callers can tell "failed after retries" apart from "failed at once".
func modelRetryExhaustedDomainError(exhausted *ModelRetryExhaustedError) error {
	code := domain.CodeUpstreamError
	var clientErr *ModelClientError
	if errors.As(exhausted.LastErr, &clientErr) {
		switch clientErr.Kind {
		case ModelErrorRateLimited:
			code = domain.CodeRateLimited
		case ModelErrorTimeout:
			code = domain.CodeUpstreamTimeout
		}
	}
	err := domain.NewError(code, "model retry ladder exhausted")
	err.Details["reason"] = string(exhausted.Reason)
	err.Details["attempts"] = exhausted.Attempts
	err.Details["retry_ladder"] = "exhausted"
	err.Err = exhausted
	return err
}
