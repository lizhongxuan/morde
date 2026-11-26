package dshguardian

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"morde/server/internal/app"
)

const (
	maxTaskProcessSnapshotProcesses = 2048
	maxTaskProcessSnapshotCgroups   = 2048
)

// TaskProcessSnapshotReader exposes the Guardian's read-only Task process
// snapshot operation to the administrator workflow-trace adapter.
type TaskProcessSnapshotReader interface {
	SnapshotTaskProcess(context.Context, app.AccountAgentRuntimeFence, app.AgentInstanceFence) (TaskProcessSnapshot, error)
}

// ValidateTaskProcessSnapshot checks the scoped process identities and the
// snapshot's canonical integrity digest. It does not claim that the snapshot
// is an immutable execution-time record.
func ValidateTaskProcessSnapshot(value TaskProcessSnapshot) error {
	if value.RuntimeFence.Validate() != nil || value.TaskFence.Validate() != nil ||
		value.RuntimeFence.OwnerID != value.TaskFence.OwnerID || !containerIDRE.MatchString(value.RuntimeContainerID) || value.CapturedAt.IsZero() ||
		value.RootPID < 2 || value.RootStartTimeTick == 0 || !value.Complete ||
		len(value.Processes) == 0 || len(value.Processes) > maxTaskProcessSnapshotProcesses {
		return errors.New("task process snapshot identity or completeness is invalid")
	}

	seen := make(map[int]struct{}, len(value.Processes))
	rootFound := false
	lastPID := 0
	for _, process := range value.Processes {
		if process.PID < 1 || process.PID <= lastPID || process.StartTimeTick == 0 || len(process.State) != 1 ||
			process.ParentPID < 0 || (process.ParentPID == 0) != process.ParentOutsideScope {
			return errors.New("task process snapshot contains an invalid process entry")
		}
		lastPID = process.PID
		seen[process.PID] = struct{}{}
		if process.PID == value.RootPID {
			if process.StartTimeTick != value.RootStartTimeTick {
				return errors.New("task process snapshot root identity changed")
			}
			rootFound = true
		}
	}
	if !rootFound {
		return errors.New("task process snapshot omits its fenced root process")
	}
	for _, process := range value.Processes {
		if process.ParentPID > 0 {
			if _, ok := seen[process.ParentPID]; !ok {
				return errors.New("task process snapshot parent is outside its scoped process list")
			}
		}
	}
	if value.IntegritySHA256 != taskProcessSnapshotIntegritySHA256(value) {
		return errors.New("task process snapshot integrity digest does not match")
	}
	return nil
}

func taskProcessSnapshotIntegritySHA256(value TaskProcessSnapshot) string {
	value.IntegritySHA256 = ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func newTaskProcessSnapshot(runtimeFence app.AccountAgentRuntimeFence, taskFence app.AgentInstanceFence, runtimeContainerID string,
	rootPID int, rootStartTime uint64, processes []TaskProcessSnapshotEntry, capturedAt time.Time) (TaskProcessSnapshot, error) {
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	value := TaskProcessSnapshot{RuntimeFence: runtimeFence, TaskFence: taskFence, RuntimeContainerID: runtimeContainerID, CapturedAt: capturedAt.UTC(),
		RootPID: rootPID, RootStartTimeTick: rootStartTime, Complete: true, Processes: processes}
	value.IntegritySHA256 = taskProcessSnapshotIntegritySHA256(value)
	if err := ValidateTaskProcessSnapshot(value); err != nil {
		return TaskProcessSnapshot{}, fmt.Errorf("validate captured Task process snapshot: %w", err)
	}
	return value, nil
}
