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

// WorkspaceCapacity checks the filesystem used by independent Workspace data.
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
	if available < uint64(c.minFreeBytes) {
		return domain.NewError(domain.CodeCapacityExceeded, "workspace filesystem is below its free-space reserve")
	}
	return nil
}

// CheckWorkspaceSyncStorageIsolation requires active mount points for sync roots on distinct filesystems.
func CheckWorkspaceSyncStorageIsolation(blockRoot, currentRoot, dockerRoot, siteRoot, nodeStateRoot, relayStateRoot string, protectedRoots []string) error {
	for _, root := range []struct {
		name string
		path string
	}{{"Workspace blocks", blockRoot}, {"Workspace current files", currentRoot}, {"Workspace node state", nodeStateRoot}, {"Workspace relay state", relayStateRoot}} {
		if err := requireWorkspaceMountPoint(root.path, root.name); err != nil {
			return err
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
	blockDevice, err := workspaceFilesystemDevice(blockRoot, "Workspace blocks")
	if err != nil {
		return err
	}
	if name, exists := protected[blockDevice]; exists {
		return fmt.Errorf("workspace sync block root must use a filesystem separate from %s", name)
	}
	syncDevices := map[uint64]string{blockDevice: "Workspace blocks"}
	for _, root := range []struct {
		name string
		path string
	}{{"Workspace current files", currentRoot}, {"Workspace node state", nodeStateRoot}, {"Workspace relay state", relayStateRoot}} {
		device, err := workspaceFilesystemDevice(root.path, root.name)
		if err != nil {
			return err
		}
		if name, exists := protected[device]; exists {
			return fmt.Errorf("workspace sync %s root must use a filesystem separate from %s", root.name, name)
		}
		if err := addWorkspaceSyncFilesystemDevice(syncDevices, device, root.name); err != nil {
			return err
		}
	}
	return nil
}

// CheckWorkspaceCurrentStorageIsolation keeps the current Workspace tree on a dedicated mount even when sync is disabled.
func CheckWorkspaceCurrentStorageIsolation(blockRoot, currentRoot, dockerRoot, siteRoot string, protectedRoots []string) error {
	if err := requireWorkspaceMountPoint(currentRoot, "Workspace current files"); err != nil {
		return err
	}
	protected := make(map[uint64]string)
	for _, root := range []struct {
		name string
		path string
	}{{"system", string(filepath.Separator)}, {"Docker data", dockerRoot}, {"Site storage", siteRoot}} {
		device, err := workspaceFilesystemDevice(root.path, root.name)
		if err != nil {
			return err
		}
		protected[device] = root.name
	}
	for i, root := range protectedRoots {
		device, err := workspaceFilesystemDevice(root, fmt.Sprintf("protected data %d", i+1))
		if err != nil {
			return err
		}
		protected[device] = fmt.Sprintf("protected data %d", i+1)
	}
	blockDevice, err := workspaceFilesystemDevice(blockRoot, "Workspace blocks")
	if err != nil {
		return err
	}
	protected[blockDevice] = "Workspace blocks"
	currentDevice, err := workspaceFilesystemDevice(currentRoot, "Workspace current files")
	if err != nil {
		return err
	}
	if name, exists := protected[currentDevice]; exists {
		return fmt.Errorf("workspace current-files root must use a filesystem separate from %s", name)
	}
	return nil
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

func addWorkspaceSyncFilesystemDevice(devices map[uint64]string, device uint64, name string) error {
	if previous, exists := devices[device]; exists {
		return fmt.Errorf("workspace sync %s root must use a filesystem separate from %s", name, previous)
	}
	devices[device] = name
	return nil
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
