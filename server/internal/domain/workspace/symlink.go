package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

const manifestDigestVersionWithSymlinks = "morde-workspace-manifest/v2\n"
const manifestTreeDigestVersionWithSymlinks = "morde-workspace-manifest/v3\n"

// CanonicalFileKind treats legacy in-memory files without a kind as regular files.
func CanonicalFileKind(kind string) string {
	if kind == "" {
		return FileKindRegular
	}
	return kind
}

func ValidateSymlinkTarget(filePath, target string) error {
	if err := ValidateRelativePath(filePath); err != nil {
		return err
	}
	if target == "" || len(target) > MaxSyncPathBytes || !utf8.ValidString(target) || strings.ContainsAny(target, "\x00\\:") || path.IsAbs(target) {
		return errors.New("symlink target must be a valid relative POSIX path")
	}
	depth := 0
	if parent := path.Dir(filePath); parent != "." {
		depth = strings.Count(parent, "/") + 1
	}
	for _, component := range strings.Split(target, "/") {
		switch component {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return errors.New("symlink target escapes the workspace")
			}
		default:
			depth++
		}
	}
	return nil
}

func validateWorkspaceSymlink(file File) error {
	if err := ValidateSymlinkTarget(file.Path, file.SymlinkTarget); err != nil {
		return err
	}
	target := []byte(file.SymlinkTarget)
	digest := sha256.Sum256(target)
	if file.Mode != 0 || len(file.Chunks) != 0 || file.SizeBytes != int64(len(target)) || file.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("symlink metadata does not match its target")
	}
	return nil
}

// ValidateNoSymlinkDescendants rejects manifest entries below a link path.
func ValidateNoSymlinkDescendants(files []File, directories []string) error {
	symlinks := make(map[string]struct{})
	for _, file := range files {
		if CanonicalFileKind(file.Kind) != FileKindSymlink {
			continue
		}
		if err := ValidateRelativePath(file.Path); err != nil {
			return err
		}
		symlinks[strings.ToLower(file.Path)] = struct{}{}
	}
	for _, file := range files {
		parts := strings.Split(strings.ToLower(file.Path), "/")
		for index := 1; index < len(parts); index++ {
			if _, exists := symlinks[strings.Join(parts[:index], "/")]; exists {
				return errors.New("workspace manifest contains a path below a symlink")
			}
		}
	}
	for _, directory := range directories {
		parts := strings.Split(strings.ToLower(directory), "/")
		for index := 1; index <= len(parts); index++ {
			if _, exists := symlinks[strings.Join(parts[:index], "/")]; exists {
				return errors.New("workspace manifest contains a directory below a symlink")
			}
		}
	}
	return nil
}
