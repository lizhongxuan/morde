package operationstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendReopenAndHashChain(t *testing.T) {
	dir := newStoreDir(t)
	store, err := Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("runtime_state", map[string]any{"mode": "blocked"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("operation", map[string]any{"status": "accepted", "operation_id": "o1234567"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[0].Hash != events[1].PreviousHash {
		t.Fatalf("unexpected replayed journal: %#v", events)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreIsExclusiveForItsLifetime(t *testing.T) {
	dir := newStoreDir(t)
	first, err := Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Open(dir, uint32(os.Geteuid())); !errors.Is(err, ErrStoreInUse) {
		t.Fatalf("second Open error = %v, want ErrStoreInUse", err)
	}
}

func TestStoreRejectsUnsafeDirectoryAndFiles(t *testing.T) {
	parent := t.TempDir()
	unsafe := filepath.Join(parent, "unsafe")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(unsafe, uint32(os.Geteuid())); !errors.Is(err, ErrUnsafeDirectory) {
		t.Fatalf("Open unsafe directory error = %v", err)
	}

	dir := filepath.Join(parent, "safe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "target"), filepath.Join(dir, journalFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, uint32(os.Geteuid())); !errors.Is(err, ErrUnsafeDirectory) {
		t.Fatalf("Open symlink journal error = %v", err)
	}
}

func TestStoreFailsClosedOnPartialOrCorruptJournal(t *testing.T) {
	t.Run("partial final record", func(t *testing.T) {
		dir := newStoreDir(t)
		if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, uint32(os.Geteuid())); err == nil {
			t.Fatal("Open accepted partial final record")
		}
	})
	t.Run("tampered hash", func(t *testing.T) {
		dir := newStoreDir(t)
		store, err := Open(dir, uint32(os.Geteuid()))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append("runtime_state", map[string]any{"mode": "blocked"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, journalFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for index := range data {
			if data[index] == 'a' {
				data[index] = 'b'
				break
			}
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, uint32(os.Geteuid())); err == nil {
			t.Fatal("Open accepted tampered journal")
		}
	})
}

func newStoreDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
