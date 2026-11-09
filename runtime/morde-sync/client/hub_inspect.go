package client

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	HubInspectMaxEntries = 128
	HubInspectMaxBytes   = 32 << 10
	maxHubInspectPath    = 1024
)

var errHubInspect = errors.New("selected directory inspection failed")

type HubRootSelection struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
}

type HubFileEntry struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	SizeBytes int64  `json:"size_bytes"`
}

type HubFileContent struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// SelectHubRoot returns a canonical directory and a stable token derived from
// its platform file identity. The token remains verifiable across helper runs.
func SelectHubRoot(directory string) (HubRootSelection, error) {
	root, canonical, identity, err := openHubInspectionRoot(directory, "")
	if err != nil {
		return HubRootSelection{}, err
	}
	_ = root.Close()
	return HubRootSelection{Directory: canonical, Identity: identity}, nil
}

// ListHubFiles lists one directory beneath the selected root. Symlinks are
// omitted; special files and paths that change during inspection fail closed.
func ListHubFiles(directory, identity, relative string) ([]HubFileEntry, error) {
	if len(identity) != sha256.Size*2 || relative != "" && !validHubInspectPath(relative) {
		return nil, errHubInspect
	}
	root, _, _, err := openHubInspectionRoot(directory, identity)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	dirRoot, closeDir, err := openHubInspectionDirectory(root, relative)
	if err != nil {
		return nil, err
	}
	defer closeDir()
	dir, err := dirRoot.Open(".")
	if err != nil {
		return nil, errHubInspect
	}
	defer dir.Close()
	entries, readErr := dir.ReadDir(HubInspectMaxEntries + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errHubInspect
	}
	if len(entries) > HubInspectMaxEntries {
		entries = entries[:HubInspectMaxEntries]
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	result := make([]HubFileEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !validHubInspectComponent(name) {
			continue
		}
		resultPath := joinHubInspectPath(relative, name)
		if !validHubInspectPath(resultPath) {
			continue
		}
		info, err := dirRoot.Lstat(name)
		if err != nil {
			return nil, errHubInspect
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case info.IsDir():
			child, err := dirRoot.OpenRoot(name)
			if err != nil {
				return nil, errHubInspect
			}
			opened, statErr := child.Stat(".")
			latest, latestErr := dirRoot.Lstat(name)
			_ = child.Close()
			if statErr != nil || latestErr != nil || !opened.IsDir() || !os.SameFile(info, opened) || latest.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, latest) {
				return nil, errHubInspect
			}
			result = append(result, HubFileEntry{Path: resultPath, Kind: "directory", SizeBytes: 0})
		case info.Mode().IsRegular():
			file, err := dirRoot.Open(name)
			if err != nil {
				return nil, errHubInspect
			}
			opened, statErr := file.Stat()
			latest, latestErr := dirRoot.Lstat(name)
			_ = file.Close()
			if statErr != nil || latestErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || latest.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, latest) || opened.Size() < 0 {
				return nil, errHubInspect
			}
			result = append(result, HubFileEntry{Path: resultPath, Kind: "file", SizeBytes: opened.Size()})
		default:
			return nil, errHubInspect
		}
	}
	if err := confirmHubInspectionDirectory(root, relative, dirRoot); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadHubFile reads bounded UTF-8 text from one regular file beneath the root.
func ReadHubFile(directory, identity, relative string, maxBytes int) (HubFileContent, error) {
	if len(identity) != sha256.Size*2 || !validHubInspectPath(relative) || maxBytes < 1 || maxBytes > HubInspectMaxBytes {
		return HubFileContent{}, errHubInspect
	}
	root, _, _, err := openHubInspectionRoot(directory, identity)
	if err != nil {
		return HubFileContent{}, err
	}
	defer root.Close()

	parts := strings.Split(relative, "/")
	parentPath := strings.Join(parts[:len(parts)-1], "/")
	parent, closeParent, err := openHubInspectionDirectory(root, parentPath)
	if err != nil {
		return HubFileContent{}, err
	}
	defer closeParent()
	name := parts[len(parts)-1]
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return HubFileContent{}, errHubInspect
	}
	file, err := parent.Open(name)
	if err != nil {
		return HubFileContent{}, errHubInspect
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return HubFileContent{}, errHubInspect
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return HubFileContent{}, errHubInspect
	}
	latest, err := parent.Lstat(name)
	if err != nil || latest.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, latest) {
		return HubFileContent{}, errHubInspect
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	if !utf8.Valid(data) {
		if !truncated {
			return HubFileContent{}, errHubInspect
		}
		trimmed := false
		for suffix := 1; suffix < utf8.UTFMax && suffix <= len(data); suffix++ {
			prefixLength := len(data) - suffix
			if utf8.Valid(data[:prefixLength]) && !utf8.FullRune(data[prefixLength:]) {
				data = data[:prefixLength]
				trimmed = true
				break
			}
		}
		if !trimmed {
			return HubFileContent{}, errHubInspect
		}
	}
	if err := confirmHubInspectionDirectory(root, parentPath, parent); err != nil {
		return HubFileContent{}, err
	}
	return HubFileContent{Path: relative, Text: string(data), Truncated: truncated}, nil
}

func openHubInspectionRoot(directory, expectedIdentity string) (*os.Root, string, string, error) {
	if directory == "" || len(directory) > 4096 || !utf8.ValidString(directory) || strings.ContainsRune(directory, 0) || !filepath.IsAbs(directory) {
		return nil, "", "", errHubInspect
	}
	abs := filepath.Clean(directory)
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", "", errHubInspect
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", "", errHubInspect
	}
	canonical = filepath.Clean(canonical)
	if len(canonical) > 4096 || !utf8.ValidString(canonical) {
		return nil, "", "", errHubInspect
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil || !canonicalInfo.IsDir() || canonicalInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, canonicalInfo) {
		return nil, "", "", errHubInspect
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, "", "", errHubInspect
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(canonicalInfo, openedInfo) {
		_ = root.Close()
		return nil, "", "", errHubInspect
	}
	fileIdentity, err := hubDirectoryFileIdentity(root)
	if err != nil {
		_ = root.Close()
		return nil, "", "", errHubInspect
	}
	identity := hubIdentityToken(canonical, fileIdentity)
	if expectedIdentity != "" && (len(expectedIdentity) != sha256.Size*2 || subtle.ConstantTimeCompare([]byte(identity), []byte(expectedIdentity)) != 1) {
		_ = root.Close()
		return nil, "", "", errHubInspect
	}
	return root, canonical, identity, nil
}

func hubIdentityToken(directory, fileIdentity string) string {
	digest := sha256.Sum256([]byte("morde-hub-root-v1\x00" + directory + "\x00" + fileIdentity))
	return hex.EncodeToString(digest[:])
}

func openHubInspectionDirectory(root *os.Root, relative string) (*os.Root, func(), error) {
	if relative == "" {
		return root, func() {}, nil
	}
	if root == nil || !validHubInspectPath(relative) {
		return nil, func() {}, errHubInspect
	}
	owned := make([]*os.Root, 0, strings.Count(relative, "/")+1)
	closeOwned := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = owned[index].Close()
		}
	}
	current := root
	for _, part := range strings.Split(relative, "/") {
		info, err := current.Lstat(part)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeOwned()
			return nil, func() {}, errHubInspect
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			closeOwned()
			return nil, func() {}, errHubInspect
		}
		opened, statErr := child.Stat(".")
		latest, latestErr := current.Lstat(part)
		if statErr != nil || latestErr != nil || !opened.IsDir() || !os.SameFile(info, opened) || latest.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, latest) {
			_ = child.Close()
			closeOwned()
			return nil, func() {}, errHubInspect
		}
		owned = append(owned, child)
		current = child
	}
	return current, closeOwned, nil
}

func confirmHubInspectionDirectory(root *os.Root, relative string, expected *os.Root) error {
	current, closeCurrent, err := openHubInspectionDirectory(root, relative)
	if err != nil {
		return err
	}
	defer closeCurrent()
	before, err := expected.Stat(".")
	if err != nil {
		return errHubInspect
	}
	after, err := current.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		return errHubInspect
	}
	return nil
}

func validHubInspectPath(value string) bool {
	if value == "" || len(value) > maxHubInspectPath || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00:") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validHubInspectComponent(value string) bool {
	return value != "" && value != "." && value != ".." && utf8.ValidString(value) && !strings.ContainsAny(value, "\\\x00:") && !strings.Contains(value, "/")
}

func joinHubInspectPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
