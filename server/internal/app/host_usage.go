package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

const HostUsageMaxSampleGap = 5 * time.Second

var (
	ErrHostUsageGenerationChanged = errors.New("host usage generation changed")
	ErrHostUsageUnavailable       = errors.New("host usage source unavailable")
)

type HostUsageStore interface {
	RecordHostUsageObservation(context.Context, host.UsageObservation, time.Duration) (host.UsageInterval, error)
	GetHostUsageHistory(context.Context, string, int64) (host.UsageHistory, error)
	GetProjectHostUsageHistory(context.Context, string, string, string) (host.UsageHistory, error)
}

type HostUsageSampler interface {
	SampleHostUsage(context.Context, string, string, string, int64, string) (host.ResourceMetrics, error)
}

type HostUsageService struct {
	hosts interface {
		GetHost(context.Context, string) (host.Host, error)
	}
	hostLister interface {
		ListHosts(context.Context) ([]host.Host, error)
	}
	store         HostUsageStore
	sampler       HostUsageSampler
	projectAccess ProjectAccessChecker
	instance      string
	now           func() time.Time
}

type ProjectHostUsageReceipt struct {
	ProjectID                 string
	HostID                    string
	Status                    host.UsageIntervalState
	Samples                   int64
	UnknownIntervals          int64
	UnknownReason             host.UsageUnknownReason
	CalculationMethod         string
	CPUSeconds                float64
	EstimatedMemoryMiBSeconds float64
	PriceStatus               string
	PriceUnknownReason        string
}

func NewHostUsageService(hosts interface {
	GetHost(context.Context, string) (host.Host, error)
}, store HostUsageStore,
	sampler HostUsageSampler, projectAccess ProjectAccessChecker, instance string, now func() time.Time) (*HostUsageService, error) {
	if hosts == nil || store == nil || sampler == nil || projectAccess == nil || instance == "" {
		return nil, errors.New("host usage dependencies and instance are required")
	}
	if now == nil {
		now = time.Now
	}
	service := &HostUsageService{hosts: hosts, store: store, sampler: sampler, projectAccess: projectAccess, instance: instance, now: now}
	if lister, ok := hosts.(interface {
		ListHosts(context.Context) ([]host.Host, error)
	}); ok {
		service.hostLister = lister
	}
	return service, nil
}

// CollectAll stores one observation for each non-deleted Host. Sampling stays
// bounded so a single slow Docker endpoint cannot prevent other Hosts from
// receiving an observation.
func (s *HostUsageService) CollectAll(ctx context.Context) error {
	if s == nil || ctx == nil || s.hostLister == nil {
		return ErrHostUsageUnavailable
	}
	values, err := s.hostLister.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("list Hosts for usage sampling: %w", err)
	}
	const concurrency = 4
	semaphore := make(chan struct{}, concurrency)
	var wait sync.WaitGroup
	var errorsMu sync.Mutex
	var failures []error
	for _, value := range values {
		if value.State == host.StateDeleted || value.DeletedAt != nil {
			continue
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wait.Add(1)
		go func(hostID string) {
			defer wait.Done()
			defer func() { <-semaphore }()
			if _, collectErr := s.Collect(ctx, hostID); collectErr != nil {
				errorsMu.Lock()
				failures = append(failures, fmt.Errorf("collect Host %s usage: %w", hostID, collectErr))
				errorsMu.Unlock()
			}
		}(value.ID)
	}
	wait.Wait()
	return errors.Join(append(failures, ctx.Err())...)
}

// Collect stores one source snapshot and returns its generation-scoped receipt.
func (s *HostUsageService) Collect(ctx context.Context, hostID string) (host.UsageReceipt, error) {
	if s == nil || s.hosts == nil || s.store == nil || s.sampler == nil || !domain.ValidHostID(hostID) {
		return host.UsageReceipt{}, domain.InvalidArgument("host_id", "Host usage request is invalid")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return host.UsageReceipt{}, err
	}
	if value.RuntimeGeneration < 1 {
		return host.UsageReceipt{}, domain.NewError(domain.CodeHostNotReady, "Host usage generation is unavailable")
	}
	observedAt := s.now().UTC()
	observation := host.UsageObservation{
		HostID: hostID, RuntimeGeneration: value.RuntimeGeneration, SandboxID: value.SandboxID,
		ObservedAt: observedAt, State: host.UsageObservationUnknown, Reason: host.UsageHostNotRunning,
	}
	if value.State == host.StateRunning && value.SandboxID != "" {
		metrics, sampleErr := s.sampler.SampleHostUsage(ctx, value.RuntimeProfile, s.instance, value.ID, value.RuntimeGeneration, value.SandboxID)
		if sampleErr == nil && metrics.Validate() == nil {
			observation.State, observation.Reason, observation.Metrics = host.UsageObservationSampled, "", &metrics
			delta := observedAt.Sub(metrics.SampledAt)
			if delta > HostUsageMaxSampleGap || delta < -HostUsageMaxSampleGap {
				observation.State, observation.Reason = host.UsageObservationUnknown, host.UsageStaleSample
			}
			current, currentErr := s.hosts.GetHost(ctx, hostID)
			if currentErr != nil {
				return host.UsageReceipt{}, currentErr
			}
			if current.RuntimeGeneration != value.RuntimeGeneration || current.SandboxID != value.SandboxID || current.State != host.StateRunning {
				observation.State, observation.Reason = host.UsageObservationUnknown, host.UsageGenerationChange
			}
		} else if errors.Is(sampleErr, ErrHostUsageGenerationChanged) {
			observation.Reason = host.UsageGenerationChange
		} else {
			observation.Reason = host.UsageSamplingFailed
		}
	}
	if _, err := s.store.RecordHostUsageObservation(ctx, observation, HostUsageMaxSampleGap); err != nil {
		return host.UsageReceipt{}, err
	}
	return s.Receipt(ctx, hostID, value.RuntimeGeneration)
}

func (s *HostUsageService) Receipt(ctx context.Context, hostID string, generation int64) (host.UsageReceipt, error) {
	if s == nil || s.store == nil || !domain.ValidHostID(hostID) || generation < 1 {
		return host.UsageReceipt{}, domain.InvalidArgument("host_usage", "Host usage receipt request is invalid")
	}
	history, err := s.store.GetHostUsageHistory(ctx, hostID, generation)
	if err != nil {
		return host.UsageReceipt{}, err
	}
	receipt := host.UsageReceipt{
		HostID: hostID, RuntimeGeneration: generation, Status: host.UsageIntervalUnknown,
		Samples: history.Samples, CalculationMethod: host.UsageCalculationMethod,
		PriceStatus: "unknown", PriceUnknownReason: "host_rate_unconfigured",
		Observations: append([]host.UsageObservation(nil), history.Observations...),
		Intervals:    append([]host.UsageInterval(nil), history.Intervals...),
	}
	if len(history.Intervals) == 0 {
		receipt.UnknownReason = host.UsageNoPreviousSample
		return receipt, nil
	}
	measuredIntervals := int64(0)
	for _, interval := range history.Intervals {
		if interval.State == host.UsageIntervalMeasured && host.UsageMeasured(interval.CPUSeconds, interval.EstimatedMemoryMiBSeconds) {
			measuredIntervals++
			receipt.CPUSeconds += interval.CPUSeconds
			receipt.EstimatedMemoryMiBSeconds += interval.EstimatedMemoryMiBSeconds
			continue
		}
		receipt.UnknownIntervals++
		if interval.UnknownReason != "" {
			receipt.UnknownReason = interval.UnknownReason
		}
	}
	if measuredIntervals == int64(len(history.Intervals)) {
		receipt.Status = host.UsageIntervalMeasured
	} else if measuredIntervals > 0 {
		receipt.Status = host.UsageIntervalPartial
	} else if receipt.UnknownReason == "" {
		receipt.UnknownReason = host.UsageSamplingFailed
	}
	return receipt, nil
}

// ProjectReceipt returns only intervals attributed to a project after checking
// the caller's current viewer membership. The store repeats that check in the
// same transaction as the read so a concurrent revocation cannot race access.
func (s *HostUsageService) ProjectReceipt(ctx context.Context, principal identity.Principal, projectID, hostID string) (ProjectHostUsageReceipt, error) {
	if s == nil || s.store == nil || s.projectAccess == nil || ctx == nil || !project.ValidID(projectID) || !domain.ValidHostID(hostID) {
		return ProjectHostUsageReceipt{}, domain.InvalidArgument("host_usage", "project usage receipt request is invalid")
	}
	if _, err := s.projectAccess.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectHostUsageReceipt{}, err
	}
	history, err := s.store.GetProjectHostUsageHistory(ctx, principal.UserID, projectID, hostID)
	if err != nil {
		return ProjectHostUsageReceipt{}, err
	}
	receipt := ProjectHostUsageReceipt{
		ProjectID: projectID, HostID: hostID, Status: host.UsageIntervalUnknown,
		Samples: history.Samples, CalculationMethod: host.UsageCalculationMethod,
		PriceStatus: "unknown", PriceUnknownReason: "host_rate_unconfigured",
	}
	if len(history.Intervals) == 0 {
		receipt.UnknownReason = host.UsageNoPreviousSample
		return receipt, nil
	}
	measuredIntervals := int64(0)
	for _, interval := range history.Intervals {
		if interval.State == host.UsageIntervalMeasured && host.UsageMeasured(interval.CPUSeconds, interval.EstimatedMemoryMiBSeconds) {
			measuredIntervals++
			receipt.CPUSeconds += interval.CPUSeconds
			receipt.EstimatedMemoryMiBSeconds += interval.EstimatedMemoryMiBSeconds
			continue
		}
		receipt.UnknownIntervals++
		if interval.UnknownReason != "" {
			receipt.UnknownReason = interval.UnknownReason
		}
	}
	if measuredIntervals == int64(len(history.Intervals)) {
		receipt.Status = host.UsageIntervalMeasured
	} else if measuredIntervals > 0 {
		receipt.Status = host.UsageIntervalPartial
	} else if receipt.UnknownReason == "" {
		receipt.UnknownReason = host.UsageSamplingFailed
	}
	return receipt, nil
}
