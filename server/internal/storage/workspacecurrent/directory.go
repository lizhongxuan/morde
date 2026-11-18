package workspacecurrent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func (s *Store) ReadDirectory(ownerID, workspaceID, relativePath string) (result []workspace.DirectoryEntry, retErr error) {
	if s == nil || s.dead.Load() || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) ||
		(relativePath != "" && workspace.ValidateRelativePath(relativePath) != nil) {
		return nil, errors.New("invalid Workspace current directory lookup")
	}
	ownerDigest := sha256.Sum256([]byte(ownerID))
	ownerHash := hex.EncodeToString(ownerDigest[:])
	lock, err := s.lockWorkspaceRead(ownerHash, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = releaseLock(lock) }()
	if err := s.ensureWorkspaceUpdateResolved(ownerHash, workspaceID); err != nil {
		return nil, err
	}
	path := filepath.Join(ownerHash, workspaceID)
	if relativePath != "" {
		path = filepath.Join(path, filepath.FromSlash(relativePath))
	}
	directory, err := openWorkspaceCurrentFile(s.rootPath, path)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	info, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, os.ErrInvalid
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result = make([]workspace.DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		if !utf8.ValidString(entry.Name()) {
			return nil, ErrUnsafePath
		}
		value, err := workspaceCurrentDirectoryEntry(directory, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
