package record

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateCapture(t *testing.T) {
	valid := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("record body"), Attachments: []AttachmentInput{{
		FileName: "notes.txt", MediaType: "text/plain", Content: []byte("ordinary notes"),
	}}}
	if err := ValidateCapture(valid); err != nil {
		t.Fatalf("valid capture: %v", err)
	}
	valid.Kind = ItemSample
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("full record with training sample kind was accepted")
	}
	valid.Kind = ItemTurn
	valid.Attachments[0].FileName = "../notes.txt"
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("path-like attachment name was accepted")
	}
	valid.Attachments[0].FileName = "notes.txt"
	valid.Attachments[0].MediaType = "text/plain\r\n"
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("invalid media type was accepted")
	}
	valid.Attachments[0].MediaType = "text/plain"
	valid.TaskID = "bad-task"
	if err := ValidateCapture(valid); err == nil {
		t.Fatal("invalid source task id was accepted")
	}
	valid.Attachments = nil
	valid.TaskID = ""
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
		[]byte(base64.StdEncoding.EncodeToString([]byte(`{"api_key":"synthetic-value"}`))),
	} {
		value := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: payload}
		if err := ValidateCapture(value); err == nil {
			t.Errorf("credential-like payload %q was accepted", payload)
		}
	}
	textAttachment := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
		FileName: "notes.txt", MediaType: "text/plain", Content: []byte(base64.StdEncoding.EncodeToString([]byte("password: synthetic-value"))),
	}}}
	if err := ValidateCapture(textAttachment); err == nil {
		t.Fatal("credential-like text attachment was accepted")
	}
}

func TestContainsSensitiveContentDetectsBase64Secrets(t *testing.T) {
	for _, value := range []string{
		"token: synthetic-token-value",
		`{"api_key":"synthetic-value"}`,
		"{{vault:OPENAI_KEY}}",
		"sk-proj-1234567890abcdefghijklmnop",
	} {
		encoded := base64.StdEncoding.EncodeToString([]byte(value))
		if !ContainsSensitiveContent([]byte(encoded)) {
			t.Errorf("base64-encoded sensitive content %q was not detected", value)
		}
	}
	urlSafe := base64.RawURLEncoding.EncodeToString([]byte("token: synthetic-token-value"))
	if !ContainsSensitiveContent([]byte(urlSafe)) {
		t.Fatal("URL-safe base64-encoded token was not detected")
	}
	ordinary := base64.StdEncoding.EncodeToString([]byte("ordinary meeting notes for tomorrow"))
	if ContainsSensitiveContent([]byte(ordinary)) {
		t.Fatal("base64-encoded ordinary text was rejected")
	}
}

func TestValidateCaptureRejectsUnscannableContent(t *testing.T) {
	for _, mediaType := range []string{"image/png", "application/octet-stream", "application/zip", "application/xml"} {
		value := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
			FileName: "capture.bin", MediaType: mediaType, Content: []byte("ordinary binary bytes"),
		}}}
		if err := ValidateCapture(value); err == nil {
			t.Errorf("unscannable %s attachment was accepted", mediaType)
		}
	}

	for _, payload := range [][]byte{{0xff, 0xfe}, {'s', 'a', 'f', 'e', 0}} {
		value := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: payload}
		if err := ValidateCapture(value); err == nil {
			t.Errorf("unscannable payload %v was accepted", payload)
		}
	}
}

func TestValidateCaptureAcceptsScannableTextAttachments(t *testing.T) {
	for _, test := range []struct {
		mediaType string
		content   string
	}{
		{mediaType: "text/plain; charset=utf-8", content: "ordinary notes"},
		{mediaType: "application/json", content: `{"message":"ordinary notes"}`},
		{mediaType: "application/vnd.example+json", content: `{"message":"ordinary notes"}`},
	} {
		value := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
			FileName: "notes.txt", MediaType: test.mediaType, Content: []byte(test.content),
		}}}
		if err := ValidateCapture(value); err != nil {
			t.Errorf("scannable %s attachment was rejected: %v", test.mediaType, err)
		}
	}
	invalidJSON := CaptureInput{Purpose: PurposeFullRecord, Kind: ItemTurn, Payload: []byte("safe"), Attachments: []AttachmentInput{{
		FileName: "notes.json", MediaType: "application/json", Content: []byte(`{"message" "ordinary notes"}`),
	}}}
	if err := ValidateCapture(invalidJSON); err == nil {
		t.Fatal("invalid JSON attachment was accepted")
	}
}

func TestRecordIDs(t *testing.T) {
	if !ValidRecordID("v1234567") || ValidRecordID("j1234567") || ValidQueueItemID("v1234567") ||
		!ValidQueueItemID("j1234567") || !ValidAttachmentID("g1234567") || !ValidTombstoneID("l1234567") {
		t.Fatal("record ID prefixes were not validated")
	}
}
