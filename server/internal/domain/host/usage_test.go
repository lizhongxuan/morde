package host

import (
	"testing"
	"time"
)

func TestResourceMetricsValidateCgroupSnapshot(t *testing.T) {
	base := ResourceMetrics{
		SampledAt:       time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		ContainerID:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CPUUsageTotalNS: 1_000_000_000, MemoryUsageBytes: 512 << 20,
		RawPayload: []byte(`{"cpu_usage_total_ns":1000000000,"memory_usage_bytes":536870912}`),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid metrics rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ResourceMetrics){
		"missing time":         func(value *ResourceMetrics) { value.SampledAt = time.Time{} },
		"negative cpu counter": func(value *ResourceMetrics) { value.CPUUsageTotalNS = -1 },
		"negative memory":      func(value *ResourceMetrics) { value.MemoryUsageBytes = -1 },
		"non-telemetry payload field": func(value *ResourceMetrics) {
			value.RawPayload = []byte(`{"cpu_usage_total_ns":1000000000,"memory_usage_bytes":536870912,"status":"measured"}`)
		},
		"null numeric field": func(value *ResourceMetrics) {
			value.RawPayload = []byte(`{"cpu_usage_total_ns":null,"memory_usage_bytes":536870912}`)
		},
		"payload value mismatch":   func(value *ResourceMetrics) { value.CPUUsageTotalNS++ },
		"invalid original payload": func(value *ResourceMetrics) { value.RawPayload = []byte("not json") },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid metrics accepted")
			}
		})
	}
}

func TestUsageObservationRequiresSafeUnknownReason(t *testing.T) {
	value := UsageObservation{HostID: "h1234567", RuntimeGeneration: 1, ObservedAt: time.Now(), State: UsageObservationUnknown}
	if err := value.Validate(); err == nil {
		t.Fatal("unknown observation without reason accepted")
	}
	value.Reason = UsageSamplingFailed
	if err := value.Validate(); err != nil {
		t.Fatalf("unknown observation rejected: %v", err)
	}
}
