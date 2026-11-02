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
	valid.Kind = ItemSample
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("full record with training sample kind was accepted")
	}
	valid.Kind = ItemTurn
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

func TestValidateCaptureRejectsCredentialsAndVaultMarkers(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"api_key":"synthetic-value"}`),
		[]byte("please use token: abcdefghijklmnopqrstuvwxyz0123456789"),
		[]byte("{{vault:OPENAI_KEY}}"),
	} {
		value := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: payload}
		if err := ValidateCapture(value); err == nil {
			t.Errorf("credential-like payload %q was accepted", payload)
		}
	}
	textAttachment := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
		FileName: "notes.txt", MediaType: "text/plain", Content: []byte("password: synthetic-value"),
	}}}
	if err := ValidateCapture(textAttachment); err == nil {
		t.Fatal("credential-like text attachment was accepted")
	}
	binaryAttachment := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
		FileName: "image.png", MediaType: "image/png", Content: []byte("binary bytes"),
	}}}
	if err := ValidateCapture(binaryAttachment); err != nil {
		t.Fatalf("binary attachment without recognizable marker: %v", err)
	}
}

func TestRecordIDs(t *testing.T) {
	if !ValidRecordID("v1234567") || ValidRecordID("j1234567") || ValidQueueItemID("v1234567") ||
		!ValidQueueItemID("j1234567") || !ValidAttachmentID("g1234567") || !ValidTombstoneID("l1234567") {
		t.Fatal("record ID prefixes were not validated")
	}
}
