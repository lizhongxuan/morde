package runtimecore

import (
	"bytes"
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

func testSupplementalOutputIdentity() contract.SupplementalOutputCaptureIdentity {
	return contract.SupplementalOutputCaptureIdentity{
		OwnerID: "u1234567", TaskID: testTaskID, AuthorizationVersion: 3, TaskRevision: 4,
		WorkspaceID: "w1234567", WorkspaceAuthorizationVersion: 5, WorkspaceBindingVersion: 6,
		WorkspaceViewID: "v" + string(bytes.Repeat([]byte{'a'}, 64)), WorkspaceCheckpointRevision: 7,
		StepID: "s1234567", CallID: "call-123",
	}
}
