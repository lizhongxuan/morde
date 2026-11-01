package runtimehttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
)

func TestHealthAndReadiness(t *testing.T) {
	handler, manager, _ := newHTTPHandler(t, 1)
	defer manager.Close()
	for _, path := range []string{"/healthz", "/readyz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
			t.Fatalf("GET %s response = %d %q", path, response.Code, response.Body.String())
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "not ready\n" {
		t.Fatalf("unready GET /readyz response = %d %q", response.Code, response.Body.String())
	}
}

func TestCapabilitiesAuthProtocolAndStrictQuery(t *testing.T) {
	handler, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	validURL := "/runtime/v1/capabilities?protocol_version=1.0&host_id=h1234567&runtime_generation=1&control_version=0"
	newRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodGet, validURL, nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
		return request
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, validURL, nil))
	assertError(t, response, http.StatusUnauthorized, "runtime_authentication_failed")

	request := newRequest()
	request.Header.Set(contract.ProtocolHeader, "2.0")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusConflict, "protocol_incompatible")

	request = newRequest()
	request.URL.RawQuery += "&host_id=h1234567"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	request = newRequest()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d body=%s", response.Code, response.Body.String())
	}
	var capabilities contract.CapabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.ProtocolVersion != contract.ProtocolVersion || capabilities.RuntimeInstanceID == "" || !capabilities.OperationStoreReady {
		t.Fatalf("invalid capabilities response: %#v", capabilities)
	}
}

func TestTaskWorkspaceExportRoutesReturnOnlyRegisteredViewBlocks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "wabc1234"), 0o700); err != nil {
		t.Fatal(err)
	}
	taskRoot := filepath.Join(t.TempDir(), "task-views")
	workspace, err := runtimefiles.OpenTaskWorkspace(root, taskRoot, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: newRuntimeDir(t), ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 1,
		Handlers: workspace.Handlers(), TaskWorkspaceViews: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close test runtime manager: %v", err)
		}
	})
	credential := testCredential()
	handler, err := NewHandler(manager, credential)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := contract.HostFence{HostID: capabilities.HostID, RuntimeGeneration: capabilities.RuntimeGeneration,
		RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion}
	if _, err := manager.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), contract.SetWorkspaceBindingsRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence,
		Bindings: []contract.WorkspaceBinding{{WorkspaceID: "wabc1234", BindingVersion: 1, Directory: "wabc1234"}},
	}); err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256([]byte("morde-workspace-manifest/v1\n[]"))
	inputDigest := hex.EncodeToString(empty[:])
	view, err := manager.SetTaskWorkspaceView(context.Background(), "qabc1234", contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", WorkspaceID: "wabc1234",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, ManifestSHA256: inputDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	viewDirectory := filepath.Join(taskRoot, "qabc1234", view.ViewID)
	if err := os.WriteFile(filepath.Join(viewDirectory, "result.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRequest := contract.TaskWorkspaceManifestRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", WorkspaceID: "wabc1234",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, InputManifestSHA256: inputDigest,
		ViewID: view.ViewID, Limit: 10,
	}
	encoded, err := json.Marshal(manifestRequest)
	if err != nil {
		t.Fatal(err)
	}
	response := postJSON(handler, credential, "/runtime/v1/tasks/qabc1234/workspace-view/manifest", string(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("manifest status=%d body=%s", response.Code, response.Body.String())
	}
	var page contract.TaskWorkspaceManifestResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	wantBlockSHA := sha256.Sum256([]byte("hello"))
	blockSHA := hex.EncodeToString(wantBlockSHA[:])
	if page.HostFence != fence || page.TaskID != "qabc1234" || page.InputManifestSHA256 != inputDigest || len(page.Files) != 1 ||
		page.Files[0].Path != "result.txt" || page.Files[0].Chunks[0].SHA256 != blockSHA || page.ManifestSHA256 == inputDigest {
		t.Fatalf("manifest page = %#v", page)
	}
	blockRequest := contract.TaskWorkspaceBlockRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", WorkspaceID: "wabc1234",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, InputManifestSHA256: inputDigest,
		ViewID: view.ViewID, ManifestSHA256: page.ManifestSHA256, SHA256: blockSHA,
	}
	encoded, err = json.Marshal(blockRequest)
	if err != nil {
		t.Fatal(err)
	}
	response = postJSON(handler, credential, "/runtime/v1/tasks/qabc1234/workspace-view/block", string(encoded))
	if response.Code != http.StatusOK {
		t.Fatalf("block status=%d body=%s", response.Code, response.Body.String())
	}
	var block contract.TaskWorkspaceBlockResponse
	if err := json.Unmarshal(response.Body.Bytes(), &block); err != nil {
		t.Fatal(err)
	}
	if block.HostFence != fence || block.SHA256 != blockSHA || block.SizeBytes != 5 || string(block.Data) != "hello" {
		t.Fatalf("block response = %#v", block)
	}
	blockRequest.ControlVersion++
	encoded, _ = json.Marshal(blockRequest)
	response = postJSON(handler, credential, "/runtime/v1/tasks/qabc1234/workspace-view/block", string(encoded))
	assertError(t, response, http.StatusConflict, "fence_rejected")
}

func TestDeleteTaskWorkspaceViewRouteRequiresExactFenceAndRequestID(t *testing.T) {
	handler, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	fence := currentFence(t, manager, 1)
	requestBody, err := json.Marshal(contract.DeleteTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1234", RequestID: "zabc1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/runtime/v1/tasks/qabc1234/workspace-view", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete Task Workspace view status=%d body=%s", response.Code, response.Body.String())
	}
	var deleted contract.DeleteTaskWorkspaceViewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &deleted); err != nil || !deleted.Deleted || deleted.TaskID != "qabc1234" || deleted.RequestID != "zabc1234" || deleted.HostFence != fence {
		t.Fatalf("delete Task Workspace view response=%#v err=%v", deleted, err)
	}

	stale := contract.DeleteTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "qabc1235", RequestID: "zabc1235",
	}
	stale.ControlVersion++
	requestBody, _ = json.Marshal(stale)
	request = httptest.NewRequest(http.MethodDelete, "/runtime/v1/tasks/qabc1235/workspace-view", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusConflict, "fence_rejected")
}

func TestApplicationsReturnsOnlyValidatedStatusAndExactFence(t *testing.T) {
	h, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	implementation := h.(*handler)
	implementation.probeCodex = func(context.Context) contract.ApplicationStatusItem {
		return contract.ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3", LoginStatus: contract.ApplicationLoginLoggedIn}
	}
	implementation.installer = &testApplications{item: contract.ApplicationStatusItem{
		ID: "wechat", Installed: true, Version: "4.1.13.26", LoginStatus: contract.ApplicationLoginUnknown,
	}}
	url := "/runtime/v1/applications?protocol_version=1.0&host_id=h1234567&runtime_generation=1&control_version=0"
	request := httptest.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET applications status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("runtime applications cache headers = %q / %q", response.Header().Get("Cache-Control"), response.Header().Get("Pragma"))
	}
	var got contract.ApplicationsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	fence := currentFence(t, manager, 1)
	if got.ProtocolVersion != contract.ProtocolVersion || got.HostID != fence.HostID || got.RuntimeGeneration != fence.RuntimeGeneration ||
		got.RuntimeInstanceID != fence.RuntimeInstanceID || got.ControlVersion != fence.ControlVersion || got.CheckedAt.IsZero() ||
		len(got.Items) != 2 || got.Items[0] != (contract.ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3", LoginStatus: contract.ApplicationLoginLoggedIn}) ||
		got.Items[1] != (contract.ApplicationStatusItem{ID: "wechat", Installed: true, Version: "4.1.13.26", LoginStatus: contract.ApplicationLoginUnknown}) {
		t.Fatalf("invalid applications response: %#v", got)
	}
	for _, secret := range []string{"account-secret", "token-secret", "stdout", "stderr", "command"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("applications response leaked %q: %s", secret, response.Body.String())
		}
	}
}

func TestApplicationsRejectsFenceMismatchBeforeProbeAndInvalidProbe(t *testing.T) {
	h, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	implementation := h.(*handler)
	probes := 0
	implementation.probeCodex = func(context.Context) contract.ApplicationStatusItem {
		probes++
		return contract.ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3", LoginStatus: contract.ApplicationLoginLoggedIn}
	}
	requestURL := func(generation, control string) string {
		return "/runtime/v1/applications?protocol_version=1.0&host_id=h1234567&runtime_generation=" + generation + "&control_version=" + control
	}
	for _, test := range []struct {
		name       string
		generation string
		control    string
	}{
		{name: "wrong generation", generation: "2", control: "0"},
		{name: "wrong control version", generation: "1", control: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, requestURL(test.generation, test.control), nil)
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			assertError(t, response, http.StatusConflict, "fence_rejected")
		})
	}
	if probes != 0 {
		t.Fatalf("probe called %d times on stale fences", probes)
	}

	implementation.probeCodex = func(context.Context) contract.ApplicationStatusItem {
		probes++
		return contract.ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3 account-secret", LoginStatus: contract.ApplicationLoginLoggedIn}
	}
	request := httptest.NewRequest(http.MethodGet, requestURL("1", "0"), nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	assertError(t, response, http.StatusServiceUnavailable, "runtime_state_unavailable")
	if probes != 1 {
		t.Fatalf("probe calls after invalid item = %d, want 1", probes)
	}
	_ = manager
}

func TestApplicationsRequiresRuntimeAuthenticationAndStrictQuery(t *testing.T) {
	h, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	implementation := h.(*handler)
	implementation.probeCodex = func(context.Context) contract.ApplicationStatusItem {
		t.Fatal("probe must not run for invalid or unauthenticated request")
		return contract.ApplicationStatusItem{}
	}
	url := "/runtime/v1/applications?protocol_version=1.0&host_id=h1234567&runtime_generation=1&control_version=0"
	request := httptest.NewRequest(http.MethodGet, url, nil)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	assertError(t, response, http.StatusUnauthorized, "runtime_authentication_failed")

	request = httptest.NewRequest(http.MethodGet, url+"&extra=1", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	_ = manager
}

func TestStrictJSONAndRequestLimit(t *testing.T) {
	handler, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	fence := currentFence(t, manager, 1)
	requestBody := `{"protocol_version":"1.0","host_id":"h1234567","runtime_generation":1,"runtime_instance_id":"` + fence.RuntimeInstanceID + `","control_version":0,"mode":"blocked","mode":"ai"}`
	request := httptest.NewRequest(http.MethodPut, "/runtime/v1/control", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	request = httptest.NewRequest(http.MethodPut, "/runtime/v1/control", strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	request = httptest.NewRequest(http.MethodPut, "/runtime/v1/control", strings.NewReader(`{"protocol_version":"1.0"}`))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "text/plain")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
}

func TestExecuteUnsupportedActionAndCrossGenerationQueryEnvelope(t *testing.T) {
	dir := newRuntimeDir(t)
	first, err := runtimecore.Open(runtimecore.Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	credential := testCredential()
	firstHandler, err := NewHandler(first, credential)
	if err != nil {
		t.Fatal(err)
	}
	fence := currentFence(t, first, 1)
	if _, err := first.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	lease := contract.SetTaskLeaseRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LeaseVersion: 1, ExpiresAt: time.Now().UTC().Add(20 * time.Second)}
	if _, err := first.SetTaskLease("q1234567", lease); err != nil {
		t.Fatal(err)
	}
	execute := contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: "q1234567", OperationID: "o1234567", LeaseVersion: 1,
		Capability: "browser.actions", Action: "navigate", Arguments: json.RawMessage(`{"url":"https://example.test"}`),
	}
	executeBody, _ := json.Marshal(execute)
	response := postJSON(firstHandler, credential, "/runtime/v1/operations", string(executeBody))
	assertError(t, response, http.StatusConflict, "capability_unavailable")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := runtimecore.Open(runtimecore.Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondHandler, err := NewHandler(second, credential)
	if err != nil {
		t.Fatal(err)
	}
	current := currentFence(t, second, 2)
	queryURL := "/runtime/v1/operations/o1234567?host_id=h1234567&runtime_generation=2&runtime_instance_id=" + current.RuntimeInstanceID + "&control_version=0&task_id=q1234567&lease_version=1"
	request := httptest.NewRequest(http.MethodGet, queryURL, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response = httptest.NewRecorder()
	secondHandler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("cross-generation query status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope contract.OperationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.RequestFence != current || envelope.Operation.RuntimeGeneration != 1 || envelope.Operation.RuntimeInstanceID != fence.RuntimeInstanceID || envelope.Operation.Status != contract.OperationFailed {
		t.Fatalf("cross-generation response lost request/original fences: %#v", envelope)
	}
}

func TestErrorBodiesExposeOnlyStableCodeAndStatus(t *testing.T) {
	handler, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	fence := currentFence(t, manager, 1)
	queryURL := "/runtime/v1/operations/o1234567?host_id=h1234567&runtime_generation=1&runtime_instance_id=" + fence.RuntimeInstanceID + "&control_version=0&task_id=q1234567&lease_version=1"
	request := httptest.NewRequest(http.MethodGet, queryURL, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusNotFound, "operation_not_found")
	_ = manager
}

func TestResponseLimitIsEnforcedBeforeWriting(t *testing.T) {
	response := httptest.NewRecorder()
	err := writeJSON(response, map[string]string{"text": strings.Repeat("x", MaxResponseBytes)})
	if err != runtimecore.ErrResultTooLarge || response.Code != 200 || response.Body.Len() != 0 {
		t.Fatalf("oversized response err=%v status=%d bytes=%d", err, response.Code, response.Body.Len())
	}
}

func TestWeChatInstallIsOwnerFenceBoundAndPollable(t *testing.T) {
	_, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	fence := currentFence(t, manager, 1)
	operation := contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: "wechat", Status: contract.OperationAccepted,
		ProgressPercent: 0, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	applications := &testApplications{
		item: contract.ApplicationStatusItem{ID: "wechat", LoginStatus: contract.ApplicationLoginNotApplicable, Installable: true},
		install: func(applicationID, requestID string) (contract.ApplicationInstallOperation, error) {
			if applicationID != "wechat" {
				t.Fatalf("application ID = %q", applicationID)
			}
			if requestID != "req-123" {
				t.Fatalf("request ID = %q", requestID)
			}
			return operation, nil
		},
	}
	handler, err := NewHandlerWithApplications(manager, credential, applications)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(contract.ApplicationInstallRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, RequestID: "req-123"})
	if err != nil {
		t.Fatal(err)
	}
	response := postJSON(handler, credential, "/runtime/v1/applications/wechat/install", string(body))
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("install response status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var got contract.ApplicationInstallResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !contract.ValidApplicationInstallResponse(got) || got.HostFence != fence || got.Operation.ID != operation.ID || got.Operation.Status != contract.OperationAccepted {
		t.Fatalf("invalid install response: %#v", got)
	}
	if strings.Contains(response.Body.String(), "request_fence") {
		t.Fatalf("install response has a non-flat fence envelope: %s", response.Body.String())
	}

	applications.item.InstallOperation = &contract.ApplicationInstallOperation{
		ID: operation.ID, ApplicationID: "wechat", Status: contract.OperationRunning,
		ProgressPercent: 42, CreatedAt: operation.CreatedAt, UpdatedAt: time.Now().UTC(),
	}
	applications.item.InstallRequestID = "req-123"
	query := "/runtime/v1/applications?protocol_version=1.0&host_id=h1234567&runtime_generation=1&control_version=0"
	request := httptest.NewRequest(http.MethodGet, query, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("poll response status=%d body=%s", response.Code, response.Body.String())
	}
	var listed contract.ApplicationsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 2 || listed.Items[1].InstallOperation == nil || listed.Items[1].InstallOperation.ProgressPercent != 42 ||
		listed.Items[1].InstallRequestID != "req-123" {
		t.Fatalf("install progress missing from applications list: %#v", listed.Items)
	}
}

func TestWeChatInstallRejectsInvalidRequestsAndSerializesFenceUpdates(t *testing.T) {
	_, manager, credential := newHTTPHandler(t, 1)
	defer manager.Close()
	fence := currentFence(t, manager, 1)
	operation := contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: "wechat", Status: contract.OperationAccepted,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installCalls := 0
	applications := &testApplications{
		item: contract.ApplicationStatusItem{ID: "wechat", LoginStatus: contract.ApplicationLoginNotApplicable, Installable: true},
		install: func(_, _ string) (contract.ApplicationInstallOperation, error) {
			installCalls++
			return operation, nil
		},
	}
	handler, err := NewHandlerWithApplications(manager, credential, applications)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(contract.ApplicationInstallRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, RequestID: "request-1"})
	response := postJSON(handler, credential, "/runtime/v1/applications/qq/install", string(body))
	assertError(t, response, http.StatusConflict, "capability_unavailable")
	response = postJSON(handler, credential, "/runtime/v1/applications/wechat/install", strings.TrimSuffix(string(body), "}")+`,"url":"https://evil.test"}`)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	if installCalls != 0 {
		t.Fatalf("installer called for invalid requests %d times", installCalls)
	}

	controlUpdate := make(chan error, 1)
	applications.install = func(_, _ string) (contract.ApplicationInstallOperation, error) {
		installCalls++
		advancedFence := fence
		advancedFence.ControlVersion++
		go func() {
			_, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: advancedFence, Mode: contract.ModeBlocked})
			controlUpdate <- err
		}()
		return operation, nil
	}
	response = postJSON(handler, credential, "/runtime/v1/applications/wechat/install", string(body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("install racing a later control update status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case err := <-controlUpdate:
		if err != nil {
			t.Fatalf("control update after committed install: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control update did not proceed after operation commit")
	}
	if installCalls != 1 {
		t.Fatalf("installer calls after race = %d, want 1", installCalls)
	}
	response = postJSON(handler, credential, "/runtime/v1/applications/wechat/install", string(body))
	assertError(t, response, http.StatusConflict, "fence_rejected")
	if installCalls != 1 {
		t.Fatalf("stale-fence request reached installer; calls=%d", installCalls)
	}
}

type testApplications struct {
	item    contract.ApplicationStatusItem
	install func(string, string) (contract.ApplicationInstallOperation, error)
}

func (a *testApplications) Statuses() []contract.ApplicationStatusItem {
	return []contract.ApplicationStatusItem{a.item}
}
func (a *testApplications) Install(applicationID, requestID string) (contract.ApplicationInstallOperation, error) {
	return a.install(applicationID, requestID)
}

func newHTTPHandler(t *testing.T, generation int64) (http.Handler, *runtimecore.Manager, string) {
	t.Helper()
	manager, err := runtimecore.Open(runtimecore.Options{StoreDir: newRuntimeDir(t), ExpectedOwnerUID: uint32(os.Geteuid()), HostID: "h1234567", Generation: generation})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close test runtime manager: %v", err)
		}
	})
	credential := testCredential()
	handler, err := NewHandler(manager, credential)
	if err != nil {
		t.Fatal(err)
	}
	return handler, manager, credential
}

func currentFence(t *testing.T, manager *runtimecore.Manager, generation int64) contract.HostFence {
	t.Helper()
	response, err := manager.Capabilities(contract.CapabilitiesRequest{ProtocolVersion: contract.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: generation, ControlVersion: 0})
	if err != nil {
		t.Fatal(err)
	}
	return contract.HostFence{HostID: response.HostID, RuntimeGeneration: response.RuntimeGeneration, RuntimeInstanceID: response.RuntimeInstanceID, ControlVersion: response.ControlVersion}
}

func postJSON(handler http.Handler, credential, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 2 || value["protocol_version"] != contract.ProtocolVersion || value["code"] != code {
		t.Fatalf("error response leaked fields or had wrong code: %#v", value)
	}
}

func testCredential() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32))
}

func newRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
