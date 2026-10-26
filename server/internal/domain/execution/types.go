package execution

import "time"

type State string

const (
	StateEnabled  State = "enabled"
	StateStopping State = "stopping"
	StateDisabled State = "disabled"
)

type Control struct {
	OwnerID          string    `json:"owner_id"`
	State            State     `json:"state"`
	Epoch            int64     `json:"epoch"`
	PendingTaskCount int64     `json:"pending_task_count"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s State) Valid() bool {
	return s == StateEnabled || s == StateStopping || s == StateDisabled
}
