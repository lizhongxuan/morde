package workspacecurrent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

// OpenRegularFile opens an already materialized file without following symlinks
// below the current-file root. The returned descriptor remains stable if the
// directory tree is atomically exchanged after it is opened.
func (s *Store) OpenRegularFile(ownerID, workspaceID, relativePath string) (*os.File, os.FileInfo, error) {
	if s == nil || s.dead.Load() {
		return nil, nil, ErrClosed
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || workspace.ValidateRelativePath(relativePath) != nil {
		return nil, nil, errors.New("invalid Workspace current file lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	path := filepath.Join(hex.EncodeToString(ownerDigest[:]), workspaceID, filepath.FromSlash(relativePath))
	file, err := openWorkspaceCurrentFile(s.rootPath, path)
	if err != nil {
		return nil, nil, fmt.Errorf("open Workspace current file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("inspect Workspace current file: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.Join(ErrUnsafePath, file.Close())
	}
	return file, info, nil
}

// StatPath returns metadata for a path beneath one Workspace current directory
// without following symlinks. It accepts directories so file-to-directory
// transitions can settle a file deletion without touching descendants.
func (s *Store) StatPath(ownerID, workspaceID, relativePath string) (os.FileInfo, error) {
	if s == nil || s.dead.Load() {
		return nil, ErrClosed
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || workspace.ValidateRelativePath(relativePath) != nil {
		return nil, errors.New("invalid Workspace current path lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	path := filepath.Join(hex.EncodeToString(ownerDigest[:]), workspaceID, filepath.FromSlash(relativePath))
	file, err := openWorkspaceCurrentFile(s.rootPath, path)
	if err != nil {
		return nil, fmt.Errorf("stat Workspace current path: %w", err)
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect Workspace current path: %w", err), closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Workspace current path: %w", closeErr)
	}
	return info, nil
}

func (s *Store) IsEmptyDirectory(ownerID, workspaceID, relativePath string) (empty bool, retErr error) {
	if s == nil || s.dead.Load() {
		return false, ErrClosed
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || workspace.ValidateRelativePath(relativePath) != nil {
		return false, errors.New("invalid Workspace current directory lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	path := filepath.Join(hex.EncodeToString(ownerDigest[:]), workspaceID, filepath.FromSlash(relativePath))
	directory, err := openWorkspaceCurrentFile(s.rootPath, path)
	if err != nil {
		return false, fmt.Errorf("open Workspace current directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	info, err := directory.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect Workspace current directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrUnsafePath
	}
	entries, err := directory.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Workspace current directory: %w", err)
	}
	return len(entries) == 0, nil
}
