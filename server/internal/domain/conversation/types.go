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
	ID                      string
	ConversationID          string
	OwnerID                 string
	ActorID                 string
	Role                    Role
	Content                 string
	TaskID                  string
	AttachmentIDs           []string
	ModelID                 string
	Sequence                int64
	SourceKind              string
	SourceExchangeID        string
	RequestedConversationID string
	DeliveryStatus          DeliveryStatus
	CreatedAt               time.Time
}
