package api

import (
	"context"
	"io"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
)

type WorkspaceService interface {
	CreateWorkspace(context.Context, identity.Principal, app.CreateWorkspaceCommand, string) (workspace.Workspace, error)
	ListWorkspaces(context.Context, identity.Principal) ([]workspace.Workspace, error)
	ListDeviceWorkspaces(context.Context, identity.Principal) ([]app.DeviceWorkspaceInfo, error)
	GetWorkspace(context.Context, identity.Principal, string) (workspace.Workspace, error)
	DeleteWorkspace(context.Context, identity.Principal, string) (workspace.Workspace, error)
}

type WorkspaceSyncService interface {
	GetManifest(context.Context, identity.Principal, string, int64, string, string, int32) (workspace.ManifestPage, error)
	GetChanges(context.Context, identity.Principal, string, string, time.Duration, int32) (workspace.ChangePage, error)
	CreateSyncSession(context.Context, identity.Principal, string, workspace.CreateSyncSession, string) (workspace.SyncSession, error)
	GetSyncSession(context.Context, identity.Principal, string, string) (workspace.SyncSession, error)
	ListDeviceSyncProgress(context.Context, identity.Principal, string) (workspace.DeviceSyncProgressSummary, error)
	CommitSyncSession(context.Context, identity.Principal, string, string, string) (workspace.SyncSession, error)
	CancelSyncSession(context.Context, identity.Principal, string, string, string) (workspace.SyncSession, error)
	PutSyncBlock(context.Context, identity.Principal, string, string, string, io.Reader) (int64, error)
	StreamSyncBlock(context.Context, identity.Principal, string, string, string, func(int64, []byte) error) error
	AckDownloadedBlock(context.Context, identity.Principal, string, string, string) error
	AckDownloadAction(context.Context, identity.Principal, string, string, string, workspace.ChangeAction, string, int64) error
}

type WorkspaceSyncDeviceService interface {
	CreateChallenge(context.Context, identity.Principal, string, string, []byte) (app.WorkspaceSyncDeviceChallengeResult, error)
	Register(context.Context, identity.Principal, string, string, string, []byte) (app.WorkspaceSyncDeviceRegistration, error)
	List(context.Context, identity.Principal, string) ([]workspace.SyncDeviceBinding, error)
	Revoke(context.Context, identity.Principal, string, string) error
}

type SiteService interface {
	Create(context.Context, identity.Principal, string, string, string) (sitedomain.Site, error)
	List(context.Context, identity.Principal, app.PageRequest) (app.SitePage, error)
	ListReleases(context.Context, identity.Principal, sitedomain.ID, app.PageRequest) (app.SiteReleasePage, error)
	CreateCandidate(context.Context, identity.Principal, sitedomain.ID, int64, int, string) (sitedomain.Release, error)
	Materialize(context.Context, identity.Principal, sitedomain.ID, sitedomain.ReleaseID) (sitedomain.Release, error)
	GetRelease(context.Context, identity.Principal, sitedomain.ID, sitedomain.ReleaseID) (sitedomain.Release, error)
}

type TemplateReader interface {
	ListTemplates(context.Context) ([]TemplateRecord, error)
}

type TemplateRecord struct {
	ID                   string
	Name                 string
	Description          string
	Isolation            string
	Resources            ResourceRecord
	WorkspaceSizeMiB     int
	TTLSeconds           int64
	IdleTimeoutSeconds   int64
	OnIdle               template.OnIdle
	KeepWorkspaceDefault bool
}

type ResourceRecord struct {
	CPUMilli      int
	MemMiB        int
	CPULimitMilli int
	MemLimitMiB   int
}

type TokenService interface {
	ListAPIKeys(context.Context, identity.Principal) ([]identity.Token, error)
	CreateAPIKey(context.Context, identity.Principal, string, time.Duration) (CreatedAPIKey, error)
	DeleteAPIKey(context.Context, identity.Principal, string) error
}

type ClientLoginService interface {
	Login(context.Context, string, string, string, string, string, string, string) (app.ClientLoginResult, error)
	WebSession(context.Context, identity.Principal) (app.ClientWebSessionResult, error)
	Logout(context.Context, identity.Principal, string) error
	LogoutWebSession(context.Context, identity.Principal, string) error
	List(context.Context, identity.Principal) ([]app.ClientInfo, error)
	Revoke(context.Context, identity.Principal, string, string) error
}

type CreatedAPIKey struct {
	ID        string
	Key       string
	ExpiresAt time.Time
}

type AdminService interface {
	ListUsers(context.Context, identity.Principal, *identity.AccountStatus) ([]AdminUserRecord, error)
	UpdateUser(context.Context, identity.Principal, string, UpdateAdminUserCommand) (AdminUserRecord, error)
	Capacity(context.Context, identity.Principal) (CapacityRecord, error)
	ListAudit(context.Context, identity.Principal, AuditQuery) ([]AuditRecord, error)
}

type AdminHostResourcesService interface {
	Get(context.Context, identity.Principal, string) (app.HostResourceSnapshot, error)
	Update(context.Context, identity.Principal, string, app.HostResources, int64) (app.HostResourceSnapshot, error)
}

type AdminUserRecord struct {
	ID                   string
	Name                 string
	Role                 identity.Role
	Status               identity.AccountStatus
	Quota                TotalsView
	Usage                TotalsView
	DefaultHostResources HostResourcesView
	CreatedAt            time.Time
}

type HostResourcesView struct {
	CPUMilli int `json:"cpu_milli"`
	MemMiB   int `json:"mem_mib"`
}

type UpdateAdminUserCommand struct {
	ExpectedStatus       identity.AccountStatus
	Role                 *identity.Role
	Status               *identity.AccountStatus
	Quota                *TotalsView
	DefaultHostResources *HostResourcesView
}

type CapacityRecord struct {
	Limit        TotalsView
	Allocated    TotalsView
	HostsByState map[host.State]int
}

type AuditQuery struct {
	Actor  string
	Action string
	Target string
	Since  *time.Time
	Limit  int
}

type AuditRecord struct {
	ID    int64
	At    time.Time
	Entry audit.Entry
}

type ReadinessChecker interface {
	CheckReady(context.Context) error
}

type NotificationService interface {
	List(context.Context, identity.Principal, bool, app.PageRequest) (app.NotificationPage, error)
	MarkRead(context.Context, identity.Principal, string) (notification.Notification, error)
}

type ModelConnectionManager interface {
	List(context.Context, identity.Principal) ([]app.ModelConnectionView, error)
	Get(context.Context, identity.Principal, string) (app.ModelConnectionView, error)
	Create(context.Context, identity.Principal, app.ModelConnectionCommand) (app.ModelConnectionView, error)
	Update(context.Context, identity.Principal, string, app.ModelConnectionCommand) (app.ModelConnectionView, error)
	Delete(context.Context, identity.Principal, string) error
	Check(context.Context, identity.Principal, string) (app.ModelCapabilities, error)
}

type MCPConnectorManager interface {
	List(context.Context, identity.Principal) ([]app.MCPConnectorView, error)
	Get(context.Context, identity.Principal, string) (app.MCPConnectorView, error)
	Create(context.Context, identity.Principal, app.CreateMCPConnectorCommand) (app.MCPConnectorView, error)
	Update(context.Context, identity.Principal, string, app.UpdateMCPConnectorCommand) (app.MCPConnectorView, error)
	RotateCredential(context.Context, identity.Principal, string, app.RotateMCPConnectorCredentialCommand) (app.MCPConnectorView, error)
	SetEnabled(context.Context, identity.Principal, string, bool, app.MCPConnectorRevisionCommand) (app.MCPConnectorView, error)
	Delete(context.Context, identity.Principal, string, app.MCPConnectorRevisionCommand) error
}

type MCPConnectorOAuthManager interface {
	BeginOAuth(context.Context, identity.Principal, string, int64, string, string) (app.MCPConnectorOAuthStart, error)
	CompleteOAuth(context.Context, string, string, string, string) error
}

type OnboardingManager interface {
	Preferences(context.Context, identity.Principal) (app.UserPreferences, error)
	SavePreferences(context.Context, identity.Principal, app.SaveUserPreferencesCommand) (app.UserPreferences, error)
	Status(context.Context, identity.Principal) (app.OnboardingSnapshot, error)
	EnsureDefaultHost(context.Context, identity.Principal, app.DefaultHostCommand) (app.DefaultHostResult, error)
}
