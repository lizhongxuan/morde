package task

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
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

type QueuePriority string

const (
	QueuePriorityPinned     QueuePriority = "pinned"
	QueuePriorityNormal     QueuePriority = "normal"
	QueuePriorityBackground QueuePriority = "background"
)

func (priority QueuePriority) Valid() bool {
	return priority == QueuePriorityPinned || priority == QueuePriorityNormal || priority == QueuePriorityBackground
}

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
	StepCancelled   StepStatus = "cancelled"
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
	InitiatorID    string
	ConversationID string
	// ToolsDisabled is set for text-only regeneration variants. It is transient
	// claim metadata sourced from the conversation variant operation ledger.
	ToolsDisabled bool `json:"-"`
	// InternalTool is transient claim metadata for a short-lived, user-hidden
	// route call that must execute through the existing Broker without DSH.
	InternalTool      bool `json:"-"`
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
	ModelRecovery               *ModelRecoveryIntent `json:"-"`
	CheckpointDigest            []byte
	CheckpointThroughSequence   int64
	EventSequence               int64
	CurrentRevision             *int64
	DesiredRevision             *int64
	Objective                   string
	QueuePriority               QueuePriority
	// QueueEnteredAt is the task's durable updated_at captured immediately
	// before a worker claim. It is internal queue-wait telemetry only.
	QueueEnteredAt      time.Time `json:"-"`
	ConversationContext *ConversationContext
	DependencyResults   []DependencyResult
	FileDeliveryIntent  FileDeliveryIntent
	Status              Status
	Authorization       Authorization
	Budget              Budget
	Usage               Usage
	Lease               Lease
	CurrentStep         string
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
	OwnerID              string
	ProjectID            string
	InitiatorID          string
	ConversationID       string
	Scope                string
	ThroughSequence      int64
	Digest               []byte
	AgentToolProfile     AgentToolProfile `json:"-"`
	Summary              *ConversationContextSummary
	CurrentMessageSource *ConversationMessageSource `json:"-"`
	Messages             []ConversationContextMessage
}

// AgentToolProfile is a server-validated, least-privilege hint for the first
// DSH Task turn. It narrows the tools exposed to the model; it never grants
// authority beyond the Task's current Broker authorization.
type AgentToolProfile string

const AgentToolProfileWebResearch AgentToolProfile = "web_research"

func (profile AgentToolProfile) Valid() bool {
	return profile == "" || profile == AgentToolProfileWebResearch
}

// ConversationMessageSource is the immutable page-hash manifest for a Task's
// current long user message. It is persisted with the initial Task event and
// loaded only after the source identity and full digest are revalidated.
type ConversationMessageSource struct {
	MessageID    string                          `json:"message_id"`
	Sequence     int64                           `json:"sequence"`
	ContentBytes int64                           `json:"content_bytes"`
	TotalRunes   int64                           `json:"total_runes"`
	SHA256       []byte                          `json:"sha256"`
	Pages        []ConversationMessageSourcePage `json:"pages"`
}

type ConversationMessageSourcePage struct {
	RuneOffset     int64  `json:"rune_offset"`
	NextRuneOffset int64  `json:"next_rune_offset"`
	SHA256         []byte `json:"sha256"`
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
	RetryAfterAt   *time.Time
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
	ID          string
	TaskID      string
	Key         string
	RequestHash []byte
	// TaskRevision, CallID, and ToolName bind an operation to one Task
	// revision and exact Agent tool call. They remain nil/empty on legacy and
	// non-Agent operations that do not carry this binding.
	TaskRevision                *int64
	CallID                      string
	ToolName                    string
	ApprovalID                  string
	ApprovalVersion             *int64
	ApprovalRequestHash         []byte
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
	ID                 string
	TaskID             string
	Key                string
	Input              string
	FileDeliveryIntent FileDeliveryIntent
	Status             BatchItemStatus
	Attempt            int32
	Result             string
	Error              string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Artifact struct {
	ID           string
	TaskID       string
	StepID       string
	SourceCallID string `json:"-"`
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

const RouteFileDeliveryIntentClarificationCallID = "morde-route-file-delivery-intent"

const (
	ClarificationPending   ClarificationStatus = "pending"
	ClarificationAnswered  ClarificationStatus = "answered"
	ClarificationCancelled ClarificationStatus = "cancelled"
)

type ClarificationAnswerFormat string

const (
	ClarificationAnswerFreeText   ClarificationAnswerFormat = "free_text"
	ClarificationAnswerStructured ClarificationAnswerFormat = "structured"
)

// ClarificationQuestion is one stable input in a structured clarification.
// Question and option IDs are opaque identifiers supplied by the Agent and
// remain bound to the enclosing clarification request version.
type ClarificationQuestion struct {
	ID        string                        `json:"id"`
	Prompt    string                        `json:"prompt"`
	Type      ClarificationQuestionType     `json:"type"`
	Required  bool                          `json:"required"`
	AllowSkip bool                          `json:"allow_skip"`
	Options   []ClarificationQuestionOption `json:"options"`
}

type ClarificationQuestionType string

const (
	ClarificationSingleChoice ClarificationQuestionType = "single_choice"
	ClarificationMultiChoice  ClarificationQuestionType = "multi_choice"
	ClarificationFreeText     ClarificationQuestionType = "free_text"
)

type ClarificationQuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ClarificationAnswer is one explicit answer to one question. Skipping is
// represented independently from an empty answer and does not grant approval
// or change any Task authorization.
type ClarificationAnswer struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids"`
	Text       string   `json:"text"`
	Skipped    bool     `json:"skipped"`
}

// ValidateClarificationQuestions validates a bounded, stable question group.
func ValidateClarificationQuestions(questions []ClarificationQuestion) error {
	if len(questions) == 0 || len(questions) > 8 {
		return errors.New("clarification question count is out of range")
	}
	questionIDs := make(map[string]struct{}, len(questions))
	for _, question := range questions {
		if !validClarificationItemID(question.ID) || !validClarificationText(question.Prompt, 1024) {
			return errors.New("clarification question identity or prompt is invalid")
		}
		if question.Required && question.AllowSkip {
			return errors.New("required clarification questions cannot allow skipping")
		}
		if _, exists := questionIDs[question.ID]; exists {
			return errors.New("clarification question IDs must be unique")
		}
		questionIDs[question.ID] = struct{}{}
		optionIDs := make(map[string]struct{}, len(question.Options))
		switch question.Type {
		case ClarificationSingleChoice, ClarificationMultiChoice:
			if len(question.Options) < 2 || len(question.Options) > 12 {
				return errors.New("clarification choice option count is out of range")
			}
			for _, option := range question.Options {
				if !validClarificationItemID(option.ID) || !validClarificationText(option.Label, 256) {
					return errors.New("clarification choice option is invalid")
				}
				if _, exists := optionIDs[option.ID]; exists {
					return errors.New("clarification option IDs must be unique")
				}
				optionIDs[option.ID] = struct{}{}
			}
		case ClarificationFreeText:
			if len(question.Options) != 0 {
				return errors.New("free-text clarification cannot include options")
			}
		default:
			return errors.New("clarification question type is invalid")
		}
	}
	return nil
}

// ValidateClarificationAnswers requires one explicit answer or skip per
// question and rejects values not offered by the bound question group.
func ValidateClarificationAnswers(questions []ClarificationQuestion, answers []ClarificationAnswer) error {
	if err := ValidateClarificationQuestions(questions); err != nil {
		return fmt.Errorf("invalid clarification question group: %w", err)
	}
	if len(answers) != len(questions) {
		return errors.New("every clarification question requires an explicit answer or skip")
	}
	byID := make(map[string]ClarificationQuestion, len(questions))
	for _, question := range questions {
		byID[question.ID] = question
	}
	seen := make(map[string]struct{}, len(answers))
	for _, answer := range answers {
		question, exists := byID[answer.QuestionID]
		if !exists {
			return errors.New("clarification answer references an unknown question")
		}
		if _, duplicate := seen[answer.QuestionID]; duplicate {
			return errors.New("clarification answer repeats a question")
		}
		seen[answer.QuestionID] = struct{}{}
		if answer.Skipped {
			if question.Required || !question.AllowSkip || answer.Text != "" || len(answer.OptionIDs) != 0 {
				return errors.New("clarification question cannot be skipped with these values")
			}
			continue
		}
		switch question.Type {
		case ClarificationFreeText:
			if !validClarificationText(answer.Text, 4096) || len(answer.OptionIDs) != 0 {
				return errors.New("free-text clarification answer is invalid")
			}
		case ClarificationSingleChoice, ClarificationMultiChoice:
			if answer.Text != "" || len(answer.OptionIDs) == 0 ||
				(question.Type == ClarificationSingleChoice && len(answer.OptionIDs) != 1) ||
				(question.Type == ClarificationMultiChoice && len(answer.OptionIDs) > len(question.Options)) {
				return errors.New("clarification choice answer is invalid")
			}
			allowed := make(map[string]struct{}, len(question.Options))
			for _, option := range question.Options {
				allowed[option.ID] = struct{}{}
			}
			selected := make(map[string]struct{}, len(answer.OptionIDs))
			for _, optionID := range answer.OptionIDs {
				if _, ok := allowed[optionID]; !ok {
					return errors.New("clarification answer selects an unknown option")
				}
				if _, duplicate := selected[optionID]; duplicate {
					return errors.New("clarification answer repeats an option")
				}
				selected[optionID] = struct{}{}
			}
		}
	}
	return nil
}

func validClarificationItemID(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validClarificationText(value string, maxRunes int) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00') && len([]rune(value)) <= maxRunes
}

// ClarificationRequest is an Agent question awaiting an answer. Legacy
// requests contain only Question; structured requests also contain Questions.
// It is separate from PendingRequest, which represents an exact tool approval.
type ClarificationRequest struct {
	ID           string
	TaskID       string
	Version      int64
	ModelStepID  string
	CallID       string
	Question     string
	Questions    []ClarificationQuestion
	AnswerFormat ClarificationAnswerFormat
	BatchItemID  string
	BatchAttempt int32
	Status       ClarificationStatus
	Answer       string
	CreatedAt    time.Time
	RespondedAt  *time.Time
}

// PendingRequest is an immutable, versioned user approval bound to one exact
// Agent tool call and Host control fence. RequestHash is internal and omitted
// from API responses.
//
// Evidence fields (E05): EvidencePointer names the in-task step whose
// command and output justify the action ("step:<step-id>"); ThisCostMicros
// and TotalCostMicros are micro-units of the task's cost, nil when no trusted
// source exists (never 0 as a fabricated placeholder); TriggerRule names the
// action-matrix category that required approval; RejectionConsequence states
// in plain text what happens if the user rejects.
type PendingRequest struct {
	ID      string
	TaskID  string
	Version int64
	// TaskRevision is the Task revision whose exact tool call this approval
	// covers. A nil value is retained only for legacy rows or non-bound paths.
	TaskRevision            *int64
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
	// EvidencePointer is "step:<step-id>" of the step whose evidence justifies
	// the action, or "" when no step is attached (legacy rows).
	EvidencePointer string
	// ThisCostMicros is the trusted cost of this exact action in micro-units,
	// or nil when no trusted per-action cost source exists. There is no such
	// source today: tool calls carry no price declaration, so this is nil at
	// creation and the API renders it unknown.
	ThisCostMicros *int64
	// TotalCostMicros is the trusted cumulative cost of the task in
	// micro-units at the moment the card was created, or nil when any cost
	// component is unknown. It is a snapshot, not a live total.
	TotalCostMicros *int64
	// TriggerRule names the action-matrix category that required approval,
	// or "" for legacy rows. See ApprovalTrigger* constants.
	TriggerRule string
	// RejectionConsequence states what happens if the user rejects, or ""
	// when the approval text did not state one (legacy rows).
	RejectionConsequence string
}

// Approval trigger rules: the action-matrix categories (TODO 05 M05-05)
// that require a per-action approval card.
const (
	// ApprovalTriggerHostSensitiveTool is a sensitive Host tool: shell,
	// browser/desktop input, Enter/Backspace/Delete, or an unknown tool.
	ApprovalTriggerHostSensitiveTool = "host_sensitive_tool"
	// ApprovalTriggerExternalMCPTool is an external MCP tool call.
	ApprovalTriggerExternalMCPTool = "external_mcp_tool"
	// ApprovalTriggerWorkspaceLiveWrite is a live Workspace file write.
	ApprovalTriggerWorkspaceLiveWrite = "workspace_live_write"
	// ApprovalTriggerMediaPublish is a media publish action. Reserved for
	// the publish path when it mints its own card; no creation site uses it
	// yet (publish approvals currently reuse the Workspace write path).
	ApprovalTriggerMediaPublish = "media_publish"
)

// ValidApprovalTriggerRule reports whether value is an empty (legacy) or a
// known approval trigger rule.
func ValidApprovalTriggerRule(value string) bool {
	switch value {
	case "", ApprovalTriggerHostSensitiveTool, ApprovalTriggerExternalMCPTool,
		ApprovalTriggerWorkspaceLiveWrite, ApprovalTriggerMediaPublish:
		return true
	default:
		return false
	}
}

// EvidencePointerForStep returns the evidence pointer for a step, or "" when
// the step ID is invalid. The second result reports validity.
func EvidencePointerForStep(stepID string) (string, bool) {
	if !domain.ValidStepID(stepID) {
		return "", false
	}
	return "step:" + stepID, true
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

// CanTransitionWithReason permits workflow-specific transitions only when the
// persisted reason names the workflow that owns the edge. Generic Task state
// transitions remain conservative for all other waiting_user reasons.
func CanTransitionWithReason(from, to Status, reason string) bool {
	if from == StatusWaitingUser && reason == "model_budget_capacity" &&
		(to == StatusQueued || to == StatusPaused) {
		return true
	}
	return CanTransition(from, to)
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
