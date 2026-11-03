package notification

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type Kind string

const (
	KindCompleted            Kind = "completed"
	KindFailed               Kind = "failed"
	KindApprovalRequired     Kind = "approval_required"
	KindClarification        Kind = "clarification_required"
	KindAttentionRequired    Kind = "attention_required"
	KindGoalStepBlocked      Kind = "goal_step_blocked"
	KindGoalCompleted        Kind = "goal_completed"
	KindProjectItemBlocked   Kind = "project_work_item_blocked"
	KindProjectItemDelivered Kind = "project_work_item_delivered"
)

const (
	SourceGoal            = "goal"
	SourceProjectWorkItem = "project_work_item"
)

type TaskEvent struct {
	OwnerID             string
	TaskID              string
	Sequence            int64
	Type                string
	Payload             []byte
	TaskStatus          task.Status
	NotificationEnabled bool
	CreatedAt           time.Time
}

type Notification struct {
	ID             string
	OwnerID        string
	TaskID         string
	EventSequence  int64
	SourceEventID  int64
	SourceKind     string
	SourceID       string
	SourceVersion  int64
	ProjectID      string
	Kind           Kind
	Summary        string
	RequestID      string
	RequestVersion int64
	Actionable     bool
	ReadAt         *time.Time
	HandledAt      *time.Time
	CreatedAt      time.Time
}

// SourceEvent is a recipient-scoped durable event that does not originate from a Task.
type SourceEvent struct {
	ID            int64
	RecipientID   string
	SourceKind    string
	SourceID      string
	SourceVersion int64
	ProjectID     string
	Kind          Kind
	CreatedAt     time.Time
}

type EventEffect struct {
	Notification *Notification
	ClosePending bool
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func NewID() (string, error) {
	var id [8]byte
	id[0] = 'n'
	max := big.NewInt(int64(len(idAlphabet)))
	for i := 1; i < len(id); i++ {
		value, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate notification ID: %w", err)
		}
		id[i] = idAlphabet[value.Int64()]
	}
	return string(id[:]), nil
}

func ValidID(value string) bool {
	if len(value) != 8 || value[0] != 'n' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !strings.ContainsRune(idAlphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

// DeriveEventEffect stores only fixed summaries and opaque task/request references.
func DeriveEventEffect(event TaskEvent, id string) (EventEffect, error) {
	if !domain.ValidUserID(event.OwnerID) || !domain.ValidTaskID(event.TaskID) || event.Sequence < 1 ||
		!json.Valid(event.Payload) || event.CreatedAt.IsZero() {
		return EventEffect{}, domain.InvalidArgument("event", "task event is invalid")
	}
	switch event.Type {
	case "task.response":
		return EventEffect{ClosePending: true}, nil
	case "task.status":
		if event.TaskStatus == task.StatusCancelled || event.TaskStatus == task.StatusFailed || event.TaskStatus == task.StatusCompleted {
			return EventEffect{ClosePending: true}, nil
		}
		return EventEffect{}, nil
	case "task.result":
		kind := Kind("")
		switch event.TaskStatus {
		case task.StatusCompleted:
			kind = KindCompleted
		case task.StatusFailed:
			kind = KindFailed
		default:
			return EventEffect{}, nil
		}
		if !event.NotificationEnabled {
			return EventEffect{ClosePending: true}, nil
		}
		effect := newNotification(event, id, kind, kind.summary(), "", 0)
		effect.ClosePending = true
		return effect, nil
	case "task.waiting_user":
		if !event.NotificationEnabled {
			return EventEffect{}, nil
		}
		var payload struct {
			Detail struct {
				PendingRequestID      string `json:"pending_request_id"`
				PendingRequestVersion int64  `json:"pending_request_version"`
				PendingRequestKind    string `json:"pending_request_kind"`
				ClarificationID       string `json:"clarification_id"`
				ClarificationVersion  int64  `json:"clarification_version"`
			} `json:"detail"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return EventEffect{}, domain.InvalidArgument("event.payload", "task event payload is invalid")
		}
		kind, requestID, requestVersion := KindAttentionRequired, "", int64(0)
		switch {
		case domain.ValidPendingRequestID(payload.Detail.PendingRequestID) && payload.Detail.PendingRequestVersion > 0 && payload.Detail.PendingRequestKind == "approval":
			kind = KindApprovalRequired
			requestID, requestVersion = payload.Detail.PendingRequestID, payload.Detail.PendingRequestVersion
		case domain.ValidPendingRequestID(payload.Detail.ClarificationID) && payload.Detail.ClarificationVersion > 0:
			kind = KindClarification
			requestID, requestVersion = payload.Detail.ClarificationID, payload.Detail.ClarificationVersion
		}
		return newNotification(event, id, kind, kind.summary(), requestID, requestVersion), nil
	default:
		return EventEffect{}, domain.InvalidArgument("event.type", "task event type is not supported for notifications")
	}
}

func newNotification(event TaskEvent, id string, kind Kind, summary, requestID string, requestVersion int64) EventEffect {
	return EventEffect{Notification: &Notification{
		ID: id, OwnerID: event.OwnerID, TaskID: event.TaskID, EventSequence: event.Sequence,
		Kind: kind, Summary: summary, RequestID: requestID, RequestVersion: requestVersion,
		Actionable: event.TaskStatus == task.StatusWaitingUser && requestID != "", CreatedAt: event.CreatedAt,
	}}
}

func Validate(value Notification) error {
	if !ValidID(value.ID) || !domain.ValidUserID(value.OwnerID) || !value.Kind.valid() || !utf8.ValidString(value.Summary) ||
		strings.TrimSpace(value.Summary) == "" || len(value.Summary) > 256 || value.CreatedAt.IsZero() {
		return errors.New("notification fields are invalid")
	}
	if value.TaskID != "" {
		if !domain.ValidTaskID(value.TaskID) || value.EventSequence < 1 || value.SourceEventID != 0 || value.SourceKind != "" ||
			value.SourceID != "" || value.SourceVersion != 0 || value.ProjectID != "" || !value.Kind.taskKind() {
			return errors.New("task notification source is invalid")
		}
	} else if value.EventSequence != 0 || value.SourceEventID < 1 || value.SourceVersion < 1 || !value.Kind.sourceKind() ||
		value.SourceID == "" || (value.SourceKind != SourceGoal && value.SourceKind != SourceProjectWorkItem) ||
		(value.SourceKind == SourceProjectWorkItem && (!project.ValidID(value.ProjectID) || !project.ValidID(value.SourceID))) ||
		(value.SourceKind == SourceGoal && (value.ProjectID != "" || personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, value.SourceID) != nil)) {
		return errors.New("source notification identity is invalid")
	}
	if value.Summary != value.Kind.summary() {
		return errors.New("notification summary does not match its kind")
	}
	switch value.Kind {
	case KindApprovalRequired, KindClarification:
		if value.TaskID == "" || !domain.ValidPendingRequestID(value.RequestID) || value.RequestVersion < 1 {
			return errors.New("notification request reference is invalid")
		}
	default:
		if value.RequestID != "" || value.RequestVersion != 0 || value.Actionable {
			return errors.New("notification request fields are unexpected")
		}
	}
	if value.Actionable && value.HandledAt != nil {
		return errors.New("handled notification cannot be actionable")
	}
	return nil
}

func ValidateSourceEvent(value SourceEvent) error {
	if value.ID < 1 || !domain.ValidUserID(value.RecipientID) || value.SourceVersion < 1 ||
		!value.Kind.sourceKind() || value.CreatedAt.IsZero() {
		return errors.New("notification source event is invalid")
	}
	switch value.SourceKind {
	case SourceGoal:
		if value.ProjectID != "" || !value.Kind.goalKind() || personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, value.SourceID) != nil {
			return errors.New("goal notification source event is invalid")
		}
	case SourceProjectWorkItem:
		if !project.ValidID(value.ProjectID) || !project.ValidID(value.SourceID) || !value.Kind.projectKind() {
			return errors.New("project notification source event is invalid")
		}
	default:
		return errors.New("notification source kind is invalid")
	}
	return nil
}

func NewSourceEventNotification(event SourceEvent, id string) (Notification, error) {
	if err := ValidateSourceEvent(event); err != nil {
		return Notification{}, domain.InvalidArgument("event", "notification source event is invalid")
	}
	value := Notification{
		ID: id, OwnerID: event.RecipientID, SourceEventID: event.ID, SourceKind: event.SourceKind,
		SourceID: event.SourceID, SourceVersion: event.SourceVersion, ProjectID: event.ProjectID,
		Kind: event.Kind, Summary: event.Kind.summary(), CreatedAt: event.CreatedAt,
	}
	if err := Validate(value); err != nil {
		return Notification{}, domain.InvalidArgument("notification", "source notification is invalid")
	}
	return value, nil
}

func (value Notification) IsHandled() bool { return value.HandledAt != nil }

func (kind Kind) valid() bool {
	switch kind {
	case KindCompleted, KindFailed, KindApprovalRequired, KindClarification, KindAttentionRequired,
		KindGoalStepBlocked, KindGoalCompleted, KindProjectItemBlocked, KindProjectItemDelivered:
		return true
	default:
		return false
	}
}

func (kind Kind) taskKind() bool {
	return kind == KindCompleted || kind == KindFailed || kind == KindApprovalRequired || kind == KindClarification || kind == KindAttentionRequired
}

func (kind Kind) sourceKind() bool { return kind.goalKind() || kind.projectKind() }

func (kind Kind) goalKind() bool { return kind == KindGoalStepBlocked || kind == KindGoalCompleted }

func (kind Kind) projectKind() bool {
	return kind == KindProjectItemBlocked || kind == KindProjectItemDelivered
}

func (kind Kind) summary() string {
	switch kind {
	case KindCompleted:
		return "任务已完成"
	case KindFailed:
		return "任务执行失败"
	case KindApprovalRequired:
		return "任务等待审批"
	case KindClarification:
		return "任务等待补充信息"
	case KindAttentionRequired:
		return "任务需要你处理"
	case KindGoalStepBlocked:
		return "目标步骤遇到阻塞"
	case KindGoalCompleted:
		return "目标已完成"
	case KindProjectItemBlocked:
		return "项目工作项遇到阻塞"
	case KindProjectItemDelivered:
		return "项目工作项已交付"
	default:
		return ""
	}
}
