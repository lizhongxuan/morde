package record

import (
	"strings"
	"testing"
)

func TestValidateCapture(t *testing.T) {
	valid := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("record body"), Attachments: []AttachmentInput{{
		FileName: "image.png", MediaType: "image/png", Content: []byte("image bytes"),
	}}}
	if err := ValidateCapture(valid); err != nil {
		t.Fatalf("valid capture: %v", err)
	}
	valid.Attachments[0].FileName = "../image.png"
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("path-like attachment name was accepted")
	}
	valid.Attachments[0].FileName = "image.png"
	valid.Attachments[0].MediaType = "text/plain\r\n"
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("invalid media type was accepted")
	}
	valid.Attachments = nil
	valid.Payload = []byte(strings.Repeat("x", int(MaxCapturePayloadBytes+1)))
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("oversized payload was accepted")
	}
}

func TestRecordIDs(t *testing.T) {
	if !ValidRecordID("v1234567") || ValidRecordID("j1234567") || ValidQueueItemID("v1234567") ||
		!ValidQueueItemID("j1234567") || !ValidAttachmentID("g1234567") || !ValidTombstoneID("l1234567") {
		t.Fatal("record ID prefixes were not validated")
	}
}
