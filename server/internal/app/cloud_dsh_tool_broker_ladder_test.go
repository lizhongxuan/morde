package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

func newLadderTestBroker(t *testing.T) (*CloudDSHToolBroker, *cloudDSHBrokerTaskStore) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return broker, store
}

func seedLadderMarkerSteps(store *cloudDSHBrokerTaskStore, sequences ...int64) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, sequence := range sequences {
		store.details.Steps = append(store.details.Steps, task.Step{
			Sequence: sequence, Kind: "verification", Status: task.StepFailed,
			FailureReason: ModelRetryLadderFailureReason, Retryable: true,
		})
	}
}

func seedLadderProgressStep(store *cloudDSHBrokerTaskStore, sequence int64) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.details.Steps = append(store.details.Steps, task.Step{
		Sequence: sequence, Kind: "execution", Status: task.StepSucceeded,
	})
}

func lastLadderStep(store *cloudDSHBrokerTaskStore) task.Step {
	return store.details.Steps[len(store.details.Steps)-1]
}

func TestClassifyModelFailureDiagnostic(t *testing.T) {
	cases := []struct {
		category string
		reason   FailoverReason
		want     FailoverReason
	}{
		{category: "rate_limited", want: FailoverRateLimit},
		{category: "provider_rate_limited", want: FailoverRateLimit},
		{category: "timeout", want: FailoverTimeout},
		{category: "transport", want: FailoverTimeout},
		{category: "unavailable", want: FailoverServerError},
		{category: "upstream_error", want: FailoverServerError},
		{category: "provider_request_rejected", want: FailoverServerError},
		{category: "quota_exhausted", want: FailoverBilling},
		{category: "task_budget_exhausted", want: FailoverBilling},
		{category: "authentication_rejected", want: FailoverAuth},
		{category: "provider_invalid_api_key", want: FailoverAuth},
		{category: "access_rejected", want: FailoverAuthPermanent},
		{category: "invalid_request", want: FailoverFormatError},
		{category: "provider_unsupported_parameter", want: FailoverFormatError},
		{category: "invalid_response", want: FailoverIncompleteResponse},
		{category: "truncated_response", want: FailoverIncompleteResponse},
		{category: "context_length_exceeded", want: FailoverContextOverflow},
		{category: "provider_model_not_found", want: FailoverModelNotFound},
		{category: "unauthorized_tool", want: FailoverGuardrailBlocked},
		{category: "attachment_reader_unavailable", want: FailoverUnknown},
		{category: "usage_unknown", want: FailoverUnknown},
		{category: "unsupported", want: FailoverUnknown},
		{category: "", want: FailoverUnknown},
		{category: "something_new", want: FailoverUnknown},
		// A diagnostic that already carries a valid reason wins over its category.
		{category: "timeout", reason: FailoverRateLimit, want: FailoverRateLimit},
		{category: "rate_limited", reason: FailoverReason("not_a_reason"), want: FailoverRateLimit},
	}
	for _, tc := range cases {
		got := ClassifyModelFailureDiagnostic(AgentModelFailureDiagnostic{Category: tc.category, Reason: tc.reason})
		if got.Reason != tc.want {
			t.Errorf("category %q reason %q = %q; want %q", tc.category, tc.reason, got.Reason, tc.want)
		}
	}
}

func TestCountTrailingModelRetryLadderAttempts(t *testing.T) {
	marker := func(sequence int64) task.Step {
		return task.Step{Sequence: sequence, FailureReason: ModelRetryLadderFailureReason}
	}
	progress := func(sequence int64) task.Step {
		return task.Step{Sequence: sequence, Status: task.StepSucceeded}
	}
	cases := []struct {
		name  string
		steps []task.Step
		want  int
	}{
		{name: "none", steps: nil, want: 0},
		{name: "one marker", steps: []task.Step{marker(1)}, want: 1},
		{name: "three markers", steps: []task.Step{marker(1), marker(2), marker(3)}, want: 3},
		{name: "progress breaks the chain", steps: []task.Step{marker(1), marker(2), progress(3)}, want: 0},
		{name: "progress then markers", steps: []task.Step{marker(1), progress(2), marker(3), marker(4)}, want: 2},
		{name: "pause step breaks the chain", steps: []task.Step{marker(1), task.Step{Sequence: 2, Status: task.StepUnknown}}, want: 0},
	}
	for _, tc := range cases {
		if got := countTrailingModelRetryLadderAttempts(tc.steps); got != tc.want {
			t.Errorf("%s = %d; want %d", tc.name, got, tc.want)
		}
	}
}

func TestApplyTaskModelFailureLadder(t *testing.T) {
	rateLimited := AgentModelFailureDiagnostic{Category: "rate_limited"}
	auth := AgentModelFailureDiagnostic{Category: "authentication_rejected"}
	quota := AgentModelFailureDiagnostic{Category: "quota_exhausted"}
	unknown := AgentModelFailureDiagnostic{Category: "weird_new_category"}

	t.Run("transient failure schedules a retry and leaves the task running", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, rateLimited)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionRetry || decision.Exhausted || decision.Delay <= 0 {
			t.Fatalf("decision = %+v; want a retry with a delay", decision)
		}
		if store.details.Task.Status != task.StatusRunning {
			t.Fatalf("task status = %s; want running", store.details.Task.Status)
		}
		if len(store.details.Steps) != 1 {
			t.Fatalf("steps = %d; want 1 retry step", len(store.details.Steps))
		}
		step := lastLadderStep(store)
		if step.FailureReason != ModelRetryLadderFailureReason || !step.Retryable || step.Attempt != 1 ||
			step.Status != task.StepFailed || !strings.Contains(step.Summary, "第 1 次重试") {
			t.Fatalf("retry step = %#v; want marker, retryable, attempt 1, failed, first-retry text", step)
		}
	})

	t.Run("two spent retries schedule the third", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		seedLadderMarkerSteps(store, 1, 2)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, rateLimited)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionRetry {
			t.Fatalf("decision = %+v; want retry", decision)
		}
		if step := lastLadderStep(store); step.Attempt != 3 || !strings.Contains(step.Summary, "第 3 次重试") {
			t.Fatalf("retry step = %#v; want attempt 3", step)
		}
	})

	t.Run("three spent retries end the ladder with the terminal text", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		seedLadderMarkerSteps(store, 1, 2, 3)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, rateLimited)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionTerminal || !decision.Exhausted {
			t.Fatalf("decision = %+v; want terminal and exhausted", decision)
		}
		if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "model_retry_ladder_exhausted" {
			t.Fatalf("task = %+v; want waiting with ladder-exhausted reason", store.details.Task)
		}
		step := lastLadderStep(store)
		if !strings.Contains(step.Summary, "已重试 3 次") || strings.Contains(step.Summary, "rate_limit") {
			t.Fatalf("terminal step summary = %q; want attempt count, no raw reason", step.Summary)
		}
		var terminalEvent struct {
			Detail map[string]any `json:"detail"`
		}
		last := store.events[len(store.events)-1]
		if err := json.Unmarshal(last.Payload, &terminalEvent); err != nil || terminalEvent.Detail["ladder_exhausted"] != true ||
			terminalEvent.Detail["retry_attempts"] != float64(ModelRetryMaxRetries) {
			t.Fatalf("terminal event detail = %+v, err=%v", terminalEvent, err)
		}
	})

	t.Run("a progress step resets the count", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		seedLadderMarkerSteps(store, 1, 2)
		seedLadderProgressStep(store, 3)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, rateLimited)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionRetry {
			t.Fatalf("decision = %+v; want retry", decision)
		}
		if step := lastLadderStep(store); step.Attempt != 1 {
			t.Fatalf("retry step = %#v; want attempt 1 after the reset", step)
		}
	})

	t.Run("auth failure falls back to a pause without a retry step", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, auth)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionFallback || !decision.Exhausted {
			t.Fatalf("decision = %+v; want fallback and exhausted", decision)
		}
		if store.details.Task.Status != task.StatusWaitingUser {
			t.Fatalf("task status = %s; want waiting", store.details.Task.Status)
		}
		for _, step := range store.details.Steps {
			if step.FailureReason == ModelRetryLadderFailureReason {
				t.Fatalf("fallback persisted a retry step: %#v", step)
			}
		}
	})

	t.Run("quota failure falls back to a pause", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, quota)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionFallback {
			t.Fatalf("decision = %+v; want fallback", decision)
		}
		if store.details.Task.Status != task.StatusWaitingUser {
			t.Fatalf("task status = %s; want waiting", store.details.Task.Status)
		}
	})

	t.Run("unknown category retries", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, unknown)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionRetry || decision.Reason != FailoverUnknown {
			t.Fatalf("decision = %+v; want an unknown retry", decision)
		}
	})

	t.Run("already exhausted diagnostic ends the ladder without a retry step", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, AgentModelFailureDiagnostic{
			Category: "model_retry_ladder_exhausted", LadderExhausted: true, Attempts: ModelRetryMaxRetries, Reason: FailoverTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionTerminal || !decision.Exhausted {
			t.Fatalf("decision = %+v; want terminal and exhausted", decision)
		}
		if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "model_retry_ladder_exhausted" {
			t.Fatalf("task = %+v; want waiting with ladder-exhausted reason", store.details.Task)
		}
		for _, step := range store.details.Steps {
			if step.FailureReason == ModelRetryLadderFailureReason {
				t.Fatalf("exhausted diagnostic persisted a retry step: %#v", step)
			}
		}
	})

	t.Run("a task that is no longer running persists nothing", func(t *testing.T) {
		broker, store := newLadderTestBroker(t)
		store.mu.Lock()
		store.details.Task.Status = task.StatusWaitingUser
		store.mu.Unlock()
		decision, err := broker.ApplyTaskModelFailureLadder(context.Background(), store.details.Task, rateLimited)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ModelRetryActionTerminal {
			t.Fatalf("decision = %+v; want terminal", decision)
		}
		if len(store.details.Steps) != 0 || len(store.events) != 0 {
			t.Fatalf("steps = %d, events = %d; want no persistence for a stopped task", len(store.details.Steps), len(store.events))
		}
	})
}
