package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/runtimeidentity"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/runtimeclient"
)

func TestControlServiceLaunchIsPersistedFencedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	service, hosts, runtimeState, auditWriter, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()

	result, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_test-01")
	if err != nil || result.Operation.Status != runtimeclient.ApplicationLaunchRunning {
		t.Fatalf("first launch = %#v, err=%v", result, err)
	}
	stored, err := hosts.GetApplicationLaunch(ctx, "h1234567", "u1234567", WechatApplicationID, "launch_test-01")
	if err != nil || stored.Status != ApplicationLaunchRunning || stored.RuntimeInstanceID != "ri_wechat" || stored.ControlVersion != 0 {
		t.Fatalf("persisted launch = %#v, err=%v", stored, err)
	}

	replay, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_test-01")
	if err != nil || replay.Operation.Status != runtimeclient.ApplicationLaunchRunning || runtimeState.launchCalls.Load() != 1 || runtimeState.getCalls.Load() != 1 {
		t.Fatalf("same-ID replay = %#v, err=%v, launches=%d gets=%d", replay, err, runtimeState.launchCalls.Load(), runtimeState.getCalls.Load())
	}
	if len(auditWriter.entries) != 2 || auditWriter.entries[0].Action != "host.application.launch" {
		t.Fatalf("launch audit entries = %#v", auditWriter.entries)
	}

	stopped, err := service.StopApplicationLaunch(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_test-01")
	if err != nil || stopped.Operation.Status != runtimeclient.ApplicationLaunchStopped || runtimeState.stopCalls.Load() != 1 {
		t.Fatalf("stop launch = %#v, err=%v, stop calls=%d", stopped, err, runtimeState.stopCalls.Load())
	}
	if len(auditWriter.entries) != 3 || auditWriter.entries[2].Action != "host.application.stop" {
		t.Fatalf("stop audit entries = %#v", auditWriter.entries)
	}
	otherUser := taskTestPrincipal()
	otherUser.UserID = "u9999999"
	if _, err := service.LaunchApplication(ctx, otherUser, "h1234567", WechatApplicationID, "launch_other-01"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("non-owner launch error = %v", err)
	}
	if runtimeState.launchCalls.Load() != 1 {
		t.Fatalf("non-owner launch reached Runtime; calls=%d", runtimeState.launchCalls.Load())
	}
}

func TestControlServiceReconcilesAndStopsAfterSameGenerationRuntimeRestart(t *testing.T) {
	ctx := context.Background()
	service, hosts, runtimeState, _, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()

	const launchID = "launch_restart-proof1"
	if _, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, launchID); err != nil {
		t.Fatalf("initial launch: %v", err)
	}

	// Runtime restarts and control state advances while the Host generation
	// remains the same. The durable launch's original fence must stay intact,
	// while status/stop use the current Runtime fence plus the same launch ID.
	newFence := runtimeclient.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "ri_restarted", ControlVersion: 1}
	hosts.mu.Lock()
	hostValue := hosts.values["h1234567"]
	hostValue.RuntimeInstanceID, hostValue.ControlVersion = newFence.RuntimeInstanceID, newFence.ControlVersion
	hosts.values["h1234567"] = hostValue
	hosts.mu.Unlock()
	runtimeState.mu.Lock()
	runtimeState.currentFence = newFence
	runtimeState.mu.Unlock()

	status, err := service.GetApplicationLaunch(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, launchID)
	if err != nil || status.Operation.Status != runtimeclient.ApplicationLaunchRunning || runtimeState.getCalls.Load() != 1 {
		t.Fatalf("status after Runtime restart = %#v, err=%v, gets=%d", status, err, runtimeState.getCalls.Load())
	}
	stopped, err := service.StopApplicationLaunch(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, launchID)
	if err != nil || stopped.Operation.Status != runtimeclient.ApplicationLaunchStopped || runtimeState.stopCalls.Load() != 1 {
		t.Fatalf("stop after Runtime restart = %#v, err=%v, stops=%d", stopped, err, runtimeState.stopCalls.Load())
	}
	stored, err := hosts.GetApplicationLaunch(ctx, "h1234567", "u1234567", WechatApplicationID, launchID)
	if err != nil || stored.RuntimeInstanceID != "ri_wechat" || stored.ControlVersion != 0 || stored.Status != ApplicationLaunchStopped {
		t.Fatalf("persisted launch lost its original fence/outcome = %#v, err=%v", stored, err)
	}
}

func TestControlServiceNeverRepeatsAnAmbiguousLaunch(t *testing.T) {
	ctx := context.Background()
	service, hosts, runtimeState, _, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()
	runtimeState.launchFailure.Store(true)

	first, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_ambiguous01")
	if err != nil || first.Operation.Status != runtimeclient.ApplicationLaunchUnknown || runtimeState.launchCalls.Load() != 1 {
		t.Fatalf("ambiguous launch = %#v, err=%v, calls=%d", first, err, runtimeState.launchCalls.Load())
	}
	stored, err := hosts.GetApplicationLaunch(ctx, "h1234567", "u1234567", WechatApplicationID, "launch_ambiguous01")
	if err != nil || stored.Status != ApplicationLaunchUnknown {
		t.Fatalf("ambiguous result was not persisted as unknown: %#v, err=%v", stored, err)
	}

	// Runtime has no operation for this ID. Reconciliation remains unknown and
	// never sends a second launch request.
	runtimeState.getNotFound.Store(true)
	retry, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_ambiguous01")
	if err != nil || retry.Operation.Status != runtimeclient.ApplicationLaunchUnknown || runtimeState.launchCalls.Load() != 1 || runtimeState.getCalls.Load() != 1 {
		t.Fatalf("ambiguous retry = %#v, err=%v, launches=%d gets=%d", retry, err, runtimeState.launchCalls.Load(), runtimeState.getCalls.Load())
	}
	if _, err := service.LaunchApplication(ctx, taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_other-ambiguous"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("different launch while prior outcome is unknown = %v", err)
	}
	if latest, err := hosts.GetLatestApplicationLaunch(ctx, "h1234567", "u1234567", WechatApplicationID); err != nil || latest.ID != "launch_ambiguous01" || latest.Status != ApplicationLaunchUnknown {
		t.Fatalf("unknown launch was obscured by a second reservation: %#v err=%v", latest, err)
	}
	if runtimeState.launchCalls.Load() != 1 {
		t.Fatalf("different launch reached Runtime while prior outcome is unknown; calls=%d", runtimeState.launchCalls.Load())
	}
}

func TestControlServiceLaunchRequiresHostActivityLease(t *testing.T) {
	service, hosts, runtimeState, auditWriter, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()
	hosts.mu.Lock()
	hosts.activityLeaseErr = domain.NewError(domain.CodeConflict, "Host release closed application admission")
	hosts.mu.Unlock()

	if _, err := service.LaunchApplication(context.Background(), taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_lease-denied1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("launch without Host activity lease error = %v, want conflict", err)
	}
	if runtimeState.launchCalls.Load() != 0 || len(auditWriter.entries) != 0 {
		t.Fatalf("launch crossed closed Host admission: Runtime calls=%d, audit=%#v", runtimeState.launchCalls.Load(), auditWriter.entries)
	}
	if _, err := hosts.GetApplicationLaunch(context.Background(), "h1234567", "u1234567", WechatApplicationID, "launch_lease-denied1"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("failed admission reserved an application launch: %v", err)
	}
}

func TestControlServiceStopRequiresHostActivityLease(t *testing.T) {
	service, hosts, runtimeState, _, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()
	const launchID = "launch_stop-lease1"
	if _, err := service.LaunchApplication(context.Background(), taskTestPrincipal(), "h1234567", WechatApplicationID, launchID); err != nil {
		t.Fatal(err)
	}
	hosts.mu.Lock()
	hosts.activityLeaseErr = domain.NewError(domain.CodeConflict, "Host release closed application admission")
	hosts.mu.Unlock()
	if _, err := service.StopApplicationLaunch(context.Background(), taskTestPrincipal(), "h1234567", WechatApplicationID, launchID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stop without Host activity lease error = %v, want conflict", err)
	}
	if runtimeState.stopCalls.Load() != 0 {
		t.Fatalf("stop crossed closed Host admission: Runtime calls=%d", runtimeState.stopCalls.Load())
	}
	stored, err := hosts.GetApplicationLaunch(context.Background(), "h1234567", "u1234567", WechatApplicationID, launchID)
	if err != nil || stored.Status != ApplicationLaunchRunning {
		t.Fatalf("failed stop changed launch state: %#v, err=%v", stored, err)
	}
}

func TestControlServiceLaunchKeepsLeaseUntilRuntimeCallAndReconciliationFinish(t *testing.T) {
	service, hosts, runtimeState, _, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()
	runtimeState.launchEntered = make(chan struct{})
	runtimeState.launchProceed = make(chan struct{})
	releaseRuntime := sync.OnceFunc(func() { close(runtimeState.launchProceed) })
	defer releaseRuntime()
	done := make(chan error, 1)
	go func() {
		_, err := service.LaunchApplication(context.Background(), taskTestPrincipal(), "h1234567", WechatApplicationID, "launch_in-flight1")
		done <- err
	}()
	select {
	case <-runtimeState.launchEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("Runtime launch was not entered")
	}
	if active, err := hosts.HasActiveHostActivityLease(context.Background(), "h1234567", 1); err != nil || !active {
		t.Fatalf("in-flight application launch lost its Host lease: active=%t, err=%v", active, err)
	}
	// A confirmed release may enter stopping after the launch is admitted.
	// Its worker must still wait for the lease before deleting the sandbox.
	hosts.mu.Lock()
	value := hosts.values["h1234567"]
	value.State = host.StateStopping
	hosts.values[value.ID] = value
	hosts.mu.Unlock()
	releaseRuntime()
	select {
	case err := <-done:
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("launch reconciliation after Host release error = %v, want conflict", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight application launch did not complete")
	}
	if active, err := hosts.HasActiveHostActivityLease(context.Background(), "h1234567", 1); err != nil || active {
		t.Fatalf("completed application launch retained its Host lease: active=%t, err=%v", active, err)
	}
}

func TestControlServiceLaunchQueriesRejectUnsupportedApplication(t *testing.T) {
	service, _, runtimeState, auditWriter, closeRuntime := newApplicationLaunchServiceTest(t)
	defer closeRuntime()
	ctx := context.Background()
	principal := taskTestPrincipal()
	if _, err := service.LaunchApplication(ctx, principal, "h1234567", "qq", "launch_unsupported01"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("launch unsupported application error = %v", err)
	}
	if _, err := service.CurrentApplicationLaunch(ctx, principal, "h1234567", "qq"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("current unsupported application error = %v", err)
	}
	if _, err := service.GetApplicationLaunch(ctx, principal, "h1234567", "qq", "launch_unsupported01"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("get unsupported application error = %v", err)
	}
	if _, err := service.StopApplicationLaunch(ctx, principal, "h1234567", "qq", "launch_unsupported01"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("stop unsupported application error = %v", err)
	}
	if runtimeState.launchCalls.Load()+runtimeState.getCalls.Load()+runtimeState.stopCalls.Load() != 0 || len(auditWriter.entries) != 0 {
		t.Fatalf("unsupported application reached Runtime or audit: runtime=%d audit=%d", runtimeState.launchCalls.Load()+runtimeState.getCalls.Load()+runtimeState.stopCalls.Load(), len(auditWriter.entries))
	}
}

func newApplicationLaunchServiceTest(t *testing.T) (*ControlService, *hostServiceTestHostStore, *applicationLaunchRuntime, *hostServiceTestAudit, func()) {
	t.Helper()
	credential := testRuntimeCredential(0x3d)
	createdAt := time.Date(2026, time.October, 25, 1, 2, 3, 0, time.UTC)
	initialFence := runtimeclient.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "ri_wechat", ControlVersion: 0}
	runtimeState := &applicationLaunchRuntime{createdAt: createdAt, operations: make(map[string]runtimeclient.ApplicationLaunchOperation), currentFence: initialFence}
	server := httptest.NewUnstartedServer(runtimeState)
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()

	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	credentialEnc, err := sealer.Seal([]byte(credential))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	value := hostServiceTestHost(host.StateRunning)
	value.RuntimeInstanceID = "ri_wechat"
	value.RuntimeCredentialEnc = credentialEnc
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host)}
	hosts.addHost(value)
	sandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: &hostServiceTestSandbox{values: make(map[string]Sandbox)}, endpoint: server.URL}
	runtime, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	auditWriter := &hostServiceTestAudit{}
	service, err := NewControlService(hosts, &taskAuthorizerStub{}, &controlTaskStoreStub{}, &controlTransitionStoreStub{hosts: hosts},
		&controlActivityDrainerStub{}, runtime, auditWriter, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return service, hosts, runtimeState, auditWriter, server.Close
}

type applicationLaunchRuntime struct {
	mu            sync.Mutex
	createdAt     time.Time
	operations    map[string]runtimeclient.ApplicationLaunchOperation
	currentFence  runtimeclient.HostFence
	launchCalls   atomic.Int64
	getCalls      atomic.Int64
	stopCalls     atomic.Int64
	launchFailure atomic.Bool
	getNotFound   atomic.Bool
	launchEntered chan struct{}
	launchProceed chan struct{}
}

func (r *applicationLaunchRuntime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+testRuntimeCredential(0x3d) || request.Header.Get(runtimeclient.ProtocolHeader) != runtimeclient.ProtocolVersion {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/runtime/v1/capabilities":
		r.mu.Lock()
		fence := r.currentFence
		r.mu.Unlock()
		writeLaunchTestJSON(w, runtimeclient.CapabilitiesResponse{
			ProtocolVersion: runtimeclient.ProtocolVersion, HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration,
			RuntimeInstanceID: fence.RuntimeInstanceID, ControlVersion: fence.ControlVersion, Mode: runtimeclient.ModeAI, OperationStoreReady: true,
			Capabilities: []string{"operation.execute", "operation.query", "operation.cancel", "control.fence", "control.quiesce", "task_lease.fence"},
		})
	case request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/applications/wechat/launch":
		r.launchCalls.Add(1)
		if r.launchEntered != nil {
			close(r.launchEntered)
			<-r.launchProceed
		}
		var body runtimeclient.ApplicationLaunchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.LaunchID == "" || body.HostID != "h1234567" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if r.launchFailure.Load() {
			http.Error(w, "upstream interrupted", http.StatusServiceUnavailable)
			return
		}
		operation := runtimeclient.ApplicationLaunchOperation{
			ID: body.LaunchID, ApplicationID: "wechat", Status: runtimeclient.ApplicationLaunchRunning,
			CreatedAt: r.createdAt, UpdatedAt: r.createdAt,
		}
		r.mu.Lock()
		r.operations[body.LaunchID] = operation
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		writeLaunchTestJSON(w, runtimeclient.ApplicationLaunchResponse{ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: body.HostFence, Operation: operation})
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/runtime/v1/applications/wechat/launches/"):
		r.getCalls.Add(1)
		launchID := strings.TrimPrefix(request.URL.Path, "/runtime/v1/applications/wechat/launches/")
		if r.getNotFound.Load() {
			http.NotFound(w, request)
			return
		}
		r.mu.Lock()
		operation, ok := r.operations[launchID]
		fence := r.currentFence
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, request)
			return
		}
		writeLaunchTestJSON(w, runtimeclient.ApplicationLaunchResponse{
			ProtocolVersion: runtimeclient.ProtocolVersion,
			HostFence:       fence,
			Operation:       operation,
		})
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/runtime/v1/applications/wechat/launches/") && strings.HasSuffix(request.URL.Path, "/stop"):
		r.stopCalls.Add(1)
		var body runtimeclient.ApplicationLaunchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		operation, ok := r.operations[body.LaunchID]
		fence := r.currentFence
		if ok {
			operation.Status = runtimeclient.ApplicationLaunchStopped
			operation.UpdatedAt = r.createdAt.Add(time.Second)
			r.operations[body.LaunchID] = operation
		}
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeLaunchTestJSON(w, runtimeclient.ApplicationLaunchResponse{ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: fence, Operation: operation})
	default:
		http.NotFound(w, request)
	}
}

func writeLaunchTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func (s *hostServiceTestHostStore) ReserveApplicationLaunch(_ context.Context, value ApplicationLaunch) (ApplicationLaunch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applicationLaunches == nil {
		s.applicationLaunches = make(map[string]ApplicationLaunch)
	}
	hostValue, ok := s.values[value.HostID]
	if !ok || hostValue.OwnerID != value.OwnerID || hostValue.DeletedAt != nil {
		return ApplicationLaunch{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	key := value.HostID + "\x00" + value.ID
	if existing, found := s.applicationLaunches[key]; found {
		if !sameApplicationLaunchReservationIdentity(existing, value) {
			return ApplicationLaunch{}, false, domain.NewError(domain.CodeConflict, "launch ID fence conflict")
		}
		return existing, false, nil
	}
	for _, existing := range s.applicationLaunches {
		if existing.HostID == value.HostID &&
			(existing.Status == ApplicationLaunchStarting || existing.Status == ApplicationLaunchRunning ||
				existing.Status == ApplicationLaunchStopping || existing.Status == ApplicationLaunchUnknown) {
			return ApplicationLaunch{}, false, domain.NewError(domain.CodeConflict, "another application launch is active or unresolved")
		}
	}
	if hostValue.State != host.StateRunning || hostValue.RuntimeGeneration != value.RuntimeGeneration || hostValue.RuntimeInstanceID != value.RuntimeInstanceID ||
		hostValue.ControlVersion != value.ControlVersion || (hostValue.ControlState != host.ControlAI && hostValue.ControlState != host.ControlUser) {
		return ApplicationLaunch{}, false, domain.NewError(domain.CodeInvalidState, "Host runtime fence changed")
	}
	value.CreatedAt, value.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	s.applicationLaunches[key] = value
	return value, true, nil
}

func TestHostServiceTestStoreApplicationLaunchReservationIsHostWide(t *testing.T) {
	for _, status := range []string{ApplicationLaunchStarting, ApplicationLaunchRunning, ApplicationLaunchStopping, ApplicationLaunchUnknown} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			hostValue := hostServiceTestHost(host.StateRunning)
			hostValue.RuntimeInstanceID = "runtime-app-launch"
			store := &hostServiceTestHostStore{
				values: map[string]host.Host{hostValue.ID: hostValue},
				applicationLaunches: map[string]ApplicationLaunch{
					hostValue.ID + "\x00launch_existing01": {
						HostID: hostValue.ID, OwnerID: hostValue.OwnerID, ID: "launch_existing01", ApplicationID: "future-app",
						RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
						ControlVersion: hostValue.ControlVersion, Status: status,
					},
				},
			}
			request := ApplicationLaunch{
				HostID: hostValue.ID, OwnerID: hostValue.OwnerID, ID: "launch_candidate01", ApplicationID: WechatApplicationID,
				RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
				ControlVersion: hostValue.ControlVersion, Status: ApplicationLaunchStarting,
			}
			if _, created, err := store.ReserveApplicationLaunch(ctx, request); domain.CodeOf(err) != domain.CodeConflict || created {
				t.Fatalf("reservation with active app %q status %q = created %t, err %v; want Host-wide conflict", "future-app", status, created, err)
			}
		})
	}

	ctx := context.Background()
	hostValue := hostServiceTestHost(host.StateRunning)
	hostValue.RuntimeInstanceID = "runtime-app-launch"
	request := ApplicationLaunch{
		HostID: hostValue.ID, OwnerID: hostValue.OwnerID, ID: "launch_replay01", ApplicationID: WechatApplicationID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, Status: ApplicationLaunchStarting,
	}
	existing := request
	existing.Status = ApplicationLaunchRunning
	existing.CreatedAt = time.Unix(1, 0).UTC()
	store := &hostServiceTestHostStore{
		values: map[string]host.Host{hostValue.ID: hostValue},
		applicationLaunches: map[string]ApplicationLaunch{
			hostValue.ID + "\x00" + existing.ID: existing,
			hostValue.ID + "\x00launch_other01": {
				HostID: hostValue.ID, OwnerID: hostValue.OwnerID, ID: "launch_other01", ApplicationID: "future-app",
				RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
				ControlVersion: hostValue.ControlVersion, Status: ApplicationLaunchUnknown,
			},
		},
	}
	replayed, created, err := store.ReserveApplicationLaunch(ctx, request)
	if err != nil || created || replayed.ID != existing.ID || replayed.Status != existing.Status || !replayed.CreatedAt.Equal(existing.CreatedAt) {
		t.Fatalf("same-ID replay with another app unresolved = %#v, created=%t, err=%v; want original record", replayed, created, err)
	}
}

func (s *hostServiceTestHostStore) GetLatestApplicationLaunch(_ context.Context, hostID, ownerID, appID string) (ApplicationLaunch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest ApplicationLaunch
	found := false
	for _, value := range s.applicationLaunches {
		if value.HostID != hostID || value.OwnerID != ownerID || value.ApplicationID != appID {
			continue
		}
		if !found || value.CreatedAt.After(latest.CreatedAt) || value.CreatedAt.Equal(latest.CreatedAt) && value.ID > latest.ID {
			latest, found = value, true
		}
	}
	if !found {
		return ApplicationLaunch{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	return latest, nil
}

func (s *hostServiceTestHostStore) GetApplicationLaunch(_ context.Context, hostID, ownerID, appID, launchID string) (ApplicationLaunch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.applicationLaunches[hostID+"\x00"+launchID]
	if !ok || value.OwnerID != ownerID || value.ApplicationID != appID {
		return ApplicationLaunch{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	return value, nil
}

func (s *hostServiceTestHostStore) UpdateApplicationLaunch(_ context.Context, value ApplicationLaunch) (ApplicationLaunch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hostValue, ok := s.values[value.HostID]
	if !ok || hostValue.OwnerID != value.OwnerID || hostValue.DeletedAt != nil {
		return ApplicationLaunch{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	key := value.HostID + "\x00" + value.ID
	stored, ok := s.applicationLaunches[key]
	if !ok || !sameApplicationLaunchTestFence(stored, value) {
		return ApplicationLaunch{}, domain.NewError(domain.CodeConflict, "application launch fence changed")
	}
	if stored.Status == ApplicationLaunchStopped {
		return stored, nil
	}
	if (hostValue.State != host.StateRunning || hostValue.RuntimeGeneration != value.RuntimeGeneration ||
		(hostValue.ControlState != host.ControlAI && hostValue.ControlState != host.ControlUser)) && value.Status != ApplicationLaunchUnknown {
		return ApplicationLaunch{}, domain.NewError(domain.CodeConflict, "Host runtime fence changed")
	}
	if !validApplicationLaunchTestTransition(stored.Status, value.Status) {
		return ApplicationLaunch{}, domain.NewError(domain.CodeInvalidState, "launch state transition invalid")
	}
	stored.Status, stored.ErrorCode, stored.UpdatedAt = value.Status, value.ErrorCode, time.Now().UTC()
	s.applicationLaunches[key] = stored
	return stored, nil
}

func sameApplicationLaunchReservationIdentity(left, right ApplicationLaunch) bool {
	return left.HostID == right.HostID && left.OwnerID == right.OwnerID && left.ID == right.ID &&
		left.ApplicationID == right.ApplicationID && left.RuntimeGeneration == right.RuntimeGeneration
}

func sameApplicationLaunchTestFence(left, right ApplicationLaunch) bool {
	return sameApplicationLaunchReservationIdentity(left, right) && left.RuntimeInstanceID == right.RuntimeInstanceID &&
		left.ControlVersion == right.ControlVersion
}

func validApplicationLaunchTestTransition(from, to string) bool {
	switch from {
	case ApplicationLaunchStarting:
		return to == ApplicationLaunchStarting || to == ApplicationLaunchRunning || to == ApplicationLaunchStopping ||
			to == ApplicationLaunchStopped || to == ApplicationLaunchExited || to == ApplicationLaunchFailed || to == ApplicationLaunchUnknown
	case ApplicationLaunchRunning:
		return to == ApplicationLaunchRunning || to == ApplicationLaunchStopping || to == ApplicationLaunchStopped ||
			to == ApplicationLaunchExited || to == ApplicationLaunchFailed || to == ApplicationLaunchUnknown
	case ApplicationLaunchStopping:
		return to == ApplicationLaunchStopping || to == ApplicationLaunchStopped || to == ApplicationLaunchExited ||
			to == ApplicationLaunchFailed || to == ApplicationLaunchUnknown
	case ApplicationLaunchUnknown:
		return to == ApplicationLaunchStarting || to == ApplicationLaunchRunning || to == ApplicationLaunchStopping ||
			to == ApplicationLaunchStopped || to == ApplicationLaunchExited || to == ApplicationLaunchFailed || to == ApplicationLaunchUnknown
	default:
		return false
	}
}
