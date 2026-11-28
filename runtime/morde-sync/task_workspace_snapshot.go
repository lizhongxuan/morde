package mordesync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const TaskWorkspaceSnapshotSHA256Domain = "morde-task-workspace-snapshot-v1"

type taskWorkspaceSnapshotFile struct {
	Path          string                    `json:"path"`
	Kind          string                    `json:"kind"`
	SymlinkTarget string                    `json:"symlink_target"`
	HardlinkTo    string                    `json:"hardlink_to"`
	Size          int64                     `json:"size"`
	Mode          uint32                    `json:"mode"`
	ModTimeNS     int64                     `json:"mod_time_ns"`
	ModTimeKnown  bool                      `json:"mod_time_known"`
	SHA256        string                    `json:"sha256"`
	MetadataKnown bool                      `json:"metadata_known"`
	Xattrs        []taskWorkspaceXattr      `json:"xattrs"`
	ACL           *taskWorkspaceSnapshotACL `json:"acl"`
}

type taskWorkspaceSnapshotDirectory struct {
	Path          string                    `json:"path"`
	Mode          uint32                    `json:"mode"`
	ModeKnown     bool                      `json:"mode_known"`
	ModTimeNS     int64                     `json:"mod_time_ns"`
	ModTimeKnown  bool                      `json:"mod_time_known"`
	MetadataKnown bool                      `json:"metadata_known"`
	Xattrs        []taskWorkspaceXattr      `json:"xattrs"`
	ACL           *taskWorkspaceSnapshotACL `json:"acl"`
}

type taskWorkspaceXattr struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

type taskWorkspaceSnapshotACL struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
	Data     []byte `json:"data"`
}

type taskWorkspaceSnapshotManifest struct {
	Files            []taskWorkspaceSnapshotFile      `json:"files"`
	EmptyDirectories []string                         `json:"empty_directories"`
	Directories      []taskWorkspaceSnapshotDirectory `json:"directories"`
}

// TaskWorkspaceSnapshotSHA256 returns the shared, versioned digest used to
// compare a Task Workspace shell check with a Runtime-exported manifest. It
// covers the tree shape, permission bits and mtimes, file content, links and
// captured xattr/ACL metadata. UID/GID are deliberately excluded because they
// are absent from the persisted Task Workspace manifest contract; Unix scans
// still compare them during race detection.
//
// Chunk boundaries, FileCount and TotalBytes are omitted because they are
// derivable from file contents and paths, and can change with transfer block
// sizing without changing the Workspace state.
func TaskWorkspaceSnapshotSHA256(manifest Manifest) (string, error) {
	if err := ValidateManifest(manifest); err != nil {
		return "", err
	}
	if len(manifest.Directories) == 0 {
		return "", errors.New("Task Workspace snapshot requires directory metadata including root")
	}
	rootCount := 0
	for _, directory := range manifest.Directories {
		if directory.Path == "." {
			rootCount++
		}
	}
	if rootCount != 1 {
		return "", errors.New("Task Workspace snapshot requires exactly one root directory")
	}

	files := make([]taskWorkspaceSnapshotFile, len(manifest.Files))
	for index, file := range manifest.Files {
		kind := file.Kind
		if kind == "" {
			kind = FileKindRegular
		}
		acl := taskWorkspaceSnapshotACLFrom(file.ACL)
		files[index] = taskWorkspaceSnapshotFile{
			Path: file.Path, Kind: kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
			Size: file.Size, Mode: file.Mode, ModTimeNS: file.ModTimeNS, ModTimeKnown: file.ModTimeKnown,
			SHA256: file.SHA256, MetadataKnown: file.MetadataKnown,
			Xattrs: taskWorkspaceXattrs(file.Xattrs), ACL: acl,
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	directories := make([]taskWorkspaceSnapshotDirectory, len(manifest.Directories))
	for index, directory := range manifest.Directories {
		acl := taskWorkspaceSnapshotACLFrom(directory.ACL)
		directories[index] = taskWorkspaceSnapshotDirectory{
			Path: directory.Path, Mode: directory.Mode, ModeKnown: directory.ModeKnown,
			ModTimeNS: directory.ModTimeNS, ModTimeKnown: directory.ModTimeKnown,
			MetadataKnown: directory.MetadataKnown, Xattrs: taskWorkspaceXattrs(directory.Xattrs), ACL: acl,
		}
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })

	emptyDirectories := append([]string{}, manifest.EmptyDirectories...)
	sort.Strings(emptyDirectories)
	encoded, err := json.Marshal(taskWorkspaceSnapshotManifest{
		Files: files, EmptyDirectories: emptyDirectories, Directories: directories,
	})
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(TaskWorkspaceSnapshotSHA256Domain + "\n"))
	_, _ = hasher.Write(encoded)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func taskWorkspaceXattrs(values []ExtendedAttribute) []taskWorkspaceXattr {
	result := make([]taskWorkspaceXattr, len(values))
	for index, value := range values {
		result[index] = taskWorkspaceXattr{Name: value.Name, Value: append([]byte{}, value.Value...)}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return string(result[i].Value) < string(result[j].Value)
	})
	return result
}

func taskWorkspaceSnapshotACLFrom(value *NativeACL) *taskWorkspaceSnapshotACL {
	if value == nil {
		return nil
	}
	return &taskWorkspaceSnapshotACL{Platform: value.Platform, Format: value.Format, Data: append([]byte{}, value.Data...)}
}
