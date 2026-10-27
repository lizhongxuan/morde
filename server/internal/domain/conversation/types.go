package conversation

import "time"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Conversation struct {
	ID              string
	OwnerID         string
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
	Role                    Role
	Content                 string
	TaskID                  string
	ModelID                 string
	Sequence                int64
	SourceKind              string
	SourceExchangeID        string
	RequestedConversationID string
	CreatedAt               time.Time
}
