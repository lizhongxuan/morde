package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde-runtime/runtimecore"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/runtimeclient"
)

func TestRuntimeTaskLeaseUsesShorterTaskOrHostDeadline(t *testing.T) {
	now := time.Date(2026, time.October, 27, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		expiry time.Time
		want   time.Time
	}{
		{name: "short task lease", expiry: now.Add(10 * time.Second), want: now.Add(10 * time.Second)},
		{name: "long task lease", expiry: now.Add(time.Minute), want: now.Add(25 * time.Second)},
		{name: "expired task lease", expiry: now.Add(-time.Second), want: now.Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundedRuntimeLeaseExpiry(now, tc.expiry); !got.Equal(tc.want) {
				t.Fatalf("host lease expiry = %v, want %v", got, tc.want)
			}
		})
	}
	service, _, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()
	session, err := service.Connect(context.Background(), "h1234567", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.SetTaskLease(context.Background(), "q1234567", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("host should accept bounded lease: %v", err)
	}
}

func TestRuntimeControlConnectActivatesBlockedAIAndPersistsInstance(t *testing.T) {
	service, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	session, err := service.Connect(context.Background(), "h1234567", nil)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer session.Close()
	if session.Mode != runtimeclient.ModeAI || session.Fence.ControlVersion != 0 {
		t.Fatalf("session = %+v, want AI at control version 0", session)
	}
	stored, err := hosts.GetHost(context.Background(), "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	if stored.RuntimeInstanceID == "" || stored.RuntimeInstanceID != session.Fence.RuntimeInstanceID {
		t.Fatalf("stored runtime instance = %q, session = %q", stored.RuntimeInstanceID, session.Fence.RuntimeInstanceID)
	}
	response, err := manager.Capabilities(runtimeclient.CapabilitiesRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostID: stored.ID, RuntimeGeneration: stored.RuntimeGeneration, ControlVersion: 0})
	if err != nil || response.Mode != runtimeclient.ModeAI {
		t.Fatalf("runtime mode = %q, err = %v, want AI", response.Mode, err)
	}
}

func TestRuntimeControlReadTaskFenceRequiresOwnedRunningHost(t *testing.T) {
	service, hosts, manager, server := newRuntimeControlTest(t, 4, 3, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	value, err := hosts.GetHost(context.Background(), "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := service.ReadTaskFence(context.Background(), value.OwnerID, value.ID)
	if err != nil || !fence.AIAllowed || fence.RuntimeGeneration != 1 || fence.ControlVersion != 4 {
		t.Fatalf("ReadTaskFence() = %+v, %v; want current AI fence", fence, err)
	}
	if _, err := service.ReadTaskFence(context.Background(), "u9999999", value.ID); err == nil {
		t.Fatal("ReadTaskFence() disclosed a Host to another owner")
	}

	value.ControlState = host.ControlTakingOver
	hosts.addHost(value)
	fence, err = service.ReadTaskFence(context.Background(), value.OwnerID, value.ID)
	if err != nil || fence.AIAllowed || fence.ControlVersion != 4 {
		t.Fatalf("ReadTaskFence() during takeover = %+v, %v; want AI denied at current fence", fence, err)
	}
}

func TestRuntimeControlConnectReconcilesBehindControlVersion(t *testing.T) {
	service, _, manager, server := newRuntimeControlTest(t, 4, 3, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	session, err := service.Connect(context.Background(), "h1234567", nil)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer session.Close()
	if session.Mode != runtimeclient.ModeAI || session.Fence.ControlVersion != 4 {
		t.Fatalf("session = %+v, want AI at control version 4", session)
	}
	response, err := manager.Capabilities(runtimeclient.CapabilitiesRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1, ControlVersion: 4})
	if err != nil || response.ControlVersion != 4 || response.Mode != runtimeclient.ModeAI {
		t.Fatalf("runtime = version %d mode %q, err = %v; want version 4 AI", response.ControlVersion, response.Mode, err)
	}
}

func TestRuntimeControlFailsClosedForAheadFenceAndWrongTLSIdentity(t *testing.T) {
	t.Run("runtime ahead", func(t *testing.T) {
		service, hosts, manager, server := newRuntimeControlTest(t, 1, 2, runtimeclient.ModeBlocked, host.ControlAI, "")
		defer func() { _ = manager.Close() }()
		defer server.Close()
		if _, err := service.Connect(context.Background(), "h1234567", nil); err == nil {
			t.Fatal("Connect() accepted runtime control version ahead of Morde")
		}
		stored, err := hosts.GetHost(context.Background(), "h1234567")
		if err != nil {
			t.Fatal(err)
		}
		if stored.RuntimeInstanceID != "" {
			t.Fatalf("persisted runtime instance after rejected handshake: %q", stored.RuntimeInstanceID)
		}
	})

	t.Run("wrong tls identity", func(t *testing.T) {
		wrongCredential := testRuntimeCredential(0x71)
		service, _, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, wrongCredential)
		defer func() { _ = manager.Close() }()
		defer server.Close()
		if _, err := service.Connect(context.Background(), "h1234567", nil); err == nil {
			t.Fatal("Connect() accepted a runtime with another TLS identity")
		}
	})
}

func TestRuntimeControlTransitionRecoversCommittedHumanMode(t *testing.T) {
	ctx := context.Background()
	service, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	initial, err := service.Connect(ctx, "h1234567", nil)
	if err != nil {
		t.Fatalf("initial runtime connect: %v", err)
	}
	initial.Close()
	fence := runtimeclient.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: initial.Fence.RuntimeInstanceID, ControlVersion: 1}
	if _, err := manager.SetControl(runtimeclient.SetControlRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: fence, Mode: runtimeclient.ModeBlocked}); err != nil {
		t.Fatalf("block runtime: %v", err)
	}
	quiesced, err := manager.Quiesce(ctx, runtimeclient.QuiesceRequest{
		ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: fence, TargetControlVersion: 2, TaskIDs: []string{},
	})
	if err != nil || quiesced.DrainedReceipt == nil {
		t.Fatalf("runtime quiesce = %#v, %v", quiesced, err)
	}
	if _, err := manager.SetControl(runtimeclient.SetControlRequest{
		ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: runtimeclient.HostFence{
			HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: initial.Fence.RuntimeInstanceID, ControlVersion: 2,
		}, Mode: runtimeclient.ModeHuman, DrainedReceipt: quiesced.DrainedReceipt,
	}); err != nil {
		t.Fatalf("commit runtime human mode: %v", err)
	}
	value, err := hosts.GetHost(ctx, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	value.ControlState, value.ControlVersion = host.ControlTakingOver, 1
	hosts.addHost(value)
	receipt, err := json.Marshal(quiesced.DrainedReceipt)
	if err != nil {
		t.Fatal(err)
	}
	transition := control.Transition{
		HostID: value.ID, OwnerID: value.OwnerID, RequestID: "takeover-recover-1", Action: control.ActionTakeOver,
		Phase: control.PhaseDrained, ExpectedControlVersion: 0, BlockedControlVersion: 1, TargetControlVersion: 2,
		TaskIDs: []string{}, TaskIDsSaved: true, DrainedReceipt: receipt,
	}
	// The database still says taking_over@1, while the runtime committed user@2.
	// Only the persisted target and receipt allow this bounded ahead-fence recovery.
	recovered, err := service.ConnectTransition(ctx, transition)
	if err != nil {
		t.Fatalf("recover transition: %v", err)
	}
	defer recovered.Close()
	if recovered.Mode != runtimeclient.ModeHuman || recovered.Fence.ControlVersion != 2 {
		t.Fatalf("recovered session = %+v, want human at version 2", recovered)
	}
}

func TestRuntimeControlRejectsStaleExpectedFenceBeforeNetworkAccess(t *testing.T) {
	service, _, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	_, err := service.Connect(context.Background(), "h1234567", &AgentHostFence{RuntimeGeneration: 1, ControlVersion: 1, AIAllowed: true})
	if err == nil || !strings.Contains(err.Error(), "fence changed") {
		t.Fatalf("Connect() error = %v, want stale Host fence rejection", err)
	}
}

func TestHostReadinessRequiresAIOThenRuntimeHandshake(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.ControlState = host.ControlAI
	sealHostServiceTestKey(t, deps.sealer, &value)
	if stage, err := service.probeReady(context.Background(), deps.sandbox, value); err != nil || stage != "" {
		t.Fatalf("probeReady() error = %v", err)
	}
	if len(deps.runtime.hosts) != 1 || deps.runtime.hosts[0] != value.ID {
		t.Fatalf("runtime handshake hosts = %v, want [%s]", deps.runtime.hosts, value.ID)
	}

	deps.runtime.hosts = nil
	deps.runtime.err = errors.New("runtime unavailable")
	if stage, err := service.probeReady(context.Background(), deps.sandbox, value); err == nil || stage != "runtime_handshake" {
		t.Fatalf("probeReady() stage/error = %q / %v, want runtime handshake failure", stage, err)
	}
	if len(deps.runtime.hosts) != 1 {
		t.Fatalf("runtime handshake calls = %d, want 1", len(deps.runtime.hosts))
	}

	deps.runtime.err = nil
	deps.probe.err = errors.New("AIO unavailable")
	deps.runtime.hosts = nil
	if stage, err := service.probeReady(context.Background(), deps.sandbox, value); err == nil || stage != "aio_health" {
		t.Fatalf("probeReady() stage/error = %q / %v, want AIO health failure", stage, err)
	}
	if len(deps.runtime.hosts) != 0 {
		t.Fatalf("runtime handshake ran after failed AIO probe: %v", deps.runtime.hosts)
	}
}

func TestHostReadinessUsesRealRuntimeHandshakeWhileCreating(t *testing.T) {
	hostsService, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateCreating)
	value.ControlState = host.ControlAI
	sealHostServiceTestKey(t, deps.sealer, &value)
	credential := testRuntimeCredential(0x46)
	sealedCredential, err := deps.sealer.Seal([]byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	value.RuntimeCredentialEnc = sealedCredential
	deps.hosts.addHost(value)

	storeDir, err := os.MkdirTemp("", "morde-runtime-readiness-")
	if err != nil {
		t.Fatal(err)
	}
	storeDir, err = filepath.EvalSymlinks(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(storeDir) })
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: value.ID, Generation: value.RuntimeGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	runtimeHandler, err := runtimehttp.NewHandler(manager, credential)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(runtimeHandler)
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)

	runtimeSandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: deps.sandbox, endpoint: server.URL}
	runtime, err := NewRuntimeControl(deps.hosts, &hostServiceTestRouter{client: runtimeSandbox}, deps.sealer)
	if err != nil {
		t.Fatal(err)
	}
	hostsService.runtime = runtime
	if _, err := hostsService.probeReady(context.Background(), deps.sandbox, value); err != nil {
		t.Fatalf("probeReady() with real runtime handshake error = %v", err)
	}
	stored, err := deps.hosts.GetHost(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != host.StateCreating || stored.RuntimeInstanceID == "" {
		t.Fatalf("Host after creating-state handshake = state %q, instance %q", stored.State, stored.RuntimeInstanceID)
	}
	if _, err := deps.hosts.TransitionHost(context.Background(), value.ID, host.StateCreating, host.StateRunning, "runtime ready"); err != nil {
		t.Fatalf("transition after runtime handshake: %v", err)
	}
	stored, err = deps.hosts.GetHost(context.Background(), value.ID)
	if err != nil || stored.State != host.StateRunning || stored.RuntimeInstanceID == "" {
		t.Fatalf("Host after running transition = state %q, instance %q, err %v", stored.State, stored.RuntimeInstanceID, err)
	}
}

func newRuntimeControlTest(t *testing.T, hostVersion, runtimeVersion int64, runtimeMode runtimeclient.Mode, hostMode host.ControlState, serverCredential string) (*RuntimeControl, *hostServiceTestHostStore, *runtimecore.Manager, *httptest.Server) {
	t.Helper()
	credential := testRuntimeCredential(0x3d)
	if serverCredential == "" {
		serverCredential = credential
	}
	storeDir, err := os.MkdirTemp("", "morde-runtime-control-")
	if err != nil {
		t.Fatal(err)
	}
	storeDir, err = filepath.EvalSymlinks(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(storeDir) })
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeVersion > 0 || runtimeMode != runtimeclient.ModeBlocked {
		fence := runtimeclient.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: runtimeInstanceID(t, manager)}
		fence.ControlVersion = runtimeVersion
		if _, err := manager.SetControl(runtimeclient.SetControlRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostFence: fence, Mode: runtimeMode}); err != nil {
			_ = manager.Close()
			t.Fatalf("prepare runtime control state: %v", err)
		}
	}
	handler, err := runtimehttp.NewHandler(manager, serverCredential)
	if err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	serverTLS, err := runtimeidentity.ServerTLSConfig(serverCredential, "h1234567")
	if err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()

	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		server.Close()
		_ = manager.Close()
		t.Fatal(err)
	}
	sealed, err := sealer.Seal([]byte(credential))
	if err != nil {
		server.Close()
		_ = manager.Close()
		t.Fatal(err)
	}
	value := hostServiceTestHost(host.StateRunning)
	value.ControlState = hostMode
	value.ControlVersion = hostVersion
	value.RuntimeCredentialEnc = sealed
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host)}
	hosts.addHost(value)
	sandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: &hostServiceTestSandbox{values: make(map[string]Sandbox)}, endpoint: server.URL}
	service, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		server.Close()
		_ = manager.Close()
		t.Fatal(err)
	}
	return service, hosts, manager, server
}

func runtimeInstanceID(t *testing.T, manager *runtimecore.Manager) string {
	t.Helper()
	response, err := manager.Capabilities(runtimeclient.CapabilitiesRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	return response.RuntimeInstanceID
}

func testRuntimeCredential(seed byte) string {
	value := make([]byte, 32)
	for index := range value {
		value[index] = seed
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

type runtimeControlTestSandbox struct {
	*hostServiceTestSandbox
	endpoint string
}

func (s *runtimeControlTestSandbox) Endpoint(context.Context, string, int) (Upstream, error) {
	return Upstream{BaseURL: s.endpoint}, nil
}
