package postgres

import (
	"strings"
	"testing"

	"morde/server/internal/app"
)

func TestValidCloudDSHSkillReadCommitUsesEmbeddedAllowlist(t *testing.T) {
	for _, id := range []string{"compare-options", "amap", "train-tickets"} {
		metadata, ok := app.CloudDSHSkillMetadataForID(id)
		if !ok {
			t.Fatalf("expected built-in skill %q", id)
		}
		commit := app.CloudDSHSkillReadCommit{
			TaskID: "q1234567", OwnerID: "u1234567", AgentSessionID: "session_1",
			AuthorizationVersion: 1, CallID: "call_" + id, SkillID: metadata.ID, SkillVersion: metadata.Version,
			BundleVersion: "nanomuse-builtin-v1", SourceRef: metadata.SourceRef,
			ContentSHA256: strings.Repeat("a", 64), OutputSHA256: strings.Repeat("b", 64),
		}
		commit.FingerprintSHA256 = cloudDSHSkillReadFingerprint(commit)
		if !validCloudDSHSkillReadCommit(commit) {
			t.Fatalf("embedded skill commit %q was rejected", id)
		}
	}
	if _, ok := app.CloudDSHSkillMetadataForID("../compare-options"); ok {
		t.Fatal("unlisted skill path was accepted")
	}
}
