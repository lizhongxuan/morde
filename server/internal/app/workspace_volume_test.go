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
	registry   WorkspaceVolumeRegistry
	readyCalls int
	readyErr   error
}

func (s *workspaceVolumeTestStore) GetWorkspaceVolumeRegistry(context.Context, string) (WorkspaceVolumeRegistry, error) {
	return s.registry, nil
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
	ensureCalls int
	verifyCalls int
	ensureErr   error
	verifyErr   error
	lastOwnerID string
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
