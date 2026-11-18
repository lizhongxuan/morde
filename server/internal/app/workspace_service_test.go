package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceServiceStore struct {
	agentInitLock sync.Mutex
	WorkspaceStore
	values        map[string]workspace.Workspace
	agentByOwner  map[string]workspace.Workspace
	agentInit     map[string]bool
	agentHistory  map[string]string
	agentCreates  int
	bindings      map[string][]workspace.Binding
	volumes       map[string]WorkspaceVolumeRegistry
	byIdempotency map[string]workspace.Workspace
	createRecord  IdempotencyRecord
	createErr     error
	claimErr      error
	claimCalls    int
	completeCalls int
	createReplay  bool
}

func (s *workspaceServiceStore) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

func (s *workspaceServiceStore) ListWorkspacesByOwner(_ context.Context, ownerID string) ([]workspace.Workspace, error) {
	values := make([]workspace.Workspace, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID && value.DeletedAt == nil {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *workspaceServiceStore) GetWorkspaceBindingsByWorkspace(_ context.Context, workspaceID string) ([]workspace.Binding, error) {
	return s.bindings[workspaceID], nil
}

func (s *workspaceServiceStore) GetWorkspaceBindingsByHost(_ context.Context, hostID string) ([]workspace.Binding, error) {
	values := make([]workspace.Binding, 0)
	for _, bindings := range s.bindings {
		for _, binding := range bindings {
			if binding.HostID == hostID {
				values = append(values, binding)
			}
		}
	}
	return values, nil
}

func (s *workspaceServiceStore) GetWorkspaceVolumeRegistry(_ context.Context, workspaceID string) (WorkspaceVolumeRegistry, error) {
	value, ok := s.volumes[workspaceID]
	if !ok {
		return WorkspaceVolumeRegistry{}, domain.NewError(domain.CodeNotFound, "workspace volume not found")
	}
	return value, nil
}

func (s *workspaceServiceStore) ListLegacyHostWorkspaceVolumes(_ context.Context, hostID string) ([]WorkspaceVolumeRegistry, error) {
	values := make([]WorkspaceVolumeRegistry, 0)
	for _, value := range s.volumes {
		if value.State == WorkspaceVolumeLegacy && value.LegacyHostID == hostID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *workspaceServiceStore) MarkWorkspaceVolumeReady(_ context.Context, ownerID, workspaceID, volumeName string) error {
	value, ok := s.volumes[workspaceID]
	if !ok || value.OwnerID != ownerID || value.State != WorkspaceVolumePending || value.VolumeName != volumeName {
		return domain.NewError(domain.CodeConflict, "workspace volume registration changed")
	}
	value.State = WorkspaceVolumeReady
	s.volumes[workspaceID] = value
	return nil
}

func (s *workspaceServiceStore) CreateWorkspace(_ context.Context, value workspace.Workspace, record IdempotencyRecord) (workspace.Workspace, bool, error) {
	s.createRecord = record
	if s.createErr != nil {
		return workspace.Workspace{}, false, s.createErr
	}
	if existing, ok := s.byIdempotency[record.OwnerID+"\x00"+record.Operation+"\x00"+record.Key]; ok {
		return existing, true, nil
	}
	if existing, ok := s.values[record.ResourceID]; ok {
		return existing, true, nil
	}
	s.values[value.ID] = value
	if s.byIdempotency == nil {
		s.byIdempotency = make(map[string]workspace.Workspace)
	}
	s.byIdempotency[record.OwnerID+"\x00"+record.Operation+"\x00"+record.Key] = value
	return value, s.createReplay, nil
}

func (s *workspaceServiceStore) GetAgentWorkspace(_ context.Context, ownerID string) (workspace.Workspace, bool, error) {
	value, ok := s.agentByOwner[ownerID]
	if !ok {
		return workspace.Workspace{}, false, domain.NewError(domain.CodeNotFound, "Agent Workspace not found")
	}
	return value, s.agentInit[ownerID], nil
}

func (s *workspaceServiceStore) CreateAgentWorkspace(_ context.Context, value workspace.Workspace) (workspace.Workspace, bool, error) {
	if s.agentByOwner == nil {
		s.agentByOwner = make(map[string]workspace.Workspace)
	}
	if existing, ok := s.agentByOwner[value.OwnerID]; ok {
		return existing, s.agentInit[value.OwnerID], nil
	}
	s.agentCreates++
	s.agentByOwner[value.OwnerID] = value
	if s.agentHistory == nil {
		s.agentHistory = make(map[string]string)
	}
	s.agentHistory[value.ID] = value.OwnerID
	if s.values == nil {
		s.values = make(map[string]workspace.Workspace)
	}
	s.values[value.ID] = value
	return value, false, nil
}

func (s *workspaceServiceStore) MarkAgentWorkspaceDirectoryInitialized(_ context.Context, ownerID, workspaceID string) error {
	value, ok := s.agentByOwner[ownerID]
	if !ok || value.ID != workspaceID {
		return domain.NewError(domain.CodeWorkspaceConflict, "Agent Workspace mapping changed")
	}
	if s.agentInit == nil {
		s.agentInit = make(map[string]bool)
	}
	s.agentInit[ownerID] = true
	return nil
}

func (s *workspaceServiceStore) AcquireAgentWorkspaceInitializationLock(context.Context, string) (func() error, error) {
	s.agentInitLock.Lock()
	return func() error {
		s.agentInitLock.Unlock()
		return nil
	}, nil
}

func (s *workspaceServiceStore) IsAgentWorkspaceWorkspace(_ context.Context, ownerID, workspaceID string) (bool, error) {
	return s.agentHistory[workspaceID] == ownerID, nil
}

func (s *workspaceServiceStore) TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error) {
	return blockGCTestLock{}, true, nil
}

func (s *workspaceServiceStore) ClaimWorkspaceForDelete(_ context.Context, id string, expectedRevision int64) (workspace.Workspace, error) {
	s.claimCalls++
	if s.claimErr != nil {
		return workspace.Workspace{}, s.claimErr
	}
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if value.Revision != expectedRevision || value.State != workspace.StateReady {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace state changed")
	}
	value.State = workspace.StateDeleting
	s.values[id] = value
	return value, nil
}

func (s *workspaceServiceStore) CompleteWorkspaceDelete(_ context.Context, id string, expectedRevision int64) (workspace.Workspace, error) {
	s.completeCalls++
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

func (s *workspaceServiceStore) ClaimAgentWorkspaceForDelete(ctx context.Context, ownerID, id string, expectedRevision int64) (workspace.Workspace, error) {
	value, ok := s.agentByOwner[ownerID]
	if !ok || value.ID != id {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceConflict, "Agent Workspace mapping is no longer current")
	}
	return s.ClaimWorkspaceForDelete(ctx, id, expectedRevision)
}

func (s *workspaceServiceStore) CompleteAgentWorkspaceDelete(ctx context.Context, ownerID, id string, expectedRevision int64) (workspace.Workspace, error) {
	value, err := s.CompleteWorkspaceDelete(ctx, id, expectedRevision)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if current, ok := s.agentByOwner[ownerID]; ok && current.ID == id {
		delete(s.agentByOwner, ownerID)
		delete(s.agentInit, ownerID)
	}
	return value, nil
}

type workspaceContentStub struct {
	err     error
	deletes []string
}

func (s *workspaceContentStub) DeleteWorkspace(id string) error {
	s.deletes = append(s.deletes, id)
	return s.err
}

type workspaceCurrentDeleterStub struct {
	err     error
	deletes []string
}

func (s *workspaceCurrentDeleterStub) Delete(_ context.Context, ownerID, workspaceID string) error {
	s.deletes = append(s.deletes, ownerID+":"+workspaceID)
	return s.err
}

type workspaceCapacityStub struct {
	err   error
	calls int
}

func (s *workspaceCapacityStub) CheckCapacity(context.Context) error {
	s.calls++
	return s.err
}

func TestWorkspaceServiceCreateUsesIndependentQuotaAndIdempotency(t *testing.T) {
	store := &workspaceServiceStore{values: make(map[string]workspace.Workspace)}
	capacity := &workspaceCapacityStub{}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, capacity, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var initializedWorkspaceID string
	if err := service.SetWorkspaceCurrentInitializer(func(_ context.Context, workspaceID string) error {
		initializedWorkspaceID = workspaceID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	created, err := service.CreateWorkspace(context.Background(), principal, CreateWorkspaceCommand{Name: "  code  "}, "retry-key-workspace-001")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidWorkspaceID(created.ID) || created.OwnerID != principal.UserID || created.Name != "code" || created.State != workspace.StateReady || created.QuotaBytes != 1<<30 || created.UsedBytes != 0 {
		t.Fatalf("CreateWorkspace() = %#v", created)
	}
	if store.values[created.ID].QuotaBytes != 1<<30 || store.createRecord.Operation != "workspace.create" || store.createRecord.Key != "retry-key-workspace-001" || len(store.createRecord.RequestHash) != 32 || store.createRecord.ResourceID != created.ID || capacity.calls != 1 || initializedWorkspaceID != created.ID {
		t.Fatalf("create contract = %#v, workspace=%#v, capacity calls=%d, initialized=%q", store.createRecord, store.values[created.ID], capacity.calls, initializedWorkspaceID)
	}
	if _, err := service.CreateWorkspace(context.Background(), identity.Principal{UserID: principal.UserID, Kind: identity.KindAPIKey, Role: identity.RoleUser}, CreateWorkspaceCommand{Name: "code"}, "retry-key-workspace-002"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key CreateWorkspace() error = %v", err)
	}
}

func TestWorkspaceServiceCreateReportsCurrentDirectoryInitializationFailure(t *testing.T) {
	store := &workspaceServiceStore{values: make(map[string]workspace.Workspace)}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	initializerErr := errors.New("current directory unavailable")
	if err := service.SetWorkspaceCurrentInitializer(func(context.Context, string) error { return initializerErr }); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.CreateWorkspace(context.Background(), principal, CreateWorkspaceCommand{Name: "code"}, "retry-key-workspace-initializer"); !errors.Is(err, initializerErr) {
		t.Fatalf("CreateWorkspace() error = %v, want initializer error", err)
	}
	if len(store.values) != 1 {
		t.Fatalf("Workspace row count after failed materialization = %d, want 1", len(store.values))
	}
}

func TestWorkspaceServiceEnsureAgentWorkspaceIsStableAndInitializesOnce(t *testing.T) {
	store := &workspaceServiceStore{
		values: map[string]workspace.Workspace{
			"w1234567": {ID: "w1234567", OwnerID: "u1234567", Name: "Agent Workspace", State: workspace.StateReady},
		},
		bindings: map[string][]workspace.Binding{},
	}
	capacity := &workspaceCapacityStub{}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, capacity, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var initialized []string
	if err := service.SetWorkspaceCurrentInitializer(func(_ context.Context, id string) error {
		initialized = append(initialized, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	first, err := service.EnsureAgentWorkspace(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureAgentWorkspace(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Name != "Agent Workspace" || store.agentCreates != 1 || capacity.calls != 1 ||
		len(initialized) != 1 || initialized[0] != first.ID {
		t.Fatalf("EnsureAgentWorkspace() = %#v then %#v; creates=%d capacity=%d initialized=%v",
			first, second, store.agentCreates, capacity.calls, initialized)
	}
	if _, exists := store.values["w1234567"]; !exists {
		t.Fatal("existing same-named user Workspace was lost")
	}
}

func TestWorkspaceServiceConcurrentEnsureUsesOneStableWorkspaceAndDirectory(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{}, bindings: map[string][]workspace.Binding{}}
	capacity := &workspaceCapacityStub{}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, capacity, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentInitializer(func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var wait sync.WaitGroup
	errorsOut := make(chan error, callers)
	ids := make(chan string, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := service.EnsureAgentWorkspace(context.Background(), "u1234567")
			if err != nil {
				errorsOut <- err
				return
			}
			ids <- value.ID
		}()
	}
	wait.Wait()
	close(errorsOut)
	close(ids)
	for err := range errorsOut {
		t.Fatalf("concurrent EnsureAgentWorkspace() error = %v", err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("concurrent EnsureAgentWorkspace() returned IDs %q and %q", first, id)
		}
	}
	if store.agentCreates != 1 || capacity.calls != 1 || !store.agentInit["u1234567"] {
		t.Fatalf("concurrent ensure creates=%d capacity=%d initialized=%t", store.agentCreates, capacity.calls, store.agentInit["u1234567"])
	}
}

func TestWorkspaceServiceEnsureAgentWorkspaceRetriesCurrentDirectoryInitialization(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{}, bindings: map[string][]workspace.Binding{}}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	initErr := errors.New("current directory unavailable")
	if err := service.SetWorkspaceCurrentInitializer(func(context.Context, string) error { return initErr }); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureAgentWorkspace(context.Background(), "u1234567"); !errors.Is(err, initErr) {
		t.Fatalf("first EnsureAgentWorkspace() error = %v, want initializer failure", err)
	}
	if store.agentCreates != 1 || len(store.agentByOwner) != 1 {
		t.Fatalf("failed initialization lost stable mapping: creates=%d mappings=%d", store.agentCreates, len(store.agentByOwner))
	}
	if err := service.SetWorkspaceCurrentInitializer(func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureAgentWorkspace(context.Background(), "u1234567"); err != nil || store.agentCreates != 1 {
		t.Fatalf("retry EnsureAgentWorkspace() = %v, creates=%d; want same mapping and successful initialization", err, store.agentCreates)
	}
}

func TestWorkspaceServiceCapacityAndInputValidation(t *testing.T) {
	capacityErr := domain.NewError(domain.CodeCapacityExceeded, "workspace storage is below its reserve")
	store := &workspaceServiceStore{values: make(map[string]workspace.Workspace)}
	capacity := &workspaceCapacityStub{err: capacityErr}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, capacity, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.CreateWorkspace(context.Background(), principal, CreateWorkspaceCommand{Name: "code"}, "retry-key-workspace-003"); !errors.Is(err, capacityErr) {
		t.Fatalf("capacity failure = %v", err)
	}
	if len(store.values) != 0 {
		t.Fatalf("created after capacity failure: %#v", store.values)
	}
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: " ", key: "retry-key-workspace-004"},
		{name: "valid", key: "bad"},
	} {
		if _, err := service.CreateWorkspace(context.Background(), principal, CreateWorkspaceCommand{Name: test.name}, test.key); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("CreateWorkspace(name=%q,key=%q) error = %v", test.name, test.key, err)
		}
	}
}

func TestWorkspaceServiceListsGetsAndHidesForeignOrDeleted(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{
		"w1234567": {ID: "w1234567", OwnerID: "u1234567", Name: "mine", State: workspace.StateReady},
		"w7654321": {ID: "w7654321", OwnerID: "u7654321", Name: "other", State: workspace.StateReady},
		"w8765432": {ID: "w8765432", OwnerID: "u1234567", State: workspace.StateDeleted},
	}}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	items, err := service.ListWorkspaces(context.Background(), principal)
	if err != nil || len(items) != 1 || items[0].ID != "w1234567" {
		t.Fatalf("ListWorkspaces() = %#v, %v", items, err)
	}
	if value, err := service.GetWorkspace(context.Background(), principal, "w1234567"); err != nil || value.Name != "mine" {
		t.Fatalf("GetWorkspace() = %#v, %v", value, err)
	}
	for _, id := range []string{"w7654321", "w8765432"} {
		if _, err := service.GetWorkspace(context.Background(), principal, id); domain.CodeOf(err) != domain.CodeNotFound {
			t.Errorf("GetWorkspace(%s) error = %v", id, err)
		}
	}
}

func TestWorkspaceServiceListsReadyOwnedWorkspaceMetadataForDevice(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{
		"w1234567": {ID: "w1234567", OwnerID: "u1234567", Name: "granted", State: workspace.StateReady, Revision: 3},
		"w2345678": {ID: "w2345678", OwnerID: "u1234567", Name: "ungranted", State: workspace.StateReady, Revision: 4},
		"w3456789": {ID: "w3456789", OwnerID: "u7654321", Name: "foreign", State: workspace.StateReady, Revision: 5},
		"w4567890": {ID: "w4567890", OwnerID: "u1234567", Name: "deleted", State: workspace.StateDeleted, Revision: 6},
	}}
	service, err := NewWorkspaceService(store, &workspaceContentStub{}, &workspaceCurrentDeleterStub{}, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindDevice,
		ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	items, err := service.ListDeviceWorkspaces(context.Background(), principal)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListDeviceWorkspaces() = %#v, %v", items, err)
	}
	principal.ClientInstallationID = ""
	items, err = service.ListDeviceWorkspaces(context.Background(), principal)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("ListDeviceWorkspaces(unbound installation) = %#v, %v", items, err)
	}
	principal.Scopes = nil
	if _, err := service.ListDeviceWorkspaces(context.Background(), principal); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("ListDeviceWorkspaces(without scope) error = %v", err)
	}
	principal.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
	principal.Kind = identity.KindSession
	if _, err := service.ListDeviceWorkspaces(context.Background(), principal); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("ListDeviceWorkspaces(session) error = %v", err)
	}
}

func TestWorkspaceServiceDeleteClaimsBeforeDeletingBodyAndCanRetry(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{
		"w1234567": {ID: "w1234567", OwnerID: "u1234567", Name: "code", State: workspace.StateReady, Revision: 7},
	}}
	content := &workspaceContentStub{err: errors.New("disk unavailable")}
	current := &workspaceCurrentDeleterStub{err: errors.New("current directory unavailable")}
	auditor := &credentialAudit{}
	service, err := NewWorkspaceService(store, content, current, &workspaceCapacityStub{}, auditor, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.DeleteWorkspace(context.Background(), principal, "w1234567"); !errors.Is(err, content.err) {
		t.Fatalf("DeleteWorkspace() error = %v", err)
	}
	if store.values["w1234567"].State != workspace.StateDeleting || store.claimCalls != 1 || store.completeCalls != 0 {
		t.Fatalf("state/calls after failed content delete = %#v, %d, %d", store.values["w1234567"], store.claimCalls, store.completeCalls)
	}
	content.err = nil
	if _, err := service.DeleteWorkspace(context.Background(), principal, "w1234567"); !errors.Is(err, current.err) || store.completeCalls != 0 {
		t.Fatalf("DeleteWorkspace() without current-directory cleanup error = %v, complete calls=%d", err, store.completeCalls)
	}
	current.err = nil
	deleted, err := service.DeleteWorkspace(context.Background(), principal, "w1234567")
	if err != nil || deleted.State != workspace.StateDeleted || deleted.DeletedAt == nil || store.claimCalls != 1 || store.completeCalls != 1 {
		t.Fatalf("retry DeleteWorkspace() = %#v, claim=%d complete=%d, err=%v", deleted, store.claimCalls, store.completeCalls, err)
	}
	if len(content.deletes) != 3 || len(current.deletes) != 2 || current.deletes[0] != "u1234567:w1234567" || len(auditor.entries) != 1 || auditor.entries[0].Action != audit.ActionWorkspaceDelete {
		t.Fatalf("content/current deletes/audit = %#v / %#v / %#v", content.deletes, current.deletes, auditor.entries)
	}
}

func TestWorkspaceServiceDeleteDoesNotTouchContentWhenStoreRejectsBoundWorkspace(t *testing.T) {
	store := &workspaceServiceStore{values: map[string]workspace.Workspace{
		"w1234567": {ID: "w1234567", OwnerID: "u1234567", State: workspace.StateReady},
	}, claimErr: domain.NewError(domain.CodeWorkspaceBusy, "workspace is bound")}
	content := &workspaceContentStub{}
	service, err := NewWorkspaceService(store, content, &workspaceCurrentDeleterStub{}, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.DeleteWorkspace(context.Background(), principal, "w1234567"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("DeleteWorkspace() error = %v", err)
	}
	if len(content.deletes) != 0 {
		t.Fatalf("deleted referenced workspace body: %#v", content.deletes)
	}
}

func TestWorkspaceServiceOnlyHostDeleteCanDeleteDetachedBoundStorage(t *testing.T) {
	const hostID = "h1234567"
	const workspaceID = "w1234567"
	value := workspace.Workspace{ID: workspaceID, OwnerID: "u1234567", Name: "Computer storage", State: workspace.StateReady, Revision: 4}
	binding := workspace.Binding{
		ID: "b1234567", OwnerID: value.OwnerID, HostID: hostID, Workspace: value,
		Directory: workspaceID, State: workspace.BindingDetached, BindingVersion: 2,
	}
	store := &workspaceServiceStore{
		values:   map[string]workspace.Workspace{workspaceID: value},
		bindings: map[string][]workspace.Binding{workspaceID: {binding}},
		volumes: map[string]WorkspaceVolumeRegistry{workspaceID: {
			WorkspaceID: workspaceID, OwnerID: value.OwnerID, ProvisionedOwnerID: value.OwnerID,
			State: WorkspaceVolumeReady, VolumeName: WorkspaceNamedVolumeName(workspaceID),
		}},
	}
	content := &workspaceContentStub{}
	current := &workspaceCurrentDeleterStub{}
	volume := &workspaceVolumeTestProvisioner{}
	volumeService, err := NewWorkspaceVolumeService(store, volume, "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWorkspaceService(store, content, current, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetHostWorkspaceVolumes(volumeService); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: value.OwnerID, TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.DeleteWorkspace(context.Background(), principal, workspaceID); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("direct DeleteWorkspace(detached Host storage) error = %v, want busy", err)
	}
	if err := service.DeleteHostWorkspace(context.Background(), value.OwnerID, hostID); err != nil {
		t.Fatalf("DeleteHostWorkspace() = %v", err)
	}
	deleted, err := store.GetWorkspace(context.Background(), workspaceID)
	if err != nil || deleted.State != workspace.StateDeleted || deleted.DeletedAt == nil || volume.deleteCalls != 1 ||
		len(content.deletes) != 1 || len(current.deletes) != 1 {
		t.Fatalf("Host storage deletion = %#v, volume deletes=%d content=%v current=%v err=%v", deleted, volume.deleteCalls, content.deletes, current.deletes, err)
	}
}

func TestWorkspaceServiceHostDeleteRetainsAgentWorkspace(t *testing.T) {
	value := workspace.Workspace{ID: "w1234567", OwnerID: "u1234567", Name: "Agent Workspace", State: workspace.StateReady}
	binding := workspace.Binding{
		ID: "b1234567", OwnerID: value.OwnerID, HostID: "h1234567", Workspace: value,
		Directory: value.ID, State: workspace.BindingDetached, BindingVersion: 2,
	}
	store := &workspaceServiceStore{
		values:       map[string]workspace.Workspace{value.ID: value},
		agentByOwner: map[string]workspace.Workspace{value.OwnerID: value},
		agentHistory: map[string]string{value.ID: value.OwnerID},
		bindings:     map[string][]workspace.Binding{value.ID: {binding}},
	}
	content := &workspaceContentStub{}
	current := &workspaceCurrentDeleterStub{}
	service, err := NewWorkspaceService(store, content, current, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteHostWorkspace(context.Background(), value.OwnerID, binding.HostID); err != nil {
		t.Fatalf("DeleteHostWorkspace(Agent Workspace) = %v", err)
	}
	stored, err := store.GetWorkspace(context.Background(), value.ID)
	if err != nil || stored.State != workspace.StateReady || store.claimCalls != 0 || len(content.deletes) != 0 || len(current.deletes) != 0 {
		t.Fatalf("Host deletion changed Agent Workspace: %#v, claims=%d content=%v current=%v err=%v",
			stored, store.claimCalls, content.deletes, current.deletes, err)
	}
}

func TestWorkspaceServiceExplicitlyDeletesAndRecreatesAgentWorkspace(t *testing.T) {
	const ownerID = "u1234567"
	const oldID = "w1234567"
	const hostID = "h1234567"
	value := workspace.Workspace{ID: oldID, OwnerID: ownerID, Name: "Agent Workspace", State: workspace.StateReady, Revision: 3}
	binding := workspace.Binding{
		ID: "b1234567", OwnerID: ownerID, HostID: hostID, Workspace: value,
		Directory: oldID, State: workspace.BindingDetached, BindingVersion: 2,
	}
	store := &workspaceServiceStore{
		values:       map[string]workspace.Workspace{oldID: value},
		agentByOwner: map[string]workspace.Workspace{ownerID: value},
		agentInit:    map[string]bool{ownerID: true},
		agentHistory: map[string]string{oldID: ownerID},
		bindings:     map[string][]workspace.Binding{oldID: {binding}},
	}
	content := &workspaceContentStub{}
	current := &workspaceCurrentDeleterStub{}
	service, err := NewWorkspaceService(store, content, current, &workspaceCapacityStub{}, &credentialAudit{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: ownerID, TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	deleted, err := service.DeleteWorkspace(context.Background(), principal, oldID)
	if err != nil || deleted.State != workspace.StateDeleted || store.agentByOwner[ownerID].ID != "" ||
		len(content.deletes) != 1 || len(current.deletes) != 1 {
		t.Fatalf("explicit Agent Workspace delete = %#v, current mapping=%#v, content=%v current=%v err=%v",
			deleted, store.agentByOwner[ownerID], content.deletes, current.deletes, err)
	}
	if err := service.SetWorkspaceCurrentInitializer(func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	recreated, err := service.EnsureAgentWorkspace(context.Background(), ownerID)
	if err != nil || recreated.ID == oldID || recreated.OwnerID != ownerID || store.agentCreates != 1 {
		t.Fatalf("recreated Agent Workspace = %#v, creates=%d, err=%v", recreated, store.agentCreates, err)
	}
	if err := service.DeleteHostWorkspace(context.Background(), ownerID, hostID); err != nil {
		t.Fatalf("late Host cleanup for retired Agent Workspace = %v", err)
	}
	if len(content.deletes) != 1 || len(current.deletes) != 1 {
		t.Fatalf("late Host cleanup deleted retired Agent Workspace data: content=%v current=%v", content.deletes, current.deletes)
	}
}
