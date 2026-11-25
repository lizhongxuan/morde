package app

import (
	"context"
	"strings"
	"testing"
)

func TestPauseModelRecoveryAfterLadderExhaustion(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecoveryAfterLadderExhaustion(context.Background(), claimed, FailoverReasonTimeout); err != nil {
		t.Fatalf("PauseModelRecoveryAfterLadderExhaustion() returned unexpected error: %v", err)
	}
	if store.details.Task.WaitingReason != "model_retry_ladder_exhausted" {
		t.Fatalf("task waiting reason = %q, want model_retry_ladder_exhausted", store.details.Task.WaitingReason)
	}
	if len(store.committedTaskProgress) != 1 {
		t.Fatalf("committed progress entries = %d, want 1", len(store.committedTaskProgress))
	}
	progress := store.committedTaskProgress[0]
	if len(progress.Steps) != 1 {
		t.Fatalf("committed steps = %d, want 1", len(progress.Steps))
	}
	summary := progress.Steps[0].Summary
	if !strings.Contains(summary, "网络超时") || !strings.Contains(summary, "3") {
		t.Fatalf("terminal step summary = %q, want it to name the cause and the retry count", summary)
	}
	for _, banned := range []string{"stack", "goroutine", "panic", "Error:", ".go:"} {
		if strings.Contains(summary, banned) {
			t.Fatalf("terminal step summary = %q, must not contain %q", summary, banned)
		}
	}
	// The exhaustion record is durable: the event metadata carries the
	// category so exhaustions can be counted later.
	event := progress.Event
	if !strings.Contains(string(event.Payload), "route_retry_ladder_exhausted") {
		t.Fatalf("committed event payload = %s, want it to carry the exhaustion category", event.Payload)
	}
}

func TestPauseModelRecoveryAfterLadderExhaustionTerminalReason(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A terminal reason never retries: the message must not claim retries
	// were spent.
	if err := broker.PauseModelRecoveryAfterLadderExhaustion(context.Background(), claimed, FailoverReasonAuthenticationRejected); err != nil {
		t.Fatalf("PauseModelRecoveryAfterLadderExhaustion() returned unexpected error: %v", err)
	}
	summary := store.committedTaskProgress[0].Steps[0].Summary
	if strings.Contains(summary, "已重试") {
		t.Fatalf("terminal step summary = %q, must not claim retries for a non-retryable failure", summary)
	}
	if !strings.Contains(summary, "模型认证失败") {
		t.Fatalf("terminal step summary = %q, want it to name the cause", summary)
	}
}
