package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/runtimeidentity"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

func (s *hostServiceTestHostStore) ReserveApplicationInstall(_ context.Context, value ApplicationInstall) (ApplicationInstall, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applicationInstalls == nil {
		s.applicationInstalls = make(map[string]ApplicationInstall)
	}
	current := s.values[value.HostID]
	if current.OwnerID != value.OwnerID || current.State != host.StateRunning ||
		current.RuntimeGeneration != value.RuntimeGeneration || current.RuntimeInstanceID != value.RuntimeInstanceID {
		return ApplicationInstall{}, false, domain.NewError(domain.CodeConflict, "Host fence changed")
	}
	key := value.HostID + "\x00" + value.RequestID
	if existing, ok := s.applicationInstalls[key]; ok {
		return existing, false, nil
	}
	for _, existing := range s.applicationInstalls {
		if existing.HostID == value.HostID && !existing.Terminal() {
			return ApplicationInstall{}, false, domain.NewError(domain.CodeConflict, "another install is unresolved")
		}
	}
	value.CreatedAt, value.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	s.applicationInstalls[key] = value
	return value, true, nil
}

func (s *hostServiceTestHostStore) GetPendingApplicationInstall(_ context.Context, hostID, ownerID string) (ApplicationInstall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.applicationInstalls {
		if value.HostID == hostID && value.OwnerID == ownerID && !value.Terminal() {
			return value, nil
		}
	}
	return ApplicationInstall{}, domain.NewError(domain.CodeNotFound, "application install not found")
}

func (s *hostServiceTestHostStore) RecordApplicationInstallOperation(_ context.Context, hostID, ownerID, requestID string, operation runtimeclient.ApplicationInstallOperation) (ApplicationInstall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hostID + "\x00" + requestID
	value, ok := s.applicationInstalls[key]
	if !ok || value.OwnerID != ownerID || value.ApplicationID != operation.ApplicationID {
		return ApplicationInstall{}, domain.NewError(domain.CodeNotFound, "application install not found")
	}
	if value.Operation != nil && value.Operation.ID != operation.ID {
		return ApplicationInstall{}, domain.NewError(domain.CodeConflict, "application install operation changed")
	}
	value.Status, value.Operation, value.UpdatedAt = string(operation.Status), &operation, time.Now().UTC()
	s.applicationInstalls[key] = value
	return value, nil
}

func TestControlServiceInstallApplicationIsOwnerOnlyAndFenced(t *testing.T) {
	ctx := context.Background()
	credential := testRuntimeCredential(0x3d)
	createdAt := time.Date(2026, time.October, 25, 1, 2, 3, 0, time.UTC)
	type installStatusFixture struct {
		requestID string
		operation runtimeclient.ApplicationInstallOperation
	}
	var runtimeStatus atomic.Value
	runtimeStatus.Store(installStatusFixture{
		requestID: "install-wechat-01",
		operation: runtimeclient.ApplicationInstallOperation{
			ID: "app_1234", ApplicationID: "wechat", Status: runtimeclient.OperationSucceeded,
			ProgressPercent: 100, CreatedAt: createdAt, UpdatedAt: createdAt.Add(time.Second),
		},
	})
	var installCalls int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential || r.Header.Get(runtimeclient.ProtocolHeader) != runtimeclient.ProtocolVersion {
			t.Errorf("Runtime auth/protocol headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get(runtimeclient.ProtocolHeader))
		}
		switch r.URL.Path {
		case "/runtime/v1/capabilities":
			query := r.URL.Query()
			if r.Method != http.MethodGet || query.Get("host_id") != "h1234567" || query.Get("runtime_generation") != "1" || query.Get("control_version") != "0" {
				t.Errorf("capabilities request = %s %s", r.Method, r.URL.String())
			}
			writeApplicationTestJSON(t, w, runtimeclient.CapabilitiesResponse{
				ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1,
				RuntimeInstanceID: "ri_install", ControlVersion: 0, Mode: runtimeclient.ModeAI, OperationStoreReady: true,
				Capabilities: []string{"operation.execute", "operation.query", "operation.cancel", "control.fence", "control.quiesce", "task_lease.fence", "workspace.task_view"},
			})
		case "/runtime/v1/applications/wechat/install":
			installCalls++
			var request runtimeclient.ApplicationInstallRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode install request: %v", err)
			}
			if r.Method != http.MethodPost || request.RequestID != "install-wechat-01" && request.RequestID != "install-wechat-lost" || request.HostFence != (runtimeclient.HostFence{
				HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "ri_install", ControlVersion: 0,
			}) {
				t.Errorf("install request = %s %#v", r.Method, request)
			}
			if request.RequestID == "install-wechat-lost" {
				// Runtime accepted the operation, but its successful POST response was lost.
				runtimeStatus.Store(installStatusFixture{
					requestID: "install-other-01",
					operation: runtimeclient.ApplicationInstallOperation{
						ID: "app_lost1", ApplicationID: "wechat", Status: runtimeclient.OperationSucceeded,
						ProgressPercent: 100, CreatedAt: createdAt.Add(3 * time.Second), UpdatedAt: createdAt.Add(4 * time.Second),
					},
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"protocol_version": runtimeclient.ProtocolVersion, "code": "runtime_state_unavailable"})
				return
			}
			writeApplicationTestJSON(t, w, runtimeclient.ApplicationInstallResponse{
				ProtocolVersion: runtimeclient.ProtocolVersion,
				HostFence:       runtimeclient.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "ri_install", ControlVersion: 0},
				Operation: runtimeclient.ApplicationInstallOperation{
					ID: "app_1234", ApplicationID: "wechat", Status: runtimeclient.OperationAccepted,
					ProgressPercent: 0, CreatedAt: createdAt, UpdatedAt: createdAt,
				},
			})
		case "/runtime/v1/applications":
			fixture := runtimeStatus.Load().(installStatusFixture)
			operation := fixture.operation
			writeApplicationTestJSON(t, w, runtimeclient.ApplicationsResponse{
				ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1,
				RuntimeInstanceID: "ri_install", ControlVersion: 0, CheckedAt: createdAt.Add(2 * time.Second),
				Items: []runtimeclient.ApplicationStatusItem{
					{ID: "codex", Installed: false, LoginStatus: runtimeclient.ApplicationLoginNotApplicable},
					{ID: "wechat", Installed: true, Version: "4.1.13.26", LoginStatus: runtimeclient.ApplicationLoginUnknown,
						Installable: true, InstallRequestID: fixture.requestID, InstallOperation: &operation},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()

	hosts := &hostServiceTestHostStore{values: map[string]host.Host{}}
	value := hostServiceTestHost(host.StateRunning)
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	value.RuntimeCredentialEnc, err = sealer.Seal([]byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	hosts.addHost(value)
	sandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: &hostServiceTestSandbox{values: map[string]Sandbox{}}, endpoint: server.URL}
	runtime, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	auditWriter := &hostServiceTestAudit{}
	service, err := NewControlService(hosts, &taskAuthorizerStub{}, &controlTaskStoreStub{}, &controlTransitionStoreStub{hosts: hosts},
		&controlActivityDrainerStub{}, runtime, auditWriter, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.InstallApplication(ctx, taskTestPrincipal(), "h1234567", "wechat", "install-wechat-01")
	if err != nil {
		t.Fatalf("InstallApplication() error = %v", err)
	}
	if result.HostID != "h1234567" || result.RuntimeGeneration != 1 || result.RuntimeInstanceID != "ri_install" || result.ControlVersion != 0 ||
		result.Operation.ID != "app_1234" || result.Operation.Status != runtimeclient.OperationAccepted || installCalls != 1 {
		t.Fatalf("InstallApplication() = %#v, Runtime calls=%d", result, installCalls)
	}
	if len(auditWriter.entries) != 1 || auditWriter.entries[0].Action != audit.ActionHostApplicationInstall ||
		auditWriter.entries[0].Detail["application_id"] != "wechat" || auditWriter.entries[0].Detail["request_id"] != "install-wechat-01" {
		t.Fatalf("application install audit = %#v", auditWriter.entries)
	}
	if pending, err := hosts.GetPendingApplicationInstall(ctx, "h1234567", value.OwnerID); err != nil || pending.Operation == nil || pending.Status != string(runtimeclient.OperationAccepted) {
		t.Fatalf("pending install after dispatch = %#v, %v", pending, err)
	}
	if status, err := service.Applications(ctx, taskTestPrincipal(), "h1234567"); err != nil || len(status.Items) != 2 || status.Items[1].InstallOperation.Status != runtimeclient.OperationSucceeded {
		t.Fatalf("Applications() confirmation = %#v, %v", status, err)
	}
	if _, err := hosts.GetPendingApplicationInstall(ctx, "h1234567", value.OwnerID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("install remained pending after matching Runtime status: %v", err)
	}

	member := taskTestPrincipal()
	member.UserID = "u9999999"
	member.Kind = identity.KindAPIKey
	member.Scopes = []identity.Scope{identity.ScopeComputerOperate}
	member.HostIDs = []string{"h1234567"}
	if _, err := service.InstallApplication(ctx, member, "h1234567", "wechat", "install-wechat-02"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("shared member with computer:operate error = %v, want hidden not-found", err)
	}
	if installCalls != 1 {
		t.Fatalf("member reached Runtime install endpoint: calls=%d", installCalls)
	}
	if len(auditWriter.entries) != 1 {
		t.Fatalf("member request created an audit record: %#v", auditWriter.entries)
	}

	hosts.mu.Lock()
	hosts.activityLeaseErr = domain.NewError(domain.CodeConflict, "Host release closed application admission")
	hosts.mu.Unlock()
	if _, err := service.InstallApplication(ctx, taskTestPrincipal(), "h1234567", "wechat", "install-wechat-03"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("install without Host activity lease error = %v, want conflict", err)
	}
	if installCalls != 1 || len(auditWriter.entries) != 1 {
		t.Fatalf("install crossed closed Host admission: Runtime calls=%d, audit=%#v", installCalls, auditWriter.entries)
	}

	hosts.mu.Lock()
	hosts.activityLeaseErr = nil
	hosts.mu.Unlock()
	if _, err := service.InstallApplication(ctx, taskTestPrincipal(), "h1234567", "wechat", "install-wechat-lost"); err == nil {
		t.Fatal("lost Runtime POST response unexpectedly succeeded")
	}
	if pending, err := hosts.GetPendingApplicationInstall(ctx, "h1234567", value.OwnerID); err != nil || pending.RequestID != "install-wechat-lost" || pending.Status != ApplicationInstallUnknown {
		t.Fatalf("lost response did not retain unknown reservation: %#v, %v", pending, err)
	}
	if _, err := service.Applications(ctx, taskTestPrincipal(), "h1234567"); err != nil {
		t.Fatalf("mismatched Runtime status lookup: %v", err)
	}
	if pending, err := hosts.GetPendingApplicationInstall(ctx, "h1234567", value.OwnerID); err != nil || pending.Status != ApplicationInstallUnknown {
		t.Fatalf("mismatched request ID cleared reservation: %#v, %v", pending, err)
	}
	fixture := runtimeStatus.Load().(installStatusFixture)
	fixture.requestID = "install-wechat-lost"
	runtimeStatus.Store(fixture)
	if _, err := service.Applications(ctx, taskTestPrincipal(), "h1234567"); err != nil {
		t.Fatalf("exact Runtime status recovery: %v", err)
	}
	if _, err := hosts.GetPendingApplicationInstall(ctx, "h1234567", value.OwnerID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("exact Runtime status did not resolve reservation: %v", err)
	}
}

func TestControlServiceInstallApplicationRejectsUnsupportedAppAndBadRequestID(t *testing.T) {
	service, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	server.Close()
	controlService, err := NewControlService(hosts, &taskAuthorizerStub{}, &controlTaskStoreStub{}, &controlTransitionStoreStub{hosts: hosts},
		&controlActivityDrainerStub{}, service, &hostServiceTestAudit{}, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		appID     string
		requestID string
	}{
		{name: "unsupported application", appID: "codex", requestID: "install-wechat-03"},
		{name: "invalid request id", appID: "wechat", requestID: "bad key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := controlService.InstallApplication(context.Background(), taskTestPrincipal(), "h1234567", test.appID, test.requestID); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("InstallApplication() error = %v, want invalid argument", err)
			}
		})
	}
}

func writeApplicationTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode application test response: %v", err)
	}
}
