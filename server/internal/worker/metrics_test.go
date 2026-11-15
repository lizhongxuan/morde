package worker

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"morde/server/internal/domain/task"
)

func TestTaskClaimWaitMetricUsesBoundedPriorityAndQueueTimestamp(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := newTaskClaimMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	metrics.observeClaim(task.Task{QueuePriority: task.QueuePriorityPinned, QueueEnteredAt: now.Add(-12 * time.Second)}, now)
	metrics.observeClaim(task.Task{QueuePriority: task.QueuePriorityPinned}, now)
	metrics.observeClaim(task.Task{QueuePriority: "untrusted", QueueEnteredAt: now.Add(-24 * time.Second)}, now)
	metrics.observeClaim(task.Task{QueuePriority: task.QueuePriorityBackground, QueueEnteredAt: now.Add(time.Second)}, now)

	if got := gatheredWorkerMetric(t, registry, "cloudhost_task_claim_wait_seconds", "queue_priority", "pinned").GetHistogram(); got.GetSampleCount() != 1 || got.GetSampleSum() != 12 {
		t.Fatalf("pinned queue wait = %#v, want one 12s sample", got)
	}
	if got := gatheredWorkerMetric(t, registry, "cloudhost_task_claim_wait_seconds", "queue_priority", "normal").GetHistogram(); got.GetSampleCount() != 1 || got.GetSampleSum() != 24 {
		t.Fatalf("invalid priority fallback = %#v, want one 24s sample", got)
	}
}

func gatheredWorkerMetric(t *testing.T, registry prometheus.Gatherer, name string, labels ...string) *dto.Metric {
	t.Helper()
	want := make(map[string]string, len(labels)/2)
	for index := 0; index+1 < len(labels); index += 2 {
		want[labels[index]] = labels[index+1]
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			got := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				got[label.GetName()] = label.GetValue()
			}
			match := len(got) == len(want)
			for key, value := range want {
				match = match && got[key] == value
			}
			if match {
				return metric
			}
		}
	}
	t.Fatalf("metric %s with labels %v was not gathered", name, want)
	return nil
}
