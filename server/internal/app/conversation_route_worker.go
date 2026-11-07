package app

import (
	"context"
	"time"
)

const ConversationAttachmentOnlyObjective = "请处理本条消息中的附件。"

// MaxConversationAgentInputBytes is a protocol ceiling, not a guarantee that a
// message of this size fits the model: history, tools and instructions also
// consume the gateway's input budget.
const MaxConversationAgentInputBytes = maxAgentInputBytes

// ConversationRouteSnapshot contains only information the sender may use to
// determine the fate of one accepted message.
type ConversationRouteSnapshot struct {
	Context              ConversationRouteContext
	PreviousMissing      bool
	PreviousCycle        bool
	PreviousRoutingState string
	PreviousTaskID       string
	PreviousMessageID    string
	PreviousSequence     int64
}

// ConversationRouteClassifier uses the same metered model gateway as Tasks.
// It must bind the call to the durable message and its current routing lease.
type ConversationRouteClassifier interface {
	ResolveConversationRouteModel(context.Context, ConversationRouteClaim) (string, error)
	ClassifyConversationRoute(context.Context, ConversationRouteClaim, ConversationRouteContext) (ConversationRouteDecision, error)
}

// ConversationRouteActions performs each action and completes the route in the
// same transaction, after rechecking authorization and the routing snapshot.
type ConversationRouteActions interface {
	LoadConversationRouteSnapshot(context.Context, ConversationRouteClaim) (ConversationRouteSnapshot, error)
	ApplyConversationRoute(context.Context, ConversationRouteClaim, ConversationRouteSnapshot, ConversationRouteDecision) error
	FailConversationRoute(context.Context, ConversationRouteClaim, string) error
	DeferConversationRoute(context.Context, ConversationRouteClaim, time.Duration) error
	ApplyNextConversationTaskRevision(context.Context) (bool, error)
}

// ConversationRoutePredecessorRecovery atomically releases only an
// undispatched reservation before requeuing a route whose predecessor arrived.
type ConversationRoutePredecessorRecovery interface {
	RequeueConversationRouteAfterPredecessor(context.Context, ConversationRouteClaim, ConversationRouteSnapshot) (bool, error)
}
