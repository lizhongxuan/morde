package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/template"
)

func TestTemplateAdapterConvertsResourceUnits(t *testing.T) {
	reader := templateAdapter{values: map[string]template.Template{
		"std": {
			ID: "std", Name: "Standard", Isolation: "container",
			Resources: template.Resources{
				Requests: template.Resource{CPU: "2", Memory: "4Gi"},
				Limits:   template.Resource{CPU: "3", Memory: "6Gi"},
			},
			Workspace: template.Workspace{Size: "10Gi"},
			Lifecycle: template.Lifecycle{TTL: 2 * time.Hour, IdleTimeout: 10 * time.Minute, OnIdle: template.OnIdleDisabled},
		},
	}}
	items, err := reader.ListTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Resources.CPUMilli != 2000 || items[0].Resources.MemMiB != 4096 ||
		items[0].Resources.CPULimitMilli != 3000 || items[0].Resources.MemLimitMiB != 6144 ||
		items[0].WorkspaceSizeMiB != 10240 || items[0].TTLSeconds != 7200 || items[0].IdleTimeoutSeconds != 600 {
		t.Fatalf("template API record = %+v", items)
	}
}

func TestCountedAuditWriterCountsFailures(t *testing.T) {
	registry := prometheus.NewRegistry()
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "portal_test_audit_failures_total", Help: "test"})
	if err := registry.Register(failures); err != nil {
		t.Fatal(err)
	}
	writer := countedAuditWriter{writer: failingAuditWriter{}, failures: failures}
	if err := writer.Write(context.Background(), audit.Entry{}); err == nil {
		t.Fatal("Write() error = nil")
	}
	if got := testCounterValue(t, registry, "portal_test_audit_failures_total"); got != 1 {
		t.Fatalf("audit failure metric = %v, want 1", got)
	}
}

type failingAuditWriter struct{}

func (failingAuditWriter) Write(context.Context, audit.Entry) error {
	return errors.New("store unavailable")
}

func testCounterValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.GetName() == name {
			return metric.Metric[0].GetCounter().GetValue()
		}
	}
	return 0
}

var _ app.AuditWriter = failingAuditWriter{}
