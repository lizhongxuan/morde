package task

import (
	"errors"
	"time"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusWaitingUser Status = "waiting_user"
	StatusPausing     Status = "pausing"
	StatusPaused      Status = "paused"
	StatusCancelling  Status = "cancelling"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
)

const WaitingReasonWorkspaceHostSync = "workspace_host_sync"

type FileDeliveryIntent string

const (
	FileDeliveryNone     FileDeliveryIntent = "none"
	FileDeliveryRequired FileDeliveryIntent = "required"
	FileDeliveryUnclear  FileDeliveryIntent = "unclear"
)

func (intent FileDeliveryIntent) Valid() bool {
	return intent == FileDeliveryNone || intent == FileDeliveryRequired || intent == FileDeliveryUnclear
}

type OperationStatus string

const (
	OperationPrepared   OperationStatus = "prepared"
	OperationDispatched OperationStatus = "dispatched"
	OperationSucceeded  OperationStatus = "succeeded"
	OperationFailed     OperationStatus = "failed"
	OperationCancelled  OperationStatus = "cancelled"
	OperationUnknown    OperationStatus = "unknown"
)

type Action string

const (
	ActionPause  Action = "pause"
	ActionResume Action = "resume"
	ActionCancel Action = "cancel"
)

type StopRequestStatus string

const (
	StopRequestPending   StopRequestStatus = "pending"
	StopRequestCompleted StopRequestStatus = "completed"
)

type StepStatus string

const (
	StepQueued      StepStatus = "queued"
	StepRunning     StepStatus = "running"
	StepWaitingUser StepStatus = "waiting_user"
	StepSucceeded   StepStatus = "succeeded"
	StepFailed      StepStatus = "failed"
	StepUnknown     StepStatus = "unknown"
)

type BatchItemStatus string

const (
	BatchItemQueued    BatchItemStatus = "queued"
	BatchItemRunning   BatchItemStatus = "running"
	BatchItemSucceeded BatchItemStatus = "succeeded"
	BatchItemFailed    BatchItemStatus = "failed"
	BatchItemUnknown   BatchItemStatus = "unknown"
)

type Budget struct {
	ActiveSeconds int64
	ToolCalls     int64
	ModelTokens   int64
}

const (
	MaxActiveSeconds = 1800
	MaxToolCalls     = 100
	MaxModelTokens   = 50000
)

func (b Budget) Validate() error {
	if b.ActiveSeconds < 1 || b.ActiveSeconds > MaxActiveSeconds {
		return errors.New("active seconds budget is out of range")
	}
	if b.ToolCalls < 1 || b.ToolCalls > MaxToolCalls {
		return errors.New("tool calls budget is out of range")
	}
	if b.ModelTokens < 1 || b.ModelTokens > MaxModelTokens {
		return errors.New("model tokens budget is out of range")
	}
	return nil
}

type Usage struct {
	ActiveSeconds int64
	ToolCalls     int64
	ModelTokens   *int64
}

type Authorization struct {
	Kind                 string
	TokenID              string
	ClientInstallationID string
	Scopes               []string
	Version              int64
	ExpiresAt            *time.Time
}

type Lease struct {
	Owner     string
	Version   int64
	ExpiresAt *time.Time
}

type Task struct {
	ID      string
	OwnerID string
	// ProjectID is set only for a Task in the project's shared conversation.
	ProjectID string
	// InitiatorID is the account whose authorization and resources execute this Task.
	// OwnerID remains the conversation owner so the historical association is stable.
	InitiatorID       string
	ConversationID    string
	HostID            string
	ExecutionLocation string
	ApplicationID     string
	ModelID           string
	WorkspaceID       string
	WorkspaceOwnerID  string

	WorkspaceProjectResourceVersion *int64

	InputRevision               *int64
	WorkspaceCheckpointRevision *int64
	ContextDigest               []byte
	AgentInstanceID             string
	AgentGeneration             int64
	AgentSessionID              string
	AgentEngineVersion          string
	AgentProfileVersion         string
	StepRetry                   *StepRetryIntent
	CheckpointDigest            []byte
	CheckpointThroughSequence   int64
	EventSequence               int64
	CurrentRevision             *int64
	DesiredRevision             *int64
	Objective                   string
	ConversationContext         *ConversationContext
	DependencyResults           []DependencyResult
	FileDeliveryIntent          FileDeliveryIntent
	Status                      Status
	Authorization               Authorization
	Budget                      Budget
	Usage                       Usage
	Lease                       Lease
	CurrentStep                 string
	// StepCount and CompletedStepCount are read projections for list summaries.
	// Budget steps are excluded; completed counts include only succeeded steps.
	StepCount          int64
	CompletedStepCount int64
	Result             string
	WaitingReason      string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	FinishedAt         *time.Time
}

type DependencyResult struct {
	TaskID           string `json:"task_id"`
	RequiredRevision int64  `json:"required_revision"`
	Objective        string `json:"objective"`
	Result           string `json:"result"`
}

// ConversationContext is the authorized, bounded history snapshot loaded for
// a freshly claimed Task. It is transient: the durable boundary and digest are
// stored with the Task's initial event, while Messages are rebuilt and checked
// by the Store on each claim. Resumed agent sessions use their own checkpoint.
type ConversationContext struct {
	OwnerID         string
	ProjectID       string
	InitiatorID     string
	ConversationID  string
	Scope           string
	ThroughSequence int64
	Digest          []byte
	Summary         *ConversationContextSummary
	Messages        []ConversationContextMessage
}

// ConversationContextSummary is an immutable reference to the encrypted
// summary snapshot fixed when the Task was created. Ciphertext, proofs and
// plaintext are loaded transiently and are not part of the Task event JSON.
type ConversationContextSummary struct {
	ServiceInstanceID    string                             `json:"service_instance_id"`
	OwnerID              string                             `json:"owner_id"`
	ActorID              string                             `json:"actor_id"`
	ConversationID       string                             `json:"conversation_id"`
	ScopeKind            string                             `json:"scope_kind"`
	ScopeID              string                             `json:"scope_id"`
	AuthorizationVersion int64                              `json:"authorization_version"`
	ScopeVersion         int64                              `json:"scope_version"`
	Version              int64                              `json:"version"`
	OperationID          string                             `json:"operation_id"`
	SummarySHA256        []byte                             `json:"summary_sha256"`
	SourcesSHA256        []byte                             `json:"sources_sha256"`
	Ciphertext           []byte                             `json:"-"`
	Sources              []ConversationContextSummarySource `json:"-"`
	Text                 string                             `json:"-"`
}

type ConversationContextSummarySource struct {
	Kind       string
	ID         string
	ObjectType string
	Version    int64
	Sequence   int64
	SHA256     []byte
}

const MaxConversationContextMessages = 128

type ConversationContextMessage struct {
	ID            string
	Sequence      int64
	Role          string
	Content       string
	Truncated     bool
	TaskID        string                          `json:"-"`
	TaskRevision  int64                           `json:"-"`
	EventSequence int64                           `json:"-"`
	TaskResults   []ConversationContextTaskResult `json:"-"`
}

type ConversationContextTaskResult struct {
	TaskID        string `json:"task_id"`
	TaskRevision  int64  `json:"task_revision"`
	EventSequence int64  `json:"event_sequence"`
}

type Step struct {
	ID             string
	TaskID         string
	Sequence       int64
	Attempt        int64
	RetryOfStepID  string
	Kind           string
	Status         StepStatus
	Title          string
	Summary        string
	FailureReason  string
	Retryable      bool
	AgentSessionID string
	TaskRevision   *int64
	ToolCallKey    string
	ToolName       string
	RequestDigest  []byte
	Phase          string
	DetailState    string
	CreatedAt      time.Time
}

// StepRetryIntent is a single-use server-issued replay fence. The model never
// supplies the arguments; the runtime must recover them from this exact DSH
// checkpoint and match the stored request digest before dispatch.
type StepRetryIntent struct {
	TaskID                      string
	OwnerID                     string
	SourceStepID                string
	AttemptStepID               string
	Attempt                     int64
	SourceCallID                string
	RetryCallID                 string
	AgentSessionID              string
	TaskRevision                int64
	ToolName                    string
	RequestDigest               []byte
	AuthorizationVersion        int64
	HostID                      string
	HostRuntimeGeneration       int64
	WorkspaceID                 string
	InputRevision               *int64
	WorkspaceCheckpointRevision *int64
	WorkspaceBindingVersion     int64
	WorkspaceRevision           *int64
	CheckpointThroughSequence   int64
	CheckpointDigest            []byte
	Consumed                    bool
	ExpiresAt                   time.Time
}

func StepCounts(steps []Step) (total, completed int64) {
	for _, step := range steps {
		if step.Kind == "budget" {
			continue
		}
		total++
		if step.Status == StepSucceeded {
			completed++
		}
	}
	return total, completed
}

type Operation struct {
	ID                          string
	TaskID                      string
	Key                         string
	RequestHash                 []byte
	HostID                      string
	WorkspaceID                 string
	BindingVersion              int64
	InputRevision               *int64
	WorkspaceCheckpointRevision *int64
	RuntimeGeneration           int64
	RuntimeInstanceID           string
	ControlVersion              int64
	LeaseVersion                int64
	// RuntimeLeaseVersion is the task lease version fencing this operation's
	// runtime journal entry. It does not advance merely because a new worker
	// owns the task; it may advance only when an exact-fenced operation_not_found
	// proves the previous lease never journaled the operation and the same ID
	// is explicitly rearmed for dispatch under the current lease.
	RuntimeLeaseVersion int64
	Status              OperationStatus
	ExternalID          string
	Result              string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type BatchItem struct {
	ID        string
	TaskID    string
	Key       string
	Input     string
	Status    BatchItemStatus
	Attempt   int32
	Result    string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Artifact struct {
	ID           string
	TaskID       string
	StepID       string
	BatchItemID  string
	BatchAttempt int32
	WorkspaceID  string
	Revision     int64
	SizeBytes    int64
	SHA256       string
	Label        string
	Path         string
	MediaType    string
	CreatedAt    time.Time
}

type Event struct {
	TaskID    string
	Sequence  int64
	Type      string
	Payload   []byte
	CreatedAt time.Time
}

type PendingRequestStatus string

const (
	PendingRequestPending   PendingRequestStatus = "pending"
	PendingRequestApproved  PendingRequestStatus = "approved"
	PendingRequestRejected  PendingRequestStatus = "rejected"
	PendingRequestCancelled PendingRequestStatus = "cancelled"
	PendingRequestExpired   PendingRequestStatus = "expired"
)

type PendingDecision string

const (
	PendingDecisionApprove PendingDecision = "approve"
	PendingDecisionReject  PendingDecision = "reject"
)

type ClarificationStatus string

const (
	ClarificationPending   ClarificationStatus = "pending"
	ClarificationAnswered  ClarificationStatus = "answered"
	ClarificationCancelled ClarificationStatus = "cancelled"
)

// ClarificationRequest is an Agent question awaiting a free-form user answer.
// It is separate from PendingRequest, which represents an exact tool approval.
type ClarificationRequest struct {
	ID          string
	TaskID      string
	Version     int64
	ModelStepID string
	CallID      string
	Question    string
	Status      ClarificationStatus
	Answer      string
	CreatedAt   time.Time
	RespondedAt *time.Time
}

// PendingRequest is an immutable, versioned user approval bound to one exact
// Agent tool call and Host control fence. RequestHash is internal and omitted
// from API responses.
type PendingRequest struct {
	ID                      string
	TaskID                  string
	Version                 int64
	Kind                    string
	Title                   string
	Object                  string
	Content                 string
	Impact                  string
	CallID                  string
	MediaPreviewOperationID string
	ToolName                string
	RequestHash             []byte
	RuntimeGeneration       int64
	RuntimeInstanceID       string
	ControlVersion          int64
	Status                  PendingRequestStatus
	Decision                PendingDecision
	CreatedAt               time.Time
	ExpiresAt               *time.Time
	RespondedAt             *time.Time
}

// StopRequest is the durable task-level request that prevents new operations
// and records when the last runtime task lease can no longer admit a late
// operation. Runtime operations are settled separately before the task enters
// paused or cancelled.
type StopRequest struct {
	ID             string
	TaskID         string
	Action         Action
	Status         StopRequestStatus
	SafeAfter      time.Time
	LeaseOwner     string
	LeaseVersion   int64
	LeaseExpiresAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func CanTransition(from, to Status) bool {
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusPausing || to == StatusCancelling || to == StatusPaused || to == StatusCancelled
	case StatusRunning:
		return to == StatusWaitingUser || to == StatusPausing || to == StatusCancelling || to == StatusCompleted || to == StatusFailed
	case StatusWaitingUser:
		return to == StatusRunning || to == StatusPausing || to == StatusCancelling || to == StatusFailed
	case StatusPausing:
		return to == StatusPaused || to == StatusFailed || to == StatusCancelling
	case StatusPaused:
		return to == StatusQueued || to == StatusCancelling || to == StatusCancelled
	case StatusCancelling:
		return to == StatusCancelled || to == StatusFailed
	default:
		return false
	}
}

func CanTransitionOperation(from, to OperationStatus) bool {
	switch from {
	case OperationPrepared:
		return to == OperationDispatched || to == OperationFailed || to == OperationCancelled
	case OperationDispatched:
		return to == OperationSucceeded || to == OperationFailed || to == OperationUnknown || to == OperationCancelled
	case OperationUnknown:
		return to == OperationPrepared || to == OperationSucceeded || to == OperationFailed || to == OperationCancelled
	default:
		return false
	}
}
