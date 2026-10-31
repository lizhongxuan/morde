package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteRemovesWorkspaceTreeMarkerAndStaleStage(t *testing.T) {
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
	ownerHash := ownerHash("u1234567")
	ownerRoot := filepath.Join(root, ownerHash)
	target := filepath.Join(ownerRoot, "w1234567")
	stage := filepath.Join(root, ".morde-staging", ownerHash+"-w1234567-old")
	marker := filepath.Join(root, revisionMarkerDirectory, ownerHash+"-w1234567.revision")
	for _, dir := range []string{target, stage} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(target, "secret.txt"), marker, filepath.Join(stage, "secret.txt")} {
		if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(context.Background(), "u1234567", "w1234567"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{target, marker, stage} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("deleted Workspace artifact %q still exists: %v", path, err)
		}
	}
	if err := store.Delete(context.Background(), "u1234567", "w1234567"); err != nil {
		t.Fatalf("repeat deletion: %v", err)
	}
}

func TestDeleteRejectsWorkspaceSymlinkWithoutRemovingTarget(t *testing.T) {
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
	ownerRoot := filepath.Join(root, ownerHash("u1234567"))
	if err := os.Mkdir(ownerRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ownerRoot, "w1234567")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), "u1234567", "w1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Delete(symlink) error = %v; want ErrUnsafePath", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("symlink target was changed: %v", err)
	}
}

func ownerHash(ownerID string) string {
	digest := sha256.Sum256([]byte(ownerID))
	return hex.EncodeToString(digest[:])
}
