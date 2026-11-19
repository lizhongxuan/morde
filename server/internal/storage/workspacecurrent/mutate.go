package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

// CreateDirectory creates one Workspace directory without following path links.
func (s *Store) CreateDirectory(ctx context.Context, ownerID, workspaceID, relativePath string, mode uint32) error {
	return s.mutate(ctx, ownerID, workspaceID, relativePath, func(ownerHash string) error {
		return createWorkspaceCurrentDirectory(s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)), mode)
	})
}

// RemoveEmptyDirectory removes one Workspace directory only when it is empty.
func (s *Store) RemoveEmptyDirectory(ctx context.Context, ownerID, workspaceID, relativePath string) error {
	return s.mutate(ctx, ownerID, workspaceID, relativePath, func(ownerHash string) error {
		return removeWorkspaceCurrentEmptyDirectory(s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)))
	})
}

// RemoveRegularFile removes one regular file only if its current bytes match the expected digest.
func (s *Store) RemoveRegularFile(ctx context.Context, ownerID, workspaceID, relativePath, expectedSHA256 string) error {
	if !validWorkspaceSHA256(expectedSHA256) {
		return errors.New("invalid Workspace current file deletion digest")
	}
	return s.mutate(ctx, ownerID, workspaceID, relativePath, func(ownerHash string) error {
		return removeWorkspaceCurrentRegularFile(s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)), expectedSHA256)
	})
}

// MoveRegularFile atomically moves one regular file to an existing directory without replacing an existing target.
func (s *Store) MoveRegularFile(ctx context.Context, ownerID, workspaceID, sourcePath, targetPath, expectedSHA256 string, expectedMode uint32) error {
	if workspace.ValidateRelativePath(targetPath) != nil || sourcePath == targetPath || !validWorkspaceSHA256(expectedSHA256) || expectedMode > 0777 {
		return errors.New("invalid Workspace current file move")
	}
	return s.mutate(ctx, ownerID, workspaceID, sourcePath, func(ownerHash string) error {
		return moveWorkspaceCurrentRegularFile(s.rootPath,
			filepath.Join(ownerHash, workspaceID, filepath.FromSlash(sourcePath)),
			filepath.Join(ownerHash, workspaceID, filepath.FromSlash(targetPath)), expectedSHA256, expectedMode)
	})
}

// WriteSymbolicLink creates or replaces one safe Workspace symbolic link using an expected-target check.
func (s *Store) WriteSymbolicLink(ctx context.Context, ownerID, workspaceID, relativePath, target, expectedTarget string) error {
	file := workspace.File{Kind: workspace.FileKindSymlink, Path: relativePath, SymlinkTarget: target,
		SizeBytes: int64(len(target)), SHA256: sha256Hex([]byte(target)), Chunks: []workspace.Chunk{}}
	if workspace.ValidateFile(file) != nil || (expectedTarget != "" && workspace.ValidateFile(workspace.File{Kind: workspace.FileKindSymlink, Path: relativePath,
		SymlinkTarget: expectedTarget, SizeBytes: int64(len(expectedTarget)), SHA256: sha256Hex([]byte(expectedTarget)), Chunks: []workspace.Chunk{}}) != nil) {
		return errors.New("invalid Workspace current symbolic link")
	}
	return s.mutate(ctx, ownerID, workspaceID, relativePath, func(ownerHash string) error {
		return writeWorkspaceCurrentSymbolicLink(s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)), target, expectedTarget)
	})
}

// RemoveSymbolicLink removes one Workspace symbolic link only if its target still matches.
func (s *Store) RemoveSymbolicLink(ctx context.Context, ownerID, workspaceID, relativePath, expectedTarget string) error {
	if expectedTarget == "" || workspace.ValidateFile(workspace.File{Kind: workspace.FileKindSymlink, Path: relativePath,
		SymlinkTarget: expectedTarget, SizeBytes: int64(len(expectedTarget)), SHA256: sha256Hex([]byte(expectedTarget)), Chunks: []workspace.Chunk{}}) != nil {
		return errors.New("invalid Workspace current symbolic link deletion")
	}
	return s.mutate(ctx, ownerID, workspaceID, relativePath, func(ownerHash string) error {
		return removeWorkspaceCurrentSymbolicLink(s.rootPath, filepath.Join(ownerHash, workspaceID, filepath.FromSlash(relativePath)), expectedTarget)
	})
}

func (s *Store) mutate(ctx context.Context, ownerID, workspaceID, relativePath string, operation func(string) error) (retErr error) {
	if s == nil || s.dead.Load() {
		return ErrClosed
	}
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) ||
		workspace.ValidateRelativePath(relativePath) != nil || operation == nil {
		return errors.New("invalid Workspace current path mutation")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	lock, err := acquireLock(ctx, filepath.Join(s.rootPath, ".morde-locks", ownerHash+"-"+workspaceID+".lock"))
	if err != nil {
		return fmt.Errorf("lock Workspace current path mutation: %w", err)
	}
	defer func() {
		if err := releaseLock(lock); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release Workspace current path mutation lock: %w", err))
		}
	}()
	if err := s.ensureWorkspaceUpdateResolved(ownerHash, workspaceID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := operation(ownerHash); err != nil {
		return fmt.Errorf("mutate Workspace current path: %w", err)
	}
	return nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
