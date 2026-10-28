package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

func TestWorkerAppDoesNotRetryUnknownRestoreCreate(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.RuntimeGeneration = 2
	value.StateReason = "restore_unknown"
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Hour))
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	metadata := hostServiceMetadata(value.ID)
	metadata["ch-generation"] = "2"
	deps.sandbox.add(Sandbox{ID: "restore-may-appear-late", State: SandboxRunning, Metadata: metadata})
	worker := newWorkerAppTest(t, service)

	ids, err := worker.ListCreatingHosts(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("ListCreatingHosts() for unknown restore = %v, %v", ids, err)
	}
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateCreating || got.StateReason != "restore_unknown" || got.RuntimeGeneration != 2 ||
		deps.sandbox.values["restore-may-appear-late"].State != SandboxRunning || ws.State != workspace.StateReady || len(volume.names) != 0 {
		t.Fatalf("unknown restore was mutated: Host=%#v sandbox=%#v Workspace=%#v volumes=%v", got,
			deps.sandbox.values["restore-may-appear-late"], ws, volume.names)
	}
}

func TestWorkerAppReconcileLostHostReleasesWorkspace(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	attempted := time.Now().Add(-time.Minute)
	value.CreateAttemptedAt = &attempted
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not safely detached") {
		t.Fatalf("Reconcile() error = %v, want a workspace preservation error", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateFailed || got.StateReason != "lost" || got.SandboxID != value.SandboxID || ws.State != workspace.StateReady {
		t.Fatalf("lost host/workspace = %#v / %#v", got, ws)
	}
	if actions["lost"] != 1 || len(deps.audit.entries) != 1 || deps.audit.entries[0].Action != audit.ActionSystemLost {
		t.Fatalf("actions/audit = %#v / %#v", actions, deps.audit.entries)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	if len(volume.names) != 0 {
		t.Fatalf("lost host runtime volume cleanup = %v", volume.names)
	}
}

func TestWorkerAppReconcileFailsHostWithFailedSandbox(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Minute))
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxFailed, Metadata: hostServiceMetadata(value.ID)})
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not safely detached") {
		t.Fatalf("Reconcile() error = %v, want a workspace preservation error", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateFailed || got.StateReason != "orchestrator_error" || got.SandboxID != value.SandboxID || ws.State != workspace.StateReady {
		t.Fatalf("failed host/workspace = %#v / %#v", got, ws)
	}
	if actions["sandbox_failed"] != 1 || deps.sandbox.values[value.SandboxID].State != SandboxFailed ||
		len(deps.audit.entries) != 1 || deps.audit.entries[0].Detail["reason"] != "sandbox_failed" {
		t.Fatalf("actions/sandbox/audit = %#v / %#v / %#v", actions, deps.sandbox.values[value.SandboxID], deps.audit.entries)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	if len(volume.names) != 0 {
		t.Fatalf("failed host runtime volume cleanup = %v", volume.names)
	}
}

func TestWorkerAppReconcileDeletesOnlyOrphansOwnedByInstance(t *testing.T) {
	service, deps := newHostServiceTest(t)
	deps.sandbox.add(Sandbox{ID: "owned-orphan", State: SandboxRunning, Metadata: hostServiceMetadata("h7654321")})
	deps.sandbox.add(Sandbox{ID: "foreign", State: SandboxRunning, Metadata: map[string]string{"ch-env": "other", "ch-host": "h7654321"}})
	volume := service.volumes.(*hostServiceTestVolume)
	volume.beforeDelete = func(string) error {
		if deps.sandbox.values["owned-orphan"].State != SandboxTerminated {
			return errors.New("orphan volume cleanup ran before sandbox cleanup")
		}
		return nil
	}
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actions["orphan_deleted"] != 1 || len(deps.sandbox.deleteCalls) != 1 || deps.sandbox.deleteCalls[0] != "owned-orphan" {
		t.Fatalf("actions/deleted = %#v / %v", actions, deps.sandbox.deleteCalls)
	}
	if len(volume.names) != 3 || volume.names[0] != domain.RuntimeVolumeName("h7654321") ||
		volume.names[1] != domain.ProfileVolumeName("h7654321") || volume.names[2] != domain.HostAppsVolumeName("h7654321") {
		t.Fatalf("orphan runtime volume cleanup = %v", volume.names)
	}
	if len(deps.audit.entries) != 1 || deps.audit.entries[0].Action != audit.ActionOrphanDeleted {
		t.Fatalf("audit entries = %#v", deps.audit.entries)
	}
}

func TestWorkerAppCleansOrphansForAlreadyDeletedHost(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleted)
	deps.hosts.addHost(value)
	deps.sandbox.add(Sandbox{ID: "deleted-host-orphan", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deps.sandbox.values["deleted-host-orphan"].State != SandboxTerminated || len(deps.sandbox.deleteCalls) != 1 {
		t.Fatalf("deleted host orphan sandbox = %#v / %v", deps.sandbox.values["deleted-host-orphan"], deps.sandbox.deleteCalls)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	if len(volume.names) != 3 || volume.names[0] != domain.RuntimeVolumeName(value.ID) ||
		volume.names[1] != domain.ProfileVolumeName(value.ID) || volume.names[2] != domain.HostAppsVolumeName(value.ID) {
		t.Fatalf("deleted host orphan runtime volume cleanup = %v", volume.names)
	}
}

func TestWorkerAppReleasesComputeWithoutDeletingData(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateStopping)
	value.RuntimeGeneration = 2
	value.StateReason = "user_release"
	value.SandboxID = "sandbox-gen2"
	deps.hosts.addHost(value)
	workspaceValue := workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	}
	deps.workspaces.add(workspaceValue)
	metadata := hostServiceMetadata(value.ID)
	metadata["ch-generation"] = "2"
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: metadata})
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateStopped || got.ID != value.ID || got.OwnerID != value.OwnerID || testHostWorkspaceID(got) != testHostWorkspaceID(value) ||
		got.SandboxID != "" || got.RuntimeGeneration != 2 || ws != workspaceValue {
		t.Fatalf("Host/Workspace after release = %#v / %#v", got, ws)
	}
	if actions["release"] != 1 || len(deps.sandbox.deleteCalls) != 1 || deps.sandbox.deleteCalls[0] != value.SandboxID {
		t.Fatalf("release actions/deletes = %#v / %v", actions, deps.sandbox.deleteCalls)
	}
	volume := service.volumes.(*hostServiceTestVolume)
	if len(volume.names) != 0 {
		t.Fatalf("release deleted a data volume: %v", volume.names)
	}
}

func TestWorkerAppWaitsForGatewayActivityLeasesBeforeReleasingCompute(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateStopping)
	value.RuntimeGeneration = 2
	value.StateReason = "user_release"
	value.SandboxID = "sandbox-gen2"
	deps.hosts.addHost(value)
	metadata := hostServiceMetadata(value.ID)
	metadata["ch-generation"] = "2"
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: metadata})
	worker := newWorkerAppTest(t, service)

	deps.hosts.activeLease = true
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deps.sandbox.values[value.SandboxID].State != SandboxRunning || len(deps.sandbox.deleteCalls) != 0 {
		t.Fatalf("Worker deleted Host sandbox before the activity lease drained: %#v / %v", deps.sandbox.values[value.SandboxID], deps.sandbox.deleteCalls)
	}

	deps.hosts.activeLease = false
	actions, err := worker.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateStopped || actions["release"] != 1 || deps.sandbox.values[value.SandboxID].State != SandboxTerminated {
		t.Fatalf("Host after activity lease drained = %#v, actions=%#v, sandbox=%#v", got, actions, deps.sandbox.values[value.SandboxID])
	}
}

func TestWorkerAppFailsClosedWhenHostActivityLeaseCheckFails(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateStopping)
	value.SandboxID = "sandbox-drain-check"
	deps.hosts.addHost(value)
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.hosts.activityLeaseErr = errors.New("database unavailable")
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Worker ignored an unavailable activity lease check")
	}
	if deps.sandbox.values[value.SandboxID].State != SandboxRunning || len(deps.sandbox.deleteCalls) != 0 {
		t.Fatalf("Worker deleted Host sandbox after a failed lease check: %#v / %v", deps.sandbox.values[value.SandboxID], deps.sandbox.deleteCalls)
	}
}

func TestWorkerAppRefusesToReleaseUnexpectedSandboxGeneration(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateStopping)
	value.RuntimeGeneration = 2
	value.StateReason = "user_release"
	value.SandboxID = ""
	deps.hosts.addHost(value)
	metadata := hostServiceMetadata(value.ID)
	metadata["ch-generation"] = "1"
	deps.sandbox.add(Sandbox{ID: "sandbox-old-gen", State: SandboxRunning, Metadata: metadata})
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() released Host with a live sandbox from another generation")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateStopping || deps.sandbox.values["sandbox-old-gen"].State != SandboxRunning || len(volume.names) != 0 {
		t.Fatalf("Host/sandbox/data after refused release = %#v / %#v / %v", got, deps.sandbox.values["sandbox-old-gen"], volume.names)
	}
}

func TestWorkerAppReconcileDeletesHostAfterVerifiedWorkspaceDetach(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: "deleting-host-sandbox", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	volume := service.volumes.(*hostServiceTestVolume)
	volume.beforeDelete = func(string) error {
		if deps.sandbox.values["deleting-host-sandbox"].State != SandboxTerminated {
			return errors.New("volume cleanup ran before sandbox cleanup")
		}
		return nil
	}
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	binding, _ := deps.workspaces.GetWorkspaceBinding(context.Background(), value.ID, testHostWorkspaceID(value))
	if got.State != host.StateDeleted || ws.State != workspace.StateReady || binding.State != workspace.BindingDetached || len(volume.names) != 4 ||
		volume.names[0] != domain.RuntimeVolumeName(value.ID) || volume.names[1] != domain.ProfileVolumeName(value.ID) ||
		volume.names[2] != domain.HostAppsVolumeName(value.ID) || volume.names[3] != domain.WorkspaceRootVolumeName(value.ID) {
		t.Fatalf("host/workspace/binding/volume = %#v / %#v / %#v / %v", got, ws, binding, volume.names)
	}
}

func TestWorkerAppWaitsForHostActivityLeasesBeforeDeletingHost(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.CreateAttemptedAt = ptrTime(time.Now())
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	worker := newWorkerAppTest(t, service)
	volume := service.volumes.(*hostServiceTestVolume)
	deps.hosts.activeLease = true

	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() while a Host activity lease is active: %v", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateDeleting || deps.sandbox.values[value.SandboxID].State != SandboxRunning ||
		len(deps.sandbox.deleteCalls) != 0 || len(volume.names) != 0 {
		t.Fatalf("Worker cleaned Host before activity lease drained: host=%#v sandbox=%#v deletes=%v volumes=%v",
			got, deps.sandbox.values[value.SandboxID], deps.sandbox.deleteCalls, volume.names)
	}

	deps.hosts.activeLease = false
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() after the Host activity lease drained: %v", err)
	}
	got, _ = deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateDeleted || deps.sandbox.values[value.SandboxID].State != SandboxTerminated ||
		len(deps.sandbox.deleteCalls) != 1 || len(volume.names) != 4 {
		t.Fatalf("Host after activity lease drain = %#v, sandbox=%#v, deletes=%v, volumes=%v",
			got, deps.sandbox.values[value.SandboxID], deps.sandbox.deleteCalls, volume.names)
	}
}

func TestWorkerAppFailsClosedWhenHostActivityLeaseCheckFailsBeforeDeletion(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.hosts.activityLeaseErr = errors.New("database unavailable")
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "before deletion") {
		t.Fatalf("Reconcile() error = %v, want activity lease inspection failure", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateDeleting || deps.sandbox.values[value.SandboxID].State != SandboxRunning ||
		len(deps.sandbox.deleteCalls) != 0 || len(volume.names) != 0 {
		t.Fatalf("Worker cleaned Host after a failed lease check: host=%#v sandbox=%#v deletes=%v volumes=%v",
			got, deps.sandbox.values[value.SandboxID], deps.sandbox.deleteCalls, volume.names)
	}
}

func TestWorkerAppRefusesToDeleteHostWithAttachedWorkspaceBeforeCleanup(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "not safely detached") {
		t.Fatalf("Reconcile() error = %v, want attached-workspace preservation error", err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateDeleting || ws.State != workspace.StateReady ||
		deps.sandbox.values[value.SandboxID].State != SandboxRunning || len(deps.sandbox.deleteCalls) != 0 || len(volume.names) != 0 {
		t.Fatalf("host/workspace/sandbox/volumes after refused deletion = %#v / %#v / %#v / %v", got, ws, deps.sandbox.values[value.SandboxID], volume.names)
	}
}

func TestWorkerAppReconcilePreservesWorkspaceForAlreadyDetachedBinding(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateDeleted || ws.State != workspace.StateReady || len(volume.names) != 4 ||
		volume.names[0] != domain.RuntimeVolumeName(value.ID) || volume.names[1] != domain.ProfileVolumeName(value.ID) ||
		volume.names[2] != domain.HostAppsVolumeName(value.ID) || volume.names[3] != domain.WorkspaceRootVolumeName(value.ID) {
		t.Fatalf("host/detached workspace/volume = %#v / %#v / %v", got, ws, volume.names)
	}
}

func TestWorkerAppKeepsDeletingHostUntilRuntimeVolumeCleanupSucceeds(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	worker := newWorkerAppTest(t, service)
	volume := service.volumes.(*hostServiceTestVolume)
	volume.err = errors.New("volume still in use")

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want runtime volume deletion failure")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleting || ws.State != workspace.StateReady || len(volume.names) != 1 ||
		volume.names[0] != domain.RuntimeVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after failure = %#v / %#v / %v", got, ws, volume.names)
	}

	volume.err = nil
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ = deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleted || ws.State != workspace.StateReady || len(volume.names) != 5 ||
		volume.names[1] != domain.RuntimeVolumeName(value.ID) || volume.names[2] != domain.ProfileVolumeName(value.ID) ||
		volume.names[3] != domain.HostAppsVolumeName(value.ID) || volume.names[4] != domain.WorkspaceRootVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after retry = %#v / %#v / %v", got, ws, volume.names)
	}
}

func TestWorkerAppKeepsDeletingHostUntilProfileVolumeCleanupSucceeds(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	worker := newWorkerAppTest(t, service)
	volume := service.volumes.(*hostServiceTestVolume)
	volume.errByName = map[string]error{domain.ProfileVolumeName(value.ID): errors.New("profile volume still in use")}

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want profile volume deletion failure")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleting || ws.State != workspace.StateReady || len(volume.names) != 2 ||
		volume.names[0] != domain.RuntimeVolumeName(value.ID) || volume.names[1] != domain.ProfileVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after profile failure = %#v / %#v / %v", got, ws, volume.names)
	}

	volume.errByName[domain.ProfileVolumeName(value.ID)] = nil
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ = deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleted || ws.State != workspace.StateReady || len(volume.names) != 6 ||
		volume.names[2] != domain.RuntimeVolumeName(value.ID) || volume.names[3] != domain.ProfileVolumeName(value.ID) ||
		volume.names[4] != domain.HostAppsVolumeName(value.ID) || volume.names[5] != domain.WorkspaceRootVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after profile retry = %#v / %#v / %v", got, ws, volume.names)
	}
}

func TestWorkerAppKeepsDeletingHostUntilHostAppsVolumeCleanupSucceeds(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	worker := newWorkerAppTest(t, service)
	volume := service.volumes.(*hostServiceTestVolume)
	volume.errByName = map[string]error{domain.HostAppsVolumeName(value.ID): errors.New("apps volume still in use")}

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want Host apps volume deletion failure")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleting || ws.State != workspace.StateReady || len(volume.names) != 3 ||
		volume.names[2] != domain.HostAppsVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after apps failure = %#v / %#v / %v", got, ws, volume.names)
	}

	volume.errByName[domain.HostAppsVolumeName(value.ID)] = nil
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ = deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if got.State != host.StateDeleted || ws.State != workspace.StateReady || len(volume.names) != 7 ||
		volume.names[3] != domain.RuntimeVolumeName(value.ID) || volume.names[4] != domain.ProfileVolumeName(value.ID) ||
		volume.names[5] != domain.HostAppsVolumeName(value.ID) || volume.names[6] != domain.WorkspaceRootVolumeName(value.ID) {
		t.Fatalf("host/workspace/volume after apps retry = %#v / %#v / %v", got, ws, volume.names)
	}
}

func TestWorkerAppKeepsDeletingHostUntilSandboxCleanupSucceeds(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = "deleting-sandbox"
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.sandbox.deleteErr = ErrSandboxUnavailable
	worker := newWorkerAppTest(t, service)

	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want sandbox cleanup failure")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	volume := service.volumes.(*hostServiceTestVolume)
	if got.State != host.StateDeleting || deps.sandbox.values[value.SandboxID].State != SandboxRunning || len(volume.names) != 0 {
		t.Fatalf("host/sandbox/volumes after sandbox cleanup failure = %#v / %#v / %v", got, deps.sandbox.values[value.SandboxID], volume.names)
	}
}

func TestWorkerAppKeepsIdleHostAndRenewsExpiredHosts(t *testing.T) {
	service, deps := newHostServiceTest(t)
	idle := hostServiceTestHost(host.StateRunning)
	idle.CreatedAt = time.Now().Add(-3 * time.Hour)
	idle.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Hour))
	idle.LastActiveAt = ptrTime(time.Now().Add(-2 * time.Hour))
	deps.hosts.addHost(idle)
	deps.sandbox.add(Sandbox{ID: idle.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(idle.ID)})

	active := hostServiceTestHost(host.StateRunning)
	active.ID, active.SandboxID = "h7654321", "sandbox-active"
	active.Workspaces[0] = testWorkspaceBinding(active.ID, active.OwnerID, "w7654321", workspace.BindingActive)
	active.CreatedAt = time.Now().Add(-2 * time.Minute)
	active.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Hour))
	active.ExpiresAt = ptrTime(time.Now().Add(time.Minute))
	active.LastActiveAt = ptrTime(time.Now().Add(-time.Minute))
	deps.hosts.addHost(active)
	deps.sandbox.add(Sandbox{ID: active.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(active.ID)})
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	idleAfter, _ := deps.hosts.GetHost(context.Background(), idle.ID)
	activeAfter, _ := deps.hosts.GetHost(context.Background(), active.ID)
	if idleAfter.State != host.StateRunning || activeAfter.State != host.StateRunning ||
		idleAfter.ExpiresAt == nil || !idleAfter.ExpiresAt.After(time.Now().Add(7*time.Hour)) ||
		activeAfter.ExpiresAt == nil || !activeAfter.ExpiresAt.After(time.Now().Add(7*time.Hour)) {
		t.Fatalf("reconciled idle/active hosts = %#v / %#v", idleAfter, activeAfter)
	}
	if actions["release"] != 0 || actions["renew"] != 2 {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestWorkerAppReconcileTimeoutDoesNotRaceAnAttemptedCreate(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.CreatedAt = time.Now().Add(-time.Hour)
	value.CreateAttemptedAt = ptrTime(time.Now().Add(-time.Minute))
	deps.hosts.addHost(value)
	worker := newWorkerAppTest(t, service)

	actions, err := worker.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if got.State != host.StateCreating || actions["create_timeout"] != 0 {
		t.Fatalf("host/actions = %#v / %#v", got, actions)
	}
}

func newWorkerAppTest(t *testing.T, service *hostService) *WorkerApp {
	t.Helper()
	runtime, err := NewRuntime(service.hosts, service.sandboxes, service.sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorkerApp(service, runtime, &workerTestOrphanCleanupStore{records: make(map[workerCleanupIdentity]bool)},
		workerAppTestLocks{}, &workerTestModelCallRecovery{}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

type workerAppTestLocks struct{}

type workerTestModelCallRecovery struct {
	count int
	err   error
	calls int
}

func (s *workerTestModelCallRecovery) MarkStaleModelCallsUnknown(context.Context) (int, error) {
	s.calls++
	return s.count, s.err
}

func (workerAppTestLocks) TryLeaderLock(context.Context) (LeaderLock, bool, error) {
	return nil, false, nil
}

func TestWorkerAppReconcileReportsStaleModelCalls(t *testing.T) {
	service, deps := newHostServiceTest(t)
	runtime, err := NewRuntime(service.hosts, service.sandboxes, deps.sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recovery := &workerTestModelCallRecovery{count: 2}
	worker, err := NewWorkerApp(service, runtime, &workerTestOrphanCleanupStore{records: make(map[workerCleanupIdentity]bool)},
		workerAppTestLocks{}, recovery, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := worker.Reconcile(context.Background())
	if err != nil || recovery.calls != 1 || actions["model_call_unknown"] != 2 {
		t.Fatalf("Reconcile() actions=%#v calls=%d err=%v", actions, recovery.calls, err)
	}
}

func TestWorkerAppReconcileReportsStaleModelCallRecoveryError(t *testing.T) {
	service, deps := newHostServiceTest(t)
	runtime, err := NewRuntime(service.hosts, service.sandboxes, deps.sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recovery := &workerTestModelCallRecovery{err: errors.New("ledger unavailable")}
	worker, err := NewWorkerApp(service, runtime, &workerTestOrphanCleanupStore{records: make(map[workerCleanupIdentity]bool)},
		workerAppTestLocks{}, recovery, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := worker.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mark stale model calls unknown: ledger unavailable") || recovery.calls != 1 {
		t.Fatalf("Reconcile() actions=%#v calls=%d err=%v", actions, recovery.calls, err)
	}
}
