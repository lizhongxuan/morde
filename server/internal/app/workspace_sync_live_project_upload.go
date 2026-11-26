package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
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
	if session.SourceBaselineSessionID != "" {
		if plan.ProjectLiveBaseline == nil {
			return []workspace.Conflict{{Path: session.Scope[0], Reason: "cloud_baseline_unavailable"}}, nil
		}
		if conflicts := projectLiveBaselineConflictsForPendingUpload(*plan.ProjectLiveBaseline, live.files, live.directories, session); len(conflicts) > 0 {
			return conflicts, nil
		}
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
			return []workspace.Conflict{{Path: session.Scope[0], Reason: "content_changed"}}, nil
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
	if plan.RecordProjectLiveBaseline != nil {
		files, _, directories, err := s.readLiveProjectManifest(ctx, ownerID, session.WorkspaceID, session.Scope[0])
		if err != nil {
			return nil, err
		}
		baseline, ok := projectLiveBaselineForSource(session, files, directories)
		if !ok {
			return []workspace.Conflict{{Path: session.Scope[0], Reason: "cloud_changed_during_apply"}}, nil
		}
		if err := plan.RecordProjectLiveBaseline(baseline); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s *WorkspaceSyncService) applyLiveProjectDirectories(ctx context.Context, ownerID string, session workspace.SyncSession,
	existing map[string]workspace.Directory) error {
	directories := append([]workspace.Directory{{Path: session.Scope[0]}}, session.UploadDirectories...)
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

func projectLiveBaselineForSource(source workspace.SyncSession, files []workspace.File, directories []workspace.Directory) (WorkspaceSyncProjectLiveBaseline, bool) {
	result := WorkspaceSyncProjectLiveBaseline{Files: []workspace.File{}, Directories: []workspace.Directory{}}
	if len(source.Scope) != 1 {
		return result, false
	}
	fileMap := make(map[string]workspace.File, len(files))
	for _, file := range files {
		fileMap[strings.ToLower(file.Path)] = file
	}
	for _, expected := range source.Files {
		actual, ok := fileMap[strings.ToLower(expected.Path)]
		if !ok || actual.Path != expected.Path || !directProjectFilesMatch(actual, expected, true) {
			return result, false
		}
		result.Files = append(result.Files, actual)
	}
	directoryMap := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		directoryMap[strings.ToLower(directory.Path)] = directory
	}
	expectedDirectories := make(map[string]workspace.Directory)
	addDirectory := func(directory string) {
		for directory != "." && !strings.EqualFold(directory, source.Scope[0]) {
			key := strings.ToLower(directory)
			if _, exists := expectedDirectories[key]; !exists {
				expectedDirectories[key] = workspace.Directory{Path: directory}
			}
			directory = path.Dir(directory)
		}
	}
	for _, file := range source.Files {
		addDirectory(path.Dir(file.Path))
	}
	for _, directory := range source.EmptyDirectories {
		addDirectory(directory)
	}
	for _, directory := range source.Directories {
		if !strings.EqualFold(directory.Path, source.Scope[0]) {
			addDirectory(directory.Path)
			expectedDirectories[strings.ToLower(directory.Path)] = directory
		}
	}
	keys := make([]string, 0, len(expectedDirectories))
	for key := range expectedDirectories {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		expected := expectedDirectories[key]
		actual, ok := directoryMap[key]
		if !ok || actual.Path != expected.Path || actual.Mode == nil || expected.Mode != nil && *actual.Mode != *expected.Mode {
			return result, false
		}
		result.Directories = append(result.Directories, actual)
	}
	return result, true
}

func projectLiveBaselineMatchesSource(live WorkspaceSyncProjectLiveBaseline, source workspace.SyncSession) bool {
	if len(source.Scope) != 1 {
		return false
	}
	expected, ok := projectLiveBaselineForSource(source, live.Files, live.Directories)
	return ok && len(live.Files) == len(expected.Files) && len(live.Directories) == len(expected.Directories)
}

func projectLiveBaselineConflicts(baseline WorkspaceSyncProjectLiveBaseline, files []workspace.File, directories []workspace.Directory) []workspace.Conflict {
	fileMap := make(map[string]workspace.File, len(files))
	for _, file := range files {
		fileMap[strings.ToLower(file.Path)] = file
	}
	directoryMap := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		directoryMap[strings.ToLower(directory.Path)] = directory
	}
	var conflicts []workspace.Conflict
	for _, previous := range baseline.Files {
		current, ok := fileMap[strings.ToLower(previous.Path)]
		if !ok || !workspace.SameSyncFile(previous, current) {
			conflicts = append(conflicts, workspace.Conflict{Path: previous.Path, Reason: "cloud_changed"})
		}
	}
	for _, previous := range baseline.Directories {
		current, ok := directoryMap[strings.ToLower(previous.Path)]
		if !ok || !workspace.SameDirectoryMetadata(previous, current) {
			conflicts = append(conflicts, workspace.Conflict{Path: previous.Path, Reason: "cloud_changed"})
		}
	}
	return conflicts
}

// A retry after a direct apply error can observe filesystem changes made by its
// own prior attempt. Ignore only pending paths whose observed state matches the
// requested result; preserve every unrelated baseline conflict.
func projectLiveBaselineConflictsForPendingUpload(baseline WorkspaceSyncProjectLiveBaseline, files []workspace.File,
	directories []workspace.Directory, session workspace.SyncSession) []workspace.Conflict {
	conflicts := projectLiveBaselineConflicts(baseline, files, directories)
	if len(conflicts) == 0 {
		return nil
	}
	liveFiles := make(map[string]workspace.File, len(files))
	for _, file := range files {
		liveFiles[strings.ToLower(file.Path)] = file
	}
	liveDirectories := make(map[string]workspace.Directory, len(directories))
	for _, directory := range directories {
		liveDirectories[strings.ToLower(directory.Path)] = directory
	}
	baselineFiles := make(map[string]workspace.File, len(baseline.Files))
	for _, file := range baseline.Files {
		baselineFiles[strings.ToLower(file.Path)] = file
	}
	baselineDirectories := make(map[string]workspace.Directory, len(baseline.Directories))
	for _, directory := range baseline.Directories {
		baselineDirectories[strings.ToLower(directory.Path)] = directory
	}
	plannedFiles := make(map[string]workspace.File, len(session.UploadFiles))
	for _, file := range session.UploadFiles {
		plannedFiles[strings.ToLower(file.Path)] = file
	}
	plannedDeletes := make(map[string]struct{}, len(session.UploadDeletedPaths))
	for _, filePath := range session.UploadDeletedPaths {
		plannedDeletes[strings.ToLower(filePath)] = struct{}{}
	}
	plannedDirectoryDeletes := make(map[string]struct{}, len(session.UploadDeletedDirectories))
	for _, directoryPath := range session.UploadDeletedDirectories {
		plannedDirectoryDeletes[strings.ToLower(directoryPath)] = struct{}{}
	}
	result := make([]workspace.Conflict, 0, len(conflicts))
	for _, conflict := range conflicts {
		key := strings.ToLower(conflict.Path)
		if previous, isFile := baselineFiles[key]; isFile {
			if actual, exists := liveFiles[key]; exists {
				if desired, planned := plannedFiles[key]; planned && pendingProjectUploadFileMatches(actual, previous, desired) {
					continue
				}
			} else if _, planned := plannedDeletes[key]; planned {
				continue
			}
			result = append(result, conflict)
			continue
		}
		previous, isDirectory := baselineDirectories[key]
		if isDirectory {
			if _, exists := liveDirectories[key]; !exists {
				if _, planned := plannedDirectoryDeletes[key]; planned {
					continue
				}
			} else if current := liveDirectories[key]; sameDirectoryMetadataExceptModTime(previous, current) &&
				pendingProjectUploadChangedWithin(session, conflict.Path, liveFiles, liveDirectories, baselineFiles, baselineDirectories) {
				continue
			}
		}
		result = append(result, conflict)
	}
	return result
}

func sameDirectoryMetadataExceptModTime(a, b workspace.Directory) bool {
	// Adding, replacing, or removing a child can advance directory mtime without
	// changing its permissions or native metadata.
	return a.Path == b.Path && sameOptionalWorkspaceMode(a.Mode, b.Mode) && a.MetadataKnown == b.MetadataKnown &&
		workspace.SameNativeMetadata(a.Xattrs, b.Xattrs, a.ACL, b.ACL)
}

func sameOptionalWorkspaceMode(a, b *uint32) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func pendingProjectUploadFileMatches(actual, previous, desired workspace.File) bool {
	// A prior attempt can replace the file and change its mtime. Accept only the
	// pending content and supported mode while preserving metadata the upload does not own.
	return actual.Path == desired.Path && directProjectFilesMatch(actual, desired, true) &&
		actual.MetadataKnown == previous.MetadataKnown && workspace.SameNativeMetadata(actual.Xattrs, previous.Xattrs, actual.ACL, previous.ACL)
}

func pendingProjectUploadChangedWithin(session workspace.SyncSession, directory string, files map[string]workspace.File,
	directories map[string]workspace.Directory, baselineFiles map[string]workspace.File,
	baselineDirectories map[string]workspace.Directory) bool {
	within := func(candidate string) bool {
		return !strings.EqualFold(candidate, directory) && projectPathWithinRoot(candidate, directory)
	}
	for _, desired := range session.UploadFiles {
		if !within(desired.Path) {
			continue
		}
		actual, exists := files[strings.ToLower(desired.Path)]
		if exists && actual.Path == desired.Path {
			if previous, hadBaseline := baselineFiles[strings.ToLower(desired.Path)]; hadBaseline {
				if pendingProjectUploadFileMatches(actual, previous, desired) {
					return true
				}
			} else if desiredCurrentFileMatches(actual, true, desired) {
				return true
			}
		}
	}
	for _, filePath := range session.UploadDeletedPaths {
		if within(filePath) {
			if _, exists := files[strings.ToLower(filePath)]; !exists {
				return true
			}
		}
	}
	for _, directoryPath := range session.UploadDeletedDirectories {
		if within(directoryPath) {
			if _, exists := directories[strings.ToLower(directoryPath)]; !exists {
				return true
			}
		}
	}
	for _, directoryPath := range append(append([]string(nil), session.UploadEmptyDirectories...), directoryPaths(session.UploadDirectories)...) {
		if within(directoryPath) {
			key := strings.ToLower(directoryPath)
			_, existedInBaseline := baselineDirectories[key]
			if _, exists := directories[key]; exists && !existedInBaseline {
				return true
			}
		}
	}
	return false
}

func directoryPaths(values []workspace.Directory) []string {
	paths := make([]string, 0, len(values))
	for _, value := range values {
		paths = append(paths, value.Path)
	}
	return paths
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
