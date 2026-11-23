//go:build darwin || linux

package mordesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestProjectFingerprintTracksChangesAndRejectsReplacedRoot(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	device, inode := fmt.Sprint(stat.Dev), fmt.Sprint(stat.Ino)
	fingerprint := func() string {
		t.Helper()
		manifest, err := ScanMatchingIdentity(context.Background(), directory, device, inode)
		if err != nil {
			t.Fatal(err)
		}
		value, err := ManifestFingerprint(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := fingerprint()
	file := filepath.Join(directory, "note.txt")
	if err := os.WriteFile(file, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	added := fingerprint()
	if added == before {
		t.Fatal("added file did not change the fingerprint")
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	modified := fingerprint()
	if modified == added {
		t.Fatal("modified file did not change the fingerprint")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if fingerprint() == modified {
		t.Fatal("deleted file did not change the fingerprint")
	}
	if _, err := ScanMatchingIdentity(context.Background(), directory, device, "0"); err == nil {
		t.Fatal("scan accepted a root with the wrong identity")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(directory, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanMatchingIdentity(context.Background(), directory, device, inode); err == nil {
		t.Fatal("scan accepted a symlink outside the selected project")
	}
	if err := os.Remove(filepath.Join(directory, "outside")); err != nil {
		t.Fatal(err)
	}
	replaced := directory + "-replacement"
	if err := os.Rename(directory, replaced); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(replaced, directory)
	if err := os.Symlink(t.TempDir(), directory); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanMatchingIdentity(context.Background(), directory, device, inode); err == nil {
		t.Fatal("scan accepted a replaced symlink root")
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
}
