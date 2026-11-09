package runtimefiles

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const taskWorkspaceDeletionVersion = "morde-task-workspace-view-deletion/v1"

type taskWorkspaceDeletionTombstone struct {
	Version   string `json:"version"`
	HostID    string `json:"host_id"`
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
}

func (w *Workspace) DeleteTaskWorkspaceView(ctx context.Context, hostID, taskID, requestID string) error {
	if ctx == nil || !validHostDirectory(hostID) || !validTaskID(taskID) || !validDeletionRequestID(requestID) || w.taskRoot == nil {
		return ErrTaskWorkspaceView
	}
	w.taskViewMu.Lock()
	defer w.taskViewMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	name := taskWorkspaceDeletionTombstoneName(taskID)
	tombstone := taskWorkspaceDeletionTombstone{
		Version: taskWorkspaceDeletionVersion, HostID: hostID, TaskID: taskID, RequestID: requestID,
	}
	stored, exists, err := readTaskWorkspaceDeletionTombstone(w.taskRoot, name, taskID)
	if err != nil {
		return err
	}
	if exists {
		if stored != tombstone {
			return ErrTaskWorkspaceView
		}
	} else if err := writeTaskWorkspaceDeletionTombstone(w.taskRoot, name, taskID, tombstone); err != nil {
		return err
	}

	// The durable tombstone is written first, so an interrupted removal cannot
	// be followed by a later PrepareTaskWorkspaceView recreating the data.
	if err := ctx.Err(); err != nil {
		return err
	}
	taskInfo, err := w.taskRoot.Lstat(taskID)
	if errors.Is(err, os.ErrNotExist) {
		return syncTaskWorkspaceRoot(w.taskRoot)
	}
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode()&os.ModeSymlink != 0 || taskInfo.Mode().Perm() != 0o700 || !ownedBy(taskInfo, uint32(os.Geteuid())) {
		return ErrTaskWorkspaceView
	}
	if err := w.taskRoot.RemoveAll(taskID); err != nil {
		return ErrTaskWorkspaceView
	}
	return syncTaskWorkspaceRoot(w.taskRoot)
}

func taskWorkspaceDeletionTombstoneName(taskID string) string {
	return taskID + ".deleted.json"
}

func readTaskWorkspaceDeletionTombstone(root *os.Root, name, taskID string) (taskWorkspaceDeletionTombstone, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return taskWorkspaceDeletionTombstone{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !ownedBy(info, uint32(os.Geteuid())) {
		return taskWorkspaceDeletionTombstone{}, false, ErrTaskWorkspaceView
	}
	file, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return taskWorkspaceDeletionTombstone{}, false, ErrTaskWorkspaceView
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(file), 4097))
	decoder.DisallowUnknownFields()
	var tombstone taskWorkspaceDeletionTombstone
	if err := decoder.Decode(&tombstone); err != nil || ensureMetadataEOF(decoder) != nil ||
		tombstone.Version != taskWorkspaceDeletionVersion || tombstone.TaskID != taskID ||
		!validHostDirectory(tombstone.HostID) || !validDeletionRequestID(tombstone.RequestID) {
		return taskWorkspaceDeletionTombstone{}, false, ErrTaskWorkspaceView
	}
	return tombstone, true, nil
}

func writeTaskWorkspaceDeletionTombstone(root *os.Root, name, taskID string, tombstone taskWorkspaceDeletionTombstone) error {
	encoded, err := json.Marshal(tombstone)
	if err != nil || len(encoded) > 4096 {
		return ErrTaskWorkspaceView
	}
	digest := sha256.Sum256([]byte(tombstone.RequestID))
	temporary := "." + taskID + ".deleted-" + hex.EncodeToString(digest[:8]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := root.Lstat(temporary)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !ownedBy(info, uint32(os.Geteuid())) {
			return ErrTaskWorkspaceView
		}
		if removeErr := root.Remove(temporary); removeErr != nil {
			return ErrTaskWorkspaceView
		}
		file, err = root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return ErrTaskWorkspaceView
	}
	_, writeErr := file.Write(encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = root.Remove(temporary)
		return ErrTaskWorkspaceView
	}
	// Link is an atomic no-replace publish: a concurrent Runtime generation
	// cannot overwrite a tombstone for a different confirmed request.
	if err := root.Link(temporary, name); err != nil {
		_ = root.Remove(temporary)
		return ErrTaskWorkspaceView
	}
	if err := root.Remove(temporary); err != nil {
		return ErrTaskWorkspaceView
	}
	return syncTaskWorkspaceRoot(root)
}

func syncTaskWorkspaceRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return ErrTaskWorkspaceView
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return ErrTaskWorkspaceView
	}
	return nil
}

func validTaskID(value string) bool {
	return len(value) == 8 && value[0] == 'q' && validIdentifierTail(value[1:])
}

func validDeletionRequestID(value string) bool {
	if len(value) != 8 || value[0] != 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}
