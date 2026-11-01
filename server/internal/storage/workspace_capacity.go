package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"morde/server/internal/domain"
)

// WorkspaceCapacity checks free space on the shared Workspace data filesystem.
type WorkspaceCapacity struct {
	rootDir      string
	minFreeBytes int64
}

func NewWorkspaceCapacity(rootDir string, minFreeBytes int64) (*WorkspaceCapacity, error) {
	if !filepath.IsAbs(rootDir) || filepath.Clean(rootDir) == string(filepath.Separator) || minFreeBytes <= 0 {
		return nil, fmt.Errorf("workspace storage directory and minimum free bytes are required")
	}
	return &WorkspaceCapacity{rootDir: filepath.Clean(rootDir), minFreeBytes: minFreeBytes}, nil
}

func (c *WorkspaceCapacity) CheckCapacity(ctx context.Context) error {
	return c.checkCapacity(ctx, 0)
}

// CheckCapacityForWrite stops a new shared-volume write before the configured
// free-space reserve would be consumed. The filesystem's own size is the hard
// cap; this is an admission guard, not a physical reservation.
func (c *WorkspaceCapacity) CheckCapacityForWrite(ctx context.Context, incomingBytes int64) error {
	if incomingBytes < 0 {
		return fmt.Errorf("workspace write capacity request must not be negative")
	}
	return c.checkCapacity(ctx, incomingBytes)
}

func (c *WorkspaceCapacity) checkCapacity(ctx context.Context, incomingBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(c.rootDir)
	if err != nil {
		return fmt.Errorf("resolve workspace storage root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect workspace storage root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace storage root is not a directory")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(root, &stats); err != nil {
		return fmt.Errorf("inspect workspace storage capacity: %w", err)
	}
	if stats.Bsize <= 0 {
		return fmt.Errorf("inspect workspace storage capacity: invalid filesystem block size")
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	if available > ^uint64(0)/blockSize {
		available = ^uint64(0)
	} else {
		available *= blockSize
	}
	minimum := uint64(c.minFreeBytes)
	if uint64(incomingBytes) > ^uint64(0)-minimum {
		return domain.NewError(domain.CodeCapacityExceeded, "workspace filesystem is below its free-space reserve")
	}
	minimum += uint64(incomingBytes)
	if available < minimum {
		return domain.NewError(domain.CodeCapacityExceeded, "workspace filesystem is below its free-space reserve")
	}
	return nil
}

// CheckWorkspaceSyncStorageIsolation requires one dedicated hard-capped mount
// for all Workspace sync data, with each data root in a distinct direct-child
// directory. The child directories intentionally share the same filesystem.
func CheckWorkspaceSyncStorageIsolation(dataRoot, blockRoot, currentRoot, dockerRoot, siteRoot, nodeStateRoot, relayStateRoot string, protectedRoots []string) error {
	return checkWorkspaceSyncDataStorage(dataRoot, blockRoot, currentRoot, dockerRoot, siteRoot,
		[]struct {
			name string
			path string
		}{{"Workspace node state", nodeStateRoot}, {"Workspace relay state", relayStateRoot}}, protectedRoots)
}

func checkWorkspaceSyncDataStorage(dataRoot, blockRoot, currentRoot, dockerRoot, siteRoot string, extraRoots []struct {
	name string
	path string
}, protectedRoots []string) error {
	if err := requireWorkspaceMountPoint(dataRoot, "shared data"); err != nil {
		return err
	}
	dataDevice, err := workspaceFilesystemDevice(dataRoot, "shared data")
	if err != nil {
		return err
	}
	dataRoots := []struct {
		name string
		path string
	}{{"Workspace blocks", blockRoot}, {"Workspace current files", currentRoot}}
	dataRoots = append(dataRoots, extraRoots...)
	seenPaths := make(map[string]string, len(dataRoots))
	for _, root := range dataRoots {
		device, err := requireWorkspaceSyncDataDirectory(dataRoot, root.path, root.name)
		if err != nil {
			return err
		}
		if previous, exists := seenPaths[root.path]; exists {
			return fmt.Errorf("workspace sync %s root must use a separate directory from %s", root.name, previous)
		}
		seenPaths[root.path] = root.name
		if device != dataDevice {
			return fmt.Errorf("workspace sync %s root must remain on the shared data filesystem", root.name)
		}
	}
	systemDevice, err := workspaceFilesystemDevice(string(filepath.Separator), "system")
	if err != nil {
		return err
	}
	dockerDevice, err := workspaceFilesystemDevice(dockerRoot, "Docker data")
	if err != nil {
		return err
	}
	protected := map[uint64]string{systemDevice: "system", dockerDevice: "Docker data"}
	siteDevice, err := workspaceFilesystemDevice(siteRoot, "Site storage")
	if err != nil {
		return err
	}
	protected[siteDevice] = "Site storage"
	for i, root := range protectedRoots {
		device, err := workspaceFilesystemDevice(root, fmt.Sprintf("protected data %d", i+1))
		if err != nil {
			return err
		}
		protected[device] = fmt.Sprintf("protected data %d", i+1)
	}
	if name, exists := protected[dataDevice]; exists {
		return fmt.Errorf("workspace sync shared data root must use a filesystem separate from %s", name)
	}
	return nil
}

// CheckWorkspaceCurrentStorageIsolation keeps the shared Workspace data volume
// bound and isolated even when sync is disabled; only blocks/current need exist.
func CheckWorkspaceCurrentStorageIsolation(dataRoot, blockRoot, currentRoot, dockerRoot, siteRoot string, protectedRoots []string) error {
	return checkWorkspaceSyncDataStorage(dataRoot, blockRoot, currentRoot, dockerRoot, siteRoot, nil, protectedRoots)
}

func requireWorkspaceSyncDataDirectory(dataRoot, path, name string) (uint64, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != dataRoot {
		return 0, fmt.Errorf("workspace sync %s root must be a direct child of the shared data root", name)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect workspace sync %s root: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("workspace sync %s root must be a real directory", name)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return 0, fmt.Errorf("workspace sync %s root must not use symlinks", name)
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, fmt.Errorf("inspect Workspace mount table: %w", err)
	}
	if mountInfoHasPath(mountInfo, path) {
		return 0, fmt.Errorf("workspace sync %s root must not be a nested mount", name)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("inspect workspace sync %s filesystem device", name)
	}
	return uint64(stat.Dev), nil
}

func requireWorkspaceMountPoint(path, name string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("workspace sync %s root must be an absolute clean path", name)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve workspace sync %s root: %w", name, err)
	}
	if resolved != path {
		return fmt.Errorf("workspace sync %s root must not use symlinks", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect workspace sync %s root: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace sync %s root is not a directory", name)
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("inspect Workspace mount table: %w", err)
	}
	if !mountInfoHasPath(mountInfo, path) {
		return fmt.Errorf("workspace sync %s root must be an active mount point", name)
	}
	return nil
}

func mountInfoHasPath(mountInfo []byte, path string) bool {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(mountInfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && unescape.Replace(fields[4]) == path {
			return true
		}
	}
	return false
}

func workspaceFilesystemDevice(path, name string) (uint64, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, fmt.Errorf("workspace sync %s root must be an absolute clean path", name)
	}
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			info, err := os.Stat(resolved)
			if err != nil {
				return 0, fmt.Errorf("inspect workspace sync %s filesystem: %w", name, err)
			}
			if !info.IsDir() {
				return 0, fmt.Errorf("workspace sync %s root is not a directory", name)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, fmt.Errorf("inspect workspace sync %s filesystem device", name)
			}
			return uint64(stat.Dev), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("resolve workspace sync %s filesystem: %w", name, err)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, fmt.Errorf("resolve workspace sync %s filesystem: %w", name, err)
		}
		path = parent
	}
}

var _ interface{ CheckCapacity(context.Context) error } = (*WorkspaceCapacity)(nil)
