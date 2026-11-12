package workspace

import (
	"fmt"
	"strings"
)

// PrepareProjectUploadSnapshot maps a complete local project manifest beneath
// the server-registered project path. Client scope and delete lists are not
// accepted: the server chooses the scope and derives deletes from its saved
// folder baseline.
func PrepareProjectUploadSnapshot(folder SyncFolder, request CreateSyncSession) (CreateSyncSession, error) {
	if !ValidSyncFolderID(folder.FolderID) || folder.Kind != SyncFolderKindProject ||
		ValidateSyncProjectFolderName(folder.ProjectName) != nil || folder.RelativePath != "project/"+folder.ProjectName ||
		ValidateRelativePath(folder.RelativePath) != nil || (folder.State != "pending_node" && folder.State != "active") {
		return CreateSyncSession{}, fmt.Errorf("registered project sync folder is invalid or unavailable")
	}
	if request.Direction != SyncUpload || request.Policy != SyncCompareMerge || request.BaseRevision != 0 ||
		len(request.Scope) != 0 || len(request.DeletedPaths) != 0 || len(request.DeletedDirectories) != 0 ||
		request.AckCursor != nil || request.TaskID != "" || request.TaskHubOperationKey != "" {
		return CreateSyncSession{}, fmt.Errorf("project upload requires a server-scoped upload snapshot")
	}
	prepared := request
	prepared.FolderID = folder.FolderID
	prepared.Scope = []string{folder.RelativePath}
	prepared.Files = append([]File(nil), request.Files...)
	for index := range prepared.Files {
		file := &prepared.Files[index]
		if err := ValidateRelativePath(file.Path); err != nil {
			return CreateSyncSession{}, fmt.Errorf("project file path is invalid: %w", err)
		}
		file.Path = folder.RelativePath + "/" + file.Path
		if file.HardlinkTo != "" {
			if err := ValidateRelativePath(file.HardlinkTo); err != nil {
				return CreateSyncSession{}, fmt.Errorf("project hardlink path is invalid: %w", err)
			}
			file.HardlinkTo = folder.RelativePath + "/" + file.HardlinkTo
		}
		if CanonicalFileKind(file.Kind) == FileKindSymlink {
			if err := validateSymlinkTargetWithinRoot(file.Path, file.SymlinkTarget, folder.RelativePath); err != nil {
				return CreateSyncSession{}, fmt.Errorf("project symlink escaped its registered root: %w", err)
			}
		}
		if err := ValidateRelativePath(file.Path); err != nil {
			return CreateSyncSession{}, fmt.Errorf("project file escaped its registered root: %w", err)
		}
	}
	prepared.EmptyDirectories = make([]string, 0, len(request.EmptyDirectories))
	for _, directory := range request.EmptyDirectories {
		if err := ValidateRelativePath(directory); err != nil {
			return CreateSyncSession{}, fmt.Errorf("project directory path is invalid: %w", err)
		}
		prepared.EmptyDirectories = append(prepared.EmptyDirectories, folder.RelativePath+"/"+directory)
	}
	prepared.Directories = make([]Directory, 0, len(request.Directories))
	for _, directory := range request.Directories {
		if directory.Path == "." {
			directory.Path = folder.RelativePath
		} else {
			if err := ValidateRelativePath(directory.Path); err != nil {
				return CreateSyncSession{}, fmt.Errorf("project directory metadata path is invalid: %w", err)
			}
			directory.Path = folder.RelativePath + "/" + directory.Path
		}
		prepared.Directories = append(prepared.Directories, directory)
	}
	if err := ValidateCreateSyncSession(prepared, 0); err != nil {
		return CreateSyncSession{}, fmt.Errorf("project upload snapshot is invalid: %w", err)
	}
	return prepared, nil
}

// ProjectSnapshotDeletions returns only paths present in the last successful
// folder snapshot and absent from the new complete local snapshot.
func ProjectSnapshotDeletions(folderRoot string, baselineFiles []File, baselineEmptyDirectories []string,
	baselineDirectories []Directory, snapshot CreateSyncSession) ([]string, []string, error) {
	if ValidateRelativePath(folderRoot) != nil || len(snapshot.Scope) != 1 || snapshot.Scope[0] != folderRoot ||
		snapshot.FolderID == "" || snapshot.Direction != SyncUpload || snapshot.Policy != SyncCompareMerge {
		return nil, nil, fmt.Errorf("project snapshot scope is invalid")
	}
	presentFiles := make(map[string]struct{}, len(snapshot.Files))
	presentDirectories := make(map[string]struct{}, len(snapshot.Directories)+len(snapshot.EmptyDirectories)+len(snapshot.Files))
	addDirectory := func(value string) error {
		if err := ValidateRelativePath(value); err != nil {
			return err
		}
		for current := value; current != "."; {
			presentDirectories[strings.ToLower(current)] = struct{}{}
			separator := strings.LastIndexByte(current, '/')
			if separator < 0 {
				break
			}
			current = current[:separator]
		}
		return nil
	}
	for _, file := range snapshot.Files {
		if err := ValidateRelativePath(file.Path); err != nil || !inScope(strings.ToLower(file.Path), snapshot.Scope) {
			return nil, nil, fmt.Errorf("project snapshot file is outside its registered root")
		}
		presentFiles[strings.ToLower(file.Path)] = struct{}{}
		separator := strings.LastIndexByte(file.Path, '/')
		if separator < 0 {
			return nil, nil, fmt.Errorf("project snapshot file is outside its registered root")
		}
		if err := addDirectory(file.Path[:separator]); err != nil {
			return nil, nil, fmt.Errorf("project snapshot parent directory is invalid: %w", err)
		}
	}
	for _, directory := range snapshot.EmptyDirectories {
		if err := ValidateRelativePath(directory); err != nil || !inScope(strings.ToLower(directory), snapshot.Scope) {
			return nil, nil, fmt.Errorf("project snapshot directory is outside its registered root")
		}
		if err := addDirectory(directory); err != nil {
			return nil, nil, fmt.Errorf("project snapshot directory is invalid: %w", err)
		}
	}
	for _, directory := range snapshot.Directories {
		if err := ValidateDirectory(directory, true); err != nil || !inDirectoryScope(strings.ToLower(directory.Path), snapshot.Scope) {
			return nil, nil, fmt.Errorf("project snapshot directory metadata is outside its registered root")
		}
		if err := addDirectory(directory.Path); err != nil {
			return nil, nil, fmt.Errorf("project snapshot directory metadata is invalid: %w", err)
		}
	}
	if err := addDirectory(folderRoot); err != nil {
		return nil, nil, err
	}
	deletedFiles := make([]string, 0)
	for _, file := range baselineFiles {
		if !inScope(strings.ToLower(file.Path), []string{folderRoot}) {
			return nil, nil, fmt.Errorf("saved project baseline contains a path outside its root")
		}
		if _, exists := presentFiles[strings.ToLower(file.Path)]; !exists {
			deletedFiles = append(deletedFiles, file.Path)
		}
	}
	deletedDirectories := make([]string, 0)
	for _, directory := range baselineEmptyDirectories {
		if !inDirectoryScope(strings.ToLower(directory), []string{folderRoot}) {
			return nil, nil, fmt.Errorf("saved project baseline contains a directory outside its root")
		}
		if _, exists := presentDirectories[strings.ToLower(directory)]; !exists {
			deletedDirectories = append(deletedDirectories, directory)
		}
	}
	for _, directory := range baselineDirectories {
		if directory.Path == folderRoot {
			continue
		}
		if !inDirectoryScope(strings.ToLower(directory.Path), []string{folderRoot}) {
			return nil, nil, fmt.Errorf("saved project baseline contains directory metadata outside its root")
		}
		if _, exists := presentDirectories[strings.ToLower(directory.Path)]; !exists {
			deletedDirectories = append(deletedDirectories, directory.Path)
		}
	}
	return uniqueStrings(deletedFiles), uniqueStrings(deletedDirectories), nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}
