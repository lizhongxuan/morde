package app

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

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
func TestCloudDSHModelRecoveryReportsUnconfirmedUsageWithoutRetryPromise(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecovery(context.Background(), store.details.Task, "usage_unknown"); err != nil {
		t.Fatal(err)
	}
	if store.details.Task.Status != task.StatusWaitingUser || len(store.details.Steps) != 1 {
		t.Fatalf("task did not pause: task=%+v steps=%+v", store.details.Task, store.details.Steps)
	}
	step := store.details.Steps[0]
	if step.Retryable || !strings.Contains(step.Summary, "尚未确认") || !strings.Contains(step.Summary, "核对") {
		t.Fatalf("model usage was presented as safely retryable: %+v", step)
	}
}
