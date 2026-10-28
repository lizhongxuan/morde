package dshguardian

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"morde/server/internal/app"
)

const (
	containerIDPattern       = `^[a-f0-9]{64}$`
	containerNamePrefix      = "morde-dsh-"
	accountRuntimeNamePrefix = "morde-account-runtime-"
	maxRequestBytes          = 1 << 20
)

var (
	containerIDRE = regexp.MustCompile(containerIDPattern)
	nonceHashRE   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Guard is the wire and storage identity shared with the DSH supervisor.
// The caller's signing key and raw nonce are intentionally not accepted.
type Guard struct {
	ContainerID   string                 `json:"container_id"`
	ContainerName string                 `json:"container_name"`
	Fence         app.AgentInstanceFence `json:"fence"`
	NonceSHA256   string                 `json:"nonce_sha256"`
	ExpiresAt     time.Time              `json:"expires_at"`
}

// AccountRuntimeGuard binds one account Runtime container to its app fence and
// supervisor nonce digest. The raw nonce never crosses the Guardian boundary.
type AccountRuntimeGuard struct {
	ContainerID   string                       `json:"container_id"`
	ContainerName string                       `json:"container_name"`
	Fence         app.AccountAgentRuntimeFence `json:"fence"`
	NonceSHA256   string                       `json:"nonce_sha256"`
	ExpiresAt     time.Time                    `json:"expires_at"`
}

// TaskProcessGuard identifies one child process inside an account Runtime.
// ContainerPID is an untrusted namespace PID hint that Guardian resolves itself.
type TaskProcessGuard struct {
	RuntimeFence       app.AccountAgentRuntimeFence `json:"runtime_fence"`
	TaskFence          app.AgentInstanceFence       `json:"task_fence"`
	RuntimeContainerID string                       `json:"runtime_container_id"`
	ContainerPID       int                          `json:"container_pid"`
	NonceSHA256        string                       `json:"nonce_sha256"`
	ExpiresAt          time.Time                    `json:"expires_at"`
}

type processProof struct {
	PID           int    `json:"pid,omitempty"`
	StartTimeTick uint64 `json:"start_time_ticks,omitempty"`
	CgroupPath    string `json:"cgroup_path,omitempty"`
}

type taskProcessProof struct {
	ContainerPID   int    `json:"container_pid"`
	HostPID        int    `json:"host_pid"`
	StartTimeTick  uint64 `json:"start_time_ticks"`
	CgroupPath     string `json:"cgroup_path"`
	NamespaceInode uint64 `json:"namespace_inode"`
}

type accountRuntimeLease struct {
	Guard           AccountRuntimeGuard `json:"guard"`
	Process         processProof        `json:"process,omitempty"`
	Started         bool                `json:"started,omitempty"`
	UnprovenRunning bool                `json:"unproven_running,omitempty"`
	Destroyed       bool                `json:"destroyed,omitempty"`
	DestroyedAt     time.Time           `json:"destroyed_at,omitempty"`
}

type taskProcessLease struct {
	Guard           TaskProcessGuard `json:"guard"`
	Key             string           `json:"key"`
	Process         taskProcessProof `json:"process,omitempty"`
	Armed           bool             `json:"armed,omitempty"`
	Started         bool             `json:"started,omitempty"`
	UnprovenRunning bool             `json:"unproven_running,omitempty"`
	Destroyed       bool             `json:"destroyed,omitempty"`
	DestroyedAt     time.Time        `json:"destroyed_at,omitempty"`
}

type lease struct {
	Guard           Guard        `json:"guard"`
	Process         processProof `json:"process,omitempty"`
	Started         bool         `json:"started,omitempty"`
	UnprovenRunning bool         `json:"unproven_running,omitempty"`
	Destroyed       bool         `json:"destroyed,omitempty"`
	DestroyedAt     time.Time    `json:"destroyed_at,omitempty"`
}

func (g Guard) validateIdentity() error {
	if err := g.Fence.Validate(); err != nil {
		return fmt.Errorf("invalid dsh fence: %w", err)
	}
	if !containerIDRE.MatchString(g.ContainerID) || !nonceHashRE.MatchString(g.NonceSHA256) || g.ContainerName != expectedContainerName(g.Fence, g.NonceSHA256) {
		return errors.New("dsh container identity is invalid")
	}
	return nil
}

func (g Guard) validateNew(now time.Time) error {
	if err := g.validateIdentity(); err != nil {
		return err
	}
	remaining := g.ExpiresAt.Sub(now)
	if remaining <= 0 || remaining > app.MaxAgentLease {
		return errors.New("dsh lease expiry is outside the allowed window")
	}
	return nil
}

func (g Guard) sameIdentity(other Guard) bool {
	return g.ContainerID == other.ContainerID && g.ContainerName == other.ContainerName && g.Fence == other.Fence && g.NonceSHA256 == other.NonceSHA256
}

func (g AccountRuntimeGuard) validateIdentity() error {
	if err := g.Fence.Validate(); err != nil {
		return fmt.Errorf("invalid account Runtime fence: %w", err)
	}
	if !containerIDRE.MatchString(g.ContainerID) || !nonceHashRE.MatchString(g.NonceSHA256) || g.ContainerName != expectedAccountRuntimeContainerName(g.Fence, g.NonceSHA256) {
		return errors.New("account Runtime container identity is invalid")
	}
	return nil
}

func (g AccountRuntimeGuard) validateNew(now time.Time) error {
	if err := g.validateIdentity(); err != nil {
		return err
	}
	remaining := g.ExpiresAt.Sub(now)
	if remaining <= 0 || remaining > app.MaxAgentLease {
		return errors.New("account Runtime lease expiry is outside the allowed window")
	}
	return nil
}

func (g AccountRuntimeGuard) sameIdentity(other AccountRuntimeGuard) bool {
	return g.ContainerID == other.ContainerID && g.ContainerName == other.ContainerName && g.Fence == other.Fence && g.NonceSHA256 == other.NonceSHA256
}

func (g TaskProcessGuard) validateIdentity() error {
	if err := g.RuntimeFence.Validate(); err != nil {
		return fmt.Errorf("invalid account Runtime fence: %w", err)
	}
	if err := g.TaskFence.Validate(); err != nil {
		return fmt.Errorf("invalid Task fence: %w", err)
	}
	if g.RuntimeFence.OwnerID != g.TaskFence.OwnerID || !containerIDRE.MatchString(g.RuntimeContainerID) ||
		g.ContainerPID < 2 || !nonceHashRE.MatchString(g.NonceSHA256) {
		return errors.New("task process identity is invalid")
	}
	return nil
}

func (g TaskProcessGuard) validateNew(now time.Time) error {
	if err := g.validateIdentity(); err != nil {
		return err
	}
	remaining := g.ExpiresAt.Sub(now)
	if remaining <= 0 || remaining > app.MaxAgentLease {
		return errors.New("task process lease expiry is outside the allowed window")
	}
	return nil
}

func (g TaskProcessGuard) sameIdentity(other TaskProcessGuard) bool {
	return g.RuntimeFence == other.RuntimeFence && g.TaskFence == other.TaskFence && g.RuntimeContainerID == other.RuntimeContainerID &&
		g.ContainerPID == other.ContainerPID && g.NonceSHA256 == other.NonceSHA256
}

func expectedAccountRuntimeContainerName(fence app.AccountAgentRuntimeFence, nonceHash string) string {
	encoded, err := json.Marshal(fence)
	if err != nil {
		return ""
	}
	nonce, err := hex.DecodeString(nonceHash)
	if err != nil {
		return ""
	}
	identity := sha256.Sum256(append(append(encoded, 0), nonce...))
	return accountRuntimeNamePrefix + hex.EncodeToString(identity[:])
}

func taskProcessKey(guard TaskProcessGuard) string {
	identity, err := json.Marshal(struct {
		RuntimeFence       app.AccountAgentRuntimeFence `json:"runtime_fence"`
		TaskFence          app.AgentInstanceFence       `json:"task_fence"`
		RuntimeContainerID string                       `json:"runtime_container_id"`
		NonceSHA256        string                       `json:"nonce_sha256"`
	}{
		RuntimeFence: guard.RuntimeFence, TaskFence: guard.TaskFence,
		RuntimeContainerID: guard.RuntimeContainerID, NonceSHA256: guard.NonceSHA256,
	})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}

func expectedContainerName(fence app.AgentInstanceFence, nonceHash string) string {
	encoded, err := json.Marshal(fence)
	if err != nil {
		return ""
	}
	nonce, err := hex.DecodeString(nonceHash)
	if err != nil {
		return ""
	}
	identity := sha256.Sum256(append(append(encoded, 0), nonce...))
	return containerNamePrefix + hex.EncodeToString(identity[:])
}

func validPersistedLease(value lease) error {
	if err := value.Guard.validateIdentity(); err != nil {
		return err
	}
	if value.Guard.ExpiresAt.IsZero() {
		return errors.New("persisted dsh lease has no expiry")
	}
	if value.Started {
		if value.Process.PID < 1 || value.Process.StartTimeTick == 0 || !containerCgroupMatches(value.Process.CgroupPath, value.Guard.ContainerID) {
			return errors.New("persisted dsh process evidence is invalid")
		}
	} else if value.Process != (processProof{}) {
		return errors.New("unstarted dsh lease contains process evidence")
	}
	if !value.Destroyed && !value.DestroyedAt.IsZero() || value.Destroyed && value.UnprovenRunning {
		return errors.New("persisted dsh destruction state is inconsistent")
	}
	return nil
}

func validPersistedAccountRuntimeLease(value accountRuntimeLease) error {
	if err := value.Guard.validateIdentity(); err != nil || value.Guard.ExpiresAt.IsZero() {
		return errors.New("persisted account Runtime identity is invalid")
	}
	if value.Started {
		if value.Process.PID < 2 || value.Process.StartTimeTick == 0 || !containerCgroupMatches(value.Process.CgroupPath, value.Guard.ContainerID) {
			return errors.New("persisted account Runtime process evidence is invalid")
		}
	} else if value.Process != (processProof{}) {
		return errors.New("unstarted account Runtime contains process evidence")
	}
	if !value.Destroyed && !value.DestroyedAt.IsZero() || value.Destroyed && value.UnprovenRunning {
		return errors.New("persisted account Runtime stop state is inconsistent")
	}
	return nil
}

func validPersistedTaskProcessLease(value taskProcessLease) error {
	if err := value.Guard.validateIdentity(); err != nil || value.Guard.ExpiresAt.IsZero() || !validTaskKey(value.Key) || value.Key != taskProcessKey(value.Guard) {
		return errors.New("persisted task process identity is invalid")
	}
	if value.Armed {
		if value.Process.ContainerPID != value.Guard.ContainerPID || value.Process.HostPID < 2 || value.Process.StartTimeTick == 0 || value.Process.NamespaceInode == 0 ||
			!validCgroupPath(value.Process.CgroupPath) || filepath.Base(value.Process.CgroupPath) != "morde-task-"+value.Key {
			return errors.New("persisted task process evidence is invalid")
		}
	} else if value.Process != (taskProcessProof{}) || value.Started {
		return errors.New("unarmed task process contains process evidence")
	}
	if value.Started && !value.Armed || !value.Destroyed && !value.DestroyedAt.IsZero() || value.Destroyed && value.UnprovenRunning {
		return errors.New("persisted task process stop state is inconsistent")
	}
	return nil
}

func validCgroupPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

func validTaskKey(key string) bool {
	return nonceHashRE.MatchString(key)
}

func cgroupPathWithin(parent, child string) bool {
	if !validCgroupPath(parent) || !validCgroupPath(child) {
		return false
	}
	return child == parent || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

func taskCgroupPath(runtimeCgroup, taskKey string) string {
	return strings.TrimSuffix(runtimeCgroup, "/") + "/morde-task-" + taskKey
}
