package app

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

type workspaceCurrentRegularFileMover interface {
	MoveRegularFile(context.Context, string, string, string, string, string, uint32) error
}

func (s *WorkspaceSyncService) readLiveProjectManifest(ctx context.Context, ownerID, workspaceID, projectRoot string) ([]workspace.File,
	[]string, []workspace.Directory, error) {
	live, err := readWorkspaceLiveSnapshot(ctx, s.currentDirectory, ownerID, workspaceID, s.maxOwnerBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	for blocked := range live.blockedPaths {
		if projectPathWithinRoot(blocked, projectRoot) {
			return nil, nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "project directory contains an unsupported filesystem entry")
		}
	}
	files := make([]workspace.File, 0)
	for _, file := range live.files {
		if projectPathWithinRoot(file.Path, projectRoot) {
			files = append(files, file)
		}
	}
	emptyDirectories := make([]string, 0)
	for _, directory := range live.emptyDirectories {
		if projectPathWithinRoot(directory, projectRoot) {
			emptyDirectories = append(emptyDirectories, directory)
		}
	}
	directories := make([]workspace.Directory, 0)
	for _, directory := range live.directories {
		if projectPathWithinRoot(directory.Path, projectRoot) {
			directories = append(directories, directory)
		}
	}
	return files, emptyDirectories, directories, nil
}

func (s *WorkspaceSyncService) applyLiveProjectUpload(ctx context.Context, plan WorkspaceSyncDirectCommitPlan) ([]workspace.Conflict, error) {
	session := plan.Session
	if s == nil || s.currentDirectory == nil || session.FolderID == "" || session.Direction != workspace.SyncUpload ||
		len(session.Scope) != 1 || workspace.ValidateRelativePath(session.Scope[0]) != nil {
		return nil, domain.NewError(domain.CodeWorkspaceUnavailable, "direct project upload directory is unavailable")
	}
	ownerID := session.OwnerID
	if !domain.ValidUserID(ownerID) {
		ownerID = session.ReplicaID
		if strings.HasPrefix(ownerID, "session:") {
			ownerID = strings.TrimPrefix(ownerID, "session:")
		} else {
			return nil, domain.NewError(domain.CodeWorkspaceUnavailable, "direct project upload owner is unavailable")
		}
	}
	live, err := readWorkspaceLiveSnapshot(ctx, s.currentDirectory, ownerID, session.WorkspaceID, s.maxOwnerBytes)
	if err != nil {
		return nil, err
	}
	files := make(map[string]workspace.File, len(live.files))
	directories := make(map[string]workspace.Directory, len(live.directories))
	emptyDirectories := make(map[string]struct{}, len(live.emptyDirectories))
	blocked := make(map[string]struct{}, len(live.blockedPaths))
	for _, file := range live.files {
		files[strings.ToLower(file.Path)] = file
	}
	for _, directory := range live.directories {
		directories[strings.ToLower(directory.Path)] = directory
	}
	for _, directory := range live.emptyDirectories {
		emptyDirectories[strings.ToLower(directory)] = struct{}{}
	}
	for filePath := range live.blockedPaths {
		blocked[strings.ToLower(filePath)] = struct{}{}
	}
	pinnedFiles := make(map[string]workspace.File, len(plan.PinnedFiles))
	for _, file := range plan.PinnedFiles {
		pinnedFiles[strings.ToLower(file.Path)] = file
	}
	pinnedDirectories := make(map[string]workspace.Directory, len(plan.PinnedDirectoryMetadata))
	for _, directory := range plan.PinnedDirectoryMetadata {
		pinnedDirectories[strings.ToLower(directory.Path)] = directory
	}
	for _, directory := range plan.PinnedEmptyDirectories {
		if _, exists := pinnedDirectories[strings.ToLower(directory)]; !exists {
			pinnedDirectories[strings.ToLower(directory)] = workspace.Directory{Path: directory}
		}
	}
	conflicts := make([]workspace.Conflict, 0)
	addConflict := func(filePath, reason string, local, remote *workspace.File) {
		conflict := workspace.Conflict{Path: filePath, Reason: reason}
		if local != nil {
			conflict.LocalSHA256 = stringPointer(local.SHA256)
		}
		if remote != nil {
			conflict.RemoteSHA256 = stringPointer(remote.SHA256)
		}
		conflicts = append(conflicts, conflict)
	}
	pathBlocked := func(filePath string) bool {
		filePath = strings.ToLower(filePath)
		for blockedPath := range blocked {
			if workspaceFilePathContains(blockedPath, filePath) || workspaceFilePathContains(filePath, blockedPath) {
				return true
			}
		}
		return false
	}

	for _, desired := range session.UploadFiles {
		key := strings.ToLower(desired.Path)
		if desired.HardlinkTo != "" {
			addConflict(desired.Path, "unsupported_file_type", &desired, nil)
			continue
		}
		if pathBlocked(desired.Path) {
			addConflict(desired.Path, "unsupported_filesystem_entry", &desired, nil)
			continue
		}
		current, exists := files[key]
		if exists && current.Path != desired.Path {
			addConflict(current.Path, "case_collision", &desired, &current)
			continue
		}
		if exists && !directProjectKindsCompatible(current, desired) {
			addConflict(desired.Path, "type_changed", &desired, &current)
			continue
		}
		if desiredCurrentFileMatches(current, exists, desired) {
			continue
		}
		if directory, exists := directories[key]; exists {
			addConflict(directory.Path, "type_changed", &desired, nil)
			continue
		}
		base, baseExists := pinnedFiles[key]
		if exists != baseExists || exists && !directProjectFilesMatch(current, base, true) {
			addConflict(desired.Path, "content_changed", &desired, optionalWorkspaceFile(current, exists))
		}
	}
	for _, filePath := range session.UploadDeletedPaths {
		key := strings.ToLower(filePath)
		if pathBlocked(filePath) {
			addConflict(filePath, "unsupported_filesystem_entry", nil, nil)
			continue
		}
		current, exists := files[key]
		if !exists {
			continue
		}
		base, baseExists := pinnedFiles[key]
		if !baseExists || !directProjectFilesMatch(current, base, true) {
			addConflict(filePath, "delete_changed", nil, &current)
		}
	}
	for _, directory := range append(append([]workspace.Directory(nil), session.UploadDirectories...), directProjectEmptyDirectoryRecords(session.UploadEmptyDirectories)...) {
		key := strings.ToLower(directory.Path)
		if pathBlocked(directory.Path) {
			addConflict(directory.Path, "unsupported_filesystem_entry", nil, nil)
			continue
		}
		if file, exists := files[key]; exists {
			addConflict(directory.Path, "type_changed", nil, &file)
		}
	}
	deletedFiles := make(map[string]struct{}, len(session.UploadDeletedPaths))
	for _, filePath := range session.UploadDeletedPaths {
		deletedFiles[strings.ToLower(filePath)] = struct{}{}
	}
	deletedDirectories := make(map[string]struct{}, len(session.UploadDeletedDirectories))
	for _, directoryPath := range session.UploadDeletedDirectories {
		deletedDirectories[strings.ToLower(directoryPath)] = struct{}{}
	}
	for _, directoryPath := range session.UploadDeletedDirectories {
		key := strings.ToLower(directoryPath)
		if pathBlocked(directoryPath) {
			addConflict(directoryPath, "unsupported_filesystem_entry", nil, nil)
			continue
		}
		current, exists := directories[key]
		if !exists {
			continue
		}
		if _, pinned := pinnedDirectories[key]; !pinned {
			addConflict(directoryPath, "delete_changed", nil, nil)
			continue
		}
		entries, err := s.currentDirectory.ReadDirectory(ownerID, session.WorkspaceID, directoryPath)
		if err != nil {
			return nil, domain.NewError(domain.CodeWorkspaceUnavailable, "project directory could not be checked before upload")
		}
		for _, entry := range entries {
			entryPath := directoryPath + "/" + entry.Name
			if entry.Kind == "directory" {
				if _, planned := deletedDirectories[strings.ToLower(entryPath)]; planned {
					continue
				}
			} else if _, planned := deletedFiles[strings.ToLower(entryPath)]; planned {
				continue
			}
			addConflict(current.Path, "directory_not_empty", nil, nil)
			break
		}
	}
	if len(conflicts) > 0 {
		return conflicts, nil
	}
	if err := s.applyLiveProjectDirectories(ctx, ownerID, session, directories); err != nil {
		if errors.Is(err, workspacecurrent.ErrWriteConflict) || errors.Is(err, workspacecurrent.ErrUnsafePath) || errors.Is(err, os.ErrExist) {
			return []workspace.Conflict{{Path: ".", Reason: "content_changed"}}, nil
		}
		return nil, err
	}
	movedSources, movedTargets := map[string]struct{}{}, map[string]struct{}{}
	if mover, ok := s.currentDirectory.(workspaceCurrentRegularFileMover); ok {
		deletedPaths := append([]string(nil), session.UploadDeletedPaths...)
		sort.Strings(deletedPaths)
		projectRoot := session.Scope[0]
		for _, desired := range session.UploadFiles {
			targetKey := strings.ToLower(desired.Path)
			if _, exists := files[targetKey]; exists || workspace.CanonicalFileKind(desired.Kind) != workspace.FileKindRegular {
				continue
			}
			for _, sourcePath := range deletedPaths {
				sourceKey := strings.ToLower(sourcePath)
				if _, moved := movedSources[sourceKey]; moved {
					continue
				}
				if _, planned := deletedFiles[sourceKey]; !planned || workspaceFilePathContains(sourceKey, targetKey) || workspaceFilePathContains(targetKey, sourceKey) {
					continue
				}
				source, exists := files[sourceKey]
				base, pinned := pinnedFiles[sourceKey]
				if !exists || !pinned || !projectPathWithinRoot(source.Path, projectRoot) || !projectPathWithinRoot(desired.Path, projectRoot) ||
					!directProjectFilesMatch(source, base, true) || !directProjectFilesMatch(source, desired, true) {
					continue
				}
				if err := mover.MoveRegularFile(ctx, ownerID, session.WorkspaceID, source.Path, desired.Path, source.SHA256, desired.Mode); err != nil {
					if errors.Is(err, workspacecurrent.ErrWriteConflict) || errors.Is(err, workspacecurrent.ErrUnsafePath) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
						return []workspace.Conflict{{Path: source.Path, Reason: "content_changed", LocalSHA256: stringPointer(desired.SHA256), RemoteSHA256: stringPointer(source.SHA256)}}, nil
					}
					return nil, err
				}
				movedSources[sourceKey] = struct{}{}
				movedTargets[targetKey] = struct{}{}
				break
			}
		}
	}
	for _, desired := range session.UploadFiles {
		key := strings.ToLower(desired.Path)
		if _, moved := movedTargets[key]; moved {
			continue
		}
		current, exists := files[key]
		if desiredCurrentFileMatches(current, exists, desired) {
			continue
		}
		var err error
		if workspace.CanonicalFileKind(desired.Kind) == workspace.FileKindSymlink {
			expectedTarget := ""
			if exists {
				expectedTarget = current.SymlinkTarget
			}
			err = s.currentDirectory.WriteSymbolicLink(ctx, ownerID, session.WorkspaceID, desired.Path, desired.SymlinkTarget, expectedTarget)
		} else if workspace.CanonicalFileKind(desired.Kind) == workspace.FileKindRegular {
			expectedSHA := ""
			if exists {
				expectedSHA = current.SHA256
			}
			reader := &workspaceSyncUploadFileReader{ctx: ctx, blocks: s.blocks, workspaceID: session.WorkspaceID, chunks: desired.Chunks}
			err = s.currentDirectory.WriteRegularFileFrom(ctx, ownerID, session.WorkspaceID, desired.Path, reader,
				desired.SizeBytes, desired.SHA256, desired.Mode, expectedSHA)
		} else {
			return nil, domain.NewError(domain.CodeWorkspaceConflict, "project upload contains an unsupported file type")
		}
		if err != nil {
			if errors.Is(err, workspacecurrent.ErrWriteConflict) || errors.Is(err, workspacecurrent.ErrUnsafePath) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
				return []workspace.Conflict{{Path: desired.Path, Reason: "content_changed", LocalSHA256: stringPointer(desired.SHA256)}}, nil
			}
			return nil, err
		}
	}
	for _, filePath := range session.UploadDeletedPaths {
		key := strings.ToLower(filePath)
		if _, moved := movedSources[key]; moved {
			continue
		}
		current, exists := files[key]
		if !exists {
			continue
		}
		if workspace.CanonicalFileKind(current.Kind) == workspace.FileKindSymlink {
			err = s.currentDirectory.RemoveSymbolicLink(ctx, ownerID, session.WorkspaceID, filePath, current.SymlinkTarget)
		} else {
			err = s.currentDirectory.RemoveRegularFile(ctx, ownerID, session.WorkspaceID, filePath, current.SHA256)
		}
		if err != nil {
			if errors.Is(err, workspacecurrent.ErrWriteConflict) || errors.Is(err, workspacecurrent.ErrUnsafePath) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
				return []workspace.Conflict{{Path: filePath, Reason: "delete_changed", RemoteSHA256: stringPointer(current.SHA256)}}, nil
			}
			return nil, err
		}
	}
	deletedDirectoryPaths := append([]string(nil), session.UploadDeletedDirectories...)
	sort.Slice(deletedDirectoryPaths, func(i, j int) bool {
		depthI, depthJ := strings.Count(deletedDirectoryPaths[i], "/"), strings.Count(deletedDirectoryPaths[j], "/")
		if depthI != depthJ {
			return depthI > depthJ
		}
		return deletedDirectoryPaths[i] > deletedDirectoryPaths[j]
	})
	for _, directoryPath := range deletedDirectoryPaths {
		key := strings.ToLower(directoryPath)
		if _, exists := directories[key]; !exists {
			continue
		}
		if err := s.currentDirectory.RemoveEmptyDirectory(ctx, ownerID, session.WorkspaceID, directoryPath); err != nil {
			if errors.Is(err, workspacecurrent.ErrWriteConflict) || errors.Is(err, workspacecurrent.ErrUnsafePath) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
				return []workspace.Conflict{{Path: directoryPath, Reason: "directory_not_empty"}}, nil
			}
			return nil, err
		}
	}
	return nil, nil
}

func (s *WorkspaceSyncService) applyLiveProjectDirectories(ctx context.Context, ownerID string, session workspace.SyncSession,
	existing map[string]workspace.Directory) error {
	directories := append([]workspace.Directory(nil), session.UploadDirectories...)
	for _, directory := range session.UploadEmptyDirectories {
		directories = append(directories, workspace.Directory{Path: directory})
	}
	sort.Slice(directories, func(i, j int) bool {
		depthI, depthJ := strings.Count(directories[i].Path, "/"), strings.Count(directories[j].Path, "/")
		if depthI != depthJ {
			return depthI < depthJ
		}
		return directories[i].Path < directories[j].Path
	})
	for _, directory := range directories {
		if _, ok := existing[strings.ToLower(directory.Path)]; ok {
			continue
		}
		mode := uint32(0750)
		if directory.Mode != nil {
			mode = *directory.Mode
		}
		if err := s.currentDirectory.CreateDirectory(ctx, ownerID, session.WorkspaceID, directory.Path, mode); err != nil {
			return err
		}
	}
	return nil
}

func directProjectEmptyDirectoryRecords(paths []string) []workspace.Directory {
	result := make([]workspace.Directory, 0, len(paths))
	for _, path := range paths {
		result = append(result, workspace.Directory{Path: path})
	}
	return result
}

func desiredCurrentFileMatches(current workspace.File, exists bool, desired workspace.File) bool {
	return exists && directProjectFilesMatch(current, desired, true)
}

func directProjectFilesMatch(current, expected workspace.File, includeMode bool) bool {
	currentKind, expectedKind := workspace.CanonicalFileKind(current.Kind), workspace.CanonicalFileKind(expected.Kind)
	if currentKind == workspace.FileKindSymlink || expectedKind == workspace.FileKindSymlink {
		return currentKind == workspace.FileKindSymlink && expectedKind == workspace.FileKindSymlink && current.SymlinkTarget == expected.SymlinkTarget
	}
	if currentKind != workspace.FileKindRegular || expectedKind != workspace.FileKindRegular || current.SizeBytes != expected.SizeBytes || current.SHA256 != expected.SHA256 {
		return false
	}
	return !includeMode || !current.MetadataKnown || !expected.MetadataKnown || current.Mode == expected.Mode
}

func directProjectKindsCompatible(current, desired workspace.File) bool {
	return workspace.CanonicalFileKind(current.Kind) == workspace.CanonicalFileKind(desired.Kind)
}

func optionalWorkspaceFile(value workspace.File, exists bool) *workspace.File {
	if !exists {
		return nil
	}
	return &value
}

type workspaceSyncUploadFileReader struct {
	ctx         context.Context
	blocks      WorkspaceBlockReader
	workspaceID string
	chunks      []workspace.Chunk
	index       int
	current     []byte
	offset      int
}

func (r *workspaceSyncUploadFileReader) Read(buffer []byte) (int, error) {
	for r.offset >= len(r.current) {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.index >= len(r.chunks) {
			return 0, io.EOF
		}
		chunk := r.chunks[r.index]
		data, err := readVerifiedBlock(r.blocks, r.workspaceID, chunk)
		if err != nil {
			return 0, err
		}
		r.index++
		r.current, r.offset = data, 0
	}
	count := copy(buffer, r.current[r.offset:])
	r.offset += count
	return count, nil
}
