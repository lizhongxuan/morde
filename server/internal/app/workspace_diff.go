package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const (
	maxWorkspaceDiffChanges       = 500
	maxWorkspaceDiffPaths         = 1000
	maxWorkspaceDiffTextFileBytes = 64 << 10
	maxWorkspaceDiffTextBytes     = 256 << 10
	maxWorkspaceDiffPathBytes     = 256 << 10
)

type WorkspaceDiffVersion struct {
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	SymlinkTarget string  `json:"symlink_target,omitempty"`
	SizeBytes     *int64  `json:"size_bytes,omitempty"`
	SHA256        string  `json:"sha256,omitempty"`
	Mode          *uint32 `json:"mode,omitempty"`
	ModTimeNS     *int64  `json:"mod_time_ns,omitempty"`
}

type WorkspaceDiffChange struct {
	Path          string                `json:"path"`
	Action        string                `json:"action"`
	Before        *WorkspaceDiffVersion `json:"before"`
	After         *WorkspaceDiffVersion `json:"after"`
	DisplayStatus string                `json:"display_status"`
	BeforeText    *string               `json:"before_text"`
	AfterText     *string               `json:"after_text"`
}

type WorkspaceDiff struct {
	WorkspaceID  string                `json:"workspace_id"`
	FromRevision int64                 `json:"from_revision"`
	ToRevision   int64                 `json:"to_revision"`
	Changes      []WorkspaceDiffChange `json:"changes"`
}

type WorkspaceRestoreCommand struct {
	TargetRevision          int64
	ExpectedCurrentRevision int64
	Paths                   []string
}

type WorkspaceRestoreResult struct {
	WorkspaceID             string `json:"workspace_id"`
	TargetRevision          int64  `json:"target_revision"`
	ExpectedCurrentRevision int64  `json:"expected_current_revision"`
	Revision                int64  `json:"revision"`
	Replayed                bool   `json:"replayed"`
}

type workspaceRevisionRestorer interface {
	RestoreWorkspaceRevision(context.Context, string, string, int64, int64, []string, IdempotencyRecord) (WorkspaceRestoreResult, error)
}

type workspaceDiffSnapshotEntry struct {
	version   *WorkspaceDiffVersion
	file      *workspace.File
	directory *workspace.Directory
}

// GetDiff compares two immutable, owner-authorized snapshots. Content is returned
// only for small UTF-8 files without NUL bytes; other files retain metadata only.
func (s *WorkspaceSyncService) GetDiff(ctx context.Context, principal identity.Principal, workspaceID string, fromRevision, toRevision int64) (WorkspaceDiff, error) {
	if s == nil || s.store == nil || s.blocks == nil {
		return WorkspaceDiff{}, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return WorkspaceDiff{}, domain.NewError(domain.CodeForbidden, "workspace diff requires an owner Portal session")
	}
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	if fromRevision < 0 || toRevision < fromRevision || toRevision > value.Revision {
		return WorkspaceDiff{}, domain.NewError(domain.CodeWorkspaceConflict, "requested Workspace diff revisions are unavailable")
	}
	return s.getDiffAtRevisions(ctx, principal.UserID, workspaceID, fromRevision, toRevision)
}

// GetHostDiff exposes the same bounded diff projection to the Host owner after
// confirming both immutable revisions are at or before the active applied
// binding revision. It intentionally accepts only the owner actor.
func (s *WorkspaceSyncService) GetHostDiff(ctx context.Context, actorID, hostID, workspaceID string, fromRevision, toRevision int64) (WorkspaceDiff, error) {
	if fromRevision < 0 || toRevision < fromRevision {
		return WorkspaceDiff{}, domain.NewError(domain.CodeWorkspaceConflict, "requested Workspace diff revisions are unavailable")
	}
	binding, appliedRevision, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	if binding.OwnerID != actorID {
		return WorkspaceDiff{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if toRevision > appliedRevision {
		return WorkspaceDiff{}, domain.NewError(domain.CodeWorkspaceConflict, "requested Workspace diff revisions are unavailable")
	}
	result, err := s.getDiffAtRevisions(ctx, actorID, workspaceID, fromRevision, toRevision)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	current, currentRevision, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	if currentRevision != appliedRevision || !sameHostWorkspaceReadBinding(binding, current) {
		return WorkspaceDiff{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding changed while reading the diff")
	}
	return result, nil
}

func (s *WorkspaceSyncService) getDiffAtRevisions(ctx context.Context, ownerID, workspaceID string, fromRevision, toRevision int64) (WorkspaceDiff, error) {
	result := WorkspaceDiff{WorkspaceID: workspaceID, FromRevision: fromRevision, ToRevision: toRevision, Changes: []WorkspaceDiffChange{}}
	fromFiles, fromDirectories, fromDirectoryMetadata, err := s.readManifest(ctx, ownerID, workspaceID, fromRevision, nil)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	toFiles, toDirectories, toDirectoryMetadata, err := s.readManifest(ctx, ownerID, workspaceID, toRevision, nil)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	changes, err := diffWorkspaceSnapshots(fromFiles, fromDirectories, fromDirectoryMetadata, toFiles, toDirectories, toDirectoryMetadata)
	if err != nil {
		return WorkspaceDiff{}, err
	}
	if len(changes) > maxWorkspaceDiffChanges {
		return WorkspaceDiff{}, domain.NewError(domain.CodeResultTooLarge, "Workspace diff has too many changes; compare a narrower set of revisions")
	}
	var pathBytes int
	for _, change := range changes {
		pathBytes += len(change.Path)
		if change.Before != nil {
			pathBytes += len(change.Before.Path)
		}
		if change.After != nil {
			pathBytes += len(change.After.Path)
		}
		if pathBytes > maxWorkspaceDiffPathBytes {
			return WorkspaceDiff{}, domain.NewError(domain.CodeResultTooLarge, "Workspace diff paths exceed the response limit")
		}
	}
	fromFileMap, toFileMap := workspaceDiffFilesByPath(fromFiles), workspaceDiffFilesByPath(toFiles)
	var displayedBytes int64
	for i := range changes {
		change := &changes[i]
		if change.Before == nil || change.Before.Kind != "file" {
			if change.After == nil || change.After.Kind != "file" {
				change.DisplayStatus = "not_applicable"
				continue
			}
		}
		var oldFile, newFile *workspace.File
		if change.Before != nil && change.Before.Kind == "file" {
			if file, exists := fromFileMap[strings.ToLower(change.Before.Path)]; exists {
				oldFile = &file
			}
		}
		if change.After != nil && change.After.Kind == "file" {
			if file, exists := toFileMap[strings.ToLower(change.After.Path)]; exists {
				newFile = &file
			}
		}
		if oldFile != nil && newFile != nil && oldFile.SHA256 == newFile.SHA256 {
			change.DisplayStatus = "metadata_only"
			continue
		}
		if (oldFile != nil && oldFile.SizeBytes > maxWorkspaceDiffTextFileBytes) || (newFile != nil && newFile.SizeBytes > maxWorkspaceDiffTextFileBytes) {
			change.DisplayStatus = "too_large"
			continue
		}
		var oldText, newText *string
		status := "text"
		if oldFile != nil {
			content, displayable, readErr := readWorkspaceDiffText(ctx, s.blocks, workspaceID, *oldFile)
			if readErr != nil {
				return WorkspaceDiff{}, readErr
			}
			if !displayable {
				status = "binary"
			} else {
				oldText = &content
			}
		}
		if newFile != nil {
			content, displayable, readErr := readWorkspaceDiffText(ctx, s.blocks, workspaceID, *newFile)
			if readErr != nil {
				return WorkspaceDiff{}, readErr
			}
			if !displayable {
				status = "binary"
			} else {
				newText = &content
			}
		}
		var textBytes int64
		if oldText != nil {
			textBytes += int64(len(*oldText))
		}
		if newText != nil {
			textBytes += int64(len(*newText))
		}
		if status == "binary" {
			change.DisplayStatus = status
			continue
		}
		if displayedBytes > maxWorkspaceDiffTextBytes-textBytes {
			change.DisplayStatus = "too_large"
			continue
		}
		displayedBytes += textBytes
		change.DisplayStatus = "text"
		change.BeforeText, change.AfterText = oldText, newText
	}
	result.Changes = changes
	return result, nil
}

func (s *WorkspaceSyncService) Restore(ctx context.Context, principal identity.Principal, workspaceID string, command WorkspaceRestoreCommand, idempotencyKey string) (WorkspaceRestoreResult, error) {
	if s == nil || s.store == nil {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeForbidden, "workspace restore requires an owner Portal session")
	}
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return WorkspaceRestoreResult{}, err
	}
	if value.ReadOnly {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
	}
	paths, err := normalizeWorkspaceRestorePaths(command.Paths)
	if err != nil {
		return WorkspaceRestoreResult{}, err
	}
	if command.TargetRevision < 0 || command.TargetRevision > value.Revision || command.ExpectedCurrentRevision < 0 || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return WorkspaceRestoreResult{}, domain.InvalidArgument("restore", "Workspace restore request is invalid")
	}
	restorer, ok := s.store.(workspaceRevisionRestorer)
	if !ok {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace restore is unavailable")
	}
	request := struct {
		WorkspaceID             string
		TargetRevision          int64
		ExpectedCurrentRevision int64
		Paths                   []string
	}{workspaceID, command.TargetRevision, command.ExpectedCurrentRevision, paths}
	encoded, err := json.Marshal(request)
	if err != nil {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeInternal, "Workspace restore request cannot be encoded")
	}
	digest := sha256.Sum256(encoded)
	result, err := restorer.RestoreWorkspaceRevision(ctx, principal.UserID, workspaceID, command.TargetRevision,
		command.ExpectedCurrentRevision, paths, IdempotencyRecord{
			OwnerID: principal.UserID, Operation: "workspace.restore", Key: idempotencyKey,
			RequestHash: digest[:], ResourceID: workspaceID,
		})
	if err != nil {
		return WorkspaceRestoreResult{}, err
	}
	if result.WorkspaceID != workspaceID || result.TargetRevision != command.TargetRevision || result.ExpectedCurrentRevision != command.ExpectedCurrentRevision || result.Revision < 0 {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace restore receipt is invalid")
	}
	if s.materializeCurrent == nil {
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace restore materializer is unavailable")
	}
	if err := s.materializeWorkspaceCurrent(ctx, workspaceID); err != nil {
		if ctx.Err() != nil {
			return WorkspaceRestoreResult{}, ctx.Err()
		}
		return WorkspaceRestoreResult{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace restore is committed but materialization is pending; retry with the same idempotency key")
	}
	return result, nil
}

func normalizeWorkspaceRestorePaths(paths []string) ([]string, error) {
	if len(paths) == 0 || len(paths) > maxWorkspaceDiffPaths {
		return nil, domain.InvalidArgument("paths", "Workspace restore requires between 1 and 1000 paths")
	}
	result := append([]string(nil), paths...)
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i]) < strings.ToLower(result[j]) })
	for i, path := range result {
		if workspace.ValidateRelativePath(path) != nil || (i > 0 && strings.EqualFold(result[i-1], path)) {
			return nil, domain.InvalidArgument("paths", "Workspace restore paths must be unique normalized relative paths")
		}
		if i > 0 && strings.HasPrefix(strings.ToLower(path), strings.ToLower(result[i-1])+"/") {
			return nil, domain.InvalidArgument("paths", "Workspace restore paths cannot overlap")
		}
	}
	return result, nil
}

func diffWorkspaceSnapshots(fromFiles []workspace.File, fromDirectories []string, fromDirectoryMetadata []workspace.Directory,
	toFiles []workspace.File, toDirectories []string, toDirectoryMetadata []workspace.Directory) ([]WorkspaceDiffChange, error) {
	makeSnapshot := func(files []workspace.File, directories []string, directoryMetadata []workspace.Directory) (map[string]workspaceDiffSnapshotEntry, error) {
		entries := make(map[string]workspaceDiffSnapshotEntry, len(files)+len(directories)+len(directoryMetadata))
		for _, file := range files {
			if err := workspace.ValidateFile(file); err != nil {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains an invalid file")
			}
			copy := file
			size := file.SizeBytes
			key := strings.ToLower(file.Path)
			if _, exists := entries[key]; exists {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains a duplicate path")
			}
			mode := file.Mode
			kind := file.Kind
			if kind == "" {
				kind = "file"
			}
			version := &WorkspaceDiffVersion{Kind: kind, Path: file.Path, SizeBytes: &size, SHA256: file.SHA256, Mode: &mode}
			if kind == "symlink" {
				version.SymlinkTarget = file.SymlinkTarget
			}
			entries[key] = workspaceDiffSnapshotEntry{version: version, file: &copy}
		}
		for _, path := range directories {
			if workspace.ValidateRelativePath(path) != nil {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains an invalid directory")
			}
			key := strings.ToLower(path)
			if entry, exists := entries[key]; exists {
				if entry.version.Kind != "directory" {
					return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains a duplicate path")
				}
				continue
			}
			entries[key] = workspaceDiffSnapshotEntry{version: &WorkspaceDiffVersion{Kind: "directory", Path: path}}
		}
		seenDirectoryMetadata := make(map[string]struct{}, len(directoryMetadata))
		for _, directory := range directoryMetadata {
			if workspace.ValidateDirectory(directory, false) != nil {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains invalid directory metadata")
			}
			key := strings.ToLower(directory.Path)
			if _, exists := seenDirectoryMetadata[key]; exists {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains duplicate directory metadata")
			}
			seenDirectoryMetadata[key] = struct{}{}
			if entry, exists := entries[key]; exists && entry.version.Kind != "directory" {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot contains a duplicate path")
			}
			copy := directory
			version := &WorkspaceDiffVersion{Kind: "directory", Path: directory.Path, Mode: directory.Mode, ModTimeNS: directory.ModTimeNS}
			entries[key] = workspaceDiffSnapshotEntry{version: version, directory: &copy}
		}
		return entries, nil
	}
	before, err := makeSnapshot(fromFiles, fromDirectories, fromDirectoryMetadata)
	if err != nil {
		return nil, err
	}
	after, err := makeSnapshot(toFiles, toDirectories, toDirectoryMetadata)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	changes := make([]WorkspaceDiffChange, 0)
	for _, key := range ordered {
		old, hadOld := before[key]
		current, hasCurrent := after[key]
		if hadOld && hasCurrent && sameWorkspaceDiffEntry(old, current) {
			continue
		}
		change := WorkspaceDiffChange{DisplayStatus: "not_applicable"}
		if hadOld {
			change.Before = old.version
			change.Path = old.version.Path
		}
		if hasCurrent {
			change.After = current.version
			change.Path = current.version.Path
		}
		switch {
		case !hadOld:
			change.Action = "added"
		case !hasCurrent:
			change.Action = "removed"
		case old.version.Kind == current.version.Kind && old.version.Path != current.version.Path:
			change.Action = "renamed"
		default:
			change.Action = "changed"
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func sameWorkspaceDiffEntry(left, right workspaceDiffSnapshotEntry) bool {
	return reflect.DeepEqual(left.version, right.version) &&
		(left.file == nil && right.file == nil || left.file != nil && right.file != nil && reflect.DeepEqual(*left.file, *right.file))
}

func workspaceDiffFilesByPath(files []workspace.File) map[string]workspace.File {
	result := make(map[string]workspace.File, len(files))
	for _, file := range files {
		result[strings.ToLower(file.Path)] = file
	}
	return result
}

func readWorkspaceDiffText(ctx context.Context, blocks WorkspaceBlockReader, workspaceID string, file workspace.File) (string, bool, error) {
	if file.Kind == "symlink" || file.SizeBytes > maxWorkspaceDiffTextFileBytes || workspace.ValidateFile(file) != nil {
		return "", false, nil
	}
	var content bytes.Buffer
	content.Grow(int(file.SizeBytes))
	hasher := sha256.New()
	for _, chunk := range file.Chunks {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		data, err := readVerifiedBlock(blocks, workspaceID, chunk)
		if err != nil {
			return "", false, hostWorkspaceBlockReadFailure(err)
		}
		if int64(content.Len()) > file.SizeBytes-int64(len(data)) {
			return "", false, domain.NewError(domain.CodeWorkspaceConflict, "Workspace diff file chunks exceed the manifest size")
		}
		_, _ = content.Write(data)
		_, _ = hasher.Write(data)
	}
	data := content.Bytes()
	if int64(len(data)) != file.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
		return "", false, domain.NewError(domain.CodeWorkspaceConflict, "Workspace diff file digest does not match its manifest")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", false, nil
	}
	return string(data), true, nil
}
