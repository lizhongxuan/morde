package runtimeclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"

	"morde/server/internal/domain"
)

const (
	hostID      = "h1234567"
	taskID      = "q1234567"
	operationID = "o1234567"
)

var runtimeCredential = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

func TestNewRequiresVerifiedHTTPSURLAnd256BitCredential(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	if _, err := New(server.URL, runtimeCredential, server.Client()); err != nil {
		t.Fatalf("New(valid input) error = %v", err)
	}
	if _, err := New("http://runtime.internal", runtimeCredential, server.Client()); err == nil {
		t.Fatal("New(http URL) succeeded")
	}
	if _, err := New(server.URL, "short", server.Client()); err == nil {
		t.Fatal("New(short credential) succeeded")
	}
	if _, err := New(server.URL, runtimeCredential, nil); err == nil {
		t.Fatal("New(nil HTTP client) succeeded")
	}
}

func TestTaskWorkspaceExportClientChecksExactViewAndBlockBytes(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	viewID := "v" + strings.Repeat("a", 64)
	inputDigest := strings.Repeat("b", 64)
	manifestDigest := strings.Repeat("c", 64)
	blockDigest := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	var corrupt atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runtime/v1/tasks/" + taskID + "/workspace-view/manifest":
			if r.Method != http.MethodPost || r.Header.Get(ProtocolHeader) != ProtocolVersion {
				t.Errorf("manifest request = %s %s", r.Method, r.URL.Path)
			}
			var request TaskWorkspaceManifestRequest
			readJSON(t, r, &request)
			responseFence := request.HostFence
			if corrupt.Load() {
				responseFence.ControlVersion++
			}
			writeJSON(t, w, TaskWorkspaceManifestResponse{
				ProtocolVersion: ProtocolVersion, HostFence: responseFence, TaskID: taskID, WorkspaceID: request.WorkspaceID,
				BindingVersion: request.BindingVersion, InputRevision: request.InputRevision,
				WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, InputManifestSHA256: request.InputManifestSHA256,
				ViewID: viewID, ManifestSHA256: manifestDigest,
				Files: []contract.TaskWorkspaceManifestFile{{Path: "hello.txt", SizeBytes: 5, SHA256: blockDigest, Mode: 0o600,
					Chunks: []contract.TaskWorkspaceManifestChunk{{SHA256: blockDigest, SizeBytes: 5}}}},
				EmptyDirectories: []string{"empty"},
			})
		case "/runtime/v1/tasks/" + taskID + "/workspace-view/block":
			var request TaskWorkspaceBlockRequest
			readJSON(t, r, &request)
			writeJSON(t, w, TaskWorkspaceBlockResponse{
				ProtocolVersion: ProtocolVersion, HostFence: request.HostFence, TaskID: taskID, WorkspaceID: request.WorkspaceID,
				BindingVersion: request.BindingVersion, InputRevision: request.InputRevision,
				WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, InputManifestSHA256: request.InputManifestSHA256,
				ViewID: viewID, ManifestSHA256: manifestDigest, SHA256: blockDigest, SizeBytes: 5, Data: []byte("hello"),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	manifestRequest := TaskWorkspaceManifestRequest{
		HostFence: fence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, InputRevision: 0,
		WorkspaceCheckpointRevision: 0, InputManifestSHA256: inputDigest, ViewID: viewID, Limit: 10,
	}
	page, err := client.TaskWorkspaceManifestPage(context.Background(), taskID, manifestRequest)
	if err != nil || len(page.Files) != 1 || len(page.EmptyDirectories) != 1 || page.ManifestSHA256 != manifestDigest {
		t.Fatalf("TaskWorkspaceManifestPage() = %#v, %v", page, err)
	}
	blockRequest := TaskWorkspaceBlockRequest{
		HostFence: fence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, InputRevision: 0,
		WorkspaceCheckpointRevision: 0, InputManifestSHA256: inputDigest, ViewID: viewID,
		ManifestSHA256: manifestDigest, SHA256: blockDigest,
	}
	block, err := client.ReadTaskWorkspaceBlock(context.Background(), taskID, blockRequest)
	if err != nil || string(block.Data) != "hello" {
		t.Fatalf("ReadTaskWorkspaceBlock() = %#v, %v", block, err)
	}
	corrupt.Store(true)
	if _, err := client.TaskWorkspaceManifestPage(context.Background(), taskID, manifestRequest); err == nil {
		t.Fatal("TaskWorkspaceManifestPage accepted a response with a different Host fence")
	}
	blockRequest.SHA256 = strings.ToUpper(blockRequest.SHA256)
	if _, err := client.ReadTaskWorkspaceBlock(context.Background(), taskID, blockRequest); err == nil {
		t.Fatal("ReadTaskWorkspaceBlock accepted a non-canonical block hash")
	}
}

func TestTaskWorkspaceManifestResponseValidatesDirectoryOnlyPages(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	viewID := "v" + strings.Repeat("a", 64)
	request := TaskWorkspaceManifestRequest{
		HostFence: fence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, InputRevision: 0,
		WorkspaceCheckpointRevision: 0, InputManifestSHA256: strings.Repeat("b", 64), ViewID: viewID,
		AfterPath: "before", Limit: 1,
	}
	response := TaskWorkspaceManifestResponse{
		ProtocolVersion: ProtocolVersion, HostFence: fence, TaskID: taskID, WorkspaceID: request.WorkspaceID,
		BindingVersion: request.BindingVersion, InputRevision: request.InputRevision,
		WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, InputManifestSHA256: request.InputManifestSHA256,
		ViewID: viewID, ManifestSHA256: strings.Repeat("c", 64),
		Files: []contract.TaskWorkspaceManifestFile{}, EmptyDirectories: []string{"docs"}, NextCursor: "docs",
	}
	if !taskWorkspaceManifestResponseMatches(response, taskID, request) {
		t.Fatal("directory-only page with its path cursor was rejected")
	}
	response.NextCursor = "other"
	if taskWorkspaceManifestResponseMatches(response, taskID, request) {
		t.Fatal("directory-only page with a mismatched cursor was accepted")
	}
	response.NextCursor = "docs"
	response.EmptyDirectories = []string{"../outside"}
	if taskWorkspaceManifestResponseMatches(response, taskID, request) {
		t.Fatal("page with an unsafe directory path was accepted")
	}
}

func TestClientMethodsUseProtocolAuthAndFences(t *testing.T) {
	now := time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC)
	currentFence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	viewID := "v" + strings.Repeat("a", 64)
	var sawHandshake, sawControl, sawLease, sawExecute, sawGet, sawCancel, sawQuiesce atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+runtimeCredential {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(ProtocolHeader); got != ProtocolVersion {
			t.Errorf("%s = %q", ProtocolHeader, got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		switch r.URL.Path {
		case "/runtime/v1/capabilities":
			sawHandshake.Store(true)
			if r.Method != http.MethodGet || r.ContentLength != 0 || r.URL.Query().Get("protocol_version") != ProtocolVersion || r.URL.Query().Get("host_id") != hostID || r.URL.Query().Get("runtime_generation") != "2" || r.URL.Query().Get("control_version") != "9" {
				t.Errorf("capabilities request = %s %s, body length %d", r.Method, r.URL.String(), r.ContentLength)
			}
			writeJSON(t, w, CapabilitiesResponse{ProtocolVersion: ProtocolVersion, HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9, Mode: ModeBlocked, OperationStoreReady: true, Capabilities: []string{"operation.execute", "control.fence"}})
		case "/runtime/v1/control":
			sawControl.Store(true)
			var request SetControlRequest
			readJSON(t, r, &request)
			if request.Mode != ModeHuman || request.DrainedReceipt == nil || request.DrainedReceipt.TargetControlVersion != 10 || request.ControlVersion != 10 {
				t.Errorf("control request = %#v", request)
			}
			writeJSON(t, w, SetControlResponse{ProtocolVersion: ProtocolVersion, HostFence: request.HostFence, Mode: request.Mode})
		case "/runtime/v1/tasks/" + taskID + "/lease":
			sawLease.Store(true)
			var request SetTaskLeaseRequest
			readJSON(t, r, &request)
			writeJSON(t, w, SetTaskLeaseResponse{ProtocolVersion: ProtocolVersion, HostFence: request.HostFence, TaskID: taskID, LeaseVersion: request.LeaseVersion, ExpiresAt: request.ExpiresAt})
		case "/runtime/v1/operations":
			sawExecute.Store(true)
			var request ExecuteRequest
			readJSON(t, r, &request)
			writeJSON(t, w, OperationResponse{ProtocolVersion: ProtocolVersion, RequestFence: request.HostFence, Operation: Operation{HostID: request.HostID, RuntimeGeneration: request.RuntimeGeneration, RuntimeInstanceID: request.RuntimeInstanceID, ControlVersion: request.ControlVersion, TaskID: request.TaskID, WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion, WorkspaceViewID: request.WorkspaceViewID, WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, OperationID: request.OperationID, LeaseVersion: request.LeaseVersion, Status: OperationSucceeded, Result: json.RawMessage(`{"text":"done"}`)}})
		case "/runtime/v1/operations/" + operationID:
			sawGet.Store(true)
			query := r.URL.Query()
			if query.Get("host_id") != hostID || query.Get("runtime_generation") != "2" || query.Get("runtime_instance_id") != "ri_current" || query.Get("control_version") != "9" || query.Get("task_id") != taskID || query.Get("lease_version") != "4" {
				t.Errorf("get query = %s", r.URL.RawQuery)
			}
			writeJSON(t, w, OperationResponse{ProtocolVersion: ProtocolVersion, RequestFence: currentFence, Operation: Operation{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_previous", ControlVersion: 7, TaskID: taskID, OperationID: operationID, LeaseVersion: 4, Status: OperationSucceeded, Result: json.RawMessage(`{"persisted":true}`)}})
		case "/runtime/v1/operations/" + operationID + "/cancel":
			sawCancel.Store(true)
			var request CancelOperationRequest
			readJSON(t, r, &request)
			writeJSON(t, w, OperationResponse{ProtocolVersion: ProtocolVersion, RequestFence: request.HostFence, Operation: Operation{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_previous", ControlVersion: 7, TaskID: taskID, OperationID: operationID, LeaseVersion: 4, Status: OperationCancelled}})
		case "/runtime/v1/quiesce":
			sawQuiesce.Store(true)
			var request QuiesceRequest
			readJSON(t, r, &request)
			writeJSON(t, w, QuiesceResponse{ProtocolVersion: ProtocolVersion, Status: QuiesceDrained, PendingOperationIDs: []string{}, DrainedReceipt: &DrainedReceipt{ReceiptID: "receipt-1", HostID: hostID, RuntimeGeneration: request.RuntimeGeneration, RuntimeInstanceID: request.RuntimeInstanceID, BlockedControlVersion: request.ControlVersion, TargetControlVersion: request.TargetControlVersion, TaskIDs: request.TaskIDs, IssuedAt: now}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	capabilities, err := client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 2, ControlVersion: 9})
	if err != nil || capabilities.RuntimeInstanceID != "ri_current" {
		t.Fatalf("Capabilities() = %#v, %v", capabilities, err)
	}

	blocked := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	receipt := &DrainedReceipt{ReceiptID: "receipt-1", HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", BlockedControlVersion: 9, TargetControlVersion: 10, TaskIDs: []string{taskID}, IssuedAt: now}
	setControl, err := client.SetControl(context.Background(), SetControlRequest{HostFence: HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 10}, Mode: ModeHuman, DrainedReceipt: receipt})
	if err != nil || setControl.Mode != ModeHuman {
		t.Fatalf("SetControl() = %#v, %v", setControl, err)
	}

	expiresAt := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)
	lease, err := client.SetTaskLease(context.Background(), taskID, SetTaskLeaseRequest{HostFence: blocked, LeaseVersion: 4, ExpiresAt: expiresAt})
	if err != nil || lease.LeaseVersion != 4 {
		t.Fatalf("SetTaskLease() = %#v, %v", lease, err)
	}

	operation, err := client.Execute(context.Background(), ExecuteRequest{HostFence: blocked, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: 3, OperationID: operationID, LeaseVersion: 4, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{"workspace_id":"w1234567","command":"true"}`)})
	if err != nil || operation.Status != OperationSucceeded {
		t.Fatalf("Execute() = %#v, %v", operation, err)
	}

	oldOperation, err := client.GetOperation(context.Background(), operationID, OperationQuery{HostFence: blocked, TaskID: taskID, LeaseVersion: 4})
	if err != nil || oldOperation.RuntimeGeneration != 1 || oldOperation.RuntimeInstanceID != "ri_previous" {
		t.Fatalf("GetOperation() = %#v, %v", oldOperation, err)
	}

	cancelled, err := client.CancelOperation(context.Background(), operationID, CancelOperationRequest{HostFence: blocked})
	if err != nil || cancelled.Status != OperationCancelled || cancelled.RuntimeGeneration != 1 {
		t.Fatalf("CancelOperation() = %#v, %v", cancelled, err)
	}

	quiesced, err := client.Quiesce(context.Background(), QuiesceRequest{HostFence: blocked, TargetControlVersion: 10, TaskIDs: []string{taskID}})
	if err != nil || quiesced.Status != QuiesceDrained || quiesced.DrainedReceipt == nil {
		t.Fatalf("Quiesce() = %#v, %v", quiesced, err)
	}

	if !sawHandshake.Load() || !sawControl.Load() || !sawLease.Load() || !sawExecute.Load() || !sawGet.Load() || !sawCancel.Load() || !sawQuiesce.Load() {
		t.Fatal("not all RuntimeControl methods issued their HTTP requests")
	}
}

func TestSetTaskWorkspaceViewEchoesTheExactTaskCheckpoint(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	request := SetTaskWorkspaceViewRequest{
		HostFence: fence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 4,
		InputRevision: 3, WorkspaceCheckpointRevision: 3, ManifestSHA256: strings.Repeat("b", 64),
	}
	viewID := "v" + strings.Repeat("a", 64)
	var mismatch atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/runtime/v1/tasks/"+taskID+"/workspace-view" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var got SetTaskWorkspaceViewRequest
		readJSON(t, r, &got)
		if got.ProtocolVersion != ProtocolVersion || got.HostFence != fence || got.TaskID != taskID || got.WorkspaceID != request.WorkspaceID ||
			got.BindingVersion != request.BindingVersion || got.InputRevision != request.InputRevision ||
			got.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision || got.ManifestSHA256 != request.ManifestSHA256 {
			t.Errorf("task view request = %#v", got)
		}
		response := SetTaskWorkspaceViewResponse{
			ProtocolVersion: ProtocolVersion, HostFence: fence, TaskID: taskID, WorkspaceID: request.WorkspaceID,
			BindingVersion: request.BindingVersion, InputRevision: request.InputRevision,
			WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, ManifestSHA256: request.ManifestSHA256, ViewID: viewID,
		}
		if mismatch.Load() {
			response.WorkspaceCheckpointRevision++
		}
		writeJSON(t, w, response)
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.SetTaskWorkspaceView(context.Background(), taskID, request)
	if err != nil || response.ViewID != viewID {
		t.Fatalf("SetTaskWorkspaceView() = %#v, %v", response, err)
	}
	mismatch.Store(true)
	if _, err := client.SetTaskWorkspaceView(context.Background(), taskID, request); err == nil {
		t.Fatal("SetTaskWorkspaceView accepted a response for another checkpoint")
	}
	request.ManifestSHA256 = strings.ToUpper(request.ManifestSHA256)
	if _, err := client.SetTaskWorkspaceView(context.Background(), taskID, request); err == nil {
		t.Fatal("SetTaskWorkspaceView accepted a non-canonical digest")
	}
}

func TestApplicationsUsesCurrentFenceAndRejectsUnvalidatedResponses(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	checkedAt := "2026-10-21T03:04:05Z"
	validBody := `{"protocol_version":"1.0","host_id":"` + hostID + `","runtime_generation":2,"runtime_instance_id":"ri_current","control_version":9,"checked_at":"` + checkedAt + `","items":[{"id":"codex","installed":true,"version":"1.2.3","login_status":"logged_in"},{"id":"wechat","installed":false,"version":"","login_status":"not_applicable","installable":true,"install_operation":null}]}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/runtime/v1/applications" || r.ContentLength != 0 {
			t.Errorf("applications request = %s %s content_length=%d", r.Method, r.URL.String(), r.ContentLength)
		}
		query := r.URL.Query()
		if query.Get("protocol_version") != ProtocolVersion || query.Get("host_id") != fence.HostID ||
			query.Get("runtime_generation") != "2" || query.Get("control_version") != "9" || len(query) != 4 {
			t.Errorf("applications query = %s", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer "+runtimeCredential || r.Header.Get(ProtocolHeader) != ProtocolVersion {
			t.Errorf("applications auth/protocol headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get(ProtocolHeader))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validBody)
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Applications(context.Background(), fence)
	if err != nil {
		t.Fatalf("Applications() error = %v", err)
	}
	if got.HostID != fence.HostID || got.RuntimeGeneration != fence.RuntimeGeneration || got.RuntimeInstanceID != fence.RuntimeInstanceID ||
		got.ControlVersion != fence.ControlVersion || got.CheckedAt.Format(time.RFC3339) != checkedAt || len(got.Items) != 2 ||
		got.Items[0] != (ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3", LoginStatus: ApplicationLoginLoggedIn}) {
		t.Fatalf("Applications() = %#v", got)
	}
	installOperation := `{"id":"app_1234","application_id":"wechat","status":"running","progress_percent":20,"error_code":"","created_at":"` + checkedAt + `","updated_at":"` + checkedAt + `"}`
	progressBody := strings.Replace(validBody, `"install_operation":null`, `"install_request_id":"install-wechat-01","install_operation":`+installOperation, 1)
	progressServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, progressBody)
	}))
	defer progressServer.Close()
	progressClient, err := New(progressServer.URL, runtimeCredential, progressServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	progress, err := progressClient.Applications(context.Background(), fence)
	if err != nil || progress.Items[1].InstallOperation == nil || progress.Items[1].InstallOperation.ProgressPercent != 20 ||
		progress.Items[1].InstallRequestID != "install-wechat-01" {
		t.Fatalf("Applications() install progress = %#v, %v", progress, err)
	}

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "wrong host", body: strings.Replace(validBody, `"host_id":"`+hostID+`"`, `"host_id":"h8765432"`, 1)},
		{name: "wrong generation", body: strings.Replace(validBody, `"runtime_generation":2`, `"runtime_generation":3`, 1)},
		{name: "wrong runtime instance", body: strings.Replace(validBody, `"runtime_instance_id":"ri_current"`, `"runtime_instance_id":"ri_old"`, 1)},
		{name: "wrong control version", body: strings.Replace(validBody, `"control_version":9`, `"control_version":8`, 1)},
		{name: "missing timestamp", body: strings.Replace(validBody, `,"checked_at":"`+checkedAt+`"`, "", 1)},
		{name: "non UTC timestamp", body: strings.Replace(validBody, checkedAt, "2026-10-21T04:04:05+01:00", 1)},
		{name: "invalid enum", body: strings.Replace(validBody, `"logged_in"`, `"maybe"`, 1)},
		{name: "extra field", body: strings.Replace(validBody, `"id":"codex"`, `"id":"codex","account":"private"`, 1)},
		{name: "duplicate fence field", body: strings.Replace(validBody, `"runtime_generation":2`, `"runtime_generation":2,"runtime_generation":3`, 1)},
		{name: "non UTC install timestamp", body: strings.Replace(progressBody, checkedAt, "2026-10-21T04:04:05+01:00", 1)},
		{name: "install operation missing request ID", body: strings.Replace(progressBody, `"install_request_id":"install-wechat-01",`, "", 1)},
		{name: "install operation invalid request ID", body: strings.Replace(progressBody, `"install_request_id":"install-wechat-01"`, `"install_request_id":"bad key"`, 1)},
		{name: "request ID without install operation", body: strings.Replace(validBody, `"install_operation":null`, `"install_request_id":"install-wechat-01","install_operation":null`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			badServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer badServer.Close()
			badClient, err := New(badServer.URL, runtimeCredential, badServer.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = badClient.Applications(context.Background(), fence)
			var callError *Error
			if !errors.As(err, &callError) || callError.Code != ErrorInvalidResponse {
				t.Fatalf("Applications() error = %#v, want invalid response", err)
			}
		})
	}
}

func TestInstallApplicationUsesCurrentFenceAndRejectsMalformedResponses(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	requestID := "install-wechat-01"
	createdAt := "2026-10-21T03:04:05Z"
	validBody := `{"protocol_version":"1.0","host_id":"` + hostID + `","runtime_generation":2,"runtime_instance_id":"ri_current","control_version":9,"operation":{"id":"app_1234","application_id":"wechat","status":"accepted","progress_percent":0,"error_code":"","created_at":"` + createdAt + `","updated_at":"` + createdAt + `"}}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/runtime/v1/applications/wechat/install" || r.URL.RawQuery != "" {
			t.Errorf("install request = %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer "+runtimeCredential || r.Header.Get(ProtocolHeader) != ProtocolVersion {
			t.Errorf("install auth/protocol headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get(ProtocolHeader))
		}
		var request ApplicationInstallRequest
		readJSON(t, r, &request)
		if request.ProtocolVersion != ProtocolVersion || request.HostFence != fence || request.RequestID != requestID {
			t.Errorf("install request body = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validBody)
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.InstallApplication(context.Background(), "wechat", ApplicationInstallRequest{HostFence: fence, RequestID: requestID})
	if err != nil {
		t.Fatalf("InstallApplication() error = %v", err)
	}
	if got.Operation.ID != "app_1234" || got.Operation.ApplicationID != "wechat" || got.Operation.Status != OperationAccepted ||
		got.Operation.ProgressPercent != 0 || got.Operation.ErrorCode != "" || got.Operation.CreatedAt.Format(time.RFC3339) != createdAt {
		t.Fatalf("InstallApplication() operation = %#v", got.Operation)
	}

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "wrong host fence", body: strings.Replace(validBody, `"host_id":"`+hostID+`"`, `"host_id":"h8765432"`, 1)},
		{name: "wrong generation", body: strings.Replace(validBody, `"runtime_generation":2`, `"runtime_generation":3`, 1)},
		{name: "wrong runtime instance", body: strings.Replace(validBody, `"runtime_instance_id":"ri_current"`, `"runtime_instance_id":"ri_old"`, 1)},
		{name: "wrong control version", body: strings.Replace(validBody, `"control_version":9`, `"control_version":8`, 1)},
		{name: "wrong application", body: strings.Replace(validBody, `"application_id":"wechat"`, `"application_id":"codex"`, 1)},
		{name: "invalid operation id", body: strings.Replace(validBody, `"id":"app_1234"`, `"id":""`, 1)},
		{name: "invalid status", body: strings.Replace(validBody, `"status":"accepted"`, `"status":"unknown"`, 1)},
		{name: "invalid progress", body: strings.Replace(validBody, `"progress_percent":0`, `"progress_percent":101`, 1)},
		{name: "failed without safe code", body: strings.Replace(validBody, `"status":"accepted"`, `"status":"failed"`, 1)},
		{name: "non UTC timestamp", body: strings.Replace(validBody, createdAt, "2026-10-21T04:04:05+01:00", 1)},
		{name: "extra field", body: strings.Replace(validBody, `"id":"app_1234"`, `"id":"app_1234","output":"secret"`, 1)},
		{name: "duplicate field", body: strings.Replace(validBody, `"status":"accepted"`, `"status":"accepted","status":"running"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			badServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer badServer.Close()
			badClient, err := New(badServer.URL, runtimeCredential, badServer.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = badClient.InstallApplication(context.Background(), "wechat", ApplicationInstallRequest{HostFence: fence, RequestID: requestID})
			var clientErr *Error
			if !errors.As(err, &clientErr) || clientErr.Code != ErrorInvalidResponse {
				t.Fatalf("InstallApplication() error = %#v, want invalid response", err)
			}
		})
	}
}

func TestInstallApplicationRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fence := HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}
	for _, test := range []struct {
		name     string
		appID    string
		request  ApplicationInstallRequest
		wantCode string
	}{
		{name: "unsupported app", appID: "codex", request: ApplicationInstallRequest{HostFence: fence, RequestID: "install-1"}, wantCode: ErrorInvalidRequest},
		{name: "invalid request id", appID: "wechat", request: ApplicationInstallRequest{HostFence: fence, RequestID: "bad key"}, wantCode: ErrorInvalidRequest},
		{name: "invalid fence", appID: "wechat", request: ApplicationInstallRequest{HostFence: HostFence{HostID: "bad"}, RequestID: "install-1"}, wantCode: ErrorInvalidRequest},
		{name: "wrong protocol", appID: "wechat", request: ApplicationInstallRequest{ProtocolVersion: "2.0", HostFence: fence, RequestID: "install-1"}, wantCode: ErrorProtocolIncompatible},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.InstallApplication(context.Background(), test.appID, test.request)
			var clientErr *Error
			if !errors.As(err, &clientErr) || clientErr.Code != test.wantCode {
				t.Fatalf("InstallApplication() error = %#v, want %q", err, test.wantCode)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid requests reached Runtime: %d calls", requests.Load())
	}
}

func TestErrorParsingMapsStableCodesAndNeverReturnsServerMessage(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       string
		publicCode domain.ErrorCode
	}{
		{name: "capability", status: http.StatusConflict, code: ErrorCapabilityUnavailable, publicCode: domain.CodeUnsupported},
		{name: "operation conflict", status: http.StatusConflict, code: ErrorOperationConflict, publicCode: domain.CodeIdempotencyConflict},
		{name: "too large", status: http.StatusRequestEntityTooLarge, code: ErrorResultTooLarge, publicCode: domain.CodeResultTooLarge},
		{name: "fence", status: http.StatusConflict, code: ErrorFenceRejected, publicCode: domain.CodeInvalidState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, fmt.Sprintf(`{"protocol_version":%q,"code":%q,"message":"PRIVATE_BODY_SECRET"}`, ProtocolVersion, test.code))
			}))
			defer server.Close()
			client, err := New(server.URL, runtimeCredential, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 2, ControlVersion: 9})
			var callError *Error
			if !errors.As(err, &callError) || callError.Code != test.code || callError.StatusCode != test.status || callError.PublicCode() != test.publicCode {
				t.Fatalf("error = %#v, want code=%q status=%d public=%q", err, test.code, test.status, test.publicCode)
			}
			if strings.Contains(err.Error(), "PRIVATE_BODY_SECRET") {
				t.Fatalf("error exposed response body: %v", err)
			}
		})
	}
}

func TestErrorStatusMismatchAndWrongProtocolFailClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "status mismatch", body: `{"protocol_version":"1.0","code":"capability_unavailable"}`, code: ErrorInvalidResponse},
		{name: "protocol mismatch", body: `{"protocol_version":"2.0","code":"protocol_incompatible"}`, code: ErrorProtocolIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := New(server.URL, runtimeCredential, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 2, ControlVersion: 9})
			var callError *Error
			if !errors.As(err, &callError) || callError.Code != test.code {
				t.Fatalf("error = %#v, want code=%q", err, test.code)
			}
		})
	}
}

func TestResponseBodyIsBounded(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"protocol_version":"1.0","host_id":"`+hostID+`","runtime_generation":2,"runtime_instance_id":"ri_current","control_version":9,"mode":"blocked","operation_store_ready":true,"capabilities":[]}`)
		_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBodyBytes+1))
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 2, ControlVersion: 9})
	var callError *Error
	if !errors.As(err, &callError) || callError.Code != ErrorResponseTooLarge {
		t.Fatalf("error = %#v, want bounded-response error", err)
	}
}

func TestRequestBodyIsBoundedBeforeSending(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{"text": strings.Repeat("x", maxRequestBodyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(context.Background(), ExecuteRequest{HostFence: HostFence{HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "ri_current", ControlVersion: 9}, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: "v" + strings.Repeat("a", 64), WorkspaceCheckpointRevision: 3, OperationID: operationID, LeaseVersion: 4, Capability: "file.write", Action: "write", Arguments: arguments})
	var callError *Error
	if !errors.As(err, &callError) || callError.Code != ErrorInvalidRequest || requests.Load() != 0 {
		t.Fatalf("error = %#v, requests = %d; want local bound rejection before send", err, requests.Load())
	}
}

func TestCanceledContextIsNotRetriedOrRewritten(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Capabilities(ctx, CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 2, ControlVersion: 9})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestValidationHelpersCoverRuntimeFencesAndEnums(t *testing.T) {
	validFence := HostFence{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", ControlVersion: 0}
	if err := validateHandshake(CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1}); err != nil {
		t.Fatalf("valid handshake: %v", err)
	}
	for _, request := range []CapabilitiesRequest{
		{HostID: "bad", RuntimeGeneration: 1},
		{HostID: hostID, RuntimeGeneration: 0},
		{HostID: hostID, RuntimeGeneration: 1, ControlVersion: -1},
	} {
		if err := validateHandshake(request); err == nil {
			t.Errorf("validateHandshake(%#v) succeeded", request)
		}
	}
	if err := validateHostFence(validFence); err != nil {
		t.Fatalf("valid fence: %v", err)
	}
	for _, fence := range []HostFence{
		{HostID: "bad", RuntimeGeneration: 1, RuntimeInstanceID: "ri_1"},
		{HostID: hostID, RuntimeGeneration: 0, RuntimeInstanceID: "ri_1"},
		{HostID: hostID, RuntimeGeneration: 1},
		{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", ControlVersion: -1},
	} {
		if err := validateHostFence(fence); err == nil {
			t.Errorf("validateHostFence(%#v) succeeded", fence)
		}
	}

	validOperationRequest := ExecuteRequest{HostFence: validFence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, WorkspaceViewID: "v" + strings.Repeat("a", 64), WorkspaceCheckpointRevision: 0, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{"workspace_id":"w1234567"}`)}
	if err := validateExecute(validOperationRequest); err != nil {
		t.Fatalf("valid execute request: %v", err)
	}
	for _, request := range []ExecuteRequest{
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{"workspace_id":"w1234567"}`)},
		{HostFence: validFence, TaskID: taskID, WorkspaceID: "w1234567", OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{"workspace_id":"w1234567"}`)},
		{HostFence: validFence, TaskID: taskID, WorkspaceID: "w1234567", BindingVersion: 1, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{"workspace_id":"w1234567"}`)},
	} {
		if err := validateExecute(request); err == nil {
			t.Errorf("validateExecute(%#v) accepted a missing Workspace binding field", request)
		}
	}
	invalidOperationRequests := []ExecuteRequest{
		{HostFence: validFence, TaskID: "bad", OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{}`)},
		{HostFence: validFence, TaskID: taskID, OperationID: "bad", LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{}`)},
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 0, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`{}`)},
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 1, Action: "run", Arguments: json.RawMessage(`{}`)},
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Arguments: json.RawMessage(`{}`)},
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`nope`)},
		{HostFence: validFence, TaskID: taskID, OperationID: operationID, LeaseVersion: 1, Capability: "shell.exec", Action: "run", Arguments: json.RawMessage(`[]`)},
	}
	for _, request := range invalidOperationRequests {
		if err := validateExecute(request); err == nil {
			t.Errorf("validateExecute(%#v) succeeded", request)
		}
	}

	for _, value := range []string{hostID, taskID, operationID} {
		if len(value) != 8 || !validID(value, value[0]) {
			t.Errorf("validID(%q) rejected valid ID", value)
		}
	}
	for _, value := range []string{"", "h123456", "h123456!", "H1234567"} {
		if validID(value, 'h') {
			t.Errorf("validID(%q) accepted malformed ID", value)
		}
	}
	if !validTaskIDs([]string{taskID}) || !validTaskIDs([]string{}) || validTaskIDs([]string{taskID, taskID}) || validTaskIDs([]string{"bad"}) {
		t.Fatal("validTaskIDs did not enforce unique task IDs")
	}
	if !validMode(ModeBlocked) || !validMode(ModeAI) || !validMode(ModeHuman) || validMode("other") {
		t.Fatal("validMode accepted/rejected an unexpected value")
	}
	for _, status := range []OperationStatus{OperationAccepted, OperationRunning, OperationSucceeded, OperationFailed, OperationCancelling, OperationCancelled, OperationUnknown} {
		if !validOperationStatus(status) {
			t.Errorf("validOperationStatus(%q) = false", status)
		}
	}
	if validOperationStatus("other") || !validQuiesceStatus(QuiesceDraining) || !validQuiesceStatus(QuiesceDrained) || !validQuiesceStatus(QuiesceFailed) || validQuiesceStatus("other") {
		t.Fatal("runtime state validators accepted/rejected an unexpected value")
	}
	if !isUTC(time.Now().UTC()) || isUTC(time.Time{}) || isUTC(time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("plus one", 3600))) {
		t.Fatal("isUTC did not validate zero-offset timestamps")
	}
	if !sameStringSet([]string{"a", "b"}, []string{"b", "a"}) || sameStringSet([]string{"a", "a"}, []string{"a", "b"}) || sameStringSet([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("sameStringSet did not compare multiplicity and length")
	}
}

func TestRuntimeErrorMappingsAreStable(t *testing.T) {
	cases := []struct {
		code   string
		status int
		public domain.ErrorCode
	}{
		{ErrorInvalidRequest, http.StatusBadRequest, domain.CodeInvalidArgument},
		{ErrorAuthenticationFailed, http.StatusUnauthorized, domain.CodeUnauthenticated},
		{ErrorFenceRejected, http.StatusConflict, domain.CodeInvalidState},
		{ErrorOperationConflict, http.StatusConflict, domain.CodeIdempotencyConflict},
		{ErrorProtocolIncompatible, http.StatusConflict, domain.CodeUpstreamError},
		{ErrorCapabilityUnavailable, http.StatusConflict, domain.CodeUnsupported},
		{ErrorOperationNotFound, http.StatusNotFound, domain.CodeNotFound},
		{ErrorResultTooLarge, http.StatusRequestEntityTooLarge, domain.CodeResultTooLarge},
		{ErrorRuntimeStateUnavailable, http.StatusServiceUnavailable, domain.CodeUpstreamError},
		{ErrorTransport, http.StatusBadGateway, domain.CodeUpstreamError},
		{ErrorTransportTimeout, http.StatusGatewayTimeout, domain.CodeUpstreamTimeout},
		{ErrorResponseTooLarge, http.StatusOK, domain.CodeResultTooLarge},
		{ErrorInvalidResponse, http.StatusBadGateway, domain.CodeUpstreamError},
		{"unknown", 0, domain.CodeUpstreamError},
	}
	for _, test := range cases {
		callError := &Error{Code: test.code, StatusCode: test.status}
		if got := callError.PublicCode(); got != test.public {
			t.Errorf("PublicCode(%q) = %q, want %q", test.code, got, test.public)
		}
		if !strings.Contains(callError.Error(), test.code) {
			t.Errorf("Error() = %q, missing stable code %q", callError.Error(), test.code)
		}
		if _, ok := statusForCode(test.code); ok != (test.status != 0 && test.code != ErrorTransport && test.code != ErrorTransportTimeout && test.code != ErrorResponseTooLarge && test.code != ErrorInvalidResponse && test.code != "unknown") {
			t.Errorf("statusForCode(%q) recognized unexpectedly", test.code)
		}
	}
	if (*Error)(nil).Error() != "runtime request failed" || (*Error)(nil).PublicCode() != domain.CodeUpstreamError {
		t.Fatal("nil Error receiver was not safe")
	}
	if (&Error{}).Error() != "runtime request failed" {
		t.Fatal("empty Error code was not safely formatted")
	}
}

func TestTransportAndSuccessfulResponseValidationFailClosed(t *testing.T) {
	t.Run("capabilities returns the runtime control fence for reconciliation", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("control_version"); got != "4" {
				t.Errorf("requested control_version = %q, want 4", got)
			}
			writeJSON(t, w, CapabilitiesResponse{ProtocolVersion: ProtocolVersion, HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", ControlVersion: 3, Mode: ModeBlocked, OperationStoreReady: true, Capabilities: []string{}})
		}))
		defer server.Close()
		client, err := New(server.URL, runtimeCredential, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1, ControlVersion: 4})
		if err != nil || response.ControlVersion != 3 || response.Mode != ModeBlocked {
			t.Fatalf("Capabilities() = %#v, %v; expected actual runtime fence for reconciliation", response, err)
		}
	})
	t.Run("transport", func(t *testing.T) {
		client, err := New("https://runtime.internal", runtimeCredential, doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("private transport diagnostic")
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1})
		var callError *Error
		if !errors.As(err, &callError) || callError.Code != ErrorTransport || strings.Contains(err.Error(), "private transport diagnostic") {
			t.Fatalf("transport error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		client, err := New("https://runtime.internal", runtimeCredential, doerFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err = client.Capabilities(ctx, CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1})
		var callError *Error
		if !errors.As(err, &callError) || callError.Code != ErrorTransportTimeout || callError.PublicCode() != domain.CodeUpstreamTimeout {
			t.Fatalf("timeout error = %v", err)
		}
	})
	t.Run("wrong success protocol", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, CapabilitiesResponse{ProtocolVersion: "2.0", HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", Mode: ModeBlocked, OperationStoreReady: true, Capabilities: []string{}})
		}))
		defer server.Close()
		client, err := New(server.URL, runtimeCredential, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1})
		var callError *Error
		if !errors.As(err, &callError) || callError.Code != ErrorProtocolIncompatible {
			t.Fatalf("protocol error = %v", err)
		}
	})
	t.Run("non-200 success status", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, CapabilitiesResponse{ProtocolVersion: ProtocolVersion, HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", Mode: ModeBlocked, OperationStoreReady: true, Capabilities: []string{}})
		}))
		defer server.Close()
		client, err := New(server.URL, runtimeCredential, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Capabilities(context.Background(), CapabilitiesRequest{HostID: hostID, RuntimeGeneration: 1})
		var callError *Error
		if !errors.As(err, &callError) || callError.Code != ErrorInvalidResponse {
			t.Fatalf("status error = %v", err)
		}
	})
}

func TestApplicationLaunchMethodsUseExactLaunchIDAndHostFence(t *testing.T) {
	launchID := "launch_client-01"
	fence := HostFence{HostID: hostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_wechat", ControlVersion: 11}
	now := time.Date(2026, 10, 25, 3, 4, 5, 0, time.UTC)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+runtimeCredential || r.Header.Get(ProtocolHeader) != ProtocolVersion {
			t.Errorf("authorization/protocol headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get(ProtocolHeader))
		}
		operation := ApplicationLaunchOperation{ID: launchID, ApplicationID: "wechat", Status: ApplicationLaunchRunning, CreatedAt: now, UpdatedAt: now}
		switch calls {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/runtime/v1/applications/wechat/launch" {
				t.Errorf("launch request = %s %s", r.Method, r.URL.String())
			}
			var request ApplicationLaunchRequest
			readJSON(t, r, &request)
			if request.LaunchID != launchID || request.HostFence != fence || request.ProtocolVersion != ProtocolVersion {
				t.Errorf("launch body = %#v", request)
			}
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/runtime/v1/applications/wechat/launches/"+launchID || r.ContentLength != 0 {
				t.Errorf("status request = %s %s body=%d", r.Method, r.URL.String(), r.ContentLength)
			}
			query := r.URL.Query()
			if len(query) != 5 || query.Get("protocol_version") != ProtocolVersion || query.Get("host_id") != fence.HostID ||
				query.Get("runtime_generation") != "3" || query.Get("runtime_instance_id") != fence.RuntimeInstanceID || query.Get("control_version") != "11" {
				t.Errorf("status query = %s", r.URL.RawQuery)
			}
		case 3:
			if r.Method != http.MethodPost || r.URL.Path != "/runtime/v1/applications/wechat/launches/"+launchID+"/stop" {
				t.Errorf("stop request = %s %s", r.Method, r.URL.String())
			}
			var request ApplicationLaunchRequest
			readJSON(t, r, &request)
			if request.LaunchID != launchID || request.HostFence != fence || request.ProtocolVersion != ProtocolVersion {
				t.Errorf("stop body = %#v", request)
			}
			operation.Status = ApplicationLaunchStopped
		default:
			t.Errorf("unexpected extra Runtime call %d", calls)
		}
		w.WriteHeader(map[int]int{1: http.StatusAccepted, 2: http.StatusOK, 3: http.StatusAccepted}[calls])
		writeJSON(t, w, ApplicationLaunchResponse{ProtocolVersion: ProtocolVersion, HostFence: fence, Operation: operation})
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	callsToMake := []struct {
		name string
		call func() (ApplicationLaunchOperation, error)
	}{
		{"launch", func() (ApplicationLaunchOperation, error) {
			return client.LaunchApplication(context.Background(), "wechat", fence, launchID)
		}},
		{"query", func() (ApplicationLaunchOperation, error) {
			return client.GetApplicationLaunch(context.Background(), "wechat", fence, launchID)
		}},
		{"stop", func() (ApplicationLaunchOperation, error) {
			return client.StopApplication(context.Background(), "wechat", fence, launchID)
		}},
	}
	for _, item := range callsToMake {
		operation, err := item.call()
		if err != nil || operation.ID != launchID {
			t.Errorf("%s() = %#v, %v", item.name, operation, err)
		}
	}
	if calls != 3 {
		t.Fatalf("Runtime call count = %d, want 3", calls)
	}
}

func TestApplicationLaunchClientRejectsMismatchedOperationIdentity(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_wechat", ControlVersion: 0}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, ApplicationLaunchResponse{
			ProtocolVersion: ProtocolVersion,
			HostFence:       fence,
			Operation: ApplicationLaunchOperation{
				ID: "launch_different01", ApplicationID: "wechat", Status: ApplicationLaunchRunning,
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			},
		})
	}))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LaunchApplication(context.Background(), "wechat", fence, "launch_expected01")
	var callError *Error
	if !errors.As(err, &callError) || callError.Code != ErrorInvalidResponse {
		t.Fatalf("mismatched operation ID error = %v", err)
	}
}

func TestApplicationLaunchClientRejectsUnsupportedApplicationBeforeRequest(t *testing.T) {
	fence := HostFence{HostID: hostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_1", ControlVersion: 0}
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client, err := New(server.URL, runtimeCredential, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error {
			_, err := client.LaunchApplication(context.Background(), "qq", fence, "launch_unsupported01")
			return err
		},
		func() error {
			_, err := client.GetApplicationLaunch(context.Background(), "qq", fence, "launch_unsupported01")
			return err
		},
		func() error {
			_, err := client.StopApplication(context.Background(), "qq", fence, "launch_unsupported01")
			return err
		},
	} {
		var callError *Error
		if err := call(); !errors.As(err, &callError) || callError.Code != ErrorInvalidRequest {
			t.Fatalf("unsupported application error = %v", err)
		}
	}
	if calls != 0 {
		t.Fatalf("unsupported application reached Runtime %d times", calls)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func readJSON(t *testing.T, r *http.Request, value any) {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		t.Errorf("read request: %v", err)
		return
	}
	if len(data) > maxRequestBodyBytes {
		t.Errorf("request body exceeded limit: %d bytes", len(data))
		return
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Errorf("decode request: %v", err)
	}
}
