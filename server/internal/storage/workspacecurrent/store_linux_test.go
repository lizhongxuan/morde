//go:build linux

package workspacecurrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"morde/server/internal/domain/workspace"
)

type testBlocks map[string][]byte

func (b testBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	value, ok := b[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}

func TestMaterializeAtomicallyPublishesVerifiedWorkspaceTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	first := []byte("first revision")
	firstDigest := sha256.Sum256(first)
	firstSHA := hex.EncodeToString(firstDigest[:])
	firstModified := time.Date(2024, time.June, 7, 8, 9, 10, 123456789, time.UTC).UnixNano()
	firstFile := workspace.File{
		Path: "docs/readme.txt", SizeBytes: int64(len(first)), SHA256: firstSHA, Mode: 0640,
		ModTimeNS: &firstModified, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: firstSHA, SizeBytes: int64(len(first))}},
	}
	blocks := testBlocks{firstSHA: first}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{firstFile}, nil, workspaceRootMetadata(), blocks); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	target := filepath.Join(root, hex.EncodeToString(ownerDigest[:]), "w1234567")
	if got, err := os.ReadFile(filepath.Join(target, "docs", "readme.txt")); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first materialized file = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(target, "docs", "readme.txt")); err != nil || info.ModTime().UnixNano() != firstModified {
		t.Fatalf("first materialized file modification time = %v, %v", info, err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || !exists || revision != 1 {
		t.Fatalf("first current revision = %d, %t, %v", revision, exists, err)
	}

	second := []byte("second revision")
	secondDigest := sha256.Sum256(second)
	secondSHA := hex.EncodeToString(secondDigest[:])
	secondFile := workspace.File{
		Path: "docs/readme.txt", SizeBytes: int64(len(second)), SHA256: secondSHA, Mode: 0600,
		Chunks: []workspace.Chunk{{SHA256: secondSHA, SizeBytes: int64(len(second))}}, MetadataKnown: true,
	}
	blocks[secondSHA] = second
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{secondFile}, []string{"empty"}, workspaceRootMetadata(), blocks); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "docs", "readme.txt")); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("second materialized file = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(target, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory stat = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(target, "missing")); !os.IsNotExist(err) {
		t.Fatalf("old file survived publish: %v", err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || !exists || revision != 2 {
		t.Fatalf("second current revision = %d, %t, %v", revision, exists, err)
	}
}

func TestMaterializeAndVerifyWorkspaceHardlinkIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	content := []byte("shared inode")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	anchor := workspace.File{Path: "a.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0640,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}, MetadataKnown: true}
	alias := anchor
	alias.Path, alias.HardlinkTo = "nested/b.txt", anchor.Path
	blocks := testBlocks{sha: content}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{alias, anchor}, nil, workspaceRootMetadata(), blocks); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(root, ownerHashForTest("u1234567"), "w1234567")
	anchorInfo, err := os.Stat(filepath.Join(workspaceRoot, anchor.Path))
	if err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(workspaceRoot, filepath.FromSlash(alias.Path))
	aliasInfo, err := os.Stat(aliasPath)
	if err != nil || !os.SameFile(anchorInfo, aliasInfo) {
		t.Fatalf("materialized hardlink identity = %v, %v; want same inode as anchor", aliasInfo, err)
	}
	assertMatches := func(want bool) {
		t.Helper()
		matched, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{anchor, alias}, nil, workspaceRootMetadata())
		if err != nil || matched != want {
			t.Fatalf("MatchesManifest() = %t, %v; want %t", matched, err, want)
		}
	}
	assertMatches(true)

	if err := os.Remove(aliasPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aliasPath, content, 0640); err != nil {
		t.Fatal(err)
	}
	assertMatches(false)
}

func TestCurrentRevisionRequiresValidMarkerAndPublishedDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, nil, nil, workspaceRootMetadata(), testBlocks{}); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	marker := filepath.Join(root, revisionMarkerDirectory, ownerHash+"-w1234567.revision")
	if err := os.WriteFile(marker, []byte("bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || exists {
		t.Fatalf("revision lookup for invalid marker = %d, %t, %v", revision, exists, err)
	}
	target := filepath.Join(root, ownerHash, "w1234567")
	unknownFile := filepath.Join(target, "untracked.txt")
	if err := os.WriteFile(unknownFile, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, nil, nil, workspaceRootMetadata(), testBlocks{}); !errors.Is(err, ErrUntrustedDirectory) {
		t.Fatalf("Materialize() with an invalid marker error = %v", err)
	}
	if got, err := os.ReadFile(unknownFile); err != nil || string(got) != "preserve me" {
		t.Fatalf("untracked current file after refused materialization = %q, %v", got, err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || exists {
		t.Fatalf("revision lookup for missing directory = %d, %t, %v", revision, exists, err)
	}
}

func TestMatchesManifestDetectsCurrentProjectionDrift(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	content := []byte("trusted")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "docs/readme.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0640,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}, MetadataKnown: true}
	directories := []string{"empty"}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, directories, workspaceRootMetadata(), testBlocks{sha: content}); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	target := filepath.Join(root, ownerHash, "w1234567")
	assertMatch := func(want bool) {
		t.Helper()
		matches, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, directories, workspaceRootMetadata())
		if err != nil || matches != want {
			t.Fatalf("MatchesManifest() = %t, %v; want %t", matches, err, want)
		}
	}
	staleStage := filepath.Join(root, ".morde-staging", ownerHash+"-w1234567-stale")
	if err := os.Mkdir(staleStage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleStage, "old-revision.txt"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	assertMatch(true)
	if _, err := os.Lstat(staleStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified manifest left stale staging tree behind: %v", err)
	}

	filePath := filepath.Join(target, filepath.FromSlash(file.Path))
	if err := os.Rename(filePath, filepath.Join(filepath.Dir(filePath), "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	assertMatch(false)
	if err := os.Rename(filepath.Join(filepath.Dir(filePath), "renamed.txt"), filePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("altered"), 0640); err != nil {
		t.Fatal(err)
	}
	assertMatch(false)
	if err := os.WriteFile(filePath, content, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filePath, 0600); err != nil {
		t.Fatal(err)
	}
	assertMatch(false)
	if err := os.Chmod(filePath, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, "empty")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "other-empty"), 0750); err != nil {
		t.Fatal(err)
	}
	assertMatch(false)
	if err := os.Remove(filepath.Join(target, "other-empty")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	assertMatch(true)
}

func TestMaterializeRestoresAndVerifiesDirectoryMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	content := []byte("directory metadata")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "src/readme.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0o640,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}, MetadataKnown: true}
	mode, modified := uint32(0o750), int64(1_700_000_000_123_456_789)
	directories := []workspace.Directory{
		{Path: ".", MetadataKnown: true},
		{Path: "src", Mode: &mode, ModTimeNS: &modified, MetadataKnown: true},
	}
	emptyDirectories := []string{"legacy-empty"}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, emptyDirectories, directories,
		testBlocks{sha: content}); err != nil {
		t.Fatalf("materialize directory metadata: %v", err)
	}
	ownerRoot := filepath.Join(root, ownerHash("u1234567"), "w1234567")
	info, err := os.Stat(filepath.Join(ownerRoot, "src"))
	if err != nil {
		t.Fatalf("stat materialized directory: %v", err)
	}
	if info.Mode().Perm() != os.FileMode(mode) || info.ModTime().UnixNano() != modified {
		t.Fatalf("materialized directory attributes = mode %v mtime %v, err=%v", info.Mode().Perm(), info.ModTime().UnixNano(), err)
	}
	assertMatches := func(want bool) {
		t.Helper()
		matched, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, emptyDirectories, directories)
		if err != nil || matched != want {
			t.Fatalf("MatchesManifest() = %t, %v; want %t", matched, err, want)
		}
	}
	assertMatches(true)
	if err := os.Chmod(filepath.Join(ownerRoot, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	assertMatches(false)
	if err := os.Chmod(filepath.Join(ownerRoot, "src"), os.FileMode(mode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(ownerRoot, "src"), time.Unix(0, modified+1), time.Unix(0, modified+1)); err != nil {
		t.Fatal(err)
	}
	assertMatches(false)
	if err := os.Chtimes(filepath.Join(ownerRoot, "src"), time.Unix(0, modified), time.Unix(0, modified)); err != nil {
		t.Fatal(err)
	}
	assertMatches(true)

	legacyPath := filepath.Join(ownerRoot, "legacy-empty")
	if err := os.Chmod(legacyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	unknownTime := time.Unix(0, modified+2)
	if err := os.Chtimes(legacyPath, unknownTime, unknownTime); err != nil {
		t.Fatal(err)
	}
	assertMatches(true)

	if err := os.Chmod(filepath.Join(ownerRoot, "src"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{file}, emptyDirectories, directories,
		testBlocks{sha: content}); err != nil {
		t.Fatalf("replace revision with restrictive prior directory mode: %v", err)
	}
	if _, err := os.Stat(ownerRoot); err != nil {
		t.Fatalf("current Workspace was not readable after replacing prior revision: %v", err)
	}
}

func TestMaterializeRecoversUnmarkedEmptyInitialDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	ownerDigest := sha256.Sum256([]byte("u1234567"))
	ownerRoot := filepath.Join(root, hex.EncodeToString(ownerDigest[:]))
	if err := os.MkdirAll(filepath.Join(ownerRoot, "w1234567"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil, workspaceRootMetadata(), testBlocks{}); err != nil {
		t.Fatal(err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || !exists || revision != 0 {
		t.Fatalf("recovered initial revision = %d, %t, %v", revision, exists, err)
	}
}

func TestOpenRejectsSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	if err := os.Mkdir(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(root, "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(linkedRoot, "current")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open() through symlink ancestor error = %v", err)
	}
}

func TestOpenRegularFileRejectsSymlinkComponents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	content := []byte("inside")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "safe/file.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}, MetadataKnown: true}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, workspaceRootMetadata(), testBlocks{sha: content}); err != nil {
		t.Fatal(err)
	}
	opened, info, err := store.OpenRegularFile("u1234567", "w1234567", "safe/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("opened size = %d, want %d", info.Size(), len(content))
	}
	directoryInfo, err := store.StatPath("u1234567", "w1234567", "safe")
	if err != nil || !directoryInfo.IsDir() {
		t.Fatalf("directory StatPath() = %v, %v", directoryInfo, err)
	}
	got, err := io.ReadAll(opened)
	closeErr := opened.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, content) {
		t.Fatalf("opened content = %q, read error %v, close error %v", got, err, closeErr)
	}

	ownerDigest := sha256.Sum256([]byte("u1234567"))
	owner := hex.EncodeToString(ownerDigest[:])
	target := filepath.Join(root, owner, "w1234567", "safe")
	if err := syscall.Mkfifo(filepath.Join(target, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := store.StatPath("u1234567", "w1234567", "safe/pipe"); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO StatPath() = %v, %v", info, err)
	}
	if opened, _, err := store.OpenRegularFile("u1234567", "w1234567", "safe/pipe"); !errors.Is(err, ErrUnsafePath) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("OpenRegularFile() on FIFO error = %v, want ErrUnsafePath", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if opened, _, err := store.OpenRegularFile("u1234567", "w1234567", "safe/file.txt"); err == nil {
		_ = opened.Close()
		t.Fatal("OpenRegularFile followed a symlink component")
	}
}

func TestMaterializeFailureKeepsPublishedTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	content := []byte("verified")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "stable.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}, MetadataKnown: true}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, workspaceRootMetadata(), testBlocks{sha: content}); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{file}, nil, workspaceRootMetadata(), testBlocks{}); err == nil {
		t.Fatal("Materialize() succeeded with a missing block")
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	target := filepath.Join(root, hex.EncodeToString(ownerDigest[:]), "w1234567", "stable.txt")
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published tree changed after failed materialization: %q, %v", got, err)
	}
}
