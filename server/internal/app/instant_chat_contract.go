package app

import (
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type InstantChatConversation struct {
	ID        string
	OwnerID   string
	ModelID   string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type InstantChatMessage struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	CreatedAt      time.Time
}

type InstantChatSend struct {
	ConversationID string
	ModelID        string
	Content        string
	RequestKey     string
}

type InstantChatReply struct {
	Conversation InstantChatConversation
	Message      InstantChatMessage
	Reply        InstantChatMessage
}

func validateInstantChatPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" ||
		len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func validInstantChatKey(key string) bool {
	for _, char := range key {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '~' && char != '-' {
			return false
		}
	}
	return true
}
