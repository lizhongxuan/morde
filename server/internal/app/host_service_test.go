package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
)

func TestHostServiceCreatePersistsEncryptedKeyAndNotifiesWorker(t *testing.T) {
	service, deps := newHostServiceTest(t)
	var notified string
	service.notifyCreate = func(_ context.Context, hostID string) error {
		notified = hostID
		return nil
	}

	created, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{Name: "demo"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.State != host.StateCreating || created.Name != "demo" || len(created.Workspaces) != 0 || notified != created.ID {
		t.Fatalf("created host = %#v, notified=%q", created, notified)
	}
	key, err := deps.sealer.Open(created.AIOKeyEnc)
	if err != nil || len(key) != 43 {
		t.Fatalf("decrypt generated AIO key: len=%d err=%v", len(key), err)
	}
	runtimeCredential, err := deps.sealer.Open(created.RuntimeCredentialEnc)
	if err != nil || len(runtimeCredential) != 43 || string(runtimeCredential) == string(key) || created.RuntimeGeneration != 1 {
		t.Fatalf("decrypt generated runtime credential: len=%d generation=%d err=%v", len(runtimeCredential), created.RuntimeGeneration, err)
	}
	params := deps.hosts.createParams
	if params == nil || params.Capacity != (Capacity{CPUMilli: 8000, MemMiB: 16384}) || len(params.WorkspaceIDs) != 0 {
		t.Fatalf("create params = %#v", params)
	}
	if len(deps.audit.entries) != 1 || deps.audit.entries[0].Action != audit.ActionHostCreate {
		t.Fatalf("audit entries = %#v", deps.audit.entries)
	}
}

func TestHostServiceUsesSavedResourcesWhenStarting(t *testing.T) {
	service, _ := newHostServiceTest(t)
	value, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{})
	if err != nil {
		t.Fatal(err)
	}
	value.CPUMilli, value.CPULimitMilli = 1500, 1500
	value.MemMiB, value.MemLimitMiB = 3072, 3072
	request, err := service.createRequest(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if request.Requests.CPU != "1500m" || request.Limits.CPU != "1500m" ||
		request.Requests.Memory != "3072Mi" || request.Limits.Memory != "3072Mi" {
		t.Fatalf("sandbox resources = %#v / %#v", request.Requests, request.Limits)
	}
}

func TestHostServiceCreateUsesAccountDefaultResources(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.users.value.DefaultHostCPUMilli = 1500
	deps.users.value.DefaultHostMemMiB = 3072
	value, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if value.CPUMilli != 1500 || value.MemMiB != 3072 || value.CPULimitMilli != 1500 || value.MemLimitMiB != 3072 {
		t.Fatalf("created resources = %#v", value)
	}
}

func TestHostServiceEnsuresStoppedDefaultWithoutStartingSandbox(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value, created, err := service.EnsureDefaultStopped(context.Background(), identity.User{ID: "u1234567"})
	if err != nil || !created || value.State != host.StateStopped || value.ExpiresAt != nil {
		t.Fatalf("EnsureDefaultStopped() = %#v, created=%t, err=%v", value, created, err)
	}
	again, created, err := service.EnsureDefaultStopped(context.Background(), identity.User{ID: "u1234567"})
	if err != nil || created || again.ID != value.ID || deps.sandbox.createCalls != 0 {
		t.Fatalf("repeated EnsureDefaultStopped() = %#v, created=%t, sandbox creates=%d, err=%v", again, created, deps.sandbox.createCalls, err)
	}
}

func TestHostServiceCreateReplaysExactRequestBeforeCurrentCapacityChecks(t *testing.T) {
	service, deps := newHostServiceTest(t)
	command := CreateHostCommand{Name: "retry", IdempotencyKey: "host-create-retry-001"}
	first, err := service.Create(context.Background(), hostServiceTestPrincipal(), command)
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	deps.diskCapacity.err = domain.NewError(domain.CodeCapacityExceeded, "disk is now full")

	replayed, err := service.Create(context.Background(), hostServiceTestPrincipal(), command)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replayed Create() = %#v, %v; want host %q", replayed, err, first.ID)
	}
	if deps.diskCapacity.calls != 1 || deps.hosts.createCalls != 1 || len(deps.audit.entries) != 1 {
		t.Fatalf("capacity checks/host creates/audits = %d/%d/%d, want 1/1/1", deps.diskCapacity.calls, deps.hosts.createCalls, len(deps.audit.entries))
	}

	command.Name = "changed"
	if _, err := service.Create(context.Background(), hostServiceTestPrincipal(), command); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with changed request error = %v, want idempotency conflict", err)
	}
}

func TestHostServiceCreateExistingWorkspaceChecksOwnerAndState(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.workspaces.values["w1234567"] = workspace.Workspace{
		ID: "w1234567", OwnerID: "u1234567",
		State: workspace.StateReady,
	}
	created, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{
		WorkspaceIDs: []string{"w1234567"},
	})
	if err != nil {
		t.Fatalf("Create(existing workspace) error = %v", err)
	}
	if testHostWorkspaceID(created) != "w1234567" || len(deps.hosts.createParams.WorkspaceIDs) != 1 {
		t.Fatalf("created host/params = %#v / %#v", created, deps.hosts.createParams)
	}
	if got := deps.workspaces.values["w1234567"]; got.State != workspace.StateReady || created.Workspaces[0].State != workspace.BindingPreparing {
		t.Fatalf("workspace/binding after create = %#v / %#v", got, created.Workspaces)
	}

	deps.workspaces.values["w7654321"] = workspace.Workspace{
		ID: "w7654321", OwnerID: "u2222222",
		State: workspace.StateReady,
	}
	if _, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{
		WorkspaceIDs: []string{"w7654321"},
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign workspace error = %v", err)
	}
}

func TestHostServiceCreateRetriesOnlyUniqueIDConflicts(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.hosts.createConflicts = 1
	created, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{})
	if err != nil {
		t.Fatalf("Create() after unique conflict error = %v", err)
	}
	if deps.hosts.createCalls != 2 || created.ID == "" {
		t.Fatalf("create calls/host = %d / %#v", deps.hosts.createCalls, created)
	}
	deps.hosts.createErr = errors.New("database unavailable")
	if _, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{}); err == nil || deps.hosts.createCalls != 3 {
		t.Fatalf("Create() non-unique error = %v, create calls=%d", err, deps.hosts.createCalls)
	}
}

func TestHostServiceAdvanceCreateCreatesAndWaitsUntilReady(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	sealHostServiceTestKey(t, deps.sealer, &value)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	var readyResult string
	service.observeReady = func(result string, duration time.Duration) { readyResult = result }

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate() error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateRunning || got.SandboxID != "sandbox-1" || got.CreateAttemptedAt == nil {
		t.Fatalf("host after create = %#v", got)
	}
	if deps.sandbox.createCalls != 1 || deps.sandbox.createRequest.Metadata["ch-env"] != "test" ||
		deps.sandbox.createRequest.Metadata["ch-host"] != value.ID || len(deps.sandbox.createRequest.Volumes) != 4 ||
		deps.sandbox.createRequest.Volumes[0] != (SandboxVolume{
			Name: "profile-data", MountPath: ProfileVolumeMountPath,
			ClaimName: domain.ProfileVolumeName(value.ID), Storage: ProfileVolumeStorage,
		}) ||
		deps.sandbox.createRequest.Volumes[1].ClaimName != domain.WorkspaceRootVolumeName(value.ID) ||
		deps.sandbox.createRequest.Volumes[2] != (SandboxVolume{
			Name: "runtime-data", MountPath: RuntimeDataVolumeMountPath,
			ClaimName: domain.RuntimeVolumeName(value.ID), Storage: RuntimeDataVolumeStorage,
		}) || deps.sandbox.createRequest.Volumes[3] != (SandboxVolume{
		Name: "host-apps", MountPath: HostAppsVolumeMountPath,
		ClaimName: domain.HostAppsVolumeName(value.ID), Storage: HostAppsVolumeStorage,
	}) {
		t.Fatalf("sandbox create calls/request = %d / %#v", deps.sandbox.createCalls, deps.sandbox.createRequest)
	}
	if deps.sandbox.createRequest.Env["TZ"] != "Asia/Shanghai" ||
		deps.sandbox.createRequest.Env["MORDE_RUNTIME_BOOTSTRAP_KEY"] == "" ||
		deps.sandbox.createRequest.Env["MORDE_HOST_REPLICA_KEY"] == "" || deps.sandbox.createRequest.Env["MORDE_PORTAL_URL"] != "https://portal.test" ||
		deps.sandbox.createRequest.Env["MORDE_HOST_ID"] != value.ID ||
		deps.sandbox.createRequest.Env["MORDE_RUNTIME_GENERATION"] != "1" ||
		deps.sandbox.createRequest.Metadata["MORDE_RUNTIME_BOOTSTRAP_KEY"] != "" ||
		deps.probe.upstream.APIKey == "" || readyResult != "running" {
		t.Fatalf("sandbox env/probe/metric = %#v / %#v / %q", deps.sandbox.createRequest.Env, deps.probe.upstream, readyResult)
	}
}

func TestHostServiceAdvanceCreateRecoversMetadataAndDeletesDuplicates(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	sealHostServiceTestKey(t, deps.sealer, &value)
	attempted := time.Now()
	value.CreateAttemptedAt = &attempted
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: "sandbox-a", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.sandbox.add(Sandbox{ID: "sandbox-b", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate(recover) error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateRunning || got.SandboxID != "sandbox-a" || deps.sandbox.createCalls != 0 {
		t.Fatalf("recovered host = %#v, create calls=%d", got, deps.sandbox.createCalls)
	}
	if deps.sandbox.values["sandbox-b"].State != SandboxTerminated {
		t.Fatalf("duplicate sandbox state = %#v", deps.sandbox.values["sandbox-b"])
	}
}

func TestHostServiceAdvanceCreateNeverRetriesAmbiguousCreate(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	attempted := time.Now().Add(-time.Minute)
	value.CreateAttemptedAt = &attempted
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate(confirmed absence) error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateFailed || got.StateReason != "ready_timeout" || deps.sandbox.createCalls != 0 {
		t.Fatalf("host/create calls = %#v / %d", got, deps.sandbox.createCalls)
	}
	if ws.State != workspace.StateReady {
		t.Fatalf("workspace after failure = %#v", ws)
	}
}

func TestHostServiceAdvanceCreateWaitsBeforeReleasingAmbiguousCreateWorkspace(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	attempted := time.Now()
	value.CreateAttemptedAt = &attempted
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate() error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateCreating || ws.State != workspace.StateReady || deps.sandbox.createCalls != 0 {
		t.Fatalf("host/workspace/create calls = %#v / %#v / %d", got, ws, deps.sandbox.createCalls)
	}
}

func TestHostServiceAdvanceCreateSkipsWhenAnotherWorkerHoldsCreateLock(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	sealHostServiceTestKey(t, deps.sealer, &value)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	lock, acquired, err := deps.createLocks.TryHostCreateLock(context.Background(), value.ID)
	if err != nil || !acquired {
		t.Fatalf("TryHostCreateLock() = %v, %v", acquired, err)
	}

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate() while locked error = %v", err)
	}
	if deps.sandbox.createCalls != 0 {
		t.Fatalf("Create() calls while locked = %d", deps.sandbox.createCalls)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate() after lock release error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateRunning || deps.sandbox.createCalls != 1 {
		t.Fatalf("host/create calls after release = %#v / %d", got, deps.sandbox.createCalls)
	}
}

func TestHostServiceAdvanceCreateKeepsWorkspaceWhenRemoteStateIsUnknown(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.listErr = ErrSandboxUnavailable

	if err := service.AdvanceCreate(context.Background(), value.ID); err == nil {
		t.Fatal("AdvanceCreate() error = nil")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateCreating || ws.State != workspace.StateReady || deps.sandbox.createCalls != 0 {
		t.Fatalf("host/workspace/create calls = %#v / %#v / %d", got, ws, deps.sandbox.createCalls)
	}
}

func TestHostServiceAdvanceCreateKeepsWorkspaceWhenDuplicateCleanupIsUncertain(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Second))
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: "sandbox-a", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.sandbox.add(Sandbox{ID: "sandbox-b", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.sandbox.deleteErr = ErrSandboxUnavailable

	if err := service.AdvanceCreate(context.Background(), value.ID); err == nil {
		t.Fatal("AdvanceCreate() error = nil")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateCreating || got.SandboxID != "sandbox-a" || ws.State != workspace.StateReady {
		t.Fatalf("host/workspace after uncertain cleanup = %#v / %#v", got, ws)
	}
}

func TestHostServiceAdvanceCreateFailureCleansSandboxBeforeDetach(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Minute))
	value.SandboxID = "sandbox-1"
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxFailed, Metadata: hostServiceMetadata(value.ID)})
	deps.sandbox.getAfterDelete = true

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate(failed sandbox) error = %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateFailed || got.SandboxID != "" || ws.State != workspace.StateReady ||
		deps.sandbox.values[value.SandboxID].State != SandboxTerminated {
		t.Fatalf("host/workspace/sandbox after cleanup = %#v / %#v / %#v", got, ws, deps.sandbox.values[value.SandboxID])
	}
}

func TestHostServiceAdminListAndOwnershipHideForeignHosts(t *testing.T) {
	service, deps := newHostServiceTest(t)
	first := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(first)
	second := first
	second.ID, second.OwnerID = "h7654321", "u2222222"
	deps.hosts.addHost(second)

	items, err := service.List(context.Background(), hostServiceTestPrincipal())
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("user List() = %#v, %v", items, err)
	}
	if _, err := service.Get(context.Background(), hostServiceTestPrincipal(), second.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign Get() error = %v", err)
	}
	admin := hostServiceTestPrincipal()
	admin.Role = identity.RoleAdmin
	items, err = service.List(context.Background(), admin)
	if err != nil || len(items) != 2 {
		t.Fatalf("admin List() = %#v, %v", items, err)
	}
}

func TestHostServiceDeleteNeverDeletesIndependentWorkspace(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateFailed)
	value.SandboxID = ""
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Hour))
	deps.hosts.addHost(value)
	binding := value.Workspaces[0]
	binding.State = workspace.BindingDetached
	deps.workspaces.addBinding(binding)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	updated, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: value.ID})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if updated.State != host.StateDeleting || ws.State != workspace.StateReady {
		t.Fatalf("host/workspace after Delete() = %#v / %#v", updated, ws)
	}

	old := hostServiceTestHost(host.StateFailed)
	old.ID = "h8765432"
	old.Workspaces[0].Workspace.ID = "w7654321"
	deps.hosts.addHost(old)
	oldBinding := old.Workspaces[0]
	oldBinding.State = workspace.BindingDetached
	deps.workspaces.addBinding(oldBinding)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(old), OwnerID: old.OwnerID, State: workspace.StateReady,
	})
	if _, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: old.ID}); err != nil {
		t.Fatalf("Delete(reused workspace) error = %v", err)
	}
	newer, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(old))
	if newer.State != workspace.StateReady {
		t.Fatalf("newer workspace changed: %#v", newer)
	}
}

func TestHostServiceRequiresVerifiedUnbindBeforeHostDeletion(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	deps.workspaces.addBinding(value.Workspaces[0])

	if _, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: value.ID}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("Delete(active workspace) error = %v, want workspace busy", err)
	}
	current, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if current.State != host.StateRunning {
		t.Fatalf("Host transitioned before workspace detach: %#v", current)
	}
}

func TestHostServiceRetryWorkspaceSyncResetsConflictIdempotently(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	binding := value.Workspaces[0]
	binding.SyncState = workspace.BindingSyncConflict
	binding.SyncConflicts = []string{"src/main.go"}
	deps.workspaces.addBinding(binding)

	retried, err := service.RetryWorkspaceSync(context.Background(), hostServiceTestPrincipal(), value.ID, binding.Workspace.ID, binding.BindingVersion)
	if err != nil || retried.SyncState != workspace.BindingSyncHealthy || len(retried.SyncConflicts) != 0 {
		t.Fatalf("RetryWorkspaceSync() = %#v, %v", retried, err)
	}
	// A retry after a lost response is safe: once healthy, it returns current state.
	replayed, err := service.RetryWorkspaceSync(context.Background(), hostServiceTestPrincipal(), value.ID, binding.Workspace.ID, binding.BindingVersion)
	if err != nil || replayed.SyncState != workspace.BindingSyncHealthy || len(replayed.SyncConflicts) != 0 {
		t.Fatalf("replayed RetryWorkspaceSync() = %#v, %v", replayed, err)
	}

	if _, err := service.RetryWorkspaceSync(context.Background(), hostServiceTestPrincipal(), value.ID, binding.Workspace.ID, binding.BindingVersion+1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("RetryWorkspaceSync(stale version) error = %v, want workspace conflict", err)
	}
	current, err := deps.workspaces.GetWorkspaceBinding(context.Background(), value.ID, binding.Workspace.ID)
	if err != nil || current.SyncState != workspace.BindingSyncHealthy || len(current.SyncConflicts) != 0 {
		t.Fatalf("binding after stale retry = %#v, %v", current, err)
	}
}

func TestHostServiceBindAndUnbindWorkspaceAreIdempotent(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	value.Workspaces = nil
	deps.hosts.addHost(value)
	ws := workspace.Workspace{ID: "w7654321", OwnerID: value.OwnerID, Name: "code", State: workspace.StateReady, Revision: 5}
	deps.workspaces.add(ws)
	revision := int64(5)

	bound, err := service.BindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, &revision, 0, "bind-workspace-01")
	if err != nil || bound.State != workspace.BindingPreparing || bound.DesiredRevision != 5 || bound.AppliedRevision != 0 {
		t.Fatalf("BindWorkspace() = %#v, %v", bound, err)
	}
	repeated, err := service.BindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, &revision, 0, "bind-workspace-01")
	if err != nil || repeated.BindingVersion != bound.BindingVersion {
		t.Fatalf("repeated BindWorkspace() = %#v, %v", repeated, err)
	}

	active := bound
	active.State = workspace.BindingActive
	active.BindingVersion++
	active.AppliedRevision = active.DesiredRevision
	deps.workspaces.addBinding(active)
	if _, err := service.UnbindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, active.BindingVersion, false, "unbind-workspace-00"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("UnbindWorkspace without writer-stop confirmation error = %v", err)
	}
	unbound, err := service.UnbindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, active.BindingVersion, true, "unbind-workspace-01")
	if err != nil || unbound.State != workspace.BindingUnbinding {
		t.Fatalf("UnbindWorkspace() = %#v, %v", unbound, err)
	}
	repeated, err = service.UnbindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, active.BindingVersion, true, "unbind-workspace-01")
	if err != nil || repeated.BindingVersion != unbound.BindingVersion {
		t.Fatalf("repeated UnbindWorkspace() = %#v, %v", repeated, err)
	}
	detached, err := deps.workspaces.TransitionWorkspaceBinding(context.Background(), value.ID, ws.ID, unbound.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, unbound.DesiredRevision, unbound.AppliedRevision)
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := service.BindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, &revision, detached.BindingVersion, "bind-workspace-02")
	if err != nil || rebound.State != workspace.BindingPreparing {
		t.Fatalf("rebind after verified detach = %#v, %v", rebound, err)
	}
	if _, err := service.UnbindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, active.BindingVersion, true, "unbind-workspace-01"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("replay stale unbind after rebind error = %v", err)
	}
	if _, err := service.BindWorkspace(context.Background(), hostServiceTestPrincipal(), value.ID, ws.ID, &revision, 0, "bind-workspace-01"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("replay stale bind after rebind error = %v", err)
	}
}

func TestHostServiceRenew(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	updated, err := service.Renew(context.Background(), hostServiceTestPrincipal(), value.ID)
	if err != nil || updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(deps.sandbox.renewExpiry) {
		t.Fatalf("Renew() = %#v, %v, upstream=%s", updated, err, deps.sandbox.renewExpiry)
	}
}

func TestHostServiceReleaseRetainsHostAndWorkspace(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})

	released, err := service.Release(context.Background(), hostServiceTestPrincipal(), value.ID, true)
	if err != nil || released.ID != value.ID || released.State != host.StateStopping || released.StateReason != "user_release" {
		t.Fatalf("Release() = %#v, %v", released, err)
	}
	if !deps.hosts.releaseConfirmed {
		t.Fatal("Release() did not pass process termination confirmation to the store")
	}
	stored, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if testHostWorkspaceID(stored) != testHostWorkspaceID(value) || stored.OwnerID != value.OwnerID || ws.State != workspace.StateReady {
		t.Fatalf("Host/Workspace after release = %#v / %#v", stored, ws)
	}
	if len(deps.audit.entries) != 1 || deps.audit.entries[0].Action != audit.ActionHostRelease || deps.audit.entries[0].Detail["runtime_generation"] != int64(1) {
		t.Fatalf("release audit = %#v", deps.audit.entries)
	}
}

func TestHostServiceReleaseRejectsUserControl(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	value.ControlState = host.ControlUser
	deps.hosts.addHost(value)
	if _, err := service.Release(context.Background(), hostServiceTestPrincipal(), value.ID, false); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Release() with user control error = %v", err)
	}
	stored, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if stored.State != host.StateRunning {
		t.Fatalf("Host after rejected release = %#v", stored)
	}
}

func TestHostServiceRestorePreservesIdentityAndVolumes(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateStopped)
	value.SandboxID = ""
	value.StateReason = "user_release"
	sealHostServiceTestKey(t, deps.sealer, &value)
	deps.hosts.addHost(value)
	workspaceValue := workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	}
	deps.workspaces.add(workspaceValue)

	oldCredential, err := deps.sealer.Open(value.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.Restore(context.Background(), hostServiceTestPrincipal(), value.ID)
	if err != nil || restored.ID != value.ID || restored.OwnerID != value.OwnerID || testHostWorkspaceID(restored) != testHostWorkspaceID(value) ||
		restored.State != host.StateCreating || restored.RuntimeGeneration != 2 || restored.StateReason != "restore_pending" {
		t.Fatalf("Restore() = %#v, %v", restored, err)
	}
	newCredential, err := deps.sealer.Open(restored.RuntimeCredentialEnc)
	if err != nil || string(newCredential) == string(oldCredential) {
		t.Fatalf("restored runtime credential was not rotated: %v", err)
	}
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if ws != workspaceValue {
		t.Fatalf("Workspace after restore start = %#v, want %#v", ws, workspaceValue)
	}
	if err := service.AdvanceCreate(context.Background(), restored.ID); err != nil {
		t.Fatalf("AdvanceCreate(restore) = %v", err)
	}
	if deps.sandbox.createRequest.Metadata["ch-generation"] != "2" ||
		len(deps.sandbox.createRequest.Volumes) != 4 ||
		deps.sandbox.createRequest.Volumes[0].ClaimName != domain.ProfileVolumeName(value.ID) ||
		deps.sandbox.createRequest.Volumes[1].ClaimName != domain.WorkspaceRootVolumeName(value.ID) ||
		deps.sandbox.createRequest.Volumes[2].ClaimName != domain.RuntimeVolumeName(value.ID) ||
		deps.sandbox.createRequest.Volumes[3] != (SandboxVolume{
			Name: "host-apps", MountPath: HostAppsVolumeMountPath,
			ClaimName: domain.HostAppsVolumeName(value.ID), Storage: HostAppsVolumeStorage,
		}) {
		t.Fatalf("restore sandbox identity/volumes = %#v / %#v", deps.sandbox.createRequest.Metadata, deps.sandbox.createRequest.Volumes)
	}
	final, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if final.State != host.StateRunning || final.RuntimeGeneration != 2 || testHostWorkspaceID(final) != testHostWorkspaceID(value) {
		t.Fatalf("Host after restore = %#v", final)
	}
}

func TestHostServiceRestoreUnknownCreateIsFailClosed(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.RuntimeGeneration = 2
	value.StateReason = "restore_pending"
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Hour))
	value.SandboxID = ""
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})

	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate(unknown restore) = %v", err)
	}
	stored, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if stored.State != host.StateCreating || stored.StateReason != "restore_unknown" || stored.RuntimeGeneration != 2 || deps.sandbox.createCalls != 0 {
		t.Fatalf("Host/create after unknown restore = %#v / %d", stored, deps.sandbox.createCalls)
	}
	if _, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: value.ID}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Delete(unknown restore) error = %v", err)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if len(volume.names) != 0 || ws.State != workspace.StateReady {
		t.Fatalf("data changed after unknown restore: volumes=%v workspace=%#v", volume.names, ws)
	}

	// The persisted marker also remains a deletion fence if a recovery attempt
	// leaves the Host in a different state or loses the timestamp field.
	markerOnly := hostServiceTestHost(host.StateStopped)
	markerOnly.RuntimeGeneration = 2
	markerOnly.StateReason = "restore_unknown"
	markerOnly.CreateAttemptedAt = nil
	deps.hosts.addHost(markerOnly)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(markerOnly), OwnerID: markerOnly.OwnerID, State: workspace.StateReady,
	})
	if _, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: markerOnly.ID}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Delete(restore_unknown marker without create timestamp) error = %v", err)
	}
	markerWorkspace, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(markerOnly))
	if markerWorkspace.State != workspace.StateReady {
		t.Fatalf("Workspace changed after restore_unknown marker delete refusal: %#v", markerWorkspace)
	}
}

func TestHostServiceAPIKeyHasDynamicHostAndComputerAccess(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	principal := identity.Principal{
		UserID: value.OwnerID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{value.ID}, Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}
	got, err := service.GetForComputer(context.Background(), principal, value.ID, identity.ScopeComputerObserve)
	if err != nil || got.ID != value.ID {
		t.Fatalf("GetForComputer() = %#v, %v", got, err)
	}
	if _, err := service.Get(context.Background(), principal, value.ID); err != nil {
		t.Fatalf("Get() with dynamic API key grant error = %v", err)
	}
	if _, err := service.GetForComputer(context.Background(), principal, value.ID, identity.ScopeHostRead); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("GetForComputer() with non-computer scope error = %v", err)
	}
}

func TestHostServiceIssueTicket(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)

	enter, err := service.IssueTicket(context.Background(), hostServiceTestPrincipal(), value.ID)
	if err != nil {
		t.Fatalf("IssueTicket() error = %v", err)
	}
	parsed, err := url.Parse(enter.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != value.ID+".hosts.test" || parsed.Path != "/__ch/auth" || !strings.HasPrefix(parsed.Query().Get("t"), "chx_") {
		t.Fatalf("ticket URL = %q (parse err %v)", enter.URL, err)
	}
	if len(deps.tokens.values) != 1 || deps.tokens.values[0].Kind != identity.KindTicket || deps.tokens.values[0].HostID != value.ID ||
		!identity.VerifySecret(mustCredentialSecret(t, parsed.Query().Get("t")), deps.tokens.values[0].SecretHash) {
		t.Fatalf("ticket persistence = %#v", deps.tokens.values)
	}

}

func TestHostServiceClientOwnerIssueTicketBindsInstallationAndAuditActor(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	principal := hostServiceTestPrincipal()
	principal.Kind = identity.KindClient
	principal.TokenID = "t1234567"
	principal.ClientInstallationID = "i1234567"

	enter, err := service.IssueTicket(context.Background(), principal, value.ID)
	parsed, parseErr := url.Parse(enter.URL)
	if err != nil || parseErr != nil || parsed.Host != value.ID+".hosts.test" || parsed.Path != "/__ch/auth" ||
		!strings.HasPrefix(parsed.Query().Get("t"), "chx_") || !enter.ExpiresAt.After(time.Now()) {
		t.Fatalf("IssueTicket(Client) = %#v, parse=%v, err=%v", enter, parseErr, err)
	}
	if len(deps.tokens.values) != 1 {
		t.Fatalf("persisted ticket count = %d, want 1", len(deps.tokens.values))
	}
	ticket := deps.tokens.values[0]
	if ticket.Kind != identity.KindTicket || ticket.OwnerID != value.OwnerID || ticket.ActorUserID != "" ||
		ticket.HostID != value.ID || ticket.ClientInstallationID != principal.ClientInstallationID {
		t.Fatalf("client owner ticket = %#v", ticket)
	}
	if len(deps.audit.entries) != 1 || deps.audit.entries[0].ActorID != principal.UserID ||
		deps.audit.entries[0].ActorKind != audit.ActorClient || deps.audit.entries[0].Action != audit.ActionHostEnter ||
		deps.audit.entries[0].Target != value.ID {
		t.Fatalf("client enter audit = %#v", deps.audit.entries)
	}
}

func TestHostServiceInstalledWebTicketsStayBoundToInstallation(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	principal := hostServiceTestPrincipal()
	principal.ClientInstallationID = "i1234567"
	if _, err := service.IssueTicket(context.Background(), principal, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.IssuePreviewTicket(context.Background(), principal, value.ID, 5173); err != nil {
		t.Fatal(err)
	}
	if len(deps.tokens.values) != 2 || deps.tokens.values[0].ClientInstallationID != principal.ClientInstallationID ||
		deps.tokens.values[1].ClientInstallationID != principal.ClientInstallationID || deps.tokens.values[1].PreviewPort != 5173 {
		t.Fatalf("installed Web tickets = %#v", deps.tokens.values)
	}
}

func TestHostServiceIssuePreviewTicketIsOwnerOnlyAndPortScoped(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)

	enter, err := service.IssuePreviewTicket(context.Background(), hostServiceTestPrincipal(), value.ID, 5173)
	parsed, parseErr := url.Parse(enter.URL)
	if err != nil || parseErr != nil || parsed.Scheme != "https" || parsed.Host != "5173-"+value.ID+".hosts.test" ||
		parsed.Path != "/__ch/auth" || parsed.Query().Get("t") == "" || !enter.ExpiresAt.After(time.Now()) {
		t.Fatalf("IssuePreviewTicket() = %#v, parse=%v, err=%v", enter, parseErr, err)
	}
	if len(deps.tokens.values) != 1 || deps.tokens.values[0].OwnerID != value.OwnerID || deps.tokens.values[0].HostID != value.ID ||
		deps.tokens.values[0].PreviewPort != 5173 || deps.tokens.values[0].ActorUserID != "" {
		t.Fatalf("preview ticket = %#v", deps.tokens.values)
	}

	member := hostServiceTestPrincipal()
	member.UserID = "u7654321"
	if _, err := service.IssuePreviewTicket(context.Background(), member, value.ID, 5173); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("member preview ticket error = %v, want not found", err)
	}
	if _, err := service.IssuePreviewTicket(context.Background(), hostServiceTestPrincipal(), value.ID, 0); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid preview port error = %v, want invalid argument", err)
	}
	if len(deps.tokens.values) != 1 {
		t.Fatalf("denied preview ticket was persisted: %#v", deps.tokens.values)
	}
}

func TestHostServiceIssueTicketRequiresPortalSession(t *testing.T) {
	tests := []struct {
		name      string
		principal func(string) identity.Principal
		wantCode  domain.ErrorCode
		wantMint  bool
	}{
		{
			name: "host cookie cannot mint a new ticket",
			principal: func(hostID string) identity.Principal {
				value := hostServiceTestPrincipal()
				value.Kind, value.HostScope = identity.KindHostCookie, hostID
				return value
			},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "host cookie scoped to another host cannot mint a ticket",
			principal: func(string) identity.Principal {
				value := hostServiceTestPrincipal()
				value.Kind, value.HostScope = identity.KindHostCookie, "h8765432"
				return value
			},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "API Key cannot enter the GUI",
			principal: func(hostID string) identity.Principal {
				value := hostServiceTestPrincipal()
				value.Kind = identity.KindAPIKey
				value.HostIDs = []string{hostID}
				value.Scopes = []identity.Scope{identity.ScopeHostRead}
				return value
			},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "admin cannot enter another owner's host",
			principal: func(string) identity.Principal {
				value := hostServiceTestPrincipal()
				value.UserID = "u8765432"
				value.Role = identity.RoleAdmin
				return value
			},
			wantCode: domain.CodeNotFound,
		},
		{
			name: "ticket credential",
			principal: func(hostID string) identity.Principal {
				value := hostServiceTestPrincipal()
				value.Kind, value.HostScope = identity.KindTicket, hostID
				return value
			},
			wantCode: domain.CodeForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, deps := newHostServiceTest(t)
			value := hostServiceTestHost(host.StateRunning)
			deps.hosts.addHost(value)

			enter, err := service.IssueTicket(context.Background(), test.principal(value.ID), value.ID)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("IssueTicket() error = %v", err)
				}
				parsed, parseErr := url.Parse(enter.URL)
				if parseErr != nil || parsed.Host != value.ID+".hosts.test" || parsed.Path != "/__ch/auth" {
					t.Fatalf("ticket URL = %q (parse err %v)", enter.URL, parseErr)
				}
			} else if domain.CodeOf(err) != test.wantCode {
				t.Fatalf("IssueTicket() error = %v, want %s", err, test.wantCode)
			}
			wantMintCount := 0
			if test.wantMint {
				wantMintCount = 1
			}
			if len(deps.tokens.values) != wantMintCount {
				t.Fatalf("minted ticket count = %d, want %t", len(deps.tokens.values), test.wantMint)
			}
			if test.wantMint && (deps.tokens.values[0].Kind != identity.KindTicket || deps.tokens.values[0].OwnerID != value.OwnerID || deps.tokens.values[0].HostID != value.ID) {
				t.Fatalf("minted ticket = %#v, want owner and host %q", deps.tokens.values[0], value.ID)
			}
		})
	}
}

func TestHostServiceConstructorRejectsMissingDependencies(t *testing.T) {
	if _, err := NewHostService(HostServiceOptions{}); err == nil {
		t.Fatal("NewHostService() error = nil")
	}
}

func TestHostServiceRejectsUnscopedOrMissingCredentialKind(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	principal := hostServiceTestPrincipal()
	principal.Kind = ""
	if _, err := service.Get(context.Background(), principal, value.ID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Get() with empty credential kind error = %v", err)
	}
	principal = hostServiceTestPrincipal()
	principal.Kind = identity.KindHostCookie
	principal.HostScope = "h7654321"
	if _, err := service.Get(context.Background(), principal, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Get() with mismatched scope error = %v", err)
	}
}

func TestHostServiceCreateAuditFailureDoesNotUndoCommittedHost(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.audit.err = errors.New("audit unavailable")
	failures := 0
	service.countAudit = func() { failures++ }
	created, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{})
	if err != nil || created.State != host.StateCreating || failures != 1 {
		t.Fatalf("Create() after audit error = %#v, %v, failures=%d", created, err, failures)
	}
}

func TestHostServiceCreateChecksStorageBeforePersistingHost(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code domain.ErrorCode
	}{
		{name: "below reserve", err: domain.NewError(domain.CodeCapacityExceeded, "low space"), code: domain.CodeCapacityExceeded},
		{name: "capacity check unavailable", err: errors.New("private engine detail"), code: domain.CodeUpstreamError},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, deps := newHostServiceTest(t)
			deps.diskCapacity.err = test.err
			if _, err := service.Create(context.Background(), hostServiceTestPrincipal(), CreateHostCommand{}); domain.CodeOf(err) != test.code {
				t.Fatalf("Create() error = %v, want %s", err, test.code)
			}
			if deps.diskCapacity.calls != 1 || deps.hosts.createCalls != 0 || len(deps.workspaces.values) != 0 {
				t.Fatalf("capacity calls=%d host creates=%d workspaces=%d, want 1/0/0", deps.diskCapacity.calls, deps.hosts.createCalls, len(deps.workspaces.values))
			}
		})
	}
}

func TestHostServiceAuthorizeTLSOnlyAllowsRunningHosts(t *testing.T) {
	service, deps := newHostServiceTest(t)
	for _, state := range []host.State{host.StateCreating, host.StateRunning, host.StateDeleting, host.StateFailed, host.StateDeleted} {
		value := hostServiceTestHost(state)
		deps.hosts.addHost(value)
		if got, want := service.AuthorizeTLS(context.Background(), value.ID), state == host.StateRunning; got != want {
			t.Errorf("AuthorizeTLS(%q, %q) = %t, want %t", value.ID, state, got, want)
		}
	}
	if service.AuthorizeTLS(context.Background(), "h7654321") {
		t.Error("AuthorizeTLS(missing host) = true, want false")
	}
	if service.AuthorizeTLS(context.Background(), "invalid") {
		t.Error("AuthorizeTLS(invalid ID) = true, want false")
	}
}

func newHostServiceTest(t *testing.T) (*hostService, hostServiceTestDeps) {
	t.Helper()
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host), idempotency: make(map[string]IdempotencyRecord)}
	workspaces := &hostServiceTestWorkspaceStore{values: make(map[string]workspace.Workspace)}
	hosts.workspaces = workspaces
	sandbox := &hostServiceTestSandbox{values: make(map[string]Sandbox), getAfterDelete: true}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	auditWriter := &hostServiceTestAudit{}
	createLocks := &hostServiceTestCreateLocks{held: make(map[string]bool)}
	tokens := &hostServiceTestTokens{}
	ready := &hostServiceTestProbe{}
	runtimeReady := &hostServiceTestRuntimeHandshaker{}
	diskCapacity := &hostServiceTestDiskCapacity{}
	users := &hostServiceTestUsers{value: identity.User{ID: "u1234567", Status: identity.StatusApproved,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048}}
	service, err := NewHostService(HostServiceOptions{
		Hosts: hosts, Users: users, Workspaces: workspaces, Tokens: tokens,
		Sandboxes: &hostServiceTestRouter{client: sandbox}, Volumes: &hostServiceTestVolume{}, DiskCapacity: diskCapacity, Probe: ready, Runtime: runtimeReady, Audit: auditWriter, CreateLocks: createLocks,
		Sealer: sealer, Templates: map[string]template.Template{"small": hostServiceTestTemplate()}, DefaultTemplateID: "small",
		Capacity: Capacity{CPUMilli: 8000, MemMiB: 16384}, Instance: "test",
		Scheme: "https", PortalURL: "https://portal.test", HostSuffix: "hosts.test", TicketTTL: time.Minute,
		ReadyTimeout: 20 * time.Millisecond, ReadyPollInterval: time.Millisecond,
		NotifyCreate:     func(context.Context, string) error { return nil },
		ObserveHostReady: func(string, time.Duration) {}, CountAuditFailure: func() {},
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, hostServiceTestDeps{
		hosts: hosts, users: users, workspaces: workspaces, tokens: tokens,
		sandbox: sandbox, probe: ready, runtime: runtimeReady, audit: auditWriter, sealer: sealer, createLocks: createLocks, diskCapacity: diskCapacity,
	}
}

type hostServiceTestDiskCapacity struct {
	err   error
	calls int
}

func (c *hostServiceTestDiskCapacity) CheckCapacity(context.Context) error {
	c.calls++
	return c.err
}

type hostServiceTestDeps struct {
	hosts        *hostServiceTestHostStore
	users        *hostServiceTestUsers
	workspaces   *hostServiceTestWorkspaceStore
	tokens       *hostServiceTestTokens
	sandbox      *hostServiceTestSandbox
	probe        *hostServiceTestProbe
	runtime      *hostServiceTestRuntimeHandshaker
	audit        *hostServiceTestAudit
	sealer       *Sealer
	createLocks  *hostServiceTestCreateLocks
	diskCapacity *hostServiceTestDiskCapacity
}

type hostServiceTestUsers struct {
	UserStore
	value identity.User
}

func (s *hostServiceTestUsers) GetUserByID(_ context.Context, id string) (identity.User, error) {
	if id != s.value.ID {
		return identity.User{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	return s.value, nil
}

type hostServiceTestRuntimeHandshaker struct {
	err   error
	hosts []string
}

func (h *hostServiceTestRuntimeHandshaker) Handshake(_ context.Context, hostID string) error {
	h.hosts = append(h.hosts, hostID)
	return h.err
}

type hostServiceTestCreateLocks struct {
	mu   sync.Mutex
	held map[string]bool
}

func (s *hostServiceTestCreateLocks) TryHostCreateLock(_ context.Context, hostID string) (LeaderLock, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[hostID] {
		return nil, false, nil
	}
	s.held[hostID] = true
	return hostServiceTestCreateLock{store: s, hostID: hostID}, true, nil
}

type hostServiceTestCreateLock struct {
	store  *hostServiceTestCreateLocks
	hostID string
}

func (l hostServiceTestCreateLock) Release(context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	delete(l.store.held, l.hostID)
	return nil
}

func hostServiceTestTemplate() template.Template {
	return template.Template{
		ID: "small", Name: "Small", Image: "example/aio:1@sha256:" + strings.Repeat("a", 64),
		Entrypoint: []string{template.RuntimeBootstrapEntrypoint},
		Resources: template.Resources{
			Requests: template.Resource{CPU: "500m", Memory: "1536Mi"},
			Limits:   template.Resource{CPU: "2", Memory: "4Gi"},
		},
		Workspace: template.Workspace{MountPath: "/home/gem/workspace", Size: "2Gi"},
		Lifecycle: template.Lifecycle{TTL: 8 * time.Hour, IdleTimeout: time.Hour, OnIdle: template.OnIdleDisabled},
		Isolation: "container",
	}
}

func hostServiceTestPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
}

func hostServiceTestHost(state host.State) host.Host {
	sealed := []byte("sealed-aio-key-placeholder")
	return host.Host{
		ID: "h1234567", OwnerID: "u1234567", Name: "test", TemplateID: "small", RuntimeProfile: "container",
		Workspaces: []workspace.Binding{testWorkspaceBinding("h1234567", "u1234567", "w1234567", workspace.BindingActive)},
		SandboxID:  "sandbox-1", State: state, RuntimeGeneration: 1, ControlState: host.ControlAI,
		CPUMilli: 500, MemMiB: 1536, CPULimitMilli: 2000, MemLimitMiB: 4096,
		AIOKeyEnc: sealed, CreatedAt: time.Now().Add(-time.Millisecond),
	}
}

func hostServiceMetadata(hostID string) map[string]string {
	return map[string]string{"ch-env": "test", "ch-host": hostID, "ch-owner": "u1234567", "ch-generation": "1"}
}

func testWorkspaceBinding(hostID, ownerID, workspaceID string, state workspace.BindingState) workspace.Binding {
	return workspace.Binding{
		ID: "b1234567", OwnerID: ownerID, HostID: hostID,
		Workspace: workspace.Workspace{ID: workspaceID, OwnerID: ownerID, Name: "test", State: workspace.StateReady},
		Directory: workspaceID, State: state, BindingVersion: 1, SyncState: workspace.BindingSyncHealthy,
	}
}

func testHostWorkspaceID(value host.Host) string {
	if len(value.Workspaces) == 0 {
		return ""
	}
	return value.Workspaces[0].Workspace.ID
}

func ptrTime(value time.Time) *time.Time { return &value }

func sealHostServiceTestKey(t *testing.T, sealer *Sealer, value *host.Host) {
	t.Helper()
	sealed, err := sealer.Seal([]byte("test-aio-key"))
	if err != nil {
		t.Fatal(err)
	}
	value.AIOKeyEnc = sealed
	sealed, err = sealer.Seal([]byte("test-runtime-credential"))
	if err != nil {
		t.Fatal(err)
	}
	value.RuntimeCredentialEnc = sealed
}

func mustCredentialSecret(t *testing.T, value string) string {
	t.Helper()
	_, _, secret, err := identity.ParseCredential(value)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

type hostServiceTestHostStore struct {
	mu                  sync.Mutex
	values              map[string]host.Host
	idempotency         map[string]IdempotencyRecord
	workspaces          *hostServiceTestWorkspaceStore
	activeLease         bool
	activityLeaseErr    error
	createParams        *CreateHostParams
	createCalls         int
	createConflicts     int
	createErr           error
	releaseConfirmed    bool
	applicationLaunches map[string]ApplicationLaunch
	applicationInstalls map[string]ApplicationInstall
}

func (s *hostServiceTestHostStore) GetHost(_ context.Context, id string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return cloneHostServiceHost(value), nil
}

func (s *hostServiceTestHostStore) ListHosts(context.Context) ([]host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]host.Host, 0, len(s.values))
	for _, value := range s.values {
		result = append(result, cloneHostServiceHost(value))
	}
	return result, nil
}

func (s *hostServiceTestHostStore) HasActiveHostActivityLease(context.Context, string, int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLease, s.activityLeaseErr
}

func (s *hostServiceTestHostStore) AcquireHostActivityLease(context.Context, HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activityLeaseErr != nil {
		return s.activityLeaseErr
	}
	s.activeLease = true
	return nil
}

func (s *hostServiceTestHostStore) RenewHostActivityLease(context.Context, HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activityLeaseErr != nil {
		return s.activityLeaseErr
	}
	return nil
}

func (s *hostServiceTestHostStore) ReleaseHostActivityLease(context.Context, HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeLease = false
	return nil
}

func (s *hostServiceTestHostStore) ListHostsByOwner(_ context.Context, ownerID string) ([]host.Host, error) {
	values, _ := s.ListHosts(context.Background())
	result := make([]host.Host, 0)
	for _, value := range values {
		if value.OwnerID == ownerID {
			result = append(result, value)
		}
	}
	return result, nil
}

func (s *hostServiceTestHostStore) GetHostCreateReplay(_ context.Context, record IdempotencyRecord) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.idempotency[hostCreateTestIdempotencyKey(record)]
	if !ok {
		return host.Host{}, false, nil
	}
	if !bytes.Equal(stored.RequestHash, record.RequestHash) {
		return host.Host{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
	}
	value, ok := s.values[stored.ResourceID]
	if !ok {
		return host.Host{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return cloneHostServiceHost(value), true, nil
}

func (s *hostServiceTestHostStore) CreateHost(_ context.Context, params CreateHostParams) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if params.Idempotency.Key != "" {
		if stored, ok := s.idempotency[hostCreateTestIdempotencyKey(params.Idempotency)]; ok {
			if !bytes.Equal(stored.RequestHash, params.Idempotency.RequestHash) {
				return host.Host{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
			}
			return cloneHostServiceHost(s.values[stored.ResourceID]), true, nil
		}
	}
	if s.createConflicts > 0 {
		s.createConflicts--
		return host.Host{}, false, &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	}
	if s.createErr != nil {
		return host.Host{}, false, s.createErr
	}
	if _, exists := s.values[params.Host.ID]; exists {
		return host.Host{}, false, fmt.Errorf("duplicate host")
	}
	copyParams := params
	s.createParams = &copyParams
	value := cloneHostServiceHost(params.Host)
	value.CreatedAt = time.Now()
	value.UpdatedAt = value.CreatedAt
	value.Workspaces = make([]workspace.Binding, 0, len(params.WorkspaceIDs))
	for _, workspaceID := range params.WorkspaceIDs {
		workspaceValue, err := s.workspaces.GetWorkspace(context.Background(), workspaceID)
		if err != nil || workspaceValue.State != workspace.StateReady || workspaceValue.OwnerID != value.OwnerID {
			return host.Host{}, false, domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available")
		}
		binding := testWorkspaceBinding(value.ID, value.OwnerID, workspaceID, workspace.BindingPreparing)
		binding.Workspace = workspaceValue
		binding.DesiredRevision = workspaceValue.Revision
		created, err := s.workspaces.CreateWorkspaceBinding(context.Background(), binding)
		if err != nil {
			return host.Host{}, false, err
		}
		value.Workspaces = append(value.Workspaces, created)
	}
	s.values[value.ID] = value
	if params.Idempotency.Key != "" {
		if s.idempotency == nil {
			s.idempotency = make(map[string]IdempotencyRecord)
		}
		s.idempotency[hostCreateTestIdempotencyKey(params.Idempotency)] = params.Idempotency
	}
	return cloneHostServiceHost(value), false, nil
}

func (s *hostServiceTestHostStore) EnsureDefaultHost(_ context.Context, params CreateHostParams) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.values {
		if value.OwnerID == params.Host.OwnerID && value.DeletedAt == nil && value.State != host.StateDeleted {
			return cloneHostServiceHost(value), false, nil
		}
	}
	value := cloneHostServiceHost(params.Host)
	if s.values == nil {
		s.values = make(map[string]host.Host)
	}
	s.values[value.ID] = value
	return value, true, nil
}

func hostCreateTestIdempotencyKey(record IdempotencyRecord) string {
	return record.OwnerID + "\x00" + record.Operation + "\x00" + record.Key
}

func (s *hostServiceTestHostStore) FailUnattemptedCreate(_ context.Context, id, reason string) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State == host.StateCreating && value.CreateAttemptedAt != nil {
		return cloneHostServiceHost(value), false, nil
	}
	if value.State != host.StateCreating {
		return host.Host{}, false, invalidHostState(value.State)
	}
	if value.RuntimeGeneration > 1 {
		value.State, value.StateReason = host.StateStopped, "restore_failed"
	} else {
		value.State, value.StateReason = host.StateFailed, reason
	}
	s.values[id] = value
	return cloneHostServiceHost(value), true, nil
}

func (s *hostServiceTestHostStore) MarkCreateAttempt(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != host.StateCreating || value.CreateAttemptedAt != nil {
		return invalidHostState(value.State)
	}
	now := time.Now()
	value.CreateAttemptedAt = &now
	s.values[id] = value
	return nil
}

func (s *hostServiceTestHostStore) SetSandboxID(_ context.Context, id string, expected host.State, sandboxID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != expected || value.CreateAttemptedAt == nil {
		return invalidHostState(value.State)
	}
	value.SandboxID = ""
	if sandboxID != nil {
		value.SandboxID = *sandboxID
	}
	s.values[id] = value
	return nil
}

func (s *hostServiceTestHostStore) SetRuntimeInstanceID(_ context.Context, id string, generation, controlVersion int64, expectedInstanceID, instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if (value.State != host.StateCreating && value.State != host.StateRunning) || value.RuntimeGeneration != generation || value.ControlVersion != controlVersion || value.RuntimeInstanceID != expectedInstanceID {
		return domain.NewError(domain.CodeInvalidState, "runtime instance fence changed")
	}
	value.RuntimeInstanceID = instanceID
	s.values[id] = value
	return nil
}

func (s *hostServiceTestHostStore) TransitionHost(_ context.Context, id string, from, to host.State, reason string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != from {
		return host.Host{}, invalidHostState(value.State)
	}
	if err := host.ValidateTransition(from, to); err != nil {
		return host.Host{}, err
	}
	value.State = to
	value.StateReason = reason
	if to == host.StateDeleted {
		now := time.Now()
		value.DeletedAt = &now
	}
	value.UpdatedAt = time.Now()
	s.values[id] = value
	return cloneHostServiceHost(value), nil
}

func (s *hostServiceTestHostStore) BeginHostRelease(_ context.Context, id string, confirmProcessTermination bool) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseConfirmed = confirmProcessTermination
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State == host.StateStopping || value.State == host.StateStopped {
		return cloneHostServiceHost(value), false, nil
	}
	if value.State != host.StateRunning || value.ControlState != host.ControlAI {
		return cloneHostServiceHost(value), false, nil
	}
	value.State, value.StateReason = host.StateStopping, "user_release"
	s.values[id] = value
	return cloneHostServiceHost(value), true, nil
}

func (s *hostServiceTestHostStore) CompleteHostRelease(_ context.Context, id string, generation int64) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != host.StateStopping || value.RuntimeGeneration != generation {
		return host.Host{}, invalidHostState(value.State)
	}
	value.State, value.SandboxID, value.RuntimeInstanceID = host.StateStopped, "", ""
	s.values[id] = value
	return cloneHostServiceHost(value), nil
}

func (s *hostServiceTestHostStore) BeginHostRestore(_ context.Context, params RestoreHostParams) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[params.HostID]
	if !ok {
		return host.Host{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State == host.StateCreating && value.RuntimeGeneration > 1 &&
		(value.StateReason == "restore_pending" || value.StateReason == "restore_unknown") {
		return cloneHostServiceHost(value), false, nil
	}
	if value.State == host.StateRunning && value.RuntimeGeneration > 1 {
		return cloneHostServiceHost(value), false, nil
	}
	if value.State != host.StateStopped {
		return host.Host{}, false, invalidHostState(value.State)
	}
	value.State, value.StateReason = host.StateCreating, "restore_pending"
	value.RuntimeGeneration++
	value.RuntimeCredentialEnc = append([]byte(nil), params.RuntimeCredentialEnc...)
	value.ExpiresAt = &params.ExpiresAt
	value.CreateAttemptedAt = nil
	value.SandboxID, value.RuntimeInstanceID = "", ""
	s.values[params.HostID] = value
	return cloneHostServiceHost(value), true, nil
}

func (s *hostServiceTestHostStore) SetHostStateReason(_ context.Context, id string, expected host.State, generation int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != expected || value.RuntimeGeneration != generation {
		return invalidHostState(value.State)
	}
	value.StateReason = reason
	s.values[id] = value
	return nil
}

func (s *hostServiceTestHostStore) SetHostExpiry(_ context.Context, id string, expected host.State, expiresAt time.Time) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.State != expected {
		return host.Host{}, invalidHostState(value.State)
	}
	value.ExpiresAt = &expiresAt
	s.values[id] = value
	return cloneHostServiceHost(value), nil
}

func (s *hostServiceTestHostStore) TouchHosts(context.Context, []string, time.Time) error { return nil }

func (s *hostServiceTestHostStore) addHost(value host.Host) {
	s.mu.Lock()
	s.values[value.ID] = cloneHostServiceHost(value)
	s.mu.Unlock()
	if s.workspaces != nil {
		for _, binding := range value.Workspaces {
			s.workspaces.addBinding(binding)
		}
	}
}

func cloneHostServiceHost(value host.Host) host.Host {
	value.AIOKeyEnc = append([]byte(nil), value.AIOKeyEnc...)
	value.Workspaces = append([]workspace.Binding(nil), value.Workspaces...)
	if value.CreateAttemptedAt != nil {
		copyValue := *value.CreateAttemptedAt
		value.CreateAttemptedAt = &copyValue
	}
	return value
}

type hostServiceTestWorkspaceStore struct {
	mu       sync.Mutex
	values   map[string]workspace.Workspace
	bindings map[string]workspace.Binding
}

func (s *hostServiceTestWorkspaceStore) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) ListWorkspacesByOwner(_ context.Context, ownerID string) ([]workspace.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]workspace.Workspace, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID && value.DeletedAt == nil {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *hostServiceTestWorkspaceStore) TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error) {
	return blockGCTestLock{}, true, nil
}

func (s *hostServiceTestWorkspaceStore) CreateWorkspace(_ context.Context, value workspace.Workspace, record IdempotencyRecord) (workspace.Workspace, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.values[record.ResourceID]; ok {
		return prior, true, nil
	}
	s.values[value.ID] = value
	return value, false, nil
}

func (s *hostServiceTestWorkspaceStore) ClaimWorkspaceForDelete(_ context.Context, id string, expectedRevision int64) (workspace.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if value.State != workspace.StateReady || value.Revision != expectedRevision {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is unavailable")
	}
	for _, binding := range s.bindings {
		if binding.Workspace.ID == id && binding.State != workspace.BindingDetached {
			return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is bound")
		}
	}
	value.State = workspace.StateDeleting
	s.values[id] = value
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) CompleteWorkspaceDelete(_ context.Context, id string, expectedRevision int64) (workspace.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if value.State != workspace.StateDeleting || value.Revision != expectedRevision {
		return workspace.Workspace{}, domain.NewError(domain.CodeInvalidState, "workspace state changed")
	}
	value.State = workspace.StateDeleted
	now := time.Now().UTC()
	value.DeletedAt = &now
	s.values[id] = value
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) GetWorkspaceBindingsByHost(_ context.Context, hostID string) ([]workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]workspace.Binding, 0)
	for _, value := range s.bindings {
		if value.HostID == hostID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *hostServiceTestWorkspaceStore) GetWorkspaceBindingsByWorkspace(_ context.Context, workspaceID string) ([]workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]workspace.Binding, 0)
	for _, value := range s.bindings {
		if value.Workspace.ID == workspaceID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *hostServiceTestWorkspaceStore) GetWorkspaceBinding(_ context.Context, hostID, workspaceID string) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.bindings[hostID+"/"+workspaceID]
	if !ok {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) CreateWorkspaceBinding(_ context.Context, value workspace.Binding) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string]workspace.Binding)
	}
	key := value.HostID + "/" + value.Workspace.ID
	if _, exists := s.bindings[key]; exists {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace binding already exists")
	}
	for _, current := range s.bindings {
		if current.Workspace.ID == value.Workspace.ID && current.State != workspace.BindingDetached {
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace is bound")
		}
	}
	if value.BindingVersion <= 0 {
		value.BindingVersion = 1
	}
	if value.SyncState == "" {
		value.SyncState = workspace.BindingSyncHealthy
	}
	s.bindings[key] = value
	if _, ok := s.values[value.Workspace.ID]; !ok {
		s.values[value.Workspace.ID] = value.Workspace
	}
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) SetWorkspaceBindingSyncStatus(_ context.Context, hostID, workspaceID string, expectedVersion int64, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hostID + "/" + workspaceID
	value, ok := s.bindings[key]
	if !ok {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if value.BindingVersion != expectedVersion || value.State != workspace.BindingActive {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace binding changed")
	}
	if err := workspace.ValidateBindingSyncStatus(state, conflicts); err != nil {
		return workspace.Binding{}, domain.InvalidArgument("sync_status", "workspace sync status is invalid")
	}
	value.SyncState = state
	value.SyncConflicts = append([]string(nil), conflicts...)
	s.bindings[key] = value
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) TransitionWorkspaceBinding(_ context.Context, hostID, workspaceID string, expectedVersion int64, from, to workspace.BindingState, desiredRevision, appliedRevision int64) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hostID + "/" + workspaceID
	value, ok := s.bindings[key]
	if !ok || value.BindingVersion != expectedVersion || value.State != from {
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding state changed")
	}
	if err := workspace.ValidateBindingTransition(from, to); err != nil {
		return workspace.Binding{}, err
	}
	value.State, value.BindingVersion = to, value.BindingVersion+1
	value.DesiredRevision, value.AppliedRevision = desiredRevision, appliedRevision
	if to == workspace.BindingPreparing && (from == workspace.BindingDetached || from == workspace.BindingFailed) {
		value.DesiredRevision = s.values[workspaceID].Revision
	}
	s.bindings[key] = value
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) AdvanceWorkspaceBindingAppliedRevision(_ context.Context, hostID, workspaceID string, expectedVersion, revision int64) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hostID + "/" + workspaceID
	value, ok := s.bindings[key]
	if !ok || value.BindingVersion != expectedVersion || (value.State != workspace.BindingPreparing && value.State != workspace.BindingActive && value.State != workspace.BindingUnbinding) {
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding state changed")
	}
	current := s.values[workspaceID]
	if current.State != workspace.StateReady || revision > current.Revision {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace applied revision is not available")
	}
	if revision > value.AppliedRevision {
		value.AppliedRevision = revision
		s.bindings[key] = value
	}
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) CompleteWorkspaceBindingUnbind(_ context.Context, hostID, workspaceID string, expectedVersion, checkpointRevision int64) (workspace.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hostID + "/" + workspaceID
	value, ok := s.bindings[key]
	current := s.values[workspaceID]
	if !ok || value.BindingVersion != expectedVersion || value.State != workspace.BindingUnbinding {
		return workspace.Binding{}, domain.NewError(domain.CodeInvalidState, "workspace binding state changed")
	}
	if current.State != workspace.StateReady || current.Revision != checkpointRevision || value.DesiredRevision != checkpointRevision {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace changed after the Host checkpoint")
	}
	value.State = workspace.BindingDetached
	value.BindingVersion++
	value.AppliedRevision = checkpointRevision
	s.bindings[key] = value
	return value, nil
}

func (s *hostServiceTestWorkspaceStore) add(value workspace.Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]workspace.Workspace)
	}
	s.values[value.ID] = value
}

func (s *hostServiceTestWorkspaceStore) addBinding(value workspace.Binding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string]workspace.Binding)
	}
	if s.values == nil {
		s.values = make(map[string]workspace.Workspace)
	}
	s.bindings[value.HostID+"/"+value.Workspace.ID] = value
	if _, ok := s.values[value.Workspace.ID]; !ok {
		s.values[value.Workspace.ID] = value.Workspace
	}
}

type hostServiceTestTokens struct{ values []identity.Token }

func (s *hostServiceTestTokens) GetTokenByID(context.Context, string) (identity.Token, error) {
	return identity.Token{}, domain.NewError(domain.CodeNotFound, "token not found")
}
func (s *hostServiceTestTokens) ListTokensByOwner(context.Context, string) ([]identity.Token, error) {
	return nil, nil
}
func (s *hostServiceTestTokens) CreateToken(_ context.Context, value identity.Token) (identity.Token, error) {
	for _, old := range s.values {
		if old.ID == value.ID {
			return identity.Token{}, fmt.Errorf("duplicate token")
		}
	}
	s.values = append(s.values, value)
	return value, nil
}
func (s *hostServiceTestTokens) RevokeToken(context.Context, string, string) error { return nil }
func (s *hostServiceTestTokens) ConsumeTicket(context.Context, string) (Ticket, error) {
	return Ticket{}, identity.ErrInvalidCredential
}

type hostServiceTestVolume struct {
	mu           sync.Mutex
	names        []string
	err          error
	errByName    map[string]error
	beforeDelete func(string) error
}

func (s *hostServiceTestVolume) DeleteVolume(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
	if s.beforeDelete != nil {
		if err := s.beforeDelete(name); err != nil {
			return err
		}
	}
	if err := s.errByName[name]; err != nil {
		return err
	}
	return s.err
}

type hostServiceTestAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (s *hostServiceTestAudit) Write(_ context.Context, value audit.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, value)
	return s.err
}

type hostServiceTestRouter struct{ client SandboxClient }

func (r *hostServiceTestRouter) For(profile string) (SandboxClient, error) {
	if profile != "container" {
		return nil, ErrSandboxUnsupported
	}
	return r.client, nil
}
func (r *hostServiceTestRouter) Profiles() []string { return []string{"container"} }

type hostServiceTestSandbox struct {
	mu             sync.Mutex
	values         map[string]Sandbox
	createCalls    int
	createRequest  SandboxCreateRequest
	deleteCalls    []string
	listErr        error
	createErr      error
	deleteErr      error
	getAfterDelete bool
	renewExpiry    time.Time
}

func (s *hostServiceTestSandbox) Create(_ context.Context, value SandboxCreateRequest) (Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.createRequest = value
	if s.createErr != nil {
		return Sandbox{}, s.createErr
	}
	created := Sandbox{ID: fmt.Sprintf("sandbox-%d", s.createCalls), State: SandboxRunning, Metadata: cloneMetadata(value.Metadata)}
	s.values[created.ID] = created
	return created, nil
}
func (s *hostServiceTestSandbox) Get(_ context.Context, id string) (Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok || (value.State == SandboxTerminated && !s.getAfterDelete) {
		return Sandbox{}, ErrSandboxNotFound
	}
	return value, nil
}
func (s *hostServiceTestSandbox) List(_ context.Context, metadata map[string]string) ([]Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	result := make([]Sandbox, 0)
	for _, value := range s.values {
		match := true
		for key, expected := range metadata {
			if value.Metadata[key] != expected {
				match = false
				break
			}
		}
		if match {
			result = append(result, value)
		}
	}
	return result, nil
}
func (s *hostServiceTestSandbox) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls = append(s.deleteCalls, id)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	value, ok := s.values[id]
	if !ok {
		return ErrSandboxNotFound
	}
	value.State = SandboxTerminated
	s.values[id] = value
	return nil
}
func (s *hostServiceTestSandbox) Renew(_ context.Context, _ string, expiry time.Time) (time.Time, error) {
	s.renewExpiry = expiry
	return expiry, nil
}
func (s *hostServiceTestSandbox) Endpoint(context.Context, string, int) (Upstream, error) {
	return Upstream{BaseURL: "http://sandbox.test", Headers: map[string]string{"x-test": "value"}}, nil
}
func (s *hostServiceTestSandbox) add(value Sandbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[value.ID] = value
}

type hostServiceTestProbe struct {
	upstream Upstream
	err      error
}

func (s *hostServiceTestProbe) Ready(_ context.Context, upstream Upstream) error {
	s.upstream = upstream
	return s.err
}

func cloneMetadata(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
