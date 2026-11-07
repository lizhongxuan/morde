package app

import (
	"context"
	"time"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

// ConversationRouteClaim identifies one leased, already accepted message.
type ConversationRouteClaim struct {
	Message           conversation.Message
	ServiceInstanceID string
	Input             ConversationRoutingInput
	Authorization     task.Authorization
	LeaseOwner        string
	LeaseVersion      int64
	LeaseExpiresAt    time.Time
}

type ConversationRouteQueue interface {
	ClaimConversationRoute(context.Context, string, time.Duration) (ConversationRouteClaim, bool, error)
	ReleaseConversationRoute(context.Context, ConversationRouteClaim) error
}
