package sitefs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"
)

const (
	testSiteID    = sitedomain.ID("site_0123456789abcdef")
	testReleaseID = sitedomain.ReleaseID("rel_0123456789abcdef")
	testManifest  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestStorePublishesReadOnlyReleaseAndRetriesIdempotently(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "<h1>hello</h1>")

	release, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := release.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != "<h1>hello</h1>" {
		t.Fatalf("release contents = %q", data)
	}
	info, err := os.Stat(filepath.Join(root, "releases", string(testSiteID), string(testReleaseID), "files", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0222 != 0 {
		t.Fatalf("published file remains writable: %v", info.Mode().Perm())
	}

	writeRelease(t, store, "index.html", "<h1>hello</h1>")
	releaseRoot := filepath.Join(root, "releases", string(testSiteID), string(testReleaseID))
	if err := os.Chmod(releaseRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(releaseRoot, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	writeRelease(t, store, "index.html", "<h1>hello</h1>")
	repaired, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatalf("idempotent retry did not restore read-only directory modes: %v", err)
	}
	if err := repaired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDeletesOnlyTheMatchingImmutableRelease(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "discard me")

	if err := store.Delete(testSiteID, testReleaseID, strings.Repeat("f", 64)); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("delete with a mismatched manifest = %v", err)
	}
	verified, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatalf("mismatched delete removed the release: %v", err)
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(testSiteID, testReleaseID, testManifest); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(testSiteID, testReleaseID, testManifest); err != nil {
		t.Fatalf("idempotent delete = %v", err)
	}
	if _, err := store.OpenRelease(testSiteID, testReleaseID, testManifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted release is still available: %v", err)
	}
}

func TestStoreDeleteWaitsForActivePublisher(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- store.Delete(testSiteID, testReleaseID, testManifest) }()
	select {
	case err := <-deleted:
		t.Fatalf("delete passed an active publisher: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete after publisher stopped = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not resume after publisher stopped")
	}
}

func TestStoreDeleteWaitsForOpenRelease(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "still readable")
	release, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- store.Delete(testSiteID, testReleaseID, testManifest) }()
	select {
	case err := <-deleted:
		t.Fatalf("delete passed an open release reader: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete after reader stopped = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not resume after reader closed")
	}
}

func TestReleaseCloseWaitsForOpenFilesBeforeReleasingDeleteLease(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "still readable")
	release, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := release.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := release.Close(); !errors.Is(err, ErrOpenFiles) {
		t.Fatalf("release close with an open file = %v", err)
	}

	deleted := make(chan error, 1)
	go func() { deleted <- store.Delete(testSiteID, testReleaseID, testManifest) }()
	select {
	case err := <-deleted:
		t.Fatalf("delete passed an open file reader: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete after reader stopped = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not resume after reader stopped")
	}
}

func TestStoreRejectsDifferentContentForPublishedRelease(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "expected")

	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("index.html", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "different"); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("write changed content error = %v", err)
	}
	if err := file.Close(); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("close changed content error = %v", err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Begin(testSiteID, testReleaseID, strings.Repeat("f", 64)); !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("different manifest error = %v", err)
	}
}

func TestStoreRetryRejectsCaseFoldedDuplicateFile(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "index.html", "expected")

	releasePath := filepath.Join(root, "releases", string(testSiteID), string(testReleaseID))
	filesPath := filepath.Join(releasePath, "files")
	if err := os.Chmod(releasePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filesPath, 0700); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(filepath.Join(filesPath, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if aliasInfo, err := os.Stat(filepath.Join(filesPath, "INDEX.HTML")); err == nil && os.SameFile(originalInfo, aliasInfo) {
		t.Skip("filesystem is case-insensitive")
	}
	if err := os.WriteFile(filepath.Join(filesPath, "INDEX.HTML"), []byte("unexpected"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.Chmod(filesPath, 0500), os.Chmod(releasePath, 0500)); err != nil {
		t.Fatal(err)
	}

	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("index.html", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "expected"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("retry with a case-folded duplicate file = %v", err)
	}
}

func TestOpenReleaseRejectsWritableOrUnexpectedReleaseEntries(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)
	writeRelease(t, store, "assets/app.js", "content")

	releasePath := filepath.Join(root, "releases", string(testSiteID), string(testReleaseID))
	if err := os.Chmod(releasePath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRelease(testSiteID, testReleaseID, testManifest); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("writable release root error = %v", err)
	}
	if err := os.Chmod(filepath.Join(releasePath, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(releasePath, "files", "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(releasePath, "files"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(releasePath, 0500); err != nil {
		t.Fatal(err)
	}
	readOnlyRelease, err := store.OpenRelease(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyRelease.Open("assets/app.js"); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("writable nested directory error = %v", err)
	}
	if err := readOnlyRelease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(releasePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releasePath, "unexpected"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(releasePath, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRelease(testSiteID, testReleaseID, testManifest); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("unexpected release entry error = %v", err)
	}
}

func TestStoreAbortCleansStagingAndEnforcesLimits(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("assets/app.js", 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "large"); !errors.Is(err, ErrReleaseTooLarge) {
		t.Fatalf("oversized write error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(root, "releases", string(testSiteID), string(testReleaseID))
	if err := os.Mkdir(finalPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("existing release destination error = %v", err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(store.rootPath, ".staging", string(testSiteID))
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging entries remain after abort: %v", entries)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReservesFreeSpace(t *testing.T) {
	store, err := Open(t.TempDir()+"/sites", 1<<20, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("index.html", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "x"); !errors.Is(err, ErrInsufficientFree) {
		t.Fatalf("reserved free-space write error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReservesPendingWritesAcrossReleases(t *testing.T) {
	const reservedBytes = int64(64 << 20)
	const reserveWindow = uint64(96 << 20)
	store, err := Open(t.TempDir()+"/sites", 128<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	freeBytes, err := availableBytes(store.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if freeBytes > uint64(^uint64(0)>>1) {
		t.Skip("filesystem free space exceeds int64 test range")
	}
	first, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Begin(testSiteID, "rel_fedcba9876543210", testManifest)
	if err != nil {
		t.Fatal(err)
	}
	freeBytes, err = availableBytes(store.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if freeBytes > uint64(^uint64(0)>>1) {
		t.Skip("filesystem free space exceeds int64 test range")
	}
	if freeBytes <= reserveWindow {
		t.Skip("filesystem has too little free space for this test")
	}
	store.minFreeBytes = int64(freeBytes - reserveWindow)
	firstWriter := first.(*stagedWriter)
	secondWriter := second.(*stagedWriter)
	if err := firstWriter.reserve(reservedBytes); err != nil {
		t.Fatal(err)
	}
	if err := secondWriter.reserve(reservedBytes); !errors.Is(err, ErrInsufficientFree) {
		t.Fatalf("concurrent reserved write error = %v", err)
	}
	firstWriter.finishWrite(0, reservedBytes)
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := second.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSerializesFreeSpaceReservationsAcrossInstances(t *testing.T) {
	root := t.TempDir() + "/sites"
	first, err := Open(root, 128<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root, 128<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	firstWriter, err := first.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	secondWriter, err := second.Begin(testSiteID, "rel_fedcba9876543210", testManifest)
	if err != nil {
		t.Fatal(err)
	}
	firstFile, err := firstWriter.CreateFile("first.bin", 0600)
	if err != nil {
		t.Fatal(err)
	}
	secondFile, err := secondWriter.CreateFile("second.bin", 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<20)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	freeBytes, err := availableBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if freeBytes > uint64(^uint64(0)>>1) {
		t.Fatal("filesystem free space exceeds int64 test range")
	}
	reserveWindow := uint64(len(chunk)) + uint64(32<<20)
	if freeBytes <= uint64(len(chunk))+reserveWindow {
		t.Fatal("filesystem has too little free space for this test")
	}
	first.minFreeBytes = int64(freeBytes) - int64(reserveWindow)
	second.minFreeBytes = int64(freeBytes) - int64(reserveWindow)
	if _, err := firstFile.Write(chunk); err != nil {
		t.Fatalf("first instance write failed: %v", err)
	}
	if err := firstFile.Close(); err != nil {
		t.Fatalf("sync first instance write: %v", err)
	}
	if _, err := secondFile.Write(chunk); !errors.Is(err, ErrInsufficientFree) {
		t.Fatalf("second instance should observe consumed capacity, got %v", err)
	}
	if err := errors.Join(firstFile.Close(), secondFile.Close()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(firstWriter.Abort(), secondWriter.Abort()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWaitsForActiveStageAndRecoversItAfterWriterStops(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("index.html", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "partial"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		store *Store
		err   error
	}, 1)
	go func() {
		reopened, err := Open(root, 1<<20, 1)
		result <- struct {
			store *Store
			err   error
		}{reopened, err}
	}()
	select {
	case outcome := <-result:
		if outcome.store != nil {
			_ = outcome.store.Close()
		}
		t.Fatalf("startup cleanup passed an active writer: %v", outcome.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		store *Store
		err   error
	}
	select {
	case outcome = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("startup recovery did not resume after the writer stopped")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	staging := filepath.Join(root, ".staging", string(testSiteID))
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stale staging entries remain: %v", entries)
	}
	if err := errors.Join(outcome.store.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPreservesUnrecognizedStagingEntries(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, ".staging", "unexpected")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, 1<<20, 1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unknown staging entry error = %v", err)
	}
	if content, err := os.ReadFile(unknown); err != nil || string(content) != "keep" {
		t.Fatalf("unknown staging data was not preserved: %q, %v", content, err)
	}
}

func TestOpenRemovesInterruptedReadOnlyStage(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, ".staging", string(testSiteID), ".tmp-0123456789abcdef0123456789abcdef")
	files := filepath.Join(stage, "files", "assets")
	if err := os.MkdirAll(files, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "app.js"), []byte("partial"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(files, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stage, "files"), 0500); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted stage still exists: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReclaimsStageAfterPublisherProcessCrash(t *testing.T) {
	root := t.TempDir() + "/sites"
	command := exec.Command(os.Args[0], "-test.run=^TestSitefsCrashHelper$")
	command.Env = append(os.Environ(), "MORDE_SITEFS_CRASH_HELPER=1", "MORDE_SITEFS_CRASH_ROOT="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
			return
		}
		ready <- ""
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("publisher did not become ready: %q; stderr=%s", line, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("publisher did not become ready: %s", stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("publisher exited without the forced crash")
	}

	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".staging", string(testSiteID)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("crashed publisher staging entries remain: %v", entries)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSitefsCrashHelper(t *testing.T) {
	if os.Getenv("MORDE_SITEFS_CRASH_HELPER") != "1" {
		return
	}
	store, err := Open(os.Getenv("MORDE_SITEFS_CRASH_ROOT"), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile("index.html", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "partial before crash"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestMaterializerWritesVerifiedWorkspaceRevisionToSiteStore(t *testing.T) {
	root := t.TempDir() + "/sites"
	store, err := Open(root, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore(t, store, root)

	data := []byte("built from pinned revision")
	digest := sha256.Sum256(data)
	fileDigest := hex.EncodeToString(digest[:])
	file := workspace.File{
		Path: "index.html", SizeBytes: int64(len(data)), SHA256: fileDigest, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: fileDigest, SizeBytes: int64(len(data))}},
	}
	manifestDigest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	manifest := siteManifestReader{page: workspace.ManifestPage{
		WorkspaceID: "w1234567", Revision: 4, Files: []workspace.File{file},
	}}
	blocks := siteBlocks{fileDigest: data}
	materializer, err := app.NewSiteRevisionMaterializer(manifest, blocks, store, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	release := sitedomain.Release{
		ID: testReleaseID, SiteID: testSiteID, WorkspaceID: "w1234567", WorkspaceRevision: 4,
		ManifestSHA256: manifestDigest, ServicePort: 3000, State: sitedomain.ReleaseCandidate,
	}
	if _, err := materializer.Materialize(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	published, err := store.OpenRelease(testSiteID, testReleaseID, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	fileReader, err := published.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(fileReader)
	closeErr := errors.Join(fileReader.Close(), published.Close())
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, data) {
		t.Fatalf("published content = %q", actual)
	}
}

func TestOpenRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, 1<<20, 1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink root error = %v", err)
	}
}

func TestOpenRejectsNonPrivateRootWithoutChangingIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "public")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, 1<<20, 1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("non-private root error = %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("Open changed the configured root mode to %v", info.Mode().Perm())
	}
}

func TestOpenChecksUnknownEntriesBeforeChangingStorageDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".staging"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, 1<<20, 1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unknown root entry error = %v", err)
	}
	info, err := os.Stat(filepath.Join(root, ".staging"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("Open changed the existing staging directory mode to %v", info.Mode().Perm())
	}
}

func writeRelease(t *testing.T, store *Store, path, value string) {
	t.Helper()
	writer, err := store.Begin(testSiteID, testReleaseID, testManifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFile(path, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, value); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		_ = writer.Abort()
		t.Fatal(err)
	}
}

func cleanupStore(t *testing.T, store *Store, root string) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Errorf("close site store: %v", err)
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
		}
		_ = os.Chmod(path, mode)
		return nil
	})
}

type siteManifestReader struct{ page workspace.ManifestPage }

func (r siteManifestReader) GetWorkspaceManifestPage(_ context.Context, workspaceID string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if workspaceID != r.page.WorkspaceID || revision != r.page.Revision || prefix != "" || after != "" || limit != 1000 {
		return workspace.ManifestPage{}, fmt.Errorf("unexpected manifest request %s/%d %q %q %d", workspaceID, revision, prefix, after, limit)
	}
	return r.page, nil
}

type siteBlocks map[string][]byte

func (siteBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("unexpected block upload")
}

func (b siteBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	data, exists := b[digest]
	if !exists {
		return nil, fmt.Errorf("missing block %s", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
