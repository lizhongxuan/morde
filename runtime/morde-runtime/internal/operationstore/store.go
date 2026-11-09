// Package operationstore provides the root-only, host-persistent journal used
// by the runtime core. It deliberately has no dependency on the wire contract.
package operationstore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	journalFileName = "operations.log"
	lockFileName    = "operations.lock"
	journalVersion  = 1
	maxJournalLine  = 16 << 20
)

var (
	ErrUnsafeDirectory = errors.New("operation store directory is not root-only")
	ErrStoreInUse      = errors.New("operation store is already open")
	ErrStoreClosed     = errors.New("operation store is closed")
	ErrStoreUnhealthy  = errors.New("operation store is unhealthy")
)

// Event is an append-only state transition. Data must be JSON and must not
// contain credentials or unbounded command output.
type Event struct {
	Version      int             `json:"version"`
	Sequence     uint64          `json:"sequence"`
	Kind         string          `json:"kind"`
	Data         json.RawMessage `json:"data"`
	PreviousHash string          `json:"previous_hash"`
	Hash         string          `json:"hash"`
}

// EventStore is the append-only contract shared by the local migration source
// and the Portal-backed ledger. Implementations must not expose an event until
// Append has durably committed it.
type EventStore interface {
	Events() ([]Event, error)
	Append(kind string, data any) error
	Ready() bool
	Close() error
}

type hashInput struct {
	Version      int             `json:"version"`
	Sequence     uint64          `json:"sequence"`
	Kind         string          `json:"kind"`
	Data         json.RawMessage `json:"data"`
	PreviousHash string          `json:"previous_hash"`
}

// Store holds the exclusive process lock for its lifetime. A second runtime
// generation cannot concurrently mutate one Host's journal.
type Store struct {
	mu       sync.Mutex
	journal  *os.File
	lock     *os.File
	events   []Event
	lastHash string
	poison   error
	closed   bool
}

// Open opens an existing, root-only runtime data directory. The expected UID
// is explicit so development tests can use their own account; production must
// pass 0 and run the service as root. The volume directory itself is provisioned
// by the trusted image bootstrap and is never created here.
func Open(dir string, expectedUID uint32) (*Store, error) {
	if os.Geteuid() != int(expectedUID) {
		return nil, fmt.Errorf("runtime store process uid %d does not match owner uid %d: %w", os.Geteuid(), expectedUID, ErrUnsafeDirectory)
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, fmt.Errorf("runtime store path must be absolute and clean: %w", ErrUnsafeDirectory)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat runtime store directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, ErrUnsafeDirectory
	}
	if err := checkUID(info, expectedUID); err != nil {
		return nil, err
	}

	lock, err := openOwnedFile(filepath.Join(dir, lockFileName), expectedUID)
	if err != nil {
		return nil, fmt.Errorf("open runtime store lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrStoreInUse
		}
		return nil, fmt.Errorf("lock runtime store: %w", err)
	}

	journal, err := openOwnedFile(filepath.Join(dir, journalFileName), expectedUID)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("open runtime journal: %w", err)
	}

	s := &Store{journal: journal, lock: lock}
	if err := s.readJournal(); err != nil {
		_ = journal.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("read runtime journal: %w", err)
	}
	return s, nil
}

// Events returns an immutable copy of the journal as loaded by Open and
// extended by Append.
func (s *Store) Events() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	copyOfEvents := make([]Event, len(s.events))
	copy(copyOfEvents, s.events)
	for i := range copyOfEvents {
		copyOfEvents[i].Data = append(json.RawMessage(nil), copyOfEvents[i].Data...)
	}
	return copyOfEvents, nil
}

// Append durably writes an event before making it visible to the caller.
func (s *Store) Append(kind string, data any) error {
	if kind == "" {
		return errors.New("journal event kind is required")
	}
	encodedData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode journal event: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkReady(); err != nil {
		return err
	}
	input := hashInput{
		Version:      journalVersion,
		Sequence:     uint64(len(s.events) + 1),
		Kind:         kind,
		Data:         encodedData,
		PreviousHash: s.lastHash,
	}
	encodedHashInput, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode journal hash input: %w", err)
	}
	digest := sha256.Sum256(encodedHashInput)
	event := Event{
		Version:      input.Version,
		Sequence:     input.Sequence,
		Kind:         input.Kind,
		Data:         input.Data,
		PreviousHash: input.PreviousHash,
		Hash:         hex.EncodeToString(digest[:]),
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode journal line: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.journal.Seek(0, io.SeekEnd); err != nil {
		return s.poisonWith(fmt.Errorf("seek runtime journal: %w", err))
	}
	if err := writeFull(s.journal, line); err != nil {
		return s.poisonWith(fmt.Errorf("append runtime journal: %w", err))
	}
	if err := s.journal.Sync(); err != nil {
		return s.poisonWith(fmt.Errorf("sync runtime journal: %w", err))
	}
	s.events = append(s.events, event)
	s.lastHash = event.Hash
	return nil
}

// Ready reports whether the journal is still usable. Any append or integrity
// error poisons the store until process restart and a complete re-audit.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.poison == nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	if s.journal != nil {
		if err := s.journal.Sync(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
		closeErr = errors.Join(closeErr, s.journal.Close())
	}
	if s.lock != nil {
		closeErr = errors.Join(closeErr, syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN))
		closeErr = errors.Join(closeErr, s.lock.Close())
	}
	return closeErr
}

func (s *Store) readJournal() error {
	if _, err := s.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(s.journal, 64<<10)
	var previousHash string
	for sequence := uint64(1); ; sequence++ {
		line, err := readLineBounded(reader, maxJournalLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		var event Event
		decoder := json.NewDecoder(bytesReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode event %d: %w", sequence, err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return fmt.Errorf("decode event %d: %w", sequence, err)
		}
		if event.Version != journalVersion || event.Sequence != sequence || event.Kind == "" || len(event.Data) == 0 {
			return fmt.Errorf("invalid event header at sequence %d", sequence)
		}
		if event.PreviousHash != previousHash {
			return fmt.Errorf("journal hash chain mismatch at sequence %d", sequence)
		}
		input := hashInput{
			Version:      event.Version,
			Sequence:     event.Sequence,
			Kind:         event.Kind,
			Data:         event.Data,
			PreviousHash: event.PreviousHash,
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode event %d hash: %w", sequence, err)
		}
		digest := sha256.Sum256(encoded)
		if event.Hash != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("journal integrity mismatch at sequence %d", sequence)
		}
		previousHash = event.Hash
		event.Data = append(json.RawMessage(nil), event.Data...)
		s.events = append(s.events, event)
	}
	s.lastHash = previousHash
	_, err := s.journal.Seek(0, io.SeekEnd)
	return err
}

func (s *Store) checkReady() error {
	if s.closed {
		return ErrStoreClosed
	}
	if s.poison != nil {
		return fmt.Errorf("%w: %v", ErrStoreUnhealthy, s.poison)
	}
	return nil
}

func (s *Store) poisonWith(err error) error {
	s.poison = err
	return fmt.Errorf("%w: %v", ErrStoreUnhealthy, err)
}

func openOwnedFile(path string, expectedUID uint32) (*os.File, error) {
	info, err := os.Lstat(path)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		created = true
		info, err = file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := checkOwnedRegularFile(info, expectedUID); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, ErrUnsafeDirectory
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	if err := checkOwnedRegularFile(openedInfo, expectedUID); err != nil {
		_ = file.Close()
		return nil, err
	}
	if created {
		return file, nil
	}
	return file, nil
}

func checkOwnedRegularFile(info os.FileInfo, expectedUID uint32) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafeDirectory
	}
	return checkUID(info, expectedUID)
}

func checkUID(info os.FileInfo, expectedUID uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(stat.Uid) != expectedUID {
		return ErrUnsafeDirectory
	}
	return nil
}

func readLineBounded(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, fmt.Errorf("runtime journal line exceeds %d bytes", limit)
		}
		line = append(line, fragment...)
		if err == nil {
			return line[:len(line)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("runtime journal has a partial final record")
		}
		return nil, err
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

type byteReader struct{ data []byte }

func bytesReader(data []byte) *byteReader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
