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
	"strings"
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
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

// TestCloudTaskAuthorizationRevocationFencesHostDispatch covers session logout.
func TestCloudTaskAuthorizationRevocationFencesHostDispatch(t *testing.T) {
	runCloudTaskAuthorizationRevocationE2E(t, false)
}

// TestCloudTaskOwnerAccountDisableFencesHostDispatch covers disabling the
// owner account while its Host write request is held in flight.
func TestCloudTaskOwnerAccountDisableFencesHostDispatch(t *testing.T) {
	runCloudTaskAuthorizationRevocationE2E(t, true)
}

// runCloudTaskAuthorizationRevocationE2E uses the real task API, PostgreSQL
// lease/revocation logic, Node dsh subprocess, CloudDSH broker,
// RuntimeToolExecutor, Host Runtime, and TaskStopWorker. The request is held at
// the Host HTTP boundary until revocation cancels it; the real Runtime handler
// must never receive the file-write operation.
func runCloudTaskAuthorizationRevocationE2E(t *testing.T, disableOwnerAccount bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	if err := app.EnsureInitialAdmin(ctx, store); err != nil {
		t.Fatal(err)
	}
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, runtimeCredential, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	if _, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, 1,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatalf("activate empty test Workspace binding: %v", err)
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
			t.Errorf("close Host runtime workspace: %v", err)
		}
	}()
	runtimeStore := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(runtimeStore, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: runtimeStore, ExpectedOwnerUID: uint32(os.Geteuid()),
		HostID: hostValue.ID, Generation: hostValue.RuntimeGeneration, ProtocolVersion: contract.ProtocolVersion,
		Handlers: files.Handlers(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeHandler, err := runtimehttp.NewHandler(runtimeManager, runtimeCredential)
	if err != nil {
		t.Fatal(err)
	}
	gate := &f01RevocationRuntimeGate{
		inner: runtimeHandler, executeEntered: make(chan struct{}), executeCanceled: make(chan struct{}), shutdown: make(chan struct{}),
	}
	runtimeServer := httptest.NewUnstartedServer(gate)
	runtimeServer.TLS, err = runtimeidentity.ServerTLSConfig(runtimeCredential, hostValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer.StartTLS()
	defer func() {
		close(gate.shutdown)
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
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
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
	userService, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	adminSessionCredential := createF01Session(t, ctx, store, admin.ID)
	adminPrincipal, err := credentials.VerifySession(ctx, adminSessionCredential)
	if err != nil || adminPrincipal.Role != identity.RoleSuperAdmin {
		t.Fatalf("verify superadmin session: principal=%#v err=%v", adminPrincipal, err)
	}
	if adminPrincipal.MustChangePassword {
		if err := credentials.ChangePassword(ctx, adminPrincipal, "admin", "f01-admin-test-password"); err != nil {
			t.Fatalf("complete initial admin password change: %v", err)
		}
		adminSessionCredential = createF01Session(t, ctx, store, admin.ID)
	}
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.UserID != owner.ID {
		t.Fatalf("verify task session: principal=%#v err=%v", principal, err)
	}
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: runtimeServer.URL}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	apiRouter, err := api.New(api.Deps{
		Hosts: f01UnusedHostService{}, HostReplicas: hostReplicas, TLSAuthorizer: f01UnusedTLSAuthorizer{},
		Verifier: credentials, Auth: f01RevocationAuth{credentials: credentials}, Tasks: taskService,
		WorkspaceSync: syncService, Admin: f01RevocationAdminAdapter{users: userService}, MCPDisabled: true,
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
	stopKey := bytes.Repeat([]byte{0x6b}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("f01-revoke-supervisor", stopKey)
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
		store: store, hostID: hostValue.ID, workspaceID: workspaceValue.ID,
		secondStarted: make(chan struct{}), content: "this file must not be written after authorization revocation\n",
	}
	supervisor := &f01CapturingSupervisor{child: &cloudDSHTestSupervisor{key: stopKey}}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "f01-revoke-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Models: model, Tools: toolBroker, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	executionDone := make(chan error, 1)
	workerContextCanceled := make(chan struct{}, 1)
	recorder := f01RevocationRecordingExecutor{inner: executor, done: executionDone, contextCanceled: workerContextCanceled}
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: recorder, WorkerID: "f01-revoke-worker", LeaseDuration: 900 * time.Millisecond,
		PollInterval: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	inputRevision := int64(0)
	postBody, err := json.Marshal(map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": inputRevision,
		"model_id": "p1234567", "objective": "Write a note to the attached Workspace.",
		"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	postRequest, err := http.NewRequest(http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(postBody))
	if err != nil {
		t.Fatal(err)
	}
	postRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	postRequest.Header.Set("Content-Type", "application/json")
	postRequest.Header.Set("Origin", portal.URL)
	postRequest.Header.Set("Idempotency-Key", "f01-cloud-revocation-e2e-001")
	postResponse, err := httpClient.Do(postRequest)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(postResponse.Body).Decode(&accepted); err != nil {
		_ = postResponse.Body.Close()
		t.Fatal(err)
	}
	_ = postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) {
		t.Fatalf("POST /api/tasks acceptance = status %d, response %#v", postResponse.StatusCode, accepted)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(workerCtx) }()
	defer stopF01Worker(t, cancelWorker, workerDone, "TaskWorker")

	select {
	case <-gate.executeEntered:
	case runErr := <-executionDone:
		t.Fatalf("dsh execution ended before Host operation boundary: %v", runErr)
	case <-ctx.Done():
		t.Fatalf("dsh did not reach Host operation boundary: %v", ctx.Err())
	}
	current, err := store.GetTask(ctx, accepted.ID, owner.ID)
	operation, operationErr := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
	if err != nil || operationErr != nil || current.Task.Status != task.StatusRunning || operation.Status != task.OperationDispatched {
		t.Fatalf("task at held Host dispatch = status %q operation=%#v task_error=%v operation_error=%v; want running with dispatched operation", current.Task.Status, operation, err, operationErr)
	}

	if disableOwnerAccount {
		requestBody := []byte(`{"expected_status":"approved","status":"disabled"}`)
		disableRequest, err := http.NewRequest(http.MethodPatch, portal.URL+"/api/admin/users/"+owner.ID, bytes.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		disableRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: adminSessionCredential})
		disableRequest.Header.Set("Content-Type", "application/json")
		disableRequest.Header.Set("Origin", portal.URL)
		disableResponse, err := httpClient.Do(disableRequest)
		if err != nil {
			t.Fatal(err)
		}
		var disabled struct {
			Status identity.AccountStatus `json:"status"`
		}
		responseBody, err := io.ReadAll(disableResponse.Body)
		if closeErr := disableResponse.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(responseBody, &disabled); err != nil && disableResponse.StatusCode == http.StatusOK {
			t.Fatalf("decode PATCH /api/admin/users/{id}: body=%s err=%v", responseBody, err)
		}
		if disableResponse.StatusCode != http.StatusOK || disabled.Status != identity.StatusDisabled {
			t.Fatalf("PATCH /api/admin/users/{id} = status %d, body=%s", disableResponse.StatusCode, responseBody)
		}
		if _, err := credentials.VerifySession(ctx, sessionCredential); err == nil {
			t.Fatal("disabled owner session still authenticates")
		}
	} else {
		logoutRequest, err := http.NewRequest(http.MethodPost, portal.URL+"/api/auth/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		logoutRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
		logoutRequest.Header.Set("Origin", portal.URL)
		logoutResponse, err := httpClient.Do(logoutRequest)
		if err != nil {
			t.Fatal(err)
		}
		_ = logoutResponse.Body.Close()
		if logoutResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("POST /api/auth/logout status = %d, want 204", logoutResponse.StatusCode)
		}
	}
	current, err = store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || current.Task.Status != task.StatusCancelling || current.StopRequest == nil || current.StopRequest.Status != task.StopRequestPending {
		t.Fatalf("task after authorization revocation = status %q stop_request=%#v err=%v; want cancelling with pending safe-after proof", current.Task.Status, current.StopRequest, err)
	}

	workerCanceled, executeCanceled, executionFinished := false, false, false
	var executionErr error
	cancelTimer := time.NewTimer(10 * time.Second)
	defer cancelTimer.Stop()
	for !workerCanceled || !executeCanceled {
		select {
		case <-workerContextCanceled:
			workerCanceled = true
		case <-gate.executeCanceled:
			executeCanceled = true
		case executionErr = <-executionDone:
			executionFinished = true
		case <-cancelTimer.C:
			t.Fatalf("revocation cancellation did not reach both TaskWorker and Host request: worker_context=%t host_request=%t execution_finished=%t execution_error=%v", workerCanceled, executeCanceled, executionFinished, executionErr)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for revocation cancellation: %v", ctx.Err())
		}
	}
	if !executionFinished {
		select {
		case executionErr = <-executionDone:
		case <-ctx.Done():
			t.Fatalf("CloudDSH executor did not stop after request cancellation: %v", ctx.Err())
		}
	}
	if executionErr == nil {
		t.Fatal("CloudDSH executor reported success for a revoked, interrupted Host operation")
	}
	if _, err := os.Stat(filepath.Join(workspaceDirectory, "task-note.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Host Workspace file after revocation: stat error=%v, want no file", err)
	}
	current, err = store.GetTask(ctx, accepted.ID, owner.ID)
	operation, operationErr = store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
	if err != nil || operationErr != nil || current.Task.Status != task.StatusCancelling ||
		current.StopRequest == nil || current.StopRequest.Status != task.StopRequestPending ||
		(operation.Status != task.OperationDispatched && operation.Status != task.OperationUnknown) {
		t.Fatalf("task/operation state while Host outcome is unverified = task %q operation=%#v task_error=%v operation_error=%v", current.Task.Status, operation, err, operationErr)
	}

	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store, Executor: runtimeControl, WorkerID: "f01-revoke-stop-worker", LeaseDuration: time.Second,
		PollInterval: 5 * time.Millisecond, OperationTimeout: 2 * time.Second, RetryAfter: 5 * time.Millisecond,
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	stopDone := make(chan error, 1)
	go func() { stopDone <- stopWorker.Run(stopCtx) }()
	defer stopF01Worker(t, cancelStop, stopDone, "TaskStopWorker")
	if err := waitF01(t, ctx, func() (bool, error) {
		details, err := store.GetTask(ctx, accepted.ID, owner.ID)
		if err != nil {
			return false, err
		}
		operation, err := store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
		if err != nil {
			return false, err
		}
		return details.Task.Status == task.StatusCancelled && operation.Status == task.OperationCancelled, nil
	}, nil, nil); err != nil {
		details, getErr := store.GetTask(context.WithoutCancel(ctx), accepted.ID, owner.ID)
		operation, operationErr := store.GetOperation(context.WithoutCancel(ctx), owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
		t.Fatalf("real TaskStopWorker did not settle cancellation after safe-after: task=%q operation=%#v err=%v get_err=%v operation_error=%v", details.Task.Status, operation, err, getErr, operationErr)
	}
	final, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = store.GetOperation(ctx, owner.ID, accepted.ID, f01OperationKey("f01-write-note-001"))
	if err != nil {
		t.Fatal(err)
	}
	if final.Task.Status != task.StatusCancelled || operation.Status != task.OperationCancelled ||
		final.StopRequest == nil || final.StopRequest.Status != task.StopRequestCompleted || !strings.Contains(operation.Result, "safe-after") {
		t.Fatalf("verified revoked task state = task %q operation %#v", final.Task.Status, operation)
	}
	if _, err := os.Stat(filepath.Join(workspaceDirectory, "task-note.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Host Workspace file after safe-after proof: stat error=%v, want no late write", err)
	}
}

type f01RevocationRuntimeGate struct {
	inner           http.Handler
	executeEntered  chan struct{}
	executeCanceled chan struct{}
	shutdown        chan struct{}
}

func (g *f01RevocationRuntimeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/runtime/v1/operations" {
		_, _ = io.Copy(io.Discard, r.Body)
		close(g.executeEntered)
		select {
		case <-r.Context().Done():
			close(g.executeCanceled)
		case <-g.shutdown:
		}
		return
	}
	g.inner.ServeHTTP(w, r)
}

type f01RevocationAuth struct {
	api.AuthService
	credentials *app.CredentialService
}

func (a f01RevocationAuth) Logout(ctx context.Context, principal identity.Principal) error {
	return a.credentials.Logout(ctx, principal)
}

type f01RevocationAdminAdapter struct {
	users *app.UserService
}

func (f01RevocationAdminAdapter) ListUsers(context.Context, identity.Principal, *identity.AccountStatus) ([]api.AdminUserRecord, error) {
	return nil, errors.New("user listing is not used by this E2E")
}

func (a f01RevocationAdminAdapter) UpdateUser(ctx context.Context, principal identity.Principal, id string, command api.UpdateAdminUserCommand) (api.AdminUserRecord, error) {
	var quota *app.ResourceUsage
	if command.Quota != nil {
		quota = &app.ResourceUsage{Hosts: command.Quota.Hosts, CPUMilli: command.Quota.CPUMilli, MemMiB: command.Quota.MemMiB}
	}
	var defaultHostResources *app.HostResources
	if command.DefaultHostResources != nil {
		defaultHostResources = &app.HostResources{CPUMilli: command.DefaultHostResources.CPUMilli, MemMiB: command.DefaultHostResources.MemMiB}
	}
	updated, err := a.users.Update(ctx, principal, id, app.UpdateUserCommand{
		ExpectedStatus: command.ExpectedStatus, Role: command.Role, Status: command.Status,
		Quota: quota, DefaultHostResources: defaultHostResources,
	})
	if err != nil {
		return api.AdminUserRecord{}, err
	}
	return api.AdminUserRecord{
		ID: updated.ID, Name: updated.Name, Role: updated.Role, Status: updated.Status,
		Quota:                api.TotalsView{Hosts: updated.Quota.Hosts, CPUMilli: updated.Quota.CPUMilli, MemMiB: updated.Quota.MemMiB},
		Usage:                api.TotalsView{Hosts: updated.Usage.Hosts, CPUMilli: updated.Usage.CPUMilli, MemMiB: updated.Usage.MemMiB},
		DefaultHostResources: api.HostResourcesView{CPUMilli: updated.DefaultHostResources.CPUMilli, MemMiB: updated.DefaultHostResources.MemMiB},
		CreatedAt:            updated.CreatedAt,
	}, nil
}

func (f01RevocationAdminAdapter) Capacity(context.Context, identity.Principal) (api.CapacityRecord, error) {
	return api.CapacityRecord{}, errors.New("capacity is not used by this E2E")
}

func (f01RevocationAdminAdapter) ListAudit(context.Context, identity.Principal, api.AuditQuery) ([]api.AuditRecord, error) {
	return nil, errors.New("audit listing is not used by this E2E")
}

type f01RevocationRecordingExecutor struct {
	inner           TaskExecutor
	done            chan<- error
	contextCanceled chan<- struct{}
}

func (e f01RevocationRecordingExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	go func() {
		<-ctx.Done()
		e.contextCanceled <- struct{}{}
	}()
	err := e.inner.ExecuteTask(ctx, value)
	e.done <- err
	return err
}

func stopF01Worker(t *testing.T, cancel context.CancelFunc, done <-chan error, name string) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s.Run() = %v, want context canceled during cleanup", name, err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("%s did not stop", name)
	}
}
