package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// TaskEventChainFacts carries the validation and receipt facts that raw
// task events alone cannot prove: verification comes from the independent
// review, delivery from the published result receipt. R04 forbids
// equating task completion with either.
type TaskEventChainFacts struct {
	// ValidationState is empty when no reviewed validation exists for the
	// task's current revision.
	ValidationState TaskValidationState
	Delivered       bool
}

// TaskEventChainStore is the minimal port the chain replay needs. It is
// deliberately separate from TaskStore: the chain only reads events and
// two facts.
type TaskEventChainStore interface {
	ListTaskEvents(context.Context, string, string, int64, int32) ([]task.Event, error)
	TaskEventChainFacts(context.Context, string, string) (TaskEventChainFacts, error)
}

// TaskEventChainEntry is one semantic step of a task's event chain.
// Verification and delivery entries are synthetic: they are derived from
// validation and receipt facts, not from a raw task event, so they carry
// no raw event type.
type TaskEventChainEntry struct {
	Sequence        int64
	Kind            task.EventKind
	RawType         string
	ValidationState TaskValidationState
	Delivered       bool
}

// TaskEventChain is the ordered semantic projection of one task's raw
// events, from head to tail. UnmappedSequences lists raw events that
// carry no lifecycle semantic; they are kept visible, never dropped
// silently. Valid is false when the chain breaks a structural rule; the
// entries are still returned so a broken chain can be inspected.
type TaskEventChain struct {
	TaskID            string
	Entries           []TaskEventChainEntry
	UnmappedSequences []int64
	Valid             bool
	InvalidReasons    []string
}

const taskEventChainPageSize int32 = 500

// ReplayTaskEventChain projects ordered raw task events into their
// semantic chain. Events must arrive in sequence order; verification is
// appended from facts.ValidationState after the task.result event, and
// delivery only after a passing verification. A failure followed by
// resumed execution or dispatch inserts a recovery entry on the resuming
// event's sequence.
func ReplayTaskEventChain(taskID string, events []task.Event, facts TaskEventChainFacts) TaskEventChain {
	chain := TaskEventChain{
		TaskID:            taskID,
		Entries:           make([]TaskEventChainEntry, 0, len(events)),
		UnmappedSequences: make([]int64, 0),
		InvalidReasons:    make([]string, 0),
	}
	if len(events) == 0 {
		chain.InvalidReasons = append(chain.InvalidReasons, "task has no events")
		return chain
	}
	var previous int64
	failed := false
	var resultSequence int64
	for index, event := range events {
		if index == 0 {
			previous = event.Sequence
		} else if event.Sequence <= previous {
			chain.InvalidReasons = append(chain.InvalidReasons, fmt.Sprintf("event sequence %d does not increase", event.Sequence))
		} else {
			previous = event.Sequence
		}
		if event.Type == "task.result" {
			resultSequence = event.Sequence
		}
		kind, ok := task.ProjectTaskEventKind(event.Type, event.Payload)
		if !ok {
			chain.UnmappedSequences = append(chain.UnmappedSequences, event.Sequence)
			continue
		}
		if kind == task.EventKindFailure {
			failed = true
		} else if failed && (kind == task.EventKindExecution || kind == task.EventKindDispatched) {
			chain.Entries = append(chain.Entries, TaskEventChainEntry{Sequence: event.Sequence, Kind: task.EventKindRecovery, RawType: event.Type})
			failed = false
		}
		chain.Entries = append(chain.Entries, TaskEventChainEntry{Sequence: event.Sequence, Kind: kind, RawType: event.Type})
	}
	if facts.ValidationState != "" {
		if !ValidTaskValidationState(facts.ValidationState) {
			chain.InvalidReasons = append(chain.InvalidReasons, fmt.Sprintf("validation state %q is unknown", facts.ValidationState))
		} else {
			sequence := resultSequence
			if sequence == 0 {
				sequence = events[len(events)-1].Sequence
				chain.InvalidReasons = append(chain.InvalidReasons, "validation without result event")
			}
			chain.Entries = append(chain.Entries, TaskEventChainEntry{Sequence: sequence, Kind: task.EventKindVerification, ValidationState: facts.ValidationState})
		}
	}
	if facts.Delivered {
		verified := false
		var verificationSequence int64
		for _, entry := range chain.Entries {
			if entry.Kind == task.EventKindVerification &&
				(entry.ValidationState == TaskValidationPass || entry.ValidationState == TaskValidationPassNotes) {
				verified = true
				verificationSequence = entry.Sequence
			}
		}
		if !verified {
			chain.InvalidReasons = append(chain.InvalidReasons, "delivered without passing verification")
		} else {
			chain.Entries = append(chain.Entries, TaskEventChainEntry{Sequence: verificationSequence, Kind: task.EventKindDelivery, Delivered: true})
		}
	}
	if len(chain.Entries) == 0 {
		chain.InvalidReasons = append(chain.InvalidReasons, "task has no mappable events")
	} else if chain.Entries[0].Kind != task.EventKindCreated {
		chain.InvalidReasons = append(chain.InvalidReasons, "chain does not start with created")
	}
	chain.Valid = len(chain.InvalidReasons) == 0
	return chain
}

// TaskEventChainService replays one task's semantic event chain.
type TaskEventChainService struct {
	store TaskEventChainStore
}

func NewTaskEventChainService(store TaskEventChainStore) (*TaskEventChainService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("task_event_chain", "task event chain store is required")
	}
	return &TaskEventChainService{store: store}, nil
}

// Replay returns the semantic event chain for one task, from head to tail.
func (s *TaskEventChainService) Replay(ctx context.Context, principal identity.Principal, taskID string) (TaskEventChain, error) {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTaskID(taskID) {
		return TaskEventChain{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	var events []task.Event
	var after int64
	for {
		page, err := s.store.ListTaskEvents(ctx, principal.UserID, taskID, after, taskEventChainPageSize)
		if err != nil {
			return TaskEventChain{}, err
		}
		if len(page) == 0 {
			break
		}
		events = append(events, page...)
		after = page[len(page)-1].Sequence
		if len(page) < int(taskEventChainPageSize) {
			break
		}
	}
	facts, err := s.store.TaskEventChainFacts(ctx, principal.UserID, taskID)
	if err != nil {
		return TaskEventChain{}, err
	}
	return ReplayTaskEventChain(taskID, events, facts), nil
}
