package conversation

import "time"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type DeliveryStatus string

const (
	DeliveryPending DeliveryStatus = "pending"
	DeliveryApplied DeliveryStatus = "applied"
	DeliveryUnknown DeliveryStatus = "unknown"
)

func (s DeliveryStatus) Valid() bool {
	switch s {
	case DeliveryPending, DeliveryApplied, DeliveryUnknown:
		return true
	default:
		return false
	}
}

type OutputStatus string

const (
	OutputStreaming   OutputStatus = "streaming"
	OutputInterrupted OutputStatus = "interrupted"
	OutputCompleted   OutputStatus = "completed"
)

func (s OutputStatus) Valid() bool {
	switch s {
	case OutputStreaming, OutputInterrupted, OutputCompleted:
		return true
	default:
		return false
	}
}

// RoutingStatus describes server-side processing after a message was accepted.
// It is independent of delivery to the Agent and of client-side send state.
type RoutingStatus string

type RoutingFailureReason string

func (r RoutingFailureReason) Valid() bool {
	switch r {
	case "model_unavailable", "routing_context_unavailable", "routing_action_unavailable", "message_too_long":
		return true
	default:
		return false
	}
}

const (
	RoutingPending           RoutingStatus = "pending"
	RoutingProcessing        RoutingStatus = "processing"
	RoutingRouted            RoutingStatus = "routed"
	RoutingWaitingUser       RoutingStatus = "waiting_user"
	RoutingWaitingAdjustment RoutingStatus = "waiting_adjustment"
	RoutingFailed            RoutingStatus = "failed"
)

func (s RoutingStatus) Valid() bool {
	switch s {
	case RoutingPending, RoutingProcessing, RoutingRouted, RoutingWaitingUser, RoutingWaitingAdjustment, RoutingFailed:
		return true
	default:
		return false
	}
}

type Conversation struct {
	ID              string
	OwnerID         string
	ProjectID       string
	HostID          string
	ModelID         string
	Title           string
	MessageSequence int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Message struct {
	ID                        string
	ConversationID            string
	OwnerID                   string
	ActorID                   string
	Role                      Role
	Content                   string
	TaskID                    string
	DuplicateOfMessageID      string
	RelatedTaskID             string
	AttachmentIDs             []string
	ClientMessageID           string
	RoutingStatus             RoutingStatus
	RoutingFailureReason      RoutingFailureReason
	Sources                   []MessageSource
	ModelID                   string
	Sequence                  int64
	SourceKind                string
	SourceExchangeID          string
	RequestedConversationID   string
	DeliveryStatus            DeliveryStatus
	OutputStatus              OutputStatus
	OutputTaskRevision        *int64
	OutputResultEventSequence *int64
	CreatedAt                 time.Time
}

// MessageSource identifies the exact Task result included in a combined reply.
type MessageSource struct {
	UserMessageID string `json:"user_message_id"`
	TaskID        string `json:"task_id"`
	TaskRevision  int64  `json:"task_revision"`
	EventSequence int64  `json:"event_sequence"`
	Content       string `json:"content"`
}
