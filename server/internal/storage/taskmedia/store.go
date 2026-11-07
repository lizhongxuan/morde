package taskmedia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

var (
	ErrInvalidRoot     = errors.New("task media root must be an absolute non-root directory")
	ErrInvalidIdentity = errors.New("task media identity is invalid")
	ErrInvalidMedia    = errors.New("task media type is unsupported")
	ErrInvalidMetadata = errors.New("task media metadata is invalid")
	ErrIntegrity       = errors.New("task media staged object failed integrity verification")
	ErrInvalidSource   = errors.New("task media source is nil")
	ErrEmpty           = errors.New("task media content is empty")
	ErrTooLarge        = errors.New("task media content exceeds the workspace file limit")
	ErrUnsafePath      = errors.New("task media path is not a regular directory or file")
	ErrClosed          = errors.New("task media store is closed")
)

// Store keeps generated media in a private, dedicated staging root. It does
// not expose filenames and accepts only validated owner, Task, and operation IDs.
type Store struct {
	root *os.Root
	mu   sync.RWMutex
	dead bool
}

func Open(rootPath string) (*Store, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) == string(filepath.Separator) {
		return nil, ErrInvalidRoot
	}
	rootPath = filepath.Clean(rootPath)
	if info, err := os.Lstat(rootPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(rootPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create task media root: %w", err)
		}
	} else {
		return nil, fmt.Errorf("inspect task media root: %w", err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("inspect task media root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrUnsafePath
	}
	if err := os.Chmod(rootPath, 0700); err != nil {
		return nil, fmt.Errorf("protect task media root: %w", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open task media root: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) Stage(ctx context.Context, ownerID, taskID, operationID, mediaType string, source io.Reader) (app.TaskMediaStagedContent, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return app.TaskMediaStagedContent{}, ErrInvalidIdentity
	}
	canonicalType, err := canonicalMediaType(mediaType)
	if err != nil {
		return app.TaskMediaStagedContent{}, err
	}
	if source == nil {
		return app.TaskMediaStagedContent{}, ErrInvalidSource
	}
	if err := ctx.Err(); err != nil {
		return app.TaskMediaStagedContent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return app.TaskMediaStagedContent{}, ErrClosed
	}
	path := objectPath(ownerID, taskID, operationID)
	if err := s.ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return app.TaskMediaStagedContent{}, err
	}
	file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return app.TaskMediaStagedContent{}, fmt.Errorf("create task media staged object: %w", err)
	}
	content, retErr := copyContent(ctx, file, source, workspace.MaxSyncFileBytes)
	if retErr != nil {
		return app.TaskMediaStagedContent{}, s.cleanupPartial(file, path, retErr)
	}
	if err := file.Sync(); err != nil {
		return app.TaskMediaStagedContent{}, s.cleanupPartial(file, path, fmt.Errorf("sync task media staged object: %w", err))
	}
	if err := file.Close(); err != nil {
		removeErr := s.root.Remove(path)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return app.TaskMediaStagedContent{}, errors.Join(fmt.Errorf("close task media staged object: %w", err), fmt.Errorf("remove incomplete task media staged object: %w", removeErr))
		}
		return app.TaskMediaStagedContent{}, fmt.Errorf("close task media staged object: %w", err)
	}
	content.MediaType = canonicalType
	return content, nil
}

func (s *Store) Open(ctx context.Context, ownerID, taskID, operationID string) (io.ReadCloser, error) {
	return s.open(ctx, ownerID, taskID, operationID, nil)
}

func (s *Store) OpenVerified(ctx context.Context, ownerID, taskID, operationID string, expected app.TaskMediaStagedContent) (io.ReadCloser, error) {
	mediaType, err := canonicalMediaType(expected.MediaType)
	if err != nil || mediaType != expected.MediaType || expected.SizeBytes < 1 || expected.SizeBytes > workspace.MaxSyncFileBytes || len(expected.SHA256) != sha256.Size {
		return nil, ErrInvalidMetadata
	}
	return s.open(ctx, ownerID, taskID, operationID, &expected)
}

func (s *Store) open(ctx context.Context, ownerID, taskID, operationID string, expected *app.TaskMediaStagedContent) (io.ReadCloser, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return nil, ErrInvalidIdentity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead {
		return nil, ErrClosed
	}
	path := objectPath(ownerID, taskID, operationID)
	if err := s.validatePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect task media staged object: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > workspace.MaxSyncFileBytes {
		return nil, ErrUnsafePath
	}
	file, err := s.root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open task media staged object: %w", err)
	}
	closeWithError := func(cause error) error {
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(cause, fmt.Errorf("close invalid task media staged object: %w", closeErr))
		}
		return cause
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, closeWithError(fmt.Errorf("inspect task media staged object: %w", err))
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() < 1 || openedInfo.Size() > workspace.MaxSyncFileBytes ||
		(expected != nil && openedInfo.Size() != expected.SizeBytes) {
		return nil, closeWithError(ErrIntegrity)
	}
	if expected != nil {
		hash := sha256.New()
		if _, err := io.CopyN(hash, file, expected.SizeBytes); err != nil {
			return nil, closeWithError(fmt.Errorf("verify task media staged object: %w", err))
		}
		if !bytes.Equal(hash.Sum(nil), expected.SHA256) {
			return nil, closeWithError(ErrIntegrity)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, closeWithError(fmt.Errorf("rewind task media staged object: %w", err))
		}
	}
	return file, nil
}

func (s *Store) Delete(ctx context.Context, ownerID, taskID, operationID string) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(operationID) {
		return ErrInvalidIdentity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return ErrClosed
	}
	path := objectPath(ownerID, taskID, operationID)
	if err := s.validatePrivateDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := s.root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete task media staged object: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil
	}
	s.dead = true
	return s.root.Close()
}

func (s *Store) ensurePrivateDirectory(path string) error {
	if err := s.root.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create task media directory: %w", err)
	}
	return s.validatePrivateDirectory(path)
}

func (s *Store) cleanupPartial(file *os.File, path string, cause error) error {
	closeErr := file.Close()
	removeErr := s.root.Remove(path)
	var cleanupErr error
	if closeErr != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close incomplete task media staged object: %w", closeErr))
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove incomplete task media staged object: %w", removeErr))
	}
	return errors.Join(cause, cleanupErr)
}

func (s *Store) validatePrivateDirectory(path string) error {
	for current := filepath.Dir(path); current != "."; current = filepath.Dir(current) {
		if current == string(filepath.Separator) || current == "" {
			break
		}
		info, err := s.root.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return err
			}
			return errors.Join(ErrUnsafePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return errors.Join(ErrUnsafePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafePath
	}
	return nil
}

func copyContent(ctx context.Context, destination io.Writer, source io.Reader, maxBytes int64) (app.TaskMediaStagedContent, error) {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(contextReader{ctx: ctx, reader: source}, maxBytes+1))
	if err != nil {
		return app.TaskMediaStagedContent{}, fmt.Errorf("write task media staged object: %w", err)
	}
	if written == 0 {
		return app.TaskMediaStagedContent{}, ErrEmpty
	}
	if written > maxBytes {
		return app.TaskMediaStagedContent{}, ErrTooLarge
	}
	return app.TaskMediaStagedContent{SizeBytes: written, SHA256: hash.Sum(nil)}, nil
}

func canonicalMediaType(value string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) > 0 {
		return "", ErrInvalidMedia
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "video/mp4", "audio/mpeg", "audio/ogg", "audio/opus", "audio/wav", "audio/aac":
		return mediaType, nil
	default:
		return "", ErrInvalidMedia
	}
}

func objectPath(ownerID, taskID, operationID string) string {
	return filepath.Join(ownerID, taskID, operationID, "media")
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(target []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(target)
}

var _ app.TaskMediaContentStore = (*Store)(nil)
var _ app.TaskMediaVerifiedContentStore = (*Store)(nil)
