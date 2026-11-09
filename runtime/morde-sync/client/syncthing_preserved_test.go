package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testSyncthingFolderID = "morde-0123456789abcdef0123456789abcdef"

func makeSyncthingPreservedFixture(t *testing.T, folderID, entryID, relative string, create func(string)) (string, string) {
	t.Helper()
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	folderHash := sha256.Sum256([]byte(folderID))
	archive := filepath.Join(parent, preservedDirectory, hex.EncodeToString(folderHash[:]), entryID)
	if err := os.MkdirAll(filepath.Join(archive, filepath.Dir(filepath.FromSlash(relative))), 0o700); err != nil {
		t.Fatal(err)
	}
	create(filepath.Join(archive, filepath.FromSlash(relative)))
	metadata := syncthingPreservedMetadata{
		Version: 1, EntryID: entryID, FolderID: folderID, Path: relative, CreatedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(archive), entryID+syncthingPreservedMetadataSuffix), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, archive
}

func TestSyncthingPreservedListRestoreAndDeleteFile(t *testing.T) {
	directory, archive := makeSyncthingPreservedFixture(t, testSyncthingFolderID, "AbcdefghijklmnopQRST", "docs/note.txt", func(path string) {
		if err := os.WriteFile(path, []byte("recovered"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	items, err := ListSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID)
	if err != nil || len(items) != 1 || items[0].Path != "docs/note.txt" || items[0].Size != int64(len("recovered")) {
		t.Fatalf("ListSyncthingPreserved() = %#v, %v", items, err)
	}
	if err := RestoreSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID, items[0].ID); err != nil {
		t.Fatalf("RestoreSyncthingPreserved() = %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(directory, "docs", "note.txt")); err != nil || string(contents) != "recovered" {
		t.Fatalf("restored file = %q, %v", contents, err)
	}
	if err := RestoreSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID, items[0].ID); !errors.Is(err, ErrPreservedDestinationExists) {
		t.Fatalf("restore over existing file = %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("restore removed the archive: %v", err)
	}
	if err := DeleteSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID, items[0].ID); err != nil {
		t.Fatalf("DeleteSyncthingPreserved() = %v", err)
	}
	items, err = ListSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID)
	if err != nil || len(items) != 0 {
		t.Fatalf("list after delete = %#v, %v", items, err)
	}
}

func TestSyncthingPreservedDirectorySizeAndSymlinkRestore(t *testing.T) {
	directory, archive := makeSyncthingPreservedFixture(t, testSyncthingFolderID, "ABCDEFGHIJKLMNOPQRST", "tree", func(path string) {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "note.txt"), []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("note.txt", filepath.Join(path, "latest.txt")); err != nil {
			t.Fatal(err)
		}
	})
	items, err := ListSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID)
	if err != nil || len(items) != 1 || items[0].Path != "tree" || items[0].Size != int64(len("abc")+len("note.txt")) {
		t.Fatalf("ListSyncthingPreserved(directory) = %#v, %v", items, err)
	}
	if err := RestoreSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID, items[0].ID); err != nil {
		t.Fatalf("restore directory: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(directory, "tree", "latest.txt")); err != nil || target != "note.txt" {
		t.Fatalf("restored symlink = %q, %v", target, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("directory restore removed the archive: %v", err)
	}
}

func TestSyncthingPreservedUnknownOrMalformedEntriesFailClosed(t *testing.T) {
	directory, archive := makeSyncthingPreservedFixture(t, testSyncthingFolderID, "AbcdefghijklmnopQRST", "notes.txt", func(path string) {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Remove(filepath.Join(filepath.Dir(archive), "AbcdefghijklmnopQRST"+syncthingPreservedMetadataSuffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSyncthingPreserved(context.Background(), directory, testWorkspace, testSyncthingFolderID); err == nil {
		t.Fatal("list accepted an archive with no path metadata")
	}
	if _, err := os.Stat(filepath.Join(archive, "notes.txt")); err != nil {
		t.Fatalf("unrecognized archive was changed: %v", err)
	}
}
