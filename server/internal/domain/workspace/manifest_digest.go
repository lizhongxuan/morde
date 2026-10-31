package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	mordesync "morde-sync"
)

const manifestDigestVersion = "morde-workspace-manifest/v1\n"
const manifestTreeDigestVersion = "morde-workspace-manifest/v2\n"

type manifestDigestFileV1 struct {
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    string  `json:"sha256"`
	Mode      uint32  `json:"mode"`
	Chunks    []Chunk `json:"chunks"`
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

	hasher := sha256.New()
	_, _ = hasher.Write([]byte(manifestTreeDigestVersion))
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
		encoded, err := json.Marshal(manifestDigestFileV1{
			Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
		})
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

// ManifestSHA256Stream hashes files yielded in strict lexical path order. The
// caller owns iteration and must return any error from yield.
func ManifestSHA256Stream(read func(func(File) error) error) (string, error) {
	if read == nil {
		return "", errors.New("workspace manifest reader is required")
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(manifestDigestVersion))
	_, _ = hasher.Write([]byte("["))
	var paths mordesync.ManifestPathValidator
	encodedBytes := 2
	count := 0
	previousPath := ""
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
		if count > 0 {
			encodedBytes++
		}
		encodedBytes += len(encoded)
		if encodedBytes > MaxSyncManifestBytes {
			streamErr = errors.New("workspace manifest exceeds byte limit")
			return streamErr
		}
		if count > 0 {
			_, _ = hasher.Write([]byte(","))
		}
		_, _ = hasher.Write(encoded)
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
	_, _ = hasher.Write([]byte("]"))
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
