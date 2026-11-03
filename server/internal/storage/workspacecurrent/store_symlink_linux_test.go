//go:build linux

package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestFileLookupsDoNotFollowTamperedExternalWorkspaceSymlink(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "current"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	target := "missing.txt"
	targetBytes := []byte(target)
	digest := sha256.Sum256(targetBytes)
	file := workspace.File{Path: "link", Kind: "symlink", SymlinkTarget: target,
		SizeBytes: int64(len(targetBytes)), SHA256: hex.EncodeToString(digest[:]), MetadataKnown: true}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, workspaceRootMetadata(), testBlocks{}); err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	const sentinelValue = "must stay untouched"
	if err := os.WriteFile(sentinel, []byte(sentinelValue), 0600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "link")
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, linkPath); err != nil {
		t.Fatal(err)
	}
	if file, _, err := store.OpenRegularFile("u1234567", "w1234567", "link"); err == nil {
		_ = file.Close()
		t.Fatal("OpenRegularFile followed a Workspace symlink")
	}
	if info, err := store.StatPath("u1234567", "w1234567", "link"); err == nil {
		t.Fatalf("StatPath followed a Workspace symlink: %#v", info)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, nil, nil, workspaceRootMetadata(), noWorkspaceBlocks{}); err != nil {
		t.Fatalf("Materialize() recovery with an external symlink = %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != sentinelValue {
		t.Fatalf("external sentinel = %q, %v; want unchanged", got, err)
	}
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("recovered current tree kept the stale link: %v", err)
	}
}
