package gateway

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
)

func (g *gateway) cookieAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.authenticate(c) || !g.validRequestOrigin(c) {
			return
		}
		if principalFrom(c).Kind != identity.KindHostCookie {
			g.authFailure(c, "host_cookie")
			return
		}
		c.Next()
	}
}

func (g *gateway) authenticate(c *gin.Context) bool {
	hostID := c.GetString(hostIDKey)
	if hostID == "" {
		writeNotFound(c)
		return false
	}
	if c.GetHeader("Authorization") != "" {
		g.authFailure(c, "host_cookie")
		return false
	}
	cookie, err := c.Request.Cookie(hostCookieName)
	if err != nil || cookie.Value == "" {
		g.authFailure(c, "host_cookie")
		return false
	}
	now := time.Now()
	principal, cached := g.credentials.get(cookie.Value, now)
	var remaining time.Duration
	if !cached {
		var valid bool
		principal, remaining, valid = g.verifyCookie(cookie.Value, hostID, now)
		if !valid {
			g.authFailure(c, "host_cookie")
			return false
		}
	}
	ref, hasRef := c.Get(hostRefKey)
	resolved, ok := ref.(host.Ref)
	if !hasRef || !ok {
		resolved, err = g.deps.Runtime.Lookup(c.Request.Context(), hostID)
		if err != nil {
			g.authFailure(c, "host_cookie")
			return false
		}
		c.Set(hostRefKey, resolved)
	}
	if principal.Kind != identity.KindHostCookie || principal.HostScope != hostID {
		g.authFailure(c, "host_cookie")
		return false
	}
	if principal.ClientInstallationID != "" {
		if err := g.checkClientInstallation(c.Request.Context(), principal); err != nil {
			g.authFailure(c, "client")
			return false
		}
	}
	c.Set("gateway.member", false)
	c.Set("gateway.member_view", false)
	if principal.UserID == resolved.OwnerID {
		owner, err := g.deps.HostSharing.Authorize(c.Request.Context(), hostID, principal.UserID, host.PermissionDesktopView)
		if err != nil || !owner {
			g.authFailure(c, "host_access")
			return false
		}
	} else {
		permissions, allowed := memberGatewayPermissions(c.Request, parsedHostFrom(c))
		if !allowed {
			g.authFailure(c, "host_member")
			return false
		}
		var permission host.Permission
		for _, candidate := range permissions {
			owner, err := g.deps.HostSharing.Authorize(c.Request.Context(), hostID, principal.UserID, candidate)
			if err == nil && !owner {
				permission = candidate
				break
			}
		}
		if permission == "" {
			g.authFailure(c, "host_member")
			return false
		}
		c.Set("gateway.member", true)
		c.Set("gateway.member_permission", string(permission))
		c.Set("gateway.member_view", permission == host.PermissionDesktopView)
	}
	if !cached {
		g.credentials.put(cookie.Value, principal, remaining, now)
	}
	g.setPrincipal(c, principal)
	return true
}

func memberGatewayPermissions(request *http.Request, parsed ParsedHost) ([]host.Permission, bool) {
	if request == nil {
		return nil, false
	}
	requestPath := request.URL.Path
	cleanPath := path.Clean(requestPath)
	if requestPath == "" || strings.Contains(requestPath, "//") ||
		(cleanPath != requestPath && cleanPath+"/" != requestPath) {
		return nil, false
	}
	if parsed.PreviewPort != nil {
		return nil, false
	}
	if requestPath == "/vnc" || strings.HasPrefix(requestPath, "/vnc/") {
		if request.Method == http.MethodGet || request.Method == http.MethodHead {
			return []host.Permission{host.PermissionDesktopView}, true
		}
		return nil, false
	}
	if requestPath == "/ws" || requestPath == "/websockify" {
		if request.Method == http.MethodGet && websocketRequest(request) {
			return []host.Permission{host.PermissionDesktopControl, host.PermissionDesktopView}, true
		}
		return nil, false
	}
	switch requestPath {
	case "/__ch/terminal", "/__ch/terminal/":
		if request.Method == http.MethodGet && request.URL.RawQuery == "" {
			return []host.Permission{host.PermissionTerminalControl}, true
		}
	case "/__ch/api/terminal-session":
		if request.URL.RawQuery == "" && (request.Method == http.MethodPut || request.Method == http.MethodPost) {
			return []host.Permission{host.PermissionTerminalControl}, true
		}
	case "/terminal":
		if request.Method == http.MethodGet && validTerminalSessionQuery(request) {
			return []host.Permission{host.PermissionTerminalControl}, true
		}
	case "/v1/shell/ws":
		if request.Method == http.MethodGet && websocketRequest(request) && validTerminalSessionQuery(request) {
			return []host.Permission{host.PermissionTerminalControl}, true
		}
	}
	return nil, false
}

func websocketRequest(request *http.Request) bool {
	return strings.EqualFold(request.Header.Get("Upgrade"), "websocket") && hasHeaderToken(request.Header.Get("Connection"), "upgrade")
}

func validTerminalSessionQuery(request *http.Request) bool {
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(values) != 1 || len(values["session_id"]) != 1 {
		return false
	}
	return values.Get("session_id") != ""
}

func (g *gateway) memberAuthorizationCheck(c *gin.Context) func(context.Context) error {
	memberValue, _ := c.Get("gateway.member")
	member := memberValue == true
	permission := host.Permission(c.GetString("gateway.member_permission"))
	if !member {
		permission = host.PermissionDesktopView
	}
	if !permission.Valid() {
		return func(context.Context) error { return domain.NewError(domain.CodeNotFound, "host not found") }
	}
	hostID, principal := c.GetString(hostIDKey), principalFrom(c)
	actorID := principal.UserID
	return func(ctx context.Context) error {
		if err := g.checkClientInstallation(ctx, principal); err != nil {
			return err
		}
		owner, err := g.deps.HostSharing.Authorize(ctx, hostID, actorID, permission)
		if err != nil {
			return err
		}
		if owner == member {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		return nil
	}
}

func (g *gateway) checkClientInstallation(ctx context.Context, principal identity.Principal) error {
	if principal.ClientInstallationID == "" {
		return nil
	}
	checker, ok := g.deps.Auth.(interface {
		ClientInstallationActive(context.Context, string, string) (bool, error)
	})
	if !ok {
		return identity.ErrInvalidCredential
	}
	active, err := checker.ClientInstallationActive(ctx, principal.ClientInstallationID, principal.UserID)
	if err != nil || !active {
		return identity.ErrInvalidCredential
	}
	return nil
}

func (g *gateway) setPrincipal(c *gin.Context, principal identity.Principal) {
	c.Set(principalKey, principal)
	c.Set("auth_kind", string(principal.Kind))
	c.Set("gateway.auth_kind", string(principal.Kind))
}

func (g *gateway) authFailure(c *gin.Context, kind string) {
	c.Set("auth_kind", kind)
	g.authFailures.WithLabelValues("gateway", kind).Inc()
	g.deps.Logger.Warn("gateway authentication failed",
		zap.String("kind", kind), zap.String("client_ip", clientIP(c.Request)))
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func (g *gateway) rateLimit(c *gin.Context) bool {
	allowed, retry := g.rateLimiter.allow(clientIP(c.Request), time.Now())
	if allowed {
		return true
	}
	g.authFailures.WithLabelValues("gateway", "rate_limited").Inc()
	c.Set("auth_kind", "rate_limited")
	g.deps.Logger.Warn("gateway rate limit reached", zap.String("kind", "rate_limited"), zap.String("client_ip", clientIP(c.Request)))
	err := domain.NewError(domain.CodeRateLimited, "rate limited")
	err.RetryAfter = retry
	httpserver.WriteError(c, err)
	return false
}

func (g *gateway) requireRunning(c *gin.Context, ref host.Ref) bool {
	switch ref.State {
	case host.StateRunning:
		return true
	case host.StateCreating:
		if strings.Contains(c.GetHeader("Accept"), "text/html") {
			c.Header("Content-Type", "text/html; charset=utf-8")
			c.Header("Cache-Control", "no-store")
			c.String(http.StatusServiceUnavailable, "<!doctype html><meta charset=utf-8><meta http-equiv=refresh content=5><title>创建中</title><p>主机创建中，请稍候…</p>")
		} else {
			err := domain.NewError(domain.CodeHostNotReady, "host is not ready")
			err.RetryAfter = 5 * time.Second
			httpserver.WriteError(c, err)
		}
		return false
	default:
		writeNotFound(c)
		return false
	}
}

// requireStableControl rejects new upstream work while the durable control
// handoff is between modes. This is only an admission check; it does not drain
// or revoke an already upgraded connection.
func (g *gateway) requireStableControl(c *gin.Context, ref host.Ref) bool {
	switch ref.ControlState {
	case host.ControlAI, host.ControlUser:
		return true
	case host.ControlTakingOver, host.ControlReturning:
		err := domain.NewError(domain.CodeHostNotReady, "host control transition is in progress")
		err.RetryAfter = time.Second
		httpserver.WriteError(c, err)
	default:
		httpserver.WriteError(c, domain.NewError(domain.CodeInvalidState, "host control state is unavailable"))
	}
	return false
}

func (g *gateway) validRequestOrigin(c *gin.Context) bool {
	websocket := strings.EqualFold(c.GetHeader("Upgrade"), "websocket") && hasHeaderToken(c.GetHeader("Connection"), "upgrade")
	safe := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions
	if safe && !websocket {
		return true
	}
	parsed := parsedHostFrom(c)
	label := parsed.HostID + "." + strings.TrimPrefix(g.deps.Domains.HostSuffix, ".")
	if parsed.PreviewPort != nil {
		label = strconv.Itoa(*parsed.PreviewPort) + "-" + label
	}
	want := g.deps.Domains.Scheme + "://" + strings.ToLower(label)
	origin := c.GetHeader("Origin")
	if origin == "" || !strings.EqualFold(origin, want) {
		g.authFailure(c, "host_cookie")
		return false
	}
	if websocket {
		c.Set(wsKey, true)
	}
	return true
}

func hasHeaderToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

type ipWindow struct {
	started time.Time
	count   int
}

type ipLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	ips    map[string]ipWindow
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, ips: make(map[string]ipWindow)}
}

func (l *ipLimiter) allow(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ip == "" {
		ip = "unknown"
	}
	for key, entry := range l.ips {
		if now.Sub(entry.started) >= l.window {
			delete(l.ips, key)
		}
	}
	entry := l.ips[ip]
	if entry.started.IsZero() || now.Sub(entry.started) >= l.window {
		entry = ipWindow{started: now}
	}
	if entry.count >= l.limit {
		l.ips[ip] = entry
		return false, l.window - now.Sub(entry.started)
	}
	if len(l.ips) >= 10000 {
		var oldestIP string
		var oldestAt time.Time
		for key, candidate := range l.ips {
			if oldestIP == "" || candidate.started.Before(oldestAt) {
				oldestIP, oldestAt = key, candidate.started
			}
		}
		delete(l.ips, oldestIP)
	}
	entry.count++
	l.ips[ip] = entry
	return true, 0
}
