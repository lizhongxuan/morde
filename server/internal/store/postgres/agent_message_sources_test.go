package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
)

func TestBuildAgentMessageReferenceSupportsAttachmentOnlySources(t *testing.T) {
	fence := app.AgentCheckpointFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		AgentInstanceID: "i1234567", AgentSessionID: "session_1", AgentRootSessionID: "session_1",
		AgentGeneration: 1, AuthorizationVersion: 1}
	if _, _, err := buildAgentMessageReference(fence, "c1234567", "m1234567", 1, 0,
		app.AgentMessageDeliveryFollowup, nil, ""); err == nil {
		t.Fatal("empty text-only message was accepted")
	}

	attachmentID := "pa_0123456789abcdef0123456789abcdef"
	envelope, attachmentDigest, err := buildAgentMessageReference(fence, "c1234567", "m1234567", 1, 0,
		app.AgentMessageDeliveryFollowup, []string{attachmentID}, "")
	if err != nil {
		t.Fatalf("build attachment-only source: %v", err)
	}
	emptyDigest := sha256.Sum256(nil)
	if envelope.ContentBytes != 0 || envelope.TotalRunes != 0 || envelope.Pages == nil || len(envelope.Pages) != 0 ||
		envelope.SourceSHA256 != hex.EncodeToString(emptyDigest[:]) || len(attachmentDigest) != sha256.Size {
		t.Fatalf("attachment-only source is not canonical: envelope=%+v attachment digest length=%d", envelope, len(attachmentDigest))
	}
	if _, err := app.ComputeAgentMessageInlineRepresentationSHA256(envelope); err != nil {
		t.Fatalf("attachment-only inline representation: %v", err)
	}
}
