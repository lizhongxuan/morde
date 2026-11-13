package app

import (
	"context"
	"time"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const MaxAssistantMessageBytes = 32 << 10
const maxEventPage = 100

type PageRequest struct {
	Limit  int32
	Cursor string
	Search string
}

type ConversationPage struct {
	Items      []conversation.Conversation
	NextCursor string
}

type MessagePage struct {
	Items      []conversation.Message
	NextCursor string
}

type TaskPage struct {
	Items                 []task.Task
	NextCursor            string
	PendingCount          int64
	PendingCountsByHostID map[string]int64
	K2Projections         map[string]TaskK2Projection
}

type ConversationTaskView string

const (
	ConversationTaskViewActive ConversationTaskView = "active"
	ConversationTaskViewRecent ConversationTaskView = "recent"
)

type TaskListRequest struct {
	HostIDs []string
	Status  task.Status
	Page    PageRequest
}

type Dependency struct {
	TaskID           string
	RequiredRevision int64
	TaskStatus       task.Status
	Satisfied        bool
}

type ChildJob struct {
	JobID        string
	TaskStepID   string
	Title        string
	TaskRevision int64
	Status       task.StepStatus
}

type ResultReview struct {
	State               string
	SourceTaskID        string
	SourceTaskRevision  int64
	SourceEventSequence int64
	TriggerMessageID    *string
	ReplyMessageID      *string
}

type TaskK2Projection struct {
	Dependencies []Dependency
	ChildJobs    []ChildJob
	ResultReview *ResultReview
}

type TaskDetails struct {
	Task                   task.Task
	Dependencies           []Dependency
	ChildJobs              []ChildJob
	ResultReview           *ResultReview
	AssistantMessage       *AssistantMessageSnapshot
	CurrentRevisionStarted *bool
	ResultMessagePending   bool
	ResultMessagePublished bool
	ScheduleObservation    *schedule.ObservationSpec
	Steps                  []task.Step
	Agents                 []TaskAgentExecution
	BatchItems             []task.BatchItem
	Artifacts              []task.Artifact
	PendingRequest         *task.PendingRequest
	ClarificationRequests  []task.ClarificationRequest
	StopRequest            *task.StopRequest
	StopOperationIDs       []string
}

type TaskAgentExecution struct {
	ID           string
	ParentID     string
	StepID       string
	TaskRevision int64
	Role         string
	Status       string
	ObservedAt   *time.Time
}

type TaskAgentActivityObservation struct {
	Fence               AgentInstanceFence
	Status              string
	ObservationSequence int64
}

type TaskAgentActivityRecorder interface {
	RecordTaskAgentActivity(context.Context, TaskAgentActivityObservation) (bool, error)
}

type TaskStepDetailRecord struct {
	TaskID        string
	OwnerID       string
	StepID        string
	TaskRevision  int64
	EventSequence int64
	Ciphertext    []byte
	SHA256        []byte
	SizeBytes     int32
	Truncated     bool
}

type TaskStepDetailDocument struct {
	Object string `json:"object"`
	Input  string `json:"input"`
	Result string `json:"result"`
}

type TaskStepDetail struct {
	TaskID        string  `json:"task_id"`
	StepID        string  `json:"step_id"`
	EventSequence int64   `json:"event_sequence"`
	ToolName      string  `json:"tool_name"`
	DetailState   string  `json:"detail_state"`
	Object        *string `json:"object"`
	Input         *string `json:"input"`
	Result        *string `json:"result"`
	Truncated     bool    `json:"truncated"`
}

type TaskStepDetailReader interface {
	GetTaskStepDetail(context.Context, string, string, string) (TaskStepDetailRecord, error)
}

type TaskToolCallProgress struct {
	AgentSessionID       string
	AgentInstanceID      string
	AgentGeneration      int64
	AuthorizationVersion int64
	TaskRevision         int64
	CallKey              string
	ToolName             string
	RequestDigest        []byte
	Phase                string
	DetailState          string
	DetailCiphertext     []byte
	DetailSHA256         []byte
	DetailSizeBytes      int32
	Truncated            bool
}

type AssistantMessageStatus string

const (
	AssistantMessageStreaming   AssistantMessageStatus = "streaming"
	AssistantMessageCompleted   AssistantMessageStatus = "completed"
	AssistantMessageInterrupted AssistantMessageStatus = "interrupted"
)

// AssistantMessageSnapshot is the latest assistant model turn for a Task.
// Streaming text survives event-cursor expiry; the final conversation message
// replaces the streamed text under the same message ID.
type AssistantMessageSnapshot struct {
	Message             conversation.Message   `json:"message"`
	Status              AssistantMessageStatus `json:"status"`
	TaskRevision        *int64                 `json:"task_revision,omitempty"`
	ResultEventSequence *int64                 `json:"result_event_sequence,omitempty"`
}

type TaskActionResult struct {
	Task                  task.Task
	Action                task.Action
	UnsettledOperationIDs []string
}

// TaskActionRevisionFence binds a user action to the exact revision pair the
// caller observed. The store compares both values while holding the task row
// lock and in the same transaction as the resulting state transition.
type TaskActionRevisionFence struct {
	CurrentRevision int64 `json:"expected_current_revision"`
	DesiredRevision int64 `json:"expected_desired_revision"`
}

type TaskStopClaim struct {
	Task       task.Task
	Request    task.StopRequest
	Operations []task.Operation
}

type IdempotencyRecord struct {
	OwnerID     string
	Operation   string
	Key         string
	RequestHash []byte
	ResourceID  string
	RelatedID   string
}

type TaskSubmission struct {
	Conversation                *conversation.Conversation
	Task                        task.Task
	Message                     conversation.Message
	InitialEvent                task.Event
	InitialSteps                []task.Step
	InitialClarificationRequest *task.ClarificationRequest
	BatchItems                  []task.BatchItem
	MCPConnectorIDs             []string
	Idempotency                 IdempotencyRecord
}

type TaskSubmissionResult struct {
	Conversation conversation.Conversation
	Message      conversation.Message
	Task         task.Task
	Replayed     bool
}

type TaskProgress struct {
	TaskID                   string
	LeaseOwner               string
	LeaseVersion             int64
	Status                   task.Status
	CurrentStep              string
	Result                   string
	WaitingReason            string
	Usage                    task.Usage
	ObservationFingerprint   string
	Steps                    []task.Step
	ToolCall                 *TaskToolCallProgress
	Artifacts                []task.Artifact
	Event                    task.Event
	PendingRequest           *task.PendingRequest
	TaskMediaApproval        *TaskMediaApprovalCommit
	ClarificationRequest     *task.ClarificationRequest
	AssistantMessage         *conversation.Message
	PublishResultMessage     bool
	CloudDSHSkillRead        *CloudDSHSkillReadCommit
	CloudDSHPersonalMutation *CloudDSHPersonalMutationCommit
}

// CloudDSHSkillReadCommit binds a fixed skill response to one durable dsh call.
// The Task event stores source metadata and hashes; the skill text stays in the encrypted dsh checkpoint.
type CloudDSHSkillReadCommit struct {
	TaskID               string
	OwnerID              string
	AgentSessionID       string
	AuthorizationVersion int64
	CallID               string
	SkillID              string
	SkillVersion         string
	BundleVersion        string
	SourceRef            string
	ContentSHA256        string
	OutputSHA256         string
	FingerprintSHA256    string
}

// CloudDSHPersonalMutationCommit binds a personal object update to one dsh
// call. The Task event records only metadata and hashes, never the body.
type CloudDSHPersonalMutationCommit struct {
	TaskID               string
	OwnerID              string
	AgentSessionID       string
	AuthorizationVersion int64
	CallID               string
	Mutation             personal.Mutation
	FingerprintSHA256    string
	// Goal writes bind the original call as well as the resulting object.
	// Empty values retain the original personal-object receipt format.
	GoalToolName        string
	GoalArgumentsSHA256 string
	GoalStatus          string
}

type AssistantTextDelta struct {
	TaskID       string
	LeaseOwner   string
	LeaseVersion int64
	MessageID    string
	TurnID       string
	Delta        string
}

type OperationUpdate struct {
	TaskID              string
	OperationKey        string
	LeaseOwner          string
	LeaseVersion        int64
	RuntimeGeneration   int64
	RuntimeInstanceID   string
	ControlVersion      int64
	RuntimeLeaseVersion int64
	RearmAfterNotFound  bool
	AdoptPrepared       bool
	Status              task.OperationStatus
	ExternalID          string
	Result              string
}

// TaskEventPage carries the task authorization and stream cursor state with a
// small event batch so SSE polling does not load the full task detail graph.
type TaskEventPage struct {
	HostID          string
	Status          task.Status
	CurrentSequence int64
	Items           []task.Event
}

type TaskWorkspaceViewDeletion struct {
	ID, TaskID, HostID string
	Status             string
}

type TaskWorkspaceViewDeletionStore interface {
	RequestTaskWorkspaceViewDeletion(context.Context, IdempotencyRecord, string) (TaskWorkspaceViewDeletion, error)
	GetTaskWorkspaceViewDeletion(context.Context, string, string) (TaskWorkspaceViewDeletion, error)
	ListPendingTaskWorkspaceViewDeletions(context.Context, string, string, int64) ([]TaskWorkspaceViewDeletion, error)
	CompleteTaskWorkspaceViewDeletion(context.Context, string, string, int64, string, string) (TaskWorkspaceViewDeletion, error)
}

// TaskStore owns atomic task submission, per-task event sequencing and worker leases.
type TaskStore interface {
	CreateConversation(context.Context, conversation.Conversation, IdempotencyRecord) (conversation.Conversation, bool, error)
	GetConversation(context.Context, string, string) (conversation.Conversation, error)
	ListConversations(context.Context, string, []string, PageRequest) (ConversationPage, error)
	ListMessages(context.Context, string, string, PageRequest) (MessagePage, error)
	// Project conversation operations must scope both by projectID and the
	// actor's current membership, even when the caller has already checked access.
	CreateProjectConversation(context.Context, string, conversation.Conversation, IdempotencyRecord) (conversation.Conversation, bool, error)
	GetProjectConversation(context.Context, string, string, string) (conversation.Conversation, error)
	ListProjectConversations(context.Context, string, string, PageRequest) (ConversationPage, error)
	ListProjectConversationMessages(context.Context, string, string, string, PageRequest) (MessagePage, error)
	SubmitTask(context.Context, TaskSubmission) (TaskSubmissionResult, error)
	GetTask(context.Context, string, string) (TaskDetails, error)
	ListTasks(context.Context, string, TaskListRequest) (TaskPage, error)
	ListConversationTasks(context.Context, string, string, string, []string, ConversationTaskView, PageRequest) (TaskPage, error)
	ListTaskEvents(context.Context, string, string, int64, int32) ([]task.Event, error)
	GetTaskEventPage(context.Context, string, string, int64, int32) (TaskEventPage, error)
	ClaimNextTask(context.Context, string, time.Duration) (task.Task, bool, error)
	RenewTaskLease(context.Context, string, string, int64, time.Duration) (task.Task, error)
	AppendAssistantTextDelta(context.Context, AssistantTextDelta) (task.Event, error)
	CommitTaskProgress(context.Context, TaskProgress) (task.Event, error)
	CreateOperation(context.Context, task.Operation, string) (task.Operation, bool, error)
	GetOperation(context.Context, string, string, string) (task.Operation, error)
	// ListUnknownOperations returns operations that must be reconciled before retrying external work.
	ListUnknownOperations(context.Context, string, string) ([]task.Operation, error)
	UpdateOperation(context.Context, OperationUpdate) (task.Operation, error)
	UpdateBatchItem(context.Context, string, task.BatchItem, string, int64) (task.BatchItem, error)
	RetryFailedBatchItem(context.Context, IdempotencyRecord, string, string) error
	RespondToPendingRequest(context.Context, IdempotencyRecord, string, string, int64, task.PendingDecision) error
	AnswerClarificationRequest(context.Context, IdempotencyRecord, string, string, int64, string, task.FileDeliveryIntent, string) error
	// RecordValidatedArtifact persists batch artifact proof after the caller reads it back from a committed Workspace revision.
	RecordValidatedArtifact(context.Context, string, task.Artifact, string, int64) (task.Artifact, error)
}

// TaskActionRequester owns the atomic, idempotent user-facing state transition.
type TaskActionRequester interface {
	RequestTaskAction(context.Context, IdempotencyRecord, string, task.Action, task.Status, *TaskActionRevisionFence, string) (TaskActionResult, error)
}

// TaskActionStore also owns recovery leases while confirming runtime operation
// cancellation.
type TaskActionStore interface {
	TaskActionRequester
	ClaimNextTaskStop(context.Context, string, time.Duration) (TaskStopClaim, bool, error)
	RenewTaskStopClaim(context.Context, string, string, string, int64, time.Duration) error
	SettleTaskStopOperation(context.Context, string, string, string, int64, string, task.OperationStatus, string, bool) error
	CompleteTaskStop(context.Context, string, string, string, int64) (task.Task, error)
	ReleaseTaskStopClaim(context.Context, string, string, string, int64, time.Duration) error
}
