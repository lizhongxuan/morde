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

func TestValidCloudDSHSkillReadCommitAcceptsOnlyVersionedPersonalSkillMetadata(t *testing.T) {
	commit := app.CloudDSHSkillReadCommit{
		TaskID: "q1234567", OwnerID: "u1234567", AgentSessionID: "session_1",
		AuthorizationVersion: 1, CallID: "call_personal_1", SkillID: "sk_0123abcd",
		SkillVersion: "4", PersonalSkillRevision: 4, BundleVersion: "morde-personal-skill-v1",
		SourceRef: "personal://sk_0123abcd/revision/4", ContentSHA256: strings.Repeat("a", 64), OutputSHA256: strings.Repeat("b", 64),
	}
	commit.FingerprintSHA256 = cloudDSHSkillReadFingerprint(commit)
	if !validCloudDSHSkillReadCommit(commit) {
		t.Fatal("well-formed personal Skill commit was rejected")
	}
	for name, change := range map[string]func(*app.CloudDSHSkillReadCommit){
		"invalid id":        func(value *app.CloudDSHSkillReadCommit) { value.SkillID = "sk_bad" },
		"revision mismatch": func(value *app.CloudDSHSkillReadCommit) { value.SkillVersion = "3" },
		"bundle mismatch":   func(value *app.CloudDSHSkillReadCommit) { value.BundleVersion = "nanomuse-builtin-v1" },
		"source mismatch":   func(value *app.CloudDSHSkillReadCommit) { value.SourceRef = "personal://sk_0123abcd/revision/3" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := commit
			change(&invalid)
			invalid.FingerprintSHA256 = cloudDSHSkillReadFingerprint(invalid)
			if validCloudDSHSkillReadCommit(invalid) {
				t.Fatal("invalid personal Skill metadata was accepted")
			}
		})
	}
}
