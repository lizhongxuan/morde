package app

import (
	"context"
	"time"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const MaxAssistantMessageBytes = 32 << 10

type PageRequest struct {
	Limit  int32
	Cursor string
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
	Items      []task.Task
	NextCursor string
}

type TaskListRequest struct {
	HostIDs []string
	Status  task.Status
	Page    PageRequest
}

type TaskDetails struct {
	Task                  task.Task
	ScheduleObservation   *schedule.ObservationSpec
	Steps                 []task.Step
	BatchItems            []task.BatchItem
	Artifacts             []task.Artifact
	PendingRequest        *task.PendingRequest
	ClarificationRequests []task.ClarificationRequest
	StopRequest           *task.StopRequest
	StopOperationIDs      []string
}

type TaskActionResult struct {
	Task                  task.Task
	Action                task.Action
	UnsettledOperationIDs []string
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
	Conversation    *conversation.Conversation
	Task            task.Task
	Message         conversation.Message
	InitialEvent    task.Event
	BatchItems      []task.BatchItem
	MCPConnectorIDs []string
	Idempotency     IdempotencyRecord
}

type TaskSubmissionResult struct {
	Conversation conversation.Conversation
	Message      conversation.Message
	Task         task.Task
	Replayed     bool
}

type TaskProgress struct {
	TaskID                 string
	LeaseOwner             string
	LeaseVersion           int64
	Status                 task.Status
	CurrentStep            string
	Result                 string
	WaitingReason          string
	Usage                  task.Usage
	ObservationFingerprint string
	Steps                  []task.Step
	Artifacts              []task.Artifact
	Event                  task.Event
	PendingRequest         *task.PendingRequest
	ClarificationRequest   *task.ClarificationRequest
	AssistantMessage       *conversation.Message
	CloudDSHSkillRead      *CloudDSHSkillReadCommit
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

// TaskStore owns atomic task submission, per-task event sequencing and worker leases.
type TaskStore interface {
	CreateConversation(context.Context, conversation.Conversation, IdempotencyRecord) (conversation.Conversation, bool, error)
	GetConversation(context.Context, string, string) (conversation.Conversation, error)
	ListConversations(context.Context, string, []string, PageRequest) (ConversationPage, error)
	ListMessages(context.Context, string, string, PageRequest) (MessagePage, error)
	SubmitTask(context.Context, TaskSubmission) (TaskSubmissionResult, error)
	AppendMessage(context.Context, TaskSubmission) (TaskSubmissionResult, error)
	GetTask(context.Context, string, string) (TaskDetails, error)
	ListTasks(context.Context, string, TaskListRequest) (TaskPage, error)
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
	AnswerClarificationRequest(context.Context, IdempotencyRecord, string, string, int64, string) error
	// RecordValidatedArtifact persists batch artifact proof after the caller reads it back from a committed Workspace revision.
	RecordValidatedArtifact(context.Context, string, task.Artifact, string, int64) (task.Artifact, error)
}

// TaskActionRequester owns the atomic, idempotent user-facing state transition.
type TaskActionRequester interface {
	RequestTaskAction(context.Context, IdempotencyRecord, string, task.Action, task.Status, string) (TaskActionResult, error)
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
