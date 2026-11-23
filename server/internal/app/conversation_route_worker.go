package app

import (
	"context"
	"time"
)

const ConversationAttachmentOnlyObjective = "请处理本条消息中的附件。"
const MaxConversationMessageChunkRunes = 32768
const MaxConversationMessageChunkBytes = 32 << 10
const ConversationRouteAggregationWindow = 350 * time.Millisecond
const ConversationRouteAggregationMaxWait = 30 * time.Second

// MaxConversationAgentInputBytes is a protocol ceiling, not a guarantee that a
// message of this size fits the model: history, tools and instructions also
// consume the gateway's input budget.
const MaxConversationAgentInputBytes = maxAgentInputBytes

// ConversationRouteSnapshot contains only information the sender may use to
// determine the fate of one accepted message.
type ConversationRouteSnapshot struct {
	Context                       ConversationRouteContext
	Summary                       *StoredConversationSummary
	AggregationUnresolved         bool
	AggregationNeedsClarification bool
	AggregationCanWait            bool
	ResultReviewSuperseded        bool
	PreviousMissing               bool
	PreviousCycle                 bool
	PreviousRoutingState          string
	PreviousTaskID                string
	PreviousMessageID             string
	PreviousSequence              int64
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

// ConversationRouteAggregation lets a fresh New classification enter its
// durable fixed collection window. Implementations must fence the lease and
// increment route_attempt so a retry is a distinct model call.
type ConversationRouteAggregation interface {
	BeginConversationRouteAggregation(context.Context, ConversationRouteClaim, ConversationRouteSnapshot, ConversationRouteDecision) (bool, error)
}

// ConversationRouteAggregationResolution rechecks unresolved receipts under
// the conversation lock before failing or releasing an aggregation lease.
type ConversationRouteAggregationResolution interface {
	ResolveConversationRouteAggregation(context.Context, ConversationRouteClaim) error
}

// ConversationRoutePredecessorRecovery atomically releases only an
// undispatched reservation before requeuing a route whose predecessor arrived.
type ConversationRoutePredecessorRecovery interface {
	RequeueConversationRouteAfterPredecessor(context.Context, ConversationRouteClaim, ConversationRouteSnapshot) (bool, error)
}

// ConversationRouteContextRecovery releases a route for a fresh classification
// after the model gateway rejects a stale authorization snapshot.
type ConversationRouteContextRecovery interface {
	RequeueConversationRouteAfterContextChange(context.Context, ConversationRouteClaim) (bool, error)
}

// ConversationRouteRateLimitRecovery records the confirmed provider rejection
// and atomically retries or fails the route.
type ConversationRouteRateLimitRecovery interface {
	HandleConversationRouteRateLimit(context.Context, ConversationRouteClaim, time.Duration) (bool, error)
}

// ConversationRouteInvalidOutputRecovery requeues one settled invalid route
// result for a separately metered, text-only repair attempt.
type ConversationRouteInvalidOutputRecovery interface {
	RequeueConversationRouteAfterInvalidOutput(context.Context, ConversationRouteClaim) (bool, error)
}

// ConversationRouteRepairStatus marks only the attempt immediately following
// a settled invalid output for the repair prompt.
type ConversationRouteRepairStatus interface {
	IsConversationRouteRepairAttempt(context.Context, ConversationRouteClaim) (bool, error)
}
