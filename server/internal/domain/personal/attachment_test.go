package personal

import (
	"bytes"
	"strings"
	"testing"
)

func validAttachmentUpload() AttachmentUpload {
	return AttachmentUpload{
		UploadID: "pu_0123456789abcdef0123456789abcdef", OwnerID: "u1234567", ObjectType: ObjectTypeLibraryItem,
		ObjectID: "library-entry-1", BaseRevision: 0, SizeBytes: 5,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	}
}

func TestPersonalAttachmentIDsUseDedicatedPrefixes(t *testing.T) {
	uploadID, err := NewPersonalAttachmentUploadID()
	if err != nil || !ValidPersonalAttachmentUploadID(uploadID) || !strings.HasPrefix(uploadID, PersonalAttachmentUploadIDPrefix) {
		t.Fatalf("generated upload id=%q err=%v", uploadID, err)
	}
	attachmentID, err := NewPersonalAttachmentID()
	if err != nil || !ValidPersonalAttachmentID(attachmentID) || !strings.HasPrefix(attachmentID, PersonalAttachmentIDPrefix) {
		t.Fatalf("generated attachment id=%q err=%v", attachmentID, err)
	}
	if ValidPersonalAttachmentUploadID("r1234567") || ValidPersonalAttachmentID("a1234567") {
		t.Fatal("personal attachment IDs accepted another domain's ID format")
	}
}

func TestValidateAttachmentUpload(t *testing.T) {
	if err := ValidateAttachmentUpload(validAttachmentUpload()); err != nil {
		t.Fatalf("valid attachment upload rejected: %v", err)
	}
	max := validAttachmentUpload()
	max.SizeBytes = MaxAttachmentBytes
	if err := ValidateAttachmentUpload(max); err != nil {
		t.Fatalf("maximum attachment size rejected: %v", err)
	}
	empty := validAttachmentUpload()
	empty.SizeBytes, empty.SHA256 = 0, emptySHA256
	if err := ValidateAttachmentUpload(empty); err != nil {
		t.Fatalf("empty attachment rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*AttachmentUpload)
	}{
		{"upload id", func(value *AttachmentUpload) { value.UploadID = "bad" }},
		{"owner", func(value *AttachmentUpload) { value.OwnerID = "other" }},
		{"object type", func(value *AttachmentUpload) { value.ObjectType = ObjectTypeMemory }},
		{"object id", func(value *AttachmentUpload) { value.ObjectID = "../file" }},
		{"negative base revision", func(value *AttachmentUpload) { value.BaseRevision = -1 }},
		{"negative size", func(value *AttachmentUpload) { value.SizeBytes = -1 }},
		{"oversized", func(value *AttachmentUpload) { value.SizeBytes = MaxAttachmentBytes + 1 }},
		{"digest", func(value *AttachmentUpload) { value.SHA256 = strings.Repeat("A", 64) }},
		{"media type", func(value *AttachmentUpload) { value.MediaType = "../image" }},
		{"media type parameters", func(value *AttachmentUpload) { value.MediaType = `image/png; name="x.png"` }},
		{"invalid request hash", func(value *AttachmentUpload) { value.RequestHash = []byte("short") }},
		{"mismatched request hash", func(value *AttachmentUpload) { value.RequestHash = make([]byte, 32) }},
		{"wrong empty digest", func(value *AttachmentUpload) { value.SizeBytes = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validAttachmentUpload()
			test.change(&value)
			if err := ValidateAttachmentUpload(value); err == nil {
				t.Fatal("invalid attachment upload accepted")
			}
		})
	}
}

func TestHashAttachmentUploadContentExcludesGeneratedIdentity(t *testing.T) {
	value := validAttachmentUpload()
	first, err := HashAttachmentUploadContent(value)
	if err != nil {
		t.Fatal(err)
	}
	value.UploadID = "pu_fedcba9876543210fedcba9876543210"
	value.OwnerID = "u7654321"
	second, err := HashAttachmentUploadContent(value)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("generated identity changed upload content hash: %x, %x, %v", first, second, err)
	}
	value.BaseRevision++
	third, err := HashAttachmentUploadContent(value)
	if err != nil || bytes.Equal(first, third) {
		t.Fatalf("changed upload plan did not change content hash: %x, %x, %v", first, third, err)
	}
}

func TestAttachmentChunkBoundariesAndUploadPlan(t *testing.T) {
	if got, err := AttachmentChunkCount(0); err != nil || got != 0 {
		t.Fatalf("zero byte chunk count = %d, %v", got, err)
	}
	if got, err := AttachmentChunkCount(MaxAttachmentChunkBytes); err != nil || got != 1 {
		t.Fatalf("one chunk count = %d, %v", got, err)
	}
	if got, err := AttachmentChunkCount(MaxAttachmentChunkBytes + 1); err != nil || got != 2 {
		t.Fatalf("two chunk count = %d, %v", got, err)
	}

	upload := validAttachmentUpload()
	upload.SizeBytes = MaxAttachmentChunkBytes + 1
	first := AttachmentChunk{UploadID: upload.UploadID, Sequence: 0, SizeBytes: MaxAttachmentChunkBytes, SHA256: strings.Repeat("b", 64)}
	last := AttachmentChunk{UploadID: upload.UploadID, Sequence: 1, SizeBytes: 1, SHA256: strings.Repeat("c", 64)}
	if err := ValidateAttachmentChunkForUpload(upload, first); err != nil {
		t.Fatalf("maximum first chunk rejected: %v", err)
	}
	if err := ValidateAttachmentChunkForUpload(upload, last); err != nil {
		t.Fatalf("final short chunk rejected: %v", err)
	}
	for _, chunk := range []AttachmentChunk{
		{UploadID: upload.UploadID, Sequence: -1, SizeBytes: 1, SHA256: strings.Repeat("a", 64)},
		{UploadID: upload.UploadID, Sequence: 2, SizeBytes: 1, SHA256: strings.Repeat("a", 64)},
		{UploadID: upload.UploadID, Sequence: 0, SizeBytes: MaxAttachmentChunkBytes + 1, SHA256: strings.Repeat("a", 64)},
		{UploadID: "pu_fedcba9876543210fedcba9876543210", Sequence: 0, SizeBytes: MaxAttachmentChunkBytes, SHA256: strings.Repeat("a", 64)},
	} {
		if err := ValidateAttachmentChunkForUpload(upload, chunk); err == nil {
			t.Errorf("invalid chunk accepted: %+v", chunk)
		}
	}
}

func TestValidateAttachmentReservationAndMetadata(t *testing.T) {
	reservation := AttachmentReservation{OwnerID: "u1234567", UploadID: "pu_0123456789abcdef0123456789abcdef", ReservationID: "r7654321", SizeBytes: 20}
	if err := ValidateAttachmentReservation(reservation); err != nil {
		t.Fatalf("valid reservation rejected: %v", err)
	}
	reservation.OwnerID = "invalid"
	if err := ValidateAttachmentReservation(reservation); err == nil {
		t.Fatal("invalid reservation owner accepted")
	}

	metadata := AttachmentMetadata{
		AttachmentID: "pa_0123456789abcdef0123456789abcdef", ObjectType: ObjectTypeLibraryItem, ObjectID: "library-entry-1",
		MediaType: "application/pdf", SizeBytes: 20, SHA256: strings.Repeat("d", 64),
	}
	if err := ValidateAttachmentMetadata(metadata); err != nil {
		t.Fatalf("valid attachment metadata rejected: %v", err)
	}
	metadata.ObjectID = "../private"
	if err := ValidateAttachmentMetadata(metadata); err == nil {
		t.Fatal("unsafe attachment target accepted")
	}
	if !ValidAttachmentUploadState(AttachmentUploadPending) || !ValidAttachmentUploadState(AttachmentUploadActive) ||
		ValidAttachmentUploadState("unknown") {
		t.Fatal("attachment upload state allowlist is incorrect")
	}
}

func TestValidateAttachmentIdempotencyKey(t *testing.T) {
	for _, key := range []string{"upload-001", "A.B_c~d-9", strings.Repeat("x", 128)} {
		if err := ValidateAttachmentIdempotencyKey(key); err != nil {
			t.Errorf("valid key %q rejected: %v", key, err)
		}
	}
	for _, key := range []string{"", "short", "has space", "has/slash", strings.Repeat("x", 129)} {
		if err := ValidateAttachmentIdempotencyKey(key); err == nil {
			t.Errorf("invalid key %q accepted", key)
		}
	}
}
