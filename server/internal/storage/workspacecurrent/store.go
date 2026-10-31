package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	mordesync "morde-sync"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

var (
	ErrInvalidRoot        = errors.New("workspace current root must be an absolute non-root directory")
	ErrUnsafePath         = errors.New("workspace current path is not a safe regular file or directory")
	ErrUntrustedDirectory = errors.New("workspace current directory has no valid revision marker")
	ErrClosed             = errors.New("workspace current store is closed")
)

const defaultWorkspaceSyncNodeUID = 65532
const revisionMarkerDirectory = ".morde-revisions"

type BlockReader interface {
	Open(workspaceID, digest string) (io.ReadCloser, error)
}

// Store publishes verified Workspace revisions as ordinary directories.
// Staging and locks live outside owner bind mounts, but on the same filesystem.
type Store struct {
	rootPath string
	ownerUID int
	ownerGID int
	dead     atomic.Bool
}

func Open(rootPath string) (*Store, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) == string(filepath.Separator) {
		return nil, ErrInvalidRoot
	}
	rootPath = filepath.Clean(rootPath)
	if err := validateNoSymlinkAncestors(rootPath); err != nil {
		return nil, fmt.Errorf("workspace current root path: %w", err)
	}
	if info, err := os.Lstat(rootPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(rootPath, 0750); err != nil {
			return nil, fmt.Errorf("create workspace current root: %w", err)
		}
		if err := syncDir(filepath.Dir(rootPath)); err != nil {
			return nil, fmt.Errorf("sync workspace current root parent: %w", err)
		}
	} else {
		return nil, fmt.Errorf("inspect workspace current root: %w", err)
	}
	if err := validateNoSymlinkAncestors(rootPath); err != nil {
		return nil, fmt.Errorf("workspace current root path: %w", err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, ErrUnsafePath
	}
	for _, name := range []string{".morde-staging", ".morde-locks", revisionMarkerDirectory} {
		path := filepath.Join(rootPath, name)
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create workspace current %s: %w", name, err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("workspace current %s: %w", name, ErrUnsafePath)
		}
	}
	if err := syncDir(rootPath); err != nil {
		return nil, fmt.Errorf("sync workspace current root: %w", err)
	}
	ownerUID, ownerGID := os.Geteuid(), os.Getegid()
	if ownerUID == 0 {
		// Keep root-run Portal materializations writable by the non-root sync container.
		ownerUID, ownerGID = defaultWorkspaceSyncNodeUID, defaultWorkspaceSyncNodeUID
	}
	return &Store{rootPath: rootPath, ownerUID: ownerUID, ownerGID: ownerGID}, nil
}

func (s *Store) Close() error {
	if s != nil {
		s.dead.Store(true)
	}
	return nil
}

// Delete removes the published Workspace tree and its revision marker under
// the same lock used for materialization.
func (s *Store) Delete(ctx context.Context, ownerID, workspaceID string) (retErr error) {
	if s == nil || s.dead.Load() {
		return ErrClosed
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return errors.New("invalid workspace current deletion request")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	lock, err := acquireLock(ctx, filepath.Join(s.rootPath, ".morde-locks", ownerHash+"-"+workspaceID+".lock"))
	if err != nil {
		return fmt.Errorf("lock workspace current directory for deletion: %w", err)
	}
	defer func() {
		if err := releaseLock(lock); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace current deletion lock: %w", err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	ownerRoot := filepath.Join(s.rootPath, ownerHash)
	var target string
	if info, err := os.Lstat(ownerRoot); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
		target = filepath.Join(ownerRoot, workspaceID)
		if targetInfo, err := os.Lstat(target); err == nil {
			if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.IsDir() {
				return ErrUnsafePath
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect workspace current directory before deletion: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workspace current owner root before deletion: %w", err)
	}
	markerRoot := filepath.Join(s.rootPath, revisionMarkerDirectory)
	marker := filepath.Join(markerRoot, ownerHash+"-"+workspaceID+".revision")
	if info, err := os.Lstat(marker); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workspace current revision marker before deletion: %w", err)
	}

	if err := removeStaleStages(filepath.Join(s.rootPath, ".morde-staging"), ownerHash+"-"+workspaceID+"-"); err != nil {
		return fmt.Errorf("remove staged workspace current trees: %w", err)
	}
	if target != "" {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove workspace current directory: %w", err)
		}
		if err := syncDir(ownerRoot); err != nil {
			return fmt.Errorf("sync workspace current directory removal: %w", err)
		}
	}
	if _, err := os.Lstat(marker); err == nil {
		if err := os.Remove(marker); err != nil {
			return fmt.Errorf("remove workspace current revision marker: %w", err)
		}
		if err := syncDir(markerRoot); err != nil {
			return fmt.Errorf("sync workspace current revision marker removal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workspace current revision marker during deletion: %w", err)
	}
	return nil
}

// Materialize reconstructs a complete revision off-path, verifies every block
// and file digest, then atomically exchanges the directory on Linux.
func (s *Store) Materialize(ctx context.Context, ownerID, workspaceID string, revision int64, files []workspace.File, emptyDirectories []string, blocks BlockReader) (retErr error) {
	if s == nil || s.dead.Load() {
		return ErrClosed
	}
	if ctx == nil || blocks == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || revision < 0 {
		return errors.New("invalid workspace current materialization request")
	}
	if err := validateTree(files, emptyDirectories); err != nil {
		return err
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	ownerRoot := filepath.Join(s.rootPath, ownerHash)
	if err := ensureDirectory(ownerRoot, 0750); err != nil {
		return fmt.Errorf("prepare workspace current owner root: %w", err)
	}
	if err := syncDir(s.rootPath); err != nil {
		return fmt.Errorf("sync workspace current owner root: %w", err)
	}
	lock, err := acquireLock(ctx, filepath.Join(s.rootPath, ".morde-locks", ownerHash+"-"+workspaceID+".lock"))
	if err != nil {
		return fmt.Errorf("lock workspace current directory: %w", err)
	}
	defer func() {
		if err := releaseLock(lock); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace current lock: %w", err))
		}
	}()
	tempRoot := filepath.Join(s.rootPath, ".morde-staging")
	if err := removeStaleStages(tempRoot, ownerHash+"-"+workspaceID+"-"); err != nil {
		return fmt.Errorf("recover Workspace current staging: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(tempRoot, ownerHash+"-"+workspaceID+"-")
	if err != nil {
		return fmt.Errorf("create workspace current staging tree: %w", err)
	}
	defer func() {
		if stage != "" {
			if err := os.RemoveAll(stage); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("remove workspace current staging tree: %w", err))
			}
		}
	}()

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := materializeFile(ctx, stage, workspaceID, file, blocks); err != nil {
			return fmt.Errorf("materialize workspace file %q: %w", file.Path, err)
		}
	}
	for _, dir := range emptyDirectories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(stage, filepath.FromSlash(dir)), 0750); err != nil {
			return fmt.Errorf("materialize workspace directory %q: %w", dir, err)
		}
	}
	if err := syncTree(stage, s.ownerUID, s.ownerGID); err != nil {
		return fmt.Errorf("sync workspace current staging tree: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	target := filepath.Join(ownerRoot, workspaceID)
	initialPublish := false
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
		if _, exists, err := s.CurrentRevision(ownerID, workspaceID); err != nil {
			return err
		} else if !exists {
			if revision == 0 && len(files) == 0 && len(emptyDirectories) == 0 {
				entries, err := os.ReadDir(target)
				if err != nil {
					return fmt.Errorf("inspect unmarked initial workspace directory: %w", err)
				}
				if len(entries) == 0 {
					if err := syncDir(ownerRoot); err != nil {
						return fmt.Errorf("sync recovered initial workspace directory: %w", err)
					}
					if err := s.writeRevisionMarker(ownerHash, workspaceID, revision); err != nil {
						return fmt.Errorf("record recovered initial Workspace revision: %w", err)
					}
					return nil
				}
			}
			// Without a valid marker, replacing this tree could erase data outside the revision ledger.
			return ErrUntrustedDirectory
		}
		if err := renameExchange(stage, target); err != nil {
			return fmt.Errorf("atomically exchange workspace current directory: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := s.writeRevisionMarker(ownerHash, workspaceID, revision); err != nil {
			return fmt.Errorf("record initial Workspace revision before publish: %w", err)
		}
		initialPublish = true
		if err := os.Rename(stage, target); err != nil {
			return fmt.Errorf("publish initial workspace current directory: %w", err)
		}
	} else {
		return fmt.Errorf("inspect workspace current directory: %w", err)
	}
	if err := syncDir(ownerRoot); err != nil {
		return fmt.Errorf("sync published workspace current directory: %w", err)
	}
	if err := syncDir(tempRoot); err != nil {
		return fmt.Errorf("sync workspace current staging parent: %w", err)
	}
	if !initialPublish {
		if err := s.writeRevisionMarker(ownerHash, workspaceID, revision); err != nil {
			return fmt.Errorf("record published Workspace revision: %w", err)
		}
	}
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove previous workspace current directory: %w", err)
	}
	stage = ""
	if err := syncDir(tempRoot); err != nil {
		return fmt.Errorf("sync workspace current staging cleanup: %w", err)
	}
	return nil
}

// CurrentRevision reports the revision marker only when its Workspace directory exists.
func (s *Store) CurrentRevision(ownerID, workspaceID string) (int64, bool, error) {
	if s == nil || s.dead.Load() {
		return 0, false, ErrClosed
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return 0, false, errors.New("invalid Workspace current revision lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	ownerRoot := filepath.Join(s.rootPath, ownerHash)
	if info, err := os.Lstat(ownerRoot); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("inspect Workspace current owner root: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, false, ErrUnsafePath
	}
	target := filepath.Join(s.rootPath, ownerHash, workspaceID)
	if info, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("inspect Workspace current directory: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, false, ErrUnsafePath
	}
	marker := filepath.Join(s.rootPath, revisionMarkerDirectory, ownerHash+"-"+workspaceID+".revision")
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("inspect Workspace current revision marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return 0, false, ErrUnsafePath
	}
	file, err := os.Open(marker)
	if err != nil {
		return 0, false, fmt.Errorf("open Workspace current revision marker: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 21))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return 0, false, fmt.Errorf("read Workspace current revision marker: %w", errors.Join(readErr, closeErr))
	}
	if len(data) > 20 {
		return 0, false, nil
	}
	value := strings.TrimSuffix(string(data), "\n")
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 || strconv.FormatInt(revision, 10)+"\n" != string(data) {
		return 0, false, nil
	}
	return revision, true, nil
}

// MatchesManifest checks the published tree against an immutable Workspace manifest.
func (s *Store) MatchesManifest(ctx context.Context, ownerID, workspaceID string, files []workspace.File, emptyDirectories []string) (bool, error) {
	if s == nil || s.dead.Load() {
		return false, ErrClosed
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return false, errors.New("invalid Workspace current manifest lookup")
	}
	if err := validateTree(files, emptyDirectories); err != nil {
		return false, err
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	ownerRoot := filepath.Join(s.rootPath, ownerHash)
	if info, err := os.Lstat(ownerRoot); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect Workspace current owner root: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, ErrUnsafePath
	}
	target := filepath.Join(ownerRoot, workspaceID)
	if info, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect Workspace current directory: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, ErrUnsafePath
	}
	actual, err := mordesync.Scan(ctx, target, nil)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// A scan failure makes this published projection unsuitable for the immutable revision.
		return false, nil
	}
	if len(actual.Files) != len(files) || len(actual.EmptyDirectories) != len(emptyDirectories) {
		return false, nil
	}
	wantFiles := append([]workspace.File(nil), files...)
	sort.Slice(wantFiles, func(i, j int) bool { return wantFiles[i].Path < wantFiles[j].Path })
	for i, want := range wantFiles {
		got := actual.Files[i]
		if got.Path != want.Path || got.Size != want.SizeBytes || got.Mode != want.Mode&0o777 || got.SHA256 != want.SHA256 {
			return false, nil
		}
	}
	wantDirectories := append([]string(nil), emptyDirectories...)
	sort.Strings(wantDirectories)
	for i, want := range wantDirectories {
		if actual.EmptyDirectories[i] != want {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) writeRevisionMarker(ownerHash, workspaceID string, revision int64) error {
	root := filepath.Join(s.rootPath, revisionMarkerDirectory)
	file, err := os.CreateTemp(root, ownerHash+"-"+workspaceID+"-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	data := strconv.FormatInt(revision, 10) + "\n"
	written, err := file.WriteString(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if written != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	marker := filepath.Join(root, ownerHash+"-"+workspaceID+".revision")
	if err := os.Rename(temp, marker); err != nil {
		return err
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return nil
}

func validateNoSymlinkAncestors(path string) error {
	current := string(os.PathSeparator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
	}
	return nil
}

func validateTree(files []workspace.File, emptyDirectories []string) error {
	if len(files) > workspace.MaxSyncFiles || len(emptyDirectories) > workspace.MaxSyncFiles-len(files) {
		return errors.New("workspace current tree exceeds item limits")
	}
	var paths mordesync.ManifestPathValidator
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil {
			return fmt.Errorf("invalid workspace current file: %w", err)
		}
		if err := paths.Add(file.Path); err != nil {
			return fmt.Errorf("invalid workspace current file path: %w", err)
		}
	}
	for _, value := range emptyDirectories {
		if err := paths.AddEmptyDirectory(value); err != nil {
			return fmt.Errorf("invalid workspace current directory: %w", err)
		}
	}
	return nil
}

func materializeFile(ctx context.Context, stage, workspaceID string, file workspace.File, blocks BlockReader) error {
	path := filepath.Join(stage, filepath.FromSlash(file.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	fileHash := sha256.New()
	var written int64
	var writeErr error
	for _, chunk := range file.Chunks {
		if err := ctx.Err(); err != nil {
			writeErr = err
			break
		}
		input, err := blocks.Open(workspaceID, chunk.SHA256)
		if err != nil {
			writeErr = err
			break
		}
		if input == nil {
			writeErr = errors.New("workspace block reader returned no content")
			break
		}
		blockHash := sha256.New()
		count, copyErr := io.CopyN(io.MultiWriter(output, fileHash, blockHash), input, chunk.SizeBytes+1)
		closeErr := input.Close()
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			writeErr = errors.Join(copyErr, closeErr)
			break
		}
		if closeErr != nil {
			writeErr = closeErr
			break
		}
		if count != chunk.SizeBytes || hex.EncodeToString(blockHash.Sum(nil)) != chunk.SHA256 {
			writeErr = errors.New("workspace block size or digest does not match its manifest")
			break
		}
		written += count
	}
	if writeErr == nil && (written != file.SizeBytes || hex.EncodeToString(fileHash.Sum(nil)) != file.SHA256) {
		writeErr = errors.New("workspace file size or digest does not match its manifest")
	}
	if writeErr == nil {
		writeErr = output.Chmod(os.FileMode(file.Mode & 0777))
	}
	if writeErr == nil {
		writeErr = output.Sync()
	}
	closeErr := output.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return syncDir(filepath.Dir(path))
}

func syncTree(root string, ownerUID, ownerGID int) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(path, ownerUID, ownerGID); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				if err := file.Sync(); err != nil {
					_ = file.Close()
					return err
				}
				if err := file.Close(); err != nil {
					return err
				}
			}
		}
		if info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return ErrUnsafePath
	}
	return nil
}

func removeStaleStages(root, prefix string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return syncDir(root)
}

func acquireLock(ctx context.Context, path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, ErrUnsafePath
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
