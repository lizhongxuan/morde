package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	projectReceipt   workspace.SyncHostApplication
	projectReport    workspace.HostProjectUploadApplicationReport
	projectCalls     int
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

func (s *hostReplicaServiceStub) RecordAppliedRevision(context.Context, identity.Principal, string, int64, string, int64) (workspace.Binding, error) {
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

func (s *hostReplicaServiceStub) RecordProjectUploadApplication(_ context.Context, _ identity.Principal, _ string, _ int64,
	_ string, report workspace.HostProjectUploadApplicationReport) (workspace.SyncHostApplication, error) {
	s.projectReport, s.projectCalls = report, s.projectCalls+1
	return s.projectReceipt, nil
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

func TestHostProjectUploadApplicationRouteReturnsSessionReceipt(t *testing.T) {
	services := newServices()
	healthy := workspace.BindingSyncHealthy
	services.hostReplica = &hostReplicaServiceStub{
		principal: identity.Principal{UserID: testUserID, Kind: identity.KindHostReplica, HostScope: testHostID, HostGeneration: 3},
		projectReceipt: workspace.SyncHostApplication{Status: "applied", HostID: testHostID, WorkspaceBindingVersion: 2,
			TargetRevision: 9, ManifestSHA256: strings.Repeat("a", 64), RuntimeGeneration: int64Pointer(3),
			AppliedRevision: int64Pointer(9), SyncState: &healthy, Impacts: []workspace.SyncHostApplicationImpact{}},
	}
	router := newRouter(t, services, Options{})
	path := "/api/host-replicas/" + testHostID + "/generations/3/workspaces/" + testWorkspaceID +
		"/bindings/2/project-uploads/p1234567/applied"
	body := `{"source_binding_id":"12345678-1234-4234-8234-123456789abc","target_revision":9,"manifest_sha256":"` +
		strings.Repeat("a", 64) + `","status":"applied","applied_revision":9,"sync_state":"healthy","impacts":[]}`
	response := request(router, http.MethodPost, path, body, "Bearer runtime-credential", "")
	if response.Code != http.StatusOK || services.hostReplica.projectCalls != 1 ||
		services.hostReplica.projectReport.SourceBindingID != "12345678-1234-4234-8234-123456789abc" ||
		services.hostReplica.projectReport.TargetRevision != 9 {
		t.Fatalf("project receipt response/call = %d %s / %#v", response.Code, response.Body, services.hostReplica)
	}
	var result workspace.SyncHostApplication
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Status != "applied" ||
		result.TargetRevision != 9 || result.RuntimeGeneration == nil || *result.RuntimeGeneration != 3 {
		t.Fatalf("project receipt response body = %#v, %v", result, err)
	}
}

func TestHostReplicaBindingsIncludePendingProjectUploadApplications(t *testing.T) {
	services := newServices()
	binding := services.hosts.item.Workspaces[0]
	binding.ProjectUploadApplications = []workspace.PendingProjectUploadApplication{{
		SessionID: "p1234567", SourceBindingID: "12345678-1234-4234-8234-123456789abc",
		ProjectPath: "project/demo", TargetRevision: 7, ManifestSHA256: strings.Repeat("b", 64),
	}}
	services.hostReplica = &hostReplicaServiceStub{
		principal: identity.Principal{UserID: testUserID, Kind: identity.KindHostReplica, HostScope: testHostID, HostGeneration: 4},
		binding:   binding,
	}
	router := newRouter(t, services, Options{})
	response := request(router, http.MethodGet, "/api/host-replicas/"+testHostID+"/generations/4/workspace-bindings", "", "Bearer runtime-credential", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list Host bindings status = %d, body=%s", response.Code, response.Body)
	}
	var result hostReplicaBindingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Items) != 1 ||
		len(result.Items[0].ProjectUploadApplications) != 1 || result.Items[0].ProjectUploadApplications[0].SessionID != "p1234567" {
		t.Fatalf("list Host bindings project applications = %#v, err=%v", result, err)
	}
}

func int64Pointer(value int64) *int64 { return &value }
