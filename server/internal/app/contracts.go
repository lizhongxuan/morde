package app

import (
	"context"
	"time"

	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type CreateHostCommand struct {
	Name           string
	WorkspaceIDs   []string
	IdempotencyKey string
}

type DeleteHostCommand struct {
	HostID string
}

type EnterInfo struct {
	URL       string
	ExpiresAt time.Time
}

type HostService interface {
	Create(context.Context, identity.Principal, CreateHostCommand) (host.Host, error)
	EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error)
	List(context.Context, identity.Principal) ([]host.Host, error)
	Get(context.Context, identity.Principal, string) (host.Host, error)
	BindWorkspace(context.Context, identity.Principal, string, string, *int64, int64, string) (workspace.Binding, error)
	UnbindWorkspace(context.Context, identity.Principal, string, string, int64, bool, string) (workspace.Binding, error)
	RetryWorkspaceSync(context.Context, identity.Principal, string, string, int64) (workspace.Binding, error)
	Delete(context.Context, identity.Principal, DeleteHostCommand) (host.Host, error)
	Release(context.Context, identity.Principal, string, bool) (host.Host, error)
	Restore(context.Context, identity.Principal, string) (host.Host, error)
	Renew(context.Context, identity.Principal, string) (host.Host, error)
	IssueTicket(context.Context, identity.Principal, string) (EnterInfo, error)
	IssuePreviewTicket(context.Context, identity.Principal, string, int) (EnterInfo, error)
}

type HostSharingService interface {
	Invite(context.Context, identity.Principal, string, string, []host.Permission) (host.Member, error)
	ListMembers(context.Context, identity.Principal, string) ([]host.Member, error)
	Revoke(context.Context, identity.Principal, string, string) error
	ListSharedHosts(context.Context, identity.Principal) ([]host.SharedHost, error)
	IssueObservationTicket(context.Context, identity.Principal, string) (EnterInfo, error)
	IssueTerminalTicket(context.Context, identity.Principal, string) (EnterInfo, error)
	Authorize(context.Context, string, string, host.Permission) (bool, error)
}

type HostPermissionAuthorizer interface {
	Authorize(context.Context, string, string, host.Permission) (bool, error)
}

type MCPRuntimeActionAuthorizer interface {
	AuthorizeMCPRuntimeAction(context.Context, string, string, string, string, []identity.Scope, identity.Scope) error
}

type HostTLSAuthorizer interface {
	AuthorizeTLS(context.Context, string) bool
}

type HostReplicaService interface {
	Authenticate(context.Context, string, int64, string) (identity.Principal, error)
	AuthenticateWorkspace(context.Context, string) (identity.Principal, error)
	ScopeWorkspace(context.Context, identity.Principal, string, int64) (identity.Principal, error)
	ListBindings(context.Context, identity.Principal) ([]workspace.Binding, error)
	CredentialForBinding(context.Context, identity.Principal, string, int64) (string, error)
	RecordAppliedRevision(context.Context, identity.Principal, string, int64, int64) (workspace.Binding, error)
	ActivateBinding(context.Context, identity.Principal, string, int64) (workspace.Binding, error)
	CompleteUnbinding(context.Context, identity.Principal, string, int64, int64) (workspace.Binding, error)
	ReportWorkspaceSyncStatus(context.Context, identity.Principal, workspace.BindingSyncState, []string) (workspace.Binding, error)
}

type RuntimeHandshaker interface {
	Handshake(context.Context, string) error
}

type HostRuntime interface {
	Lookup(context.Context, string) (host.Ref, error)
	Resolve(context.Context, string, int) (Upstream, error)
	InvalidateUpstream(string)
	MarkActive(string)
}

type Ticket struct {
	TokenID              string
	UserID               string
	ActorUserID          string
	HostID               string
	PreviewPort          int
	ClientInstallationID string
}

type Verifier interface {
	VerifySession(context.Context, string) (identity.Principal, error)
	VerifyAPIKey(context.Context, string) (identity.Principal, error)
	ConsumeTicket(context.Context, string) (Ticket, error)
}

type AuditWriter interface {
	Write(context.Context, audit.Entry) error
}
