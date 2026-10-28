package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := session.SetTaskLease(ctx, taskID, 1, leaseExpiry); err != nil {
		t.Fatalf("set runtime lease: %v", err)
	}

	runner := NewRuntimeOperationRunner(time.Millisecond)
	arguments := json.RawMessage(`{"target_id":"tab-1"}`)
	operation, err := runner.Execute(ctx, session, taskID, operationID, 1, "browser.observe", "snapshot", arguments, "", 0, nil)
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

type runnerTestActionHandler struct{ executions *atomic.Int32 }

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
