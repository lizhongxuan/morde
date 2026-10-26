package app

import (
	"context"
	"time"

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
	FailUnattemptedCreate(context.Context, string, string) (host.Host, bool, error)
	MarkCreateAttempt(context.Context, string) error
	SetSandboxID(context.Context, string, host.State, *string) error
	SetRuntimeInstanceID(context.Context, string, int64, int64, string, string) error
	TransitionHost(context.Context, string, host.State, host.State, string) (host.Host, error)
	BeginHostRelease(context.Context, string, bool) (host.Host, bool, error)
	CompleteHostRelease(context.Context, string, int64) (host.Host, error)
	BeginHostRestore(context.Context, RestoreHostParams) (host.Host, bool, error)
	SetHostStateReason(context.Context, string, host.State, int64, string) error
	SetHostExpiry(context.Context, string, host.State, time.Time) (host.Host, error)
	TouchHosts(context.Context, []string, time.Time) error
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
	AdvanceWorkspaceBindingAppliedRevision(context.Context, string, string, int64, int64) (workspace.Binding, error)
	CompleteWorkspaceBindingUnbind(context.Context, string, string, int64, int64) (workspace.Binding, error)
}

type workspaceBindingSyncStatusStore interface {
	SetWorkspaceBindingSyncStatus(context.Context, string, string, int64, workspace.BindingSyncState, []string) (workspace.Binding, error)
}

// WorkspaceSyncStore owns durable manifests, change cursors, and resumable
// transfer plans. Chunk bytes stay in the workspace-scoped block store.
type WorkspaceSyncStore interface {
	GetWorkspace(context.Context, string) (workspace.Workspace, error)
	GetWorkspaceBinding(context.Context, string, string) (workspace.Binding, error)
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
	CommitWorkspaceSyncSession(context.Context, string, string, IdempotencyRecord) (workspace.SyncSession, bool, error)
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
	ListUsers(context.Context) ([]identity.User, error)
	CreateUser(context.Context, identity.User) (identity.User, error)
	UpdateUser(context.Context, string, identity.User) (identity.User, error)
}

type TokenStore interface {
	GetTokenByID(context.Context, string) (identity.Token, error)
	ListTokensByOwner(context.Context, string) ([]identity.Token, error)
	CreateToken(context.Context, identity.Token) (identity.Token, error)
	RevokeToken(context.Context, string, string) error
	ConsumeTicket(context.Context, string) (Ticket, error)
}

type ClientAuthorizationStore interface {
	CreateClientAuthorization(context.Context, identity.ClientAuthorization) error
	GetClientAuthorization(context.Context, string) (identity.ClientAuthorization, error)
	GetClientAuthorizationByCode(context.Context, []byte) (identity.ClientAuthorization, error)
	ApproveClientAuthorization(context.Context, []byte, string, []string, string, string, time.Time) (identity.ClientInstallation, error)
	DenyClientAuthorization(context.Context, []byte, string, string) error
	ClaimClientAuthorization(context.Context, string, []byte, identity.Token, *identity.Token) (identity.ClientInstallation, error)
	ListClientInstallations(context.Context, string) ([]identity.ClientInstallation, error)
	GetClientInstallation(context.Context, string) (identity.ClientInstallation, error)
	RevokeClientInstallation(context.Context, string, string, string) error
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
