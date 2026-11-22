package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type HostStore interface {
	GetHost(context.Context, string) (host.Host, error)
	ListHosts(context.Context) ([]host.Host, error)
	ListHostsByOwner(context.Context, string) ([]host.Host, error)
	GetHostCreateReplay(context.Context, IdempotencyRecord) (host.Host, bool, error)
	CreateHost(context.Context, CreateHostParams) (host.Host, bool, error)
	EnsureDefaultHost(context.Context, CreateHostParams) (host.Host, bool, error)
	FailUnattemptedCreate(context.Context, string, string) (host.Host, bool, error)
	MarkCreateAttempt(context.Context, string) error
	SetSandboxID(context.Context, string, host.State, *string) error
	SetRuntimeInstanceID(context.Context, string, int64, int64, string, string) error
	TransitionHost(context.Context, string, host.State, host.State, string) (host.Host, error)
	BeginHostDelete(context.Context, string, host.State) (host.Host, error)
	BeginHostRelease(context.Context, string, bool) (host.Host, bool, error)
	CompleteHostRelease(context.Context, string, int64) (host.Host, error)
	BeginHostRestore(context.Context, RestoreHostParams) (host.Host, bool, error)
	SetHostStateReason(context.Context, string, host.State, int64, string) error
	SetHostExpiry(context.Context, string, host.State, time.Time) (host.Host, error)
	TouchHosts(context.Context, []string, time.Time) error
}

type HostRestoreRecoveryStore interface {
	RequeueUnknownHostRestore(context.Context, string, int64, string, time.Time) error
	AdoptFailedHostRestore(context.Context, string, int64, string, string) error
}

// HostReplicaLivenessStore records authenticated Host Runtime poll activity.
type HostReplicaLivenessStore interface {
	RecordHostReplicaHeartbeat(context.Context, string, int64) error
}

// HostRuntimeLivenessReader adds current Host Runtime status to API responses.
type HostRuntimeLivenessReader interface {
	PopulateHostRuntimeLiveness(context.Context, []host.Host) error
}

type OrphanHostVolumeCleanupStore interface {
	EnsureOrphanHostVolumeCleanup(context.Context, string, string, string, string, int64, bool) (bool, error)
	ListOrphanHostVolumeCleanups(context.Context, string) ([]string, error)
	CompleteOrphanHostVolumeCleanup(context.Context, string, string) error
}

type HostSharingStore interface {
	UpsertHostMember(context.Context, host.Member) (host.Member, error)
	ListHostMembers(context.Context, string, string) ([]host.Member, error)
	RevokeHostMember(context.Context, string, string, string) error
	ListSharedHosts(context.Context, string) ([]host.SharedHost, error)
	GetHostActorAccess(context.Context, string, string) (host.ActorAccess, error)
}

type WorkspaceStore interface {
	GetWorkspace(context.Context, string) (workspace.Workspace, error)
	ListWorkspacesByOwner(context.Context, string) ([]workspace.Workspace, error)
	CreateWorkspace(context.Context, workspace.Workspace, IdempotencyRecord) (workspace.Workspace, bool, error)
	TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error)
	ClaimWorkspaceForDelete(context.Context, string, int64) (workspace.Workspace, error)
	CompleteWorkspaceDelete(context.Context, string, int64) (workspace.Workspace, error)
	GetWorkspaceBindingsByHost(context.Context, string) ([]workspace.Binding, error)
	GetWorkspaceBindingsByWorkspace(context.Context, string) ([]workspace.Binding, error)
	GetWorkspaceBinding(context.Context, string, string) (workspace.Binding, error)
	CreateWorkspaceBinding(context.Context, workspace.Binding) (workspace.Binding, error)
	TransitionWorkspaceBinding(context.Context, string, string, int64, workspace.BindingState, workspace.BindingState, int64, int64) (workspace.Binding, error)
	AdvanceWorkspaceBindingAppliedRevision(context.Context, string, int64, string, int64, string, int64) (workspace.Binding, error)
	CompleteWorkspaceBindingUnbind(context.Context, string, string, int64, int64, int64) (workspace.Binding, error)
}

// AgentWorkspaceStore maps each account to its stable Agent Workspace. Unlike
// host bindings, this identity survives Host creation and deletion.
type AgentWorkspaceStore interface {
	GetAgentWorkspace(context.Context, string) (workspace.Workspace, bool, error)
	CreateAgentWorkspace(context.Context, workspace.Workspace) (workspace.Workspace, bool, error)
	MarkAgentWorkspaceDirectoryInitialized(context.Context, string, string) error
	AcquireAgentWorkspaceInitializationLock(context.Context, string) (func() error, error)
	IsAgentWorkspaceWorkspace(context.Context, string, string) (bool, error)
}

type HostAgentWorkspaceBindingStore interface {
	GetWorkspaceBindingsByHost(context.Context, string) ([]workspace.Binding, error)
	IsAgentWorkspaceWorkspace(context.Context, string, string) (bool, error)
}

func requireAgentWorkspaceHostBinding(ctx context.Context, source any, hostID, ownerID string) (workspace.Binding, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	store, ok := source.(HostAgentWorkspaceBindingStore)
	if !ok || store == nil {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Agent Workspace binding cannot be verified")
	}
	bindings, err := store.GetWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return workspace.Binding{}, err
	}
	var active workspace.Binding
	activeCount := 0
	for _, binding := range bindings {
		if binding.State == workspace.BindingDetached {
			continue
		}
		activeCount++
		active = binding
		if binding.HostID != hostID || binding.OwnerID != ownerID || binding.Workspace.OwnerID != ownerID ||
			!domain.ValidWorkspaceID(binding.Workspace.ID) {
			return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "Host workspace binding is inconsistent")
		}
	}
	if activeCount != 1 {
		return workspace.Binding{}, domain.NewError(domain.CodeWorkspaceConflict, "host must have exactly one active workspace binding")
	}
	if err := requireAgentWorkspaceMapping(ctx, store, ownerID, active.Workspace.ID); err != nil {
		return workspace.Binding{}, err
	}
	return active, nil
}

func requireAgentWorkspaceMapping(ctx context.Context, source any, ownerID, workspaceID string) error {
	mapper, ok := source.(interface {
		IsAgentWorkspaceWorkspace(context.Context, string, string) (bool, error)
	})
	if !ok {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "Agent Workspace identity cannot be verified")
	}
	isAgentWorkspace, err := mapper.IsAgentWorkspaceWorkspace(ctx, ownerID, workspaceID)
	if err != nil {
		return err
	}
	if !isAgentWorkspace {
		return domain.NewError(domain.CodeWorkspaceConflict, "computer is not bound to its owner's stable Agent Workspace")
	}
	return nil
}

// AgentWorkspaceDeleteStore permits only an explicit owner deletion to retire
// the current stable Agent Workspace mapping. Generic and Host cleanup paths
// remain unable to delete its contents.
type AgentWorkspaceDeleteStore interface {
	ClaimAgentWorkspaceForDelete(context.Context, string, string, int64) (workspace.Workspace, error)
	CompleteAgentWorkspaceDelete(context.Context, string, string, int64) (workspace.Workspace, error)
}

type workspaceBindingSyncStatusStore interface {
	SetWorkspaceBindingSyncStatus(context.Context, string, string, int64, workspace.BindingSyncState, []string) (workspace.Binding, error)
}

// HostReplicaWorkspaceStore provides generation-fenced mutations for the Host runtime; management paths keep using WorkspaceStore.
type HostReplicaWorkspaceStore interface {
	ActivateWorkspaceBindingForHost(context.Context, string, int64, string, int64, int64, int64) (workspace.Binding, error)
	SetWorkspaceBindingSyncStatusForHost(context.Context, string, int64, string, int64, workspace.BindingSyncState, []string) (workspace.Binding, error)
}

type WorkspaceSyncCommitState string

const (
	WorkspaceSyncCommitPending       WorkspaceSyncCommitState = "pending"
	WorkspaceSyncCommitSucceeded     WorkspaceSyncCommitState = "succeeded"
	WorkspaceSyncCommitQuotaRejected WorkspaceSyncCommitState = "quota_rejected"
)

type WorkspaceSyncCommitReceipt struct {
	State  WorkspaceSyncCommitState
	Result workspace.SyncSession
}

type WorkspaceCurrentMaterialization struct {
	WorkspaceID, OwnerID             string
	DesiredRevision, AppliedRevision int64
}

type TaskWorkspacePublishState string

const (
	TaskWorkspacePublishPending       TaskWorkspacePublishState = "pending"
	TaskWorkspacePublishSucceeded     TaskWorkspacePublishState = "succeeded"
	TaskWorkspacePublishConflict      TaskWorkspacePublishState = "conflict"
	TaskWorkspacePublishQuotaRejected TaskWorkspacePublishState = "quota_rejected"
	TaskWorkspacePublishSuperseded    TaskWorkspacePublishState = "superseded"
)

type TaskWorkspacePublishCommand struct {
	OwnerID, TaskID, WorkspaceID, HostID, LeaseOwner, OperationKey string
	ProjectID, InitiatorID, WorkspaceOwnerID                       string
	BindingVersion, LeaseVersion, InputRevision, BaseRevision      int64
	HostFence                                                      AgentHostFence
	ViewID, InputManifestSHA256                                    string
	DirectDirectorySource                                          bool
	RequestHash                                                    []byte
}

type TaskWorkspacePublishReceipt struct {
	State                 TaskWorkspacePublishState
	WorkspaceID           string
	BaseRevision          int64
	PublishedRevision     int64
	ManifestSHA256        string
	RequestHash           string
	DirectDirectorySource bool
	SupersedingMessages   []AgentConversationMessage
}

// WorkspaceSyncStore owns durable manifests, change cursors, and resumable
// transfer plans. Chunk bytes stay in the workspace-scoped block store.
type WorkspaceSyncStore interface {
	GetWorkspace(context.Context, string) (workspace.Workspace, error)
	GetWorkspaceBinding(context.Context, string, string) (workspace.Binding, error)
	CheckWorkspaceSyncHostReplicaAccess(context.Context, identity.Principal) error
	WithWorkspaceSyncHostSessionAccess(context.Context, identity.Principal, string, string, func() error) error
	WithWorkspaceSyncSessionAccess(context.Context, identity.Principal, string, string, func() error) error
	CheckWorkspaceSyncDeviceAccess(context.Context, identity.Principal, string) error
	WithWorkspaceSyncDeviceSessionAccess(context.Context, identity.Principal, string, string, func() error) error
	TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error)
	TryWorkspaceBlockStoreReadLock(context.Context, string) (LeaderLock, bool, error)
	GetWorkspaceManifestPage(context.Context, string, int64, string, string, int32) (workspace.ManifestPage, error)
	GetWorkspaceChangePage(context.Context, string, int64, string, int32) (workspace.ChangePage, error)
	CreateWorkspaceSyncSession(context.Context, workspace.SyncSession, []workspace.TransferBlock, []WorkspaceBlockReservation, int64, IdempotencyRecord) (workspace.SyncSession, bool, error)
	GetWorkspaceSyncSession(context.Context, string, string) (workspace.SyncSession, error)
	ListDeviceWorkspaceSyncProgress(context.Context, string, string) ([]workspace.DeviceSyncProgress, error)
	ListWorkspaceSyncTransferBlocks(context.Context, string, string) ([]workspace.TransferBlock, error)
	AckWorkspaceSyncBlock(context.Context, string, string, string, int64, int64) error
	FinalizeWorkspaceBlockUsage(context.Context, string, string, int64, int64) error
	AckWorkspaceSyncDownloadBlock(context.Context, string, string, string) error
	AckWorkspaceSyncDownloadAction(context.Context, string, string, string, workspace.ChangeAction, string, int64) error
	ReserveWorkspaceSyncCommit(context.Context, string, string, IdempotencyRecord) (WorkspaceSyncCommitReceipt, error)
	InspectWorkspaceSyncCommit(context.Context, string, string, IdempotencyRecord) (WorkspaceSyncCommitReceipt, error)
	CommitWorkspaceSyncSession(context.Context, string, string, IdempotencyRecord) (workspace.SyncSession, bool, error)
	ReserveTaskWorkspacePublish(context.Context, TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error)
	GetTaskWorkspacePublishReceipt(context.Context, TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error)
	FindTaskWorkspacePublishReceipt(context.Context, string, string, string, string) (TaskWorkspacePublishReceipt, error)
	CommitTaskWorkspacePublish(context.Context, TaskWorkspacePublishCommand, []workspace.File, []string, []workspace.Directory, string) (TaskWorkspacePublishReceipt, error)
	RejectTaskWorkspacePublishQuota(context.Context, TaskWorkspacePublishCommand) error
	ReserveTaskWorkspaceBlockUsage(context.Context, string, []WorkspaceBlockReservation, int64) error
	CancelWorkspaceSyncSession(context.Context, string, string, IdempotencyRecord) (workspace.SyncSession, bool, error)
	FailWorkspaceSyncSession(context.Context, string, string) error
}

// WorkspaceBlockGCStore coordinates block collection with workspace writes and
// provides the database protection set used while deleting unreferenced files.
type WorkspaceBlockGCStore interface {
	ListWorkspaceIDsForBlockGC(context.Context) ([]string, error)
	TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error)
	ExpireIdleWorkspaceSyncSessions(context.Context, string, time.Time) (int64, error)
	WithWorkspaceBlockGC(context.Context, string, func([]workspace.BlockReference) ([]WorkspaceBlockRemoval, error)) error
}

type WorkspaceBlockReservation struct {
	WorkspaceID   string
	Digest        string
	ReservedBytes int64
	SizeBytes     int64
}

type WorkspaceBlockReservationTarget struct {
	WorkspaceID string
	Digest      string
	SizeBytes   int64
}

type WorkspaceBlockAllocation struct {
	WorkspaceID    string
	Digest         string
	AllocatedBytes int64
}

type WorkspaceBlockRemoval struct {
	Digest         string
	AllocatedBytes int64
}

// WorkspaceBlockUsageStore reconciles physical block usage before serving.
type WorkspaceBlockUsageStore interface {
	TryWorkspaceStorageReconciliationLock(context.Context) (LeaderLock, bool, error)
	ListWorkspaceBlocksNeedingReservation(context.Context) ([]WorkspaceBlockReservationTarget, error)
	ReconcileWorkspaceBlockUsage(context.Context, []WorkspaceBlockAllocation, []WorkspaceBlockReservation, int64) error
}

type UserStore interface {
	GetUserByID(context.Context, string) (identity.User, error)
	GetUserByName(context.Context, string) (identity.User, error)
	ListUsersByStatus(context.Context, *identity.AccountStatus) ([]identity.User, error)
	CreateUser(context.Context, identity.User) (identity.User, error)
	UpdateUserWithAudit(context.Context, string, identity.AccountStatus, identity.User, audit.Entry, func(context.Context) error) (identity.User, error)
}

type invitedUserStore interface {
	CreateInvitedUser(context.Context, identity.User, string) (identity.User, error)
}

type TokenStore interface {
	GetTokenByID(context.Context, string) (identity.Token, error)
	ListTokensByOwner(context.Context, string) ([]identity.Token, error)
	CreateToken(context.Context, identity.Token) (identity.Token, error)
	RevokeToken(context.Context, string, string) error
	ConsumeTicket(context.Context, string) (Ticket, error)
}

// ClientLoginStore commits installation credentials and audit as one operation.
type ClientLoginStore interface {
	LoginClient(context.Context, identity.ClientInstallation, identity.User, string, int64, bool, []byte, []byte, string, string, func(string) (ClientIssuedTokens, error)) (identity.ClientInstallation, ClientIssuedTokens, error)
	IssueClientWebSession(context.Context, identity.Principal, identity.Token) error
	IssueClientScheduleGrant(context.Context, identity.Principal, identity.Token) error
	RevokeClientScheduleGrants(context.Context, identity.Principal) error
	ListClientInstallations(context.Context, string) ([]identity.ClientInstallation, error)
	GetClientInstallation(context.Context, string) (identity.ClientInstallation, error)
	RevokeClientInstallation(context.Context, string, string, string, audit.ActorKind, func(context.Context) error) error
}

type AuditStore interface {
	WriteAudit(context.Context, audit.Entry) error
	ListAuditLogs(context.Context, AuditFilter) ([]AuditLog, error)
}

type AuditFilter struct {
	Actor  string
	Action string
	Target string
	Since  *time.Time
	Limit  int32
}

type AuditLog struct {
	ID    int64
	At    time.Time
	Entry audit.Entry
}
