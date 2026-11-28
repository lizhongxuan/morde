package task

import "encoding/json"

// EventKind is one of the nine semantic kinds a task event chain is
// projected into (R04: created, split, dispatched, approval, execution,
// verification, delivery, failure, recovery). Raw task_events keep their
// eleven write-side types (task.status, task.step, task.operation,
// task.message, task.batch_item, task.artifact, task.waiting_user,
// task.response, task.result, task.plan, task.priority); kinds are a
// read-side projection over those facts, never a second write enum.
type EventKind string

const (
	EventKindCreated      EventKind = "created"
	EventKindSplit        EventKind = "split"
	EventKindDispatched   EventKind = "dispatched"
	EventKindApproval     EventKind = "approval"
	EventKindExecution    EventKind = "execution"
	EventKindVerification EventKind = "verification"
	EventKindDelivery     EventKind = "delivery"
	EventKindFailure      EventKind = "failure"
	EventKindRecovery     EventKind = "recovery"
)

// Valid reports whether the kind is one of the nine ontology kinds.
func (k EventKind) Valid() bool {
	switch k {
	case EventKindCreated, EventKindSplit, EventKindDispatched, EventKindApproval, EventKindExecution,
		EventKindVerification, EventKindDelivery, EventKindFailure, EventKindRecovery:
		return true
	default:
		return false
	}
}

// eventStatusPayload carries the status field of task.status, task.step,
// task.operation and task.batch_item payloads.
type eventStatusPayload struct {
	Status string `json:"status"`
}

// ProjectTaskEventKind maps one raw task event to its semantic kind. The
// second result is false when the raw event carries no lifecycle semantic
// or its payload does not determine one; callers must keep such events
// visible as unmapped instead of guessing a kind.
//
// Rules (R04):
//   - task.status queued -> created; running -> dispatched;
//     waiting_user -> approval; failed/cancelled -> failure. completed is
//     deliberately not delivery: delivery is projected from receipt
//     facts, never equated with task completion. pausing, paused and
//     cancelling are transient control states, not lifecycle semantics.
//   - task.plan -> split.
//   - task.step, task.operation, task.batch_item -> execution, or failure
//     when the payload status is failed. An unknown or missing status
//     maps to no kind.
//   - task.waiting_user -> approval.
//   - task.result, task.message, task.response, task.artifact and
//     task.priority carry no lifecycle kind here: verification and
//     delivery come from validation and receipt facts at replay time.
//   - unknown types and malformed payloads map to no kind.
func ProjectTaskEventKind(eventType string, payload []byte) (EventKind, bool) {
	switch eventType {
	case "task.status":
		var decoded eventStatusPayload
		if json.Unmarshal(payload, &decoded) != nil {
			return "", false
		}
		switch Status(decoded.Status) {
		case StatusQueued:
			return EventKindCreated, true
		case StatusRunning:
			return EventKindDispatched, true
		case StatusWaitingUser:
			return EventKindApproval, true
		case StatusFailed, StatusCancelled:
			return EventKindFailure, true
		default:
			return "", false
		}
	case "task.plan":
		return EventKindSplit, true
	case "task.step":
		var decoded eventStatusPayload
		if json.Unmarshal(payload, &decoded) != nil {
			return "", false
		}
		if StepStatus(decoded.Status) == StepFailed {
			return EventKindFailure, true
		}
		switch StepStatus(decoded.Status) {
		case StepQueued, StepRunning, StepWaitingUser, StepSucceeded, StepCancelled, StepUnknown:
			return EventKindExecution, true
		default:
			return "", false
		}
	case "task.operation":
		var decoded eventStatusPayload
		if json.Unmarshal(payload, &decoded) != nil {
			return "", false
		}
		if OperationStatus(decoded.Status) == OperationFailed {
			return EventKindFailure, true
		}
		switch OperationStatus(decoded.Status) {
		case OperationPrepared, OperationDispatched, OperationSucceeded, OperationCancelled, OperationUnknown:
			return EventKindExecution, true
		default:
			return "", false
		}
	case "task.batch_item":
		var decoded eventStatusPayload
		if json.Unmarshal(payload, &decoded) != nil {
			return "", false
		}
		if BatchItemStatus(decoded.Status) == BatchItemFailed {
			return EventKindFailure, true
		}
		switch BatchItemStatus(decoded.Status) {
		case BatchItemQueued, BatchItemRunning, BatchItemSucceeded, BatchItemUnknown:
			return EventKindExecution, true
		default:
			return "", false
		}
	case "task.waiting_user":
		return EventKindApproval, true
	default:
		return "", false
	}
}
