package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/httpserver"
)

const (
	portalCookieName             = "ch_portal"
	principalKey                 = "api.principal"
	workspaceSyncJSONBodyLimit   = 16 << 20
	workspaceSyncBlockBodyLimit  = 4 << 20
	workspaceSyncActionBodyLimit = 32 << 10
	modelGatewayBodyLimit        = 8 << 20
	personalAttachmentJSONLimit  = 16 << 10
	personalAttachmentChunkLimit = 4 << 20
	personalHistoryImportLimit   = 8 << 20
)

type Deps struct {
	Hosts                        app.HostService
	HostSharing                  app.HostSharingService
	HostReplicas                 app.HostReplicaService
	HostUsage                    HostUsageReceiptService
	TLSAuthorizer                app.HostTLSAuthorizer
	Verifier                     app.Verifier
	Auth                         AuthService
	WorkspaceOps                 WorkspaceService
	WorkspaceSync                WorkspaceSyncService
	WorkspaceSyncDevices         WorkspaceSyncDeviceService
	WorkspaceDiff                WorkspaceDiffService
	WorkspaceCopies              WorkspaceCopyEndpointService
	HostCopies                   HostCopyEndpointService
	WorkspaceTransfers           WorkspaceOwnershipTransferService
	WorkspaceShares              WorkspaceShareEndpointService
	Sites                        SiteService
	Templates                    TemplateReader
	Tokens                       TokenService
	ClientLogin                  ClientLoginService
	Notifications                NotificationService
	Control                      ControlService
	ExecutionControl             ExecutionControlService
	Tasks                        TaskService
	TaskMedia                    TaskMediaService
	TaskFileFetch                TaskFileFetchEndpointService
	TaskObservationTickets       TaskWorkbenchObservationTicketService
	Conversations                ConversationService
	ProjectConversations         projectConversationRoutesService
	ProjectWorkItems             projectWorkItemRoutesService
	ProjectBudget                projectBudgetRoutesService
	ProjectMCPConnectors         projectMCPConnectorRoutesService
	ProjectBudgetExecutionReady  bool
	ConversationAutoRoutingReady bool
	RuntimeOperationLedger       app.RuntimeOperationLedgerService
	Projects                     projectRoutesService
	ProjectContent               projectContentRoutesService
	ModelConnections             ModelConnectionManager
	MCPConnectors                MCPConnectorManager
	ModelGateway                 ModelGateway
	ModelUsageAdmin              ModelUsageAdmin
	Hub                          app.HubConnectionService
	Devices                      deviceService
	PersonalData                 personalDataService
	PersonalSync                 personalSyncService
	Goals                        goalRoutesService
	Schedules                    scheduleRoutesService
	PersonalDataQuotaUser        personalDataQuotaUserService
	PersonalDataQuota            AdminPersonalDataQuotaService
	PersonalAttachments          *app.PersonalAttachmentService
	PersonalHistoryImport        *app.PersonalHistoryImportService
	Onboarding                   OnboardingManager
	Admin                        AdminService
	AdminConsole                 AdminConsoleService
	AdminQualityActivity         AdminQualityActivityService
	AdminControls                AdminControlsService
	AdminOperations              AdminOperationsService
	AdminModelBudgetDefaults     AdminModelBudgetDefaultService
	AdminModelDefaults           AdminModelDefaultService
	RecordConsents               RecordConsentService
	RecordAccess                 RecordAccessService
	RecordCaptureStatus          RecordCaptureStatusService
	AdminHostResources           AdminHostResourcesService
	AdminHostRecovery            AdminHostRecoveryService
	AdminWorkspaces              AdminWorkspaceService
	AdminAudit                   AdminAuditWriter
	Readiness                    ReadinessChecker
	MCP                          http.Handler
	MCPDisabled                  bool

	ProjectConversationAttachments projectConversationAttachmentService
}

type Options struct {
	Logger            *zap.Logger
	Metrics           prometheus.Registerer
	ServiceInstanceID string
	APIMaxBodyBytes   int64
	MCPMaxBodyBytes   int64
	CookieSecure      bool
	SessionTTL        time.Duration
	HostScheme        string
	PortalDomain      string
	HostSuffix        string
	PreviewPortMin    int
	PreviewPortDeny   []int
	PortalFiles       fs.FS
}

type api struct {
	deps         Deps
	options      Options
	authFailures *prometheus.CounterVec
}

func New(deps Deps, options Options) (*gin.Engine, error) {
	if deps.Hosts == nil || deps.TLSAuthorizer == nil || deps.Verifier == nil || deps.Auth == nil {
		return nil, errors.New("api host, verifier, and auth services are required")
	}
	if options.APIMaxBodyBytes <= 0 || options.MCPMaxBodyBytes <= 0 || options.SessionTTL <= 0 {
		return nil, errors.New("api body limits and credential lifetimes must be positive")
	}
	if options.HostScheme != "http" && options.HostScheme != "https" {
		return nil, errors.New("api host scheme must be http or https")
	}
	portalURL, portalErr := url.Parse("https://" + options.PortalDomain)
	if portalErr != nil || portalURL.Host != options.PortalDomain || portalURL.Hostname() == "" || portalURL.User != nil || portalURL.Path != "" || portalURL.RawQuery != "" || portalURL.Fragment != "" {
		return nil, errors.New("api portal domain must be a hostname")
	}
	domainURL, err := url.Parse(options.HostScheme + "://" + options.HostSuffix)
	if err != nil || domainURL.Host != options.HostSuffix || domainURL.Hostname() == "" || domainURL.User != nil || domainURL.Path != "" || domainURL.RawQuery != "" || domainURL.Fragment != "" {
		return nil, errors.New("api host suffix must be a hostname")
	}
	if options.PreviewPortMin < 1 || options.PreviewPortMin > 65535 {
		return nil, errors.New("api preview port minimum must be between 1 and 65535")
	}

	middleware, err := httpserver.Base(httpserver.Options{Server: "api", Logger: options.Logger, Metrics: options.Metrics})
	if err != nil {
		return nil, err
	}
	authFailures, err := registerAuthFailureCounter(options.Metrics)
	if err != nil {
		return nil, err
	}
	engine := gin.New()
	if err := engine.SetTrustedProxies([]string{"127.0.0.1/32", "::1/128"}); err != nil {
		return nil, fmt.Errorf("configure api trusted proxies: %w", err)
	}
	h := &api{deps: deps, options: options, authFailures: authFailures}
	engine.Use(middleware...)
	engine.Use(h.securityHeaders)
	engine.GET("/healthz", h.healthz)
	engine.GET("/readyz", h.readyz)
	engine.GET("/api/capabilities", h.capabilities)
	engine.GET("/api/mcp/oauth/callback", h.mcpConnectorOAuthCallback)
	engine.GET("/__ch/cert-authorize", h.authorizeTLS)
	h.registerHostReplicaRoutes(engine)
	if deps.TaskFileFetch != nil {
		h.registerTaskFileFetchTransferRoutes(engine, deps.TaskFileFetch)
	}
	if deps.AdminOperations != nil {
		engine.GET("/api/nudges", h.getPublicNudges)
	}
	if deps.MCPDisabled {
		engine.Any("/mcp", h.mcpDisabled)
	} else {
		engine.POST("/mcp", h.authenticateMCP, h.limitMCPBody, h.serveMCP)
		engine.GET("/mcp", h.authenticateMCP, h.serveMCP)
		engine.DELETE("/mcp", h.authenticateMCP, h.serveMCP)
	}
	group := engine.Group("/api", h.limitBody, h.authenticate, h.checkOrigin)
	h.registerAuth(group)
	h.registerOnboarding(group)
	h.registerHosts(group)
	h.registerHostCopies(group, deps.HostCopies)
	h.registerControl(group, deps.Control)
	h.registerExecutionControl(group, deps.ExecutionControl)
	h.registerConversations(group, deps.Conversations, deps.Tasks)
	RegisterProjectRoutes(group, deps.Projects)
	RegisterProjectHostUsageRoutes(group, deps.HostUsage)
	RegisterProjectConversationRoutes(group, deps.ProjectConversations)
	RegisterProjectConversationAttachmentRoutes(group, deps.ProjectConversationAttachments)
	RegisterProjectWorkItemRoutes(group, deps.ProjectWorkItems)
	RegisterProjectContentRoutes(group, deps.ProjectContent)
	RegisterProjectBudgetRoutes(group, deps.ProjectBudget)
	RegisterProjectMCPConnectorRoutes(group, deps.ProjectMCPConnectors)
	h.registerTasks(group, deps.Tasks)
	if deps.TaskFileFetch != nil {
		h.registerTaskFileFetchOwnerRoutes(group, deps.TaskFileFetch)
	}
	if deps.TaskMedia != nil {
		group.GET("/tasks/:id/media/:operation_id/preview", h.getTaskMediaPreview)
	}
	h.registerTaskWorkbenchObservationTickets(group, deps.TaskObservationTickets)
	h.registerResources(group)
	h.registerWorkspaceSync(group, deps.WorkspaceSync, deps.WorkspaceSyncDevices)
	h.registerWorkspaceSyncDeviceRoutes(group, deps.WorkspaceSyncDevices)
	h.registerWorkspaceDiff(group, deps.WorkspaceDiff)
	h.registerWorkspaceCopies(group, deps.WorkspaceCopies)
	h.registerWorkspaceOwnershipTransfers(group, deps.WorkspaceTransfers)
	h.registerWorkspaceShares(group, deps.WorkspaceShares)
	workspaceFileOperations, _ := deps.WorkspaceSync.(workspaceFileOperationService)
	h.registerWorkspaceFileOperations(group, workspaceFileOperations)
	h.registerSites(group)
	h.registerClients(group)
	registerDeviceRoutes(group, deps.Devices)
	registerPersonalDataRoutes(group, deps.PersonalData)
	registerPersonalSyncRoutes(group, deps.PersonalSync)
	if deps.Goals != nil {
		registerGoalRoutes(group, deps.Goals)
	}
	if deps.Schedules != nil {
		registerScheduleRoutes(group, deps.Schedules)
	}
	personalDataGroup := group.Group("/personal-data")
	RegisterPersonalDataQuotaRoute(personalDataGroup, deps.PersonalDataQuotaUser)
	RegisterPersonalAttachmentRoutes(personalDataGroup, deps.PersonalAttachments)
	RegisterPersonalOfficePreviewRoute(personalDataGroup)
	RegisterPersonalHistoryImportRoutes(personalDataGroup, deps.PersonalHistoryImport)
	h.registerNotifications(group, deps.Notifications)
	h.registerModelConnections(group)
	h.registerMCPConnectors(group)
	group.GET("/hub", hubConnectionHandler(deps.Hub, options.PortalDomain))
	h.registerModelGateway(group)
	h.registerAdmin(group)
	h.registerAdminQualityActivity(group)
	h.registerAdminControls(group)
	h.registerAdminOperations(group)
	h.registerAdminRecords(group)
	h.registerAdminPersonalDataQuota(group)
	h.registerAdminModelUsage(group)
	engine.NoRoute(h.noRoute)
	engine.NoMethod(func(c *gin.Context) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
	})
	return engine, nil
}

func (h *api) limitBody(c *gin.Context) {
	limit := h.options.APIMaxBodyBytes
	switch {
	case c.Request.Method == http.MethodPost && c.FullPath() == "/api/model/v1/chat/completions":
		limit = modelGatewayBodyLimit
	case c.Request.Method == http.MethodPost && strings.HasSuffix(c.FullPath(), "/workspaces/:id/sync-sessions"):
		limit = workspaceSyncJSONBodyLimit
	case c.Request.Method == http.MethodPost && c.FullPath() == "/api/personal-data/uploads":
		limit = personalAttachmentJSONLimit
	case c.Request.Method == http.MethodPost && c.FullPath() == "/api/personal-data/office-preview":
		limit = int64(app.PersonalOfficePreviewMaxBytes)
	case c.Request.Method == http.MethodPost && c.FullPath() == "/api/personal-data/history/import":
		limit = personalHistoryImportLimit
	case c.Request.Method == http.MethodPut && c.FullPath() == "/api/personal-data/uploads/:upload_id/chunks/:sequence":
		limit = personalAttachmentChunkLimit
	case c.Request.Method == http.MethodPut && c.FullPath() == "/api"+projectConversationAttachmentBase+"/chunks/:sequence":
		limit = personalAttachmentChunkLimit
	case c.Request.Method == http.MethodPut && c.FullPath() == "/api"+projectConversationAttachmentBase+"/derivatives":
		limit = app.ProjectConversationDerivativeMaxJSONBytes
	case c.Request.Method == http.MethodPut && (c.FullPath() == "/api/conversations/:id/message-uploads/:client_message_id/chunks/:sequence" ||
		c.FullPath() == "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id/chunks/:sequence"):
		limit = conversationMessageChunkJSONLimit
	case (c.Request.Method == http.MethodPut || c.Request.Method == http.MethodDelete) && c.FullPath() == "/api/personal-data/:type/:id":
		limit = personal.MaxMutationBodyBytes + 4096
	case (c.Request.Method == http.MethodPost || c.Request.Method == http.MethodPatch || c.Request.Method == http.MethodDelete) &&
		(c.FullPath() == "/api/goals" || c.FullPath() == "/api/goals/:id"):
		limit = personal.MaxMutationBodyBytes + 4096
	case c.Request.Method == http.MethodPost && (strings.HasSuffix(c.FullPath(), "/workspaces/:id/sync-device-challenges") ||
		strings.HasSuffix(c.FullPath(), "/workspaces/:id/sync-devices")):
		limit = workspaceSyncActionBodyLimit
	case c.Request.Method == http.MethodPut && strings.HasSuffix(c.FullPath(), "/workspaces/:id/sync-sessions/:session_id/blocks/:digest"):
		limit = workspaceSyncBlockBodyLimit
	case c.Request.Method == http.MethodPost && strings.HasSuffix(c.FullPath(), "/workspaces/:id/sync-sessions/:session_id/download-actions/ack"):
		limit = workspaceSyncActionBodyLimit
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	c.Next()
}

func (h *api) authenticate(c *gin.Context) {
	if c.FullPath() == "/api/auth/login" || c.FullPath() == "/api/auth/register" || c.FullPath() == "/api/auth/client-login" {
		c.Next()
		return
	}

	var cookieValue string
	foundCookie := false
	for _, cookie := range c.Request.Cookies() {
		if cookie.Name == portalCookieName {
			if foundCookie {
				h.authFailure(c, "session")
				return
			}
			foundCookie = true
			cookieValue = cookie.Value
		}
	}
	authorization := c.GetHeader("Authorization")
	if foundCookie && authorization != "" {
		h.authFailure(c, credentialFailureKind(c))
		return
	}
	var principal identity.Principal
	var err error
	switch {
	case foundCookie:
		kind, _, _, parseErr := identity.ParseCredential(cookieValue)
		if parseErr != nil || kind != identity.KindSession {
			h.authFailure(c, "session")
			return
		}
		principal, err = h.deps.Verifier.VerifySession(c.Request.Context(), cookieValue)
		principal.Kind = identity.KindSession
		c.Set("auth_kind", "session")
	case authorization != "":
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			h.authFailure(c, "api_key")
			return
		}
		if strings.HasPrefix(parts[1], "morde-replica-v1.") {
			if h.deps.HostReplicas == nil {
				h.authFailure(c, "host_replica")
				return
			}
			principal, err = h.deps.HostReplicas.AuthenticateWorkspace(c.Request.Context(), parts[1])
			c.Set("auth_kind", "host_replica")
		} else {
			kind, _, _, parseErr := identity.ParseCredential(parts[1])
			if parseErr != nil || (kind != identity.KindAPIKey && kind != identity.KindDevice && kind != identity.KindClient) {
				failureKind := string(kind)
				if !validCredentialMetricKind(failureKind) {
					failureKind = "api_key"
				}
				h.authFailure(c, failureKind)
				return
			}
			switch kind {
			case identity.KindClient:
				verifier, ok := h.deps.Verifier.(interface {
					VerifyClient(context.Context, string) (identity.Principal, error)
				})
				if !ok {
					h.authFailure(c, "client")
					return
				}
				principal, err = verifier.VerifyClient(c.Request.Context(), parts[1])
				principal.Kind = identity.KindClient
				principal.Role = identity.RoleUser
				c.Set("auth_kind", "client")
			case identity.KindDevice:
				verifier, ok := h.deps.Verifier.(interface {
					VerifyDevice(context.Context, string) (identity.Principal, error)
				})
				if !ok {
					h.authFailure(c, "device")
					return
				}
				principal, err = verifier.VerifyDevice(c.Request.Context(), parts[1])
				principal.Kind = identity.KindDevice
				c.Set("auth_kind", "device")
			default:
				principal, err = h.deps.Verifier.VerifyAPIKey(c.Request.Context(), parts[1])
				principal.Kind = identity.KindAPIKey
				c.Set("auth_kind", "api_key")
			}
		}
	default:
		h.authFailure(c, "session")
		return
	}
	if err != nil {
		h.authFailure(c, c.GetString("auth_kind"))
		return
	}
	if principal.Kind == identity.KindSession && principal.ClientInstallationID != "" {
		principal.Role = identity.RoleUser
	}
	if principal.Kind == identity.KindSession && principal.MustChangePassword && !passwordChangeRoute(c.Request.Method, c.FullPath()) {
		writeError(c, domain.NewError(domain.CodeForbidden, "password change is required before continuing"))
		c.Abort()
		return
	}
	if principal.Kind == identity.KindSession && principal.ClientInstallationID != "" && strings.HasPrefix(c.FullPath(), "/api/admin/") {
		writeError(c, domain.NewError(domain.CodeForbidden, "client Web session cannot access administration"))
		c.Abort()
		return
	}
	if principal.Kind == identity.KindDevice && !deviceCredentialRoute(c.Request.Method, c.FullPath()) {
		writeError(c, domain.NewError(domain.CodeForbidden, "device credential is limited to authorized workspace synchronization"))
		c.Abort()
		return
	}
	if principal.Kind == identity.KindClient && !clientCredentialRoute(c.Request.Method, c.FullPath()) {
		if isTaskFileFetchOwnerRoute(c.Request.Method, c.FullPath()) {
			writeError(c, domain.NewError(domain.CodeNotFound, "file fetch not found"))
			return
		}
		writeError(c, domain.NewError(domain.CodeForbidden, "client credential is limited to user actions"))
		c.Abort()
		return
	}
	if principal.Kind == identity.KindClient && len(principal.Scopes) != 0 && !clientScheduleGrantRoute(c.Request.Method, c.FullPath()) {
		writeError(c, domain.NewError(domain.CodeForbidden, "schedule grant is limited to schedule management"))
		c.Abort()
		return
	}
	if principal.Kind == identity.KindHostReplica && !hostReplicaCredentialRoute(c.Request.Method, c.FullPath()) {
		writeError(c, domain.NewError(domain.CodeForbidden, "Host replica credential is limited to workspace synchronization"))
		c.Abort()
		return
	}
	c.Set(principalKey, principal)
	c.Next()
}

func passwordChangeRoute(method, path string) bool {
	return (method == http.MethodGet && path == "/api/me") ||
		(method == http.MethodPost && (path == "/api/me/password" || path == "/api/auth/logout"))
}

func (h *api) checkOrigin(c *gin.Context) {
	if !isUnsafeMethod(c.Request.Method) {
		c.Next()
		return
	}
	origin := c.GetHeader("Origin")
	if origin != "" && !sameOriginHost(origin, c.Request.Host) {
		writeError(c, domain.NewError(domain.CodeForbidden, "request origin is not allowed"))
		c.Abort()
		return
	}
	if c.GetString("auth_kind") == "session" && origin == "" {
		writeError(c, domain.NewError(domain.CodeForbidden, "request origin is required"))
		c.Abort()
		return
	}
	if c.GetString("auth_kind") == "session" {
		if c.Request.ContentLength == 0 {
			c.Next()
			return
		}
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if c.Request.Method == http.MethodPost && c.FullPath() == "/api/personal-data/office-preview" {
			if err != nil || !isPersonalOfficePreviewMediaType(mediaType) {
				writeError(c, domain.NewError(domain.CodeForbidden, "content type is not allowed"))
				c.Abort()
				return
			}
			c.Next()
			return
		}
		expected := "application/json"
		if c.Request.Method == http.MethodPut {
			switch c.FullPath() {
			case "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest",
				"/api/personal-data/uploads/:upload_id/chunks/:sequence",
				"/api" + projectConversationAttachmentBase + "/chunks/:sequence":
				expected = "application/octet-stream"
			}
		}
		if err != nil || mediaType != expected {
			writeError(c, domain.NewError(domain.CodeForbidden, "content type is not allowed"))
			c.Abort()
			return
		}
	}
	c.Next()
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func sameOriginHost(origin, requestHost string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && strings.EqualFold(parsed.Host, requestHost)
}

func principalFrom(c *gin.Context) (identity.Principal, bool) {
	principal, ok := c.Get(principalKey)
	if !ok {
		return identity.Principal{}, false
	}
	value, ok := principal.(identity.Principal)
	return value, ok
}

func unauthenticated(c *gin.Context) {
	writeError(c, domain.NewError(domain.CodeUnauthenticated, "authentication required"))
	c.Abort()
}

func (h *api) authFailure(c *gin.Context, kind string) {
	if !validCredentialMetricKind(kind) {
		kind = "session"
	}
	if h.authFailures != nil {
		h.authFailures.WithLabelValues("api", kind).Inc()
	}
	c.Set("auth_kind", kind)
	unauthenticated(c)
}

func credentialFailureKind(c *gin.Context) string {
	if authorization := strings.TrimSpace(c.GetHeader("Authorization")); authorization != "" {
		parts := strings.Fields(authorization)
		if len(parts) == 2 {
			kind, _, _, err := identity.ParseCredential(parts[1])
			if err == nil && validCredentialMetricKind(string(kind)) {
				return string(kind)
			}
		}
		return "api_key"
	}
	return "session"
}

func validCredentialMetricKind(kind string) bool {
	switch kind {
	case "session", "api_key", "device", "client", "host_cookie", "host_replica", "ticket", "rate_limited":
		return true
	default:
		return false
	}
}

func hostReplicaCredentialRoute(method, path string) bool {
	if deviceCredentialRoute(method, path) && path != "/api/workspaces/:id/sync-progress" {
		return true
	}
	return (method == http.MethodGet && path == "/api/host-replicas/:host_id/generations/:generation/task-workspace-view-deletions") ||
		(method == http.MethodPost && path == "/api/host-replicas/:host_id/generations/:generation/tasks/:task_id/workspace-view-deletions/:request_id/applied")
}

func deviceCredentialRoute(method, path string) bool {
	switch {
	case path == "/api/device/workspaces" && method == http.MethodGet:
		return true
	case (path == "/api/workspaces/:id/manifest" || path == "/api/workspaces/:id/changes" ||
		path == "/api/workspaces/:id/sync-sessions/:session_id") && method == http.MethodGet:
		return true
	case (path == "/api/workspaces/:id/sync-sessions" ||
		path == "/api/workspaces/:id/sync-sessions/:session_id/commit" ||
		path == "/api/workspaces/:id/sync-sessions/:session_id/cancel") && method == http.MethodPost:
		return true
	case path == "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest" &&
		(method == http.MethodGet || method == http.MethodPut):
		return true
	case path == "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest/ack" && method == http.MethodPost:
		return true
	case path == "/api/workspaces/:id/sync-sessions/:session_id/download-actions/ack" && method == http.MethodPost:
		return true
	}
	return false
}

func clientCredentialRoute(method, path string) bool {
	switch method {
	case http.MethodGet:
		switch path {
		case "/api/me", "/api/me/record-captures", "/api/me/main-conversation", "/api/clients", "/api/devices", "/api/hosts", "/api/hosts/:id", "/api/hosts/:id/workspace-bindings",
			"/api/hosts/:id/control",
			"/api/shared-hosts", "/api/workspaces", "/api/workspaces/:id", "/api/conversations", "/api/conversations/:id",
			"/api/conversations/:id/messages", "/api/conversations/:id/tasks", "/api/conversations/:id/message-uploads/:client_message_id",
			"/api/projects", "/api/projects/:id", "/api/projects/:id/members", "/api/projects/:id/resources",
			"/api/projects/:id/budget", "/api/projects/:id/budget/ledger", "/api/projects/:id/work-items",
			"/api/projects/:id/work-items/:work_item_id", "/api/me/project-invitations",
			"/api/projects/:id/conversations", "/api/projects/:id/conversations/:conversation_id",
			"/api/projects/:id/conversations/:conversation_id/messages", "/api/projects/:id/conversations/:conversation_id/tasks",
			"/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/content",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments-by-id/:attachment_id",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments-by-id/:attachment_id/content",
			"/api/tasks", "/api/tasks/:id", "/api/tasks/:id/events",
			"/api/tasks/:id/workspace-view-deletions", "/api/hub", "/api/personal-data/:type/:id", "/api/personal-data/quota",
			"/api/personal-data/attachments/:attachment_id",
			"/api/goals", "/api/goals/:id",
			"/api/schedules", "/api/schedules/:id", "/api/schedules/:id/runs",
			"/api/sync/snapshot", "/api/sync/changes",
			"/api/notifications", "/api/available-models", "/api/model-connections", "/api/model-connections/:id":
			return true
		}
	case http.MethodPost:
		switch path {
		case "/api/auth/client-web-session", "/api/auth/client-logout", "/api/auth/client-schedule-grant":
			return true
		case "/api/conversations", "/api/conversations/:id/messages", "/api/tasks", "/api/tasks/:id/actions",
			"/api/conversations/:id/message-uploads/:client_message_id/commit",
			"/api/conversations/:id/messages/:message_id/retry-processing",
			"/api/projects/:id/conversations", "/api/projects/:id/conversations/:conversation_id/messages",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/retry-processing",
			"/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id/commit",
			"/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/commit",
			"/api/tasks/:id/workspace-view-deletions",
			"/api/tasks/:id/inputs", "/api/tasks/:id/approvals/:approval_id/responses",
			"/api/model-connections", "/api/model-connections/:id/check":
			return true
		case "/api/schedules", "/api/schedules/:id/actions":
			return true
		case "/api/goals":
			return true
		case "/api/workspaces/:id/sync-device-challenges", "/api/workspaces/:id/sync-devices":
			return true
		case "/api/model/v1/chat/completions":
			return true
		case "/api/notifications/:id/read", "/api/hosts/:id/enter", "/api/hosts/:id/observation-ticket",
			"/api/hosts/:id/control/actions", "/api/hosts/:id/control/lease/renew":
			return true
		case "/api/personal-data/uploads", "/api/personal-data/uploads/:upload_id/commit", "/api/personal-data/history/import",
			"/api/personal-data/office-preview":
			return true
		case "/api/me/main-conversation/ensure":
			return true
		}
	case http.MethodPut:
		return path == "/api/personal-data/:type/:id" || path == "/api/personal-data/uploads/:upload_id/chunks/:sequence" ||
			path == "/api/conversations/:id/message-uploads/:client_message_id" ||
			path == "/api/conversations/:id/message-uploads/:client_message_id/chunks/:sequence" ||
			path == "/api/conversations/:id/messages/:message_id/attachments/:client_attachment_id" ||
			path == "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id" ||
			path == "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id/chunks/:sequence" ||
			path == "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id" ||
			path == "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/derivatives" ||
			path == "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/chunks/:sequence" ||
			path == "/api/schedules/:id" || path == "/api/model-connections/:id" || path == "/api/me/main-conversation"
	case http.MethodPatch:
		return path == "/api/devices/:id" || path == "/api/goals/:id"
	case http.MethodDelete:
		return path == "/api/clients/:id" || path == "/api/devices/:id" || path == "/api/personal-data/:type/:id" ||
			path == "/api/goals/:id" || path == "/api/auth/client-schedule-grant" ||
			path == "/api/personal-data/uploads/:upload_id" || path == "/api/model-connections/:id"
	}
	return false
}

func clientScheduleGrantRoute(method, path string) bool {
	switch method {
	case http.MethodGet:
		return path == "/api/schedules" || path == "/api/schedules/:id" || path == "/api/schedules/:id/runs"
	case http.MethodPost:
		return path == "/api/schedules" || path == "/api/schedules/:id/actions"
	case http.MethodPut:
		return path == "/api/schedules/:id"
	default:
		return false
	}
}

func registerAuthFailureCounter(reg prometheus.Registerer) (*prometheus.CounterVec, error) {
	collector := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudhost_auth_failures_total",
		Help: "Authentication failures by server and credential kind.",
	}, []string{"server", "kind"})
	if err := reg.Register(collector); err == nil {
		return collector, nil
	} else {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, fmt.Errorf("register API authentication counter: %w", err)
		}
		existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, errors.New("cloudhost_auth_failures_total is already registered with an incompatible collector")
		}
		return existing, nil
	}
}

func writeError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	var appError *domain.Error
	if !errors.As(err, &appError) || appError == nil {
		httpserver.WriteError(c, err)
		return
	}
	// Normalize application messages and details so secrets cannot leak through errors.
	safe := domain.NewError(appError.Code, safeMessage(appError.Code))
	safe.RetryAfter = appError.RetryAfter
	if field, ok := appError.Details["field"].(string); ok && validFieldName(field) {
		safe.Details["field"] = field
	}
	if reason, ok := appError.Details["reason"].(string); ok && validSafeErrorReason(reason) {
		safe.Details["reason"] = reason
	}
	if appError.Code == domain.CodeConflict {
		switch appError.Details["reason"] {
		case "personal_object_revision_changed":
			if base, ok := appError.Details["base_revision"].(int64); ok && base >= 0 {
				safe.Details["base_revision"] = base
			}
			if current, ok := appError.Details["current_revision"].(int64); ok && current >= 0 {
				safe.Details["current_revision"] = current
			}
			if deleted, ok := appError.Details["deleted"].(bool); ok {
				safe.Details["deleted"] = deleted
			}
		case "host_release_unsettled_operation":
			if id, ok := appError.Details["task_id"].(string); ok && domain.ValidTaskID(id) {
				safe.Details["reason"] = "host_release_unsettled_operation"
				safe.Details["task_id"] = id
				safe.Message = "host has an unsettled task operation"
			}
		case "host_release_uncommitted_sync", "host_release_sync_conflict":
			if id, ok := appError.Details["workspace_id"].(string); ok && domain.ValidWorkspaceID(id) {
				safe.Details["reason"] = appError.Details["reason"]
				safe.Details["workspace_id"] = id
				safe.Message = "host Workspace changes are not settled"
			}
		}
	}
	httpserver.WriteError(c, safe)
}

func validSafeErrorReason(reason string) bool {
	switch reason {
	case "authentication_rejected", "access_rejected", "rate_limited", "quota_exhausted", "invalid_request", "unavailable", "timeout", "transport", "invalid_response":
		return true
	default:
		return false
	}
}

func safeMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeInvalidArgument:
		return "invalid request"
	case domain.CodeUnauthenticated:
		return "authentication required"
	case domain.CodeForbidden:
		return "forbidden"
	case domain.CodeNotFound:
		return "not found"
	case domain.CodeQuotaExceeded:
		return "quota exceeded"
	case domain.CodeCapacityExceeded:
		return "capacity exceeded"
	case domain.CodeWorkspaceBusy:
		return "workspace busy"
	case domain.CodeWorkspaceUnavailable:
		return "workspace service is unavailable"
	case domain.CodeInvalidState:
		return "invalid state"
	case domain.CodeConflict:
		return "conflict"
	case domain.CodeIdempotencyConflict:
		return "request conflicts with an earlier operation"
	case domain.CodeEventCursorExpired:
		return "event cursor expired; fetch a fresh snapshot"
	case domain.CodeUnsupported:
		return "operation unsupported"
	case domain.CodeRequestTooLarge:
		return "request body too large"
	case domain.CodeRateLimited:
		return "rate limit exceeded"
	case domain.CodeUpstreamError:
		return "upstream error"
	case domain.CodeHostNotReady:
		return "host is not ready"
	case domain.CodeUpstreamTimeout:
		return "upstream timeout"
	default:
		return "internal error"
	}
}

func validFieldName(field string) bool {
	if field == "" || len(field) > 64 {
		return false
	}
	for _, r := range field {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func safeID(c *gin.Context, field string, valid func(string) bool) (string, bool) {
	return safeIDParam(c, "id", field, valid)
}

func safeIDParam(c *gin.Context, param, field string, valid func(string) bool) (string, bool) {
	id := c.Param(param)
	if !valid(id) {
		writeError(c, domain.InvalidArgument(field, "invalid "+field))
		return "", false
	}
	return id, true
}
