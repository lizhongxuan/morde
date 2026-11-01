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
	"syscall"

	mordesync "morde-sync"
)

const (
	maxTaskViewFiles               = 100_000
	maxTaskViewBytes         int64 = 5 << 30
	maxTaskViewManifestBytes       = 16 << 20
)

var ErrTaskWorkspaceView = errors.New("Task Workspace view unavailable")

type taskManifestChunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type taskManifestFile struct {
	Path      string              `json:"path"`
	SizeBytes int64               `json:"size_bytes"`
	SHA256    string              `json:"sha256"`
	Mode      uint32              `json:"mode"`
	Chunks    []taskManifestChunk `json:"chunks"`
}

type persistedTaskView struct {
	HostID                      string `json:"host_id"`
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	ManifestSHA256              string `json:"manifest_sha256"`
	ViewID                      string `json:"view_id"`
}

func ensurePrivateDirectory(directory string) error {
	parent := filepath.Dir(directory)
	parentInfo, err := os.Lstat(parent)
	parentMode := os.FileMode(0)
	if err == nil {
		parentMode = parentInfo.Mode()
	}
	// The Host Workspace mount is group-writable for gem; sticky prevents gem from replacing this root-owned child.
	groupWritableWithoutSticky := parentMode.Perm()&0o020 != 0 && parentMode&os.ModeSticky == 0
	if err != nil || !parentInfo.IsDir() || parentMode&os.ModeSymlink != 0 || parentMode.Perm()&0o002 != 0 || groupWritableWithoutSticky {
		return ErrInvalidWorkspace
	}
	if stat, ok := parentInfo.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrInvalidWorkspace
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return ErrInvalidWorkspace
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrInvalidWorkspace
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrInvalidWorkspace
	}
	return nil
}

func validTaskViewID(value string) bool {
	if len(value) != 65 || value[0] != 'v' {
		return false
	}
	for _, char := range value[1:] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validTaskWorkspaceDirectory(taskID, viewID string) bool {
	return len(taskID) == 8 && taskID[0] == 'q' && validIdentifierTail(taskID[1:]) && validTaskViewID(viewID)
}

func validIdentifierTail(value string) bool {
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

// PrepareTaskWorkspaceView copies only a server-identified Host Workspace
// snapshot into the Runtime's private persistent view root.
func (w *Workspace) PrepareTaskWorkspaceView(ctx context.Context, hostID, taskID, workspaceDirectory, viewID string, bindingVersion, inputRevision, checkpointRevision int64, manifestSHA256 string) error {
	if ctx == nil || !validTaskWorkspaceDirectory(taskID, viewID) || !validWorkspaceDirectory(workspaceDirectory) || !validHostDirectory(hostID) || bindingVersion < 1 || inputRevision < 0 || checkpointRevision < inputRevision || !isLowerSHA256(manifestSHA256) || w.taskRoot == nil {
		return ErrTaskWorkspaceView
	}
	w.taskViewMu.Lock()
	defer w.taskViewMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := w.taskRoot.Lstat(taskWorkspaceDeletionTombstoneName(taskID)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrTaskWorkspaceView
	}

	taskInfo, err := w.taskRoot.Lstat(taskID)
	if errors.Is(err, os.ErrNotExist) {
		if err := w.taskRoot.Mkdir(taskID, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrTaskWorkspaceView
		}
		taskInfo, err = w.taskRoot.Lstat(taskID)
	}
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode()&os.ModeSymlink != 0 || taskInfo.Mode().Perm() != 0o700 {
		return ErrTaskWorkspaceView
	}
	taskRoot, err := openWorkspaceRoot(w.taskRoot, taskID, taskInfo)
	if err != nil {
		return ErrTaskWorkspaceView
	}
	defer taskRoot.Close()

	persisted := persistedTaskView{
		HostID: hostID, TaskID: taskID, WorkspaceID: workspaceDirectory, BindingVersion: bindingVersion,
		InputRevision: inputRevision, WorkspaceCheckpointRevision: checkpointRevision,
		ManifestSHA256: manifestSHA256, ViewID: viewID,
	}
	metadata, hasMetadata, err := readTaskViewMetadata(taskRoot, viewID)
	if err != nil || hasMetadata && (metadata.HostID != persisted.HostID || metadata.TaskID != persisted.TaskID ||
		metadata.WorkspaceID != persisted.WorkspaceID || metadata.BindingVersion != persisted.BindingVersion ||
		metadata.InputRevision != persisted.InputRevision || metadata.ManifestSHA256 != persisted.ManifestSHA256 ||
		metadata.ViewID != persisted.ViewID || metadata.WorkspaceCheckpointRevision > persisted.WorkspaceCheckpointRevision) {
		return ErrTaskWorkspaceView
	}
	viewInfo, err := taskRoot.Lstat(viewID)
	if err == nil {
		if !viewInfo.IsDir() || viewInfo.Mode()&os.ModeSymlink != 0 {
			return ErrTaskWorkspaceView
		}
		if hasMetadata && viewInfo.Mode().Perm() == 0o770 && ownedBy(viewInfo, w.taskViewUID) {
			if metadata.WorkspaceCheckpointRevision < persisted.WorkspaceCheckpointRevision {
				return writeTaskViewMetadata(taskRoot, persisted)
			}
			return nil
		}
		// A crash before writing the sidecar leaves only an initial, unexposed
		// tree. Revalidate it before installing the durable identity record.
		if hasMetadata {
			return ErrTaskWorkspaceView
		}
		_, existingDigest, scanErr := scanTaskWorkspace(ctx, filepath.Join(w.taskRootPath, taskID, viewID))
		if scanErr != nil || existingDigest != manifestSHA256 || viewInfo.Mode().Perm() != 0o770 || !ownedBy(viewInfo, w.taskViewUID) {
			return ErrTaskWorkspaceView
		}
		return writeTaskViewMetadata(taskRoot, persisted)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ErrTaskWorkspaceView
	}
	entries, err := taskRoot.Open(".")
	if err != nil {
		return ErrTaskWorkspaceView
	}
	otherEntries, readErr := entries.ReadDir(-1)
	_ = entries.Close()
	if readErr != nil {
		return ErrTaskWorkspaceView
	}
	for _, entry := range otherEntries {
		if entry.IsDir() && entry.Name() != ".stage-"+viewID {
			return ErrTaskWorkspaceView
		}
		if strings.HasSuffix(entry.Name(), ".json") && entry.Name() != viewID+".json" {
			return ErrTaskWorkspaceView
		}
	}

	sourcePath := filepath.Join(w.rootPath, workspaceDirectory)
	sourceInfo, err := w.root.Lstat(workspaceDirectory)
	if err != nil || !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return ErrTaskWorkspaceView
	}
	sourceRoot, err := openWorkspaceRoot(w.root, workspaceDirectory, sourceInfo)
	if err != nil {
		return ErrTaskWorkspaceView
	}
	defer sourceRoot.Close()
	manifest, sourceDigest, err := scanTaskWorkspace(ctx, sourcePath)
	if err != nil || sourceDigest != manifestSHA256 {
		return ErrTaskWorkspaceView
	}

	stageName := ".stage-" + viewID
	_ = taskRoot.RemoveAll(stageName)
	if err := taskRoot.Mkdir(stageName, 0o700); err != nil {
		return ErrTaskWorkspaceView
	}
	stageInfo, err := taskRoot.Lstat(stageName)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		_ = taskRoot.RemoveAll(stageName)
		return ErrTaskWorkspaceView
	}
	stageRoot, err := openWorkspaceRoot(taskRoot, stageName, stageInfo)
	if err != nil {
		_ = taskRoot.RemoveAll(stageName)
		return ErrTaskWorkspaceView
	}
	stagePath := filepath.Join(w.taskRootPath, taskID, stageName)
	copyErr := copyTaskManifest(ctx, sourcePath, sourceRoot, stageRoot, stagePath, manifest, w.taskViewUID, w.taskViewGID)
	if copyErr == nil {
		_, copiedDigest, scanErr := scanTaskWorkspace(ctx, stagePath)
		if scanErr != nil || copiedDigest != manifestSHA256 {
			copyErr = ErrTaskWorkspaceView
		}
	}
	if copyErr == nil {
		copyErr = stageRoot.Chown(".", int(w.taskViewUID), int(w.taskViewGID))
	}
	if copyErr == nil {
		copyErr = stageRoot.Chmod(".", 0o770|os.ModeSetgid)
	}
	if copyErr == nil {
		_, sourceDigest, err = scanTaskWorkspace(ctx, sourcePath)
		if err != nil || sourceDigest != manifestSHA256 {
			copyErr = ErrTaskWorkspaceView
		}
	}
	closeErr := stageRoot.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = taskRoot.RemoveAll(stageName)
		return ErrTaskWorkspaceView
	}
	if err := taskRoot.Rename(stageName, viewID); err != nil {
		_ = taskRoot.RemoveAll(stageName)
		return ErrTaskWorkspaceView
	}
	return writeTaskViewMetadata(taskRoot, persisted)
}

func validHostDirectory(value string) bool {
	return len(value) == 8 && value[0] == 'h' && validIdentifierTail(value[1:])
}

func ownedBy(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}

func readTaskViewMetadata(root *os.Root, viewID string) (persistedTaskView, bool, error) {
	name := viewID + ".json"
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return persistedTaskView{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !ownedBy(info, uint32(os.Geteuid())) {
		return persistedTaskView{}, false, ErrTaskWorkspaceView
	}
	file, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return persistedTaskView{}, false, ErrTaskWorkspaceView
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(file), 4097))
	decoder.DisallowUnknownFields()
	var metadata persistedTaskView
	if err := decoder.Decode(&metadata); err != nil || ensureMetadataEOF(decoder) != nil || metadata.ViewID != viewID {
		return persistedTaskView{}, false, ErrTaskWorkspaceView
	}
	return metadata, true, nil
}

func ensureMetadataEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrTaskWorkspaceView
	}
	return nil
}

func writeTaskViewMetadata(root *os.Root, metadata persistedTaskView) error {
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > 4096 {
		return ErrTaskWorkspaceView
	}
	temporary := "." + metadata.ViewID + ".json.tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		_ = root.Remove(temporary)
		file, err = root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
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
	if err := root.Rename(temporary, metadata.ViewID+".json"); err != nil {
		_ = root.Remove(temporary)
		return ErrTaskWorkspaceView
	}
	directory, err := root.Open(".")
	if err != nil {
		return ErrTaskWorkspaceView
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return ErrTaskWorkspaceView
	}
	return nil
}

func scanTaskWorkspace(ctx context.Context, directory string) (mordesync.Manifest, string, error) {
	scanContext, cancel := context.WithCancel(ctx)
	defer cancel()
	tooLarge := false
	manifest, err := mordesync.Scan(scanContext, directory, func(progress mordesync.ScanProgress) {
		if progress.FilesFound > maxTaskViewFiles || progress.BytesFound > maxTaskViewBytes {
			tooLarge = true
			cancel()
		}
	})
	if err != nil {
		if tooLarge {
			return mordesync.Manifest{}, "", ErrTaskWorkspaceView
		}
		return mordesync.Manifest{}, "", err
	}
	if manifest.FileCount > maxTaskViewFiles || int64(len(manifest.EmptyDirectories)) > maxTaskViewFiles-manifest.FileCount || manifest.TotalBytes > maxTaskViewBytes {
		return mordesync.Manifest{}, "", ErrTaskWorkspaceView
	}
	digest, err := taskManifestSHA256(manifest)
	return manifest, digest, err
}

func taskManifestSHA256(manifest mordesync.Manifest) (string, error) {
	if err := mordesync.ValidateManifest(manifest); err != nil || manifest.FileCount != int64(len(manifest.Files)) || manifest.TotalBytes > maxTaskViewBytes ||
		manifest.FileCount > maxTaskViewFiles || int64(len(manifest.EmptyDirectories)) > maxTaskViewFiles-manifest.FileCount {
		return "", ErrTaskWorkspaceView
	}
	if len(manifest.EmptyDirectories) > 0 {
		return taskManifestTreeSHA256(manifest)
	}
	return taskManifestFilesSHA256(manifest)
}

func taskManifestFilesSHA256(manifest mordesync.Manifest) (string, error) {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("morde-workspace-manifest/v1\n"))
	_, _ = hasher.Write([]byte("["))
	encodedBytes := 2
	previousPath := ""
	for index, file := range manifest.Files {
		if index > 0 && file.Path <= previousPath {
			return "", ErrTaskWorkspaceView
		}
		chunks := make([]taskManifestChunk, 0, len(file.Blocks))
		for _, block := range file.Blocks {
			chunks = append(chunks, taskManifestChunk{SHA256: block.SHA256, SizeBytes: block.Size})
		}
		encoded, err := json.Marshal(taskManifestFile{
			Path: file.Path, SizeBytes: file.Size, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
		})
		if err != nil {
			return "", ErrTaskWorkspaceView
		}
		if index > 0 {
			encodedBytes++
		}
		encodedBytes += len(encoded)
		if encodedBytes > maxTaskViewManifestBytes {
			return "", ErrTaskWorkspaceView
		}
		if index > 0 {
			_, _ = hasher.Write([]byte(","))
		}
		_, _ = hasher.Write(encoded)
		previousPath = file.Path
	}
	_, _ = hasher.Write([]byte("]"))
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func taskManifestTreeSHA256(manifest mordesync.Manifest) (string, error) {
	files := taskManifestFiles(manifest)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	directories := append([]string(nil), manifest.EmptyDirectories...)
	sort.Strings(directories)

	hasher := sha256.New()
	_, _ = hasher.Write([]byte("morde-workspace-manifest/v2\n"))
	encodedBytes := 0
	write := func(value []byte) bool {
		if len(value) > maxTaskViewManifestBytes-encodedBytes {
			return false
		}
		encodedBytes += len(value)
		_, _ = hasher.Write(value)
		return true
	}
	if !write([]byte("{\"empty_directories\":[")) {
		return "", ErrTaskWorkspaceView
	}
	for index, directory := range directories {
		if index > 0 && !write([]byte(",")) {
			return "", ErrTaskWorkspaceView
		}
		encoded, err := json.Marshal(directory)
		if err != nil || !write(encoded) {
			return "", ErrTaskWorkspaceView
		}
	}
	if !write([]byte("],\"files\":[")) {
		return "", ErrTaskWorkspaceView
	}
	for index, file := range files {
		if index > 0 && !write([]byte(",")) {
			return "", ErrTaskWorkspaceView
		}
		encoded, err := json.Marshal(file)
		if err != nil || !write(encoded) {
			return "", ErrTaskWorkspaceView
		}
	}
	if !write([]byte("]}")) {
		return "", ErrTaskWorkspaceView
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyTaskManifest(ctx context.Context, sourcePath string, sourceRoot, stageRoot *os.Root, stagePath string, manifest mordesync.Manifest, uid, gid uint32) error {
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ensureTaskViewParents(stageRoot, filepath.ToSlash(filepath.Dir(file.Path)), uid, gid); err != nil {
			return err
		}
		source, err := sourceRoot.OpenFile(file.Path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		target, err := stageRoot.OpenFile(file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = source.Close()
			return err
		}
		blocks := make([]mordesync.BlockRef, len(file.Blocks))
		for index := range file.Blocks {
			blocks[index] = mordesync.BlockRef{File: file, Index: index}
		}
		copyErr := mordesync.ReadBlocks(ctx, sourcePath, blocks, func(_ mordesync.File, _ int, block mordesync.Block, data []byte) error {
			written, err := target.WriteAt(data, block.Offset)
			if err != nil {
				return err
			}
			if written != len(data) {
				return errors.New("short Task Workspace copy")
			}
			return nil
		})
		if copyErr == nil {
			copyErr = target.Truncate(file.Size)
		}
		if copyErr == nil {
			copyErr = target.Sync()
		}
		if copyErr == nil {
			copyErr = target.Chown(int(uid), int(gid))
		}
		if copyErr == nil {
			copyErr = target.Chmod(os.FileMode(file.Mode))
		}
		closeErr := errors.Join(source.Close(), target.Close())
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return copyErr
		}
	}
	for _, directory := range manifest.EmptyDirectories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ensureTaskViewParents(stageRoot, directory, uid, gid); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func ensureTaskViewParents(root *os.Root, directory string, uid, gid uint32) error {
	if directory == "." || directory == "" {
		return nil
	}
	current := root
	var opened []*os.Root
	defer func() {
		for index := len(opened) - 1; index >= 0; index-- {
			_ = opened[index].Close()
		}
	}()
	for _, part := range strings.Split(directory, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrTaskWorkspaceView
		}
		if err := current.Mkdir(part, 0o770); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := current.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrTaskWorkspaceView
		}
		child, err := openWorkspaceRoot(current, part, info)
		if err != nil {
			return ErrTaskWorkspaceView
		}
		opened = append(opened, child)
		if err := child.Chown(".", int(uid), int(gid)); err != nil {
			return err
		}
		if err := child.Chmod(".", 0o770|os.ModeSetgid); err != nil {
			return err
		}
		current = child
	}
	return nil
}
