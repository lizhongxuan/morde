package conversation

import "time"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Conversation struct {
	ID        string
	OwnerID   string
	HostID    string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID             string
	ConversationID string
	OwnerID        string
	Role           Role
	Content        string
	TaskID         string
	CreatedAt      time.Time
}
