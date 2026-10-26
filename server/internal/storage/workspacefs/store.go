package workspacefs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"morde/server/internal/domain"
)

const MaxBlockBytes int64 = 4 << 20
const maxVerifyTotal int64 = 1<<63 - 1

var (
	ErrInvalidRoot        = errors.New("workspace storage root must be an absolute non-root directory")
	ErrInvalidWorkspaceID = errors.New("workspace id is invalid")
	ErrInvalidDigest      = errors.New("block digest must be a lowercase sha-256 hex string")
	ErrInvalidSource      = errors.New("block source is nil")
	ErrBlockTooLarge      = errors.New("block exceeds 4 MiB")
	ErrDigestMismatch     = errors.New("block content does not match digest")
	ErrCorruptBlock       = errors.New("stored block does not match its digest")
	ErrUnsafePath         = errors.New("workspace storage path is not a regular directory or file")
	ErrClosed             = errors.New("workspace storage is closed")
)

// Store keeps immutable blocks under one private directory per workspace.
// The root directory must be dedicated to this store and writable only by its
// service account.
type Store struct {
	root     *os.Root
	rootPath string
	mu       sync.RWMutex
	dead     bool
}

// BlockAllocation reports allocated data bytes for one verified immutable block.
type BlockAllocation struct {
	WorkspaceID    string
	Digest         string
	AllocatedBytes int64
}

// VerifyStats reports logical and allocated data bytes for verified blocks.
type VerifyStats struct {
	BlockCount     int64
	LogicalBytes   int64
	AllocatedBytes int64
}

// Open creates or opens a private, persistent store root. Existing contents
// are retained; callers must configure a dedicated directory for this store.
func Open(rootPath string) (*Store, error) {
	return open(rootPath, true)
}

// OpenExisting opens an existing store without creating it or changing its
// permissions. It is intended for read-only maintenance checks.
func OpenExisting(rootPath string) (*Store, error) {
	return open(rootPath, false)
}

func open(rootPath string, initialize bool) (*Store, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) == string(filepath.Separator) {
		return nil, ErrInvalidRoot
	}
	rootPath = filepath.Clean(rootPath)
	if info, err := os.Lstat(rootPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
	} else if errors.Is(err, os.ErrNotExist) && initialize {
		if err := os.Mkdir(rootPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create workspace storage root: %w", err)
		}
		if err := syncPathDir(filepath.Dir(rootPath)); err != nil {
			return nil, fmt.Errorf("sync workspace storage parent: %w", err)
		}
	} else {
		return nil, fmt.Errorf("inspect workspace storage root: %w", err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace storage root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrUnsafePath
	}
	if initialize {
		if err := os.Chmod(rootPath, 0700); err != nil {
			return nil, fmt.Errorf("protect workspace storage root: %w", err)
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace storage root: %w", err)
	}
	return &Store{root: root, rootPath: rootPath}, nil
}

// ReservationBytes returns a safe upper bound for allocating one block with
// the requested logical size on this store's filesystem.
func (s *Store) ReservationBytes(size int64) (int64, error) {
	if size < 0 || size > MaxBlockBytes {
		return 0, ErrBlockTooLarge
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return 0, ErrClosed
	}
	if size == 0 {
		return 0, nil
	}
	unit, err := filesystemAllocationUnit(s.rootPath)
	if err != nil {
		return 0, err
	}
	blocks := (size-1)/unit + 1
	if blocks > maxVerifyTotal/unit {
		return 0, errors.New("workspace block reservation overflows int64")
	}
	return blocks * unit, nil
}

// BlockAllocatedBytes verifies an existing block and returns its allocated
// data bytes, excluding directory and filesystem metadata.
func (s *Store) BlockAllocatedBytes(workspaceID, digest string) (int64, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return 0, ErrInvalidWorkspaceID
	}
	if !validDigest(digest) {
		return 0, ErrInvalidDigest
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return 0, ErrClosed
	}
	root, err := s.openWorkspace(workspaceID, false)
	if err != nil {
		return 0, err
	}
	_, allocatedBytes, verifyErr := verifyBlockAllocated(root, digest, nil)
	if closeErr := root.Close(); verifyErr == nil && closeErr != nil {
		verifyErr = fmt.Errorf("close workspace %q after allocation check: %w", workspaceID, closeErr)
	}
	return allocatedBytes, verifyErr
}

// ListWorkspaceIDs returns the validated workspace directories in this store.
func (s *Store) ListWorkspaceIDs() (ids []string, retErr error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return nil, ErrClosed
	}
	entries, err := readEntries(s.root)
	if err != nil {
		return nil, fmt.Errorf("read workspace storage root: %w", err)
	}
	ids = make([]string, 0, len(entries))
	for _, entry := range entries {
		workspaceID := entry.Name()
		if !domain.ValidWorkspaceID(workspaceID) {
			return nil, fmt.Errorf("unrecognized workspace storage entry %q: %w", workspaceID, ErrUnsafePath)
		}
		root, err := s.openWorkspace(workspaceID, false)
		if err != nil {
			return nil, err
		}
		if err := root.Close(); err != nil {
			return nil, fmt.Errorf("close workspace %q after listing: %w", workspaceID, err)
		}
		ids = append(ids, workspaceID)
	}
	sort.Strings(ids)
	return ids, nil
}

// ScanBlockAllocations verifies every digest-named block and returns its
// allocated data bytes. Recognized regular upload temp files are ignored.
func (s *Store) ScanBlockAllocations() (allocations []BlockAllocation, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil, ErrClosed
	}
	entries, err := readEntries(s.root)
	if err != nil {
		return nil, fmt.Errorf("read workspace storage root: %w", err)
	}
	allocations = make([]BlockAllocation, 0)
	for _, entry := range entries {
		workspaceID := entry.Name()
		if !domain.ValidWorkspaceID(workspaceID) {
			return nil, fmt.Errorf("unrecognized workspace storage entry %q: %w", workspaceID, ErrUnsafePath)
		}
		info, err := s.root.Lstat(workspaceID)
		if err != nil {
			return nil, fmt.Errorf("inspect workspace storage entry %q: %w", workspaceID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("workspace storage entry %q: %w", workspaceID, ErrUnsafePath)
		}
		root, err := s.openWorkspace(workspaceID, false)
		if err != nil {
			return nil, err
		}
		openedInfo, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("stat workspace storage entry %q: %w", workspaceID, err)
		}
		if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = root.Close()
			return nil, fmt.Errorf("workspace storage entry %q changed during scan: %w", workspaceID, ErrUnsafePath)
		}
		blocks, err := readEntries(root)
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("read workspace %q: %w", workspaceID, err)
		}
		for _, block := range blocks {
			digest := block.Name()
			if !validDigest(digest) {
				if validTempBlockName(digest) {
					tempInfo, err := root.Lstat(digest)
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if err != nil {
						_ = root.Close()
						return nil, fmt.Errorf("inspect temporary workspace block: %w", err)
					}
					if tempInfo.Mode()&os.ModeSymlink != 0 || !tempInfo.Mode().IsRegular() {
						_ = root.Close()
						return nil, ErrUnsafePath
					}
					continue
				}
				_ = root.Close()
				return nil, fmt.Errorf("unrecognized file %q in workspace %q: %w", digest, workspaceID, ErrUnsafePath)
			}
			_, allocatedBytes, err := verifyBlockAllocated(root, digest, nil)
			if err != nil {
				_ = root.Close()
				return nil, fmt.Errorf("verify block %q in workspace %q: %w", digest, workspaceID, err)
			}
			allocations = append(allocations, BlockAllocation{WorkspaceID: workspaceID, Digest: digest, AllocatedBytes: allocatedBytes})
		}
		if err := root.Close(); err != nil {
			return nil, fmt.Errorf("close workspace %q after scan: %w", workspaceID, err)
		}
	}
	sort.Slice(allocations, func(i, j int) bool {
		if allocations[i].WorkspaceID != allocations[j].WorkspaceID {
			return allocations[i].WorkspaceID < allocations[j].WorkspaceID
		}
		return allocations[i].Digest < allocations[j].Digest
	})
	return allocations, nil
}

// Open returns a reader for a verified workspace-scoped block path. Block
// bytes are verified when stored; this method does not reread the whole block.
func (s *Store) Open(workspaceID, digest string) (io.ReadCloser, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, ErrInvalidWorkspaceID
	}
	if !validDigest(digest) {
		return nil, ErrInvalidDigest
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return nil, ErrClosed
	}
	workspaceRoot, err := s.openWorkspace(workspaceID, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = workspaceRoot.Close() }()
	info, err := workspaceRoot.Lstat(digest)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace block: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxBlockBytes {
		return nil, ErrUnsafePath
	}
	file, err := workspaceRoot.Open(digest)
	if err != nil {
		return nil, fmt.Errorf("open workspace block: %w", err)
	}
	return file, nil
}

// VerifyBlock checks one database-referenced block without changing storage.
func (s *Store) VerifyBlock(workspaceID, digest string, expectedSize int64) error {
	if !domain.ValidWorkspaceID(workspaceID) {
		return ErrInvalidWorkspaceID
	}
	if !validDigest(digest) {
		return ErrInvalidDigest
	}
	if expectedSize < 0 || expectedSize > MaxBlockBytes {
		return ErrCorruptBlock
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return ErrClosed
	}
	workspaceRoot, err := s.openWorkspace(workspaceID, false)
	if err != nil {
		return err
	}
	_, err = verifyBlock(workspaceRoot, digest, &expectedSize)
	if closeErr := workspaceRoot.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("close workspace %q after verification: %w", workspaceID, closeErr)
	}
	return err
}

// ListBlockNames returns safe digest file names without reading their contents.
// Recognized upload temp files are ignored; unrelated residue fails closed.
func (s *Store) ListBlockNames(workspaceID string) (names []string, retErr error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, ErrInvalidWorkspaceID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return nil, ErrClosed
	}
	root, err := s.openWorkspace(workspaceID, false)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := root.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close workspace block directory: %w", err)
		}
	}()
	return listBlockNames(root, workspaceID)
}

// ListBlockDigests returns verified block names for legacy imports, which need
// content integrity checks before reusing blocks.
func (s *Store) ListBlockDigests(workspaceID string) (digests []string, retErr error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, ErrInvalidWorkspaceID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return nil, ErrClosed
	}
	root, err := s.openWorkspace(workspaceID, false)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := root.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close workspace block directory: %w", err)
		}
	}()
	digests, err = listBlockNames(root, workspaceID)
	if err != nil {
		return nil, err
	}
	for _, digest := range digests {
		if _, err := verifyBlock(root, digest, nil); err != nil {
			return nil, fmt.Errorf("verify workspace block %q: %w", digest, err)
		}
	}
	return digests, nil
}

// RemoveStaleTempBlocks removes only recognized regular upload temp files.
// Callers must hold the workspace block-store lock while invoking it.
func (s *Store) RemoveStaleTempBlocks(workspaceID string) (retErr error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return ErrInvalidWorkspaceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return ErrClosed
	}
	root, err := s.openWorkspace(workspaceID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close workspace block directory after import recovery: %w", err)
		}
	}()

	entries, err := readEntries(root)
	if err != nil {
		return fmt.Errorf("list workspace blocks before import recovery: %w", err)
	}
	temps := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if validDigest(name) {
			continue
		}
		if !validTempBlockName(name) {
			return fmt.Errorf("unrecognized file %q in workspace %q: %w", name, workspaceID, ErrUnsafePath)
		}
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect temporary workspace block: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		temps = append(temps, name)
	}
	for _, name := range temps {
		if err := root.Remove(name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("remove abandoned workspace block: %w", err)
		}
	}
	if len(temps) > 0 {
		if err := syncDir(root); err != nil {
			return fmt.Errorf("sync workspace after import recovery: %w", err)
		}
	}
	return nil
}

// VerifyAll checks every entry in the store and fails on any unknown residue.
func (s *Store) VerifyAll() (VerifyStats, error) {
	var stats VerifyStats

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return stats, ErrClosed
	}
	rootEntries, err := readEntries(s.root)
	if err != nil {
		return stats, fmt.Errorf("read workspace storage root: %w", err)
	}
	for _, entry := range rootEntries {
		workspaceID := entry.Name()
		if !domain.ValidWorkspaceID(workspaceID) {
			return stats, fmt.Errorf("unrecognized workspace storage entry %q: %w", workspaceID, ErrUnsafePath)
		}
		info, err := s.root.Lstat(workspaceID)
		if err != nil {
			return stats, fmt.Errorf("inspect workspace storage entry %q: %w", workspaceID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return stats, fmt.Errorf("workspace storage entry %q: %w", workspaceID, ErrUnsafePath)
		}
		workspaceRoot, err := s.openWorkspace(workspaceID, false)
		if err != nil {
			return stats, err
		}
		openedInfo, err := workspaceRoot.Stat(".")
		if err != nil {
			_ = workspaceRoot.Close()
			return stats, fmt.Errorf("stat workspace storage entry %q: %w", workspaceID, err)
		}
		if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = workspaceRoot.Close()
			return stats, fmt.Errorf("workspace storage entry %q changed during verification: %w", workspaceID, ErrUnsafePath)
		}
		blocks, err := readEntries(workspaceRoot)
		if err != nil {
			_ = workspaceRoot.Close()
			return stats, fmt.Errorf("read workspace %q: %w", workspaceID, err)
		}
		for _, block := range blocks {
			digest := block.Name()
			if !validDigest(digest) {
				_ = workspaceRoot.Close()
				return stats, fmt.Errorf("unrecognized file %q in workspace %q: %w", digest, workspaceID, ErrUnsafePath)
			}
			logicalBytes, allocatedBytes, err := verifyBlockAllocated(workspaceRoot, digest, nil)
			if err != nil {
				_ = workspaceRoot.Close()
				return stats, fmt.Errorf("verify block %q in workspace %q: %w", digest, workspaceID, err)
			}
			if !addVerifyStats(&stats, logicalBytes, allocatedBytes) {
				_ = workspaceRoot.Close()
				return stats, errors.New("workspace verification totals overflow int64")
			}
		}
		if err := workspaceRoot.Close(); err != nil {
			return stats, fmt.Errorf("close workspace %q after verification: %w", workspaceID, err)
		}
	}
	return stats, nil
}

// PutBlock streams a block into a temporary file, validates its digest, and
// publishes it with an atomic no-replace hard link. Replaying an identical
// block succeeds without replacing the stored file.
func (s *Store) PutBlock(workspaceID, digest string, src io.Reader) (int64, error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return 0, ErrInvalidWorkspaceID
	}
	if !validDigest(digest) {
		return 0, ErrInvalidDigest
	}
	if src == nil {
		return 0, ErrInvalidSource
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return 0, ErrClosed
	}
	workspaceRoot, err := s.openWorkspace(workspaceID, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = workspaceRoot.Close() }()

	tempName, file, err := createTemp(workspaceRoot)
	if err != nil {
		return 0, err
	}
	// shortcut: a crash can leave unreferenced temp files; reclaim them only in a reference-aware maintenance path.
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = workspaceRoot.Remove(tempName)
		}
	}()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(src, MaxBlockBytes+1))
	if err != nil {
		return 0, fmt.Errorf("write temporary workspace block: %w", err)
	}
	if n > MaxBlockBytes {
		return 0, ErrBlockTooLarge
	}
	if hex.EncodeToString(hasher.Sum(nil)) != digest {
		return 0, ErrDigestMismatch
	}
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("sync temporary workspace block: %w", err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close temporary workspace block: %w", err)
	}

	if err := workspaceRoot.Link(tempName, digest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return 0, fmt.Errorf("publish workspace block: %w", err)
		}
		existingSize, verifyErr := verifyExisting(workspaceRoot, digest)
		if verifyErr != nil {
			return 0, verifyErr
		}
		if err := syncDir(workspaceRoot); err != nil {
			return 0, fmt.Errorf("sync existing workspace block directory: %w", err)
		}
		return existingSize, nil
	}
	if err := syncDir(workspaceRoot); err != nil {
		return 0, fmt.Errorf("sync workspace block directory: %w", err)
	}
	removeTemp = true
	return n, nil
}

// DeleteWorkspace removes only the validated workspace directory. It does not
// collect blocks from other workspaces or infer whether content is referenced.
func (s *Store) DeleteWorkspace(workspaceID string) error {
	if !domain.ValidWorkspaceID(workspaceID) {
		return ErrInvalidWorkspaceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return ErrClosed
	}
	workspaceRoot, err := s.openWorkspace(workspaceID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := readEntries(workspaceRoot)
	if err != nil {
		_ = workspaceRoot.Close()
		return fmt.Errorf("list workspace blocks before deletion: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validDigest(name) && !validTempBlockName(name) {
			_ = workspaceRoot.Close()
			return fmt.Errorf("unrecognized workspace entry %q: %w", name, ErrUnsafePath)
		}
		info, err := workspaceRoot.Lstat(name)
		if err != nil {
			_ = workspaceRoot.Close()
			return fmt.Errorf("inspect workspace entry %q before deletion: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			_ = workspaceRoot.Close()
			return ErrUnsafePath
		}
		if err := workspaceRoot.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = workspaceRoot.Close()
			return fmt.Errorf("delete workspace entry %q: %w", name, err)
		}
	}
	if err := syncDir(workspaceRoot); err != nil {
		_ = workspaceRoot.Close()
		return fmt.Errorf("sync workspace after deleting its blocks: %w", err)
	}
	if err := workspaceRoot.Close(); err != nil {
		return fmt.Errorf("close workspace after deleting its blocks: %w", err)
	}
	if err := s.root.Remove(workspaceID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete workspace storage directory: %w", err)
	}
	if err := syncDir(s.root); err != nil {
		return fmt.Errorf("sync workspace storage root: %w", err)
	}
	return nil
}

// DeleteBlock removes one validated digest path from a workspace. Missing
// workspaces and blocks are treated as already collected.
func (s *Store) DeleteBlock(workspaceID, digest string) (removedBytes int64, retErr error) {
	if !domain.ValidWorkspaceID(workspaceID) {
		return 0, ErrInvalidWorkspaceID
	}
	if !validDigest(digest) {
		return 0, ErrInvalidDigest
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return 0, ErrClosed
	}
	root, err := s.openWorkspace(workspaceID, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := root.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close workspace block directory after collection: %w", err)
		}
	}()

	info, err := root.Lstat(digest)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect workspace block before collection: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, ErrUnsafePath
	}
	file, err := root.Open(digest)
	if err != nil {
		return 0, fmt.Errorf("open workspace block before collection: %w", err)
	}
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return 0, fmt.Errorf("stat opened workspace block before collection: %w", statErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close workspace block before collection: %w", closeErr)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return 0, ErrUnsafePath
	}
	if openedInfo.Size() != info.Size() || openedInfo.Size() > MaxBlockBytes {
		return 0, ErrCorruptBlock
	}
	allocatedBytes, err := allocatedDataBytes(openedInfo)
	if err != nil {
		return 0, err
	}
	if err := root.Remove(digest); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("remove workspace block: %w", err)
	}
	if err := syncDir(root); err != nil {
		return 0, fmt.Errorf("sync workspace after block collection: %w", err)
	}
	return allocatedBytes, nil
}

// Close releases the descriptor held for the persistent root.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil
	}
	s.dead = true
	if err := s.root.Close(); err != nil {
		return fmt.Errorf("close workspace storage root: %w", err)
	}
	return nil
}

func (s *Store) openWorkspace(workspaceID string, create bool) (*os.Root, error) {
	info, err := s.root.Lstat(workspaceID)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := s.root.Mkdir(workspaceID, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create workspace storage directory: %w", err)
		}
		if err := syncDir(s.root); err != nil {
			return nil, fmt.Errorf("sync workspace storage root: %w", err)
		}
		info, err = s.root.Lstat(workspaceID)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect workspace storage directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrUnsafePath
	}
	root, err := s.root.OpenRoot(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("open workspace storage directory: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stat opened workspace storage directory: %w", err)
	}
	if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, ErrUnsafePath
	}
	return root, nil
}

func listBlockNames(root *os.Root, workspaceID string) ([]string, error) {
	entries, err := readEntries(root)
	if err != nil {
		return nil, fmt.Errorf("list workspace blocks: %w", err)
	}
	digests := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if validDigest(name) {
			info, err := root.Lstat(name)
			if err != nil {
				return nil, fmt.Errorf("inspect workspace block: %w", err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, ErrUnsafePath
			}
			if info.Size() > MaxBlockBytes {
				return nil, ErrCorruptBlock
			}
			digests = append(digests, name)
			continue
		}
		if validTempBlockName(name) {
			info, err := root.Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect temporary workspace block: %w", err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, ErrUnsafePath
			}
			continue
		}
		return nil, fmt.Errorf("unrecognized file %q in workspace %q: %w", name, workspaceID, ErrUnsafePath)
	}
	sort.Strings(digests)
	return digests, nil
}

func createTemp(root *os.Root) (string, *os.File, error) {
	for range 8 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, fmt.Errorf("generate workspace block temp name: %w", err)
		}
		name := ".tmp-" + hex.EncodeToString(random[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create temporary workspace block: %w", err)
		}
		return name, file, nil
	}
	return "", nil, errors.New("could not allocate temporary workspace block name")
}

func validTempBlockName(name string) bool {
	if len(name) != len(".tmp-")+32 || name[:len(".tmp-")] != ".tmp-" {
		return false
	}
	random, err := hex.DecodeString(name[len(".tmp-"):])
	return err == nil && len(random) == 16 && hex.EncodeToString(random) == name[len(".tmp-"):]
}

func verifyExisting(root *os.Root, digest string) (int64, error) {
	return verifyBlock(root, digest, nil)
}

func verifyBlock(root *os.Root, digest string, expectedSize *int64) (int64, error) {
	size, _, err := verifyBlockAllocated(root, digest, expectedSize)
	return size, err
}

func verifyBlockAllocated(root *os.Root, digest string, expectedSize *int64) (size, allocatedBytes int64, retErr error) {
	info, err := root.Lstat(digest)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect workspace block: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxBlockBytes {
		return 0, 0, ErrCorruptBlock
	}
	if expectedSize != nil && info.Size() != *expectedSize {
		return 0, 0, ErrCorruptBlock
	}
	file, err := root.Open(digest)
	if err != nil {
		return 0, 0, fmt.Errorf("open workspace block: %w", err)
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			size = 0
			allocatedBytes = 0
			retErr = fmt.Errorf("close workspace block: %w", err)
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("stat opened workspace block: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() || !openedInfo.ModTime().Equal(info.ModTime()) {
		return 0, 0, ErrCorruptBlock
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(file, MaxBlockBytes+1))
	if err != nil {
		return 0, 0, fmt.Errorf("verify workspace block: %w", err)
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("restat workspace block: %w", err)
	}
	if n > MaxBlockBytes || n != openedInfo.Size() || (expectedSize != nil && n != *expectedSize) || !os.SameFile(openedInfo, afterInfo) || afterInfo.Size() != openedInfo.Size() || !afterInfo.ModTime().Equal(openedInfo.ModTime()) || hex.EncodeToString(hasher.Sum(nil)) != digest {
		return 0, 0, ErrCorruptBlock
	}
	allocatedBytes, err = allocatedDataBytes(afterInfo)
	if err != nil {
		return 0, 0, err
	}
	return n, allocatedBytes, nil
}

func allocatedDataBytes(info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 || stat.Blocks > maxVerifyTotal/512 {
		return 0, errors.New("workspace block allocated-byte stat is unavailable or invalid")
	}
	return stat.Blocks * 512, nil
}

func readEntries(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}

func addVerifyStats(stats *VerifyStats, logicalBytes, allocatedBytes int64) bool {
	if logicalBytes < 0 || allocatedBytes < 0 || stats.BlockCount == maxVerifyTotal ||
		logicalBytes > maxVerifyTotal-stats.LogicalBytes || allocatedBytes > maxVerifyTotal-stats.AllocatedBytes {
		return false
	}
	stats.BlockCount++
	stats.LogicalBytes += logicalBytes
	stats.AllocatedBytes += allocatedBytes
	return true
}

func syncDir(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func syncPathDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}
