// Package runtimefiles implements bounded file actions confined to a Host's
// mounted Workspace.
package runtimefiles

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"morde-runtime/runtimecore"
)

const (
	tempFilePrefix      = ".morde-tmp-"
	maxWriteBytes       = 1 << 20
	maxListEntries      = 500
	maxListScanned      = 10_000
	maxFindEntries      = 10_000
	maxFindMatches      = 500
	maxFindDepth        = 64
	maxReadLineBytes    = 8 << 20
	maxReadScanBytes    = 64 << 20
	maxFindDirectories  = 10_000
	maxWriteTempRetries = 3
	maxParallelActions  = 8
	maxVerifyFiles      = 16
	maxVerifyFileBytes  = 1 << 20
	maxVerifyTotalBytes = 8 << 20
)

var (
	ErrInvalidWorkspace = errors.New("invalid workspace root")
	ErrInvalidRequest   = errors.New("invalid file action request")
	ErrUnsafePath       = errors.New("unsafe workspace path")
	ErrFileUnavailable  = errors.New("workspace file unavailable")
)

// Workspace is a safe root handle. os.Root confines every operation beneath
// the opened directory, including when a path component changes concurrently.
type Workspace struct {
	root         *os.Root
	taskRoot     *os.Root
	rootPath     string
	taskRootPath string
	taskViewUID  uint32
	taskViewGID  uint32
	taskViewMu   sync.Mutex
	writeGate    chan struct{}
	parallel     chan struct{}
	closed       bool
	mu           sync.RWMutex
	registry     *operationRegistry
}

// OpenWorkspace opens a directory as the only root available to file actions.
// The root path itself must be absolute and clean so configuration errors fail
// before any action is advertised.
func OpenWorkspace(rootPath string) (*Workspace, error) {
	return openWorkspace(rootPath, "", 0, 0)
}

// OpenTaskWorkspace opens the Host Workspace source and a separate persistent
// root for Task views. The view root must be outside every synced Workspace.
func OpenTaskWorkspace(rootPath, taskRootPath string, viewUID, viewGID uint32) (*Workspace, error) {
	if !filepath.IsAbs(taskRootPath) || filepath.Clean(taskRootPath) != taskRootPath {
		return nil, ErrInvalidWorkspace
	}
	if err := ensurePrivateDirectory(taskRootPath); err != nil {
		return nil, err
	}
	return openWorkspace(rootPath, taskRootPath, viewUID, viewGID)
}

func openWorkspace(rootPath, taskRootPath string, viewUID, viewGID uint32) (*Workspace, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, ErrInvalidWorkspace
	}
	rootInfo, err := os.Lstat(rootPath)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidWorkspace
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, ErrInvalidWorkspace
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || !os.SameFile(rootInfo, info) {
		_ = root.Close()
		return nil, ErrInvalidWorkspace
	}
	workspace := &Workspace{
		root: root, rootPath: rootPath, taskRootPath: taskRootPath, taskViewUID: viewUID, taskViewGID: viewGID,
		writeGate: make(chan struct{}, 1), parallel: make(chan struct{}, maxParallelActions), registry: newOperationRegistry(),
	}
	if taskRootPath != "" {
		workspace.taskRoot, err = os.OpenRoot(taskRootPath)
		if err != nil {
			_ = root.Close()
			return nil, ErrInvalidWorkspace
		}
	}
	return workspace, nil
}

// Handlers returns the file actions supported by this Workspace.
func (w *Workspace) Handlers() []runtimecore.ActionHandler {
	return []runtimecore.ActionHandler{
		&actionHandler{workspace: w, action: "read"},
		&actionHandler{workspace: w, action: "write"},
		&actionHandler{workspace: w, action: "list"},
		&actionHandler{workspace: w, action: "find"},
		&actionHandler{workspace: w, action: "verify"},
	}
}

// Close releases the rooted directory handle. Call it after the runtime
// manager has stopped all operations using these handlers.
func (w *Workspace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.taskRoot != nil {
		err = w.taskRoot.Close()
	}
	return errors.Join(err, w.root.Close())
}

func (w *Workspace) getRoot() (*os.Root, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return nil, ErrFileUnavailable
	}
	return w.root, nil
}

// getTaskWorkspaceRoot opens only the exact view injected by Runtimecore. A
// missing view fails closed; it never falls back to the shared Host directory.
func (w *Workspace) getTaskWorkspaceRoot(taskID, viewID, directory string) (*os.Root, error) {
	if w.taskRoot == nil || !validTaskWorkspaceDirectory(taskID, viewID) || directory != taskID+"/"+viewID {
		return nil, ErrFileUnavailable
	}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return nil, ErrFileUnavailable
	}
	root := w.taskRoot
	w.mu.RUnlock()
	taskInfo, err := root.Lstat(taskID)
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode()&os.ModeSymlink != 0 || taskInfo.Mode().Perm() != 0o700 {
		return nil, ErrFileUnavailable
	}
	taskRoot, err := openWorkspaceRoot(root, taskID, taskInfo)
	if err != nil {
		return nil, ErrFileUnavailable
	}
	viewInfo, err := taskRoot.Lstat(viewID)
	if err != nil || !viewInfo.IsDir() || viewInfo.Mode()&os.ModeSymlink != 0 || viewInfo.Mode().Perm() != 0o770 || !ownedBy(viewInfo, w.taskViewUID) {
		_ = taskRoot.Close()
		return nil, ErrFileUnavailable
	}
	viewContainer, err := openWorkspaceRoot(taskRoot, viewID, viewInfo)
	if err != nil {
		_ = taskRoot.Close()
		return nil, ErrFileUnavailable
	}
	contentInfo, err := viewContainer.Lstat(taskWorkspaceContentDirectory)
	if err != nil || !contentInfo.IsDir() || contentInfo.Mode()&os.ModeSymlink != 0 || !ownedBy(contentInfo, w.taskViewUID) {
		_ = taskRoot.Close()
		_ = viewContainer.Close()
		return nil, ErrFileUnavailable
	}
	viewRoot, err := openWorkspaceRoot(viewContainer, taskWorkspaceContentDirectory, contentInfo)
	_ = viewContainer.Close()
	_ = taskRoot.Close()
	if err != nil {
		return nil, ErrFileUnavailable
	}
	return viewRoot, nil
}

func openWorkspaceRoot(root *os.Root, directory string, expected os.FileInfo) (*os.Root, error) {
	workspaceRoot, err := root.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	opened, err := workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		_ = workspaceRoot.Close()
		return nil, ErrFileUnavailable
	}
	return workspaceRoot, nil
}

func validWorkspaceDirectory(directory string) bool {
	if len(directory) != 8 || directory[0] != 'w' {
		return false
	}
	for _, char := range directory[1:] {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}
