package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

func TestCloudTaskApprovalIsRevalidatedAfterHostControlFenceChanges(t *testing.T) {
	testCloudTaskApprovalIsRevalidatedAfterAuthorizationChange(t, false)
}

func TestCloudTaskApprovalIsCancelledAfterHostMemberGrantRevocation(t *testing.T) {
	testCloudTaskApprovalIsRevalidatedAfterAuthorizationChange(t, true)
}

func submitF01SharedHostTask(t *testing.T, ctx context.Context, store *storepostgres.Store,
	taskOwnerID, actorID, projectID, hostID, credential string) string {
	t.Helper()
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	kind, tokenID, _, err := identity.ParseCredential(credential)
	if err != nil || kind != identity.KindSession {
		t.Fatalf("parse shared Host Task session: kind=%q err=%v", kind, err)
	}
	objective := "Click the test desktop target after approval."
	expiresAt := time.Now().UTC().Add(time.Hour)
	requestHash := sha256.Sum256([]byte("f01-shared-host-task:" + taskID))
	_, err = store.SubmitTask(ctx, app.TaskSubmission{
		Conversation: &conversation.Conversation{ID: conversationID, OwnerID: taskOwnerID, ProjectID: projectID,
			Title: objective, ModelID: "p1234567"},
		Task: task.Task{ID: taskID, OwnerID: taskOwnerID, InitiatorID: actorID, ProjectID: projectID, ConversationID: conversationID,
			HostID: hostID, ModelID: "p1234567", Objective: objective, Status: task.StatusQueued,
			Authorization: task.Authorization{Kind: string(kind), TokenID: tokenID, Version: 1, ExpiresAt: &expiresAt},
			Budget:        task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Message:      conversation.Message{ID: messageID, OwnerID: taskOwnerID, ActorID: actorID, ConversationID: conversationID, Role: conversation.RoleUser, Content: objective},
		InitialEvent: task.Event{Type: "task.status", Payload: []byte(`{"status":"queued"}`)},
		Idempotency: app.IdempotencyRecord{OwnerID: actorID, Operation: "submit-task", Key: "f01-shared-host-task-" + taskID,
			RequestHash: requestHash[:]},
	})
	if err != nil {
		t.Fatalf("submit Task authorized by shared Host grant: %v", err)
	}
	return taskID
}

func createF01ProjectMember(t *testing.T, ctx context.Context, store *storepostgres.Store, ownerID, memberID string) string {
	t.Helper()
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	projectValue, err := project.New(projectID, ownerID, "F01 shared Host project", "Host grant revocation test")
	if err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := store.CreateProject(ctx, ownerID, projectValue, "f01-host-grant-project-create"); err != nil || replayed {
		t.Fatalf("create project for shared Host Task: replay=%t err=%v", replayed, err)
	}
	invitationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateProjectInvitation(ctx, ownerID, project.Invitation{ID: invitationID, ProjectID: projectID,
		InviterID: ownerID, InviteeID: memberID, Role: project.RoleMember, State: project.InvitationPending, Version: 1},
		projectValue.Version, "f01-host-grant-project-invite"); err != nil {
		t.Fatalf("invite shared Host Task actor to project: %v", err)
	}
	if _, _, err := store.AcceptProjectInvitation(ctx, invitationID, memberID, "f01-host-grant-project-accept"); err != nil {
		t.Fatalf("accept shared Host Task project invitation: %v", err)
	}
	return projectID
}

func testCloudTaskApprovalIsRevalidatedAfterAuthorizationChange(t *testing.T, revokeHostMember bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	hostOwner := owner
	taskOwnerID := owner.ID
	projectID := ""
	if revokeHostMember {
		hostOwner = createF01User(t, ctx, store)
		taskOwnerID = hostOwner.ID
	}
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if hostOwner.ID != owner.ID {
		if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, hostOwner.ID); err != nil {
			t.Fatal(err)
		}
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, hostOwner.ID)
	hostValue, runtimeCredential, _ := createF01Host(t, ctx, store, sealer, hostOwner.ID, workspaceValue.ID)
	if hostOwner.ID != owner.ID {
		if _, err := store.UpsertHostMember(ctx, host.Member{HostID: hostValue.ID, OwnerID: hostOwner.ID, UserID: owner.ID,
			Permissions: []host.Permission{host.PermissionDesktopControl}}); err != nil {
			t.Fatalf("grant Task initiator Host control: %v", err)
		}
		projectID = createF01ProjectMember(t, ctx, store, hostOwner.ID, owner.ID)
	}
	binding := hostValue.Workspaces[0]
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision); err != nil {
		t.Fatalf("activate stable Agent Workspace binding: %v", err)
	}
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
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x23}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalFiles, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatal(err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, runtimeTools, nil, nil,
		recordCapture, personalData, personalSync, goals, personalFiles, nil)
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
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate real morde-cloud-runtime entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x47}, 32)
	instances, conversationRuntimeManager, supervisor := newF01ConversationRuntime(t, store, node, script, "f01-approval-fence-supervisor", stopKey)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	model := &f01CloudTaskModel{
		approvalFirst: true,
		secondStarted: make(chan struct{}), content: "unused after stale approval",
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: "f01-approval-fence-supervisor", AccountRuntimes: conversationRuntimeManager,
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

	requestAs := func(credential, method, path string, payload any, key string) *http.Response {
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
		req.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
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
	request := func(method, path string, payload any, key string) *http.Response {
		return requestAs(sessionCredential, method, path, payload, key)
	}

	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	var response *http.Response
	if revokeHostMember {
		accepted.ID = submitF01SharedHostTask(t, ctx, store, taskOwnerID, owner.ID, projectID, hostValue.ID, sessionCredential)
		accepted.Status = string(task.StatusQueued)
	} else {
		response = request(http.MethodPost, "/api/tasks", map[string]any{
			"host_id": hostValue.ID, "model_id": "p1234567", "objective": "Click the test desktop target after approval.",
			"budget": map[string]int64{"active_seconds": 120, "tool_calls": 5, "model_tokens": 1000},
		}, "f01-approval-fence-submit-001")
		responseBody, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("read POST /api/tasks response: %v", err)
		}
		if err := json.Unmarshal(responseBody, &accepted); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) {
			t.Fatalf("POST /api/tasks = %d, %#v, body=%s", response.StatusCode, accepted, responseBody)
		}
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
	var initialDetails app.TaskDetails
	if err := waitF01(t, ctx, func() (bool, error) {
		details, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
		if err != nil {
			return false, err
		}
		initialDetails = details
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
	if revokeHostMember {
		stopF01Worker(t, firstCancel, firstDone, "initial TaskWorker")
		firstStopped = true
		leaseVersionBeforeRevocation := initialDetails.Task.Lease.Version
		if err := store.RevokeHostMember(ctx, hostValue.ID, hostOwner.ID, owner.ID); err != nil {
			t.Fatalf("revoke Task initiator Host grant while approval is pending: %v", err)
		}
		revoked, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
		if err != nil || revoked.Task.Status != task.StatusCancelling || revoked.Task.WaitingReason != "" ||
			revoked.Task.Lease.Version <= leaseVersionBeforeRevocation || revoked.PendingRequest == nil ||
			revoked.PendingRequest.ID != originalApproval.ID || revoked.PendingRequest.Status != task.PendingRequestCancelled ||
			revoked.StopRequest == nil || revoked.StopRequest.Status != task.StopRequestPending {
			t.Fatalf("Host grant revocation did not fence the approval wait: status=%q waiting=%q lease=%d approval=%#v stop=%#v err=%v",
				revoked.Task.Status, revoked.Task.WaitingReason, revoked.Task.Lease.Version, revoked.PendingRequest, revoked.StopRequest, err)
		}
		responseHash := sha256.Sum256([]byte("late approval after Host member revocation"))
		lateApproval := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.pending.respond",
			Key: "f01-host-member-revocation-late-approval", RequestHash: responseHash[:], ResourceID: accepted.ID, RelatedID: originalApproval.ID}
		if err := store.RespondToPendingRequest(ctx, lateApproval, accepted.ID, originalApproval.ID,
			originalApproval.Version, task.PendingDecisionApprove); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("late approval after Host grant revocation = %v, want conflict", err)
		}
		stillFenced, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
		if err != nil || stillFenced.Task.Status != task.StatusCancelling || stillFenced.PendingRequest == nil ||
			stillFenced.PendingRequest.Status != task.PendingRequestCancelled || model.callCount() != 1 || approvalDesktop.calls.Load() != 0 {
			t.Fatalf("late approval changed Host-revoked Task: status=%q approval=%#v model_calls=%d Host_calls=%d err=%v",
				stillFenced.Task.Status, stillFenced.PendingRequest, model.callCount(), approvalDesktop.calls.Load(), err)
		}
		stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
			Store: store, Executor: runtimeControl, TaskAgentStopper: conversationRuntimeManager,
			WorkerID: "f01-host-member-revoke-stop", LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond,
			OperationTimeout: 2 * time.Second, RetryAfter: 5 * time.Millisecond, Logger: zap.NewNop(),
		})
		if err != nil {
			t.Fatal(err)
		}
		stopCtx, stopTaskWorker := context.WithCancel(context.Background())
		stopDone := make(chan error, 1)
		go func() { stopDone <- stopWorker.Run(stopCtx) }()
		defer stopF01Worker(t, stopTaskWorker, stopDone, "Host grant revocation TaskStopWorker")
		if err := waitF01(t, ctx, func() (bool, error) {
			final, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
			if err != nil {
				return false, err
			}
			return final.Task.Status == task.StatusCancelled, nil
		}, executionErrors, nil); err != nil {
			final, readErr := store.GetTask(context.WithoutCancel(ctx), accepted.ID, taskOwnerID)
			t.Fatalf("TaskStopWorker did not settle Host-revoked Task: status=%q err=%v read_err=%v", final.Task.Status, err, readErr)
		}
		if model.callCount() != 1 || approvalDesktop.calls.Load() != 0 {
			t.Fatalf("Host grant revocation caused new work: model calls=%d Host calls=%d", model.callCount(), approvalDesktop.calls.Load())
		}
		return
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
	details, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
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
		current, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
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
	current, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
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

	// Revoke the authorization while DSH is waiting for the renewed approval.
	// The request, Task lease fence, and stop intent must change atomically;
	// a separate still-valid session may inspect the Task but cannot reuse the
	// approval that was created under the revoked authorization.
	observerCredential := createF01Session(t, ctx, store, owner.ID)
	observerPrincipal, err := credentials.VerifySession(ctx, observerCredential)
	if err != nil || observerPrincipal.UserID != owner.ID {
		t.Fatalf("verify independent post-revocation observer session: principal=%#v err=%v", observerPrincipal, err)
	}
	leaseVersionBeforeRevocation := current.Task.Lease.Version
	_, revokedTokenID, _, err := identity.ParseCredential(sessionCredential)
	if err != nil {
		t.Fatalf("parse original Task authorization: %v", err)
	}
	if err := store.RevokeToken(ctx, revokedTokenID, owner.ID); err != nil {
		t.Fatalf("revoke Task authorization while approval is pending: %v", err)
	}
	revoked, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
	if err != nil || revoked.Task.Status != task.StatusCancelling || revoked.Task.WaitingReason != "" ||
		revoked.Task.Lease.Version <= leaseVersionBeforeRevocation || revoked.PendingRequest == nil ||
		revoked.PendingRequest.ID != renewedApproval.ID || revoked.PendingRequest.Status != task.PendingRequestCancelled ||
		revoked.StopRequest == nil || revoked.StopRequest.Status != task.StopRequestPending {
		t.Fatalf("revoked approval wait did not atomically fence the Task: status=%q waiting=%q lease=%d approval=%#v stop=%#v err=%v",
			revoked.Task.Status, revoked.Task.WaitingReason, revoked.Task.Lease.Version, revoked.PendingRequest, revoked.StopRequest, err)
	}
	oldCredentialRead := requestAs(sessionCredential, http.MethodGet, "/api/tasks/"+accepted.ID, nil, "")
	oldCredentialStatus := oldCredentialRead.StatusCode
	_ = oldCredentialRead.Body.Close()
	if oldCredentialStatus != http.StatusUnauthorized {
		t.Fatalf("revoked Task session GET = %d, want unauthorized", oldCredentialStatus)
	}
	observerRead := requestAs(observerCredential, http.MethodGet, "/api/tasks/"+accepted.ID, nil, "")
	var observerSnapshot struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(observerRead.Body).Decode(&observerSnapshot); err != nil {
		_ = observerRead.Body.Close()
		t.Fatalf("decode Task for independent session: %v", err)
	}
	_ = observerRead.Body.Close()
	if observerRead.StatusCode != http.StatusOK || observerSnapshot.ID != accepted.ID || observerSnapshot.Status != string(task.StatusCancelling) {
		t.Fatalf("independent session Task snapshot = status %d %#v, want the same cancelling Task", observerRead.StatusCode, observerSnapshot)
	}
	lateApproval := requestAs(observerCredential, http.MethodPost,
		"/api/tasks/"+accepted.ID+"/approvals/"+renewedApproval.ID+"/responses",
		map[string]any{"version": renewedApproval.Version, "decision": "approve"}, "f01-approval-revoked-late-response")
	lateApprovalStatus := lateApproval.StatusCode
	_ = lateApproval.Body.Close()
	if lateApprovalStatus == http.StatusAccepted {
		t.Fatal("a fresh session reused the approval that authorization revocation had cancelled")
	}
	stillFenced, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
	if err != nil || stillFenced.Task.Status != task.StatusCancelling || stillFenced.PendingRequest == nil ||
		stillFenced.PendingRequest.Status != task.PendingRequestCancelled || model.callCount() != 1 || approvalDesktop.calls.Load() != 0 {
		t.Fatalf("late approval changed revoked Task execution: task=%q approval=%#v model_calls=%d Host_calls=%d err=%v",
			stillFenced.Task.Status, stillFenced.PendingRequest, model.callCount(), approvalDesktop.calls.Load(), err)
	}

	stopWorker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store, Executor: runtimeControl, TaskAgentStopper: conversationRuntimeManager,
		WorkerID: "f01-approval-revoke-stop", LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond,
		OperationTimeout: 2 * time.Second, RetryAfter: 5 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, stopTaskWorker := context.WithCancel(context.Background())
	stopDone := make(chan error, 1)
	go func() { stopDone <- stopWorker.Run(stopCtx) }()
	defer stopF01Worker(t, stopTaskWorker, stopDone, "approval-revocation TaskStopWorker")
	if err := waitF01(t, ctx, func() (bool, error) {
		final, err := store.GetTask(ctx, accepted.ID, taskOwnerID)
		if err != nil {
			return false, err
		}
		return final.Task.Status == task.StatusCancelled, nil
	}, executionErrors, nil); err != nil {
		final, readErr := store.GetTask(context.WithoutCancel(ctx), accepted.ID, taskOwnerID)
		t.Fatalf("TaskStopWorker did not settle revoked approval Task: status=%q err=%v read_err=%v", final.Task.Status, err, readErr)
	}
	finalResponse := requestAs(observerCredential, http.MethodGet, "/api/tasks/"+accepted.ID, nil, "")
	var finalSnapshot struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(finalResponse.Body).Decode(&finalSnapshot); err != nil {
		_ = finalResponse.Body.Close()
		t.Fatalf("decode final revoked Task: %v", err)
	}
	_ = finalResponse.Body.Close()
	if finalResponse.StatusCode != http.StatusOK || finalSnapshot.ID != accepted.ID || finalSnapshot.Status != string(task.StatusCancelled) {
		t.Fatalf("post-stop Task snapshot = status %d %#v, want cancelled same Task", finalResponse.StatusCode, finalSnapshot)
	}
	if model.callCount() != 1 || approvalDesktop.calls.Load() != 0 {
		t.Fatalf("revoked approval caused additional execution: model calls=%d Host calls=%d", model.callCount(), approvalDesktop.calls.Load())
	}
}
