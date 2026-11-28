package runtimecore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestSupplementalOutputCapturePreservesChunkBytesAndFencesPages(t *testing.T) {
	store := newSupplementalOutputCaptureStore(nil)
	defer store.close()
	identity := testSupplementalOutputIdentity()
	capture := store.begin("o1234567", identity)
	if capture == nil {
		t.Fatal("begin rejected a valid capture")
	}
	for _, chunk := range [][]byte{[]byte("sec"), []byte("ret"), []byte("-tail")} {
		if !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, chunk) {
			t.Fatal("capture rejected a small output chunk")
		}
	}
	capture.MarkSupplementalOutputIncomplete("test_interruption")
	store.finish("o1234567", true)
	page, ok := store.page("o1234567", identity, contract.SupplementalOutputCaptureStdout, 0)
	if !ok || page.State != contract.SupplementalOutputCaptureIncomplete || page.TotalBytes != 11 || !bytes.Equal(page.Data, []byte("secret-tail")) || !page.EOF {
		t.Fatalf("first page=%#v ok=%v", page, ok)
	}
	wrongIdentity := identity
	wrongIdentity.CallID = "another-call"
	if _, ok := store.page("o1234567", wrongIdentity, contract.SupplementalOutputCaptureStdout, 0); ok {
		t.Fatal("page accepted an output capture with a different CallID")
	}
	if _, ok := store.page("o1234567", identity, contract.SupplementalOutputCaptureStdout, page.TotalBytes+1); ok {
		t.Fatal("page accepted an offset beyond the stream")
	}
}

func TestSupplementalOutputCaptureExactCapacityAndOverrun(t *testing.T) {
	store := newSupplementalOutputCaptureStore(nil)
	defer store.close()
	identity := testSupplementalOutputIdentity()
	capture := store.begin("o1234567", identity)
	if capture == nil {
		t.Fatal("begin rejected a valid capture")
	}
	exact := bytes.Repeat([]byte{'x'}, maxSupplementalOutputCaptureBytes)
	if !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, exact) {
		t.Fatal("capture rejected exact byte limit")
	}
	if got := store.retained; got != maxSupplementalOutputCaptureBytes {
		t.Fatalf("retained=%d, want exact limit %d", got, maxSupplementalOutputCaptureBytes)
	}
	if capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStderr, []byte{'y'}) {
		t.Fatal("capture accepted a byte past its limit")
	}
	page, ok := store.page("o1234567", identity, contract.SupplementalOutputCaptureStdout, 0)
	if !ok || page.State != contract.SupplementalOutputCaptureIncomplete || page.TotalBytes != maxSupplementalOutputCaptureBytes {
		t.Fatalf("over-limit page=%#v ok=%v", page, ok)
	}
}

func TestSupplementalOutputCaptureExpiryTaskCleanupAndReleaseClearBuffers(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newSupplementalOutputCaptureStore(func() time.Time { return now })
	identity := testSupplementalOutputIdentity()
	capture := store.begin("o1234567", identity)
	if capture == nil || !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("secret")) {
		t.Fatal("could not initialize capture")
	}
	if !store.release("o1234567", identity) || store.retained != 0 || len(capture.stdout) != 0 {
		t.Fatal("release did not clear captured bytes")
	}
	capture = store.begin("o1234567", identity)
	if capture == nil || !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("secret")) {
		t.Fatal("could not initialize second capture")
	}
	now = now.Add(supplementalOutputCaptureTTL)
	if _, ok := store.page("o1234567", identity, contract.SupplementalOutputCaptureStdout, 0); ok {
		t.Fatal("expired capture remained readable")
	}
	if store.retained != 0 || len(capture.stdout) != 0 {
		t.Fatal("expiry did not clear captured bytes")
	}
	capture = store.begin("o8765432", identity)
	if capture == nil || !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("secret")) {
		t.Fatal("could not initialize task cleanup capture")
	}
	store.deleteTask(identity.TaskID)
	if store.retained != 0 || len(capture.stdout) != 0 || len(store.items) != 0 {
		t.Fatal("task cleanup did not clear captured bytes")
	}
}

func TestSupplementalOutputCaptureGlobalCapacityFailureIsIncomplete(t *testing.T) {
	store := newSupplementalOutputCaptureStore(nil)
	defer store.close()
	store.maxRetained = 3
	identity := testSupplementalOutputIdentity()
	capture := store.begin("o1234567", identity)
	if capture == nil || !capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("abc")) {
		t.Fatal("could not fill test capacity")
	}
	if capture.WriteSupplementalOutput(contract.SupplementalOutputCaptureStderr, []byte("d")) {
		t.Fatal("capture accepted bytes over global capacity")
	}
	page, ok := store.page("o1234567", identity, contract.SupplementalOutputCaptureStdout, 0)
	if !ok || page.State != contract.SupplementalOutputCaptureIncomplete {
		t.Fatalf("capacity failure page=%#v ok=%v", page, ok)
	}
}

func TestSupplementalOutputCaptureDoesNotSurviveRuntimeRestartOrReplayTerminalOperation(t *testing.T) {
	dir := newStoreDir(t)
	handler := &supplementalOutputCaptureRestartHandler{}
	options := Options{
		StoreDir: dir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: testHostID,
		Generation: 1, ProtocolVersion: contract.ProtocolVersion, Handlers: []ActionHandler{handler},
		TaskWorkspaceViews: testTaskWorkspaceViewStore{},
	}
	first, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	firstFence := currentFence(t, first)
	if _, err := first.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: firstFence, Mode: contract.ModeAI,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetWorkspaceBindings(context.Background(), bindingSnapshot(firstFence, contract.WorkspaceBinding{
		WorkspaceID: "w1234567", BindingVersion: 1, Directory: "w1234567",
	})); err != nil {
		t.Fatal(err)
	}
	view, err := first.SetTaskWorkspaceView(context.Background(), testTaskID, contract.SetTaskWorkspaceViewRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: firstFence, TaskID: testTaskID,
		WorkspaceID: "w1234567", BindingVersion: 1, InputRevision: 0,
		WorkspaceCheckpointRevision: 0, ManifestSHA256: strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetTaskLease(testTaskID, testLease(firstFence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	identity := contract.SupplementalOutputCaptureIdentity{
		OwnerID: "u1234567", TaskID: testTaskID, AuthorizationVersion: 1, TaskRevision: 1,
		WorkspaceID: "w1234567", WorkspaceAuthorizationVersion: 1, WorkspaceBindingVersion: 1,
		WorkspaceViewID: view.ViewID, WorkspaceCheckpointRevision: 0, StepID: "s1234567", CallID: "restart-capture",
	}
	request := testExecute(firstFence, `{}`)
	request.OperationID = "ocap0001"
	request.Capability, request.Action = "shell.exec", "run"
	request.WorkspaceID, request.BindingVersion = "w1234567", 1
	request.WorkspaceViewID, request.WorkspaceCheckpointRevision = view.ViewID, 0
	request.OutputCapture = &identity
	if _, err := first.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit captured Shell operation: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var terminal contract.Operation
	for time.Now().Before(deadline) {
		terminal, err = first.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
			HostFence: firstFence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
		})
		if err == nil && terminal.Status == contract.OperationSucceeded {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || terminal.Status != contract.OperationSucceeded || handler.calls != 1 {
		t.Fatalf("captured Shell operation=%#v err=%v handler calls=%d", terminal, err, handler.calls)
	}
	pageRequest := contract.SupplementalOutputCapturePageRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: firstFence, TaskID: testTaskID,
		LeaseVersion: 1, OperationID: request.OperationID, Identity: identity,
		Stream: contract.SupplementalOutputCaptureStdout, Offset: 0,
	}
	page, err := first.SupplementalOutputCapturePage(pageRequest)
	if err != nil || page.State != contract.SupplementalOutputCaptureComplete || !bytes.Equal(page.Data, []byte("spill-after-prefix")) {
		t.Fatalf("captured output page=%#v err=%v", page, err)
	}
	closeManager(t, first)

	options.Generation = 2
	second, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer closeManager(t, second)
	secondFence := currentFence(t, second)
	if secondFence.RuntimeInstanceID == firstFence.RuntimeInstanceID {
		t.Fatal("runtime restart reused the prior instance ID")
	}
	if _, err := second.SetControl(contract.SetControlRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: secondFence, Mode: contract.ModeAI,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SetTaskLease(testTaskID, testLease(secondFence, 1, time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	pageRequest.HostFence = secondFence
	if _, err := second.SupplementalOutputCapturePage(pageRequest); !errors.Is(err, ErrFenceRejected) {
		t.Fatalf("old capture page after Runtime restart error=%v, want fence rejection", err)
	}
	reconciled, err := second.QueryOperation(contract.ProtocolVersion, request.OperationID, contract.OperationQuery{
		HostFence: secondFence, TaskID: request.TaskID, LeaseVersion: request.LeaseVersion,
	})
	if err != nil || reconciled.Status != contract.OperationSucceeded || handler.calls != 1 {
		t.Fatalf("terminal operation after Runtime restart=%#v err=%v handler calls=%d", reconciled, err, handler.calls)
	}
}

type supplementalOutputCaptureRestartHandler struct{ calls int }

func (*supplementalOutputCaptureRestartHandler) Capability() string { return "shell.exec" }
func (*supplementalOutputCaptureRestartHandler) Action() string     { return "run" }
func (*supplementalOutputCaptureRestartHandler) SupportsSupplementalOutputCapture() bool {
	return true
}
func (h *supplementalOutputCaptureRestartHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	h.calls++
	sink := contract.SupplementalOutputSinkFromContext(ctx)
	if sink == nil || !sink.WriteSupplementalOutput(contract.SupplementalOutputCaptureStdout, []byte("spill-after-prefix")) {
		return nil, errors.New("supplemental output sink unavailable")
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (*supplementalOutputCaptureRestartHandler) Stop(context.Context, contract.Operation) (StopResult, error) {
	return StopResult{VerifiedStopped: true}, nil
}

func testSupplementalOutputIdentity() contract.SupplementalOutputCaptureIdentity {
	return contract.SupplementalOutputCaptureIdentity{
		OwnerID: "u1234567", TaskID: testTaskID, AuthorizationVersion: 3, TaskRevision: 4,
		WorkspaceID: "w1234567", WorkspaceAuthorizationVersion: 5, WorkspaceBindingVersion: 6,
		WorkspaceViewID: "v" + string(bytes.Repeat([]byte{'a'}, 64)), WorkspaceCheckpointRevision: 7,
		StepID: "s1234567", CallID: "call-123",
	}
}
