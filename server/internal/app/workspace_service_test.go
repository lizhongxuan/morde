package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceServiceStore struct {
	WorkspaceStore
	values        map[string]workspace.Workspace
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

func (s *workspaceServiceStore) CreateWorkspace(_ context.Context, value workspace.Workspace, record IdempotencyRecord) (workspace.Workspace, bool, error) {
	s.createRecord = record
	if s.createErr != nil {
		return workspace.Workspace{}, false, s.createErr
	}
	if existing, ok := s.values[record.ResourceID]; ok {
		return existing, true, nil
	}
	s.values[value.ID] = value
	return value, s.createReplay, nil
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
