//go:build darwin || linux

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
	"runtime"
	"testing"

	"golang.org/x/sys/unix"

	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

type metadataTestBlocks map[string][]byte

func (blocks metadataTestBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	value, ok := blocks[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}

func TestMaterializeRejectsUnknownNativeMetadata(t *testing.T) {
	store := openSymlinkTestStore(t)
	content := []byte("unknown metadata must fail closed")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "unknown.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0o640,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil,
		workspaceRootMetadata(), metadataTestBlocks{sha: content})
	if err == nil {
		t.Fatal("Materialize() accepted a file with unknown native metadata")
	}
	current := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	if _, err := os.Lstat(current); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown metadata published a Workspace tree: %v", err)
	}
}

func TestMaterializeNativeMetadataFailureKeepsPublishedTree(t *testing.T) {
	store := openSymlinkTestStore(t)
	content := []byte("published revision")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "stable.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0o640,
		MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	blocks := metadataTestBlocks{sha: content}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil,
		workspaceRootMetadata(), blocks); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		file.ACL = &workspace.NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: []byte("incompatible")}
	} else {
		file.ACL = &workspace.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte("incompatible")}
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2, []workspace.File{file}, nil,
		workspaceRootMetadata(), blocks); err == nil {
		t.Fatal("Materialize() accepted cross-platform native ACL data")
	}
	current := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", file.Path)
	if got, err := os.ReadFile(current); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("failed metadata application changed the published tree: %q, %v", got, err)
	}
}

func TestMaterializePreservesRootExtendedAttributes(t *testing.T) {
	store := openSymlinkTestStore(t)
	attribute := "user.workspace-current-root-test"
	probe := t.TempDir()
	if err := unix.Setxattr(probe, attribute, []byte("supported"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
		t.Fatal(err)
	}
	metadata := workspaceRootMetadata()
	rootMode := uint32(0)
	metadata[0].Mode = &rootMode
	metadata[0].Xattrs = []workspace.ExtendedAttribute{{Name: attribute, Value: []byte("root-only")}}
	content := []byte("root metadata applies after child files")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{Path: "child.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0o640,
		MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}}}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1, []workspace.File{file}, nil, metadata,
		metadataTestBlocks{sha: content}); err != nil {
		t.Fatalf("Materialize() root xattr: %v", err)
	}
	current := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	defer func() { _ = os.Chmod(current, 0o700) }()
	info, err := os.Stat(current)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("materialized root mode = %v, %v; want 0", info, err)
	}
	if err := os.Chmod(current, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := os.OpenRoot(current)
	if err != nil {
		t.Fatal(err)
	}
	got, acl, err := workspacefs.ReadTreeCopyMetadata(opened, current, ".", false)
	_ = opened.Close()
	want := metadata[0].Xattrs
	if err != nil || !workspacefs.SameTreeCopyMetadata(want, nil, got, acl) {
		t.Fatalf("materialized root metadata = %#v / %#v, %v; want %#v / nil", got, acl, err, want)
	}
}
