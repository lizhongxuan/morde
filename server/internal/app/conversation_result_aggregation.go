package app

import "context"

// ConversationResultPublisher publishes ready result batches atomically.
type ConversationResultPublisher interface {
	PublishReadyConversationResults(context.Context, int) (int, error)
}
