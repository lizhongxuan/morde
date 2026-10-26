package workspacefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestPutAndOpenBlock(t *testing.T) {
	store := newTestStore(t)
	data := []byte("workspace block")
	digest := digestOf(data)

	n, err := store.PutBlock("w1234567", digest, bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("PutBlock() = (%d, %v)", n, err)
	}
	reader, err := store.Open("w1234567", digest)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Open() bytes = %q, error = %v", got, err)
	}
}

func TestListBlockDigestsReturnsOnlyVerifiedSortedBlocks(t *testing.T) {
	store := newTestStore(t)
	data := [][]byte{[]byte("second"), []byte("first")}
	want := []string{digestOf(data[0]), digestOf(data[1])}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	for _, block := range data {
		if _, err := store.PutBlock("w1234567", digestOf(block), bytes.NewReader(block)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListBlockDigests("w1234567")
	if err != nil || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListBlockDigests() = %v, %v; want %v", got, err, want)
	}
	if got, err := store.ListBlockDigests("w7654321"); err != nil || len(got) != 0 {
		t.Fatalf("ListBlockDigests(missing) = %v, %v; want empty", got, err)
	}

	root, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(want[0], []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if _, err := store.ListBlockDigests("w1234567"); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("ListBlockDigests(corrupt) error = %v, want corrupt-block", err)
	}
}

func TestListBlockDigestsIgnoresButKeepsUploadTempFiles(t *testing.T) {
	store := newTestStore(t)
	data := []byte("committed block")
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile(tempName, []byte("upload in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListBlockDigests("w1234567")
	if err != nil || len(got) != 1 || got[0] != digest {
		t.Fatalf("ListBlockDigests() = %v, %v; want only %s", got, err, digest)
	}
	temp, err := store.root.Open("w1234567/" + tempName)
	if err != nil {
		t.Fatalf("ListBlockDigests() removed active temp file: %v", err)
	}
	contents, err := io.ReadAll(temp)
	_ = temp.Close()
	if err != nil || string(contents) != "upload in progress" {
		t.Fatalf("active temp file = %q, error = %v", contents, err)
	}
}

func TestListBlockDigestsFailsClosedOnUnknownAndUnsafeTempEntries(t *testing.T) {
	t.Run("unknown residue", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		workspaceRoot, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := workspaceRoot.WriteFile("unknown", []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := workspaceRoot.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListBlockDigests("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockDigests(unknown) error = %v, want unsafe path", err)
		}
	})

	t.Run("symlink with temp name", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		const tempName = ".tmp-0123456789abcdef0123456789abcdef"
		if err := os.Symlink(filepath.Join(store.root.Name(), "w1234567"), filepath.Join(store.root.Name(), "w1234567", tempName)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.ListBlockDigests("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockDigests(temp symlink) error = %v, want unsafe path", err)
		}
	})
}

func TestListBlockNamesSkipsPayloadVerificationAndReturnsSortedNames(t *testing.T) {
	store := newTestStore(t)
	if err := store.root.Mkdir("w1234567", 0700); err != nil {
		t.Fatal(err)
	}
	want := []string{digestOf([]byte("first")), digestOf([]byte("second"))}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	root, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range want {
		if err := root.WriteFile(digest, []byte("damaged payload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	if err := root.WriteFile(tempName, []byte("upload in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListBlockNames("w1234567")
	if err != nil || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListBlockNames() = %v, %v; want %v", got, err, want)
	}
	if _, err := store.ListBlockDigests("w1234567"); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("ListBlockDigests(corrupt payload) error = %v, want corrupt-block", err)
	}
	if got, err := store.ListBlockNames("w7654321"); err != nil || len(got) != 0 {
		t.Fatalf("ListBlockNames(missing workspace) = %v, %v; want empty", got, err)
	}
	root, err = store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	temp, err := root.Open(tempName)
	if err != nil {
		t.Fatalf("ListBlockNames() removed active temp file: %v", err)
	}
	_ = temp.Close()
	_ = root.Close()
}

func TestListBlockNamesRejectsUnknownUnsafeAndOversizedEntries(t *testing.T) {
	t.Run("unknown residue", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		root, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile("unknown", []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockNames(unknown) error = %v, want unsafe path", err)
		}
	})

	t.Run("symlink with digest name", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		digest := digestOf([]byte("target"))
		if err := os.Symlink(filepath.Join(store.root.Name(), "w1234567"), filepath.Join(store.root.Name(), "w1234567", digest)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockNames(symlink) error = %v, want unsafe path", err)
		}
	})

	t.Run("directory with digest name", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		root, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Mkdir(digestOf([]byte("directory")), 0700); err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockNames(directory) error = %v, want unsafe path", err)
		}
	})

	t.Run("symlink with temp name", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		const tempName = ".tmp-0123456789abcdef0123456789abcdef"
		if err := os.Symlink(filepath.Join(store.root.Name(), "w1234567"), filepath.Join(store.root.Name(), "w1234567", tempName)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockNames(temp symlink) error = %v, want unsafe path", err)
		}
	})

	t.Run("workspace symlink", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w7654321", 0700); err != nil {
			t.Fatal(err)
		}
		if err := store.root.Symlink("w7654321", "w1234567"); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ListBlockNames(workspace symlink) error = %v, want unsafe path", err)
		}
	})

	t.Run("oversized digest file", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		file, err := store.root.OpenFile("w1234567/"+digestOf(nil), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(MaxBlockBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListBlockNames("w1234567"); !errors.Is(err, ErrCorruptBlock) {
			t.Fatalf("ListBlockNames(oversized) error = %v, want corrupt block", err)
		}
	})
}

func TestRemoveStaleTempBlocksKeepsVerifiedBlocks(t *testing.T) {
	store := newTestStore(t)
	data := []byte("committed block")
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	if err := workspaceRoot.WriteFile(tempName, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.RemoveStaleTempBlocks("w1234567"); err != nil {
		t.Fatalf("RemoveStaleTempBlocks() error = %v", err)
	}
	if _, err := store.root.Open("w1234567/" + tempName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary block still exists: %v", err)
	}
	if got, err := store.ListBlockDigests("w1234567"); err != nil || len(got) != 1 || got[0] != digest {
		t.Fatalf("ListBlockDigests() = %v, %v; want committed block %s", got, err, digest)
	}
}

func TestRemoveStaleTempBlocksFailsClosedOnUnknownResidue(t *testing.T) {
	store := newTestStore(t)
	if err := store.root.Mkdir("w1234567", 0700); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	if err := workspaceRoot.WriteFile(tempName, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile("unknown", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveStaleTempBlocks("w1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("RemoveStaleTempBlocks() error = %v, want unsafe path", err)
	}
	if _, err := store.root.Open("w1234567/" + tempName); err != nil {
		t.Fatalf("known temp was removed before unknown residue validation: %v", err)
	}
}

func TestRemoveStaleTempBlocksRemovesOversizedTempFiles(t *testing.T) {
	store := newTestStore(t)
	if err := store.root.Mkdir("w1234567", 0700); err != nil {
		t.Fatal(err)
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	file, err := store.root.OpenFile("w1234567/"+tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxBlockBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveStaleTempBlocks("w1234567"); err != nil {
		t.Fatalf("RemoveStaleTempBlocks(oversized temp) error = %v", err)
	}
	if _, err := store.root.Open("w1234567/" + tempName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized temp still exists: %v", err)
	}
}

func TestRemoveStaleTempBlocksRejectsSymlinkAndDirectory(t *testing.T) {
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	t.Run("symlink", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(store.root.Name(), "w1234567", tempName)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := store.RemoveStaleTempBlocks("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveStaleTempBlocks(symlink) error = %v, want unsafe path", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("symlink target was removed: %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		root, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Mkdir(tempName, 0700); err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		if err := store.RemoveStaleTempBlocks("w1234567"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveStaleTempBlocks(directory) error = %v, want unsafe path", err)
		}
		if _, err := os.Stat(filepath.Join(store.root.Name(), "w1234567", tempName)); err != nil {
			t.Fatalf("temporary directory was removed: %v", err)
		}
	})
}

func TestPutBlockReplayIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	data := []byte("same immutable block")
	digest := digestOf(data)
	for i := 0; i < 2; i++ {
		n, err := store.PutBlock("w1234567", digest, bytes.NewReader(data))
		if err != nil || n != int64(len(data)) {
			t.Fatalf("PutBlock replay %d = (%d, %v)", i, n, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(store.root.Name(), "w1234567"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != digest {
		t.Fatalf("workspace files after replay = %v", entryNames(entries))
	}
}

func TestPutBlockRejectsInvalidInputWithoutPublishing(t *testing.T) {
	store := newTestStore(t)
	data := []byte("data")
	digest := digestOf(data)
	cases := []struct {
		name        string
		workspaceID string
		digest      string
		source      io.Reader
		wantErr     error
	}{
		{name: "workspace traversal", workspaceID: "../w1234567", digest: digest, source: bytes.NewReader(data), wantErr: ErrInvalidWorkspaceID},
		{name: "malformed digest", workspaceID: "w1234567", digest: "../" + digest, source: bytes.NewReader(data), wantErr: ErrInvalidDigest},
		{name: "uppercase digest", workspaceID: "w1234567", digest: upperHex(digest), source: bytes.NewReader(data), wantErr: ErrInvalidDigest},
		{name: "mismatched content", workspaceID: "w1234567", digest: digestOf([]byte("other")), source: bytes.NewReader(data), wantErr: ErrDigestMismatch},
		{name: "oversized content", workspaceID: "w1234567", digest: digestOf(bytes.Repeat([]byte{'x'}, int(MaxBlockBytes+1))), source: bytes.NewReader(bytes.Repeat([]byte{'x'}, int(MaxBlockBytes+1))), wantErr: ErrBlockTooLarge},
		{name: "nil source", workspaceID: "w1234567", digest: digest, wantErr: ErrInvalidSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.PutBlock(tc.workspaceID, tc.digest, tc.source); !errors.Is(err, tc.wantErr) {
				t.Fatalf("PutBlock() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
	if _, err := store.Open("w1234567", digest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() after rejected writes error = %v, want not-exist", err)
	}
}

func TestPutBlockRejectsCorruptExistingFileWithoutOverwriting(t *testing.T) {
	store := newTestStore(t)
	data := []byte("expected data")
	digest := digestOf(data)
	if err := store.root.Mkdir("w1234567", 0700); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile(digest, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("PutBlock() error = %v, want corrupt-block", err)
	}
	reader, err := store.Open("w1234567", digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "corrupt" {
		t.Fatalf("existing block = %q, error = %v", got, err)
	}
}

func TestBlocksAreIsolatedByWorkspace(t *testing.T) {
	store := newTestStore(t)
	data := []byte("same bytes, distinct workspace namespaces")
	digest := digestOf(data)
	for _, workspaceID := range []string{"w1234567", "w7654321"} {
		if _, err := store.PutBlock(workspaceID, digest, bytes.NewReader(data)); err != nil {
			t.Fatalf("PutBlock(%q) error = %v", workspaceID, err)
		}
	}
	if err := store.DeleteWorkspace("w1234567"); err != nil {
		t.Fatalf("DeleteWorkspace() error = %v", err)
	}
	reader, err := store.Open("w7654321", digest)
	if err != nil {
		t.Fatalf("Open(other workspace) error = %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("other workspace block = %q, error = %v", got, err)
	}
}

func TestDeleteBlockIsWorkspaceScopedAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	data := []byte("same block in separate workspaces")
	digest := digestOf(data)
	for _, workspaceID := range []string{"w1234567", "w7654321"} {
		if _, err := store.PutBlock(workspaceID, digest, bytes.NewReader(data)); err != nil {
			t.Fatalf("PutBlock(%q) error = %v", workspaceID, err)
		}
	}
	wantRemoved := statAllocatedBytes(t, filepath.Join(store.root.Name(), "w1234567", digest))
	if removed, err := store.DeleteBlock("w1234567", digest); err != nil || removed != wantRemoved {
		t.Fatalf("DeleteBlock() = (%d, %v), want (%d, nil)", removed, err, wantRemoved)
	}
	if removed, err := store.DeleteBlock("w1234567", digest); err != nil || removed != 0 {
		t.Fatalf("DeleteBlock(replay) = (%d, %v), want (0, nil)", removed, err)
	}
	if removed, err := store.DeleteBlock("w1234567", digestOf(nil)); err != nil || removed != 0 {
		t.Fatalf("DeleteBlock(missing block) = (%d, %v), want (0, nil)", removed, err)
	}
	if removed, err := store.DeleteBlock("w8765432", digest); err != nil || removed != 0 {
		t.Fatalf("DeleteBlock(missing workspace) = (%d, %v), want (0, nil)", removed, err)
	}
	if _, err := store.Open("w1234567", digest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open(deleted block) error = %v, want not-exist", err)
	}
	reader, err := store.Open("w7654321", digest)
	if err != nil {
		t.Fatalf("Open(other workspace) error = %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("other workspace block = %q, error = %v", got, err)
	}
}

func TestAllocatedBytesAndReservationUseFilesystemBlocks(t *testing.T) {
	store := newTestStore(t)
	data := bytes.Repeat([]byte("workspace allocation"), 819)
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	want := statAllocatedBytes(t, filepath.Join(store.root.Name(), "w1234567", digest))
	got, err := store.BlockAllocatedBytes("w1234567", digest)
	if err != nil || got != want {
		t.Fatalf("BlockAllocatedBytes() = (%d, %v), want (%d, nil)", got, err, want)
	}
	reservation, err := store.ReservationBytes(int64(len(data)))
	if err != nil || reservation < got {
		t.Fatalf("ReservationBytes() = (%d, %v), want at least allocated bytes %d", reservation, err, got)
	}
}

func TestScanBlockAllocationsVerifiesAndRejectsResidue(t *testing.T) {
	store := newTestStore(t)
	data := []byte("verified allocated block")
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	const tempName = ".tmp-0123456789abcdef0123456789abcdef"
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile(tempName, []byte("in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.ScanBlockAllocations()
	want := statAllocatedBytes(t, filepath.Join(store.root.Name(), "w1234567", digest))
	if err != nil || len(got) != 1 || got[0] != (BlockAllocation{WorkspaceID: "w1234567", Digest: digest, AllocatedBytes: want}) {
		t.Fatalf("ScanBlockAllocations() = (%+v, %v), want one verified block with %d allocated bytes", got, err, want)
	}

	if err := os.WriteFile(filepath.Join(store.root.Name(), "w1234567", "unknown"), []byte("residue"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ScanBlockAllocations(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ScanBlockAllocations(unknown residue) error = %v, want unsafe path", err)
	}
}

func TestScanBlockAllocationsRejectsCorruptDigestContent(t *testing.T) {
	store := newTestStore(t)
	data := []byte("valid before edit")
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root.Name(), "w1234567", digest), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ScanBlockAllocations(); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("ScanBlockAllocations(corrupt block) error = %v, want corrupt block", err)
	}
}

func TestDeleteBlockRejectsInvalidNamesAndNonRegularEntries(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.DeleteBlock("../w1234567", digestOf(nil)); !errors.Is(err, ErrInvalidWorkspaceID) {
		t.Fatalf("DeleteBlock(invalid workspace) error = %v, want invalid workspace id", err)
	}
	if _, err := store.DeleteBlock("w1234567", "../"+digestOf(nil)); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("DeleteBlock(invalid digest) error = %v, want invalid digest", err)
	}
	if _, err := store.DeleteBlock("w1234567", upperHex(digestOf(nil))); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("DeleteBlock(uppercase digest) error = %v, want invalid digest", err)
	}

	directoryDigest := digestOf([]byte("directory"))
	if err := store.root.Mkdir("w1234567", 0700); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Mkdir(directoryDigest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteBlock("w1234567", directoryDigest); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DeleteBlock(directory) error = %v, want unsafe path", err)
	}
	if _, err := os.Stat(filepath.Join(store.root.Name(), "w1234567", directoryDigest)); err != nil {
		t.Fatalf("DeleteBlock(directory) removed the directory: %v", err)
	}

	symlinkDigest := digestOf([]byte("symlink"))
	if err := os.Symlink(filepath.Join(store.root.Name(), "w1234567", directoryDigest), filepath.Join(store.root.Name(), "w1234567", symlinkDigest)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.DeleteBlock("w1234567", symlinkDigest); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DeleteBlock(symlink) error = %v, want unsafe path", err)
	}
}

func TestConcurrentPutSameBlock(t *testing.T) {
	store := newTestStore(t)
	data := bytes.Repeat([]byte("parallel"), 32*1024)
	digest := digestOf(data)
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
				errs <- err
			} else if n != int64(len(data)) {
				errs <- errors.New("unexpected block length")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent PutBlock() error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.root.Name(), "w1234567"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != digest {
		t.Fatalf("workspace files after concurrent writes = %v", entryNames(entries))
	}
}

func TestOpenRejectsUnsafePathsAndClosedStore(t *testing.T) {
	root := t.TempDir()
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(rootLink); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open(symlink root) error = %v, want unsafe path", err)
	}
	if _, err := Open("relative/path"); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("Open(relative) error = %v, want invalid root", err)
	}
	if _, err := Open(string(filepath.Separator)); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("Open(root directory) error = %v, want invalid root", err)
	}

	store := newTestStore(t)
	if err := store.root.Symlink("w7654321", "w1234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBlock("w1234567", digestOf(nil), bytes.NewReader(nil)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("PutBlock(symlink workspace) error = %v, want unsafe path", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := store.PutBlock("w7654321", digestOf(nil), bytes.NewReader(nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("PutBlock(closed) error = %v, want closed", err)
	}
	if err := store.DeleteWorkspace("w7654321"); !errors.Is(err, ErrClosed) {
		t.Fatalf("DeleteWorkspace(closed) error = %v, want closed", err)
	}
}

func TestOpenExistingDoesNotCreateOrChangeRootPermissions(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Chmod(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExisting(rootPath)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("root permissions after OpenExisting() = %v, want 0755", info.Mode().Perm())
	}

	missingPath := filepath.Join(t.TempDir(), "missing-store")
	if _, err := OpenExisting(missingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting(missing) error = %v, want not-exist", err)
	}
	if _, err := os.Stat(missingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting(missing) created the directory: stat error = %v", err)
	}
}

func TestMissingWorkspaceAndDeleteAreIdempotent(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Open("w1234567", digestOf(nil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open(missing workspace) error = %v, want not-exist", err)
	}
	if err := store.DeleteWorkspace("w1234567"); err != nil {
		t.Fatalf("DeleteWorkspace(missing) error = %v", err)
	}
	if _, err := store.PutBlock("w1234567", digestOf(nil), bytes.NewReader(nil)); err != nil {
		t.Fatalf("PutBlock(empty) error = %v", err)
	}
	if err := store.DeleteWorkspace("w1234567"); err != nil {
		t.Fatalf("DeleteWorkspace(existing) error = %v", err)
	}
	if err := store.DeleteWorkspace("w1234567"); err != nil {
		t.Fatalf("DeleteWorkspace(replay) error = %v", err)
	}
	if err := store.DeleteWorkspace("../w1234567"); !errors.Is(err, ErrInvalidWorkspaceID) {
		t.Fatalf("DeleteWorkspace(traversal) error = %v, want invalid workspace id", err)
	}
}

func TestRejectsNonDirectoryWorkspaceEntry(t *testing.T) {
	store := newTestStore(t)
	if err := store.root.WriteFile("w1234567", []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBlock("w1234567", digestOf(nil), bytes.NewReader(nil)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("PutBlock(file workspace entry) error = %v, want unsafe path", err)
	}
	if err := store.DeleteWorkspace("w1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DeleteWorkspace(file entry) error = %v, want unsafe path", err)
	}
}

func TestPutBlockReadFailureDoesNotPublish(t *testing.T) {
	store := newTestStore(t)
	readErr := errors.New("source read failed")
	if _, err := store.PutBlock("w1234567", digestOf(nil), failingReader{err: readErr}); !errors.Is(err, readErr) {
		t.Fatalf("PutBlock() error = %v, want source error", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.root.Name(), "w1234567"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace files after read failure = %v", entryNames(entries))
	}
}

func TestPutBlockAcceptsMaximumSize(t *testing.T) {
	store := newTestStore(t)
	data := bytes.Repeat([]byte{'x'}, int(MaxBlockBytes))
	digest := digestOf(data)
	n, err := store.PutBlock("w1234567", digest, bytes.NewReader(data))
	if err != nil || n != MaxBlockBytes {
		t.Fatalf("PutBlock(max block) = (%d, %v)", n, err)
	}
	if err := store.VerifyBlock("w1234567", digest, MaxBlockBytes); err != nil {
		t.Fatalf("VerifyBlock(max block) error = %v", err)
	}
	allocatedBytes, err := store.BlockAllocatedBytes("w1234567", digest)
	if err != nil {
		t.Fatalf("BlockAllocatedBytes(max block) error = %v", err)
	}
	stats, err := store.VerifyAll()
	if err != nil || stats != (VerifyStats{BlockCount: 1, LogicalBytes: MaxBlockBytes, AllocatedBytes: allocatedBytes}) {
		t.Fatalf("VerifyAll(max block) = (%+v, %v)", stats, err)
	}
}

func TestVerifyBlockChecksSizeAndDigest(t *testing.T) {
	store := newTestStore(t)
	data := []byte("referenced workspace content")
	digest := digestOf(data)
	if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBlock("w1234567", digest, int64(len(data))); err != nil {
		t.Fatalf("VerifyBlock(valid) error = %v", err)
	}
	if err := store.VerifyBlock("w1234567", digest, int64(len(data)+1)); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("VerifyBlock(wrong size) error = %v, want corrupt-block", err)
	}
	if err := store.VerifyBlock("w1234567", digest, -1); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("VerifyBlock(negative size) error = %v, want corrupt-block", err)
	}
	if err := store.VerifyBlock("w1234567", digest, MaxBlockBytes+1); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("VerifyBlock(oversized reference) error = %v, want corrupt-block", err)
	}
	if err := store.VerifyBlock("bad", digest, int64(len(data))); !errors.Is(err, ErrInvalidWorkspaceID) {
		t.Fatalf("VerifyBlock(invalid workspace) error = %v", err)
	}
	if err := store.VerifyBlock("w1234567", "bad", int64(len(data))); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("VerifyBlock(invalid digest) error = %v", err)
	}

	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile(digest, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = workspaceRoot.Close()
	if err := store.VerifyBlock("w1234567", digest, int64(len(data))); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("VerifyBlock(corrupt content) error = %v, want corrupt-block", err)
	}
}

func TestVerifyAllCountsBlocksAndRejectsUnknownResidue(t *testing.T) {
	store := newTestStore(t)
	for _, item := range []struct {
		workspaceID string
		data        []byte
	}{
		{workspaceID: "w1234567", data: []byte("one")},
		{workspaceID: "w1234567", data: nil},
		{workspaceID: "w7654321", data: []byte("another block")},
	} {
		if _, err := store.PutBlock(item.workspaceID, digestOf(item.data), bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.VerifyAll()
	if err != nil {
		t.Fatalf("VerifyAll() error = %v", err)
	}
	var allocatedBytes int64
	for _, item := range []struct {
		workspaceID string
		data        []byte
	}{
		{workspaceID: "w1234567", data: []byte("one")},
		{workspaceID: "w1234567", data: nil},
		{workspaceID: "w7654321", data: []byte("another block")},
	} {
		allocated, err := store.BlockAllocatedBytes(item.workspaceID, digestOf(item.data))
		if err != nil {
			t.Fatal(err)
		}
		allocatedBytes += allocated
	}
	if want := (VerifyStats{
		BlockCount: 3, LogicalBytes: int64(len("one") + len("another block")), AllocatedBytes: allocatedBytes,
	}); stats != want {
		t.Fatalf("VerifyAll() = %+v, want %+v", stats, want)
	}

	workspaceRoot, err := store.root.OpenRoot("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.WriteFile(".tmp-leftover", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = workspaceRoot.Close()
	if _, err := store.VerifyAll(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("VerifyAll(unrecognized file) error = %v, want unsafe path", err)
	}
	if _, err := os.Lstat(filepath.Join(store.root.Name(), "w1234567", ".tmp-leftover")); err != nil {
		t.Fatalf("VerifyAll() removed the unrecognized residue: %v", err)
	}
}

func TestAddVerifyStatsRejectsOverflow(t *testing.T) {
	stats := VerifyStats{BlockCount: 1, LogicalBytes: maxVerifyTotal - 1, AllocatedBytes: 7}
	if addVerifyStats(&stats, 2, 3) {
		t.Fatal("addVerifyStats() accepted logical byte total overflow")
	}
	if stats != (VerifyStats{BlockCount: 1, LogicalBytes: maxVerifyTotal - 1, AllocatedBytes: 7}) {
		t.Fatalf("overflow changed stats to %+v", stats)
	}
	stats = VerifyStats{BlockCount: 1, LogicalBytes: 7, AllocatedBytes: maxVerifyTotal - 1}
	if addVerifyStats(&stats, 3, 2) {
		t.Fatal("addVerifyStats() accepted allocated byte total overflow")
	}
	if stats != (VerifyStats{BlockCount: 1, LogicalBytes: 7, AllocatedBytes: maxVerifyTotal - 1}) {
		t.Fatalf("allocated overflow changed stats to %+v", stats)
	}
	stats = VerifyStats{BlockCount: maxVerifyTotal}
	if addVerifyStats(&stats, 0, 0) {
		t.Fatal("addVerifyStats() accepted block count overflow")
	}
}

func TestVerifyAllRejectsCorruptAndUnsafeEntries(t *testing.T) {
	t.Run("corrupt hash", func(t *testing.T) {
		store := newTestStore(t)
		data := []byte("expected")
		digest := digestOf(data)
		if _, err := store.PutBlock("w1234567", digest, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		workspaceRoot, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := workspaceRoot.WriteFile(digest, []byte("corrupt!"), 0600); err != nil {
			t.Fatal(err)
		}
		_ = workspaceRoot.Close()
		if _, err := store.VerifyAll(); !errors.Is(err, ErrCorruptBlock) {
			t.Fatalf("VerifyAll(corrupt hash) error = %v, want corrupt-block", err)
		}
	})

	t.Run("oversized block", func(t *testing.T) {
		store := newTestStore(t)
		data := bytes.Repeat([]byte{'x'}, int(MaxBlockBytes+1))
		digest := digestOf(data)
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		workspaceRoot, err := store.root.OpenRoot("w1234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := workspaceRoot.WriteFile(digest, data, 0600); err != nil {
			t.Fatal(err)
		}
		_ = workspaceRoot.Close()
		if _, err := store.VerifyAll(); !errors.Is(err, ErrCorruptBlock) {
			t.Fatalf("VerifyAll(oversized block) error = %v, want corrupt-block", err)
		}
	})

	t.Run("invalid workspace entry", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.WriteFile("unexpected", nil, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.VerifyAll(); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("VerifyAll(invalid workspace name) error = %v, want unsafe path", err)
		}
	})

	t.Run("workspace symlink", func(t *testing.T) {
		store := newTestStore(t)
		if err := store.root.Mkdir("w7654321", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(store.root.Name(), "w7654321"), filepath.Join(store.root.Name(), "w1234567")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.VerifyAll(); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("VerifyAll(workspace symlink) error = %v, want unsafe path", err)
		}
	})

	t.Run("block symlink", func(t *testing.T) {
		store := newTestStore(t)
		data := []byte("target")
		digest := digestOf(data)
		if _, err := store.PutBlock("w7654321", digest, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := store.root.Mkdir("w1234567", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(store.root.Name(), "w7654321", digest), filepath.Join(store.root.Name(), "w1234567", digest)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.VerifyAll(); !errors.Is(err, ErrCorruptBlock) {
			t.Fatalf("VerifyAll(block symlink) error = %v, want corrupt-block", err)
		}
	})
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "workspace-data"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func digestOf(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func statAllocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 {
		t.Fatalf("stat %q does not report allocated blocks", path)
	}
	return stat.Blocks * 512
}

func upperHex(value string) string {
	upper := []byte(value)
	for i := range upper {
		if upper[i] >= 'a' && upper[i] <= 'f' {
			upper[i] -= 'a' - 'A'
		}
	}
	return string(upper)
}

func entryNames(entries []os.DirEntry) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Name())
	}
	return values
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
