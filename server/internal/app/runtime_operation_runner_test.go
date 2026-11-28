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
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/domain/host"
	"morde/server/internal/runtimeclient"
)

func TestRuntimeOperationRunnerExecutesAndQueriesOverPinnedRuntimeHTTP(t *testing.T) {
	credentialBytes := make([]byte, 32)
	for index := range credentialBytes {
		credentialBytes[index] = 0x35
	}
	credential := base64.RawURLEncoding.EncodeToString(credentialBytes)

	storeDir, err := os.MkdirTemp("", "morde-runtime-operation-runner-")
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

	var executions atomic.Int32
	handler := &runnerTestActionHandler{executions: &executions}
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 1,
		Handlers: []runtimecore.ActionHandler{handler},
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
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)

	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealedCredential, err := sealer.Seal([]byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	value := hostServiceTestHost(host.StateRunning)
	value.ControlState = host.ControlAI
	value.RuntimeCredentialEnc = sealedCredential
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host)}
	hosts.addHost(value)
	sandbox := &runtimeControlTestSandbox{
		hostServiceTestSandbox: &hostServiceTestSandbox{values: make(map[string]Sandbox)},
		endpoint:               server.URL,
	}
	control, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	session, err := control.Connect(ctx, value.ID, nil)
	if err != nil {
		t.Fatalf("connect to runtime: %v", err)
	}
	defer session.Close()
	if session.Mode != runtimeclient.ModeAI || !session.Supports("browser.observe") {
		t.Fatalf("runtime mode/capability = %q/%v, want AI and browser.observe", session.Mode, session.Capabilities)
	}
	const taskID = "q1234567"
	const operationID = "o1234567"
	leaseExpiry := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if err := session.SetTaskLease(ctx, taskID, 1, leaseExpiry, false); err != nil {
		t.Fatalf("set runtime lease: %v", err)
	}

	runner := NewRuntimeOperationRunner(time.Millisecond)
	arguments := json.RawMessage(`{"target_id":"tab-1"}`)
	operation, err := runner.Execute(ctx, session, taskID, operationID, 1, "browser.observe", "snapshot", arguments, "", 0, "", 0, nil)
	if err != nil {
		t.Fatalf("execute runtime operation: %v", err)
	}
	operation, err = runner.Poll(ctx, session, taskID, operationID, 1, operation)
	if err != nil {
		t.Fatalf("poll runtime operation: %v", err)
	}
	if operation.OperationID != operationID || operation.Status != runtimeclient.OperationSucceeded {
		t.Fatalf("operation = %+v, want %s succeeded", operation, operationID)
	}
	var result map[string]string
	if err := json.Unmarshal(operation.Result, &result); err != nil {
		t.Fatalf("decode runtime result %q: %v", operation.Result, err)
	}
	if result["target_id"] != "tab-1" || executions.Load() != 1 {
		t.Fatalf("runtime result/executions = %v/%d, want tab-1/1", result, executions.Load())
	}

	queried, err := runner.Get(ctx, session, taskID, operationID, 1)
	if err != nil {
		t.Fatalf("query original operation: %v", err)
	}
	if queried.OperationID != operationID || queried.Status != runtimeclient.OperationSucceeded || executions.Load() != 1 {
		t.Fatalf("queried operation/executions = %+v/%d, want same terminal operation without redispatch", queried, executions.Load())
	}
}

func TestRuntimeOperationRunnerKeepsSupplementalOutputOutsideOperationResult(t *testing.T) {
	credentialBytes := make([]byte, 32)
	credential := base64.RawURLEncoding.EncodeToString(credentialBytes)
	storeDir, err := os.MkdirTemp("", "morde-runtime-output-capture-")
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

	var executions atomic.Int32
	handler := &runnerTestSupplementalShellHandler{executions: &executions}
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 1,
		Handlers: []runtimecore.ActionHandler{handler}, TaskWorkspaceViews: runnerTaskWorkspaceViewStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	initial, err := manager.Capabilities(contract.CapabilitiesRequest{ProtocolVersion: contract.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	fence := contract.HostFence{HostID: initial.HostID, RuntimeGeneration: initial.RuntimeGeneration,
		RuntimeInstanceID: initial.RuntimeInstanceID, ControlVersion: initial.ControlVersion}
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), contract.SetWorkspaceBindingsRequest{ProtocolVersion: contract.ProtocolVersion,
		HostFence: fence, Bindings: []contract.WorkspaceBinding{{WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567"}}}); err != nil {
		t.Fatal(err)
	}
	view, err := manager.SetTaskWorkspaceView(context.Background(), "q1234567", contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "q1234567", WorkspaceID: "w1234567", BindingVersion: 1,
		InputRevision: 2, WorkspaceCheckpointRevision: 3, ManifestSHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeHandler, err := runtimehttp.NewHandler(manager, credential)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(runtimeHandler)
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)

	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealedCredential, err := sealer.Seal([]byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	value := hostServiceTestHost(host.StateRunning)
	value.ControlState, value.RuntimeCredentialEnc = host.ControlAI, sealedCredential
	value.Workspaces[0].Workspace.Revision = 3
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host)}
	hosts.addHost(value)
	sandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: &hostServiceTestSandbox{values: make(map[string]Sandbox)}, endpoint: server.URL}
	control, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := control.Connect(ctx, value.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !session.Supports(runtimeclient.SupplementalOutputCaptureCapability) {
		t.Fatalf("runtime capabilities=%v, want supplemental output capture", session.Capabilities)
	}
	if err := session.SetTaskLease(ctx, "q1234567", 1, time.Now().UTC().Add(time.Minute).Truncate(time.Second), true); err != nil {
		t.Fatal(err)
	}
	identity := runtimeclient.SupplementalOutputCaptureIdentity{OwnerID: "u1234567", TaskID: "q1234567", AuthorizationVersion: 3,
		TaskRevision: 4, WorkspaceID: "w1234567", WorkspaceAuthorizationVersion: 5, WorkspaceBindingVersion: 1,
		WorkspaceViewID: view.ViewID, WorkspaceCheckpointRevision: 3, StepID: "s1234567", CallID: "call-123"}
	operation, err := NewRuntimeOperationRunner(time.Millisecond).ExecuteWithSupplementalOutputCapture(ctx, session, "q1234567", "o1234567", 1,
		"shell.exec", "run", json.RawMessage(`{"command":"true"}`), "w1234567", 1, view.ViewID, 3, int64Pointer(2), identity)
	if err != nil {
		t.Fatalf("ExecuteWithSupplementalOutputCapture() error=%v", err)
	}
	operation, err = NewRuntimeOperationRunner(time.Millisecond).Poll(ctx, session, "q1234567", "o1234567", 1, operation)
	if err != nil || operation.Status != runtimeclient.OperationSucceeded {
		t.Fatalf("poll operation=%#v err=%v", operation, err)
	}
	if string(operation.Result) != `{"exit_code":0,"stdout":"legacy","stderr":"","stdout_truncated":true,"stderr_truncated":false}` || executions.Load() != 1 {
		t.Fatalf("operation result/executions=%s/%d", operation.Result, executions.Load())
	}
	page, err := session.SupplementalOutputCapturePage(ctx, "q1234567", "o1234567", 1, identity, runtimeclient.SupplementalOutputCaptureStdout, 0)
	if err != nil || page.State != runtimeclient.SupplementalOutputCaptureComplete || string(page.Data) != "secret" {
		t.Fatalf("supplemental page=%#v err=%v", page, err)
	}
	if err := session.ReleaseSupplementalOutputCapture(ctx, "q1234567", "o1234567", 1, identity); err != nil {
		t.Fatalf("release supplemental capture: %v", err)
	}
	if _, err := session.SupplementalOutputCapturePage(ctx, "q1234567", "o1234567", 1, identity, runtimeclient.SupplementalOutputCaptureStdout, 0); err != nil {
		t.Fatalf("released capture should fail closed as an empty failed page: %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("capture read/release replayed original operation %d times", executions.Load())
	}
}

type runnerTestActionHandler struct{ executions *atomic.Int32 }

type runnerTestSupplementalShellHandler struct{ executions *atomic.Int32 }

func (*runnerTestSupplementalShellHandler) Capability() string                      { return "shell.exec" }
func (*runnerTestSupplementalShellHandler) Action() string                          { return "run" }
func (*runnerTestSupplementalShellHandler) SupportsSupplementalOutputCapture() bool { return true }
func (h *runnerTestSupplementalShellHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	h.executions.Add(1)
	sink := contract.SupplementalOutputSinkFromContext(ctx)
	if sink != nil {
		if !sink.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("sec")) ||
			!sink.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("ret")) {
			return nil, errors.New("supplemental output sink rejected test bytes")
		}
	}
	return json.RawMessage(`{"exit_code":0,"stdout":"legacy","stderr":"","stdout_truncated":true,"stderr_truncated":false}`), nil
}
func (*runnerTestSupplementalShellHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

type runnerTaskWorkspaceViewStore struct{}

func (runnerTaskWorkspaceViewStore) PrepareTaskWorkspaceView(context.Context, string, string, string, string, int64, int64, int64, string) error {
	return nil
}
func (runnerTaskWorkspaceViewStore) DeleteTaskWorkspaceView(context.Context, string, string, string) error {
	return nil
}
func (runnerTaskWorkspaceViewStore) ExportTaskWorkspaceManifestPage(context.Context, contract.TaskWorkspaceManifestRequest) (contract.TaskWorkspaceManifestResponse, error) {
	return contract.TaskWorkspaceManifestResponse{}, errors.New("unused")
}
func (runnerTaskWorkspaceViewStore) ReadTaskWorkspaceBlock(context.Context, string, string, string, string) ([]byte, error) {
	return nil, errors.New("unused")
}

func (*runnerTestActionHandler) Capability() string { return "browser.observe" }
func (*runnerTestActionHandler) Action() string     { return "snapshot" }
func (h *runnerTestActionHandler) Execute(_ context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	h.executions.Add(1)
	var arguments struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"target_id": arguments.TargetID})
}
func (*runnerTestActionHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}
