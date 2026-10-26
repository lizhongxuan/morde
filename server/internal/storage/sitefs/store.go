package sitefs

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"
)

const manifestFile = ".morde-manifest-sha256"

const (
	stagingLeaseLock    = ".staging/.lease.lock"
	stagingCapacityLock = ".staging/.capacity.lock"
)

var (
	ErrInvalidRoot      = errors.New("site storage root must be an absolute non-root directory")
	ErrUnsafePath       = errors.New("site artifact path is not a safe regular file or directory")
	ErrInvalidDigest    = errors.New("site manifest digest must be a lowercase sha-256 hex string")
	ErrReleaseConflict  = errors.New("site release already exists with different content")
	ErrArtifactCorrupt  = errors.New("stored site artifact does not match its manifest")
	ErrReleaseTooLarge  = errors.New("site release exceeds its configured byte limit")
	ErrInsufficientFree = errors.New("site artifact filesystem is below its free-space reserve")
	ErrClosed           = errors.New("site artifact store is closed")
	ErrActiveWriters    = errors.New("site artifact store has active writers")
	ErrOpenFiles        = errors.New("site artifact release has open files")
)

// Store keeps immutable site releases under a dedicated private root. The root
// must be separate from WorkspaceFS and Docker data, writable only by Morde.
type Store struct {
	root            *os.Root
	rootPath        string
	capacityLock    *os.File
	maxReleaseBytes int64
	minFreeBytes    int64
	mu              sync.Mutex
	capacityMu      sync.Mutex
	dead            bool
	writers         int
	reservedBytes   int64
}

// Open creates or opens the dedicated Site artifact root.
func Open(rootPath string, maxReleaseBytes, minFreeBytes int64) (*Store, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) == string(filepath.Separator) || maxReleaseBytes <= 0 || minFreeBytes <= 0 {
		return nil, ErrInvalidRoot
	}
	rootPath = filepath.Clean(rootPath)
	if info, err := os.Lstat(rootPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(rootPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create site storage root: %w", err)
		}
		if err := syncPathDir(filepath.Dir(rootPath)); err != nil {
			return nil, fmt.Errorf("sync site storage parent: %w", err)
		}
	} else {
		return nil, fmt.Errorf("inspect site storage root: %w", err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700 {
		return nil, ErrUnsafePath
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open site storage root: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || !opened.IsDir() || opened.Mode().Perm()&0077 != 0 || opened.Mode().Perm()&0700 != 0700 {
		_ = root.Close()
		return nil, ErrUnsafePath
	}
	store := &Store{root: root, rootPath: rootPath, maxReleaseBytes: maxReleaseBytes, minFreeBytes: minFreeBytes}
	entries, err := readEntries(root)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("read site storage root: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".staging" && entry.Name() != "releases" {
			_ = root.Close()
			return nil, fmt.Errorf("unrecognized site storage root entry %q: %w", entry.Name(), ErrUnsafePath)
		}
	}
	for _, name := range []string{".staging", "releases"} {
		dir, err := ensureDirectory(root, name, true)
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("initialize site storage %s: %w", name, err)
		}
		if err := dir.Close(); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("close site storage %s: %w", name, err)
		}
	}
	leaseLock, err := openLockFile(root, stagingLeaseLock)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open site staging lease: %w", err)
	}
	capacityLock, err := openLockFile(root, stagingCapacityLock)
	if err != nil {
		_ = leaseLock.Close()
		_ = root.Close()
		return nil, fmt.Errorf("open site capacity lease: %w", err)
	}
	if err := recoverStaging(root, leaseLock); err != nil {
		_ = capacityLock.Close()
		_ = leaseLock.Close()
		_ = root.Close()
		return nil, fmt.Errorf("recover interrupted site staging: %w", err)
	}
	if err := leaseLock.Close(); err != nil {
		_ = capacityLock.Close()
		_ = root.Close()
		return nil, fmt.Errorf("close site staging lease: %w", err)
	}
	store.capacityLock = capacityLock
	return store, nil
}

func openLockFile(root *os.Root, path string) (file *os.File, retErr error) {
	parent, err := openDirectory(root, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := parent.Close(); err != nil {
			if file != nil {
				retErr = errors.Join(retErr, file.Close())
				file = nil
			}
			retErr = errors.Join(retErr, err)
		}
	}()
	name := filepath.Base(path)
	info, err := parent.Lstat(name)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return nil, err
	}
	if !created && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		return nil, ErrUnsafePath
	}
	flags := os.O_RDWR
	if created {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err = parent.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 || (!created && !os.SameFile(info, opened)) {
		_ = file.Close()
		return nil, ErrUnsafePath
	}
	if created {
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := syncDir(parent); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func flock(file *os.File, operation int) error {
	for {
		err := syscall.Flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func closeLockFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(flock(file, syscall.LOCK_UN), file.Close())
}

func recoverStaging(root *os.Root, leaseLock *os.File) (retErr error) {
	if err := flock(leaseLock, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire recovery lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, flock(leaseLock, syscall.LOCK_UN)) }()
	staging, err := openDirectory(root, ".staging")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, staging.Close()) }()
	entries, err := readEntries(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == filepath.Base(stagingLeaseLock) || name == filepath.Base(stagingCapacityLock) {
			info, err := staging.Lstat(name)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return ErrUnsafePath
			}
			continue
		}
		if !sitedomain.ValidID(sitedomain.ID(name)) || !entry.IsDir() {
			return fmt.Errorf("unrecognized site staging entry %q: %w", name, ErrUnsafePath)
		}
		siteStaging, err := ensureDirectory(staging, name, false)
		if err != nil {
			return err
		}
		stages, err := readEntries(siteStaging)
		if err != nil {
			_ = siteStaging.Close()
			return err
		}
		for _, stage := range stages {
			stageName := stage.Name()
			if !validStageName(stageName) || !stage.IsDir() {
				_ = siteStaging.Close()
				return fmt.Errorf("unrecognized interrupted site stage %q: %w", stageName, ErrUnsafePath)
			}
			info, err := siteStaging.Lstat(stageName)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700 {
				_ = siteStaging.Close()
				return ErrUnsafePath
			}
			stageRoot, err := siteStaging.OpenRoot(stageName)
			if err != nil {
				_ = siteStaging.Close()
				return err
			}
			if err := makeStagingWritable(stageRoot); err != nil {
				_ = stageRoot.Close()
				_ = siteStaging.Close()
				return err
			}
			if err := stageRoot.Close(); err != nil {
				_ = siteStaging.Close()
				return err
			}
			if err := siteStaging.RemoveAll(stageName); err != nil {
				_ = siteStaging.Close()
				return fmt.Errorf("remove interrupted site stage: %w", err)
			}
		}
		if err := syncDir(siteStaging); err != nil {
			_ = siteStaging.Close()
			return err
		}
		if err := siteStaging.Close(); err != nil {
			return err
		}
	}
	return syncDir(staging)
}

func validStageName(name string) bool {
	if !strings.HasPrefix(name, ".tmp-") {
		return false
	}
	value := strings.TrimPrefix(name, ".tmp-")
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == value
}

func makeStagingWritable(root *os.Root) error {
	entries, err := readEntries(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrUnsafePath
		}
		if info.IsDir() {
			if err := root.Chmod(name, 0700); err != nil {
				return err
			}
			child, err := root.OpenRoot(name)
			if err != nil {
				return err
			}
			childErr := makeStagingWritable(child)
			closeErr := child.Close()
			if err := errors.Join(childErr, closeErr); err != nil {
				return err
			}
		} else if err := root.Chmod(name, 0600); err != nil {
			return err
		}
	}
	return root.Chmod(".", 0700)
}

// Begin starts a private staged release. Reusing an existing release ID with
// the same digest verifies streamed file content and succeeds idempotently.
func (s *Store) Begin(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, manifestSHA256 string) (app.SiteArtifactWriter, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) {
		return nil, domain.InvalidArgument("site.release", "site release identity is invalid")
	}
	if !validDigest(manifestSHA256) {
		return nil, ErrInvalidDigest
	}
	leaseLock, err := openLockFile(s.root, stagingLeaseLock)
	if err != nil {
		return nil, fmt.Errorf("open site staging lease: %w", err)
	}
	if err := flock(leaseLock, syscall.LOCK_SH); err != nil {
		_ = leaseLock.Close()
		return nil, fmt.Errorf("acquire site staging lease: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil, errors.Join(ErrClosed, closeLockFile(leaseLock))
	}
	releasePath := filepath.Join("releases", string(siteID), string(releaseID))
	if _, err := s.root.Lstat(releasePath); err == nil {
		writer, err := s.openExistingWriter(releasePath, siteID, releaseID, manifestSHA256)
		if err != nil {
			_ = closeLockFile(leaseLock)
			return nil, err
		}
		s.writers++
		writer.leaseLock = leaseLock
		return writer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = closeLockFile(leaseLock)
		return nil, fmt.Errorf("inspect existing site release: %w", err)
	}
	if err := s.ensureSiteDirectories(siteID); err != nil {
		_ = closeLockFile(leaseLock)
		return nil, err
	}
	stageParentPath := filepath.Join(".staging", string(siteID))
	stageName, err := createStage(s.root, stageParentPath)
	if err != nil {
		_ = closeLockFile(leaseLock)
		return nil, err
	}
	stagePath := filepath.Join(stageParentPath, stageName)
	stageRoot, err := s.root.OpenRoot(stagePath)
	if err != nil {
		_ = s.root.RemoveAll(stagePath)
		_ = closeLockFile(leaseLock)
		return nil, fmt.Errorf("open staged site release: %w", err)
	}
	filesRoot, err := ensureDirectory(stageRoot, "files", true)
	if err != nil {
		_ = stageRoot.Close()
		_ = s.root.RemoveAll(stagePath)
		_ = closeLockFile(leaseLock)
		return nil, fmt.Errorf("create staged site files: %w", err)
	}
	s.writers++
	return &stagedWriter{
		store: s, siteID: siteID, releaseID: releaseID, manifestSHA256: manifestSHA256,
		stagePath: stagePath, stageParentPath: stageParentPath, finalPath: releasePath,
		stageRoot: stageRoot, filesRoot: filesRoot, files: make(map[string]uint32), dirs: map[string]struct{}{".": {}}, leaseLock: leaseLock,
	}, nil
}

var _ app.SiteArtifactStore = (*Store)(nil)

// Delete removes one exact immutable release after verifying its identity and
// safely restoring directory permissions for removal. The exclusive lease
// waits for active publishers and readers across Portal instances.
func (s *Store) Delete(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, manifestSHA256 string) (retErr error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) {
		return domain.InvalidArgument("site.release", "site release identity is invalid")
	}
	if !validDigest(manifestSHA256) {
		return ErrInvalidDigest
	}
	leaseLock, err := openLockFile(s.root, stagingLeaseLock)
	if err != nil {
		return fmt.Errorf("open site artifact deletion lease: %w", err)
	}
	if err := flock(leaseLock, syscall.LOCK_EX); err != nil {
		_ = leaseLock.Close()
		return fmt.Errorf("acquire site artifact deletion lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, closeLockFile(leaseLock)) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return ErrClosed
	}

	releasePath := filepath.Join("releases", string(siteID), string(releaseID))
	releaseRoot, err := openDirectory(s.root, releasePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open site release for deletion: %w", err)
	}
	info, err := releaseRoot.Stat(".")
	if err != nil || info.Mode().Perm()&0222 != 0 {
		_ = releaseRoot.Close()
		return ErrArtifactCorrupt
	}
	if err := verifyManifestMarker(releaseRoot, manifestSHA256); err != nil {
		_ = releaseRoot.Close()
		return err
	}
	if err := verifyReleaseEntries(releaseRoot); err != nil {
		_ = releaseRoot.Close()
		return err
	}
	filesRoot, err := openDirectory(releaseRoot, "files")
	if err != nil {
		_ = releaseRoot.Close()
		return err
	}
	filesInfo, statErr := filesRoot.Stat(".")
	if statErr != nil || filesInfo.Mode().Perm()&0222 != 0 {
		_ = filesRoot.Close()
		_ = releaseRoot.Close()
		return ErrArtifactCorrupt
	}
	if err := readFileTree(filesRoot, "", make(map[string]uint32)); err != nil {
		_ = filesRoot.Close()
		_ = releaseRoot.Close()
		return err
	}
	if err := filesRoot.Close(); err != nil {
		_ = releaseRoot.Close()
		return err
	}
	if err := makeStagingWritable(releaseRoot); err != nil {
		_ = releaseRoot.Close()
		return err
	}
	if err := releaseRoot.Close(); err != nil {
		return err
	}
	if err := s.root.RemoveAll(releasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove cancelled site release: %w", err)
	}
	if err := syncPathDirectory(s.root, filepath.Dir(releasePath)); err != nil {
		return fmt.Errorf("sync site release directory after deletion: %w", err)
	}
	return nil
}

// OpenRelease opens an immutable release only when its marker matches the
// expected manifest digest. Callers receive read-only methods only.
func (s *Store) OpenRelease(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, manifestSHA256 string) (*Release, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) {
		return nil, domain.InvalidArgument("site.release", "site release identity is invalid")
	}
	if !validDigest(manifestSHA256) {
		return nil, ErrInvalidDigest
	}
	leaseLock, err := openLockFile(s.root, stagingLeaseLock)
	if err != nil {
		return nil, fmt.Errorf("open site release lease: %w", err)
	}
	if err := flock(leaseLock, syscall.LOCK_SH); err != nil {
		_ = leaseLock.Close()
		return nil, fmt.Errorf("acquire site release lease: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil, errors.Join(ErrClosed, closeLockFile(leaseLock))
	}
	path := filepath.Join("releases", string(siteID), string(releaseID))
	releaseRoot, err := openDirectory(s.root, path)
	if err != nil {
		return nil, errors.Join(err, closeLockFile(leaseLock))
	}
	info, err := releaseRoot.Stat(".")
	if err != nil || info.Mode().Perm()&0222 != 0 {
		_ = releaseRoot.Close()
		return nil, errors.Join(ErrArtifactCorrupt, closeLockFile(leaseLock))
	}
	if err := verifyManifestMarker(releaseRoot, manifestSHA256); err != nil {
		_ = releaseRoot.Close()
		return nil, errors.Join(err, closeLockFile(leaseLock))
	}
	if err := verifyReleaseEntries(releaseRoot); err != nil {
		_ = releaseRoot.Close()
		return nil, errors.Join(err, closeLockFile(leaseLock))
	}
	filesRoot, err := openDirectory(releaseRoot, "files")
	_ = releaseRoot.Close()
	if err != nil {
		return nil, errors.Join(err, closeLockFile(leaseLock))
	}
	filesInfo, err := filesRoot.Stat(".")
	if err != nil || filesInfo.Mode().Perm()&0222 != 0 {
		_ = filesRoot.Close()
		return nil, errors.Join(ErrArtifactCorrupt, closeLockFile(leaseLock))
	}
	return &Release{root: filesRoot, leaseLock: leaseLock}, nil
}

// Release provides read-only access to a verified immutable release tree.
type Release struct {
	root      *os.Root
	leaseLock *os.File
	openFiles int
	mu        sync.Mutex
}

// Open reads a regular file below the immutable release tree.
func (r *Release) Open(path string) (io.ReadCloser, error) {
	if r == nil {
		return nil, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.root == nil {
		return nil, ErrClosed
	}
	if err := workspace.ValidateRelativePath(path); err != nil {
		return nil, ErrUnsafePath
	}
	parent, name, err := openParent(r.root, path, false, nil)
	if err != nil {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
		return nil, errors.Join(ErrUnsafePath, parent.Close())
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0222 != 0 {
		return nil, errors.Join(ErrUnsafePath, file.Close(), parent.Close())
	}
	if err := parent.Close(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	r.openFiles++
	return &releaseFile{file: file, release: r}, nil
}

// Close releases the descriptor for the immutable release tree.
func (r *Release) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.root == nil {
		return nil
	}
	if r.openFiles != 0 {
		return ErrOpenFiles
	}
	err := errors.Join(r.root.Close(), closeLockFile(r.leaseLock))
	r.root = nil
	r.leaseLock = nil
	return err
}

type releaseFile struct {
	file    *os.File
	release *Release
	closed  bool
	mu      sync.Mutex
}

func (f *releaseFile) Read(data []byte) (int, error) {
	return f.file.Read(data)
}

func (f *releaseFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	err := f.file.Close()
	f.release.mu.Lock()
	f.release.openFiles--
	f.release.mu.Unlock()
	return err
}

// Close releases the descriptor held for the Site artifact root. Active
// writers must commit or abort first so they can complete durability work.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil
	}
	if s.writers != 0 {
		return ErrActiveWriters
	}
	s.dead = true
	if err := errors.Join(s.capacityLock.Close(), s.root.Close()); err != nil {
		return fmt.Errorf("close site storage root: %w", err)
	}
	return nil
}

type stagedWriter struct {
	store           *Store
	siteID          sitedomain.ID
	releaseID       sitedomain.ReleaseID
	manifestSHA256  string
	stagePath       string
	stageParentPath string
	finalPath       string
	stageRoot       *os.Root
	filesRoot       *os.Root
	files           map[string]uint32
	dirs            map[string]struct{}
	usedBytes       int64
	openFiles       int
	finished        bool
	mu              sync.Mutex
	leaseLock       *os.File
}

func (w *stagedWriter) CreateFile(path string, mode uint32) (io.WriteCloser, error) {
	if err := workspace.ValidateRelativePath(path); err != nil {
		return nil, ErrUnsafePath
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil, ErrClosed
	}
	key := strings.ToLower(path)
	if _, exists := w.files[key]; exists {
		return nil, ErrUnsafePath
	}
	if err := rejectFileAncestor(w.files, key); err != nil {
		return nil, err
	}
	parent, name, err := openParent(w.filesRoot, path, true, w.dirs)
	if err != nil {
		return nil, err
	}
	file, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	closeErr := parent.Close()
	if err != nil {
		return nil, fmt.Errorf("create staged site file: %w", err)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("close staged site parent: %w", closeErr)
	}
	w.files[key] = mode
	w.openFiles++
	return &stagedFile{writer: w, file: file, path: path, mode: immutableMode(mode)}, nil
}

func (w *stagedWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return ErrClosed
	}
	if w.openFiles != 0 {
		return ErrOpenFiles
	}
	if err := writeManifestMarker(w.stageRoot, w.manifestSHA256); err != nil {
		return err
	}
	if err := makeReadOnly(w.filesRoot, w.dirs); err != nil {
		return err
	}
	if err := w.stageRoot.Chmod("files", 0500); err != nil {
		return fmt.Errorf("protect staged site files: %w", err)
	}
	if err := verifyFileTree(w.filesRoot, w.files); err != nil {
		return err
	}
	if err := syncDir(w.stageRoot); err != nil {
		return fmt.Errorf("sync staged site release: %w", err)
	}
	filesCloseErr := w.filesRoot.Close()
	w.filesRoot = nil
	if filesCloseErr != nil {
		return fmt.Errorf("close staged site files: %w", filesCloseErr)
	}

	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return ErrClosed
	}
	if _, err := s.root.Lstat(w.finalPath); err == nil {
		return ErrReleaseConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect site release destination: %w", err)
	}
	if err := s.root.Rename(w.stagePath, w.finalPath); err != nil {
		return fmt.Errorf("publish immutable site release: %w", err)
	}
	w.finished = true
	var postPublishErr error
	if err := w.stageRoot.Chmod(".", 0500); err != nil {
		postPublishErr = errors.Join(postPublishErr, fmt.Errorf("protect published site release: %w", err))
	}
	if err := syncDir(w.stageRoot); err != nil {
		postPublishErr = errors.Join(postPublishErr, fmt.Errorf("sync published site release: %w", err))
	}
	if err := w.stageRoot.Close(); err != nil {
		postPublishErr = errors.Join(postPublishErr, fmt.Errorf("close published site release: %w", err))
	}
	w.stageRoot = nil
	if err := syncPathDirectory(s.root, filepath.Dir(w.stagePath)); err != nil {
		postPublishErr = errors.Join(postPublishErr, fmt.Errorf("sync staged site directory after publish: %w", err))
	}
	if err := syncPathDirectory(s.root, filepath.Dir(w.finalPath)); err != nil {
		postPublishErr = errors.Join(postPublishErr, fmt.Errorf("sync site release directory: %w", err))
	}
	postPublishErr = errors.Join(postPublishErr, w.finishStoreWriterLocked())
	if postPublishErr != nil {
		return &app.SiteArtifactPublishedError{Err: postPublishErr}
	}
	return nil
}

func (w *stagedWriter) Abort() error {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return nil
	}
	if w.openFiles != 0 {
		w.mu.Unlock()
		return ErrOpenFiles
	}
	if w.filesRoot != nil {
		for dir := range w.dirs {
			_ = w.filesRoot.Chmod(dir, 0700)
		}
		_ = w.stageRoot.Chmod("files", 0700)
		_ = w.filesRoot.Close()
		w.filesRoot = nil
	}
	if w.stageRoot != nil {
		_ = w.stageRoot.Chmod(".", 0700)
		_ = w.stageRoot.Close()
		w.stageRoot = nil
	}
	w.finished = true
	w.mu.Unlock()

	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	var retErr error
	if !s.dead {
		if root, err := s.root.OpenRoot(w.stagePath); err == nil {
			if files, err := root.OpenRoot("files"); err == nil {
				for dir := range w.dirs {
					if err := files.Chmod(dir, 0700); err != nil {
						retErr = errors.Join(retErr, err)
					}
				}
				retErr = errors.Join(retErr, files.Close())
			} else if !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, err)
			}
			if err := root.Chmod("files", 0700); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, err)
			}
			if err := root.Chmod(".", 0700); err != nil {
				retErr = errors.Join(retErr, err)
			}
			retErr = errors.Join(retErr, root.Close())
		} else if !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
		if err := s.root.RemoveAll(w.stagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove staged site release: %w", err))
		} else if err := syncPathDirectory(s.root, w.stageParentPath); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("sync site staging directory after abort: %w", err))
		}
	}
	return errors.Join(retErr, w.finishStoreWriterLocked())
}

func (w *stagedWriter) finishStoreWriterLocked() error {
	if w.store.writers > 0 {
		w.store.writers--
	}
	return closeLockFile(w.leaseLock)
}

type stagedFile struct {
	writer *stagedWriter
	file   *os.File
	path   string
	mode   os.FileMode
	closed bool
	mu     sync.Mutex
}

func (f *stagedFile) Write(data []byte) (written int, retErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	store := f.writer.store
	store.capacityMu.Lock()
	defer store.capacityMu.Unlock()
	if err := flock(store.capacityLock, syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("acquire site capacity lease: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, flock(store.capacityLock, syscall.LOCK_UN))
	}()
	if err := f.writer.reserve(int64(len(data))); err != nil {
		return 0, err
	}
	written, err := f.file.Write(data)
	if written < 0 || written > len(data) {
		f.writer.finishWrite(0, int64(len(data)))
		return 0, io.ErrShortWrite
	}
	f.writer.finishWrite(int64(written), int64(len(data)-written))
	return written, err
}

func (f *stagedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	syncErr := f.file.Sync()
	chmodErr := f.file.Chmod(f.mode)
	modeSyncErr := f.file.Sync()
	closeErr := f.file.Close()
	f.writer.mu.Lock()
	if f.writer.openFiles > 0 {
		f.writer.openFiles--
	}
	f.writer.mu.Unlock()
	if err := errors.Join(syncErr, chmodErr, modeSyncErr, closeErr); err != nil {
		return fmt.Errorf("sync immutable site file %q: %w", f.path, err)
	}
	return nil
}

func (w *stagedWriter) reserve(size int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return ErrClosed
	}
	if size < 0 || size > w.store.maxReleaseBytes-w.usedBytes {
		return ErrReleaseTooLarge
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if w.store.dead {
		return ErrClosed
	}
	freeBytes, err := availableBytes(w.store.rootPath)
	if err != nil {
		return fmt.Errorf("inspect site artifact capacity: %w", err)
	}
	if w.store.reservedBytes < 0 || size > int64(^uint64(0)>>1)-w.store.reservedBytes {
		return ErrInsufficientFree
	}
	reserved := uint64(w.store.reservedBytes)
	minimumFree := uint64(w.store.minFreeBytes)
	if freeBytes < minimumFree || reserved > freeBytes-minimumFree || uint64(size) > freeBytes-minimumFree-reserved {
		return ErrInsufficientFree
	}
	w.usedBytes += size
	w.store.reservedBytes += size
	return nil
}

func (w *stagedWriter) finishWrite(written, unused int64) {
	w.mu.Lock()
	w.usedBytes -= unused
	if w.usedBytes < 0 {
		w.usedBytes = 0
	}
	w.store.mu.Lock()
	w.store.reservedBytes -= written + unused
	if w.store.reservedBytes < 0 {
		w.store.reservedBytes = 0
	}
	w.store.mu.Unlock()
	w.mu.Unlock()
}

type existingWriter struct {
	store          *Store
	siteID         sitedomain.ID
	releaseID      sitedomain.ReleaseID
	manifestSHA256 string
	releaseRoot    *os.Root
	filesRoot      *os.Root
	leaseLock      *os.File
	files          map[string]uint32
	openFiles      int
	finished       bool
	mu             sync.Mutex
}

func (s *Store) openExistingWriter(releasePath string, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, digest string) (*existingWriter, error) {
	releaseRoot, err := openDirectory(s.root, releasePath)
	if err != nil {
		return nil, err
	}
	if err := verifyManifestMarker(releaseRoot, digest); err != nil {
		_ = releaseRoot.Close()
		if errors.Is(err, ErrArtifactCorrupt) {
			return nil, ErrReleaseConflict
		}
		return nil, err
	}
	if err := verifyReleaseEntries(releaseRoot); err != nil {
		_ = releaseRoot.Close()
		return nil, err
	}
	filesRoot, err := openDirectory(releaseRoot, "files")
	if err != nil {
		_ = releaseRoot.Close()
		return nil, err
	}
	return &existingWriter{store: s, siteID: siteID, releaseID: releaseID, manifestSHA256: digest, releaseRoot: releaseRoot, filesRoot: filesRoot, files: make(map[string]uint32)}, nil
}

func (w *existingWriter) CreateFile(path string, mode uint32) (io.WriteCloser, error) {
	if err := workspace.ValidateRelativePath(path); err != nil {
		return nil, ErrUnsafePath
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil, ErrClosed
	}
	key := strings.ToLower(path)
	if _, exists := w.files[key]; exists {
		return nil, ErrUnsafePath
	}
	if err := rejectFileAncestor(w.files, key); err != nil {
		return nil, err
	}
	parent, name, err := openParent(w.filesRoot, path, false, nil)
	if err != nil {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != immutableMode(mode).Perm() {
		_ = parent.Close()
		return nil, ErrArtifactCorrupt
	}
	file, err := parent.Open(name)
	_ = parent.Close()
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != immutableMode(mode).Perm() {
		_ = file.Close()
		return nil, ErrArtifactCorrupt
	}
	w.files[key] = mode
	w.openFiles++
	return &compareFile{file: file, path: path, mode: immutableMode(mode), writer: w}, nil
}

func (w *existingWriter) Commit() error {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return ErrClosed
	}
	if w.openFiles != 0 {
		w.mu.Unlock()
		return ErrOpenFiles
	}
	if err := verifyManifestMarker(w.releaseRoot, w.manifestSHA256); err != nil {
		w.mu.Unlock()
		return err
	}
	err := verifyFileTree(w.filesRoot, w.files)
	if err == nil {
		err = w.filesRoot.Chmod(".", 0500)
	}
	if err == nil {
		err = syncDir(w.filesRoot)
	}
	if err == nil {
		err = w.releaseRoot.Chmod(".", 0500)
	}
	if err == nil {
		err = syncDir(w.releaseRoot)
	}
	_ = w.filesRoot.Close()
	_ = w.releaseRoot.Close()
	w.finished = true
	w.mu.Unlock()
	return errors.Join(err, w.finishStoreWriter())
}

func (w *existingWriter) Abort() error {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return nil
	}
	if w.openFiles != 0 {
		w.mu.Unlock()
		return ErrOpenFiles
	}
	filesErr := w.filesRoot.Close()
	releaseErr := w.releaseRoot.Close()
	w.finished = true
	w.mu.Unlock()
	return errors.Join(filesErr, releaseErr, w.finishStoreWriter())
}

func (w *existingWriter) finishStoreWriter() error {
	w.store.mu.Lock()
	if w.store.writers > 0 {
		w.store.writers--
	}
	w.store.mu.Unlock()
	return closeLockFile(w.leaseLock)
}

type compareFile struct {
	file   *os.File
	path   string
	mode   os.FileMode
	writer *existingWriter
	err    error
	closed bool
	mu     sync.Mutex
}

func (f *compareFile) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.err != nil {
		return 0, f.err
	}
	actual := make([]byte, len(data))
	if _, err := io.ReadFull(f.file, actual); err != nil {
		f.err = ErrArtifactCorrupt
		return 0, f.err
	}
	if !bytes.Equal(actual, data) {
		f.err = ErrArtifactCorrupt
		return 0, f.err
	}
	return len(data), nil
}

func (f *compareFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.err == nil {
		var extra [1]byte
		n, err := f.file.Read(extra[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			f.err = ErrArtifactCorrupt
		}
	}
	closeErr := f.file.Close()
	f.writer.mu.Lock()
	if f.writer.openFiles > 0 {
		f.writer.openFiles--
	}
	f.writer.mu.Unlock()
	return errors.Join(f.err, closeErr)
}

func (s *Store) ensureSiteDirectories(siteID sitedomain.ID) error {
	for _, rootName := range []string{".staging", "releases"} {
		root, err := s.root.OpenRoot(rootName)
		if err != nil {
			return fmt.Errorf("open site storage %s: %w", rootName, err)
		}
		dir, err := ensureDirectory(root, string(siteID), true)
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("create site directory: %w", err)
		}
		if err := dir.Close(); err != nil {
			return fmt.Errorf("close site directory: %w", err)
		}
	}
	return nil
}

func createStage(root *os.Root, parentPath string) (stageName string, retErr error) {
	parent, err := openDirectory(root, parentPath)
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := parent.Close()
		if closeErr != nil && retErr == nil {
			if stageName != "" {
				_ = root.RemoveAll(filepath.Join(parentPath, stageName))
				stageName = ""
			}
			retErr = fmt.Errorf("close site staging parent: %w", closeErr)
		} else {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	for range 8 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate site staging ID: %w", err)
		}
		name := ".tmp-" + hex.EncodeToString(random[:])
		if err := parent.Mkdir(name, 0700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("create site staging directory: %w", err)
		}
		if err := syncDir(parent); err != nil {
			_ = parent.Remove(name)
			return "", fmt.Errorf("sync site staging parent: %w", err)
		}
		return name, nil
	}
	return "", errors.New("could not allocate site staging directory")
}

func ensureDirectory(parent *os.Root, name string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create directory: %w", err)
		}
		if err := syncDir(parent); err != nil {
			return nil, fmt.Errorf("sync directory parent: %w", err)
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrUnsafePath
	}
	if create && info.Mode().Perm()&0700 != 0700 {
		return nil, ErrUnsafePath
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open directory: %w", err)
	}
	opened, err := child.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		_ = child.Close()
		return nil, ErrUnsafePath
	}
	return child, nil
}

func openDirectory(parent *os.Root, path string) (*os.Root, error) {
	current, err := parent.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == "." || component == "" {
			continue
		}
		next, err := ensureDirectory(current, component, false)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openParent(root *os.Root, path string, create bool, dirs map[string]struct{}) (*os.Root, string, error) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	currentName := ""
	for _, part := range parts[:len(parts)-1] {
		next, err := ensureDirectory(current, part, create)
		_ = current.Close()
		if err != nil {
			return nil, "", err
		}
		if !create {
			info, err := next.Stat(".")
			if err != nil || info.Mode().Perm()&0222 != 0 {
				_ = next.Close()
				return nil, "", ErrArtifactCorrupt
			}
		}
		if currentName == "" {
			currentName = part
		} else {
			currentName = filepath.ToSlash(filepath.Join(currentName, part))
		}
		if dirs != nil {
			dirs[currentName] = struct{}{}
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func rejectFileAncestor(files map[string]uint32, path string) error {
	for parent := path; ; {
		separator := strings.LastIndexByte(parent, '/')
		if separator < 0 {
			return nil
		}
		parent = parent[:separator]
		if _, exists := files[parent]; exists {
			return ErrUnsafePath
		}
	}
}

func verifyManifestMarker(root *os.Root, expected string) error {
	info, err := root.Lstat(manifestFile)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != int64(len(expected)+1) || info.Mode().Perm()&0222 != 0 {
		return ErrArtifactCorrupt
	}
	marker, err := root.ReadFile(manifestFile)
	if err != nil || string(marker) != expected+"\n" {
		return ErrArtifactCorrupt
	}
	return nil
}

func verifyReleaseEntries(root *os.Root) error {
	entries, err := readEntries(root)
	if err != nil {
		return fmt.Errorf("read immutable site release: %w", err)
	}
	if len(entries) != 2 {
		return ErrArtifactCorrupt
	}
	for _, entry := range entries {
		if entry.Name() != "files" && entry.Name() != manifestFile {
			return ErrArtifactCorrupt
		}
	}
	return nil
}

func writeManifestMarker(root *os.Root, digest string) error {
	file, err := root.OpenFile(manifestFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return fmt.Errorf("create site release manifest marker: %w", err)
	}
	_, writeErr := io.WriteString(file, digest+"\n")
	syncErr := file.Sync()
	chmodErr := file.Chmod(0400)
	modeSyncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, chmodErr, modeSyncErr, closeErr); err != nil {
		return fmt.Errorf("write site release manifest marker: %w", err)
	}
	return nil
}

func verifyFileTree(root *os.Root, expected map[string]uint32) error {
	actual := make(map[string]uint32, len(expected))
	if err := readFileTree(root, "", actual); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return ErrArtifactCorrupt
	}
	for path, mode := range expected {
		got, exists := actual[path]
		if !exists || got != uint32(immutableMode(mode).Perm()) {
			return ErrArtifactCorrupt
		}
	}
	return nil
}

func readFileTree(root *os.Root, prefix string, files map[string]uint32) error {
	entries, err := readEntries(root)
	if err != nil {
		return fmt.Errorf("read site artifact directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		if info.IsDir() {
			if info.Mode().Perm()&0222 != 0 {
				return ErrArtifactCorrupt
			}
			child, err := ensureDirectory(root, name, false)
			if err != nil {
				return err
			}
			err = readFileTree(child, path, files)
			closeErr := child.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
			return ErrUnsafePath
		}
		key := strings.ToLower(path)
		if _, exists := files[key]; exists {
			return ErrArtifactCorrupt
		}
		files[key] = uint32(info.Mode().Perm())
	}
	return nil
}

func makeReadOnly(root *os.Root, dirs map[string]struct{}) error {
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return strings.Count(ordered[i], "/") > strings.Count(ordered[j], "/")
	})
	for _, dir := range ordered {
		if err := root.Chmod(dir, 0500); err != nil {
			return fmt.Errorf("protect site artifact directory: %w", err)
		}
		child, err := root.OpenRoot(dir)
		if err != nil {
			return fmt.Errorf("open site artifact directory for sync: %w", err)
		}
		err = syncDir(child)
		closeErr := child.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return fmt.Errorf("sync site artifact directory: %w", err)
		}
	}
	return nil
}

func immutableMode(mode uint32) os.FileMode {
	return os.FileMode((mode & 0555) | 0400)
}

func availableBytes(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 {
		return 0, errors.New("invalid filesystem block size")
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	if available > ^uint64(0)/blockSize {
		return ^uint64(0), nil
	}
	return available * blockSize, nil
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
	return errors.Join(dir.Sync(), dir.Close())
}

func syncPathDirectory(root *os.Root, path string) error {
	dir, err := openDirectory(root, path)
	if err != nil {
		return err
	}
	return errors.Join(syncDir(dir), dir.Close())
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

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}
