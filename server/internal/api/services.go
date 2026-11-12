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
	"morde/server/internal/domain/record"
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

// TaskMediaService exposes previews. GenerationReady is true only when the
// provider, Token settlement, operation recovery, and Workspace publication
// paths are all assembled.
type TaskMediaService interface {
	OpenPreview(context.Context, identity.Principal, string, string) (app.TaskMediaPreview, error)
	GenerationReady() bool
}

// TaskFileFetchEndpointService contains the ticket-only PC data plane and the
// owner Portal Session lifecycle endpoints for one Task-bound file fetch.
type TaskFileFetchEndpointService interface {
	GetTaskFileFetch(context.Context, identity.Principal, string, string) (app.TaskFileFetch, error)
	ListTaskFileFetches(context.Context, identity.Principal, string) ([]app.TaskFileFetch, error)
	ManifestTaskFileFetch(context.Context, string, string, string, app.TaskFileFetchManifestRequest) (app.TaskFileFetchManifestReceipt, error)
	PutTaskFileFetchChunk(context.Context, string, string, string, int32, string, io.Reader) (app.TaskFileFetchChunkReceipt, error)
	CommitTaskFileFetch(context.Context, string, string, string, app.TaskFileFetchCommitRequest) (app.TaskFileFetchCommitReceipt, error)
	AbortTaskFileFetch(context.Context, string, string, string) error
	CancelTaskFileFetch(context.Context, identity.Principal, string, string) (app.TaskFileFetch, error)
	OpenTaskFileFetchContent(context.Context, identity.Principal, string, string) (app.TaskFileFetchContent, error)
	DeleteTaskFileFetchContent(context.Context, identity.Principal, string, string) (app.TaskFileFetch, error)
}

type WorkspaceSyncDeviceService interface {
	CreateFolder(context.Context, identity.Principal, string, string, string) (workspace.SyncFolder, error)
	ListFolders(context.Context, identity.Principal, string) ([]workspace.SyncFolder, error)
	GetFolder(context.Context, identity.Principal, string, string) (workspace.SyncFolder, error)
	CreateChallenge(context.Context, identity.Principal, string, string, string, []byte) (app.WorkspaceSyncDeviceChallengeResult, error)
	Register(context.Context, identity.Principal, string, string, string, string, []byte) (app.WorkspaceSyncDeviceRegistration, error)
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
	Login(context.Context, string, string, string, string, string, string, string, app.ClientLoginAttempt) (app.ClientLoginResult, error)
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

type AdminUserDetailService interface {
	GetUser(context.Context, identity.Principal, string) (AdminUserRecord, error)
}

type AdminUserPageService interface {
	ListUserPage(context.Context, identity.Principal, *identity.AccountStatus, int, int) (app.AdminPage[AdminUserRecord], error)
}

type AdminConsoleService interface {
	AgentStates(context.Context, identity.Principal) (map[string]int64, error)
	Installations(context.Context, identity.Principal, string, int, int) (app.AdminPage[app.AdminInstallation], error)
	Authorizations(context.Context, identity.Principal, string, int, int) (app.AdminPage[app.AdminAuthorization], error)
	ModelCalls(context.Context, identity.Principal, string, int, int) (app.AdminPage[app.AdminModelCall], error)
	Usage(context.Context, identity.Principal, int) (app.AdminUsage, error)
	Stats(context.Context, identity.Principal) (app.AdminStats, error)
}

type AdminQualityActivityService interface {
	Quality(context.Context, identity.Principal, int) (app.AdminQuality, error)
	Activity(context.Context, identity.Principal, int) (app.AdminActivity, error)
	Places(context.Context, identity.Principal, int) (app.AdminPlaces, error)
}

type AdminPersonalDataQuotaService interface {
	GetAdminPersonalDataQuota(context.Context, identity.Principal, string) (app.PersonalDataQuota, error)
	SetAdminPersonalDataQuota(context.Context, identity.Principal, string, int64, int64, int64, string) (app.PersonalDataQuota, error)
}

type AdminControlsService interface {
	List(context.Context, identity.Principal) (app.AdminControls, error)
	Create(context.Context, identity.Principal, int64, string) (app.AdminControlRule, error)
	Update(context.Context, identity.Principal, app.UpdateAdminControlRuleCommand) (app.AdminControlRule, error)
	Delete(context.Context, identity.Principal, int64, int64) error
	Evaluate(context.Context, identity.Principal) (app.AdminControlEvaluation, error)
	NotifyTest(context.Context, identity.Principal) (app.AdminControlNotifyTest, error)
}

type AdminOperationsService interface {
	Settings(context.Context, identity.Principal) (app.AdminSettings, error)
	InviteCode(context.Context, identity.Principal) (string, error)
	ListInvites(context.Context, identity.Principal, app.AdminInviteFilter) (app.AdminInvitePage, error)
	ListSites(context.Context, identity.Principal, int, int) (app.AdminSitePage, error)
	NudgePolicy(context.Context, identity.Principal) (app.AdminNudgePolicy, error)
	PublicNudgePolicy(context.Context) (app.AdminNudgePolicy, error)
	UpdateNudgePolicy(context.Context, identity.Principal, app.UpdateAdminNudgeCommand) (app.AdminNudgePolicy, error)
}

type AdminModelBudgetDefaultService interface {
	Get(context.Context, identity.Principal) (app.ModelBudgetDefault, error)
	Update(context.Context, identity.Principal, app.UpdateModelBudgetDefaultCommand, string) (app.ModelBudgetDefault, error)
}

type AdminModelDefaultService interface {
	Get(context.Context, identity.Principal) (app.AdminModelDefaultView, error)
	Update(context.Context, identity.Principal, int64, string, string) (app.AdminModelDefaultView, error)
	Available(context.Context) (app.AdminModelDefaultAvailability, error)
}

type RecordConsentService interface {
	List(context.Context, identity.Principal) ([]record.Consent, error)
	Set(context.Context, identity.Principal, record.Purpose, bool) (record.Consent, error)
	Quota(context.Context, identity.Principal, string) (record.Quota, error)
	SetQuota(context.Context, identity.Principal, string, int64, int32, int64) (record.Quota, error)
}

type RecordCaptureStatusService interface {
	Statuses(context.Context, identity.Principal, record.CaptureStatusQuery) (record.CaptureStatusPage, error)
}

type RecordAccessService interface {
	SetGrant(context.Context, identity.Principal, string, string, record.AdminCapability, bool) (record.AdminGrant, error)
	AdminSettings(context.Context, identity.Principal, string) (app.RecordAdminSettings, error)
	AccessSettings(context.Context, identity.Principal, string) (app.RecordAdminAccess, error)
	List(context.Context, identity.Principal, string, record.Query) (record.Page, error)
	Read(context.Context, identity.Principal, string, string) (record.Content, error)
	Export(context.Context, identity.Principal, string, string) (record.Content, error)
	ListAttachments(context.Context, identity.Principal, string, string) ([]record.AttachmentMetadata, error)
	ReadAttachment(context.Context, identity.Principal, string, string, string) (record.AttachmentContent, error)
	Delete(context.Context, identity.Principal, string, string) (record.DeleteTombstone, error)
}

type HubPresenceReader interface {
	OnlineInstallationCount() int
	InstallationOnline(string, string) bool
}

type AdminAuditWriter interface {
	Write(context.Context, audit.Entry) error
}

type AdminHostResourcesService interface {
	Get(context.Context, identity.Principal, string) (app.HostResourceSnapshot, error)
	Update(context.Context, identity.Principal, string, app.HostResources, int64) (app.HostResourceSnapshot, error)
}

type AdminHostRecoveryService interface {
	RecoverUnknownRestore(context.Context, identity.Principal, string, int64, bool) (app.HostRecoveryResult, error)
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
