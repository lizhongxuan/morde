package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type workspaceLiveBlockSource struct {
	path   string
	offset int64
	size   int64
}

type workspaceLiveSnapshot struct {
	files            []workspace.File
	emptyDirectories []string
	directories      []workspace.Directory
	manifestSHA256   string
	blocks           map[string]workspaceLiveBlockSource
	blockedPaths     map[string]struct{}
}

func readWorkspaceLiveSnapshot(ctx context.Context, directory WorkspaceCurrentDirectory, ownerID, workspaceID string, maxBytes int64) (workspaceLiveSnapshot, error) {
	if ctx == nil || directory == nil || !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || maxBytes <= 0 {
		return workspaceLiveSnapshot{}, domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace directory is unavailable")
	}
	result := workspaceLiveSnapshot{
		files: make([]workspace.File, 0), emptyDirectories: make([]string, 0),
		directories: []workspace.Directory{{Path: "."}}, blocks: make(map[string]workspaceLiveBlockSource),
		blockedPaths: make(map[string]struct{}),
	}
	seen := map[string]string{}
	var usedBytes int64
	items := 0
	var walk func(string) error
	walk = func(relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDirectory(ownerID, workspaceID, relative)
		if err != nil {
			return fmt.Errorf("read live Workspace directory %q: %w", relative, err)
		}
		if len(entries) == 0 && relative != "" {
			result.emptyDirectories = append(result.emptyDirectories, relative)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			filePath := entry.Name
			if relative != "" {
				filePath = path.Join(relative, entry.Name)
			}
			if workspace.ValidateRelativePath(filePath) != nil {
				return domain.NewError(domain.CodeWorkspaceConflict, "live Workspace contains an invalid path")
			}
			items++
			if items > workspace.MaxSyncFiles {
				return domain.NewError(domain.CodeRequestTooLarge, "live Workspace contains too many entries for a snapshot")
			}
			folded := strings.ToLower(filePath)
			if previous, exists := seen[folded]; exists && previous != filePath {
				return domain.NewError(domain.CodeWorkspaceConflict, "live Workspace contains paths that collide by case")
			}
			seen[folded] = filePath
			switch entry.Kind {
			case "directory":
				info, err := directory.StatPath(ownerID, workspaceID, filePath)
				if err != nil || !info.IsDir() {
					return domain.NewError(domain.CodeWorkspaceConflict, "live Workspace directory changed while being read")
				}
				mode, modTime := uint32(info.Mode().Perm()), info.ModTime().UnixNano()
				result.directories = append(result.directories, workspace.Directory{Path: filePath, Mode: &mode, ModTimeNS: &modTime, MetadataKnown: true})
				if err := walk(filePath); err != nil {
					return err
				}
			case "file":
				file, info, err := directory.OpenRegularFile(ownerID, workspaceID, filePath)
				if err != nil {
					return fmt.Errorf("open live Workspace file %q: %w", filePath, err)
				}
				if info.Size() < 0 || info.Size() > workspace.MaxSyncFileBytes || info.Size() > maxBytes-usedBytes {
					_ = file.Close()
					return domain.NewError(domain.CodeRequestTooLarge, "live Workspace exceeds the snapshot size limit")
				}
				if info.Size() != entry.SizeBytes {
					_ = file.Close()
					return domain.NewError(domain.CodeWorkspaceConflict, "live Workspace file changed while being read")
				}
				value, err := snapshotWorkspaceLiveFile(filePath, file, info, result.blocks)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					return errors.Join(err, closeErr)
				}
				result.files = append(result.files, value)
				usedBytes += value.SizeBytes
			case "symlink":
				target, err := directory.ReadSymbolicLink(ownerID, workspaceID, filePath)
				if err != nil {
					result.blockedPaths[filePath] = struct{}{}
					continue
				}
				data := []byte(target)
				value := workspace.File{Kind: workspace.FileKindSymlink, Path: filePath, SymlinkTarget: target,
					SizeBytes: int64(len(data)), SHA256: sha256Hex(data), Mode: 0777, Chunks: []workspace.Chunk{}}
				if workspace.ValidateFile(value) == nil {
					result.files = append(result.files, value)
				} else {
					result.blockedPaths[filePath] = struct{}{}
				}
			default:
				result.blockedPaths[filePath] = struct{}{}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return workspaceLiveSnapshot{}, err
	}
	sort.Slice(result.files, func(i, j int) bool { return result.files[i].Path < result.files[j].Path })
	sort.Strings(result.emptyDirectories)
	sort.Slice(result.directories, func(i, j int) bool { return result.directories[i].Path < result.directories[j].Path })
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(result.files, result.emptyDirectories, result.directories)
	if err != nil {
		return workspaceLiveSnapshot{}, domain.NewError(domain.CodeWorkspaceConflict, "live Workspace snapshot is invalid")
	}
	result.manifestSHA256 = manifestSHA
	return result, nil
}

func snapshotWorkspaceLiveFile(filePath string, file *os.File, info os.FileInfo, blocks map[string]workspaceLiveBlockSource) (workspace.File, error) {
	if file == nil || info == nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > workspace.MaxSyncFileBytes {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "live Workspace file type or size is invalid")
	}
	value := workspace.File{Kind: workspace.FileKindRegular, Path: filePath, SizeBytes: info.Size(), Mode: uint32(info.Mode().Perm()), MetadataKnown: true,
		Chunks: make([]workspace.Chunk, 0)}
	modTime := info.ModTime().UnixNano()
	value.ModTimeNS = &modTime
	fileHash := sha256.New()
	for offset := int64(0); offset < info.Size(); {
		size := min(int64(workspace.MaxSyncBlockBytes), info.Size()-offset)
		data := make([]byte, size)
		if _, err := file.ReadAt(data, offset); err != nil {
			return workspace.File{}, fmt.Errorf("read live Workspace file %q: %w", filePath, err)
		}
		_, _ = fileHash.Write(data)
		digest := sha256.Sum256(data)
		hexDigest := hex.EncodeToString(digest[:])
		value.Chunks = append(value.Chunks, workspace.Chunk{SHA256: hexDigest, SizeBytes: size})
		if _, exists := blocks[hexDigest]; !exists {
			blocks[hexDigest] = workspaceLiveBlockSource{path: filePath, offset: offset, size: size}
		}
		offset += size
	}
	value.SHA256 = hex.EncodeToString(fileHash.Sum(nil))
	if err := workspace.ValidateFile(value); err != nil {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "live Workspace file metadata is invalid")
	}
	return value, nil
}

func (snapshot workspaceLiveSnapshot) readBlock(directory WorkspaceCurrentDirectory, ownerID, workspaceID, digest string) ([]byte, error) {
	source, exists := snapshot.blocks[digest]
	if !exists {
		return nil, domain.NewError(domain.CodeNotFound, "live Workspace block is unavailable")
	}
	file, info, err := directory.OpenRegularFile(ownerID, workspaceID, source.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if source.offset < 0 || source.size < 1 || info.Size() < source.offset+source.size {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "live Workspace file changed while storing its snapshot")
	}
	data := make([]byte, source.size)
	if _, err := file.ReadAt(data, source.offset); err != nil {
		return nil, err
	}
	if sha256Hex(data) != digest {
		return nil, domain.NewError(domain.CodeWorkspaceConflict, "live Workspace file changed while storing its snapshot")
	}
	return data, nil
}
