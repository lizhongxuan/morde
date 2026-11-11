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
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	HubInspectMaxEntries    = 128
	HubInspectMaxBytes      = 32 << 10
	HubInspectMaxChunkBytes = 4 << 20
	maxHubInspectPath       = 1024
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

type HubFileSnapshot struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Version   string `json:"version"`
}

type HubFileChunk struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Data    []byte `json:"data"`
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

// SnapshotHubFile computes a content digest without retaining the file in memory.
func SnapshotHubFile(directory, identity, relative string) (HubFileSnapshot, error) {
	file, err := openHubInspectionFile(directory, identity, relative)
	if err != nil {
		return HubFileSnapshot{}, err
	}
	defer file.close()

	digest, _, size, err := hashHubInspectionFile(file.file, file.info.Size(), 0, 0)
	if err != nil || size != file.info.Size() || file.verify() != nil {
		return HubFileSnapshot{}, errHubInspect
	}
	sha := hex.EncodeToString(digest)
	return HubFileSnapshot{
		Path: relative, SizeBytes: size, SHA256: sha,
		Version: hubFileSnapshotVersion(identity, relative, file.info, sha),
	}, nil
}

// ReadHubFileChunk verifies the full snapshot while retaining at most one bounded chunk.
func ReadHubFileChunk(directory, identity, relative, version string, offset int64, maxBytes int) (HubFileChunk, error) {
	if len(version) != sha256.Size*2 || maxBytes < 1 || maxBytes > HubInspectMaxChunkBytes || offset < 0 {
		return HubFileChunk{}, errHubInspect
	}
	if _, err := hex.DecodeString(version); err != nil {
		return HubFileChunk{}, errHubInspect
	}
	file, err := openHubInspectionFile(directory, identity, relative)
	if err != nil {
		return HubFileChunk{}, err
	}
	defer file.close()

	size := file.info.Size()
	if offset > size {
		return HubFileChunk{}, errHubInspect
	}
	length := min(int64(maxBytes), size-offset)
	digest, data, readSize, err := hashHubInspectionFile(file.file, size, offset, length)
	if err != nil || readSize != size || file.verify() != nil {
		return HubFileChunk{}, errHubInspect
	}
	sha := hex.EncodeToString(digest)
	actualVersion := hubFileSnapshotVersion(identity, relative, file.info, sha)
	if subtle.ConstantTimeCompare([]byte(actualVersion), []byte(version)) != 1 {
		return HubFileChunk{}, errHubInspect
	}
	chunkDigest := sha256.Sum256(data)
	return HubFileChunk{Path: relative, Offset: offset, Version: actualVersion, SHA256: hex.EncodeToString(chunkDigest[:]), Data: data}, nil
}

type hubInspectionFile struct {
	directory   string
	identity    string
	parentPath  string
	name        string
	root        *os.Root
	parent      *os.Root
	closeParent func()
	file        *os.File
	info        os.FileInfo
}

func openHubInspectionFile(directory, identity, relative string) (*hubInspectionFile, error) {
	if len(identity) != sha256.Size*2 || !validHubInspectPath(relative) {
		return nil, errHubInspect
	}
	root, canonical, _, err := openHubInspectionRoot(directory, identity)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(relative, "/")
	parentPath := strings.Join(parts[:len(parts)-1], "/")
	parent, closeParent, err := openHubInspectionDirectory(root, parentPath)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	name := parts[len(parts)-1]
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		closeParent()
		_ = root.Close()
		return nil, errHubInspect
	}
	openedFile, err := parent.Open(name)
	if err != nil {
		closeParent()
		_ = root.Close()
		return nil, errHubInspect
	}
	openedInfo, err := openedFile.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() < 0 || !os.SameFile(info, openedInfo) {
		_ = openedFile.Close()
		closeParent()
		_ = root.Close()
		return nil, errHubInspect
	}
	return &hubInspectionFile{
		directory: canonical, identity: identity, parentPath: parentPath, name: name,
		root: root, parent: parent, closeParent: closeParent, file: openedFile, info: openedInfo,
	}, nil
}

func (f *hubInspectionFile) close() {
	_ = f.file.Close()
	f.closeParent()
	_ = f.root.Close()
}

func (f *hubInspectionFile) verify() error {
	opened, err := f.file.Stat()
	if err != nil || !sameHubInspectionFileInfo(f.info, opened) {
		return errHubInspect
	}
	latest, err := f.parent.Lstat(f.name)
	if err != nil || latest.Mode()&os.ModeSymlink != 0 || !sameHubInspectionFileInfo(f.info, latest) {
		return errHubInspect
	}
	if err := confirmHubInspectionDirectory(f.root, f.parentPath, f.parent); err != nil || confirmHubInspectionRoot(f.directory, f.identity, f.root) != nil {
		return errHubInspect
	}
	return nil
}

func sameHubInspectionFileInfo(left, right os.FileInfo) bool {
	return left != nil && right != nil && left.Mode().IsRegular() && right.Mode().IsRegular() &&
		left.Size() == right.Size() && left.ModTime().Equal(right.ModTime()) && os.SameFile(left, right)
}

func confirmHubInspectionRoot(directory, identity string, expected *os.Root) error {
	current, _, _, err := openHubInspectionRoot(directory, identity)
	if err != nil {
		return errHubInspect
	}
	defer current.Close()
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

func hubFileSnapshotVersion(identity, relative string, info os.FileInfo, sha string) string {
	value := "morde-hub-file-v1\x00" + identity + "\x00" + relative + "\x00" +
		strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "\x00" + sha
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func hashHubInspectionFile(file *os.File, size, offset, length int64) ([]byte, []byte, int64, error) {
	if size < 0 || offset < 0 || offset > size || length < 0 || length > int64(HubInspectMaxChunkBytes) || length > size-offset {
		return nil, nil, 0, errHubInspect
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, 0, errHubInspect
	}
	hasher := sha256.New()
	buffer := make([]byte, 64<<10)
	chunk := make([]byte, int(length))
	var read int64
	for {
		count, err := file.Read(buffer)
		if count > 0 {
			if _, writeErr := hasher.Write(buffer[:count]); writeErr != nil {
				return nil, nil, 0, errHubInspect
			}
			start := max(offset, read)
			end := min(offset+length, read+int64(count))
			if start < end {
				copy(chunk[int(start-offset):int(end-offset)], buffer[int(start-read):int(end-read)])
			}
			read += int64(count)
		}
		if err == io.EOF {
			break
		}
		if err != nil || count == 0 {
			return nil, nil, 0, errHubInspect
		}
	}
	return hasher.Sum(nil), chunk, read, nil
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
