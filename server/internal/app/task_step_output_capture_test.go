package app

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"unicode/utf8"
)

func testTaskStepOutputCaptureIdentity() TaskStepOutputCaptureIdentity {
	return TaskStepOutputCaptureIdentity{
		OwnerID: "u1234567", TaskID: "q1234567", AuthorizationVersion: 2, TaskRevision: 3,
		WorkspaceID: "w1234567", WorkspaceAuthorizationVersion: 4, WorkspaceBindingVersion: 5,
		WorkspaceViewID: "v" + strings.Repeat("a", 64), WorkspaceCheckpointRevision: 6,
		StepID: "s1234567", CallID: "call-12345678", OperationID: "o1234567",
	}
}

func TestSealTaskStepOutputCaptureRedactsAcrossSourceChunksBeforeEncryption(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("api_key=split-secret-value")
	first := append(bytes.Repeat([]byte("x"), TaskStepOutputCapturePageBytes-8), secret[:11]...)
	second := append(append([]byte(nil), secret[11:]...), []byte("\nfinished")...)
	joined := append(append([]byte(nil), first...), second...)
	if bytes.Contains(joined, []byte("split-secret-value")) == false {
		t.Fatal("test stream does not contain the split secret")
	}
	capture, err := SealTaskStepOutputCapture(sealer, testTaskStepOutputCaptureIdentity(), joined, []byte("stderr"))
	if err != nil {
		t.Fatalf("SealTaskStepOutputCapture() error=%v", err)
	}
	if !validTaskStepOutputCaptureRecord(capture) || capture.Stdout.SourceBytes != len(joined) ||
		capture.Stdout.TotalBytes >= capture.Stdout.SourceBytes || len(capture.Stdout.Pages) < 2 {
		t.Fatalf("capture record is invalid or did not preserve redaction accounting: %+v", capture.Stdout)
	}
	var redacted bytes.Buffer
	for _, page := range capture.Stdout.Pages {
		if bytes.Contains(page.Ciphertext, []byte("split-secret-value")) {
			t.Fatal("raw secret appears in encrypted payload bytes")
		}
		plain, err := sealer.OpenWithAAD(page.Ciphertext, taskStepOutputCaptureAAD(page))
		if err != nil {
			t.Fatalf("open page %d: %v", page.PageIndex, err)
		}
		if len(plain) != page.PlaintextBytes || !utf8.Valid(plain) || !bytes.Equal(sumSHA256(plain), page.PageSHA256) {
			t.Fatalf("page %d plaintext metadata does not match", page.PageIndex)
		}
		redacted.Write(plain)
	}
	if bytes.Contains(redacted.Bytes(), []byte("split-secret-value")) || !bytes.Contains(redacted.Bytes(), []byte("[已隐藏凭据]")) {
		t.Fatalf("joined output was not fully redacted: %q", redacted.String())
	}
	if strings.Contains(string(capture.Stdout.Pages[0].Ciphertext), "split-secret") {
		t.Fatal("ciphertext contains a raw secret fragment")
	}
}

func TestSealTaskStepOutputCapturePageBoundariesAndAAD(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("a", TaskStepOutputCapturePageBytes-2) + "猫" + "tail"
	capture, err := SealTaskStepOutputCapture(sealer, testTaskStepOutputCaptureIdentity(), []byte(value), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.Stdout.Pages) != 2 || capture.Stdout.Pages[0].PlaintextBytes != TaskStepOutputCapturePageBytes-2 ||
		capture.Stdout.Pages[1].Offset != TaskStepOutputCapturePageBytes-2 {
		t.Fatalf("UTF-8 page boundary = %+v", capture.Stdout.Pages)
	}
	var got bytes.Buffer
	for _, page := range capture.Stdout.Pages {
		plain, err := sealer.OpenWithAAD(page.Ciphertext, taskStepOutputCaptureAAD(page))
		if err != nil {
			t.Fatal(err)
		}
		got.Write(plain)
	}
	if got.String() != value {
		t.Fatal("page concatenation changed output bytes")
	}
	wrongFence := capture.Stdout.Pages[0]
	wrongFence.Identity.AuthorizationVersion++
	if _, err := sealer.OpenWithAAD(capture.Stdout.Pages[0].Ciphertext, taskStepOutputCaptureAAD(wrongFence)); err == nil {
		t.Fatal("page decrypted under a different authorization version")
	}
	wrongPage := capture.Stdout.Pages[0]
	wrongPage.Stream = "stderr"
	if _, err := sealer.OpenWithAAD(capture.Stdout.Pages[0].Ciphertext, taskStepOutputCaptureAAD(wrongPage)); err == nil {
		t.Fatal("page decrypted under a different stream")
	}
}

func TestSealTaskStepOutputCaptureRejectsInvalidAndOversizedSource(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := testTaskStepOutputCaptureIdentity()
	if _, err := SealTaskStepOutputCapture(sealer, identity, []byte{0xff}, nil); err == nil {
		t.Fatal("invalid UTF-8 source was accepted")
	}
	if _, err := SealTaskStepOutputCapture(sealer, identity, []byte("api_key=secret\x00value"), nil); err == nil {
		t.Fatal("NUL-containing source was accepted")
	}
	if _, err := SealTaskStepOutputCapture(sealer, identity,
		bytes.Repeat([]byte("x"), TaskStepOutputCaptureMaxBytes+1), nil); err == nil {
		t.Fatal("oversized source was accepted")
	}
	invalidIdentity := identity
	invalidIdentity.OperationID = ""
	if _, err := SealTaskStepOutputCapture(sealer, invalidIdentity, []byte("safe"), nil); err == nil {
		t.Fatal("capture without operation identity was accepted")
	}
}

func TestTaskStepOutputCaptureRecordRejectsPageGapsAndMetadataChanges(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{10}, 32))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := SealTaskStepOutputCapture(sealer, testTaskStepOutputCaptureIdentity(),
		[]byte(strings.Repeat("x", TaskStepOutputCapturePageBytes+1)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !validTaskStepOutputCaptureRecord(capture) {
		t.Fatal("valid capture rejected")
	}
	gap := capture
	gap.Stdout.Pages = append([]TaskStepOutputCapturePageRecord(nil), capture.Stdout.Pages...)
	gap.Stdout.Pages[1].Offset++
	if validTaskStepOutputCaptureRecord(gap) {
		t.Fatal("capture with a page gap was accepted")
	}
	metadata := capture
	metadata.Stdout = capture.Stdout
	metadata.Stdout.Pages = append([]TaskStepOutputCapturePageRecord(nil), capture.Stdout.Pages...)
	differentDigest := sha256.Sum256([]byte("different"))
	metadata.Stdout.Pages[0].StreamSHA256 = differentDigest[:]
	if validTaskStepOutputCaptureRecord(metadata) {
		t.Fatal("capture with changed page/stream metadata was accepted")
	}
}

func sumSHA256(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
