package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/domain/host"
)

func TestCoverageGuardBranches(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New() accepted empty options")
	}

	queue := NewCreateQueue()
	if err := queue.Notify(context.Background(), ""); err == nil {
		t.Fatal("Notify() accepted an empty host ID")
	}
	for i := 0; i < createQueueCapacity; i++ {
		queue.jobs <- fmt.Sprintf("queued-%d", i)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := queue.Notify(canceled, "h1234567"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Notify() error = %v, want context.Canceled", err)
	}
	queue.mu.Lock()
	_, pending := queue.pending["h1234567"]
	queue.mu.Unlock()
	if pending {
		t.Fatal("Notify() kept a canceled host in the pending set")
	}

	for _, state := range []host.State{
		host.StateCreating, host.StateRunning, host.StateStopping, host.StateStopped,
		host.StateFailed, host.StateDeleting, host.StateDeleted,
	} {
		if !validHostState(state) {
			t.Errorf("validHostState(%q) = false", state)
		}
	}
	if validHostState("unknown") {
		t.Fatal("validHostState accepted an unknown state")
	}

	worker, registry := testWorker(t, &applicationStub{}, NewCreateQueue())
	worker.recordHostCounts(map[host.State]int{
		host.StateRunning:     -1,
		host.StateStopping:    2,
		host.StateStopped:     1,
		host.State("unknown"): 1,
	})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	gotStates := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "cloudhost_hosts" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "state" {
					gotStates[label.GetValue()] = metric.GetGauge().GetValue()
				}
			}
		}
	}
	if gotStates["stopping"] != 2 || gotStates["stopped"] != 1 {
		t.Fatalf("host state gauges = %#v", gotStates)
	}
}

func TestCoverageMetricRegistrationRejectsDuplicates(t *testing.T) {
	registry := prometheus.NewRegistry()
	if _, err := newMetrics(registry); err != nil {
		t.Fatalf("first newMetrics() error = %v", err)
	}
	if _, err := newMetrics(registry); err == nil {
		t.Fatal("newMetrics() accepted duplicate collectors")
	}
}
