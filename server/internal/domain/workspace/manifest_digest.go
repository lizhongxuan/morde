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
