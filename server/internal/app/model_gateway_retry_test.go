package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestRetryModelCallWithLadder(t *testing.T) {
	rateLimited := &ModelClientError{Kind: ModelErrorRateLimited}
	rateLimited45s := &ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 45 * time.Second}
	rateLimited900s := &ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 900 * time.Second}
	auth := &ModelClientError{Kind: ModelErrorAuthentication}
	access := &ModelClientError{Kind: ModelErrorAccess}
	quota := &ModelClientError{Kind: ModelErrorQuota}
	unknown := errors.New("provider exploded")

	cases := []struct {
		name       string
		script     []error // nil = success; past the end, the last error repeats
		wantCalls  int
		wantSleeps []time.Duration // exact delays; jittered delays use a range instead
		sleepLow   []time.Duration
		sleepHigh  []time.Duration
		wantErr    string
	}{
		{name: "success first try", script: []error{nil}, wantCalls: 1},
		{name: "rate limited twice then success", script: []error{rateLimited, rateLimited, nil}, wantCalls: 3,
			sleepLow: []time.Duration{5 * time.Second, 10 * time.Second}, sleepHigh: []time.Duration{7500 * time.Millisecond, 15 * time.Second}},
		{name: "rate limited always", script: []error{rateLimited}, wantCalls: 4,
			sleepLow:  []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second},
			sleepHigh: []time.Duration{7500 * time.Millisecond, 15 * time.Second, 30 * time.Second},
			// RateLimit's strategy offers a fallback, so a spent gateway ladder
			// reports fallback (the caller retries the claim another way);
			// the broker short-circuits the same state to terminal instead.
			wantErr: "fallback"},
		{name: "retry after wins over backoff", script: []error{rateLimited45s, nil}, wantCalls: 2,
			wantSleeps: []time.Duration{45 * time.Second}},
		{name: "retry after capped at 600s", script: []error{rateLimited900s, nil}, wantCalls: 2,
			wantSleeps: []time.Duration{600 * time.Second}},
		{name: "auth falls back immediately", script: []error{auth}, wantCalls: 1, wantErr: "fallback"},
		{name: "access is terminal immediately", script: []error{access}, wantCalls: 1, wantErr: "exhausted"},
		{name: "quota falls back immediately", script: []error{quota}, wantCalls: 1, wantErr: "fallback"},
		{name: "unknown error retries then exhausts", script: []error{unknown}, wantCalls: 4,
			sleepLow:  []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second},
			sleepHigh: []time.Duration{7500 * time.Millisecond, 15 * time.Second, 30 * time.Second}, wantErr: "exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			attempt := func(context.Context) (ModelResponse, error) {
				calls++
				err := tc.script[len(tc.script)-1]
				if calls-1 < len(tc.script) {
					err = tc.script[calls-1]
				}
				if err == nil {
					return ModelResponse{Text: "ok"}, nil
				}
				return ModelResponse{}, err
			}
			var sleeps []time.Duration
			sleep := func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				return nil
			}
			response, err := retryModelCallWithLadder(context.Background(), attempt, sleep)
			if calls != tc.wantCalls {
				t.Fatalf("model calls = %d; want %d (err = %v)", calls, tc.wantCalls, err)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v; want nil", err)
				}
				if response.Text != "ok" {
					t.Fatalf("response = %#v; want ok text", response)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v; want %q", err, tc.wantErr)
			}
			for i, want := range tc.wantSleeps {
				if i >= len(sleeps) || sleeps[i] != want {
					t.Fatalf("sleeps = %v; want %v at index %d", sleeps, tc.wantSleeps, i)
				}
			}
			for i := range tc.sleepLow {
				if i >= len(sleeps) || sleeps[i] < tc.sleepLow[i] || sleeps[i] > tc.sleepHigh[i] {
					t.Fatalf("sleeps = %v; want [%v, %v] at index %d", sleeps, tc.sleepLow[i], tc.sleepHigh[i], i)
				}
			}
			if len(sleeps) != len(tc.wantSleeps)+len(tc.sleepLow) {
				t.Fatalf("sleeps = %v; want %d sleeps", sleeps, len(tc.wantSleeps)+len(tc.sleepLow))
			}
		})
	}
}

func TestRetryModelCallWithLadderStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempt := func(context.Context) (ModelResponse, error) {
		return ModelResponse{}, &ModelClientError{Kind: ModelErrorRateLimited}
	}
	sleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	_, err := retryModelCallWithLadder(ctx, attempt, sleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if err := sleepModelRetryLadder(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepModelRetryLadder on cancelled context = %v; want context.Canceled", err)
	}
}

func TestModelCallRetryAfter(t *testing.T) {
	if got := modelCallRetryAfter(&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: 45 * time.Second}); got != 45*time.Second {
		t.Fatalf("client Retry-After = %v; want 45s", got)
	}
	routeErr := domain.NewError(domain.CodeUpstreamError, "provider unavailable")
	routeErr.RetryAfter = 30 * time.Second
	if got := modelCallRetryAfter(routeErr); got != 30*time.Second {
		t.Fatalf("domain Retry-After = %v; want 30s", got)
	}
	if got := modelCallRetryAfter(errors.New("plain failure")); got != 0 {
		t.Fatalf("plain error Retry-After = %v; want 0", got)
	}
	if got := modelCallRetryAfter(&ModelClientError{Kind: ModelErrorRateLimited, RetryAfter: -time.Second}); got != 0 {
		t.Fatalf("negative Retry-After = %v; want 0", got)
	}
}
