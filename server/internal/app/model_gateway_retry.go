package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"morde/server/internal/domain"
)

// sleepModelRetryLadder waits for delay or until ctx ends. It never sleeps
// past the caller's deadline: the gateway callers run under a claim lease,
// so a cancelled context ends the ladder instead of holding the claim.
func sleepModelRetryLadder(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// modelCallRetryAfter extracts a provider Retry-After from a gateway model
// error, preferring the typed client error. Zero means the ladder falls back
// to jittered backoff.
func modelCallRetryAfter(err error) time.Duration {
	var clientErr *ModelClientError
	if errors.As(err, &clientErr) && clientErr != nil && clientErr.RetryAfter > 0 {
		return clientErr.RetryAfter
	}
	var routeErr *domain.Error
	if errors.As(err, &routeErr) && routeErr != nil && routeErr.RetryAfter > 0 {
		return routeErr.RetryAfter
	}
	return 0
}

// retryModelCallWithLadder applies the E12 ladder to one gateway model call
// (compaction, validation review). Transient failures retry in-process on the
// same provider with jittered backoff (provider Retry-After wins, capped at
// ModelRetryMaxRetryAfter) up to ModelRetryMaxRetries times; a spent ladder
// returns the last failure wrapped as a fallback or terminal error. sleep is
// injectable for tests; nil sleeps for real.
//
// Callers with their own durable queue must NOT use this: the route worker
// already owns its ladder (durable requeue + ModelRetryDelay), so wrapping a
// route call would double-retry.
func retryModelCallWithLadder(ctx context.Context, attempt func(context.Context) (ModelResponse, error), sleep func(context.Context, time.Duration) error) (ModelResponse, error) {
	if sleep == nil {
		sleep = sleepModelRetryLadder
	}
	attemptsUsed := 0
	for {
		response, err := attempt(ctx)
		if err == nil {
			return response, nil
		}
		classification := ClassifyModelClientError(err)
		decision := DecideModelRetry(classification, attemptsUsed, modelCallRetryAfter(err), ModelRetryMaxRetryAfter, rand.Float64)
		switch decision.Action {
		case ModelRetryActionRetry:
			attemptsUsed++
			if sleepErr := sleep(ctx, decision.Delay); sleepErr != nil {
				return ModelResponse{}, sleepErr
			}
		case ModelRetryActionFallback:
			return ModelResponse{}, fmt.Errorf("model retry ladder fallback for %s after %d attempts: %w", classification.Reason, attemptsUsed, err)
		default:
			return ModelResponse{}, fmt.Errorf("model retry ladder exhausted for %s after %d attempts: %w", classification.Reason, attemptsUsed, err)
		}
	}
}

// completeWithRetry runs one gateway model call through the E12 ladder.
func (s *ModelGatewayService) completeWithRetry(ctx context.Context, attempt func(context.Context) (ModelResponse, error)) (ModelResponse, error) {
	return retryModelCallWithLadder(ctx, attempt, nil)
}
