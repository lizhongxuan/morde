package app

import (
	"context"
	"errors"
	"sort"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

type workerCleanupIdentity struct {
	instance   string
	hostID     string
	profile    string
	sandboxID  string
	generation int64
}

type workerTestOrphanCleanupStore struct {
	records     map[workerCleanupIdentity]bool
	ensureErr   error
	listErr     error
	completeErr error
}

func (s *workerTestOrphanCleanupStore) EnsureOrphanHostVolumeCleanup(_ context.Context, instanceID, hostID, profile, sandboxID string, generation int64, active bool) (bool, error) {
	if s.ensureErr != nil {
		return false, s.ensureErr
	}
	key := workerCleanupIdentity{instance: instanceID, hostID: hostID, profile: profile, sandboxID: sandboxID, generation: generation}
	pending, exists := s.records[key]
	if !exists || active {
		pending = true
		s.records[key] = pending
	}
	return pending, nil
}

func (s *workerTestOrphanCleanupStore) ListOrphanHostVolumeCleanups(_ context.Context, instanceID string) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	set := make(map[string]struct{})
	for key, pending := range s.records {
		if key.instance == instanceID && pending {
			set[key.hostID] = struct{}{}
		}
	}
	values := make([]string, 0, len(set))
	for hostID := range set {
		values = append(values, hostID)
	}
	sort.Strings(values)
	return values, nil
}

func (s *workerTestOrphanCleanupStore) CompleteOrphanHostVolumeCleanup(_ context.Context, instanceID, hostID string) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	for key := range s.records {
		if key.instance == instanceID && key.hostID == hostID {
			s.records[key] = false
		}
	}
	return nil
}

func TestWorkerAppRetriesOrphanVolumesAfterSandboxIsGone(t *testing.T) {
	service, deps := newHostServiceTest(t)
	hostID := "h7654321"
	deps.sandbox.add(Sandbox{ID: "orphan-before-volume-retry", State: SandboxRunning, Metadata: hostServiceMetadata(hostID)})
	volume := service.volumes.(*hostServiceTestVolume)
	volume.errByName = map[string]error{domain.HostAppsVolumeName(hostID): errors.New("volume daemon unavailable")}
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want application volume failure")
	}
	if deps.sandbox.values["orphan-before-volume-retry"].State != SandboxTerminated {
		t.Fatalf("orphan sandbox state = %v, want terminated", deps.sandbox.values["orphan-before-volume-retry"].State)
	}
	cleanupStore := worker.cleanups.(*workerTestOrphanCleanupStore)
	if pending, err := cleanupStore.ListOrphanHostVolumeCleanups(context.Background(), service.instance); err != nil || len(pending) != 1 || pending[0] != hostID {
		t.Fatalf("pending cleanup = %v, %v; want %s", pending, err, hostID)
	}

	delete(deps.sandbox.values, "orphan-before-volume-retry")
	volume.errByName[domain.HostAppsVolumeName(hostID)] = nil
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatalf("retry after sandbox disappeared: %v", err)
	}
	if pending, err := cleanupStore.ListOrphanHostVolumeCleanups(context.Background(), service.instance); err != nil || len(pending) != 0 {
		t.Fatalf("pending cleanup after retry = %v, %v; want empty", pending, err)
	}
	if len(volume.names) != 6 || volume.names[3] != domain.RuntimeVolumeName(hostID) ||
		volume.names[4] != domain.ProfileVolumeName(hostID) || volume.names[5] != domain.HostAppsVolumeName(hostID) {
		t.Fatalf("volume deletion retries = %v", volume.names)
	}
}

func TestWorkerAppRequiresCleanupRecordBeforeDeletingOrphanSandbox(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.sandbox.add(Sandbox{ID: "orphan-no-cleanup-record", State: SandboxRunning, Metadata: hostServiceMetadata("h7654321")})
	worker := newWorkerAppTest(t, service)
	worker.cleanups.(*workerTestOrphanCleanupStore).ensureErr = errors.New("database unavailable")

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want durable cleanup record failure")
	}
	if deps.sandbox.values["orphan-no-cleanup-record"].State != SandboxRunning {
		t.Fatal("orphan sandbox was deleted without a durable cleanup record")
	}
	if len(service.volumes.(*hostServiceTestVolume).names) != 0 {
		t.Fatal("orphan volumes were touched without a durable cleanup record")
	}
}

func TestWorkerAppDoesNotRepeatCompletedOrphanCleanup(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.sandbox.add(Sandbox{ID: "orphan-complete-once", State: SandboxRunning, Metadata: hostServiceMetadata("h7654321")})
	worker := newWorkerAppTest(t, service)
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	if len(volume.names) != 3 {
		t.Fatalf("initial volume cleanup = %v", volume.names)
	}
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(volume.names) != 3 {
		t.Fatalf("completed volume cleanup repeated for retained terminated sandbox: %v", volume.names)
	}
}

func TestWorkerAppChecksGenerationForSoftDeletedHostOrphans(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleted)
	deps.hosts.addHost(value)
	metadata := hostServiceMetadata(value.ID)
	metadata["ch-generation"] = "2"
	deps.sandbox.add(Sandbox{ID: "stale-generation-orphan", State: SandboxRunning, Metadata: metadata})
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want stale generation rejection")
	}
	if deps.sandbox.values["stale-generation-orphan"].State != SandboxRunning {
		t.Fatal("orphan sandbox with mismatched generation was deleted")
	}
	if pending, err := worker.cleanups.(*workerTestOrphanCleanupStore).ListOrphanHostVolumeCleanups(context.Background(), service.instance); err != nil || len(pending) != 0 {
		t.Fatalf("pending cleanup for stale generation = %v, %v", pending, err)
	}
}

func TestWorkerAppPendingOrphanCleanupWaitsForActiveHostOrSandbox(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	worker := newWorkerAppTest(t, service)
	cleanupStore := worker.cleanups.(*workerTestOrphanCleanupStore)
	cleanupStore.records[workerCleanupIdentity{instance: service.instance, hostID: value.ID, profile: "test", sandboxID: "old", generation: 1}] = true

	if err := worker.reconcileOrphanHostVolumeCleanups(context.Background(), map[string][]remoteSandbox{
		value.ID: {{value: Sandbox{ID: value.SandboxID, State: SandboxRunning}}},
	}, map[string]host.Host{value.ID: value}); err != nil {
		t.Fatal(err)
	}
	if len(service.volumes.(*hostServiceTestVolume).names) != 0 {
		t.Fatal("pending orphan cleanup deleted volumes while Host/sandbox was active")
	}
}
