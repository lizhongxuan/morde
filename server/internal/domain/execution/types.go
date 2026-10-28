package execution

import "time"

type State string

const (
	StateEnabled  State = "enabled"
	StateStopping State = "stopping"
	StateDisabled State = "disabled"
)

type RuntimeState string

const (
	RuntimeStateNotStarted RuntimeState = "not_started"
	RuntimeStateStarting   RuntimeState = "starting"
	RuntimeStateRunning    RuntimeState = "running"
	RuntimeStateStopping   RuntimeState = "stopping"
	RuntimeStateStopped    RuntimeState = "stopped"
	RuntimeStateFailed     RuntimeState = "failed"
)

type Runtime struct {
	State              RuntimeState `json:"state"`
	RuntimeID          string       `json:"runtime_id,omitempty"`
	RuntimeGeneration  int64        `json:"runtime_generation"`
	MaxConcurrentTasks int          `json:"max_concurrent_tasks"`
	ActiveTaskCount    int64        `json:"active_task_count"`
}

func (r Runtime) Valid() bool {
	if r.State != RuntimeStateNotStarted && r.State != RuntimeStateStarting && r.State != RuntimeStateRunning &&
		r.State != RuntimeStateStopping && r.State != RuntimeStateStopped && r.State != RuntimeStateFailed {
		return false
	}
	if r.ActiveTaskCount < 0 || r.MaxConcurrentTasks < 0 || r.MaxConcurrentTasks > 16 || r.RuntimeGeneration < 0 {
		return false
	}
	if r.State == RuntimeStateNotStarted {
		return r.RuntimeID == "" && r.RuntimeGeneration == 0 && r.MaxConcurrentTasks == 0 && r.ActiveTaskCount == 0
	}
	return r.RuntimeID != "" && r.RuntimeGeneration > 0 && r.MaxConcurrentTasks > 0
}

type Control struct {
	OwnerID          string    `json:"owner_id"`
	State            State     `json:"state"`
	Epoch            int64     `json:"epoch"`
	PendingTaskCount int64     `json:"pending_task_count"`
	Runtime          Runtime   `json:"runtime"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s State) Valid() bool {
	return s == StateEnabled || s == StateStopping || s == StateDisabled
}
