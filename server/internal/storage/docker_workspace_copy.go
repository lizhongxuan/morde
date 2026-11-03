package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	mordesync "morde-sync"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

var workspaceCopyReservationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

type WorkspaceVolumeCopyRequest = app.WorkspaceVolumeCopyRequest

// WorkspaceVolumeCopyReservation must check a committed Store reservation
// that blocks new Workspace bindings and Host starts until the copy completes.
type WorkspaceVolumeCopyReservation = app.WorkspaceVolumeCopyReservationChecker

type dockerWorkspaceCopyVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	Mountpoint string            `json:"Mountpoint"`
}

// ValidateWorkspaceVolumeCopySource checks for unsupported source entries before
// the caller moves a new reservation into Copying or creates the destination.
func (d *Docker) ValidateWorkspaceVolumeCopySource(ctx context.Context, request WorkspaceVolumeCopyRequest,
	reservation WorkspaceVolumeCopyReservation) error {
	if d == nil || ctx == nil || reservation == nil ||
		!workspaceCopyReservationID.MatchString(request.ReservationID) ||
		strings.TrimSpace(request.ServiceInstanceID) == "" || len(request.ServiceInstanceID) > 128 ||
		!domain.ValidUserID(request.OwnerID) || !domain.ValidWorkspaceID(request.SourceWorkspaceID) ||
		!domain.ValidWorkspaceID(request.TargetWorkspaceID) || request.SourceWorkspaceID == request.TargetWorkspaceID ||
		request.SourceRevision < 0 || request.Files == nil || request.EmptyDirectories == nil || request.Directories == nil {
		return errors.New("workspace volume copy request and reservation are required")
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation is not active: %w", err)
	}

	sourceName := app.WorkspaceNamedVolumeName(request.SourceWorkspaceID)
	root, err := d.workspaceCopyDockerRoot(ctx)
	if err != nil {
		return err
	}
	sourcePath, sourceInfo, err := d.workspaceCopyVolumePath(ctx, root, sourceName, request)
	if err != nil {
		return fmt.Errorf("inspect source Workspace volume: %w", err)
	}
	if err := d.rejectAttachedVolume(ctx, sourceName); err != nil {
		return fmt.Errorf("source Workspace volume must be detached before copy: %w", err)
	}
	if matches, err := workspaceCopyTreeMatches(ctx, sourcePath, request); err != nil {
		return fmt.Errorf("inspect source Workspace checkpoint: %w", err)
	} else if !matches {
		return errors.New("source Workspace volume does not match the requested checkpoint")
	}
	if err := workspacefs.ValidateTreeCopy(ctx, sourcePath); err != nil {
		return fmt.Errorf("validate source Workspace tree copy: %w", err)
	}
	if err := ensureWorkspaceCopyRootIdentity(sourcePath, sourceInfo); err != nil {
		return err
	}
	if err := d.rejectAttachedVolume(ctx, sourceName); err != nil {
		return fmt.Errorf("source Workspace volume changed during copy preflight: %w", err)
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation changed during preflight: %w", err)
	}
	return nil
}

// CopyWorkspaceVolume copies a frozen Workspace and returns the verified
// destination directory metadata for its newly staged revision.
func (d *Docker) CopyWorkspaceVolume(ctx context.Context, request WorkspaceVolumeCopyRequest, reservation WorkspaceVolumeCopyReservation) ([]workspace.File, []workspace.Directory, error) {
	if err := d.copyWorkspaceVolume(ctx, request, reservation); err != nil {
		return nil, nil, err
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return nil, nil, fmt.Errorf("workspace volume copy reservation changed before metadata readback: %w", err)
	}
	root, err := d.workspaceCopyDockerRoot(ctx)
	if err != nil {
		return nil, nil, err
	}
	targetName := app.WorkspaceNamedVolumeName(request.TargetWorkspaceID)
	targetPath, targetInfo, err := d.workspaceCopyVolumePath(ctx, root, targetName, request)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect copied Workspace volume metadata: %w", err)
	}
	if err := d.rejectAttachedVolume(ctx, targetName); err != nil {
		return nil, nil, fmt.Errorf("copied Workspace volume must remain detached: %w", err)
	}
	if err := ensureWorkspaceCopyRootIdentity(targetPath, targetInfo); err != nil {
		return nil, nil, err
	}
	manifest, err := mordesync.Scan(ctx, targetPath, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("read copied Workspace metadata: %w", err)
	}
	if !workspaceCopyManifestMatches(manifest, request) {
		return nil, nil, errors.New("copied Workspace volume changed before metadata readback")
	}
	if err := ensureWorkspaceCopyRootIdentity(targetPath, targetInfo); err != nil {
		return nil, nil, err
	}
	if err := d.rejectAttachedVolume(ctx, targetName); err != nil {
		return nil, nil, fmt.Errorf("copied Workspace volume changed during metadata readback: %w", err)
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return nil, nil, fmt.Errorf("workspace volume copy reservation changed during metadata readback: %w", err)
	}
	actualFiles := make(map[string]mordesync.File, len(manifest.Files))
	for _, file := range manifest.Files {
		actualFiles[file.Path] = file
	}
	files := make([]workspace.File, 0, len(request.Files))
	for _, expected := range request.Files {
		file, exists := actualFiles[expected.Path]
		if !exists || !file.ModTimeKnown {
			return nil, nil, errors.New("copied Workspace file metadata is incomplete")
		}
		modified := file.ModTimeNS
		copy := expected
		copy.ModTimeNS = &modified
		files = append(files, copy)
	}
	directories := make([]workspace.Directory, 0, len(manifest.Directories))
	for _, directory := range manifest.Directories {
		if !directory.ModeKnown || !directory.ModTimeKnown {
			return nil, nil, errors.New("copied Workspace directory metadata is incomplete")
		}
		mode, modified := directory.Mode, directory.ModTimeNS
		directories = append(directories, workspace.Directory{Path: directory.Path, Mode: &mode, ModTimeNS: &modified,
			MetadataKnown: directory.MetadataKnown, Xattrs: workspaceCopyXattrs(directory.Xattrs), ACL: workspaceCopyACL(directory.ACL)})
	}
	return files, directories, nil
}

// The caller owns reservation creation and reconciliation.
func (d *Docker) copyWorkspaceVolume(ctx context.Context, request WorkspaceVolumeCopyRequest, reservation WorkspaceVolumeCopyReservation) (retErr error) {
	if d == nil || ctx == nil || reservation == nil ||
		!workspaceCopyReservationID.MatchString(request.ReservationID) ||
		strings.TrimSpace(request.ServiceInstanceID) == "" || len(request.ServiceInstanceID) > 128 ||
		!domain.ValidUserID(request.OwnerID) || !domain.ValidWorkspaceID(request.SourceWorkspaceID) ||
		!domain.ValidWorkspaceID(request.TargetWorkspaceID) || request.SourceWorkspaceID == request.TargetWorkspaceID ||
		request.SourceRevision < 0 || request.Files == nil || request.EmptyDirectories == nil {
		return errors.New("workspace volume copy request and reservation are required")
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation is not active: %w", err)
	}

	sourceName := app.WorkspaceNamedVolumeName(request.SourceWorkspaceID)
	targetName := app.WorkspaceNamedVolumeName(request.TargetWorkspaceID)
	if sourceName == targetName {
		return errors.New("workspace volume copy source and destination must differ")
	}
	root, err := d.workspaceCopyDockerRoot(ctx)
	if err != nil {
		return err
	}
	sourcePath, sourceInfo, err := d.workspaceCopyVolumePath(ctx, root, sourceName, request)
	if err != nil {
		return fmt.Errorf("inspect source Workspace volume: %w", err)
	}
	targetPath, targetInfo, err := d.workspaceCopyVolumePath(ctx, root, targetName, request)
	if err != nil {
		return fmt.Errorf("inspect destination Workspace volume: %w", err)
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return errors.New("workspace volume copy source and destination resolve to the same directory")
	}
	if err := d.rejectAttachedVolume(ctx, sourceName); err != nil {
		return fmt.Errorf("source Workspace volume must be detached before copy: %w", err)
	}
	if err := d.rejectAttachedVolume(ctx, targetName); err != nil {
		return fmt.Errorf("destination Workspace volume must be detached before copy: %w", err)
	}
	sourceMatches, err := workspaceCopyTreeMatches(ctx, sourcePath, request)
	if err != nil {
		return fmt.Errorf("inspect source Workspace checkpoint: %w", err)
	}
	if !sourceMatches {
		return errors.New("source Workspace volume does not match the requested checkpoint")
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation changed before copy: %w", err)
	}
	if err := ensureWorkspaceCopyRootIdentity(sourcePath, sourceInfo); err != nil {
		return err
	}
	copyStarted := false
	copySucceeded := false
	defer func() {
		if retErr == nil || !copyStarted || copySucceeded {
			return
		}
		cleanupErr := d.cleanWorkspaceCopyDestination(ctx, request, targetName, targetPath, targetInfo)
		retErr = errors.Join(retErr, cleanupErr)
	}()
	destinationRoot, err := os.OpenRoot(targetPath)
	if err != nil {
		return fmt.Errorf("open destination Workspace volume: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	openedTargetInfo, err := destinationRoot.Stat(".")
	if err != nil || !os.SameFile(targetInfo, openedTargetInfo) {
		return errors.Join(errors.New("destination Workspace volume changed before copy"), err)
	}
	entries, err := readWorkspaceCopyRoot(destinationRoot)
	if err != nil {
		return fmt.Errorf("inspect destination Workspace volume: %w", err)
	}
	if len(entries) != 0 {
		snapshotsMatch, verifyErr := workspaceCopySnapshotsMatch(ctx, sourcePath, targetPath, request)
		if verifyErr == nil && snapshotsMatch {
			if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
				return fmt.Errorf("workspace volume copy reservation changed before accepting existing copy: %w", err)
			}
			if err := ensureWorkspaceCopyRootIdentity(sourcePath, sourceInfo); err != nil {
				return err
			}
			if err := ensureWorkspaceCopyRootIdentity(targetPath, targetInfo); err != nil {
				return err
			}
			if err := d.rejectAttachedVolume(ctx, sourceName); err != nil {
				return fmt.Errorf("source Workspace volume changed during copy: %w", err)
			}
			if err := d.rejectAttachedVolume(ctx, targetName); err != nil {
				return fmt.Errorf("destination Workspace volume changed during copy: %w", err)
			}
			copySucceeded = true
			return nil
		}
		if err := d.cleanWorkspaceCopyDestination(ctx, request, targetName, targetPath, targetInfo); err != nil {
			return fmt.Errorf("clear incomplete Workspace copy destination: %w", err)
		}
		if err := destinationRoot.Close(); err != nil {
			return fmt.Errorf("close destination Workspace volume before retry: %w", err)
		}
		destinationRoot, err = os.OpenRoot(targetPath)
		if err != nil {
			return fmt.Errorf("reopen destination Workspace volume: %w", err)
		}
		defer func() { _ = destinationRoot.Close() }()
		entries, err = readWorkspaceCopyRoot(destinationRoot)
		if err != nil || len(entries) != 0 {
			return errors.Join(errors.New("destination Workspace volume could not be cleared for retry"), err)
		}
	}
	incomingBytes, err := workspaceCopyManifestBytes(request.Files)
	if err != nil {
		return err
	}
	if err := d.CheckCapacityForWrite(ctx, incomingBytes); err != nil {
		return err
	}

	copyStarted = true
	if err := workspacefs.CopyTree(sourcePath, targetPath); err != nil {
		return fmt.Errorf("copy Workspace volume tree: %w", err)
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation changed during copy: %w", err)
	}
	if err := ensureWorkspaceCopyRootIdentity(sourcePath, sourceInfo); err != nil {
		return err
	}
	if err := ensureWorkspaceCopyRootIdentity(targetPath, targetInfo); err != nil {
		return err
	}
	sourceMatches, err = workspaceCopyTreeMatches(ctx, sourcePath, request)
	if err != nil || !sourceMatches {
		return errors.Join(errors.New("source Workspace volume changed during copy"), err)
	}
	snapshotsMatch, err := workspaceCopySnapshotsMatch(ctx, sourcePath, targetPath, request)
	if err != nil || !snapshotsMatch {
		return errors.Join(errors.New("copied Workspace volume does not match the source checkpoint and metadata"), err)
	}
	if err := d.rejectAttachedVolume(ctx, sourceName); err != nil {
		return fmt.Errorf("source Workspace volume changed during copy: %w", err)
	}
	if err := d.rejectAttachedVolume(ctx, targetName); err != nil {
		return fmt.Errorf("destination Workspace volume changed during copy: %w", err)
	}
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return fmt.Errorf("workspace volume copy reservation changed before completion: %w", err)
	}
	copySucceeded = true
	return nil
}

func workspaceCopyManifestBytes(files []workspace.File) (int64, error) {
	var total int64
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil || total > math.MaxInt64-file.SizeBytes {
			return 0, errors.New("Workspace copy manifest has an invalid file size")
		}
		total += file.SizeBytes
	}
	return total, nil
}

func workspaceCopyTreeMatches(ctx context.Context, path string, request WorkspaceVolumeCopyRequest) (bool, error) {
	manifest, err := mordesync.Scan(ctx, path, nil)
	if err != nil {
		return false, err
	}
	return workspaceCopyManifestMatches(manifest, request), nil
}

func workspaceCopySnapshotsMatch(ctx context.Context, sourcePath, targetPath string, request WorkspaceVolumeCopyRequest) (bool, error) {
	source, err := mordesync.Scan(ctx, sourcePath, nil)
	if err != nil {
		return false, err
	}
	target, err := mordesync.Scan(ctx, targetPath, nil)
	if err != nil {
		return false, err
	}
	return workspaceCopyManifestMatches(source, request) && workspaceCopyManifestMatches(target, request) &&
		reflect.DeepEqual(source, target), nil
}

func workspaceCopyManifestMatches(manifest mordesync.Manifest, request WorkspaceVolumeCopyRequest) bool {
	files := append([]workspace.File(nil), request.Files...)
	directories := append([]string(nil), request.EmptyDirectories...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(directories)
	if len(manifest.Files) != len(files) || len(manifest.EmptyDirectories) != len(directories) {
		return false
	}
	for i, expected := range files {
		if workspace.ValidateFile(expected) != nil || !expected.MetadataKnown {
			return false
		}
		actual := manifest.Files[i]
		actualKind, expectedKind := actual.Kind, expected.Kind
		if actualKind == "" {
			actualKind = "file"
		}
		if expectedKind == "" {
			expectedKind = "file"
		}
		if actual.Path != expected.Path || actual.Size != expected.SizeBytes || actual.SHA256 != expected.SHA256 ||
			actual.Mode != expected.Mode&0o777 || actualKind != expectedKind || actual.SymlinkTarget != expected.SymlinkTarget ||
			actual.HardlinkTo != expected.HardlinkTo || !actual.MetadataKnown ||
			!workspace.SameNativeMetadata(workspaceCopyXattrs(actual.Xattrs), expected.Xattrs, workspaceCopyACL(actual.ACL), expected.ACL) ||
			expected.ModTimeNS != nil && actual.ModTimeNS != *expected.ModTimeNS {
			return false
		}
	}
	for i, expected := range directories {
		if workspace.ValidateRelativePath(expected) != nil || manifest.EmptyDirectories[i] != expected {
			return false
		}
	}
	wantDirectoryPaths := workspaceCopyDirectoryPaths(files, directories)
	actualDirectories := make(map[string]mordesync.Directory, len(manifest.Directories))
	for _, directory := range manifest.Directories {
		key := strings.ToLower(directory.Path)
		if _, duplicate := actualDirectories[key]; duplicate {
			return false
		}
		actualDirectories[key] = directory
	}
	if len(actualDirectories) != len(wantDirectoryPaths) {
		return false
	}
	wantMetadata := make(map[string]workspace.Directory, len(request.Directories))
	for _, directory := range request.Directories {
		if workspace.ValidateDirectory(directory, false) != nil {
			return false
		}
		key := strings.ToLower(directory.Path)
		if _, duplicate := wantMetadata[key]; duplicate {
			return false
		}
		wantMetadata[key] = directory
	}
	for key, path := range wantDirectoryPaths {
		actual, exists := actualDirectories[key]
		if !exists || actual.Path != path || !actual.ModeKnown || !actual.ModTimeKnown {
			return false
		}
		expected, exists := wantMetadata[key]
		if !exists || !expected.MetadataKnown || !actual.MetadataKnown ||
			!workspace.SameNativeMetadata(workspaceCopyXattrs(actual.Xattrs), expected.Xattrs, workspaceCopyACL(actual.ACL), expected.ACL) ||
			expected.Mode != nil && *expected.Mode != actual.Mode || expected.ModTimeNS != nil && *expected.ModTimeNS != actual.ModTimeNS {
			return false
		}
	}
	return len(wantMetadata) == len(actualDirectories)
}

func workspaceCopyXattrs(values []mordesync.ExtendedAttribute) []workspace.ExtendedAttribute {
	result := make([]workspace.ExtendedAttribute, len(values))
	for index, value := range values {
		result[index] = workspace.ExtendedAttribute{Name: value.Name, Value: bytes.Clone(value.Value)}
	}
	return result
}

func workspaceCopyACL(value *mordesync.NativeACL) *workspace.NativeACL {
	if value == nil {
		return nil
	}
	return &workspace.NativeACL{Platform: value.Platform, Format: value.Format, Data: bytes.Clone(value.Data)}
}

func workspaceCopyDirectoryPaths(files []workspace.File, emptyDirectories []string) map[string]string {
	result := map[string]string{".": "."}
	add := func(path string, includeSelf bool) {
		parts := strings.Split(path, "/")
		limit := len(parts)
		if !includeSelf {
			limit--
		}
		prefix := ""
		for index := 0; index < limit; index++ {
			if index > 0 {
				prefix += "/"
			}
			prefix += parts[index]
			result[strings.ToLower(prefix)] = prefix
		}
	}
	for _, file := range files {
		add(file.Path, false)
	}
	for _, directory := range emptyDirectories {
		add(directory, true)
	}
	return result
}

func (d *Docker) workspaceCopyDockerRoot(ctx context.Context) (string, error) {
	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := d.getJSON(ctx, "/info", &info, "read docker data root for Workspace copy"); err != nil {
		return "", err
	}
	actualRoot, err := filepath.EvalSymlinks(info.DockerRootDir)
	if err != nil {
		return "", fmt.Errorf("resolve docker data root: %w", err)
	}
	expectedRoot, err := filepath.EvalSymlinks(d.dataDir)
	if err != nil || !filepath.IsAbs(actualRoot) || filepath.Clean(actualRoot) != filepath.Clean(expectedRoot) {
		return "", errors.New("configured docker data directory does not match the running engine")
	}
	return filepath.Clean(actualRoot), nil
}

func (d *Docker) workspaceCopyVolumePath(ctx context.Context, dockerRoot, name string, request WorkspaceVolumeCopyRequest) (string, os.FileInfo, error) {
	if err := d.VerifyWorkspaceVolume(ctx, name, request.ServiceInstanceID, request.OwnerID, workspaceIDFromVolumeName(name)); err != nil {
		return "", nil, err
	}
	var volume dockerWorkspaceCopyVolume
	if err := d.getJSON(ctx, "/volumes/"+url.PathEscape(name), &volume, "inspect Workspace copy volume"); err != nil {
		return "", nil, err
	}
	workspaceID := workspaceIDFromVolumeName(name)
	labels, err := workspaceVolumeLabels(name, request.ServiceInstanceID, request.OwnerID, workspaceID)
	if err != nil {
		return "", nil, err
	}
	if err := verifyWorkspaceVolume(dockerVolumeIdentity{Name: volume.Name, Driver: volume.Driver, Labels: volume.Labels}, name, labels); err != nil {
		return "", nil, err
	}
	expectedPath := filepath.Join(dockerRoot, "volumes", name, "_data")
	if !filepath.IsAbs(volume.Mountpoint) || filepath.Clean(volume.Mountpoint) != expectedPath {
		return "", nil, errors.New("Workspace volume mountpoint is outside its expected Docker data root")
	}
	resolvedPath, err := filepath.EvalSymlinks(volume.Mountpoint)
	if err != nil || filepath.Clean(resolvedPath) != expectedPath {
		return "", nil, errors.New("Workspace volume mountpoint does not resolve to its expected Docker path")
	}
	root, err := os.OpenRoot(expectedPath)
	if err != nil {
		return "", nil, fmt.Errorf("open Workspace volume mountpoint: %w", err)
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	pathInfo, pathErr := os.Lstat(expectedPath)
	if err != nil || pathErr != nil || !pathInfo.IsDir() || !os.SameFile(rootInfo, pathInfo) {
		return "", nil, errors.Join(errors.New("Workspace volume changed while opening"), err, pathErr)
	}
	return expectedPath, rootInfo, nil
}

func workspaceIDFromVolumeName(name string) string {
	return strings.TrimPrefix(name, "workspace-")
}

func ensureWorkspaceCopyRootIdentity(path string, expected os.FileInfo) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return fmt.Errorf("open Workspace copy volume root: %w", err)
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !pathInfo.IsDir() || !os.SameFile(expected, rootInfo) || !os.SameFile(expected, pathInfo) {
		return errors.Join(errors.New("Workspace volume root identity changed"), err, pathErr)
	}
	return nil
}

func readWorkspaceCopyRoot(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return directory.ReadDir(-1)
}

func (d *Docker) cleanWorkspaceCopyDestination(ctx context.Context, request WorkspaceVolumeCopyRequest, name, path string, expected os.FileInfo) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 40*time.Second)
	defer cancel()
	dockerRoot, err := d.workspaceCopyDockerRoot(cleanupCtx)
	if err != nil {
		return fmt.Errorf("resolve destination for Workspace copy cleanup: %w", err)
	}
	verifiedPath, currentInfo, err := d.workspaceCopyVolumePath(cleanupCtx, dockerRoot, name, request)
	if err != nil {
		return fmt.Errorf("verify destination before Workspace copy cleanup: %w", err)
	}
	if verifiedPath != path || !os.SameFile(expected, currentInfo) {
		return errors.New("Workspace copy cleanup skipped a changed destination volume")
	}
	if err := d.rejectAttachedVolume(cleanupCtx, name); err != nil {
		return fmt.Errorf("Workspace copy cleanup skipped an attached destination: %w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return fmt.Errorf("open destination for Workspace copy cleanup: %w", err)
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, rootInfo) {
		return errors.Join(errors.New("Workspace copy cleanup skipped a changed destination root"), err)
	}
	entries, err := readWorkspaceCopyRoot(root)
	if err != nil {
		return fmt.Errorf("list destination for Workspace copy cleanup: %w", err)
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove Workspace copy rollback path %q: %w", entry.Name(), err)
		}
	}
	entries, err = readWorkspaceCopyRoot(root)
	if err != nil || len(entries) != 0 {
		return errors.Join(errors.New("Workspace copy cleanup did not empty destination"), err)
	}
	if err := ensureWorkspaceCopyRootIdentity(path, expected); err != nil {
		return err
	}
	return nil
}
