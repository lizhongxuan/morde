package workspacecurrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

// WriteRegularFile atomically creates or replaces one regular file beneath a
// Workspace without following links in any path component.
func (s *Store) WriteRegularFile(ctx context.Context, ownerID, workspaceID, relativePath string, content []byte, mode uint32, expectedSHA256 string) (retErr error) {
	digest := sha256.Sum256(content)
	return s.WriteRegularFileFrom(ctx, ownerID, workspaceID, relativePath, bytes.NewReader(content), int64(len(content)), hex.EncodeToString(digest[:]), mode, expectedSHA256)
}

// WriteRegularFileFrom atomically creates or replaces one regular file beneath a
// Workspace without following links in any path component.
func (s *Store) WriteRegularFileFrom(ctx context.Context, ownerID, workspaceID, relativePath string, content io.Reader, size int64,
	sha256Hex string, mode uint32, expectedSHA256 string) (retErr error) {
	if s == nil || s.dead.Load() {
		return ErrClosed
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) ||
		content == nil || size < 0 || !validWorkspaceSHA256(sha256Hex) || workspace.ValidateRelativePath(relativePath) != nil || mode > 0777 {
		return errors.New("invalid Workspace current file write")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	lock, err := acquireLock(ctx, filepath.Join(s.rootPath, ".morde-locks", ownerHash+"-"+workspaceID+".lock"))
	if err != nil {
		return fmt.Errorf("lock Workspace current file write: %w", err)
	}
	defer func() {
		if err := releaseLock(lock); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release Workspace current file write lock: %w", err))
		}
	}()
	if err := s.ensureWorkspaceUpdateResolved(ownerHash, workspaceID); err != nil {
		return err
	}
	if err := writeWorkspaceCurrentFileFrom(ctx, s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)), content,
		size, sha256Hex, mode, expectedSHA256); err != nil {
		return fmt.Errorf("write Workspace current file: %w", err)
	}
	return nil
}

func validWorkspaceSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}
