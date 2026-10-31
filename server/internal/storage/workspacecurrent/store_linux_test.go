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
	defer store.Close()

	first := []byte("first revision")
	firstDigest := sha256.Sum256(first)
	firstSHA := hex.EncodeToString(firstDigest[:])
	firstFile := workspace.File{
		Path: "docs/readme.txt", SizeBytes: int64(len(first)), SHA256: firstSHA, Mode: 0640,
		Chunks: []workspace.Chunk{{SHA256: firstSHA, SizeBytes: int64(len(first))}},
	}
	blocks := testBlocks{firstSHA: first}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{firstFile}, nil, blocks); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	target := filepath.Join(root, hex.EncodeToString(ownerDigest[:]), "w1234567")
	if got, err := os.ReadFile(filepath.Join(target, "docs", "readme.txt")); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first materialized file = %q, %v", got, err)
	}
	if revision, exists, err := store.CurrentRevision("u1234567", "w1234567"); err != nil || !exists || revision != 1 {
		t.Fatalf("first current revision = %d, %t, %v", revision, exists, err)
	}

	second := []byte("second revision")
	secondDigest := sha256.Sum256(second)
	secondSHA := hex.EncodeToString(secondDigest[:])
	secondFile := workspace.File{
		Path: "docs/readme.txt", SizeBytes: int64(len(second)), SHA256: secondSHA, Mode: 0600,
		Chunks: []workspace.Chunk{{SHA256: secondSHA, SizeBytes: int64(len(second))}},
	}
	blocks[secondSHA] = second
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{secondFile}, []string{"empty"}, blocks); err != nil {
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

func TestCurrentRevisionRequiresValidMarkerAndPublishedDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, nil, nil, testBlocks{}); err != nil {
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
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, nil, nil, testBlocks{}); !errors.Is(err, ErrUntrustedDirectory) {
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
	defer store.Close()

	content := []byte("trusted")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "docs/readme.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0640,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	directories := []string{"empty"}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, directories, testBlocks{sha: content}); err != nil {
		t.Fatal(err)
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	target := filepath.Join(root, hex.EncodeToString(ownerDigest[:]), "w1234567")
	assertMatch := func(want bool) {
		t.Helper()
		matches, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, directories)
		if err != nil || matches != want {
			t.Fatalf("MatchesManifest() = %t, %v; want %t", matches, err, want)
		}
	}
	assertMatch(true)

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

func TestMaterializeRecoversUnmarkedEmptyInitialDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "current")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerDigest := sha256.Sum256([]byte("u1234567"))
	ownerRoot := filepath.Join(root, hex.EncodeToString(ownerDigest[:]))
	if err := os.MkdirAll(filepath.Join(ownerRoot, "w1234567"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil, testBlocks{}); err != nil {
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
	defer store.Close()

	content := []byte("inside")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "safe/file.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, testBlocks{sha: content}); err != nil {
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
	defer store.Close()

	content := []byte("verified")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "stable.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, testBlocks{sha: content}); err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{file}, nil, testBlocks{}); err == nil {
		t.Fatal("Materialize() succeeded with a missing block")
	}
	ownerDigest := sha256.Sum256([]byte("u1234567"))
	target := filepath.Join(root, hex.EncodeToString(ownerDigest[:]), "w1234567", "stable.txt")
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published tree changed after failed materialization: %q, %v", got, err)
	}
}
