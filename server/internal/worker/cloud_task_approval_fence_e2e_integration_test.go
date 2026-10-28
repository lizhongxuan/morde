package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

func TestCloudTaskApprovalIsRevalidatedAfterHostControlFenceChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, runtimeCredential, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	approvalDesktop := &f01ApprovalDesktopHandler{}
	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()),
		HostID: hostValue.ID, Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: []runtimecore.ActionHandler{approvalDesktop},
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
		t.Fatalf("connect Host runtime before Task: %v", err)
	}
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
		Verifier: credentials, Auth: f01UnusedAuth{}, Control: controlService, Tasks: taskService, WorkspaceSync: syncService,
		MCPDisabled: true,
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
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x47}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-approval-fence-supervisor", stopKey)
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
	model := &f01CloudTaskModel{
		store: store, hostID: hostValue.ID, approvalFirst: true,
		secondStarted: make(chan struct{}), content: "unused after stale approval",
	}
	supervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-approval-fence-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Models: model, Tools: toolBroker, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	executionErrors := make(chan error, 4)
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: f01RecordingExecutor{inner: executor, errors: executionErrors, supervisor: supervisor, tools: toolBroker},
		WorkerID: "f01-approval-fence-worker", LeaseDuration: 30 * time.Second, PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	request := func(method, path string, payload any, key string) *http.Response {
		t.Helper()
		var body bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&body).Encode(payload); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, portal.URL+path, &body)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
		req.Header.Set("Origin", portal.URL)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := request(http.MethodPost, "/api/tasks", map[string]any{
		"host_id": hostValue.ID, "model_id": "p1234567", "objective": "Click the test desktop target after approval.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 1000},
	}, "f01-approval-fence-submit-001")
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
		t.Fatalf("POST /api/tasks = %d, %#v", response.StatusCode, accepted)
	}

	startWorker := func(worker *TaskWorker) (context.CancelFunc, <-chan error) {
		workerCtx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- worker.Run(workerCtx) }()
		return stop, done
	}
	firstCancel, firstDone := startWorker(backgroundWorker)
	firstStopped := false
	defer func() {
		if !firstStopped {
			stopF01Worker(t, firstCancel, firstDone, "initial TaskWorker")
		}
	}()

	var originalApproval task.PendingRequest
	if err := waitF01(t, ctx, func() (bool, error) {
		details, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		if details.Task.Status != task.StatusWaitingUser || details.PendingRequest == nil {
			return false, nil
		}
		originalApproval = *details.PendingRequest
		return true, nil
	}, executionErrors, nil); err != nil {
		t.Fatalf("sensitive Host action did not wait for approval: %v", err)
	}
	if originalApproval.Kind != "approval" || originalApproval.Status != task.PendingRequestPending ||
		originalApproval.ControlVersion != hostValue.ControlVersion || approvalDesktop.calls.Load() != 0 {
		t.Fatalf("initial approval fence/status or Host side effect = %#v calls=%d", originalApproval, approvalDesktop.calls.Load())
	}
	stopF01Worker(t, firstCancel, firstDone, "initial TaskWorker")
	firstStopped = true

	response = request(http.MethodPost,
		"/api/tasks/"+accepted.ID+"/approvals/"+originalApproval.ID+"/responses",
		map[string]any{"version": originalApproval.Version, "decision": "approve"}, "f01-approval-fence-response-001")
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("approve pending Host action = %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	details, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.PendingRequest == nil ||
		details.PendingRequest.Status != task.PendingRequestApproved {
		t.Fatalf("Task after approval = status %s pending %#v err=%v", details.Task.Status, details.PendingRequest, err)
	}

	type controlResult struct {
		ControlState   string `json:"control_state"`
		ControlVersion int64  `json:"control_version"`
	}
	changeControl := func(action control.Action, requestID string, expected int64, wantState string) int64 {
		t.Helper()
		response := request(http.MethodPost, "/api/hosts/"+hostValue.ID+"/control/actions", map[string]any{
			"action": action, "request_id": requestID, "expected_control_version": expected,
		}, "")
		var result controlResult
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || result.ControlState != wantState || result.ControlVersion <= expected {
			t.Fatalf("Host control %s = status %d state=%s version=%d; want %s and a newer fence", action,
				response.StatusCode, result.ControlState, result.ControlVersion, wantState)
		}
		return result.ControlVersion
	}
	userControlVersion := changeControl(control.ActionTakeOver, "f01-approval-fence-takeover", hostValue.ControlVersion, string(host.ControlUser))
	currentControlVersion := changeControl(control.ActionRelease, "f01-approval-fence-release", userControlVersion, string(host.ControlAI))
	if currentControlVersion == originalApproval.ControlVersion {
		t.Fatalf("Host control fence did not change: old=%d new=%d", originalApproval.ControlVersion, currentControlVersion)
	}

	resumedWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: f01RecordingExecutor{inner: executor, errors: executionErrors, supervisor: supervisor, tools: toolBroker},
		WorkerID: "f01-approval-fence-resume", LeaseDuration: 30 * time.Second, PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeCancel, resumeDone := startWorker(resumedWorker)
	defer stopF01Worker(t, resumeCancel, resumeDone, "resumed TaskWorker")

	var renewedApproval task.PendingRequest
	if err := waitF01(t, ctx, func() (bool, error) {
		current, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		if current.Task.Status != task.StatusWaitingUser || current.Task.WaitingReason != "approval_required" || current.PendingRequest == nil {
			return false, nil
		}
		renewedApproval = *current.PendingRequest
		return true, nil
	}, executionErrors, nil); err != nil {
		t.Fatalf("Task did not pause for a new approval after the Host fence changed: %v", err)
	}
	if renewedApproval.Kind != "approval" || renewedApproval.Status != task.PendingRequestPending ||
		renewedApproval.ID == originalApproval.ID || renewedApproval.Version != originalApproval.Version+1 ||
		renewedApproval.ControlVersion != currentControlVersion || renewedApproval.CallID != originalApproval.CallID {
		t.Fatalf("fresh approval is not bound to the new Host fence: old=%#v new=%#v Host control=%d",
			originalApproval, renewedApproval, currentControlVersion)
	}
	current, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || current.Task.Usage.ToolCalls != details.Task.Usage.ToolCalls || approvalDesktop.calls.Load() != 0 {
		t.Fatalf("stale approval dispatched or recounted: current=%#v calls=%d err=%v", current.Task, approvalDesktop.calls.Load(), err)
	}
	if _, err := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-desktop-click-001")); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("old approval created a Host operation, err=%v", err)
	}
	updatedHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || updatedHost.ControlState != host.ControlAI || updatedHost.ControlVersion != currentControlVersion {
		t.Fatalf("Host did not return to AI control at new fence: %#v err=%v", updatedHost, err)
	}
	if model.callCount() != 1 {
		t.Fatalf("dsh advanced past the stale approval checkpoint: model calls=%d", model.callCount())
	}
}
