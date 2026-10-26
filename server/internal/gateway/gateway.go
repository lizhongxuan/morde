package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
)

const (
	hostIDKey                      = "gateway.host_id"
	parsedHostKey                  = "gateway.parsed_host"
	hostRefKey                     = "gateway.host_ref"
	principalKey                   = "gateway.principal"
	previewKey                     = "gateway.preview_port"
	wsKey                          = "gateway.ws"
	hostActivityLeaseRenewInterval = 15 * time.Second
)

type Deps struct {
	Config           config.Gateway
	Domains          config.Domains
	Security         config.Security
	Hosts            app.HostService
	HostSharing      app.HostSharingService
	Control          ControlService
	Runtime          app.HostRuntime
	ActivityLeases   app.HostActivityLeaseStore
	InputPermits     app.HostInputPermitStore
	TerminalSessions app.TerminalSessionStore
	TerminalAIO      terminalSessionAIO
	Auth             app.Verifier
	Audit            app.AuditWriter
	Logger           *zap.Logger
	Metrics          prometheus.Registerer
}

type gateway struct {
	deps                             Deps
	engine                           *gin.Engine
	cookieKey                        []byte
	credentials                      *credentialCache
	transport                        *http.Transport
	wsGauge                          prometheus.Gauge
	upstreamErrs                     *prometheus.CounterVec
	authFailures                     *prometheus.CounterVec
	rateLimiter                      *ipLimiter
	wsMu                             sync.Mutex
	wsBodies                         map[*trackedBody]struct{}
	activityMu                       sync.Mutex
	activityAt                       map[string]time.Time
	activityRenewInterval            time.Duration
	memberAuthorizationCheckInterval time.Duration
}

type gatewayHandler struct {
	*gateway
}

func (h *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.engine.ServeHTTP(w, r)
}

// CloseConnections closes active upgraded connections during HTTP server shutdown.
func (h *gatewayHandler) CloseConnections() {
	h.closeConnections()
}

func New(d Deps) (http.Handler, error) {
	if d.Logger == nil || d.Metrics == nil || d.Hosts == nil || d.HostSharing == nil || d.Control == nil || d.Runtime == nil || d.ActivityLeases == nil || d.InputPermits == nil || d.TerminalSessions == nil || d.TerminalAIO == nil || d.Auth == nil {
		return nil, fmt.Errorf("gateway dependencies are required")
	}
	if d.Domains.Scheme != "http" && d.Domains.Scheme != "https" {
		return nil, fmt.Errorf("gateway domains.scheme must be http or https")
	}
	if d.Domains.HostSuffix == "" || d.Config.MaxBodyBytes <= 0 || d.Config.PreviewPortMin < 1 || d.Config.PreviewPortMin > 65535 || d.Security.HostCookieTTL <= 0 || d.Config.CredentialCacheTTL <= 0 {
		return nil, fmt.Errorf("gateway configuration is invalid")
	}
	cookieKey, err := os.ReadFile(d.Security.CookieKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read host cookie key: %w", err)
	}
	if len(cookieKey) != 32 {
		return nil, fmt.Errorf("host cookie key must contain exactly 32 bytes")
	}
	base, err := httpserver.Base(httpserver.Options{Server: "gateway", Logger: d.Logger, Metrics: d.Metrics})
	if err != nil {
		return nil, fmt.Errorf("configure gateway HTTP middleware: %w", err)
	}
	g := &gateway{
		deps:        d,
		cookieKey:   cookieKey,
		credentials: newCredentialCache(d.Config.CredentialCacheTTL),
		transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 0,
			ExpectContinueTimeout: time.Second,
		},
		wsBodies:                         make(map[*trackedBody]struct{}),
		activityAt:                       make(map[string]time.Time),
		activityRenewInterval:            hostActivityLeaseRenewInterval,
		memberAuthorizationCheckInterval: hostMemberAuthorizationCheckInterval,
		rateLimiter:                      newIPLimiter(30, time.Minute),
	}
	if err := g.registerMetrics(); err != nil {
		return nil, err
	}
	g.engine = gin.New()
	g.engine.Use(base...)
	g.engine.Use(g.parseRequestHost())
	g.engine.GET("/__ch/auth", g.consumeTicket)
	g.engine.POST("/__ch/logout", g.cookieAuthMiddleware(), g.logout)
	g.engine.GET("/__ch/wb/*filepath", g.cookieAuthMiddleware(), g.workbench)
	g.engine.GET("/__ch/terminal", g.cookieAuthMiddleware(), g.terminalEntry)
	g.engine.GET("/__ch/terminal/", g.cookieAuthMiddleware(), g.terminalEntry)
	g.engine.GET("/__ch/api/host", g.cookieAuthMiddleware(), g.getHost)
	g.engine.POST("/__ch/api/renew", g.cookieAuthMiddleware(), g.renewHost)
	g.engine.GET("/__ch/api/control", g.cookieAuthMiddleware(), g.getControl)
	g.engine.GET("/__ch/api/applications", g.cookieAuthMiddleware(), g.getApplications)
	g.engine.POST("/__ch/api/applications/:app_id/install", g.cookieAuthMiddleware(), g.installApplication)
	g.engine.POST("/__ch/api/applications/:app_id/launch", g.cookieAuthMiddleware(), g.launchApplication)
	g.engine.GET("/__ch/api/applications/:app_id/launches/current", g.cookieAuthMiddleware(), g.getCurrentApplicationLaunch)
	g.engine.GET("/__ch/api/applications/:app_id/launches/:launch_id", g.cookieAuthMiddleware(), g.getApplicationLaunch)
	g.engine.POST("/__ch/api/applications/:app_id/launches/:launch_id/stop", g.cookieAuthMiddleware(), g.stopApplicationLaunch)
	g.engine.POST("/__ch/api/control/actions", g.cookieAuthMiddleware(), g.actOnControl)
	g.engine.PUT("/__ch/api/terminal-session", g.cookieAuthMiddleware(), g.ensureTerminalSession)
	g.engine.POST("/__ch/api/terminal-session", g.cookieAuthMiddleware(), g.createTerminalSession)
	g.engine.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/" {
			g.root(c)
			return
		}
		g.proxy(c)
	})
	return &gatewayHandler{gateway: g}, nil
}

func (g *gateway) parseRequestHost() gin.HandlerFunc {
	return func(c *gin.Context) {
		parsed, err := parseHost(c.Request.Host, g.deps.Domains.HostSuffix)
		if err != nil {
			writeNotFound(c)
			return
		}
		c.Set(parsedHostKey, parsed)
		c.Set(hostIDKey, parsed.HostID)
		if parsed.PreviewPort != nil {
			port := *parsed.PreviewPort
			if !g.previewPortAllowed(port) {
				writeNotFound(c)
				return
			}
			c.Set(previewKey, port)
			httpserver.SetRoute(c, "preview")
			if strings.HasPrefix(c.Request.URL.Path, "/__ch/") && c.Request.URL.Path != "/__ch/auth" {
				writeNotFound(c)
				return
			}
		}
		if c.Request.URL.Path == "/__ch/auth" {
			c.Next()
			return
		}
		if parsed.PreviewPort == nil {
			c.Next()
			return
		}
		ref, err := g.deps.Runtime.Lookup(c.Request.Context(), parsed.HostID)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				writeNotFound(c)
				return
			}
			httpserver.WriteError(c, err)
			return
		}
		c.Set(hostRefKey, ref)
		c.Next()
	}
}

func (g *gateway) registerMetrics() error {
	var err error
	g.wsGauge, err = registerGauge(g.deps.Metrics, prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cloudhost_gateway_ws_connections",
		Help: "Current WebSocket connections proxied by the gateway.",
	}))
	if err != nil {
		return fmt.Errorf("register gateway websocket gauge: %w", err)
	}
	g.upstreamErrs, err = registerCounterVec(g.deps.Metrics, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudhost_gateway_upstream_errors_total",
		Help: "Upstream errors observed by the gateway.",
	}, []string{"kind"}))
	if err != nil {
		return fmt.Errorf("register gateway upstream counter: %w", err)
	}
	g.authFailures, err = registerCounterVec(g.deps.Metrics, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudhost_auth_failures_total",
		Help: "Authentication failures by server and credential kind.",
	}, []string{"server", "kind"}))
	if err != nil {
		return fmt.Errorf("register gateway authentication counter: %w", err)
	}
	return nil
}

func registerGauge(reg prometheus.Registerer, c prometheus.Gauge) (prometheus.Gauge, error) {
	if err := reg.Register(c); err == nil {
		return c, nil
	} else {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		existing, ok := already.ExistingCollector.(prometheus.Gauge)
		if !ok {
			return nil, fmt.Errorf("gateway websocket gauge already has an incompatible collector")
		}
		return existing, nil
	}
}

func registerCounterVec(reg prometheus.Registerer, c *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := reg.Register(c); err == nil {
		return c, nil
	} else {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, fmt.Errorf("gateway counter vector already has an incompatible collector")
		}
		return existing, nil
	}
}

func (g *gateway) closeConnections() {
	g.wsMu.Lock()
	bodies := make([]*trackedBody, 0, len(g.wsBodies))
	for body := range g.wsBodies {
		bodies = append(bodies, body)
	}
	g.wsMu.Unlock()
	for _, body := range bodies {
		_ = body.Close()
	}
	g.transport.CloseIdleConnections()
}

func (g *gateway) root(c *gin.Context) {
	if parsedHostFrom(c).PreviewPort != nil {
		g.proxy(c)
		return
	}
	if !strings.Contains(c.GetHeader("Accept"), "text/html") {
		g.proxy(c)
		return
	}
	if !g.authenticate(c) {
		return
	}
	c.Redirect(http.StatusFound, "/__ch/wb/")
}

func (g *gateway) previewPortAllowed(port int) bool {
	if port < g.deps.Config.PreviewPortMin || !domain.ValidPreviewTargetPort(port) {
		return false
	}
	for _, denied := range g.deps.Config.PreviewPortDeny {
		if denied == port {
			return false
		}
	}
	return true
}

func parsedHostFrom(c *gin.Context) ParsedHost {
	value, _ := c.Get(parsedHostKey)
	parsed, _ := value.(ParsedHost)
	return parsed
}

func hostRefFrom(c *gin.Context) host.Ref {
	value, _ := c.Get(hostRefKey)
	ref, _ := value.(host.Ref)
	return ref
}

func principalFrom(c *gin.Context) identity.Principal {
	value, _ := c.Get(principalKey)
	p, _ := value.(identity.Principal)
	return p
}

func hostURL(scheme, hostID, suffix string) string {
	return scheme + "://" + hostID + "." + strings.TrimPrefix(suffix, ".")
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeNotFound(c *gin.Context) {
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func urlForHost(u app.Upstream, path string) (*url.URL, error) {
	base, err := url.Parse(u.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
		return nil, fmt.Errorf("invalid upstream base url")
	}
	if strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("invalid upstream path")
	}
	escapedPath := strings.TrimRight(base.EscapedPath(), "/") + "/" + strings.TrimLeft(path, "/")
	base.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream path")
	}
	base.RawPath = escapedPath
	return base, nil
}
