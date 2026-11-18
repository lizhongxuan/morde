package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
)

func TestWorkspaceVolumeServiceProvisionsPendingAndVerifiesReady(t *testing.T) {
	store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
		WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567", State: WorkspaceVolumePending,
		VolumeName: WorkspaceNamedVolumeName("w1234567"),
	}}
	provisioner := &workspaceVolumeTestProvisioner{}
	service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
	if err != nil {
		t.Fatal(err)
	}

	volume, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi")
	if err != nil {
		t.Fatalf("PrepareHostWorkspaceVolume(pending) = %v", err)
	}
	if volume.ClaimName != "workspace-w1234567" || volume.MountPath != "/home/gem/workspace" || !volume.RequireExisting {
		t.Fatalf("pending volume = %#v", volume)
	}
	if provisioner.ensureCalls != 1 || provisioner.verifyCalls != 0 || store.readyCalls != 1 || store.registry.State != WorkspaceVolumeReady {
		t.Fatalf("provisioner/store state = %#v / %#v", provisioner, store)
	}

	store.registry.OwnerID = "u8765432"
	if _, err := service.PrepareHostWorkspaceVolume(context.Background(), "u8765432", "h8765432", "w1234567", "/home/gem/workspace", "20Gi"); err != nil {
		t.Fatalf("PrepareHostWorkspaceVolume(ready) = %v", err)
	}
	if provisioner.ensureCalls != 1 || provisioner.verifyCalls != 1 || store.readyCalls != 1 {
		t.Fatalf("ready volume unexpectedly reprovisioned: provisioner=%#v store=%#v", provisioner, store)
	}
	if provisioner.lastOwnerID != "u1234567" {
		t.Fatalf("Docker volume verification owner = %q, want immutable provisioned owner", provisioner.lastOwnerID)
	}
}

func TestWorkspaceVolumeServiceUsesStableAgentWorkspaceHostPath(t *testing.T) {
	store := &workspaceVolumeTestStore{}
	provisioner := &workspaceVolumeTestProvisioner{}
	service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &workspaceVolumeHostPathResolverStub{resolution: AgentWorkspaceHostPathResolution{
		IsAgentWorkspace: true,
		HostPath:         "/srv/morde-workspace-sync/current/owner/workspace",
	}}
	if err := service.SetAgentWorkspaceHostPathResolver(resolver); err != nil {
		t.Fatal(err)
	}

	volume, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "")
	if err != nil {
		t.Fatalf("PrepareHostWorkspaceVolume(Agent Workspace) = %v", err)
	}
	if volume.Name != "workspace" || volume.MountPath != "/home/gem/workspace" || volume.HostPath != resolver.resolution.HostPath ||
		volume.ClaimName != "" || volume.Storage != "" || !volume.RequireExisting {
		t.Fatalf("Agent Workspace mount = %#v", volume)
	}
	if resolver.calls != 1 || resolver.lastOwnerID != "u1234567" || resolver.lastWorkspaceID != "w1234567" {
		t.Fatalf("Agent Workspace resolver call = %#v", resolver)
	}
	if store.getCalls != 0 || store.readyCalls != 0 || provisioner.ensureCalls != 0 || provisioner.verifyCalls != 0 {
		t.Fatalf("Agent Workspace unexpectedly used the PVC registry: store=%#v provisioner=%#v", store, provisioner)
	}
}

func TestWorkspaceVolumeServiceRequiresAgentWorkspaceMountAtUserWorkspacePath(t *testing.T) {
	service, err := NewWorkspaceVolumeService(&workspaceVolumeTestStore{}, &workspaceVolumeTestProvisioner{}, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &workspaceVolumeHostPathResolverStub{resolution: AgentWorkspaceHostPathResolution{
		IsAgentWorkspace: true,
		HostPath:         "/srv/morde-workspace-sync/current/owner/workspace",
	}}
	if err := service.SetAgentWorkspaceHostPathResolver(resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/workspace", "20Gi"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("PrepareHostWorkspaceVolume(alternate Agent Workspace path) error = %v, want invalid argument", err)
	}
}

func TestWorkspaceVolumeServiceRejectsInvalidAgentWorkspaceHostPaths(t *testing.T) {
	missingDirectoryErr := errors.New("Agent Workspace current directory is missing")
	tests := []struct {
		name        string
		resolution  AgentWorkspaceHostPathResolution
		resolverErr error
		wantCode    domain.ErrorCode
		wantCause   error
	}{
		{
			name: "missing current directory", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true},
			resolverErr: missingDirectoryErr, wantCause: missingDirectoryErr,
		},
		{
			name: "Host mount gate is closed", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true},
			resolverErr: domain.NewError(domain.CodeWorkspaceUnavailable, "Agent Workspace Host mount is disabled"),
			wantCode:    domain.CodeWorkspaceUnavailable,
		},
		{name: "empty path", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true}, wantCode: domain.CodeInvalidState},
		{name: "relative path", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true, HostPath: "current/workspace"}, wantCode: domain.CodeInvalidState},
		{name: "root path", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true, HostPath: "/"}, wantCode: domain.CodeInvalidState},
		{name: "noncanonical path", resolution: AgentWorkspaceHostPathResolution{IsAgentWorkspace: true, HostPath: "/srv/current/../workspace"}, wantCode: domain.CodeInvalidState},
		{name: "non-Agent result with path", resolution: AgentWorkspaceHostPathResolution{HostPath: "/srv/current/workspace"}, wantCode: domain.CodeInvalidState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
				WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567",
				State: WorkspaceVolumeReady, VolumeName: WorkspaceNamedVolumeName("w1234567"),
			}}
			provisioner := &workspaceVolumeTestProvisioner{}
			service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
			if err != nil {
				t.Fatal(err)
			}
			resolver := &workspaceVolumeHostPathResolverStub{resolution: test.resolution, err: test.resolverErr}
			if err := service.SetAgentWorkspaceHostPathResolver(resolver); err != nil {
				t.Fatal(err)
			}

			if _, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi"); err == nil {
				t.Fatal("PrepareHostWorkspaceVolume() error = nil, want fail-closed result")
			} else if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("PrepareHostWorkspaceVolume() error = %v, want cause %v", err, test.wantCause)
			} else if test.wantCode != "" && domain.CodeOf(err) != test.wantCode {
				t.Fatalf("PrepareHostWorkspaceVolume() error code = %q, want %q: %v", domain.CodeOf(err), test.wantCode, err)
			}
			if store.getCalls != 0 || provisioner.ensureCalls != 0 || provisioner.verifyCalls != 0 {
				t.Fatalf("invalid Agent Workspace path fell back to PVC: store=%#v provisioner=%#v", store, provisioner)
			}
		})
	}
}

func TestWorkspaceVolumeServiceKeepsPVCForNonAgentWorkspace(t *testing.T) {
	store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
		WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567",
		State: WorkspaceVolumeReady, VolumeName: WorkspaceNamedVolumeName("w1234567"),
	}}
	provisioner := &workspaceVolumeTestProvisioner{}
	service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetAgentWorkspaceHostPathResolver(&workspaceVolumeHostPathResolverStub{}); err != nil {
		t.Fatal(err)
	}

	volume, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi")
	if err != nil {
		t.Fatalf("PrepareHostWorkspaceVolume(non-Agent Workspace) = %v", err)
	}
	if volume.HostPath != "" || volume.ClaimName != WorkspaceNamedVolumeName("w1234567") || volume.Storage != "20Gi" || !volume.RequireExisting {
		t.Fatalf("non-Agent Workspace PVC = %#v", volume)
	}
	if store.getCalls != 1 || provisioner.verifyCalls != 1 || provisioner.ensureCalls != 0 {
		t.Fatalf("non-Agent Workspace did not use its registered PVC: store=%#v provisioner=%#v", store, provisioner)
	}
}

func TestWorkspaceVolumeServiceDoesNotDeleteStableAgentWorkspaceDirectory(t *testing.T) {
	store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
		WorkspaceID: "w1234567", OwnerID: "u1234567", State: WorkspaceVolumeReady,
		VolumeName: WorkspaceNamedVolumeName("w1234567"), ProvisionedOwnerID: "u1234567",
	}}
	provisioner := &workspaceVolumeTestProvisioner{}
	service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetAgentWorkspaceHostPathResolver(&workspaceVolumeHostPathResolverStub{resolution: AgentWorkspaceHostPathResolution{
		IsAgentWorkspace: true, HostPath: "/srv/morde-workspace-sync/current/owner/workspace",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := service.ValidateHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567"); err != nil {
		t.Fatalf("ValidateHostWorkspaceVolume(Agent Workspace) = %v", err)
	}
	if err := service.DeleteHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567"); err != nil {
		t.Fatalf("DeleteHostWorkspaceVolume(Agent Workspace) = %v", err)
	}
	if store.getCalls != 0 || provisioner.verifyDeleteCalls != 0 || provisioner.deleteWorkspaceCalls != 0 {
		t.Fatalf("Agent Workspace directory was routed through PVC cleanup: store=%#v provisioner=%#v", store, provisioner)
	}
}

func TestWorkspaceVolumeServiceFailsClosedForUnmappedLegacyWorkspace(t *testing.T) {
	for _, legacyHostID := range []string{"", "h8765432"} {
		store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
			WorkspaceID: "w1234567", OwnerID: "u1234567", State: WorkspaceVolumeLegacy, LegacyHostID: legacyHostID,
		}}
		provisioner := &workspaceVolumeTestProvisioner{}
		service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("PrepareHostWorkspaceVolume(legacy host %q) error = %v, want Workspace conflict", legacyHostID, err)
		}
		if provisioner.ensureCalls != 0 || provisioner.verifyCalls != 0 || store.readyCalls != 0 {
			t.Fatalf("unmapped legacy volume caused storage mutation: provisioner=%#v store=%#v", provisioner, store)
		}
	}
}

func TestWorkspaceVolumeServiceUsesOnlyExactLegacyHostVolume(t *testing.T) {
	store := &workspaceVolumeTestStore{registry: WorkspaceVolumeRegistry{
		WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567", State: WorkspaceVolumeLegacy, LegacyHostID: "h1234567",
	}}
	service, err := NewWorkspaceVolumeService(store, &workspaceVolumeTestProvisioner{}, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	volume, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi")
	if err != nil {
		t.Fatal(err)
	}
	if volume.ClaimName != domain.WorkspaceRootVolumeName("h1234567") || !volume.RequireExisting {
		t.Fatalf("legacy volume = %#v", volume)
	}
}

func TestWorkspaceVolumeServiceRejectsSharedLegacyHostRootForDeletion(t *testing.T) {
	store := &workspaceVolumeTestStore{
		registry: WorkspaceVolumeRegistry{
			WorkspaceID: "w1234567", OwnerID: "u1234567", State: WorkspaceVolumeLegacy, LegacyHostID: "h1234567",
		},
		legacyVolumes: []WorkspaceVolumeRegistry{{
			WorkspaceID: "w7654321", OwnerID: "u1234567", State: WorkspaceVolumeLegacy, LegacyHostID: "h1234567",
		}},
	}
	service, err := NewWorkspaceVolumeService(store, &workspaceVolumeTestProvisioner{}, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ValidateHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("ValidateHostWorkspaceVolume(shared legacy root) error = %v, want workspace conflict", err)
	}
}

func TestWorkspaceVolumeServiceRejectsIdentityAndProvisionFailures(t *testing.T) {
	tests := []struct {
		name      string
		registry  WorkspaceVolumeRegistry
		ensureErr error
		readyErr  error
		markErr   error
	}{
		{
			name: "owner mismatch",
			registry: WorkspaceVolumeRegistry{WorkspaceID: "w1234567", OwnerID: "u8765432", State: WorkspaceVolumePending,
				VolumeName: WorkspaceNamedVolumeName("w1234567")},
		},
		{
			name: "pending name mismatch",
			registry: WorkspaceVolumeRegistry{WorkspaceID: "w1234567", OwnerID: "u1234567", State: WorkspaceVolumePending,
				VolumeName: "workspace-other"},
		},
		{
			name: "docker provision failure",
			registry: WorkspaceVolumeRegistry{WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567", State: WorkspaceVolumePending,
				VolumeName: WorkspaceNamedVolumeName("w1234567")},
			ensureErr: errors.New("Docker unavailable"),
		},
		{
			name: "ready registration failure",
			registry: WorkspaceVolumeRegistry{WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567", State: WorkspaceVolumePending,
				VolumeName: WorkspaceNamedVolumeName("w1234567")},
			markErr: errors.New("database unavailable"),
		},
		{
			name: "ready volume missing",
			registry: WorkspaceVolumeRegistry{WorkspaceID: "w1234567", OwnerID: "u1234567", ProvisionedOwnerID: "u1234567", State: WorkspaceVolumeReady,
				VolumeName: WorkspaceNamedVolumeName("w1234567")},
			readyErr: errors.New("registered Workspace volume is missing"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &workspaceVolumeTestStore{registry: test.registry, readyErr: test.markErr}
			provisioner := &workspaceVolumeTestProvisioner{ensureErr: test.ensureErr, verifyErr: test.readyErr}
			service, err := NewWorkspaceVolumeService(store, provisioner, "instance-a")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.PrepareHostWorkspaceVolume(context.Background(), "u1234567", "h1234567", "w1234567", "/home/gem/workspace", "20Gi"); err == nil {
				t.Fatal("PrepareHostWorkspaceVolume() error = nil, want fail-closed result")
			}
			wantReadyCalls := 0
			if test.markErr != nil {
				wantReadyCalls = 1
			}
			if store.readyCalls != wantReadyCalls {
				t.Fatalf("ready transition calls = %d, want %d", store.readyCalls, wantReadyCalls)
			}
		})
	}
}

type workspaceVolumeTestStore struct {
	registry      WorkspaceVolumeRegistry
	legacyVolumes []WorkspaceVolumeRegistry
	getCalls      int
	readyCalls    int
	readyErr      error
}

func (s *workspaceVolumeTestStore) GetWorkspaceVolumeRegistry(context.Context, string) (WorkspaceVolumeRegistry, error) {
	s.getCalls++
	return s.registry, nil
}

func (s *workspaceVolumeTestStore) ListLegacyHostWorkspaceVolumes(_ context.Context, hostID string) ([]WorkspaceVolumeRegistry, error) {
	values := append([]WorkspaceVolumeRegistry(nil), s.legacyVolumes...)
	if s.registry.State == WorkspaceVolumeLegacy && s.registry.LegacyHostID == hostID {
		values = append(values, s.registry)
	}
	return values, nil
}

func (s *workspaceVolumeTestStore) MarkWorkspaceVolumeReady(_ context.Context, ownerID, workspaceID, volumeName string) error {
	s.readyCalls++
	if s.readyErr != nil {
		return s.readyErr
	}
	if s.registry.OwnerID != ownerID || s.registry.WorkspaceID != workspaceID || s.registry.VolumeName != volumeName || s.registry.State != WorkspaceVolumePending {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace volume registry changed")
	}
	s.registry.State = WorkspaceVolumeReady
	return nil
}

type workspaceVolumeTestProvisioner struct {
	ensureCalls          int
	verifyCalls          int
	verifyDeleteCalls    int
	deleteCalls          int
	deleteWorkspaceCalls int
	ensureErr            error
	verifyErr            error
	deleteErr            error
	lastOwnerID          string
}

func (p *workspaceVolumeTestProvisioner) EnsureWorkspaceVolume(_ context.Context, _, _, ownerID, _ string) error {
	p.ensureCalls++
	p.lastOwnerID = ownerID
	return p.ensureErr
}

func (p *workspaceVolumeTestProvisioner) VerifyWorkspaceVolume(_ context.Context, _, _, ownerID, _ string) error {
	p.verifyCalls++
	p.lastOwnerID = ownerID
	return p.verifyErr
}

func (p *workspaceVolumeTestProvisioner) VerifyWorkspaceVolumeForDelete(context.Context, string, string, string, string) error {
	p.verifyDeleteCalls++
	return p.verifyErr
}

func (p *workspaceVolumeTestProvisioner) DeleteWorkspaceVolume(context.Context, string, string, string, string) error {
	p.deleteCalls++
	p.deleteWorkspaceCalls++
	return p.deleteErr
}

type workspaceVolumeHostPathResolverStub struct {
	resolution      AgentWorkspaceHostPathResolution
	err             error
	calls           int
	lastOwnerID     string
	lastWorkspaceID string
}

func (r *workspaceVolumeHostPathResolverStub) ResolveAgentWorkspaceHostPath(_ context.Context, ownerID, workspaceID string) (AgentWorkspaceHostPathResolution, error) {
	r.calls++
	r.lastOwnerID = ownerID
	r.lastWorkspaceID = workspaceID
	return r.resolution, r.err
}
