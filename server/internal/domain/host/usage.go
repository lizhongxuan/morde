package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"time"
)

type UsageObservationState string

const (
	UsageObservationSampled UsageObservationState = "sampled"
	UsageObservationUnknown UsageObservationState = "unknown"
)

type UsageUnknownReason string

const (
	UsageNoPreviousSample  UsageUnknownReason = "no_previous_sample"
	UsageSamplingFailed    UsageUnknownReason = "sampling_failed"
	UsageStaleSample       UsageUnknownReason = "stale_sample"
	UsageSamplingGap       UsageUnknownReason = "sampling_gap"
	UsageGenerationChange  UsageUnknownReason = "generation_changed"
	UsageHostNotRunning    UsageUnknownReason = "host_not_running"
	UsageCounterReset      UsageUnknownReason = "counter_reset"
	UsageContainerReplaced UsageUnknownReason = "container_replaced"
)

type UsageIntervalState string

const (
	UsageIntervalUnknown  UsageIntervalState = "unknown"
	UsageIntervalMeasured UsageIntervalState = "measured"
	UsageIntervalPartial  UsageIntervalState = "partial"
)

type UsageAttributionState string

const (
	UsageAttributionUnknown UsageAttributionState = "unknown"
	UsageAttributionProject UsageAttributionState = "project"
)

type UsageAttributionReason string

const (
	UsageAttributionNoActiveBinding   UsageAttributionReason = "no_active_workspace_binding"
	UsageAttributionMultipleBindings  UsageAttributionReason = "multiple_active_workspace_bindings"
	UsageAttributionNoProject         UsageAttributionReason = "no_active_project_resource"
	UsageAttributionMultipleProjects  UsageAttributionReason = "multiple_active_project_resources"
	UsageAttributionGenerationChange  UsageAttributionReason = "generation_changed"
	UsageAttributionAssociationChange UsageAttributionReason = "association_changed"
)

const UsageCalculationMethod = "docker_cgroup_cpu_counter_diff_and_memory_trapezoid_v1"

// ResourceMetrics is a cgroup-scoped Docker Engine snapshot. CPU is a
// cumulative nanosecond counter; memory is a point-in-time byte value.
type ResourceMetrics struct {
	SampledAt        time.Time
	ContainerID      string `json:"-"`
	CPUUsageTotalNS  int64
	MemoryUsageBytes int64
	RawPayload       []byte
}

func (m ResourceMetrics) Validate() error {
	if m.SampledAt.IsZero() || m.CPUUsageTotalNS < 0 || m.MemoryUsageBytes < 0 ||
		!validUsageContainerID(m.ContainerID) || len(m.RawPayload) == 0 || len(m.RawPayload) > 1024 {
		return errors.New("host resource metrics are invalid")
	}
	canonical, err := json.Marshal(struct {
		CPUUsageTotalNS  int64 `json:"cpu_usage_total_ns"`
		MemoryUsageBytes int64 `json:"memory_usage_bytes"`
	}{m.CPUUsageTotalNS, m.MemoryUsageBytes})
	if err != nil || !bytes.Equal(m.RawPayload, canonical) {
		return errors.New("host resource metrics payload does not match sample")
	}
	return nil
}

func validUsageContainerID(value string) bool {
	if len(value) < 12 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}

type UsageAttribution struct {
	State                   UsageAttributionState
	Reason                  UsageAttributionReason
	OwnerID                 string
	WorkspaceID             string
	WorkspaceBindingID      string
	WorkspaceBindingVersion int64
	ProjectID               string
	ProjectResourceVersion  int64
	ActorID                 *string `json:"actor_id"`
}

type UsageObservation struct {
	ID                int64
	HostID            string
	RuntimeGeneration int64
	SandboxID         string
	ObservedAt        time.Time
	State             UsageObservationState
	Reason            UsageUnknownReason
	Metrics           *ResourceMetrics
	Attribution       UsageAttribution
}

func (o UsageObservation) Validate() error {
	if o.HostID == "" || o.RuntimeGeneration < 1 || o.ObservedAt.IsZero() ||
		o.State == UsageObservationSampled && o.SandboxID == "" {
		return errors.New("host usage observation identity is invalid")
	}
	switch o.State {
	case UsageObservationSampled:
		if o.Reason != "" || o.Metrics == nil || o.Metrics.Validate() != nil {
			return errors.New("sampled host usage observation is invalid")
		}
	case UsageObservationUnknown:
		if o.Reason == "" || o.Metrics != nil && o.Metrics.Validate() != nil {
			return errors.New("unknown host usage observation is invalid")
		}
	default:
		return errors.New("host usage observation state is invalid")
	}
	return nil
}

type UsageInterval struct {
	ID                        int64
	HostID                    string
	RuntimeGeneration         int64
	SandboxID                 string
	StartObservationID        *int64
	EndObservationID          int64
	StartedAt                 time.Time
	EndedAt                   time.Time
	State                     UsageIntervalState
	UnknownReason             UsageUnknownReason
	CalculationMethod         string
	Attribution               UsageAttribution
	CPUSeconds                float64
	EstimatedMemoryMiBSeconds float64
}

type UsageHistory struct {
	Samples      int64
	Observations []UsageObservation
	Intervals    []UsageInterval
}

type UsageReceipt struct {
	HostID                    string
	RuntimeGeneration         int64
	Status                    UsageIntervalState
	Samples                   int64
	UnknownIntervals          int64
	UnknownReason             UsageUnknownReason
	CalculationMethod         string
	CPUSeconds                float64
	EstimatedMemoryMiBSeconds float64
	PriceStatus               string
	PriceUnknownReason        string
	Observations              []UsageObservation
	Intervals                 []UsageInterval
}

// UsageMeasured validates the computed values before they are persisted or
// included in a receipt. Memory is an estimate from sparse snapshots.
func UsageMeasured(cpuSeconds, memoryMiBSeconds float64) bool {
	return !math.IsNaN(cpuSeconds) && !math.IsInf(cpuSeconds, 0) && cpuSeconds >= 0 &&
		!math.IsNaN(memoryMiBSeconds) && !math.IsInf(memoryMiBSeconds, 0) && memoryMiBSeconds >= 0
}
