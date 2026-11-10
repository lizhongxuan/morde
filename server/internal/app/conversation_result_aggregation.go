package app

import "context"

// ConversationResultReviewQueue durably wakes the conversation coordinator for ready Task results.
type ConversationResultReviewQueue interface {
	QueueReadyConversationResultReviews(context.Context, int) (int, error)
}
