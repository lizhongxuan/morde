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
	"morde/server/internal/httpserver"
)

const (
	portalCookieName             = "ch_portal"
	principalKey                 = "api.principal"
	workspaceSyncJSONBodyLimit   = 16 << 20
	workspaceSyncBlockBodyLimit  = 4 << 20
	workspaceSyncActionBodyLimit = 32 << 10
	modelGatewayBodyLimit        = 8 << 20
)

type Deps struct {
	Hosts                app.HostService
	HostSharing          app.HostSharingService
	HostReplicas         app.HostReplicaService
	TLSAuthorizer        app.HostTLSAuthorizer
	Verifier             app.Verifier
	Auth                 AuthService
	WorkspaceOps         WorkspaceService
	WorkspaceSync        WorkspaceSyncService
	WorkspaceSyncDevices WorkspaceSyncDeviceService
	Sites                SiteService
	Templates            TemplateReader
	Tokens               TokenService
	ClientLogin          ClientLoginService
	Notifications        NotificationService
	Control              ControlService
	ExecutionControl     ExecutionControlService
	Tasks                TaskService
	Conversations        ConversationService
	InstantChat          InstantChatService
	ModelConnections     ModelConnectionManager
	MCPConnectors        MCPConnectorManager
	ModelGateway         ModelGateway
	ModelUsageAdmin      ModelUsageAdmin
	Onboarding           OnboardingManager
	Admin                AdminService
	AdminHostResources   AdminHostResourcesService
	AdminWorkspaces      AdminWorkspaceService
	Readiness            ReadinessChecker
	MCP                  http.Handler
	MCPDisabled          bool
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
	h.registerControl(group, deps.Control)
	h.registerExecutionControl(group, deps.ExecutionControl)
	h.registerConversations(group, deps.Conversations, deps.Tasks)
	h.registerTasks(group, deps.Tasks)
	h.registerResources(group)
	h.registerWorkspaceSync(group, deps.WorkspaceSync)
	h.registerWorkspaceSyncDeviceRoutes(group, deps.WorkspaceSyncDevices)
	h.registerSites(group)
	h.registerClients(group)
	h.registerInstantChat(group)
	h.registerNotifications(group, deps.Notifications)
	h.registerModelConnections(group)
	h.registerMCPConnectors(group)
	h.registerModelGateway(group)
	h.registerAdmin(group)
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
		writeError(c, domain.NewError(domain.CodeForbidden, "client credential is limited to user actions"))
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
		expected := "application/json"
		if c.Request.Method == http.MethodPut && c.FullPath() == "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest" {
			expected = "application/octet-stream"
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
	return deviceCredentialRoute(method, path) && path != "/api/workspaces/:id/sync-progress"
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
		case "/api/me", "/api/clients", "/api/hosts", "/api/hosts/:id", "/api/hosts/:id/workspace-bindings",
			"/api/shared-hosts", "/api/workspaces", "/api/workspaces/:id", "/api/conversations", "/api/conversations/:id",
			"/api/conversations/:id/messages", "/api/tasks", "/api/tasks/:id", "/api/tasks/:id/events",
			"/api/notifications", "/api/available-models":
			return true
		}
	case http.MethodPost:
		switch path {
		case "/api/auth/client-web-session", "/api/auth/client-logout":
			return true
		case "/api/conversations", "/api/conversations/:id/messages", "/api/tasks", "/api/tasks/:id/actions",
			"/api/tasks/:id/inputs", "/api/tasks/:id/approvals/:approval_id/responses":
			return true
		case "/api/workspaces/:id/sync-device-challenges", "/api/workspaces/:id/sync-devices":
			return true
		case "/api/model/v1/chat/completions":
			return true
		case "/api/notifications/:id/read", "/api/hosts/:id/enter", "/api/hosts/:id/observation-ticket":
			return true
		}
	case http.MethodDelete:
		return path == "/api/clients/:id"
	}
	return false
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
