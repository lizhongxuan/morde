package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostReplicaServiceStub struct {
	principal        identity.Principal
	binding          workspace.Binding
	reportedState    workspace.BindingSyncState
	reportedConflict []string
	reportCalls      int
}

func (s *hostReplicaServiceStub) Authenticate(context.Context, string, int64, string) (identity.Principal, error) {
	return s.principal, nil
}

func (s *hostReplicaServiceStub) AuthenticateWorkspace(context.Context, string) (identity.Principal, error) {
	return s.principal, nil
}

func (s *hostReplicaServiceStub) ScopeWorkspace(_ context.Context, principal identity.Principal, workspaceID string, version int64) (identity.Principal, error) {
	principal.ReplicaWorkspaceID, principal.BindingVersion = workspaceID, version
	return principal, nil
}

func (s *hostReplicaServiceStub) ListBindings(context.Context, identity.Principal) ([]workspace.Binding, error) {
	return []workspace.Binding{s.binding}, nil
}

func (s *hostReplicaServiceStub) CredentialForBinding(context.Context, identity.Principal, string, int64) (string, error) {
	return "credential", nil
}

func (s *hostReplicaServiceStub) RecordAppliedRevision(context.Context, identity.Principal, string, int64, int64) (workspace.Binding, error) {
	return s.binding, nil
}

func (s *hostReplicaServiceStub) ActivateBinding(context.Context, identity.Principal, string, int64) (workspace.Binding, error) {
	return s.binding, nil
}

func (s *hostReplicaServiceStub) CompleteUnbinding(context.Context, identity.Principal, string, int64, int64) (workspace.Binding, error) {
	return s.binding, nil
}

func (s *hostReplicaServiceStub) ReportWorkspaceSyncStatus(_ context.Context, _ identity.Principal, state workspace.BindingSyncState, conflicts []string) (workspace.Binding, error) {
	s.reportedState, s.reportedConflict, s.reportCalls = state, append([]string(nil), conflicts...), s.reportCalls+1
	s.binding.SyncState, s.binding.SyncConflicts = state, append([]string(nil), conflicts...)
	return s.binding, nil
}

var _ app.HostReplicaService = (*hostReplicaServiceStub)(nil)

func TestHostReplicaSyncStatusRouteValidatesAndProjectsConflictStatus(t *testing.T) {
	services := newServices()
	services.hostReplica = &hostReplicaServiceStub{
		principal: identity.Principal{UserID: testUserID, Kind: identity.KindHostReplica, HostScope: testHostID, HostGeneration: 1},
		binding:   services.hosts.item.Workspaces[0],
	}
	router := newRouter(t, services, Options{})
	path := "/api/host-replicas/" + testHostID + "/generations/1/workspaces/" + testWorkspaceID + "/bindings/1/sync-status"
	response := request(router, http.MethodPost, path, `{"state":"conflict","conflicts":["src/main.go"]}`, "Bearer runtime-credential", "")
	if response.Code != http.StatusOK || services.hostReplica.reportCalls != 1 ||
		services.hostReplica.reportedState != workspace.BindingSyncConflict || len(services.hostReplica.reportedConflict) != 1 || services.hostReplica.reportedConflict[0] != "src/main.go" {
		t.Fatalf("sync status response/call = %d %s / %#v", response.Code, response.Body, services.hostReplica)
	}
	var result hostReplicaBindingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.SyncState != workspace.BindingSyncConflict || len(result.SyncConflicts) != 1 {
		t.Fatalf("sync status response body = %#v, %v", result, err)
	}
	unsafe := request(router, http.MethodPost, path, `{"state":"conflict","conflicts":["../secret"]}`, "Bearer runtime-credential", "")
	if unsafe.Code != http.StatusBadRequest || services.hostReplica.reportCalls != 1 {
		t.Fatalf("unsafe sync status = %d %s, report calls=%d", unsafe.Code, unsafe.Body, services.hostReplica.reportCalls)
	}
}
