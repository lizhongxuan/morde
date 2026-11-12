package runtimecore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/internal/operationstore"
)

func TestCapabilitiesAndSameVersionAIModeReconciliation(t *testing.T) {
	manager, fence := openManager(t, 1)
	capabilities, err := manager.Capabilities(capabilityRequest(fence))
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Mode != contract.ModeBlocked || !capabilities.OperationStoreReady {
		t.Fatalf("initial capabilities = %#v", capabilities)
	}
	for _, unavailable := range []string{"desktop.observe", "desktop.input", "browser.observe", "browser.actions", "shell.exec", "file.read"} {
		if contains(capabilities.Capabilities, unavailable) {
			t.Fatalf("capabilities incorrectly claim %q", unavailable)
		}
	}

	request := contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}
	response, err := manager.SetControl(request)
	if err != nil {
		t.Fatalf("blocked→AI reconciliation at same control version failed: %v", err)
	}
	if response.ControlVersion != fence.ControlVersion || response.Mode != contract.ModeAI {
		t.Fatalf("same-version reconciliation changed fence or mode: %#v", response)
	}
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeHuman}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("same-version blocked→human without receipt error = %v, want invalid request", err)
	}
	closeManager(t, manager)
}

func TestDesktopCapabilitiesAreAdvertisedFromRegisteredHandlers(t *testing.T) {
	handlers := []ActionHandler{
		capabilityHandler{capability: "desktop.observe", action: "screenshot"},
		capabilityHandler{capability: "window.list", action: "list"},
		capabilityHandler{capability: "window.activate", action: "activate"},
		capabilityHandler{capability: "desktop.input", action: "click"},
		capabilityHandler{capability: "desktop.input", action: "type"},
		capabilityHandler{capability: "desktop.input", action: "press_key"},
	}
	manager, fence := openManager(t, 1, handlers...)
	defer closeManager(t, manager)
	capabilities, err := manager.Capabilities(capabilityRequest(fence))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"desktop.observe", "desktop.input", "window.list", "window.activate"} {
		if !contains(capabilities.Capabilities, expected) {
			t.Errorf("registered desktop capability %q is absent: %v", expected, capabilities.Capabilities)
		}
	}
}

func TestRestartFencesLeasesAndRequiresReissue(t *testing.T) {
	dir := newStoreDir(t)
	first, err := Open(Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstFence := currentFence(t, first)
	if _, err := first.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: firstFence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	firstFence.ControlVersion = 0
	leaseRequest := contract.SetTaskLeaseRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: firstFence, LeaseVersion: 1, ExpiresAt: time.Now().UTC().Add(20 * time.Second),
	}
	if _, err := first.SetTaskLease(testTaskID, leaseRequest); err != nil {
		t.Fatal(err)
	}
	// Simulate process termination after durable state; the old instance cannot
	// safely continue to own the operation lock or lease fence.
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondFence := currentFence(t, second)
	if secondFence.RuntimeInstanceID == firstFence.RuntimeInstanceID {
		t.Fatal("runtime restart reused the previous instance ID")
	}
	if secondFence.ControlVersion != firstFence.ControlVersion {
		t.Fatal("runtime restart changed the Host control version")
	}
	if _, err := second.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: secondFence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	staleLease := leaseRequest
	staleLease.HostFence = firstFence
	if _, err := second.SetTaskLease(testTaskID, staleLease); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("old runtime instance lease was accepted: %v", err)
	}
	secondFence.ControlVersion = 0
	leaseRequest.HostFence = secondFence
	if _, err := second.SetTaskLease(testTaskID, leaseRequest); err != nil {
		t.Fatalf("same Morde lease version was not accepted after instance re-fencing: %v", err)
	}
	closeManager(t, second)
}

func TestTaskLeaseDurationRenewalAndFenceBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manager, err := Open(Options{
		StoreDir: newStoreDir(t), ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID,
		Generation: 1, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeManager(t, manager)
	fence := currentFence(t, manager)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	for _, expiry := range []time.Time{now.Add(-time.Nanosecond), now, now.Add(contract.MaxTaskLeaseDuration + time.Nanosecond)} {
		if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, expiry)); !errors.Is(err, ErrFenceRejected) {
			t.Fatalf("SetTaskLease(%s) error = %v, want fence rejection", expiry, err)
		}
	}
	if _, exists := manager.state.Leases[testTaskID]; exists {
		t.Fatal("rejected expiry changed the durable lease")
	}
	lease := testLease(fence, 1, now.Add(contract.MaxTaskLeaseDuration))
	if _, err := manager.SetTaskLease(testTaskID, lease); err != nil {
		t.Fatalf("exactly 30 seconds must be accepted: %v", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, lease); err != nil {
		t.Fatalf("identical live lease must be idempotent: %v", err)
	}
	now = now.Add(time.Second)
	renewed := testLease(fence, 1, now.Add(contract.MaxTaskLeaseDuration))
	if _, err := manager.SetTaskLease(testTaskID, renewed); err != nil {
		t.Fatalf("bounded renewal rejected: %v", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, now.Add(contract.MaxTaskLeaseDuration+time.Nanosecond))); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("overlong renewal error = %v, want fence rejection", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, lease); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("shortened renewal error = %v, want fence rejection", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 2, renewed.ExpiresAt)); err != nil {
		t.Fatalf("new lease version rejected: %v", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, renewed); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("stale lease version error = %v, want fence rejection", err)
	}
	for name, stale := range map[string]contract.HostFence{
		"generation": {HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration + 1, RuntimeInstanceID: fence.RuntimeInstanceID, ControlVersion: fence.ControlVersion},
		"instance":   {HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration, RuntimeInstanceID: "ri_stale", ControlVersion: fence.ControlVersion},
		"control":    {HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration, RuntimeInstanceID: fence.RuntimeInstanceID, ControlVersion: fence.ControlVersion + 1},
	} {
		if _, err := manager.SetTaskLease(testTaskID, testLease(stale, 3, now.Add(time.Second))); !errors.Is(err, ErrFenceRejected) {
			t.Fatalf("stale %s fence error = %v, want rejection", name, err)
		}
	}
	now = renewed.ExpiresAt
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 2, now.Add(time.Second))); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("expired same-version lease was renewed: %v", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 3, now.Add(time.Second))); err != nil {
		t.Fatalf("new version after expiry rejected: %v", err)
	}
}

func TestOperationIdempotencyConflictAndUnsupportedCapability(t *testing.T) {
	manager, fence := openManager(t, 1)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	lease := testLease(fence, 1, time.Now().UTC().Add(20*time.Second))
	if _, err := manager.SetTaskLease(testTaskID, lease); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, "{\"text\":\"hello\"}")
	operation, err := manager.Submit(context.Background(), request)
	if !errors.Is(err, ErrCapabilityUnavailable) || operation.Status != contract.OperationFailed || !json.Valid(operation.Result) {
		t.Fatalf("unsupported operation result=%#v err=%v", operation, err)
	}
	duplicate, err := manager.Submit(context.Background(), request)
	if err != nil || duplicate.Status != contract.OperationFailed || duplicate.OperationID != request.OperationID {
		t.Fatalf("same ID/hash retry result=%#v err=%v", duplicate, err)
	}
	conflict := request
	conflict.Arguments = json.RawMessage(`{"text":"different"}`)
	if _, err := manager.Submit(context.Background(), conflict); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different request with same operation ID error = %v", err)
	}
	conflict = request
	conflict.WorkspaceID = "wabc1234"
	conflict.BindingVersion = 1
	if _, err := manager.Submit(context.Background(), conflict); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-workspace operation with a Workspace binding error = %v", err)
	}
	closeManager(t, manager)
}

func TestOperationFailureKeepsOnlySafeDesktopCodes(t *testing.T) {
	if got := operationFailureCode(ErrDesktopFocusChanged); got != "desktop_focus_changed" {
		t.Fatalf("focus failure code = %q", got)
	}
	if got := operationFailureCode(ErrDesktopGeometryChanged); got != "desktop_geometry_changed" {
		t.Fatalf("geometry failure code = %q", got)
	}
	if got := operationFailureCode(errors.New("private action details")); got != "operation_failed" {
		t.Fatalf("ordinary failure exposed details as %q", got)
	}
}

func TestTaskWorkspaceCheckpointAdvancesOnTheSamePrivateView(t *testing.T) {
	manager, fence := openManager(t, 1)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence, contract.WorkspaceBinding{
		WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567",
	})); err != nil {
		t.Fatal(err)
	}
	request := contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 4, WorkspaceCheckpointRevision: 4, ManifestSHA256: strings.Repeat("0", 64),
	}
	initial, err := manager.SetTaskWorkspaceView(context.Background(), testTaskID, request)
	if err != nil {
		t.Fatal(err)
	}
	request.WorkspaceCheckpointRevision = 5
	advanced, err := manager.SetTaskWorkspaceView(context.Background(), testTaskID, request)
	if err != nil || advanced.ViewID != initial.ViewID || manager.taskWorkspaceViews[testTaskID].checkpointRevision != 5 {
		t.Fatalf("advanced view = %#v, map = %#v, err = %v", advanced, manager.taskWorkspaceViews[testTaskID], err)
	}
	request.WorkspaceCheckpointRevision = 4
	if _, err := manager.SetTaskWorkspaceView(context.Background(), testTaskID, request); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("stale checkpoint was accepted: %v", err)
	}
	closeManager(t, manager)
}

func TestWorkspaceTaskLeaseRejectsDesktopInputWithoutWorkspaceRequestFields(t *testing.T) {
	manager, fence := openManager(t, 1, capabilityHandler{capability: "desktop.input", action: "click"})
	defer closeManager(t, manager)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	lease := testLease(fence, 1, time.Now().UTC().Add(20*time.Second))
	lease.WorkspaceTask = true
	if _, err := manager.SetTaskLease(testTaskID, lease); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, `{}`)
	request.Capability, request.Action = "desktop.input", "click"
	if _, err := manager.Submit(context.Background(), request); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("Workspace Task desktop input error=%v, want fenced rejection", err)
	}
}

func TestTaskWorkspaceExportFreezesOperationsUntilCheckpointConfirmation(t *testing.T) {
	manager, fence := openManager(t, 1)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence, contract.WorkspaceBinding{
		WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567",
	})); err != nil {
		t.Fatal(err)
	}
	registerTestTaskWorkspaceView(t, manager, fence)
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	request := contract.TaskWorkspaceManifestRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, InputManifestSHA256: strings.Repeat("0", 64),
		ViewID: manager.taskWorkspaceViews[testTaskID].viewID, Limit: 10,
	}
	if _, err := manager.TaskWorkspaceManifestPage(context.Background(), testTaskID, request); err == nil {
		t.Fatal("test export unexpectedly succeeded")
	}
	if _, err := manager.Submit(context.Background(), workspaceReadRequest(fence, "oread006")); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("operation during Task Workspace publish error=%v, want fenced rejection", err)
	}
	if _, err := manager.SetTaskWorkspaceView(context.Background(), testTaskID, contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64),
	}); err != nil {
		t.Fatalf("clear failed publish freeze: %v", err)
	}
	if _, exists := manager.taskWorkspacePublishing[testTaskID]; exists {
		t.Fatal("Task Workspace publish remained frozen after checkpoint confirmation")
	}
	closeManager(t, manager)
}

func TestWorkspaceBindingReplacementStopsOldOperationsBeforeApplyingSnapshot(t *testing.T) {
	handler := newBindingDrainHandler(true, true)
	manager, fence := openManager(t, 1, handler)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence, contract.WorkspaceBinding{
		WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567",
	})); err != nil {
		t.Fatal(err)
	}
	registerTestTaskWorkspaceView(t, manager, fence)
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	request := workspaceReadRequest(fence, "oread001")
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	<-handler.started

	updated := make(chan error, 1)
	go func() {
		_, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence, contract.WorkspaceBinding{
			WorkspaceID: "w1234567", BindingVersion: 2, Directory: "w1234567",
		}))
		updated <- err
	}()
	<-handler.stopStarted
	if _, err := manager.Submit(context.Background(), workspaceReadRequest(fence, "oread002")); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("Submit during binding drain error=%v, want fenced rejection", err)
	}
	close(handler.allowStop)
	if err := <-updated; err != nil {
		t.Fatalf("binding update after verified stop: %v", err)
	}
	select {
	case <-handler.finished:
	default:
		t.Fatal("binding update returned before the old operation exited")
	}
	if operation := manager.ops[request.OperationID].Operation; operation.Status != contract.OperationCancelled {
		t.Fatalf("old operation status=%q, want cancelled", operation.Status)
	}
	if _, err := manager.Submit(context.Background(), workspaceReadRequest(fence, "oread003")); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("Submit after binding removal error=%v, want fenced rejection", err)
	}
	closeManager(t, manager)
}

func TestUnverifiedBindingStopQuarantinesWorkspaceUntilRetryProvesStop(t *testing.T) {
	handler := newBindingDrainHandler(false, false)
	manager, fence := openManager(t, 1, handler)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	initial := bindingSnapshot(fence, contract.WorkspaceBinding{WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567"})
	if _, err := manager.SetWorkspaceBindings(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	registerTestTaskWorkspaceView(t, manager, fence)
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	request := workspaceReadRequest(fence, "oread004")
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	<-handler.started
	if _, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence)); !errors.Is(err, ErrOperationsPending) {
		t.Fatalf("unverified binding stop error=%v, want unresolved operation", err)
	}
	if operation := manager.ops[request.OperationID].Operation; operation.Status != contract.OperationUnknown {
		t.Fatalf("unverified operation status=%q, want unknown", operation.Status)
	}
	if _, err := manager.Submit(context.Background(), workspaceReadRequest(fence, "oread005")); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("Submit to quarantined Workspace error=%v, want fenced rejection", err)
	}

	handler.verified = true
	if _, err := manager.SetWorkspaceBindings(context.Background(), bindingSnapshot(fence)); err != nil {
		t.Fatalf("retry after stop proof: %v", err)
	}
	if operation := manager.ops[request.OperationID].Operation; operation.Status != contract.OperationCancelled {
		t.Fatalf("retried operation status=%q, want cancelled", operation.Status)
	}
	closeManager(t, manager)
}

func bindingSnapshot(fence contract.HostFence, bindings ...contract.WorkspaceBinding) contract.SetWorkspaceBindingsRequest {
	if bindings == nil {
		bindings = []contract.WorkspaceBinding{}
	}
	return contract.SetWorkspaceBindingsRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Bindings: bindings}
}

func workspaceReadRequest(fence contract.HostFence, operationID string) contract.ExecuteRequest {
	request := testExecute(fence, `{}`)
	request.OperationID = operationID
	request.Capability = "file.read"
	request.Action = "read"
	request.WorkspaceID = "w1234567"
	request.BindingVersion = 1
	request.WorkspaceViewID, _ = taskWorkspaceViewID(contract.SetTaskWorkspaceViewRequest{
		HostFence: fence, TaskID: testTaskID, WorkspaceID: request.WorkspaceID, BindingVersion: 1,
		InputRevision: 0, WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64),
	})
	request.WorkspaceCheckpointRevision = 0
	return request
}

type testTaskWorkspaceViewStore struct{}

func (testTaskWorkspaceViewStore) PrepareTaskWorkspaceView(context.Context, string, string, string, string, int64, int64, int64, string) error {
	return nil
}

func (testTaskWorkspaceViewStore) DeleteTaskWorkspaceView(context.Context, string, string, string) error {
	return nil
}

func (testTaskWorkspaceViewStore) ExportTaskWorkspaceManifestPage(context.Context, contract.TaskWorkspaceManifestRequest) (contract.TaskWorkspaceManifestResponse, error) {
	return contract.TaskWorkspaceManifestResponse{}, errors.New("not implemented")
}

func (testTaskWorkspaceViewStore) ReadTaskWorkspaceBlock(context.Context, string, string, string, string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func TestDeleteTaskWorkspaceViewWaitsForLeaseAndUnknownOperations(t *testing.T) {
	now := time.Now().UTC()
	manager, err := Open(Options{
		StoreDir: newStoreDir(t), ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 1,
		ProtocolVersion: contract.ProtocolVersion, TaskWorkspaceViews: testTaskWorkspaceViewStore{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil && !errors.Is(err, ErrOperationsPending) {
			t.Errorf("close runtime manager: %v", err)
		}
	})
	fence := currentFence(t, manager)
	if _, err := manager.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, now.Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	request := contract.DeleteTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, RequestID: "z1234567",
	}
	if _, err := manager.DeleteTaskWorkspaceView(context.Background(), testTaskID, request); !errors.Is(err, ErrOperationsPending) {
		t.Fatalf("live Task lease deletion error = %v", err)
	}
	manager.mu.Lock()
	lease := manager.state.Leases[testTaskID]
	lease.ExpiresAt = now.Add(-time.Second)
	manager.state.Leases[testTaskID] = lease
	manager.ops["ounkn001"] = operationRecord{Operation: contract.Operation{TaskID: testTaskID, Status: contract.OperationUnknown}}
	manager.mu.Unlock()
	if _, err := manager.DeleteTaskWorkspaceView(context.Background(), testTaskID, request); !errors.Is(err, ErrOperationsPending) {
		t.Fatalf("unknown Task operation deletion error = %v", err)
	}
	manager.mu.Lock()
	operation := manager.ops["ounkn001"]
	operation.Operation.Status = contract.OperationSucceeded
	manager.ops["ounkn001"] = operation
	manager.mu.Unlock()
	response, err := manager.DeleteTaskWorkspaceView(context.Background(), testTaskID, request)
	if err != nil || !response.Deleted || response.RequestID != request.RequestID || response.TaskID != testTaskID || response.HostFence != fence {
		t.Fatalf("quiescent Task view deletion = %#v, %v", response, err)
	}
	replayed, err := manager.DeleteTaskWorkspaceView(context.Background(), testTaskID, request)
	if err != nil || !replayed.Deleted || replayed.RequestID != request.RequestID {
		t.Fatalf("same deletion request replay = %#v, %v", replayed, err)
	}
	differentRequest := request
	differentRequest.RequestID = "z7654321"
	if _, err := manager.DeleteTaskWorkspaceView(context.Background(), testTaskID, differentRequest); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("different deletion request replay error = %v", err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 2, now.Add(20*time.Second))); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("deleted Task accepted a new lease: %v", err)
	}
}

func registerTestTaskWorkspaceView(t *testing.T, manager *Manager, fence contract.HostFence) {
	t.Helper()
	_, err := manager.SetTaskWorkspaceView(context.Background(), testTaskID, contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, WorkspaceID: "w1234567",
		BindingVersion: 1, InputRevision: 0, WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatalf("register Task Workspace view: %v", err)
	}
}

type bindingDrainHandler struct {
	started     chan struct{}
	stopStarted chan struct{}
	allowStop   chan struct{}
	finished    chan struct{}
	verified    bool
	waitStop    bool
	startOnce   sync.Once
	stopOnce    sync.Once
	finishOnce  sync.Once
}

func newBindingDrainHandler(verified, waitStop bool) *bindingDrainHandler {
	return &bindingDrainHandler{
		started: make(chan struct{}), stopStarted: make(chan struct{}), allowStop: make(chan struct{}),
		finished: make(chan struct{}), verified: verified, waitStop: waitStop,
	}
}

func (*bindingDrainHandler) Capability() string { return "file.read" }
func (*bindingDrainHandler) Action() string     { return "read" }
func (h *bindingDrainHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	h.startOnce.Do(func() { close(h.started) })
	<-ctx.Done()
	h.finishOnce.Do(func() { close(h.finished) })
	return nil, ctx.Err()
}
func (h *bindingDrainHandler) Stop(ctx context.Context, _ contract.Operation) (StopResult, error) {
	h.stopOnce.Do(func() { close(h.stopStarted) })
	if h.waitStop {
		select {
		case <-h.allowStop:
		case <-ctx.Done():
			return StopResult{}, ctx.Err()
		}
	}
	return StopResult{VerifiedStopped: h.verified}, nil
}

func TestQuiesceReceiptIsVersionBoundAndOneTime(t *testing.T) {
	manager, fence := openManager(t, 1)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	current := currentFence(t, manager)
	blocked := current
	blocked.ControlVersion = 1
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: blocked, Mode: contract.ModeBlocked}); err != nil {
		t.Fatal(err)
	}
	quiesce, err := manager.Quiesce(context.Background(), contract.QuiesceRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: blocked, TargetControlVersion: 2, TaskIDs: []string{},
	})
	if err != nil || quiesce.Status != contract.QuiesceDrained || quiesce.DrainedReceipt == nil || len(quiesce.PendingOperationIDs) != 0 {
		t.Fatalf("quiesce response=%#v err=%v", quiesce, err)
	}
	human := blocked
	human.ControlVersion = 2
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: human, Mode: contract.ModeHuman, DrainedReceipt: quiesce.DrainedReceipt}); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	blockedAgain := human
	blockedAgain.ControlVersion = 3
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: blockedAgain, Mode: contract.ModeBlocked}); err != nil {
		t.Fatal(err)
	}
	replay := blockedAgain
	replay.ControlVersion = 4
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: replay, Mode: contract.ModeHuman, DrainedReceipt: quiesce.DrainedReceipt}); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("replayed drained receipt error = %v, want fence rejection", err)
	}
	if _, err := manager.Quiesce(context.Background(), contract.QuiesceRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: blockedAgain, TargetControlVersion: 4,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("quiesce with omitted task_ids error = %v", err)
	}
	closeManager(t, manager)
}

func TestRestartQuarantinesUnknownOperationWithoutReplay(t *testing.T) {
	dir := newStoreDir(t)
	first, err := Open(Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	fence := currentFence(t, first)
	first.mu.Lock()
	for _, id := range []string{"o1234567", "o7654321"} {
		record := operationRecord{Operation: contract.Operation{
			HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration, RuntimeInstanceID: fence.RuntimeInstanceID,
			ControlVersion: fence.ControlVersion, TaskID: testTaskID, OperationID: id, LeaseVersion: 1,
			Capability: "test.action", Action: "run", Status: contract.OperationAccepted,
		}, RequestHash: "sha256-test"}
		if err := first.appendOperationLocked(record); err != nil {
			first.mu.Unlock()
			t.Fatal(err)
		}
		if id == "o7654321" {
			record.Operation.Status = contract.OperationRunning
			if err := first.appendOperationLocked(record); err != nil {
				first.mu.Unlock()
				t.Fatal(err)
			}
		}
	}
	first.mu.Unlock()
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 2}); !errors.Is(err, ErrRuntimeStateUnavailable) {
		t.Fatalf("startup with an old unknown operation error = %v, want fail closed", err)
	}
	ledger, err := operationstore.Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	events, err := ledger.Events()
	if err != nil {
		_ = ledger.Close()
		t.Fatal(err)
	}
	statuses := map[string]contract.OperationStatus{}
	for _, event := range events {
		if event.Kind != "operation" {
			continue
		}
		var record operationRecord
		if err := json.Unmarshal(event.Data, &record); err != nil {
			t.Fatal(err)
		}
		statuses[record.Operation.OperationID] = record.Operation.Status
	}
	if statuses["o1234567"] != contract.OperationFailed || statuses["o7654321"] != contract.OperationUnknown {
		t.Fatalf("restart recovery statuses = %#v", statuses)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 3}); !errors.Is(err, ErrRuntimeStateUnavailable) {
		t.Fatalf("replay of an old unknown operation error = %v, want fail closed", err)
	}
	ledger, err = operationstore.Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	events, err = ledger.Events()
	if err != nil {
		_ = ledger.Close()
		t.Fatal(err)
	}
	statuses = map[string]contract.OperationStatus{}
	for _, event := range events {
		if event.Kind != "operation" {
			continue
		}
		var record operationRecord
		if err := json.Unmarshal(event.Data, &record); err != nil {
			_ = ledger.Close()
			t.Fatal(err)
		}
		statuses[record.Operation.OperationID] = record.Operation.Status
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if statuses["o1234567"] != contract.OperationFailed || statuses["o7654321"] != contract.OperationUnknown {
		t.Fatalf("unknown operation recovery statuses = %#v", statuses)
	}

	manager, err := Open(Options{
		StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 3,
		Handlers: []ActionHandler{capabilityHandler{capability: "test.action", action: "run"}},
	})
	if err != nil {
		t.Fatalf("startup with a Stop handler for the old unknown operation = %v", err)
	}
	fence = currentFence(t, manager)
	if got := manager.ops["o7654321"].Operation.Status; got != contract.OperationUnknown || manager.state.Mode != contract.ModeBlocked {
		t.Fatalf("startup replayed unknown operation or enabled control: status=%q mode=%q", got, manager.state.Mode)
	}
	if _, err := manager.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI,
	}); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("unknown operation allowed AI control: %v", err)
	}
	quiesce, err := manager.Quiesce(context.Background(), contract.QuiesceRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence,
		TargetControlVersion: fence.ControlVersion + 1, TaskIDs: []string{testTaskID},
	})
	if err != nil || quiesce.Status != contract.QuiesceDrained || quiesce.DrainedReceipt == nil {
		t.Fatalf("verified Stop did not drain old unknown operation: response=%#v err=%v", quiesce, err)
	}
	if got := manager.ops["o7654321"].Operation.Status; got != contract.OperationCancelled {
		t.Fatalf("verified Stop status=%q, want cancelled", got)
	}
	humanFence := fence
	humanFence.ControlVersion++
	if _, err := manager.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: humanFence, Mode: contract.ModeHuman, DrainedReceipt: quiesce.DrainedReceipt,
	}); err != nil {
		t.Fatalf("verified takeover after stopping unknown operation: %v", err)
	}
	closeManager(t, manager)
}

func TestOperationStoreUnavailableFailsClosed(t *testing.T) {
	manager, fence := openManager(t, 1)
	if err := manager.store.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.Ready() {
		t.Fatal("closed operation store reported ready")
	}
	if _, err := manager.Capabilities(capabilityRequest(fence)); !errors.Is(err, ErrRuntimeStateUnavailable) {
		t.Fatalf("capabilities with closed store error = %v", err)
	}
	if _, err := manager.Submit(context.Background(), testExecute(fence, `{"x":1}`)); !errors.Is(err, ErrRuntimeStateUnavailable) {
		t.Fatalf("submit with closed store error = %v", err)
	}
}

type failingEventStore struct {
	events      []operationstore.Event
	appendCalls int
	appendErr   error
	reconciled  bool
	closed      bool
}

func (s *failingEventStore) Reconcile(legacy operationstore.EventStore) error {
	events, err := legacy.Events()
	if err != nil {
		return err
	}
	s.events = events
	s.reconciled = true
	return nil
}

func (s *failingEventStore) Events() ([]operationstore.Event, error) {
	events := make([]operationstore.Event, len(s.events))
	copy(events, s.events)
	return events, nil
}

func (s *failingEventStore) Append(string, any) error {
	s.appendCalls++
	return s.appendErr
}

func (s *failingEventStore) Ready() bool { return !s.closed }

func (s *failingEventStore) Close() error {
	s.closed = true
	return nil
}

func TestPortalLedgerWriteFailureDoesNotFallBackToLocalJournal(t *testing.T) {
	dir := newStoreDir(t)
	remote := &failingEventStore{appendErr: errors.New("Portal unavailable")}
	if _, err := Open(Options{
		StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID, Generation: 1, Store: remote,
	}); !errors.Is(err, ErrRuntimeStateUnavailable) {
		t.Fatalf("runtime startup with Portal append failure = %v, want fail closed", err)
	}
	if !remote.reconciled || remote.appendCalls != 1 {
		t.Fatalf("remote ledger reconciliation=%t append calls=%d", remote.reconciled, remote.appendCalls)
	}
	legacy, err := operationstore.Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	events, err := legacy.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("failed Portal write fell back to local journal: %#v", events)
	}
}

func TestCancelRequiresVerifiedStop(t *testing.T) {
	t.Run("verified stop", func(t *testing.T) {
		handler := newBlockingHandler(true)
		manager, fence := openManager(t, 1, handler)
		startOperation(t, manager, fence, testOperationID)
		waitFor(t, handler.started)
		result, err := manager.CancelOperation(context.Background(), contract.ProtocolVersion, testOperationID, contract.CancelOperationRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence})
		if err != nil || result.Status != contract.OperationCancelled {
			t.Fatalf("cancel result=%#v err=%v", result, err)
		}
		closeManager(t, manager)
	})

	t.Run("unverified stop stays unknown", func(t *testing.T) {
		handler := newBlockingHandler(false)
		manager, fence := openManager(t, 1, handler)
		startOperation(t, manager, fence, testOperationID)
		waitFor(t, handler.started)
		result, err := manager.CancelOperation(context.Background(), contract.ProtocolVersion, testOperationID, contract.CancelOperationRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence})
		if err != nil || result.Status != contract.OperationUnknown {
			t.Fatalf("cancel result=%#v err=%v", result, err)
		}
		close(handler.release)
		waitFor(t, handler.finished)
		if err := manager.Close(); !errors.Is(err, ErrOperationsPending) {
			t.Fatalf("Close with unknown operation = %v, want pending", err)
		}
		_ = manager.store.Close()
	})
}

func TestCloseStopsActiveOperationBeforeClosingJournal(t *testing.T) {
	handler := newBlockingHandler(true)
	manager, fence := openManager(t, 1, handler)
	startOperation(t, manager, fence, testOperationID)
	waitFor(t, handler.started)

	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitFor(t, handler.finished)
	if got := manager.ops[testOperationID].Operation.Status; got != contract.OperationCancelled {
		t.Fatalf("operation status after Close() = %q, want cancelled", got)
	}
}

func TestExpiredLeaseCancelsAndRequiresStopProof(t *testing.T) {
	handler := newBlockingHandler(true)
	manager, fence := openManager(t, 1, handler)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(250*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, `{"value":1}`)
	request.Capability = handler.Capability()
	request.Action = handler.Action()
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitFor(t, handler.started)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := manager.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
			HostFence: fence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status == contract.OperationCancelled {
			waitFor(t, handler.finished)
			closeManager(t, manager)
			return
		}
		if operation.Status == contract.OperationUnknown {
			t.Fatalf("verified stop unexpectedly left operation unknown: %#v", operation)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lease expiry did not stop the operation")
}

func TestExpiredLeaseWithoutStopProofRemainsUnknown(t *testing.T) {
	handler := newBlockingHandler(false)
	manager, fence := openManager(t, 1, handler)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(100*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, `{"value":1}`)
	request.Capability, request.Action = handler.Capability(), handler.Action()
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitFor(t, handler.started)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := manager.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
			HostFence: fence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status == contract.OperationUnknown {
			if _, err := manager.Submit(context.Background(), request); !errors.Is(err, ErrFenceRejected) {
				t.Fatalf("expired lease allowed a new dispatch: %v", err)
			}
			if err := manager.Close(); !errors.Is(err, ErrOperationsPending) {
				t.Fatalf("unconfirmed stop closed manager: %v", err)
			}
			_ = manager.store.Close()
			return
		}
		if operation.Status == contract.OperationCancelled {
			t.Fatal("expired lease was reported cancelled without stop proof")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired lease did not become unknown")
}

func TestLeaseRenewalExtendsRunningOperation(t *testing.T) {
	handler := newBlockingHandler(true)
	manager, fence := openManager(t, 1, handler)
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	initialExpiry := time.Now().UTC().Add(250 * time.Millisecond)
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, initialExpiry)); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, `{"value":1}`)
	request.Capability = handler.Capability()
	request.Action = handler.Action()
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitFor(t, handler.started)
	time.Sleep(75 * time.Millisecond)
	renewedExpiry := time.Now().UTC().Add(500 * time.Millisecond)
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, renewedExpiry)); err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Until(initialExpiry) + 75*time.Millisecond)
	operation, err := manager.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
		HostFence: fence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != contract.OperationRunning {
		t.Fatalf("operation status after renewed original expiry = %q, want running", operation.Status)
	}
	select {
	case <-handler.finished:
		t.Fatal("handler stopped at the original lease expiry")
	default:
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err = manager.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
			HostFence: fence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status == contract.OperationCancelled {
			waitFor(t, handler.finished)
			closeManager(t, manager)
			return
		}
		if operation.Status == contract.OperationUnknown {
			t.Fatalf("verified stop unexpectedly left operation unknown: %#v", operation)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("renewed lease expiry did not stop the operation")
}

func startOperation(t *testing.T, manager *Manager, fence contract.HostFence, operationID string) {
	t.Helper()
	if _, err := manager.SetControl(contract.SetControlRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Mode: contract.ModeAI}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetTaskLease(testTaskID, testLease(fence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	request := testExecute(fence, `{"value":1}`)
	request.OperationID = operationID
	for _, handler := range manager.handlers {
		request.Capability = handler.Capability()
		request.Action = handler.Action()
		break
	}
	if _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

type blockingHandler struct {
	started    chan struct{}
	finished   chan struct{}
	release    chan struct{}
	verified   bool
	startOnce  sync.Once
	finishOnce sync.Once
}

type capabilityHandler struct{ capability, action string }

func (h capabilityHandler) Capability() string { return h.capability }
func (h capabilityHandler) Action() string     { return h.action }
func (capabilityHandler) Execute(context.Context, contract.ExecuteRequest) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}
func (capabilityHandler) Stop(context.Context, contract.Operation) (StopResult, error) {
	return StopResult{VerifiedStopped: true}, nil
}

func newBlockingHandler(verified bool) *blockingHandler {
	return &blockingHandler{started: make(chan struct{}), finished: make(chan struct{}), release: make(chan struct{}), verified: verified}
}

func (*blockingHandler) Capability() string { return "test.action" }
func (*blockingHandler) Action() string     { return "run" }
func (h *blockingHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	h.startOnce.Do(func() { close(h.started) })
	select {
	case <-ctx.Done():
	case <-h.release:
	}
	h.finishOnce.Do(func() { close(h.finished) })
	return json.RawMessage(`{"done":true}`), nil
}
func (h *blockingHandler) Stop(_ context.Context, _ contract.Operation) (StopResult, error) {
	if h.verified {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	}
	return StopResult{VerifiedStopped: h.verified}, nil
}

func openManager(t *testing.T, generation int64, handlers ...ActionHandler) (*Manager, contract.HostFence) {
	t.Helper()
	manager, err := Open(Options{
		StoreDir: newStoreDir(t), ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID,
		Generation: generation, ProtocolVersion: contract.ProtocolVersion, Handlers: handlers,
		TaskWorkspaceViews: testTaskWorkspaceViewStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := currentFence(t, manager)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil && !errors.Is(err, ErrOperationsPending) {
			t.Errorf("close runtime manager: %v", err)
		}
	})
	return manager, fence
}

func currentFence(t *testing.T, manager *Manager) contract.HostFence {
	t.Helper()
	response, err := manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: testHostID, RuntimeGeneration: manager.state.Generation, ControlVersion: manager.state.ControlVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return contract.HostFence{HostID: response.HostID, RuntimeGeneration: response.RuntimeGeneration, RuntimeInstanceID: response.RuntimeInstanceID, ControlVersion: response.ControlVersion}
}

func capabilityRequest(fence contract.HostFence) contract.CapabilitiesRequest {
	return contract.CapabilitiesRequest{ProtocolVersion: contract.ProtocolVersion, HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration, ControlVersion: fence.ControlVersion}
}

func testLease(fence contract.HostFence, version int64, expiration time.Time) contract.SetTaskLeaseRequest {
	return contract.SetTaskLeaseRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LeaseVersion: version, ExpiresAt: expiration}
}

func testExecute(fence contract.HostFence, args string) contract.ExecuteRequest {
	return contract.ExecuteRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: testTaskID, OperationID: testOperationID,
		LeaseVersion: 1, Capability: "browser.actions", Action: "navigate", Arguments: json.RawMessage(args),
	}
}

func newStoreDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for operation goroutine")
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

const (
	testHostID      = "h1234567"
	testTaskID      = "q1234567"
	testOperationID = "o1234567"
)
