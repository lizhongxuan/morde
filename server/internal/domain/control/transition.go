package control

import (
	"encoding/json"
	"math"
	"regexp"
	"time"

	"morde/server/internal/domain"
)

// Action identifies the user-requested direction of a control handoff.
type Action string

const (
	ActionTakeOver Action = "take_over"
	ActionRelease  Action = "release"
)

// Phase is the durable progress of a handoff. A failed step records a reason
// without advancing or rolling back the phase, so the Host remains blocked.
type Phase string

const (
	PhaseBlocking  Phase = "blocking"
	PhaseDraining  Phase = "draining"
	PhaseDrained   Phase = "drained"
	PhaseCompleted Phase = "completed"
)

const MaxFailureReasonBytes = 512

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// BeginRequest is the immutable, idempotent input used to reserve a Host
// control handoff.
type BeginRequest struct {
	HostID                 string
	OwnerID                string
	RequestID              string
	Action                 Action
	ExpectedControlVersion int64
}

func (r BeginRequest) Validate() error {
	if !domain.ValidHostID(r.HostID) {
		return domain.InvalidArgument("host_id", "host id is invalid")
	}
	if !domain.ValidUserID(r.OwnerID) {
		return domain.InvalidArgument("owner_id", "owner id is invalid")
	}
	if !ValidRequestID(r.RequestID) {
		return domain.InvalidArgument("request_id", "request id is invalid")
	}
	if r.Action != ActionTakeOver && r.Action != ActionRelease {
		return domain.InvalidArgument("action", "control action is invalid")
	}
	if r.ExpectedControlVersion < 0 || r.ExpectedControlVersion > math.MaxInt64-2 {
		return domain.InvalidArgument("expected_control_version", "control version is out of range")
	}
	return nil
}

func ValidRequestID(requestID string) bool {
	return requestIDPattern.MatchString(requestID)
}

// Transition is the persisted control handoff state. The drained receipt is
// opaque to this package so the domain does not depend on a runtime protocol.
type Transition struct {
	HostID                 string          `json:"host_id"`
	OwnerID                string          `json:"owner_id"`
	RequestID              string          `json:"request_id"`
	Action                 Action          `json:"action"`
	Phase                  Phase           `json:"phase"`
	ExpectedControlVersion int64           `json:"expected_control_version"`
	BlockedControlVersion  int64           `json:"blocked_control_version"`
	TargetControlVersion   int64           `json:"target_control_version"`
	TaskIDs                []string        `json:"task_ids"`
	TaskIDsSaved           bool            `json:"task_ids_saved"`
	DrainedReceipt         json.RawMessage `json:"drained_receipt,omitempty"`
	FailureReason          string          `json:"failure_reason,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

func ValidPhase(phase Phase) bool {
	switch phase {
	case PhaseBlocking, PhaseDraining, PhaseDrained, PhaseCompleted:
		return true
	default:
		return false
	}
}
