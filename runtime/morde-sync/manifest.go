package mordesync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// ValidateDirectoryPathList validates explicit empty-directory paths without
// changing file counts or byte totals.
func ValidateDirectoryPathList(paths []string) error {
	if len(paths) > MaxManifestEntries {
		return ErrInvalidManifest
	}
	var validator ManifestPathValidator
	for _, value := range paths {
		if err := validator.AddEmptyDirectory(value); err != nil {
			return err
		}
	}
	return nil
}

type PlanEntry struct {
	Path           string      `json:"path"`
	Status         ApplyStatus `json:"status"`
	ExistingSHA256 string      `json:"existing_sha256,omitempty"`
}

type MissingPlan struct {
	Entries          []PlanEntry `json:"entries"`
	DirectoryEntries []PlanEntry `json:"directory_entries"`
}

// ValidateManifest checks paths, hashes, block coverage, and aggregate sizes
// before a manifest is used as a transfer or filesystem instruction.
func ValidateManifest(manifest Manifest) error {
	return validateManifest(manifest, false)
}

// ValidateManifestSubset validates a path-filtered manifest whose hard-link
// anchor may be present in the surrounding complete manifest instead.
func ValidateManifestSubset(manifest Manifest) error {
	return validateManifest(manifest, true)
}

// ValidateFile validates one manifest entry without requiring its referenced
// hard-link anchor to be in the same page or change event.
func ValidateFile(file File) error {
	return validateFile(file)
}

func validateManifest(manifest Manifest, allowMissingHardlinkAnchors bool) error {
	if manifest.FileCount != int64(len(manifest.Files)) || manifest.TotalBytes < 0 || len(manifest.Files)+max(len(manifest.EmptyDirectories), len(manifest.Directories)) > MaxManifestEntries {
		return ErrInvalidManifest
	}
	var total int64
	var paths ManifestPathValidator
	for _, file := range manifest.Files {
		if err := validateFile(file); err != nil {
			return err
		}
		if err := paths.Add(file.Path); err != nil {
			return err
		}
		if file.Size > int64(^uint64(0)>>1)-total {
			return ErrInvalidManifest
		}
		total += file.Size
	}
	for _, directory := range manifest.EmptyDirectories {
		if err := paths.AddEmptyDirectory(directory); err != nil {
			return err
		}
	}
	directoryPaths := make(map[string]string, len(manifest.Directories))
	expectedDirectories := make(map[string]string, len(manifest.Files)+len(manifest.EmptyDirectories))
	canonicalComponents := make(map[string]string)
	addDirectoryAncestors := func(value string, includeSelf bool) {
		parts := strings.Split(value, "/")
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
			expectedDirectories[strings.ToLower(prefix)] = prefix
		}
	}
	for _, file := range manifest.Files {
		addDirectoryAncestors(file.Path, false)
	}
	for _, directory := range manifest.EmptyDirectories {
		addDirectoryAncestors(directory, true)
	}
	for key, value := range expectedDirectories {
		canonicalComponents[key] = value
	}
	for _, directory := range manifest.Directories {
		if directory.Path != "." && validRelativePath(directory.Path) != nil || directory.ModeKnown && directory.Mode&^uint32(0o777) != 0 ||
			validateMetadata(directory.Xattrs, directory.ACL) != nil || !directory.MetadataKnown && (len(directory.Xattrs) != 0 || directory.ACL != nil) {
			return ErrInvalidManifest
		}
		key := strings.ToLower(directory.Path)
		if previous, exists := directoryPaths[key]; exists {
			return fmt.Errorf("directory path %q duplicates %q: %w", directory.Path, previous, ErrInvalidManifest)
		}
		if directory.Path == "." {
			directoryPaths[key] = directory.Path
			continue
		}
		for _, file := range manifest.Files {
			fileKey := strings.ToLower(file.Path)
			if key == fileKey || strings.HasPrefix(fileKey, key+"/") == false && strings.HasPrefix(key, fileKey+"/") {
				return fmt.Errorf("directory path %q conflicts with file %q: %w", directory.Path, file.Path, ErrInvalidManifest)
			}
		}
		parts := strings.Split(directory.Path, "/")
		prefix := ""
		for index, part := range parts {
			if index > 0 {
				prefix += "/"
			}
			prefix += part
			componentKey := strings.ToLower(prefix)
			if previous, exists := canonicalComponents[componentKey]; exists && previous != prefix {
				return fmt.Errorf("directory path %q conflicts with %q: %w", directory.Path, previous, ErrInvalidManifest)
			}
			canonicalComponents[componentKey] = prefix
		}
		directoryPaths[key] = directory.Path
	}
	if len(manifest.Directories) > 0 {
		rootCount := 0
		if _, exists := directoryPaths["."]; exists {
			rootCount = 1
		}
		if len(directoryPaths)-rootCount != len(expectedDirectories) {
			return ErrInvalidManifest
		}
		for key, expected := range expectedDirectories {
			if actual, exists := directoryPaths[key]; !exists || actual != expected {
				return ErrInvalidManifest
			}
		}
	}
	if total != manifest.TotalBytes {
		return ErrInvalidManifest
	}
	if err := validateHardlinkGroups(manifest.Files, allowMissingHardlinkAnchors); err != nil {
		return err
	}
	return nil
}

func validateFile(file File) error {
	if validRelativePath(file.Path) != nil || file.Size < 0 || file.Mode&^uint32(0o777) != 0 || !validHash(file.SHA256) ||
		validateMetadata(file.Xattrs, file.ACL) != nil || !file.MetadataKnown && (len(file.Xattrs) != 0 || file.ACL != nil) {
		return ErrInvalidManifest
	}
	if file.Kind == FileKindSymlink {
		if file.HardlinkTo != "" || ValidateSymlinkTarget(file.Path, file.SymlinkTarget) != nil || file.Mode != 0 ||
			file.Size != int64(len([]byte(file.SymlinkTarget))) || file.SHA256 != sumHex([]byte(file.SymlinkTarget)) || len(file.Blocks) != 0 {
			return ErrInvalidManifest
		}
		return nil
	}
	if file.Kind != "" && file.Kind != FileKindRegular || file.SymlinkTarget != "" ||
		file.HardlinkTo != "" && (validRelativePath(file.HardlinkTo) != nil || file.HardlinkTo >= file.Path) {
		return ErrInvalidManifest
	}
	blockCount := file.Size / BlockSize
	if file.Size%BlockSize != 0 {
		blockCount++
	}
	if blockCount != int64(len(file.Blocks)) {
		return ErrInvalidManifest
	}
	for index, block := range file.Blocks {
		expectedOffset := int64(index) * BlockSize
		expectedSize := min(int64(BlockSize), file.Size-expectedOffset)
		if block.Offset != expectedOffset || block.Size != expectedSize || block.Size <= 0 || !validHash(block.SHA256) {
			return ErrInvalidManifest
		}
	}
	return nil
}

func validateHardlinkGroups(files []File, allowMissingAnchors bool) error {
	byPath := make(map[string]File, len(files))
	members := make(map[string][]File)
	for _, file := range files {
		byPath[file.Path] = file
		if file.HardlinkTo != "" {
			members[file.HardlinkTo] = append(members[file.HardlinkTo], file)
		}
	}
	for anchorPath, linked := range members {
		anchor, exists := byPath[anchorPath]
		if !exists {
			if allowMissingAnchors {
				for _, file := range linked[1:] {
					if !sameHardlinkMetadata(linked[0], file) {
						return fmt.Errorf("hard-linked files for absent anchor %q have different metadata: %w", anchorPath, ErrInvalidManifest)
					}
				}
				continue
			}
			return fmt.Errorf("hard-link anchor %q is absent: %w", anchorPath, ErrInvalidManifest)
		}
		if anchor.Kind == FileKindSymlink || anchor.HardlinkTo != "" {
			return fmt.Errorf("hard-link anchor %q is not a regular canonical file: %w", anchorPath, ErrInvalidManifest)
		}
		for _, file := range linked {
			if file.Kind == FileKindSymlink || !sameHardlinkMetadata(anchor, file) {
				return fmt.Errorf("hard-linked file %q differs from anchor %q: %w", file.Path, anchorPath, ErrInvalidManifest)
			}
		}
	}
	return nil
}

func sameHardlinkMetadata(left, right File) bool {
	if left.Size != right.Size || left.SHA256 != right.SHA256 || left.Mode != right.Mode ||
		left.ModTimeKnown != right.ModTimeKnown || left.ModTimeKnown && left.ModTimeNS != right.ModTimeNS ||
		left.MetadataKnown != right.MetadataKnown || !sameMetadata(left.Xattrs, left.ACL, right.Xattrs, right.ACL) ||
		len(left.Blocks) != len(right.Blocks) {
		return false
	}
	for index := range left.Blocks {
		if left.Blocks[index] != right.Blocks[index] {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// PlanMissing classifies only requested paths. Existing identical files are
// already present; different content and non-regular targets are conflicts.
// The eventual ApplyMissing call repeats the check to close the plan/apply race.
func PlanMissing(ctx context.Context, directory string, manifest Manifest) (MissingPlan, error) {
	if ctx == nil {
		return MissingPlan{}, errors.New("context is required")
	}
	if err := acquireOperation(ctx); err != nil {
		return MissingPlan{}, err
	}
	defer func() { <-operationSlots }()
	if err := ValidateManifestSubset(manifest); err != nil {
		return MissingPlan{}, err
	}
	root, err := openDirectoryRoot(directory)
	if err != nil {
		return MissingPlan{}, err
	}
	defer root.Close()
	plan := MissingPlan{Entries: make([]PlanEntry, 0, len(manifest.Files)), DirectoryEntries: make([]PlanEntry, 0, len(manifest.EmptyDirectories))}
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return MissingPlan{}, err
		}
		status, existingHash, exists, err := classifyExisting(ctx, root, file)
		if errors.Is(err, fs.ErrNotExist) {
			status, exists, err = "", false, nil
		}
		if err != nil {
			return MissingPlan{}, fmt.Errorf("classify %q: %w", file.Path, err)
		}
		if !exists {
			status = ApplyMissingStatus
		}
		plan.Entries = append(plan.Entries, PlanEntry{Path: file.Path, Status: status, ExistingSHA256: existingHash})
	}
	for _, directory := range manifest.EmptyDirectories {
		if err := ctx.Err(); err != nil {
			return MissingPlan{}, err
		}
		status, err := classifyEmptyDirectory(root, directory)
		if err != nil {
			return MissingPlan{}, fmt.Errorf("classify directory %q: %w", directory, err)
		}
		plan.DirectoryEntries = append(plan.DirectoryEntries, PlanEntry{Path: directory, Status: status})
	}
	return plan, nil
}

func classifyEmptyDirectory(root *os.Root, relative string) (ApplyStatus, error) {
	parent, name, closeParent, err := openParent(root, relative, false)
	if errors.Is(err, fs.ErrNotExist) {
		return ApplyMissingStatus, nil
	}
	if errors.Is(err, ErrPathConflict) || errors.Is(err, ErrUnsafePath) {
		return ApplyConflict, nil
	}
	if err != nil {
		return "", err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return ApplyMissingStatus, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ApplyConflict, nil
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer child.Close()
	openedInfo, err := child.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		return "", ErrDestinationChange
	}
	entries, err := child.Open(".")
	if err != nil {
		return "", err
	}
	defer entries.Close()
	children, err := entries.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(children) != 0 {
		return ApplyConflict, nil
	}
	return ApplyPresent, nil
}

const ApplyMissingStatus ApplyStatus = "missing"
