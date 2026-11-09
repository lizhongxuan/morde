package runtimefiles

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"morde-runtime/contract"
	mordesync "morde-sync"
)

const (
	TaskWorkspaceManifestPageFilesMax = 1000
	maxTaskWorkspaceManifestPageBytes = 4 << 20
	maxTaskWorkspaceSnapshotBytes     = maxTaskViewManifestBytes + 4096
)

// TaskWorkspaceManifestChunk is one content-addressed morde-sync block.
type TaskWorkspaceManifestChunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// TaskWorkspaceManifestFile contains only workspace-relative metadata.
type TaskWorkspaceManifestFile struct {
	Path          string                        `json:"path"`
	Kind          string                        `json:"kind"`
	SymlinkTarget string                        `json:"symlink_target,omitempty"`
	HardlinkTo    string                        `json:"hardlink_to,omitempty"`
	SizeBytes     int64                         `json:"size_bytes"`
	SHA256        string                        `json:"sha256"`
	Mode          uint32                        `json:"mode"`
	ModTimeNS     *int64                        `json:"mod_time_ns,omitempty"`
	MetadataKnown bool                          `json:"metadata_known,omitempty"`
	Xattrs        []mordesync.ExtendedAttribute `json:"xattrs,omitempty"`
	ACL           *mordesync.NativeACL          `json:"acl,omitempty"`
	Chunks        []TaskWorkspaceManifestChunk  `json:"chunks"`
}

type TaskWorkspaceManifestDirectory struct {
	Path          string                        `json:"path"`
	Mode          uint32                        `json:"mode"`
	ModTimeNS     *int64                        `json:"mod_time_ns,omitempty"`
	MetadataKnown bool                          `json:"metadata_known,omitempty"`
	Xattrs        []mordesync.ExtendedAttribute `json:"xattrs,omitempty"`
	ACL           *mordesync.NativeACL          `json:"acl,omitempty"`
}

// TaskWorkspaceManifestPage is a bounded page from one immutable manifest
// snapshot of a Task's private Workspace view. An empty manifestSHA256 starts
// a fresh snapshot; subsequent pages must pass the returned digest.
type TaskWorkspaceManifestPage struct {
	TaskID                      string                           `json:"task_id"`
	WorkspaceID                 string                           `json:"workspace_id"`
	BindingVersion              int64                            `json:"binding_version"`
	InputRevision               int64                            `json:"input_revision"`
	WorkspaceCheckpointRevision int64                            `json:"workspace_checkpoint_revision"`
	InputManifestSHA256         string                           `json:"input_manifest_sha256"`
	ViewID                      string                           `json:"view_id"`
	ManifestSHA256              string                           `json:"manifest_sha256"`
	Files                       []TaskWorkspaceManifestFile      `json:"files"`
	EmptyDirectories            []string                         `json:"empty_directories"`
	Directories                 []TaskWorkspaceManifestDirectory `json:"directories"`
	NextCursor                  string                           `json:"next_cursor,omitempty"`
}

type taskWorkspaceManifestSnapshot struct {
	Metadata         persistedTaskView       `json:"metadata"`
	ManifestSHA256   string                  `json:"manifest_sha256"`
	Files            []taskManifestFile      `json:"files"`
	EmptyDirectories []string                `json:"empty_directories"`
	Directories      []taskManifestDirectory `json:"directories"`
}

type taskWorkspaceBlockLocation struct {
	Path   string
	Offset int64
	Size   int64
}

type taskWorkspaceManifestEntry struct {
	Path      string
	FileIndex int
	Directory bool
}

func (w *Workspace) ExportTaskWorkspaceManifestPage(ctx context.Context, request contract.TaskWorkspaceManifestRequest) (contract.TaskWorkspaceManifestResponse, error) {
	page, err := w.TaskWorkspaceManifestPage(ctx, request.TaskID, request.ViewID, request.ManifestSHA256, request.AfterPath, request.Limit)
	if err != nil {
		return contract.TaskWorkspaceManifestResponse{}, err
	}
	if page.WorkspaceID != request.WorkspaceID || page.BindingVersion != request.BindingVersion || page.InputRevision != request.InputRevision ||
		page.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision || page.InputManifestSHA256 != request.InputManifestSHA256 {
		return contract.TaskWorkspaceManifestResponse{}, ErrTaskWorkspaceView
	}
	files := make([]contract.TaskWorkspaceManifestFile, len(page.Files))
	for index, file := range page.Files {
		chunks := make([]contract.TaskWorkspaceManifestChunk, len(file.Chunks))
		for chunkIndex, chunk := range file.Chunks {
			chunks[chunkIndex] = contract.TaskWorkspaceManifestChunk{SHA256: chunk.SHA256, SizeBytes: chunk.SizeBytes}
		}
		files[index] = contract.TaskWorkspaceManifestFile{
			Path: file.Path, Kind: file.Kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, ModTimeNS: file.ModTimeNS,
			MetadataKnown: file.MetadataKnown, Xattrs: contractXattrs(file.Xattrs), ACL: contractACL(file.ACL), Chunks: chunks,
		}
	}
	directories := make([]contract.TaskWorkspaceManifestDirectory, len(page.Directories))
	for index, directory := range page.Directories {
		directories[index] = contract.TaskWorkspaceManifestDirectory{
			Path: directory.Path, Mode: directory.Mode, ModTimeNS: directory.ModTimeNS,
			MetadataKnown: directory.MetadataKnown, Xattrs: contractXattrs(directory.Xattrs), ACL: contractACL(directory.ACL),
		}
	}
	return contract.TaskWorkspaceManifestResponse{
		TaskID: page.TaskID, WorkspaceID: page.WorkspaceID, BindingVersion: page.BindingVersion,
		InputRevision: page.InputRevision, WorkspaceCheckpointRevision: page.WorkspaceCheckpointRevision,
		InputManifestSHA256: page.InputManifestSHA256, ViewID: page.ViewID, ManifestSHA256: page.ManifestSHA256,
		Files: files, EmptyDirectories: append([]string{}, page.EmptyDirectories...), Directories: directories, NextCursor: page.NextCursor,
	}, nil
}

func contractXattrs(xattrs []mordesync.ExtendedAttribute) []contract.WorkspaceExtendedAttribute {
	converted := make([]contract.WorkspaceExtendedAttribute, len(xattrs))
	for index, attribute := range xattrs {
		converted[index] = contract.WorkspaceExtendedAttribute{Name: attribute.Name, Value: append([]byte(nil), attribute.Value...)}
	}
	return converted
}

func contractACL(acl *mordesync.NativeACL) *contract.WorkspaceNativeACL {
	if acl == nil {
		return nil
	}
	return &contract.WorkspaceNativeACL{Platform: acl.Platform, Format: acl.Format, Data: append([]byte(nil), acl.Data...)}
}

// TaskWorkspaceManifestPage returns one bounded page. The only caller-supplied
// path-like value is a cursor, which must match a path in the saved manifest.
func (w *Workspace) TaskWorkspaceManifestPage(ctx context.Context, taskID, viewID, manifestSHA256, afterPath string, limit int) (TaskWorkspaceManifestPage, error) {
	if ctx == nil || !validTaskWorkspaceDirectory(taskID, viewID) || limit < 1 || limit > TaskWorkspaceManifestPageFilesMax || manifestSHA256 != "" && !isLowerSHA256(manifestSHA256) || len(afterPath) > 4096 {
		return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
	}
	if err := ctx.Err(); err != nil {
		return TaskWorkspaceManifestPage{}, err
	}
	if manifestSHA256 == "" && afterPath != "" {
		return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
	}

	var snapshot taskWorkspaceManifestSnapshot
	if manifestSHA256 == "" {
		w.taskViewMu.Lock()
		defer w.taskViewMu.Unlock()
		var err error
		snapshot, err = w.createTaskWorkspaceManifestSnapshot(ctx, taskID, viewID)
		if err != nil {
			return TaskWorkspaceManifestPage{}, err
		}
	} else {
		taskRoot, _, metadata, err := w.openTaskWorkspaceView(taskID, viewID)
		if err != nil {
			return TaskWorkspaceManifestPage{}, err
		}
		defer taskRoot.Close()
		snapshot, err = readTaskWorkspaceManifestSnapshot(taskRoot, viewID)
		if err != nil || snapshot.Metadata != metadata || snapshot.ManifestSHA256 != manifestSHA256 {
			return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
		}
		if err := validateTaskWorkspaceManifestSnapshot(&snapshot); err != nil {
			return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
		}
	}
	if err := ctx.Err(); err != nil {
		return TaskWorkspaceManifestPage{}, err
	}
	return pageTaskWorkspaceManifest(snapshot, afterPath, limit)
}

// ReadTaskWorkspaceBlock returns bytes only for a block listed by the exact
// saved output manifest. It never accepts a filesystem path from the caller.
func (w *Workspace) ReadTaskWorkspaceBlock(ctx context.Context, taskID, viewID, manifestSHA256, blockSHA256 string) ([]byte, error) {
	if ctx == nil || !validTaskWorkspaceDirectory(taskID, viewID) || !isLowerSHA256(manifestSHA256) || !isLowerSHA256(blockSHA256) {
		return nil, ErrTaskWorkspaceView
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taskRoot, viewRoot, metadata, err := w.openTaskWorkspaceView(taskID, viewID)
	if err != nil {
		return nil, err
	}
	defer taskRoot.Close()
	defer viewRoot.Close()
	snapshot, err := readTaskWorkspaceManifestSnapshot(taskRoot, viewID)
	if err != nil || snapshot.Metadata != metadata || snapshot.ManifestSHA256 != manifestSHA256 {
		return nil, ErrTaskWorkspaceView
	}
	if err := validateTaskWorkspaceManifestSnapshot(&snapshot); err != nil {
		return nil, ErrTaskWorkspaceView
	}
	locations := taskWorkspaceBlockLocations(snapshot.Files, blockSHA256)
	if len(locations) == 0 {
		return nil, ErrTaskWorkspaceView
	}
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, readErr := readTaskWorkspaceBlock(viewRoot, location)
		if readErr != nil || len(data) != int(location.Size) {
			continue
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) == blockSHA256 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	return nil, ErrTaskWorkspaceView
}

func (w *Workspace) createTaskWorkspaceManifestSnapshot(ctx context.Context, taskID, viewID string) (taskWorkspaceManifestSnapshot, error) {
	taskRoot, viewRoot, metadata, err := w.openTaskWorkspaceView(taskID, viewID)
	if err != nil {
		return taskWorkspaceManifestSnapshot{}, err
	}
	defer taskRoot.Close()
	defer viewRoot.Close()
	viewInfo, err := viewRoot.Stat(".")
	if err != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	manifest, digest, err := scanTaskWorkspace(ctx, filepath.Join(w.taskRootPath, taskID, viewID, taskWorkspaceContentDirectory))
	if err != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	if err := ctx.Err(); err != nil {
		return taskWorkspaceManifestSnapshot{}, err
	}
	currentTaskRoot, currentViewRoot, currentMetadata, err := w.openTaskWorkspaceView(taskID, viewID)
	if err != nil {
		return taskWorkspaceManifestSnapshot{}, err
	}
	defer currentTaskRoot.Close()
	defer currentViewRoot.Close()
	currentViewInfo, err := currentViewRoot.Stat(".")
	if err != nil || !os.SameFile(viewInfo, currentViewInfo) || currentMetadata != metadata {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	snapshot := taskWorkspaceManifestSnapshot{
		Metadata: metadata, ManifestSHA256: digest, Files: taskManifestFiles(manifest),
		EmptyDirectories: append([]string{}, manifest.EmptyDirectories...), Directories: taskManifestDirectories(manifest),
	}
	if err := validateTaskWorkspaceManifestSnapshot(&snapshot); err != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	if err := writeTaskWorkspaceManifestSnapshot(taskRoot, viewID, snapshot); err != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	return snapshot, nil
}

func (w *Workspace) openTaskWorkspaceView(taskID, viewID string) (*os.Root, *os.Root, persistedTaskView, error) {
	if w.taskRoot == nil || !validTaskWorkspaceDirectory(taskID, viewID) {
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	root := w.taskRoot
	w.mu.RUnlock()
	taskInfo, err := root.Lstat(taskID)
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode()&os.ModeSymlink != 0 || taskInfo.Mode().Perm() != 0o700 || !ownedBy(taskInfo, uint32(os.Geteuid())) {
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	taskRoot, err := openWorkspaceRoot(root, taskID, taskInfo)
	if err != nil {
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	viewInfo, err := taskRoot.Lstat(viewID)
	if err != nil || !viewInfo.IsDir() || viewInfo.Mode()&os.ModeSymlink != 0 || viewInfo.Mode().Perm() != 0o770 || !ownedBy(viewInfo, w.taskViewUID) {
		_ = taskRoot.Close()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	viewContainer, err := openWorkspaceRoot(taskRoot, viewID, viewInfo)
	if err != nil {
		_ = taskRoot.Close()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	contentInfo, err := viewContainer.Lstat(taskWorkspaceContentDirectory)
	if err != nil || !contentInfo.IsDir() || contentInfo.Mode()&os.ModeSymlink != 0 || !ownedBy(contentInfo, w.taskViewUID) {
		_ = taskRoot.Close()
		_ = viewContainer.Close()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	viewRoot, err := openWorkspaceRoot(viewContainer, taskWorkspaceContentDirectory, contentInfo)
	_ = viewContainer.Close()
	if err != nil {
		_ = taskRoot.Close()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	metadata, exists, err := readTaskViewMetadata(taskRoot, viewID)
	if err != nil || !exists || !validPersistedTaskView(metadata, taskID, viewID) {
		_ = taskRoot.Close()
		_ = viewRoot.Close()
		return nil, nil, persistedTaskView{}, ErrTaskWorkspaceView
	}
	return taskRoot, viewRoot, metadata, nil
}

func validPersistedTaskView(metadata persistedTaskView, taskID, viewID string) bool {
	return metadata.TaskID == taskID && metadata.ViewID == viewID && validHostDirectory(metadata.HostID) &&
		validWorkspaceDirectory(metadata.WorkspaceID) && metadata.BindingVersion >= 1 && metadata.InputRevision >= 0 &&
		metadata.WorkspaceCheckpointRevision >= metadata.InputRevision && isLowerSHA256(metadata.ManifestSHA256)
}

func readTaskWorkspaceManifestSnapshot(root *os.Root, viewID string) (taskWorkspaceManifestSnapshot, error) {
	name := viewID + ".manifest"
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ownedBy(info, uint32(os.Geteuid())) || info.Size() < 2 || info.Size() > maxTaskWorkspaceSnapshotBytes {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	file, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(file), maxTaskWorkspaceSnapshotBytes+1))
	decoder.DisallowUnknownFields()
	var snapshot taskWorkspaceManifestSnapshot
	if err := decoder.Decode(&snapshot); err != nil || ensureMetadataEOF(decoder) != nil {
		return taskWorkspaceManifestSnapshot{}, ErrTaskWorkspaceView
	}
	return snapshot, nil
}

func writeTaskWorkspaceManifestSnapshot(root *os.Root, viewID string, snapshot taskWorkspaceManifestSnapshot) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > maxTaskWorkspaceSnapshotBytes {
		return ErrTaskWorkspaceView
	}
	temporary := "." + viewID + ".manifest.tmp"
	if info, err := root.Lstat(temporary); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ownedBy(info, uint32(os.Geteuid())) {
			return ErrTaskWorkspaceView
		}
		if err := root.Remove(temporary); err != nil {
			return ErrTaskWorkspaceView
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrTaskWorkspaceView
	}
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrTaskWorkspaceView
	}
	_, writeErr := file.Write(encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = root.Remove(temporary)
		return ErrTaskWorkspaceView
	}
	if err := root.Rename(temporary, viewID+".manifest"); err != nil {
		_ = root.Remove(temporary)
		return ErrTaskWorkspaceView
	}
	directory, err := root.Open(".")
	if err != nil {
		return ErrTaskWorkspaceView
	}
	err = directory.Sync()
	_ = directory.Close()
	return err
}

func validateTaskWorkspaceManifestSnapshot(snapshot *taskWorkspaceManifestSnapshot) error {
	if snapshot == nil || !validTaskWorkspaceDirectory(snapshot.Metadata.TaskID, snapshot.Metadata.ViewID) ||
		!validPersistedTaskView(snapshot.Metadata, snapshot.Metadata.TaskID, snapshot.Metadata.ViewID) ||
		!isLowerSHA256(snapshot.ManifestSHA256) || len(snapshot.Files)+max(len(snapshot.EmptyDirectories), len(snapshot.Directories)) > maxTaskViewFiles || snapshot.Files == nil {
		return ErrTaskWorkspaceView
	}
	if snapshot.EmptyDirectories == nil {
		snapshot.EmptyDirectories = []string{}
	}
	manifest := mordesync.Manifest{
		Files: make([]mordesync.File, len(snapshot.Files)), EmptyDirectories: append([]string{}, snapshot.EmptyDirectories...),
		Directories: make([]mordesync.Directory, len(snapshot.Directories)),
	}
	for fileIndex, file := range snapshot.Files {
		if file.Chunks == nil {
			return ErrTaskWorkspaceView
		}
		blocks := make([]mordesync.Block, len(file.Chunks))
		for chunkIndex, chunk := range file.Chunks {
			blocks[chunkIndex] = mordesync.Block{
				Offset: int64(chunkIndex) * mordesync.BlockSize, Size: chunk.SizeBytes, SHA256: chunk.SHA256,
			}
		}
		manifest.Files[fileIndex] = mordesync.File{
			Path: file.Path, Kind: file.Kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			MetadataKnown: file.MetadataKnown, Xattrs: file.Xattrs, ACL: file.ACL,
			Size: file.SizeBytes, Mode: file.Mode, SHA256: file.SHA256, Blocks: blocks,
		}
		if file.ModTimeNS != nil {
			manifest.Files[fileIndex].ModTimeNS = *file.ModTimeNS
			manifest.Files[fileIndex].ModTimeKnown = true
		}
		if file.SizeBytes > maxTaskViewBytes-manifest.TotalBytes {
			return ErrTaskWorkspaceView
		}
		manifest.TotalBytes += file.SizeBytes
	}
	for index, directory := range snapshot.Directories {
		if directory.Mode == nil {
			return ErrTaskWorkspaceView
		}
		manifest.Directories[index] = mordesync.Directory{
			Path: directory.Path, Mode: *directory.Mode, ModeKnown: true,
			MetadataKnown: directory.MetadataKnown, Xattrs: directory.Xattrs, ACL: directory.ACL,
		}
		if directory.ModTimeNS != nil {
			manifest.Directories[index].ModTimeNS = *directory.ModTimeNS
			manifest.Directories[index].ModTimeKnown = true
		}
	}
	manifest.FileCount = int64(len(manifest.Files))
	digest, err := taskManifestSHA256(manifest)
	if err != nil || digest != snapshot.ManifestSHA256 {
		return ErrTaskWorkspaceView
	}
	return nil
}

func pageTaskWorkspaceManifest(snapshot taskWorkspaceManifestSnapshot, afterPath string, limit int) (TaskWorkspaceManifestPage, error) {
	entries := taskWorkspaceManifestEntries(snapshot)
	start := 0
	if afterPath != "" {
		index := sort.Search(len(entries), func(index int) bool { return entries[index].Path >= afterPath })
		if index == len(entries) || entries[index].Path != afterPath || index+1 == len(entries) {
			return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
		}
		start = index + 1
	}
	end := min(start+limit, len(entries))
	for {
		page, err := taskWorkspaceManifestPageRange(snapshot, entries[start:end], end < len(entries), start == 0)
		if err != nil {
			return TaskWorkspaceManifestPage{}, err
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
		}
		if len(encoded) <= maxTaskWorkspaceManifestPageBytes {
			return page, nil
		}
		if end-start <= 1 {
			return TaskWorkspaceManifestPage{}, ErrTaskWorkspaceView
		}
		end = start + (end-start)/2
	}
}

func taskWorkspaceManifestEntries(snapshot taskWorkspaceManifestSnapshot) []taskWorkspaceManifestEntry {
	entries := make([]taskWorkspaceManifestEntry, 0, len(snapshot.Files)+len(snapshot.EmptyDirectories))
	for index, file := range snapshot.Files {
		entries = append(entries, taskWorkspaceManifestEntry{Path: file.Path, FileIndex: index})
	}
	for _, directory := range snapshot.EmptyDirectories {
		entries = append(entries, taskWorkspaceManifestEntry{Path: directory, Directory: true})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

func taskWorkspaceManifestPageRange(snapshot taskWorkspaceManifestSnapshot, entries []taskWorkspaceManifestEntry, hasMore, includeRoot bool) (TaskWorkspaceManifestPage, error) {
	files := make([]TaskWorkspaceManifestFile, 0, len(entries))
	emptyDirectories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Directory {
			emptyDirectories = append(emptyDirectories, entry.Path)
			continue
		}
		file := snapshot.Files[entry.FileIndex]
		chunks := make([]TaskWorkspaceManifestChunk, len(file.Chunks))
		for chunkIndex, chunk := range file.Chunks {
			chunks[chunkIndex] = TaskWorkspaceManifestChunk{SHA256: chunk.SHA256, SizeBytes: chunk.SizeBytes}
		}
		kind := file.Kind
		if kind == "" {
			kind = "file"
		}
		files = append(files, TaskWorkspaceManifestFile{
			Path: file.Path, Kind: kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, ModTimeNS: file.ModTimeNS,
			MetadataKnown: file.MetadataKnown, Xattrs: file.Xattrs, ACL: file.ACL, Chunks: chunks,
		})
	}
	directories, err := taskWorkspaceManifestDirectoriesForEntries(snapshot.Directories, entries, includeRoot)
	if err != nil {
		return TaskWorkspaceManifestPage{}, err
	}
	page := TaskWorkspaceManifestPage{
		TaskID: snapshot.Metadata.TaskID, WorkspaceID: snapshot.Metadata.WorkspaceID,
		BindingVersion: snapshot.Metadata.BindingVersion, InputRevision: snapshot.Metadata.InputRevision,
		WorkspaceCheckpointRevision: snapshot.Metadata.WorkspaceCheckpointRevision,
		InputManifestSHA256:         snapshot.Metadata.ManifestSHA256, ViewID: snapshot.Metadata.ViewID,
		ManifestSHA256: snapshot.ManifestSHA256, Files: files, EmptyDirectories: emptyDirectories, Directories: directories,
	}
	if hasMore && len(entries) > 0 {
		page.NextCursor = entries[len(entries)-1].Path
	}
	return page, nil
}

func taskWorkspaceManifestDirectoriesForEntries(all []taskManifestDirectory, entries []taskWorkspaceManifestEntry, includeRoot bool) ([]TaskWorkspaceManifestDirectory, error) {
	if len(all) == 0 {
		return []TaskWorkspaceManifestDirectory{}, nil
	}
	wanted := make(map[string]struct{})
	addDirectory := func(value string, includeSelf bool) {
		if value == "." {
			wanted["."] = struct{}{}
			return
		}
		current := value
		if includeSelf {
			wanted[current] = struct{}{}
		}
		for {
			separator := strings.LastIndexByte(current, '/')
			if separator < 0 {
				wanted["."] = struct{}{}
				return
			}
			current = current[:separator]
			wanted[current] = struct{}{}
		}
	}
	for _, entry := range entries {
		addDirectory(entry.Path, entry.Directory)
	}
	if includeRoot {
		wanted["."] = struct{}{}
	}
	result := make([]TaskWorkspaceManifestDirectory, 0, len(wanted))
	for _, directory := range all {
		if _, ok := wanted[directory.Path]; !ok {
			continue
		}
		if directory.Mode == nil {
			return nil, ErrTaskWorkspaceView
		}
		result = append(result, TaskWorkspaceManifestDirectory{
			Path: directory.Path, Mode: *directory.Mode, ModTimeNS: directory.ModTimeNS,
			MetadataKnown: directory.MetadataKnown, Xattrs: directory.Xattrs, ACL: directory.ACL,
		})
	}
	if len(result) != len(wanted) {
		return nil, ErrTaskWorkspaceView
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func taskManifestFiles(manifest mordesync.Manifest) []taskManifestFile {
	files := make([]taskManifestFile, len(manifest.Files))
	for fileIndex, file := range manifest.Files {
		chunks := make([]taskManifestChunk, len(file.Blocks))
		for chunkIndex, block := range file.Blocks {
			chunks[chunkIndex] = taskManifestChunk{SHA256: block.SHA256, SizeBytes: block.Size}
		}
		kind := file.Kind
		if kind == "file" {
			kind = ""
		}
		files[fileIndex] = taskManifestFile{
			Path: file.Path, Kind: kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			SizeBytes: file.Size, SHA256: file.SHA256, Mode: file.Mode, MetadataKnown: file.MetadataKnown,
			Xattrs: file.Xattrs, ACL: file.ACL, Chunks: chunks,
		}
		if file.ModTimeKnown {
			modified := file.ModTimeNS
			files[fileIndex].ModTimeNS = &modified
		}
	}
	return files
}

func taskManifestDirectories(manifest mordesync.Manifest) []taskManifestDirectory {
	directories := make([]taskManifestDirectory, len(manifest.Directories))
	for index, directory := range manifest.Directories {
		var mode *uint32
		if directory.ModeKnown {
			value := directory.Mode
			mode = &value
		}
		var modified *int64
		if directory.ModTimeKnown {
			value := directory.ModTimeNS
			modified = &value
		}
		directories[index] = taskManifestDirectory{
			Path: directory.Path, Mode: mode, ModTimeNS: modified, MetadataKnown: directory.MetadataKnown,
			Xattrs: directory.Xattrs, ACL: directory.ACL,
		}
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })
	return directories
}

func taskWorkspaceBlockLocations(files []taskManifestFile, digest string) []taskWorkspaceBlockLocation {
	var locations []taskWorkspaceBlockLocation
	for _, file := range files {
		if file.Kind == "symlink" {
			continue
		}
		for index, chunk := range file.Chunks {
			if chunk.SHA256 == digest {
				locations = append(locations, taskWorkspaceBlockLocation{
					Path: file.Path, Offset: int64(index) * mordesync.BlockSize, Size: chunk.SizeBytes,
				})
			}
		}
	}
	return locations
}

func readTaskWorkspaceBlock(viewRoot *os.Root, location taskWorkspaceBlockLocation) ([]byte, error) {
	parts := strings.Split(location.Path, "/")
	if location.Size <= 0 || location.Size > mordesync.BlockSize || len(parts) == 0 {
		return nil, ErrTaskWorkspaceView
	}
	current := viewRoot
	var opened []*os.Root
	defer func() {
		for index := len(opened) - 1; index >= 0; index-- {
			_ = opened[index].Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, ErrTaskWorkspaceView
		}
		info, err := current.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrTaskWorkspaceView
		}
		child, err := openWorkspaceRoot(current, part, info)
		if err != nil {
			return nil, ErrTaskWorkspaceView
		}
		opened = append(opened, child)
		current = child
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return nil, ErrTaskWorkspaceView
	}
	info, err := current.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrTaskWorkspaceView
	}
	file, err := current.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, ErrTaskWorkspaceView
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || location.Offset < 0 || location.Offset > openedInfo.Size() || location.Size > openedInfo.Size()-location.Offset {
		return nil, ErrTaskWorkspaceView
	}
	data := make([]byte, int(location.Size))
	read, err := file.ReadAt(data, location.Offset)
	if err != nil && !errors.Is(err, io.EOF) || read != len(data) {
		return nil, ErrTaskWorkspaceView
	}
	return data, nil
}
