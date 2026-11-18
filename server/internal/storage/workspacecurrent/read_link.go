package workspacecurrent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func (s *Store) ReadSymbolicLink(ownerID, workspaceID, relativePath string) (string, error) {
	if s == nil || s.dead.Load() {
		return "", ErrClosed
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || workspace.ValidateRelativePath(relativePath) != nil {
		return "", errors.New("invalid Workspace current symbolic link lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	lock, err := s.lockWorkspaceRead(ownerHash, workspaceID)
	if err != nil {
		return "", fmt.Errorf("lock Workspace current symbolic link lookup: %w", err)
	}
	defer func() { _ = releaseLock(lock) }()
	if err := s.ensureWorkspaceUpdateResolved(ownerHash, workspaceID); err != nil {
		return "", err
	}
	parentPath := filepath.Join(ownerHash, workspaceID)
	if parent := path.Dir(relativePath); parent != "." {
		parentPath = filepath.Join(parentPath, filepath.FromSlash(parent))
	}
	return readWorkspaceCurrentSymbolicLink(s.rootPath, parentPath, path.Base(relativePath))
}
