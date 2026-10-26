package host

import (
	"time"

	"morde/server/internal/domain/workspace"
)

type State string

type ControlState string

const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"
)

const (
	ControlAI         ControlState = "ai"
	ControlTakingOver ControlState = "taking_over"
	ControlUser       ControlState = "user"
	ControlReturning  ControlState = "returning"
)

type Ref struct {
	ID                string
	OwnerID           string
	State             State
	RuntimeGeneration int64
	ControlVersion    int64
	ControlState      ControlState
}

type Host struct {
	ID                   string
	OwnerID              string
	Name                 string
	TemplateID           string
	RuntimeProfile       string
	Workspaces           []workspace.Binding
	SandboxID            string
	CreateAttemptedAt    *time.Time
	State                State
	StateReason          string
	RuntimeGeneration    int64
	ControlVersion       int64
	ControlState         ControlState
	CPUMilli             int
	MemMiB               int
	CPULimitMilli        int
	MemLimitMiB          int
	AIOKeyEnc            []byte
	RuntimeCredentialEnc []byte
	RuntimeInstanceID    string
	ExpiresAt            *time.Time
	LastActiveAt         *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}
