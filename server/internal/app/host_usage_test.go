package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

func TestHostUsageCollectStoresRawSampleAndKeepsPriceUnknown(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	value := usageTestHost(host.StateRunning, 1, "sandbox-a")
	store := &hostUsageStoreStub{history: host.UsageHistory{Samples: 2, Observations: []host.UsageObservation{{
		State: host.UsageObservationSampled, Metrics: ptrUsageMetrics(usageTestMetrics(now.Add(-time.Second), 1_250_000_000, 256<<20)),
	}}, Intervals: []host.UsageInterval{{
		State: host.UsageIntervalMeasured, StartedAt: now.Add(-time.Second), EndedAt: now,
		CPUSeconds: 1.25, EstimatedMemoryMiBSeconds: 256,
	}}}}
	sampler := &hostUsageSamplerStub{metrics: usageTestMetrics(now.Add(-time.Second), 1_250_000_000, 256<<20)}
	service := newHostUsageServiceForTest(t, &hostUsageReaderStub{values: []host.Host{value, value}}, store, sampler, now)

	receipt, err := service.Collect(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sampler.calls != 1 || sampler.hostID != value.ID || sampler.generation != value.RuntimeGeneration || sampler.sandboxID != value.SandboxID {
		t.Fatalf("sampler identity = %#v", sampler)
	}
	if store.observation.State != host.UsageObservationSampled || store.observation.Metrics == nil ||
		string(store.observation.Metrics.RawPayload) != string(sampler.metrics.RawPayload) || store.maxGap != HostUsageMaxSampleGap {
		t.Fatalf("stored observation = %#v gap=%s", store.observation, store.maxGap)
	}
	if receipt.Status != host.UsageIntervalMeasured || receipt.UnknownReason != "" || receipt.CPUSeconds != 1.25 ||
		receipt.EstimatedMemoryMiBSeconds != 256 || receipt.Samples != 2 || len(receipt.Observations) != 1 ||
		receipt.Observations[0].Metrics.CPUUsageTotalNS != 1_250_000_000 ||
		receipt.PriceStatus != "unknown" || receipt.PriceUnknownReason != "host_rate_unconfigured" {
		t.Fatalf("usage receipt = %#v", receipt)
	}
}

func TestHostUsageCollectRecordsUnknownOnSourceFailureOrGenerationChange(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		hosts       []host.Host
		sampleError error
		wantReason  host.UsageUnknownReason
	}{
		{name: "source failure", hosts: []host.Host{usageTestHost(host.StateRunning, 1, "sandbox-a")}, sampleError: ErrHostUsageUnavailable, wantReason: host.UsageSamplingFailed},
		{name: "sandbox generation changed", hosts: []host.Host{usageTestHost(host.StateRunning, 1, "sandbox-a"), usageTestHost(host.StateRunning, 2, "sandbox-b")}, wantReason: host.UsageGenerationChange},
		{name: "host stopped", hosts: []host.Host{usageTestHost(host.StateStopped, 1, "")}, wantReason: host.UsageHostNotRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &hostUsageStoreStub{}
			sampler := &hostUsageSamplerStub{metrics: usageTestMetrics(now, 2_000_000_000, 128<<20), err: test.sampleError}
			service := newHostUsageServiceForTest(t, &hostUsageReaderStub{values: test.hosts}, store, sampler, now)
			if _, err := service.Collect(context.Background(), test.hosts[0].ID); err != nil {
				t.Fatal(err)
			}
			if store.observation.State != host.UsageObservationUnknown || store.observation.Reason != test.wantReason {
				t.Fatalf("observation = %#v, want reason %q", store.observation, test.wantReason)
			}
		})
	}
}

func TestHostUsageCollectKeepsStaleRawSampleButMarksItUnknown(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	value := usageTestHost(host.StateRunning, 1, "sandbox-a")
	store := &hostUsageStoreStub{}
	sampler := &hostUsageSamplerStub{metrics: usageTestMetrics(now.Add(-10*time.Second), 3_000_000_000, 500<<20)}
	service := newHostUsageServiceForTest(t, &hostUsageReaderStub{values: []host.Host{value, value}}, store, sampler, now)
	if _, err := service.Collect(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	if store.observation.State != host.UsageObservationUnknown || store.observation.Reason != host.UsageStaleSample ||
		store.observation.Metrics == nil || len(store.observation.Metrics.RawPayload) == 0 {
		t.Fatalf("stale observation lost source sample or reason: %#v", store.observation)
	}
}

func TestHostUsageCollectAllRecordsActiveHostsAndSkipsDeleted(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	values := []host.Host{
		{ID: "h1234567", State: host.StateStopped, RuntimeGeneration: 1},
		{ID: "h7654321", State: host.StateFailed, RuntimeGeneration: 1},
		{ID: "h2345678", State: host.StateDeleted, RuntimeGeneration: 1},
	}
	reader := &hostUsageListReaderStub{values: values}
	store := &hostUsageStoreStub{}
	service := newHostUsageServiceForTest(t, reader, store, &hostUsageSamplerStub{}, now)
	if err := service.CollectAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.observations) != 2 {
		t.Fatalf("observations = %d, want 2", len(store.observations))
	}
	seen := map[string]bool{}
	for _, observation := range store.observations {
		seen[observation.HostID] = observation.State == host.UsageObservationUnknown && observation.Reason == host.UsageHostNotRunning
	}
	if !seen["h1234567"] || !seen["h7654321"] || seen["h2345678"] {
		t.Fatalf("observations = %#v", store.observations)
	}
}

func TestHostUsageProjectReceiptRechecksViewerScopeAndAggregatesOnlyScopedHistory(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	store := &hostUsageStoreStub{projectHistory: host.UsageHistory{Samples: 4, Intervals: []host.UsageInterval{
		{State: host.UsageIntervalMeasured, CPUSeconds: 2, EstimatedMemoryMiBSeconds: 300},
		{State: host.UsageIntervalUnknown, UnknownReason: host.UsageSamplingGap},
	}}}
	access := &hostUsageProjectAccessStub{member: project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1}}
	service, err := NewHostUsageService(&hostUsageReaderStub{values: []host.Host{usageTestHost(host.StateStopped, 1, "")}}, store,
		&hostUsageSamplerStub{}, access, "hostctl", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	receipt, err := service.ProjectReceipt(context.Background(), principal, "j1234567", "h1234567")
	if err != nil || receipt.ProjectID != "j1234567" || receipt.HostID != "h1234567" || receipt.Status != host.UsageIntervalPartial ||
		receipt.Samples != 4 || receipt.UnknownIntervals != 1 || receipt.UnknownReason != host.UsageSamplingGap ||
		receipt.CPUSeconds != 2 || receipt.EstimatedMemoryMiBSeconds != 300 || receipt.PriceStatus != "unknown" ||
		receipt.PriceUnknownReason != "host_rate_unconfigured" || store.projectActorID != principal.UserID ||
		store.projectID != "j1234567" || store.projectHostID != "h1234567" || access.calls != 1 {
		t.Fatalf("project usage receipt = %#v, err=%v, store=%#v access=%#v", receipt, err, store, access)
	}
	access.err = fmt.Errorf("membership revoked")
	if _, err := service.ProjectReceipt(context.Background(), principal, "j1234567", "h1234567"); err == nil || store.projectHistoryCalls != 1 {
		t.Fatalf("revoked project receipt err=%v, history calls=%d", err, store.projectHistoryCalls)
	}
}

func newHostUsageServiceForTest(t *testing.T, reader interface {
	GetHost(context.Context, string) (host.Host, error)
},
	store HostUsageStore, sampler HostUsageSampler, now time.Time) *HostUsageService {
	t.Helper()
	service, err := NewHostUsageService(reader, store, sampler, &hostUsageProjectAccessStub{
		member: project.Member{ProjectID: "j1234567", UserID: "u1234567", Role: project.RoleViewer, Version: 1},
	}, "hostctl", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func usageTestHost(state host.State, generation int64, sandboxID string) host.Host {
	return host.Host{ID: "h1234567", RuntimeProfile: "container", RuntimeGeneration: generation, SandboxID: sandboxID, State: state}
}

func usageTestMetrics(at time.Time, cpuTotalNS, memoryBytes int64) host.ResourceMetrics {
	return host.ResourceMetrics{SampledAt: at, ContainerID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CPUUsageTotalNS: cpuTotalNS, MemoryUsageBytes: memoryBytes,
		RawPayload: []byte(fmt.Sprintf(`{"cpu_usage_total_ns":%d,"memory_usage_bytes":%d}`, cpuTotalNS, memoryBytes))}
}

func ptrUsageMetrics(value host.ResourceMetrics) *host.ResourceMetrics { return &value }

type hostUsageReaderStub struct {
	values []host.Host
	calls  int
}

type hostUsageListReaderStub struct {
	values []host.Host
}

func (s *hostUsageListReaderStub) ListHosts(context.Context) ([]host.Host, error) {
	return append([]host.Host(nil), s.values...), nil
}

func (s *hostUsageListReaderStub) GetHost(_ context.Context, hostID string) (host.Host, error) {
	for _, value := range s.values {
		if value.ID == hostID {
			return value, nil
		}
	}
	return host.Host{}, fmt.Errorf("host %s not found", hostID)
}

func (s *hostUsageReaderStub) GetHost(_ context.Context, _ string) (host.Host, error) {
	index := s.calls
	s.calls++
	if index >= len(s.values) {
		index = len(s.values) - 1
	}
	return s.values[index], nil
}

type hostUsageSamplerStub struct {
	metrics    host.ResourceMetrics
	err        error
	calls      int
	profile    string
	instance   string
	hostID     string
	generation int64
	sandboxID  string
}

func (s *hostUsageSamplerStub) SampleHostUsage(_ context.Context, profile, instance, hostID string, generation int64, sandboxID string) (host.ResourceMetrics, error) {
	s.calls++
	s.profile, s.instance, s.hostID, s.generation, s.sandboxID = profile, instance, hostID, generation, sandboxID
	return s.metrics, s.err
}

type hostUsageStoreStub struct {
	mu                  sync.Mutex
	observation         host.UsageObservation
	observations        []host.UsageObservation
	maxGap              time.Duration
	interval            host.UsageInterval
	history             host.UsageHistory
	projectHistory      host.UsageHistory
	projectHistoryCalls int
	projectActorID      string
	projectID           string
	projectHostID       string
}

func (s *hostUsageStoreStub) RecordHostUsageObservation(_ context.Context, observation host.UsageObservation, maxGap time.Duration) (host.UsageInterval, error) {
	if err := observation.Validate(); err != nil {
		return host.UsageInterval{}, err
	}
	s.mu.Lock()
	s.observation, s.maxGap = observation, maxGap
	s.observations = append(s.observations, observation)
	s.mu.Unlock()
	return s.interval, nil
}

func (s *hostUsageStoreStub) GetHostUsageHistory(context.Context, string, int64) (host.UsageHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.history.Samples == 0 && s.observation.HostID != "" {
		s.history.Samples = 1
	}
	return s.history, nil
}

func (s *hostUsageStoreStub) GetProjectHostUsageHistory(_ context.Context, actorID, projectID, hostID string) (host.UsageHistory, error) {
	s.projectHistoryCalls++
	s.projectActorID, s.projectID, s.projectHostID = actorID, projectID, hostID
	return s.projectHistory, nil
}

type hostUsageProjectAccessStub struct {
	member    project.Member
	err       error
	calls     int
	principal identity.Principal
	projectID string
	minimum   project.Role
}

func (s *hostUsageProjectAccessStub) RequireAccess(_ context.Context, principal identity.Principal, projectID string, minimum project.Role) (project.Member, error) {
	s.calls++
	s.principal, s.projectID, s.minimum = principal, projectID, minimum
	return s.member, s.err
}
