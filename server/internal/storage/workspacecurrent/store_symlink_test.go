//go:build darwin || linux

package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde/server/internal/domain/workspace"
)

func workspaceRootMetadata() []workspace.Directory {
	return []workspace.Directory{{Path: ".", MetadataKnown: true}}
}

type noWorkspaceBlocks struct{}

func (noWorkspaceBlocks) Open(string, string) (io.ReadCloser, error) { return nil, os.ErrNotExist }

func openSymlinkTestStore(t *testing.T) *Store {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tempRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMaterializeAndVerifyDanglingWorkspaceSymlink(t *testing.T) {
	store := openSymlinkTestStore(t)

	target := "missing.txt"
	targetBytes := []byte(target)
	digest := sha256.Sum256(targetBytes)
	modified := time.Date(2025, time.July, 8, 9, 10, 11, 123456789, time.UTC).UnixNano()
	file := workspace.File{Path: "links/dangling", Kind: "symlink", SymlinkTarget: target,
		SizeBytes: int64(len(targetBytes)), SHA256: hex.EncodeToString(digest[:]), ModTimeNS: &modified, MetadataKnown: true}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, workspaceRootMetadata(), noWorkspaceBlocks{}); err != nil {
		t.Fatalf("Materialize() = %v", err)
	}
	currentPath := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "links", "dangling")
	info, err := os.Lstat(currentPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("materialized path = %v, %v; want symlink", info, err)
	}
	gotTarget, err := os.Readlink(currentPath)
	if err != nil || gotTarget != target {
		t.Fatalf("materialized link target = %q, %v; want %q", gotTarget, err, target)
	}
	if info.ModTime().UnixNano() != modified {
		t.Fatalf("symlink modification time = %d, want %d", info.ModTime().UnixNano(), modified)
	}
	assertMatches := func(want bool) {
		t.Helper()
		matches, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, nil, workspaceRootMetadata())
		if err != nil || matches != want {
			t.Fatalf("MatchesManifest() = %t, %v; want %t", matches, err, want)
		}
	}
	assertMatches(true)

	if err := os.Remove(currentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other-missing.txt", currentPath); err != nil {
		t.Fatal(err)
	}
	assertMatches(false)
}

func TestMatchesManifestRejectsTamperedExternalSymlinkWithoutChangingSentinel(t *testing.T) {
	store := openSymlinkTestStore(t)

	target := "missing.txt"
	targetBytes := []byte(target)
	digest := sha256.Sum256(targetBytes)
	file := workspace.File{Path: "link", Kind: "symlink", SymlinkTarget: target,
		SizeBytes: int64(len(targetBytes)), SHA256: hex.EncodeToString(digest[:]), MetadataKnown: true}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, workspaceRootMetadata(), noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(filepath.Dir(store.rootPath), "sentinel.txt")
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
	matches, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, nil, workspaceRootMetadata())
	if err != nil || matches {
		t.Fatalf("MatchesManifest() with an external symlink = %t, %v; want false", matches, err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != sentinelValue {
		t.Fatalf("external sentinel = %q, %v; want unchanged", got, err)
	}
}

func ownerHashForTest(ownerID string) string {
	digest := sha256.Sum256([]byte(ownerID))
	return hex.EncodeToString(digest[:])
}
