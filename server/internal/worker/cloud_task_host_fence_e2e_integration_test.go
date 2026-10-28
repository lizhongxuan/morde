package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/runtimeclient"
	"morde/server/internal/storage/workspacefs"
)

func TestCloudTaskActiveHostOperationWaitsForTakeoverStopProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}

	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	hostValue, runtimeCredential, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	activeBinding, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate Host Workspace binding: %v", err)
	}

	hostRoot := t.TempDir()
	workspaceDirectory := filepath.Join(hostRoot, workspaceValue.ID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := runtimefiles.OpenWorkspace(hostRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := files.Close(); err != nil {
			t.Errorf("close Host runtime Workspace: %v", err)
		}
	}()
	var fileWrite runtimecore.ActionHandler
	runtimeHandlers := files.Handlers()
	for _, handler := range runtimeHandlers {
		if handler.Capability() == "file.write" && handler.Action() == "write" {
			fileWrite = handler
			break
		}
	}
	if fileWrite == nil {
		t.Fatal("Host runtime did not register file.write")
	}
	heldWrite := &f01TakeoverHeldFileWriteHandler{
		inner: fileWrite, entered: make(chan struct{}), canceled: make(chan struct{}),
		allowInitialWrite: make(chan struct{}, 1), stopStarted: make(chan struct{}), stopProof: make(chan struct{}),
	}
	defer heldWrite.confirmStopProof()
	for index, handler := range runtimeHandlers {
		if handler == fileWrite {
			runtimeHandlers[index] = heldWrite
			break
		}
	}

	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()),
		HostID: hostValue.ID, Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: runtimeHandlers,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeHandler, err := runtimehttp.NewHandler(runtimeManager, runtimeCredential)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer := httptest.NewUnstartedServer(runtimeHandler)
	runtimeServer.TLS, err = runtimeidentity.ServerTLSConfig(runtimeCredential, hostValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer.StartTLS()
	defer func() {
		runtimeServer.Close()
		if err := runtimeManager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close Host runtime: %v", err)
		}
	}()

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace blocks: %v", err)
		}
	}()
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: runtimeServer.URL}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeControl.Handshake(ctx, hostValue.ID); err != nil {
		t.Fatalf("handshake with Host runtime: %v", err)
	}
	oldSession, err := runtimeControl.Connect(ctx, hostValue.ID, nil)
	if err != nil {
		t.Fatalf("capture original Host fence: %v", err)
	}
	defer oldSession.Close()
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controlService, err := app.NewControlService(store, credentials, store, store, store, runtimeControl, auditService, store)
	if err != nil {
		t.Fatal(err)
	}
	apiRouter, err := api.New(api.Deps{
		Hosts: f01UnusedHostService{}, TLSAuthorizer: f01UnusedTLSAuthorizer{},
		Verifier: credentials, Auth: f01UnusedAuth{}, Control: controlService, Tasks: taskService,
		WorkspaceSync: syncService, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "hosts.example.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(apiRouter)
	defer portal.Close()
	httpClient := portal.Client()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real dsh subprocess cannot run")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x53}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-host-fence-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-host-fence-supervisor",
		Supervisor: &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}},
		Instances:  instances, Checkpoints: checkpoints, Models: f01TakeoverFileWriteModel{workspaceID: workspaceValue.ID},
		Tools: toolBroker, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "f01-host-fence-worker", LeaseDuration: 30 * time.Second,
		PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(workerCtx) }()
	workerStopped := false
	defer func() {
		if !workerStopped {
			stopF01Worker(t, cancelWorker, workerDone, "Host-fence TaskWorker")
		}
	}()

	request := func(method, path string, payload any) *http.Response {
		t.Helper()
		var body bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&body).Encode(payload); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, portal.URL+path, &body)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
		req.Header.Set("Origin", portal.URL)
		req.Header.Set("Idempotency-Key", "f01-host-fence-task-001")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := request(http.MethodPost, "/api/tasks", map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": 0,
		"model_id": "p1234567", "objective": "Write a file in the Host Workspace.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 1000},
	})
	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) {
		t.Fatalf("POST /api/tasks = status %d, response %#v", response.StatusCode, accepted)
	}

	select {
	case <-heldWrite.entered:
	case <-ctx.Done():
		t.Fatalf("dsh did not reach the Host file.write handler: %v", ctx.Err())
	}
	operation, err := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-host-fence-write-001"))
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("Host operation at takeover boundary = %#v, %v; want dispatched", operation, err)
	}

	takeoverRequest, err := newF01HostControlRequest(ctx, portal.URL, sessionCredential, hostValue.ID, control.ActionTakeOver,
		"f01-host-fence-takeover", hostValue.ControlVersion)
	if err != nil {
		t.Fatal(err)
	}
	takeoverDone := make(chan f01HostFenceHTTPResult, 1)
	go func() {
		result, err := httpClient.Do(takeoverRequest) //nolint:bodyclose // The receiver closes each response after reading it.
		takeoverDone <- f01HostFenceHTTPResult{response: result, err: err}
	}()
	stopStarted, actionCanceled := false, false
	stopTimer := time.NewTimer(10 * time.Second)
	defer stopTimer.Stop()
	for !stopStarted || !actionCanceled {
		select {
		case <-heldWrite.stopStarted:
			stopStarted = true
		case <-heldWrite.canceled:
			actionCanceled = true
		case result := <-takeoverDone:
			closeF01HTTPResult(result)
			t.Fatal("take-over returned before Host stop proof was available")
		case <-stopTimer.C:
			t.Fatalf("take-over did not reach both Host cancellation signals: stop_started=%t action_cancelled=%t", stopStarted, actionCanceled)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Host operation cancellation: %v", ctx.Err())
		}
	}

	statusResponse := request(http.MethodGet, "/api/hosts/"+hostValue.ID+"/control", nil)
	var intermediate struct {
		ControlState     host.ControlState `json:"control_state"`
		ControlVersion   int64             `json:"control_version"`
		ActiveTransition *struct {
			Phase          control.Phase   `json:"phase"`
			DrainedReceipt json.RawMessage `json:"drained_receipt"`
		} `json:"active_transition"`
	}
	if err := json.NewDecoder(statusResponse.Body).Decode(&intermediate); err != nil {
		_ = statusResponse.Body.Close()
		t.Fatal(err)
	}
	_ = statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK || intermediate.ControlState != host.ControlTakingOver ||
		intermediate.ActiveTransition == nil || intermediate.ActiveTransition.Phase != control.PhaseDraining ||
		len(intermediate.ActiveTransition.DrainedReceipt) != 0 {
		t.Fatalf("control state before Host stop proof = status %d %#v; want draining with no receipt", statusResponse.StatusCode, intermediate)
	}
	select {
	case result := <-takeoverDone:
		closeF01HTTPResult(result)
		t.Fatal("take-over response reported completion before Host stop proof")
	case <-time.After(100 * time.Millisecond):
	}

	heldWrite.confirmStopProof()
	result := awaitF01HostControlResponse(t, ctx, takeoverDone, http.StatusOK)
	var takeover struct {
		ControlState   host.ControlState `json:"control_state"`
		ControlVersion int64             `json:"control_version"`
	}
	if err := json.NewDecoder(result.Body).Decode(&takeover); err != nil {
		_ = result.Body.Close()
		t.Fatal(err)
	}
	_ = result.Body.Close()
	if takeover.ControlState != host.ControlUser || takeover.ControlVersion <= hostValue.ControlVersion {
		t.Fatalf("take-over response = %#v; want user control at a newer fence", takeover)
	}

	firstPath := filepath.Join(workspaceDirectory, "during-takeover.txt")
	select {
	case heldWrite.allowInitialWrite <- struct{}{}:
	default:
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Host Workspace file after canceled write = stat error %v; want no file", err)
	}
	if heldWrite.calls.Load() != 1 || heldWrite.effects.Load() != 0 || heldWrite.stopCalls.Load() != 1 {
		t.Fatalf("Host handler calls=%d effects=%d stop calls=%d; want one cancelled call, no write, one proof", heldWrite.calls.Load(), heldWrite.effects.Load(), heldWrite.stopCalls.Load())
	}

	stopF01Worker(t, cancelWorker, workerDone, "Host-fence TaskWorker")
	workerStopped = true

	oldOperationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = oldSession.Execute(ctx, "q1234567", oldOperationID, 1, "file.write", "write",
		json.RawMessage(`{"path":"stale-fence.txt","content":"must not write"}`), workspaceValue.ID, activeBinding.BindingVersion, nil)
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host write with original fence after take-over = %v; want fence rejection", err)
	}
	assertF01HostRuntimeFenceRejected(t, ctx, oldSession)
	if _, err := os.Stat(filepath.Join(workspaceDirectory, "stale-fence.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale-fence Host file = stat error %v; want no file", err)
	}

	releaseRequest, err := newF01HostControlRequest(ctx, portal.URL, sessionCredential, hostValue.ID, control.ActionRelease,
		"f01-host-fence-release", takeover.ControlVersion)
	if err != nil {
		t.Fatal(err)
	}
	releaseResponse, err := httpClient.Do(releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	var released struct {
		ControlState   host.ControlState `json:"control_state"`
		ControlVersion int64             `json:"control_version"`
	}
	if err := json.NewDecoder(releaseResponse.Body).Decode(&released); err != nil {
		_ = releaseResponse.Body.Close()
		t.Fatal(err)
	}
	_ = releaseResponse.Body.Close()
	if releaseResponse.StatusCode != http.StatusOK || released.ControlState != host.ControlAI ||
		released.ControlVersion <= takeover.ControlVersion {
		t.Fatalf("release response = status %d %#v; want AI control at a newer fence", releaseResponse.StatusCode, released)
	}

	newSession, err := runtimeControl.Connect(ctx, hostValue.ID, nil)
	if err != nil {
		t.Fatalf("connect with released Host fence: %v", err)
	}
	defer newSession.Close()
	if newSession.Mode != runtimeclient.ModeAI || newSession.Fence.ControlVersion != released.ControlVersion {
		t.Fatalf("released Host runtime session = mode %q fence %#v; want AI at version %d", newSession.Mode, newSession.Fence, released.ControlVersion)
	}
	if version, err := newSession.SyncWorkspaceBindings(ctx, workspaceValue.ID, activeBinding.BindingVersion); err != nil || version != activeBinding.BindingVersion {
		t.Fatalf("sync Workspace binding under released Host fence = version %d, err=%v", version, err)
	}
	newTaskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	newOperationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := newSession.SetTaskLease(ctx, newTaskID, 1, time.Now().UTC().Add(10*time.Second)); err != nil {
		t.Fatalf("set lease under released Host fence: %v", err)
	}
	_, err = newSession.Execute(ctx, newTaskID, newOperationID, 1, "file.write", "write",
		json.RawMessage(`{"path":"after-release.txt","content":"new fence write"}`), workspaceValue.ID, activeBinding.BindingVersion, nil)
	if err != nil {
		t.Fatalf("Host write under released fence: %v", err)
	}
	var newRuntimeOperation runtimeclient.Operation
	if err := waitF01(t, ctx, func() (bool, error) {
		newRuntimeOperation, err = newSession.GetOperation(ctx, newTaskID, newOperationID, 1)
		if err != nil {
			return false, err
		}
		if newRuntimeOperation.Status == contract.OperationFailed || newRuntimeOperation.Status == contract.OperationUnknown ||
			newRuntimeOperation.Status == contract.OperationCancelled {
			return false, fmt.Errorf("new-fence Host operation ended as %s", newRuntimeOperation.Status)
		}
		return newRuntimeOperation.Status == contract.OperationSucceeded, nil
	}, nil, nil); err != nil {
		t.Fatalf("new-fence Host operation did not succeed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(workspaceDirectory, "after-release.txt"))
	if err != nil || string(content) != "new fence write" {
		t.Fatalf("new-fence Workspace content = %q err=%v", content, err)
	}
	if heldWrite.effects.Load() != 1 {
		t.Fatalf("Host file write effects after release = %d; want exactly one", heldWrite.effects.Load())
	}

	staleAfterReleaseID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = oldSession.Execute(ctx, "q1234567", staleAfterReleaseID, 1, "file.write", "write",
		json.RawMessage(`{"path":"stale-after-release.txt","content":"must not write"}`), workspaceValue.ID, activeBinding.BindingVersion, nil)
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host write with stale fence after release = %v; want fence rejection", err)
	}
	assertF01HostRuntimeFenceRejected(t, ctx, oldSession)
	if _, err := os.Stat(filepath.Join(workspaceDirectory, "stale-after-release.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale after-release file = stat error %v; want no file", err)
	}
}

func assertF01HostRuntimeFenceRejected(t *testing.T, ctx context.Context, session *app.RuntimeSession) {
	t.Helper()
	operationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Execute(ctx, "q1234567", operationID, 1, "test.action", "run", json.RawMessage(`{}`), "", 0, nil)
	var runtimeErr *runtimeclient.Error
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != runtimeclient.ErrorFenceRejected {
		t.Fatalf("Host runtime accepted stale operation fence: %v; want %s", err, runtimeclient.ErrorFenceRejected)
	}
}

type f01TakeoverHeldFileWriteHandler struct {
	inner             runtimecore.ActionHandler
	entered           chan struct{}
	canceled          chan struct{}
	allowInitialWrite chan struct{}
	stopStarted       chan struct{}
	stopProof         chan struct{}
	firstOnce         sync.Once
	stopOnce          sync.Once
	proofOnce         sync.Once
	calls             atomic.Int32
	effects           atomic.Int32
	stopCalls         atomic.Int32
}

func (h *f01TakeoverHeldFileWriteHandler) Capability() string { return "file.write" }
func (h *f01TakeoverHeldFileWriteHandler) Action() string     { return "write" }

func (h *f01TakeoverHeldFileWriteHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	if h.calls.Add(1) == 1 {
		h.firstOnce.Do(func() { close(h.entered) })
		select {
		case <-ctx.Done():
			close(h.canceled)
			return nil, ctx.Err()
		case <-h.allowInitialWrite:
		}
	}
	result, err := h.inner.Execute(ctx, request)
	if err == nil {
		h.effects.Add(1)
	}
	return result, err
}

func (h *f01TakeoverHeldFileWriteHandler) Stop(ctx context.Context, _ contract.Operation) (runtimecore.StopResult, error) {
	h.stopCalls.Add(1)
	h.stopOnce.Do(func() { close(h.stopStarted) })
	select {
	case <-h.stopProof:
		return runtimecore.StopResult{VerifiedStopped: true}, nil
	case <-ctx.Done():
		return runtimecore.StopResult{}, ctx.Err()
	}
}

func (h *f01TakeoverHeldFileWriteHandler) confirmStopProof() {
	h.proofOnce.Do(func() { close(h.stopProof) })
}

type f01TakeoverFileWriteModel struct{ workspaceID string }

func (m f01TakeoverFileWriteModel) Complete(_ context.Context, _ app.ModelCall) (app.ModelResponse, error) {
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}
	return app.ModelResponse{
		ToolCalls: []app.ModelToolCall{{CallID: "f01-host-fence-write-001", Name: "file_write",
			Arguments: fmt.Sprintf(`{"workspace_id":%q,"path":"during-takeover.txt","content":"must not land"}`, m.workspaceID)}},
		StopReason: "tool_calls", Usage: usage,
	}, nil
}

func newF01HostControlRequest(ctx context.Context, baseURL, sessionCredential, hostID string, action control.Action, requestID string, expectedVersion int64) (*http.Request, error) {
	payload, err := json.Marshal(map[string]any{
		"action": action, "request_id": requestID, "expected_control_version": expectedVersion,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/hosts/"+hostID+"/control/actions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

type f01HostFenceHTTPResult struct {
	response *http.Response
	err      error
}

func awaitF01HostControlResponse(t *testing.T, ctx context.Context, responses <-chan f01HostFenceHTTPResult, wantStatus int) *http.Response {
	t.Helper()
	select {
	case result := <-responses:
		if result.err != nil {
			t.Fatalf("Host control action: %v", result.err)
		}
		if result.response == nil || result.response.StatusCode != wantStatus {
			if result.response == nil {
				t.Fatal("Host control action returned no response")
			}
			body, _ := io.ReadAll(result.response.Body)
			_ = result.response.Body.Close()
			t.Fatalf("Host control action status = %d, body %s; want %d", result.response.StatusCode, body, wantStatus)
		}
		return result.response
	case <-ctx.Done():
		t.Fatalf("timed out waiting for Host control action: %v", ctx.Err())
		return nil
	}
}

func closeF01HTTPResult(result f01HostFenceHTTPResult) {
	if result.response != nil {
		_, _ = io.Copy(io.Discard, result.response.Body)
		_ = result.response.Body.Close()
	}
}
