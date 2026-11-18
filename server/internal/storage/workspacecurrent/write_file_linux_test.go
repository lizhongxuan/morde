//go:build linux

package workspacecurrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestWriteRegularFileCreatesParentsAndChecksCurrentFile(t *testing.T) {
	store := openSymlinkTestStore(t)
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRegularFile(context.Background(), "u1234567", "w1234567", "reports/today.txt", []byte("first"), 0644, ""); err != nil {
		t.Fatalf("create nested file: %v", err)
	}
	digest := sha256.Sum256([]byte("first"))
	oldSHA := hex.EncodeToString(digest[:])
	if err := store.WriteRegularFile(context.Background(), "u1234567", "w1234567", "reports/today.txt", []byte("second"), 0644, oldSHA); err != nil {
		t.Fatalf("replace existing file: %v", err)
	}
	if err := store.WriteRegularFile(context.Background(), "u1234567", "w1234567", "reports/today.txt", []byte("stale"), 0644, oldSHA); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("stale file write error = %v, want ErrWriteConflict", err)
	}
	file, _, err := store.OpenRegularFile("u1234567", "w1234567", "reports/today.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := os.ReadFile(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "reports", "today.txt"))
	if err != nil || string(content) != "second" {
		t.Fatalf("file after stale write = %q, err=%v", content, err)
	}
}

func TestWriteRegularFileFromRejectsUnverifiedContentWithoutReplacingTarget(t *testing.T) {
	store := openSymlinkTestStore(t)
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	const original = "preserve me"
	if err := store.WriteRegularFile(context.Background(), "u1234567", "w1234567", "report.bin", []byte(original), 0644, ""); err != nil {
		t.Fatal(err)
	}
	originalDigest := sha256.Sum256([]byte(original))
	expectedSHA := hex.EncodeToString(originalDigest[:])
	wrongDigest := sha256.Sum256([]byte("not the content"))
	validNewDigest := sha256.Sum256([]byte("new content"))
	for _, test := range []struct {
		name   string
		body   string
		size   int64
		digest string
	}{
		{name: "digest mismatch", body: "new content", size: int64(len("new content")), digest: hex.EncodeToString(wrongDigest[:])},
		{name: "short body", body: "new", size: int64(len("new content")), digest: hex.EncodeToString(validNewDigest[:])},
		{name: "long body", body: "new content plus extra", size: int64(len("new content")), digest: hex.EncodeToString(validNewDigest[:])},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := store.WriteRegularFileFrom(context.Background(), "u1234567", "w1234567", "report.bin", bytes.NewBufferString(test.body),
				test.size, test.digest, 0644, expectedSHA)
			if err == nil {
				t.Fatal("WriteRegularFileFrom() accepted content that did not match its declared size and digest")
			}
			got, err := os.ReadFile(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "report.bin"))
			if err != nil || string(got) != original {
				t.Fatalf("target after rejected write = %q, %v; want original content", got, err)
			}
		})
	}
}

func TestWorkspaceCurrentWriteRejectsSymbolicLinkParents(t *testing.T) {
	store := openSymlinkTestStore(t)
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadSymbolicLink("u1234567", "w1234567", "linked"); err != nil {
		t.Fatalf("ReadSymbolicLink(): %v", err)
	}
	if err := store.WriteRegularFile(context.Background(), "u1234567", "w1234567", "linked/out.txt", []byte("blocked"), 0644, ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("write through symbolic link error = %v, want ErrUnsafePath", err)
	}
}
