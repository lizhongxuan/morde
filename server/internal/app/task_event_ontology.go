package app

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// Task event ontology (E08): the nine kinds a task event chain is replayed
// in. This is a read-side projection only. The write side keeps its eleven
// `task.*` event types (`task.status`, `task.step`, `task.operation`,
// `task.message`, `task.batch_item`, `task.artifact`, `task.waiting_user`,
// `task.response`, `task.result`, `task.plan`, `task.priority`); no write-side
// enum is added and no migration backs this file. The projection maps stored
// events to kinds with fixed rules and never drops an event silently: every
// event that maps to no kind is listed in UnmappedSequences.
//
// Mapping rules (fixed; changing one changes replay for every task):
//   - the earliest stored event is the creation record and always projects
//     to created, whatever its write-side type is. Task creation normally
//     writes `task.status`/`queued` first, but a task opened on an unclear
//     file-delivery intent starts with `task.waiting_user` instead, so the
//     head is derived from position, not from one type.
//   - `task.plan` -> split.
//   - `task.step`, `task.batch_item` -> execution, except a payload status of
//     `failed` -> failure.
//   - `task.operation` -> dispatched when its payload status is `dispatched`,
//     failure when `failed`, recovery when `unknown` with
//     `requires_reconciliation` (the lease-claim reconciliation record),
//     execution otherwise.
//   - `task.waiting_user` -> approval; `task.response` -> approval only when
//     it answers a pending approval (`pending_id` present), otherwise
//     unmapped (a clarification answer is not an approval).
//   - `task.status` after the head -> dispatched when `running`, failure
//     when `failed`, unmapped otherwise. `completed` is deliberately
//     unmapped: completion is not delivery.
//   - `task.message`, `task.artifact`, `task.priority` and any other type
//     -> unmapped.
//   - verification and delivery are projected from facts, not from events.
//     verification appears when a reviewed validation verdict exists for the
//     task revision, anchored at its result event sequence. delivery appears
//     only when that verdict is PASS or PASS+NOTES and the result receipt
//     has been published, anchored at the published source's event sequence.
//     A FAIL verdict never yields a delivery node, even if a receipt row
//     exists: the projection does not assume the publish gate held.

type TaskEventKind string

const (
	TaskEventKindCreated      TaskEventKind = "created"
	TaskEventKindSplit        TaskEventKind = "split"
	TaskEventKindDispatched   TaskEventKind = "dispatched"
	TaskEventKindApproval     TaskEventKind = "approval"
	TaskEventKindExecution    TaskEventKind = "execution"
	TaskEventKindVerification TaskEventKind = "verification"
	TaskEventKindDelivery     TaskEventKind = "delivery"
	TaskEventKindFailure      TaskEventKind = "failure"
	TaskEventKindRecovery     TaskEventKind = "recovery"
)

// TaskEventChainFacts carries the two non-event facts the projection needs.
// ValidationState is empty when no reviewed verdict exists for the revision;
// ResultEventSequence is the reviewed verdict's result event sequence.
// ReceiptPublished reports a published result receipt for the revision;
// ReceiptEventSequence is that published source's event sequence.
type TaskEventChainFacts struct {
	ValidationState      TaskValidationState
	ResultEventSequence  int64
	ReceiptPublished     bool
	ReceiptEventSequence int64
}

// TaskEventChainNode is one replayable step of a task. SourceType is the
// write-side event type for event-derived nodes and empty for fact-derived
// nodes (verification, delivery). Detail is a short stable label: the event
// payload status or reason, the verdict, or "receipt published".
type TaskEventChainNode struct {
	Kind           TaskEventKind
	SourceSequence int64
	SourceType     string
	Detail         string
	CreatedAt      time.Time
}

// TaskEventChain is the replayable projection of one task. Nodes are ordered
// by source sequence; UnmappedSequences lists every stored event sequence
// that mapped to no kind, in ascending order.
type TaskEventChain struct {
	TaskID            string
	Nodes             []TaskEventChainNode
	UnmappedSequences []int64
}

func taskEventKindRank(kind TaskEventKind) int {
	switch kind {
	case TaskEventKindCreated:
		return 0
	case TaskEventKindSplit:
		return 1
	case TaskEventKindDispatched:
		return 2
	case TaskEventKindApproval:
		return 3
	case TaskEventKindExecution:
		return 4
	case TaskEventKindVerification:
		return 5
	case TaskEventKindDelivery:
		return 6
	case TaskEventKindFailure:
		return 7
	case TaskEventKindRecovery:
		return 8
	default:
		return 9
	}
}

// ProjectTaskEventChain projects the full stored event list of one task into
// its nine-kind chain. Callers must pass the complete list in any order; the
// projection sorts by sequence. An empty list, events from more than one
// task, duplicate sequences, or inconsistent facts are rejected instead of
// producing a partial chain.
func ProjectTaskEventChain(events []task.Event, facts TaskEventChainFacts) (TaskEventChain, error) {
	if len(events) == 0 {
		return TaskEventChain{}, domain.InvalidArgument("task_events", "task event chain is empty")
	}
	taskID := events[0].TaskID
	if !domain.ValidTaskID(taskID) {
		return TaskEventChain{}, domain.InvalidArgument("task_id", "task event chain task is invalid")
	}
	ordered := append([]task.Event(nil), events...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	for index, event := range ordered {
		if event.TaskID != taskID {
			return TaskEventChain{}, domain.InvalidArgument("task_events", "task event chain spans more than one task")
		}
		if event.Sequence < 1 {
			return TaskEventChain{}, domain.InvalidArgument("task_events", "task event sequence is invalid")
		}
		if index > 0 && event.Sequence == ordered[index-1].Sequence {
			return TaskEventChain{}, domain.InvalidArgument("task_events", "task event sequence is duplicated")
		}
	}
	if facts.ValidationState != "" {
		if !ValidTaskValidationState(facts.ValidationState) {
			return TaskEventChain{}, domain.InvalidArgument("validation_state", "task event chain validation state is invalid")
		}
		if facts.ResultEventSequence < 1 {
			return TaskEventChain{}, domain.NewError(domain.CodeInvalidState, "task event chain verification has no result event")
		}
	}
	if facts.ReceiptPublished && facts.ReceiptEventSequence < 1 {
		return TaskEventChain{}, domain.NewError(domain.CodeInvalidState, "task event chain delivery has no published receipt event")
	}
	anchorAt := make(map[int64]time.Time, len(ordered))
	for _, event := range ordered {
		anchorAt[event.Sequence] = event.CreatedAt
	}
	chain := TaskEventChain{TaskID: taskID, Nodes: make([]TaskEventChainNode, 0, len(ordered)+2),
		UnmappedSequences: make([]int64, 0)}
	for index, event := range ordered {
		node, mapped := projectTaskEventOntologyNode(event, index == 0)
		if !mapped {
			chain.UnmappedSequences = append(chain.UnmappedSequences, event.Sequence)
			continue
		}
		chain.Nodes = append(chain.Nodes, node)
	}
	if facts.ValidationState != "" {
		chain.Nodes = append(chain.Nodes, TaskEventChainNode{Kind: TaskEventKindVerification,
			SourceSequence: facts.ResultEventSequence, Detail: string(facts.ValidationState),
			CreatedAt: anchorAt[facts.ResultEventSequence]})
		if (facts.ValidationState == TaskValidationPass || facts.ValidationState == TaskValidationPassNotes) && facts.ReceiptPublished {
			chain.Nodes = append(chain.Nodes, TaskEventChainNode{Kind: TaskEventKindDelivery,
				SourceSequence: facts.ReceiptEventSequence, Detail: "receipt published",
				CreatedAt: anchorAt[facts.ReceiptEventSequence]})
		}
	}
	sort.SliceStable(chain.Nodes, func(i, j int) bool {
		if chain.Nodes[i].SourceSequence != chain.Nodes[j].SourceSequence {
			return chain.Nodes[i].SourceSequence < chain.Nodes[j].SourceSequence
		}
		return taskEventKindRank(chain.Nodes[i].Kind) < taskEventKindRank(chain.Nodes[j].Kind)
	})
	if len(chain.Nodes) == 0 || chain.Nodes[0].Kind != TaskEventKindCreated {
		return TaskEventChain{}, domain.NewError(domain.CodeInvalidState, "task event chain does not start with created")
	}
	return chain, nil
}

func projectTaskEventOntologyNode(event task.Event, first bool) (TaskEventChainNode, bool) {
	fields := taskEventPayloadFields(event.Payload)
	detail := taskEventPayloadDetail(fields)
	node := TaskEventChainNode{SourceSequence: event.Sequence, SourceType: event.Type,
		Detail: detail, CreatedAt: event.CreatedAt}
	if first {
		node.Kind = TaskEventKindCreated
		return node, true
	}
	switch event.Type {
	case "task.plan":
		node.Kind = TaskEventKindSplit
		return node, true
	case "task.step":
		if taskEventPayloadStatus(fields) == "failed" {
			node.Kind = TaskEventKindFailure
		} else {
			node.Kind = TaskEventKindExecution
		}
		return node, true
	case "task.operation":
		switch taskEventPayloadStatus(fields) {
		case "dispatched":
			node.Kind = TaskEventKindDispatched
		case "failed":
			node.Kind = TaskEventKindFailure
		case "unknown":
			if taskEventPayloadBool(fields, "requires_reconciliation") {
				node.Kind = TaskEventKindRecovery
			} else {
				node.Kind = TaskEventKindExecution
			}
		default:
			node.Kind = TaskEventKindExecution
		}
		return node, true
	case "task.batch_item":
		if taskEventPayloadStatus(fields) == "failed" {
			node.Kind = TaskEventKindFailure
		} else {
			node.Kind = TaskEventKindExecution
		}
		return node, true
	case "task.waiting_user":
		node.Kind = TaskEventKindApproval
		return node, true
	case "task.response":
		if taskEventPayloadString(fields, "pending_id") != "" {
			node.Kind = TaskEventKindApproval
			return node, true
		}
		return node, false
	case "task.status":
		switch taskEventPayloadStatus(fields) {
		case "running":
			node.Kind = TaskEventKindDispatched
			return node, true
		case "failed":
			node.Kind = TaskEventKindFailure
			return node, true
		default:
			return node, false
		}
	default:
		return node, false
	}
}

func taskEventPayloadFields(payload []byte) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &fields) != nil || fields == nil {
		return map[string]json.RawMessage{}
	}
	return fields
}

func taskEventPayloadString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func taskEventPayloadStatus(fields map[string]json.RawMessage) string {
	return taskEventPayloadString(fields, "status")
}

func taskEventPayloadBool(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

// taskEventPayloadDetail keeps one short stable label per node: the payload
// status when present, otherwise the reason. Anything longer than 128 runes
// is cut; the chain records what happened, not the full payload.
func taskEventPayloadDetail(fields map[string]json.RawMessage) string {
	detail := taskEventPayloadStatus(fields)
	if detail == "" {
		detail = taskEventPayloadString(fields, "reason")
	}
	runes := []rune(detail)
	if len(runes) > 128 {
		detail = string(runes[:128])
	}
	return strings.TrimSpace(detail)
}
