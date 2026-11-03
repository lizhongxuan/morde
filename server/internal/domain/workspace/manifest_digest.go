package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	mordesync "morde-sync"
)

const manifestDigestVersion = "morde-workspace-manifest/v1\n"
const manifestTreeDigestVersion = "morde-workspace-manifest/v2\n"
const manifestHardlinkDigestVersion = "morde-workspace-manifest/v3\n"
const manifestMetadataDigestVersion = "morde-workspace-manifest/v4\n"

type manifestDigestFileV1 struct {
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    string  `json:"sha256"`
	Mode      uint32  `json:"mode"`
	Chunks    []Chunk `json:"chunks"`
}

type manifestDigestFileV2 struct {
	Path          string  `json:"path"`
	Kind          string  `json:"kind"`
	SymlinkTarget string  `json:"symlink_target,omitempty"`
	SizeBytes     int64   `json:"size_bytes"`
	SHA256        string  `json:"sha256"`
	Mode          uint32  `json:"mode"`
	Chunks        []Chunk `json:"chunks"`
}

type manifestDigestFileV3 struct {
	Path          string  `json:"path"`
	Kind          string  `json:"kind"`
	SymlinkTarget string  `json:"symlink_target,omitempty"`
	HardlinkTo    string  `json:"hardlink_to,omitempty"`
	SizeBytes     int64   `json:"size_bytes"`
	SHA256        string  `json:"sha256"`
	Mode          uint32  `json:"mode"`
	ModTimeNS     *int64  `json:"mod_time_ns,omitempty"`
	Chunks        []Chunk `json:"chunks"`
}

type manifestDigestFileV4 struct {
	Path          string              `json:"path"`
	Kind          string              `json:"kind"`
	SymlinkTarget string              `json:"symlink_target,omitempty"`
	HardlinkTo    string              `json:"hardlink_to,omitempty"`
	SizeBytes     int64               `json:"size_bytes"`
	SHA256        string              `json:"sha256"`
	Mode          uint32              `json:"mode"`
	ModTimeNS     *int64              `json:"mod_time_ns,omitempty"`
	MetadataKnown bool                `json:"metadata_known,omitempty"`
	Xattrs        []ExtendedAttribute `json:"xattrs,omitempty"`
	ACL           *NativeACL          `json:"acl,omitempty"`
	Chunks        []Chunk             `json:"chunks"`
}

// ManifestSHA256 returns the stable digest used to identify a complete
// Workspace revision. The explicit v1 projection keeps future File fields
// from silently changing release identity.
func ManifestSHA256(files []File) (string, error) {
	if len(files) > MaxSyncFiles {
		return "", errors.New("workspace manifest exceeds file limit")
	}
	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	return ManifestSHA256Stream(func(yield func(File) error) error {
		for _, file := range ordered {
			if err := yield(file); err != nil {
				return err
			}
		}
		return nil
	})
}

// ManifestTreeSHA256 identifies files and explicitly tracked empty leaf
// directories. File-only snapshots retain the historical V1 digest so stored
// Task contexts remain verifiable across the directory-metadata rollout.
func ManifestTreeSHA256(files []File, emptyDirectories []string) (string, error) {
	if len(emptyDirectories) == 0 {
		return ManifestSHA256(files)
	}
	if len(files) > MaxSyncFiles || len(emptyDirectories) > MaxSyncFiles-len(files) {
		return "", errors.New("workspace manifest exceeds item limit")
	}
	orderedFiles := append([]File(nil), files...)
	sort.Slice(orderedFiles, func(i, j int) bool { return orderedFiles[i].Path < orderedFiles[j].Path })
	orderedDirectories := append([]string(nil), emptyDirectories...)
	sort.Strings(orderedDirectories)

	var paths mordesync.ManifestPathValidator
	for _, directory := range orderedDirectories {
		if err := ValidateRelativePath(directory); err != nil {
			return "", err
		}
		if err := paths.Add(directory); err != nil {
			return "", err
		}
	}
	for _, file := range orderedFiles {
		if err := ValidateFile(file); err != nil {
			return "", err
		}
		if err := paths.Add(file.Path); err != nil {
			return "", err
		}
	}
	if err := ValidateHardlinkAnchors(orderedFiles); err != nil {
		return "", err
	}
	if err := ValidateNoSymlinkDescendants(orderedFiles, orderedDirectories); err != nil {
		return "", err
	}

	hasher := sha256.New()
	hasSymlinks := false
	hasHardlinks := false
	hasNativeMetadata := false
	for _, file := range orderedFiles {
		if CanonicalFileKind(file.Kind) == FileKindSymlink {
			hasSymlinks = true
		}
		if file.HardlinkTo != "" {
			hasHardlinks = true
		}
		if file.MetadataKnown || len(file.Xattrs) > 0 || file.ACL != nil {
			hasNativeMetadata = true
		}
	}
	if hasNativeMetadata {
		_, _ = hasher.Write([]byte(manifestMetadataDigestVersion))
	} else if hasHardlinks {
		_, _ = hasher.Write([]byte(manifestHardlinkDigestVersion))
	} else if hasSymlinks {
		_, _ = hasher.Write([]byte(manifestTreeDigestVersionWithSymlinks))
	} else {
		_, _ = hasher.Write([]byte(manifestTreeDigestVersion))
	}
	_, _ = hasher.Write([]byte("{\"empty_directories\":["))
	encodedBytes := len(`{"empty_directories":[],"files":[]}`)
	for index, directory := range orderedDirectories {
		encoded, err := json.Marshal(directory)
		if err != nil {
			return "", err
		}
		if index > 0 {
			encodedBytes++
			_, _ = hasher.Write([]byte(","))
		}
		encodedBytes += len(encoded)
		if encodedBytes > MaxSyncManifestBytes {
			return "", errors.New("workspace manifest exceeds byte limit")
		}
		_, _ = hasher.Write(encoded)
	}
	_, _ = hasher.Write([]byte("],\"files\":["))
	for index, file := range orderedFiles {
		if index > 0 {
			encodedBytes++
			_, _ = hasher.Write([]byte(","))
		}
		chunks := file.Chunks
		if chunks == nil {
			chunks = []Chunk{}
		}
		var encoded []byte
		var err error
		if hasNativeMetadata {
			encoded, err = json.Marshal(manifestDigestFileV4{
				Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
				HardlinkTo: file.HardlinkTo, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode,
				ModTimeNS: file.ModTimeNS, MetadataKnown: file.MetadataKnown, Xattrs: file.Xattrs, ACL: file.ACL, Chunks: chunks,
			})
		} else if hasHardlinks {
			encoded, err = json.Marshal(manifestDigestFileV3{
				Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
				HardlinkTo: file.HardlinkTo, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode,
				ModTimeNS: file.ModTimeNS, Chunks: chunks,
			})
		} else if hasSymlinks {
			encoded, err = json.Marshal(manifestDigestFileV2{
				Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
				SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
			})
		} else {
			encoded, err = json.Marshal(manifestDigestFileV1{
				Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
			})
		}
		if err != nil {
			return "", err
		}
		encodedBytes += len(encoded)
		if encodedBytes > MaxSyncManifestBytes {
			return "", errors.New("workspace manifest exceeds byte limit")
		}
		_, _ = hasher.Write(encoded)
	}
	_, _ = hasher.Write([]byte("]}"))
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ManifestTreeWithMetadataSHA256 binds native file and directory metadata to
// the revision digest while keeping older metadata-free revisions compatible.
func ManifestTreeWithMetadataSHA256(files []File, emptyDirectories []string, directories []Directory) (string, error) {
	digest, err := ManifestTreeSHA256(files, emptyDirectories)
	if err != nil {
		return "", err
	}
	if len(files)+max(len(emptyDirectories), len(directories)) > MaxSyncFiles {
		return "", errors.New("workspace manifest exceeds item limit")
	}
	hasNativeMetadata := false
	for _, file := range files {
		if file.MetadataKnown || len(file.Xattrs) > 0 || file.ACL != nil {
			hasNativeMetadata = true
			break
		}
	}
	expectedDirectories := make(map[string]string, len(files)+len(emptyDirectories))
	addAncestors := func(value string, includeSelf bool) {
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
	for _, file := range files {
		addAncestors(file.Path, false)
	}
	for _, directory := range emptyDirectories {
		addAncestors(directory, true)
	}
	ordered := append([]Directory(nil), directories...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	seen := make(map[string]string, len(ordered))
	rootSeen := false
	for _, directory := range ordered {
		if err := ValidateDirectory(directory, false); err != nil {
			return "", err
		}
		key := strings.ToLower(directory.Path)
		if previous, exists := seen[key]; exists {
			return "", errors.New("workspace manifest contains duplicate directory paths: " + previous + " and " + directory.Path)
		}
		seen[key] = directory.Path
		if directory.Path == "." {
			rootSeen = true
		} else if expected, exists := expectedDirectories[key]; !exists || expected != directory.Path {
			return "", errors.New("workspace directory metadata does not match manifest paths")
		}
		if directory.MetadataKnown || len(directory.Xattrs) > 0 || directory.ACL != nil {
			hasNativeMetadata = true
		}
	}
	metadataDirectoryCount := len(seen)
	if rootSeen {
		metadataDirectoryCount--
	}
	if len(ordered) > 0 && metadataDirectoryCount != len(expectedDirectories) {
		return "", errors.New("workspace directory metadata does not cover manifest directories")
	}
	if hasNativeMetadata && (!rootSeen || metadataDirectoryCount != len(expectedDirectories)) {
		return "", errors.New("workspace native metadata does not cover the root and manifest directories")
	}
	manifestDirectories := append([]string(nil), emptyDirectories...)
	for _, directory := range ordered {
		manifestDirectories = append(manifestDirectories, directory.Path)
	}
	if err := ValidateNoSymlinkDescendants(files, manifestDirectories); err != nil {
		return "", err
	}
	if !hasNativeMetadata {
		return digest, nil
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return "", err
	}
	if len(encoded)+len(digest) > MaxSyncManifestBytes {
		return "", errors.New("workspace manifest exceeds byte limit")
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(manifestMetadataDigestVersion))
	_, _ = hasher.Write([]byte(digest))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(encoded)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ManifestSHA256Stream hashes files yielded in strict lexical path order. The
// caller owns iteration and must return any error from yield.
func ManifestSHA256Stream(read func(func(File) error) error) (string, error) {
	if read == nil {
		return "", errors.New("workspace manifest reader is required")
	}
	hasherV1, hasherV2, hasherV3, hasherV4 := sha256.New(), sha256.New(), sha256.New(), sha256.New()
	_, _ = hasherV1.Write([]byte(manifestDigestVersion))
	_, _ = hasherV1.Write([]byte("["))
	_, _ = hasherV2.Write([]byte(manifestDigestVersionWithSymlinks))
	_, _ = hasherV2.Write([]byte("["))
	_, _ = hasherV3.Write([]byte(manifestHardlinkDigestVersion))
	_, _ = hasherV3.Write([]byte("["))
	_, _ = hasherV4.Write([]byte(manifestMetadataDigestVersion))
	_, _ = hasherV4.Write([]byte("["))
	var paths mordesync.ManifestPathValidator
	encodedBytesV1, encodedBytesV2, encodedBytesV3, encodedBytesV4 := 2, 2, 2, 2
	count := 0
	previousPath := ""
	containsSymlink, containsHardlinks, containsNativeMetadata := false, false, false
	symlinkPaths := make(map[string]struct{})
	filesByPath := make(map[string]File)
	var streamErr error
	err := read(func(file File) error {
		if streamErr != nil {
			return streamErr
		}
		if count >= MaxSyncFiles {
			streamErr = errors.New("workspace manifest exceeds file limit")
			return streamErr
		}
		if err := ValidateFile(file); err != nil {
			streamErr = err
			return streamErr
		}
		if count > 0 && file.Path <= previousPath {
			streamErr = errors.New("workspace manifest files are not in strict path order")
			return streamErr
		}
		if err := paths.Add(file.Path); err != nil {
			streamErr = err
			return streamErr
		}
		foldedParts := strings.Split(strings.ToLower(file.Path), "/")
		for index := 1; index < len(foldedParts); index++ {
			if _, exists := symlinkPaths[strings.Join(foldedParts[:index], "/")]; exists {
				streamErr = errors.New("workspace manifest contains a path below a symlink")
				return streamErr
			}
		}

		chunks := file.Chunks
		if chunks == nil {
			chunks = []Chunk{}
		}
		encoded, err := json.Marshal(manifestDigestFileV1{
			Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
		})
		if err != nil {
			streamErr = err
			return streamErr
		}
		encodedV2, err := json.Marshal(manifestDigestFileV2{
			Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
			SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
		})
		if err != nil {
			streamErr = err
			return err
		}
		encodedV3, err := json.Marshal(manifestDigestFileV3{
			Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
			HardlinkTo: file.HardlinkTo, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode,
			ModTimeNS: file.ModTimeNS, Chunks: chunks,
		})
		if err != nil {
			streamErr = err
			return err
		}
		encodedV4, err := json.Marshal(manifestDigestFileV4{
			Path: file.Path, Kind: CanonicalFileKind(file.Kind), SymlinkTarget: file.SymlinkTarget,
			HardlinkTo: file.HardlinkTo, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode,
			ModTimeNS: file.ModTimeNS, MetadataKnown: file.MetadataKnown, Xattrs: file.Xattrs, ACL: file.ACL, Chunks: chunks,
		})
		if err != nil {
			streamErr = err
			return err
		}
		if file.MetadataKnown || len(file.Xattrs) > 0 || file.ACL != nil {
			containsNativeMetadata = true
		}
		if file.HardlinkTo != "" {
			anchor, exists := filesByPath[file.HardlinkTo]
			if !exists || !hardlinkCompatible(anchor, file) {
				streamErr = errors.New("workspace hardlink anchor is missing or incompatible")
				return streamErr
			}
			containsHardlinks = true
		}
		if count > 0 {
			_, _ = hasherV1.Write([]byte(","))
			_, _ = hasherV2.Write([]byte(","))
			_, _ = hasherV3.Write([]byte(","))
			_, _ = hasherV4.Write([]byte(","))
		}
		if CanonicalFileKind(file.Kind) == FileKindSymlink {
			containsSymlink = true
			symlinkPaths[strings.ToLower(file.Path)] = struct{}{}
		}
		if count > 0 {
			encodedBytesV1++
			encodedBytesV2++
			encodedBytesV3++
			encodedBytesV4++
		}
		encodedBytesV1 += len(encoded)
		encodedBytesV2 += len(encodedV2)
		encodedBytesV3 += len(encodedV3)
		encodedBytesV4 += len(encodedV4)
		if encodedBytesV1 > MaxSyncManifestBytes || containsSymlink && encodedBytesV2 > MaxSyncManifestBytes ||
			(containsSymlink || containsHardlinks) && encodedBytesV3 > MaxSyncManifestBytes ||
			containsNativeMetadata && encodedBytesV4 > MaxSyncManifestBytes {
			streamErr = errors.New("workspace manifest exceeds byte limit")
			return streamErr
		}
		_, _ = hasherV1.Write(encoded)
		_, _ = hasherV2.Write(encodedV2)
		_, _ = hasherV3.Write(encodedV3)
		_, _ = hasherV4.Write(encodedV4)
		filesByPath[file.Path] = file
		previousPath = file.Path
		count++
		return nil
	})
	if streamErr != nil {
		return "", streamErr
	}
	if err != nil {
		return "", err
	}
	_, _ = hasherV1.Write([]byte("]"))
	_, _ = hasherV2.Write([]byte("]"))
	_, _ = hasherV3.Write([]byte("]"))
	_, _ = hasherV4.Write([]byte("]"))
	if containsNativeMetadata {
		return hex.EncodeToString(hasherV4.Sum(nil)), nil
	}
	if containsHardlinks {
		return hex.EncodeToString(hasherV3.Sum(nil)), nil
	}
	if containsSymlink {
		return hex.EncodeToString(hasherV2.Sum(nil)), nil
	}
	return hex.EncodeToString(hasherV1.Sum(nil)), nil
}

func ValidateHardlinkAnchors(files []File) error {
	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	filesByPath := make(map[string]File, len(files))
	for _, file := range ordered {
		if file.HardlinkTo != "" {
			anchor, exists := filesByPath[file.HardlinkTo]
			if !exists || !hardlinkCompatible(anchor, file) {
				return errors.New("workspace hardlink anchor is missing or incompatible")
			}
		}
		filesByPath[file.Path] = file
	}
	return nil
}

func hardlinkCompatible(anchor, file File) bool {
	return CanonicalFileKind(anchor.Kind) == FileKindRegular && anchor.HardlinkTo == "" &&
		CanonicalFileKind(file.Kind) == FileKindRegular && file.HardlinkTo == anchor.Path && anchor.Path < file.Path &&
		anchor.SizeBytes == file.SizeBytes && anchor.SHA256 == file.SHA256 && anchor.Mode == file.Mode &&
		sameOptionalInt64(anchor.ModTimeNS, file.ModTimeNS) && slices.Equal(nonNilChunks(anchor.Chunks), nonNilChunks(file.Chunks)) &&
		anchor.MetadataKnown == file.MetadataKnown && SameNativeMetadata(anchor.Xattrs, file.Xattrs, anchor.ACL, file.ACL)
}

func nonNilChunks(chunks []Chunk) []Chunk {
	if chunks == nil {
		return []Chunk{}
	}
	return chunks
}
