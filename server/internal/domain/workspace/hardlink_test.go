package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestWorkspaceHardlinkManifestPreservesTopologyAndRequiresCanonicalAnchor(t *testing.T) {
	content := []byte("shared")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	anchor := File{Path: "a.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0644,
		Chunks: []Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	alias := anchor
	alias.Path = "b.txt"
	alias.HardlinkTo = anchor.Path
	independent := alias
	independent.HardlinkTo = ""

	hardlinkDigest, err := ManifestSHA256([]File{alias, anchor})
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := ManifestSHA256Stream(func(yield func(File) error) error {
		if err := yield(anchor); err != nil {
			return err
		}
		return yield(alias)
	})
	if err != nil || streamed != hardlinkDigest {
		t.Fatalf("streamed hardlink digest = %q, %v; want %q", streamed, err, hardlinkDigest)
	}
	independentDigest, err := ManifestSHA256([]File{anchor, independent})
	if err != nil || independentDigest == hardlinkDigest {
		t.Fatalf("manifest digest did not distinguish hardlink topology: linked=%q independent=%q err=%v", hardlinkDigest, independentDigest, err)
	}
	if _, err := ManifestSHA256([]File{alias}); err == nil {
		t.Fatal("accepted a hardlink with a missing canonical anchor")
	}
	changed := alias
	changed.Mode = 0600
	if _, err := ManifestSHA256([]File{anchor, changed}); err == nil {
		t.Fatal("accepted a hardlink whose metadata differs from its anchor")
	}
	chain := alias
	chain.Path = "c.txt"
	chain.HardlinkTo = alias.Path
	if _, err := ManifestSHA256([]File{anchor, alias, chain}); err == nil {
		t.Fatal("accepted a chained hardlink anchor")
	}
}
