package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestHostReplicaAppliesAndActivatesOnlyCurrentBindingRevision(t *testing.T) {
	ctx := context.Background()
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue := hostServiceTestHost(host.StateRunning)
	sealHostServiceTestKey(t, sealer, &hostValue)
	hosts := &hostServiceTestHostStore{values: map[string]host.Host{hostValue.ID: hostValue}}
	workspaceValue := workspace.Workspace{ID: "w1234567", OwnerID: hostValue.OwnerID, Name: "test", State: workspace.StateReady, Revision: 3}
	binding := testWorkspaceBinding(hostValue.ID, hostValue.OwnerID, workspaceValue.ID, workspace.BindingPreparing)
	binding.Workspace = workspaceValue
	binding.DesiredRevision = workspaceValue.Revision
	workspaces := &hostServiceTestWorkspaceStore{
		values:   map[string]workspace.Workspace{workspaceValue.ID: workspaceValue},
		bindings: map[string]workspace.Binding{hostValue.ID + "/" + workspaceValue.ID: binding},
	}
	replicaWorkspaces := &hostReplicaWorkspaceStoreTest{
		hostServiceTestWorkspaceStore: workspaces, generation: hostValue.RuntimeGeneration, running: true,
	}
	users := &credentialUsers{values: map[string]identity.User{hostValue.OwnerID: {ID: hostValue.OwnerID, Role: identity.RoleUser, Status: identity.StatusApproved}}}
	service, err := NewHostReplicaService(hosts, replicaWorkspaces, users, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCredential, err := sealer.Open(hostValue.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	replicaCredential := hostReplicaCredential(runtimeCredential)
	clear(runtimeCredential)

	principal, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, replicaCredential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if _, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration+1, replicaCredential); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Authenticate() with stale generation error = %v", err)
	}
	if _, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, "invalid"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Authenticate() with invalid secret error = %v", err)
	}

	principal, err = service.ScopeWorkspace(ctx, principal, workspaceValue.ID, binding.BindingVersion)
	if err != nil {
		t.Fatalf("ScopeWorkspace() error = %v", err)
	}
	scopedCredential, err := service.CredentialForBinding(ctx, identity.Principal{
		UserID: principal.UserID, Role: principal.Role, Kind: principal.Kind, HostScope: principal.HostScope, HostGeneration: principal.HostGeneration,
	}, workspaceValue.ID, binding.BindingVersion)
	if err != nil {
		t.Fatalf("CredentialForBinding() error = %v", err)
	}
	workspacePrincipal, err := service.AuthenticateWorkspace(ctx, scopedCredential)
	if err != nil || workspacePrincipal.ReplicaWorkspaceID != workspaceValue.ID || workspacePrincipal.BindingVersion != binding.BindingVersion {
		t.Fatalf("AuthenticateWorkspace() = %#v, %v", workspacePrincipal, err)
	}
	if _, err := service.AuthenticateWorkspace(ctx, scopedCredential+"x"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("AuthenticateWorkspace() with modified token error = %v", err)
	}
	if _, err := service.ScopeWorkspace(ctx, principal, workspaceValue.ID, binding.BindingVersion+1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("ScopeWorkspace() with stale binding version error = %v, want workspace conflict", err)
	}
	updated, err := service.RecordAppliedRevision(ctx, principal, workspaceValue.ID, binding.BindingVersion, "p1234567", workspaceValue.Revision)
	if err != nil || updated.AppliedRevision != workspaceValue.Revision || updated.State != workspace.BindingPreparing {
		t.Fatalf("RecordAppliedRevision() = %#v, %v", updated, err)
	}
	if _, err := service.RecordAppliedRevision(ctx, principal, workspaceValue.ID, binding.BindingVersion, "p1234567", workspaceValue.Revision+1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("RecordAppliedRevision() for unavailable revision error = %v", err)
	}
	if _, err := service.RecordAppliedRevision(ctx, principal, workspaceValue.ID, binding.BindingVersion, "", workspaceValue.Revision); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("RecordAppliedRevision() without a session receipt error = %v", err)
	}
	replicaWorkspaces.generation++
	if _, err := service.ActivateBinding(ctx, principal, workspaceValue.ID, binding.BindingVersion); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("ActivateBinding() with a stale generation error = %v", err)
	}
	replicaWorkspaces.generation--
	active, err := service.ActivateBinding(ctx, principal, workspaceValue.ID, binding.BindingVersion)
	if err != nil || active.State != workspace.BindingActive || active.BindingVersion != binding.BindingVersion+1 {
		t.Fatalf("ActivateBinding() = %#v, %v", active, err)
	}
	if _, err := service.ActivateBinding(ctx, principal, workspaceValue.ID, binding.BindingVersion); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("ActivateBinding() with superseded binding version error = %v", err)
	}
	if _, err := service.AuthenticateWorkspace(ctx, scopedCredential); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("AuthenticateWorkspace() with superseded binding credential error = %v, want workspace conflict", err)
	}
	hostValue.State = host.StateDeleting
	hosts.values[hostValue.ID] = hostValue
	if _, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, replicaCredential); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Authenticate() while Host is deleting error = %v", err)
	}
}

func TestHostReplicaReportsWorkspaceSyncConflictAndRecovery(t *testing.T) {
	ctx := context.Background()
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue := hostServiceTestHost(host.StateRunning)
	sealHostServiceTestKey(t, sealer, &hostValue)
	workspaceValue := hostValue.Workspaces[0].Workspace
	workspaceValue.State = workspace.StateReady
	binding := testWorkspaceBinding(hostValue.ID, hostValue.OwnerID, workspaceValue.ID, workspace.BindingActive)
	binding.Workspace = workspaceValue
	binding.BindingVersion = 4
	workspaces := &hostServiceTestWorkspaceStore{
		values:   map[string]workspace.Workspace{workspaceValue.ID: workspaceValue},
		bindings: map[string]workspace.Binding{hostValue.ID + "/" + workspaceValue.ID: binding},
	}
	replicaWorkspaces := &hostReplicaWorkspaceStoreTest{
		hostServiceTestWorkspaceStore: workspaces, generation: hostValue.RuntimeGeneration, running: true,
	}
	users := &credentialUsers{values: map[string]identity.User{hostValue.OwnerID: {ID: hostValue.OwnerID, Role: identity.RoleUser, Status: identity.StatusApproved}}}
	service, err := NewHostReplicaService(&hostServiceTestHostStore{values: map[string]host.Host{hostValue.ID: hostValue}}, replicaWorkspaces, users, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCredential, err := sealer.Open(hostValue.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostReplicaCredential(runtimeCredential))
	clear(runtimeCredential)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = service.ScopeWorkspace(ctx, principal, workspaceValue.ID, binding.BindingVersion)
	if err != nil {
		t.Fatal(err)
	}

	replicaWorkspaces.generation++
	if _, err := service.ReportWorkspaceSyncStatus(ctx, principal, workspace.BindingSyncConflict, []string{"src/main.go"}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("ReportWorkspaceSyncStatus() with a stale generation error = %v", err)
	}
	replicaWorkspaces.generation--
	conflict, err := service.ReportWorkspaceSyncStatus(ctx, principal, workspace.BindingSyncConflict, []string{"src/main.go"})
	if err != nil || conflict.SyncState != workspace.BindingSyncConflict || len(conflict.SyncConflicts) != 1 || conflict.SyncConflicts[0] != "src/main.go" {
		t.Fatalf("ReportWorkspaceSyncStatus(conflict) = %#v, %v", conflict, err)
	}
	healthy, err := service.ReportWorkspaceSyncStatus(ctx, principal, workspace.BindingSyncHealthy, nil)
	if err != nil || healthy.SyncState != workspace.BindingSyncHealthy || len(healthy.SyncConflicts) != 0 {
		t.Fatalf("ReportWorkspaceSyncStatus(healthy) = %#v, %v", healthy, err)
	}
	if _, err := service.ReportWorkspaceSyncStatus(ctx, principal, workspace.BindingSyncConflict, []string{"../secret"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("ReportWorkspaceSyncStatus(unsafe path) error = %v, want invalid argument", err)
	}

	binding.BindingVersion++
	workspaces.addBinding(binding)
	if _, err := service.ReportWorkspaceSyncStatus(ctx, principal, workspace.BindingSyncConflict, []string{"src/main.go"}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("ReportWorkspaceSyncStatus(stale binding) error = %v, want not found", err)
	}
}

func TestHostReplicaCannotRunWhenOwnerIsDisabled(t *testing.T) {
	ctx := context.Background()
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue := hostServiceTestHost(host.StateRunning)
	sealHostServiceTestKey(t, sealer, &hostValue)
	hosts := &hostServiceTestHostStore{values: map[string]host.Host{hostValue.ID: hostValue}}
	users := &credentialUsers{values: map[string]identity.User{hostValue.OwnerID: {ID: hostValue.OwnerID, Status: identity.StatusDisabled}}}
	service, err := NewHostReplicaService(hosts, &hostServiceTestWorkspaceStore{}, users, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCredential, err := sealer.Open(hostValue.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	replicaCredential := hostReplicaCredential(runtimeCredential)
	clear(runtimeCredential)
	if _, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, replicaCredential); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Authenticate() for disabled owner error = %v", err)
	}
}

func TestHostReplicaDetachesOnlyAfterCurrentCheckpoint(t *testing.T) {
	ctx := context.Background()
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue := hostServiceTestHost(host.StateRunning)
	sealHostServiceTestKey(t, sealer, &hostValue)
	workspaceValue := workspace.Workspace{ID: "w1234567", OwnerID: hostValue.OwnerID, Name: "test", State: workspace.StateReady, Revision: 3}
	binding := testWorkspaceBinding(hostValue.ID, hostValue.OwnerID, workspaceValue.ID, workspace.BindingUnbinding)
	binding.Workspace = workspaceValue
	binding.BindingVersion, binding.DesiredRevision, binding.AppliedRevision = 4, 3, 2
	workspaces := &hostServiceTestWorkspaceStore{
		values:   map[string]workspace.Workspace{workspaceValue.ID: workspaceValue},
		bindings: map[string]workspace.Binding{hostValue.ID + "/" + workspaceValue.ID: binding},
	}
	users := &credentialUsers{values: map[string]identity.User{hostValue.OwnerID: {ID: hostValue.OwnerID, Role: identity.RoleUser, Status: identity.StatusApproved}}}
	service, err := NewHostReplicaService(
		&hostServiceTestHostStore{values: map[string]host.Host{hostValue.ID: hostValue}}, workspaces, users, sealer,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCredential, err := sealer.Open(hostValue.RuntimeCredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	replicaCredential := hostReplicaCredential(runtimeCredential)
	clear(runtimeCredential)
	principal, err := service.Authenticate(ctx, hostValue.ID, hostValue.RuntimeGeneration, replicaCredential)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = service.ScopeWorkspace(ctx, principal, workspaceValue.ID, binding.BindingVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteUnbinding(ctx, principal, workspaceValue.ID, binding.BindingVersion, 2); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("CompleteUnbinding(stale checkpoint) error = %v", err)
	}
	if _, err := service.CompleteUnbinding(ctx, principal, workspaceValue.ID, binding.BindingVersion, workspaceValue.Revision); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("CompleteUnbinding(unapplied checkpoint) error = %v; want workspace conflict", err)
	}
	binding.AppliedRevision = workspaceValue.Revision
	workspaces.bindings[hostValue.ID+"/"+workspaceValue.ID] = binding
	detached, err := service.CompleteUnbinding(ctx, principal, workspaceValue.ID, binding.BindingVersion, workspaceValue.Revision)
	if err != nil || detached.State != workspace.BindingDetached || detached.AppliedRevision != workspaceValue.Revision || detached.BindingVersion != binding.BindingVersion+1 {
		t.Fatalf("CompleteUnbinding(current checkpoint) = %#v, %v", detached, err)
	}
}

var _ HostStore = (*hostServiceTestHostStore)(nil)

type hostReplicaWorkspaceStoreTest struct {
	*hostServiceTestWorkspaceStore
	generation int64
	running    bool
}

func (s *hostReplicaWorkspaceStoreTest) ActivateWorkspaceBindingForHost(ctx context.Context, hostID string, expectedGeneration int64,
	workspaceID string, expectedVersion, desiredRevision, appliedRevision int64) (workspace.Binding, error) {
	if expectedGeneration != s.generation {
		return workspace.Binding{}, domain.NewError(domain.CodeUnauthenticated, "Host replica generation is no longer current")
	}
	if !s.running {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "Host must be running to activate a workspace binding")
	}
	return s.TransitionWorkspaceBinding(ctx, hostID, workspaceID, expectedVersion,
		workspace.BindingPreparing, workspace.BindingActive, desiredRevision, appliedRevision)
}

func (s *hostReplicaWorkspaceStoreTest) SetWorkspaceBindingSyncStatusForHost(ctx context.Context, hostID string, expectedGeneration int64,
	workspaceID string, expectedVersion int64, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	if expectedGeneration != s.generation {
		return workspace.Binding{}, domain.NewError(domain.CodeUnauthenticated, "Host replica generation is no longer current")
	}
	if !s.running {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceBusy, "Host must be running to report workspace sync status")
	}
	return s.SetWorkspaceBindingSyncStatus(ctx, hostID, workspaceID, expectedVersion, state, conflicts)
}

var _ HostReplicaWorkspaceStore = (*hostReplicaWorkspaceStoreTest)(nil)
