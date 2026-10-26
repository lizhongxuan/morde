package gateway

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"morde/server/internal/aio"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/httpserver"
)

func (g *gateway) proxy(c *gin.Context) {
	parsed := parsedHostFrom(c)
	label := "proxy"
	if parsed.PreviewPort != nil {
		label = "preview"
	}
	httpserver.SetRoute(c, label)
	if strings.HasPrefix(c.Request.URL.Path, "/__ch/") || removedEndpoint(c.Request.URL.Path) {
		writeNotFound(c)
		return
	}
	ref := hostRefFrom(c)
	preview := parsed.PreviewPort != nil
	if preview && c.GetHeader("Authorization") == "" && hasNoHostCookie(c.Request) {
		g.redirectToPreviewAuth(c, *parsed.PreviewPort)
		return
	}
	if !g.authenticate(c) || !g.validRequestOrigin(c) {
		return
	}
	if ref.ID == "" {
		var err error
		ref, err = g.deps.Runtime.Lookup(c.Request.Context(), parsed.HostID)
		if err != nil {
			writeNotFound(c)
			return
		}
		c.Set(hostRefKey, ref)
	}
	if !g.requireRunning(c, ref) {
		return
	}
	if !g.requireStableControl(c, ref) {
		return
	}
	if parsed.PreviewPort == nil && !allowMainAIORequest(ref.ControlState, c.Request) {
		writeNotFound(c)
		return
	}
	hostID := parsed.HostID
	c.Set("gateway.started_at", time.Now())
	c.Set("host_id", hostID)
	c.Set("gateway.host_id", hostID)
	c.Set("gateway.upstream_prefix", upstreamPrefix(c.Request.URL.Path))
	port := 8080
	if preview {
		port = *parsed.PreviewPort
		c.Set("gateway.preview_port", port)
	}
	kind := app.HostActivityLeaseKindGateway
	if preview {
		kind = app.HostActivityLeaseKindPreview
	}
	checkAuthorization := g.memberAuthorizationCheck(c)
	guard, leaseCtx, err := g.acquireHostActivityLease(c.Request.Context(), ref, kind, checkAuthorization)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			notReady := domain.NewError(domain.CodeHostNotReady, "host control or runtime state changed")
			notReady.RetryAfter = time.Second
			err = notReady
		}
		httpserver.WriteError(c, err)
		return
	}
	c.Request = c.Request.WithContext(leaseCtx)
	defer func() {
		if err := guard.Close(); err != nil {
			g.deps.Logger.Warn("gateway activity lease release failed", zap.String("host_id", hostID), zap.String("kind", kind))
		}
	}()
	g.markActive(hostID)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, g.deps.Config.MaxBodyBytes)
	upstream, err := g.deps.Runtime.Resolve(c.Request.Context(), hostID, port)
	if err != nil {
		httpserver.WriteError(c, upstreamError(err))
		return
	}
	if preview {
		upstream.APIKey = ""
	}
	if _, err := urlForHost(upstream, c.Request.URL.EscapedPath()); err != nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeUpstreamError, "upstream unavailable"))
		return
	}
	if err := clearDeadlines(c); err != nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "gateway deadline control unavailable"))
		return
	}
	g.serveProxy(c, upstream, port, preview, 0)
}

func hasNoHostCookie(r *http.Request) bool {
	cookie, err := r.Cookie(hostCookieName)
	return err != nil || cookie.Value == ""
}

func (g *gateway) redirectToPreviewAuth(c *gin.Context, port int) {
	nextURL := &url.URL{Path: c.Request.URL.Path, RawQuery: c.Request.URL.RawQuery}
	query := url.Values{"port": []string{strconv.Itoa(port)}, "next": []string{nextURL.String()}}
	target := g.deps.Domains.Scheme + "://" + g.deps.Domains.Portal + "/api/hosts/" + c.GetString(hostIDKey) + "/preview-auth?" + query.Encode()
	c.Redirect(http.StatusFound, target)
}

func (g *gateway) serveProxy(c *gin.Context, upstream app.Upstream, port int, preview bool, attempt int) {
	hostID := c.GetString(hostIDKey)
	proxy := &httputil.ReverseProxy{
		Transport:     g.transport,
		FlushInterval: -1,
		Rewrite: func(request *httputil.ProxyRequest) {
			target, err := urlForHost(upstream, request.In.URL.EscapedPath())
			if err != nil {
				request.Out.URL = &url.URL{Scheme: "invalid"}
				return
			}
			target.RawQuery = request.In.URL.RawQuery
			request.Out.URL = target
			request.Out.Host = target.Host
			request.Out.Header.Del("Authorization")
			request.Out.Header.Del(aio.HeaderAPIKey)
			if isRFBWebSocketRequest(request.In, port, preview) {
				request.Out.Header.Del("Sec-WebSocket-Extensions")
			}
			removeGatewayCookie(request.Out)
			for name, value := range upstream.Headers {
				request.Out.Header.Set(name, value)
			}
			removeGatewayCookie(request.Out)
			request.Out.Header.Del(aio.HeaderAPIKey)
			if !preview && upstream.APIKey != "" {
				request.Out.Header.Set(aio.HeaderAPIKey, upstream.APIKey)
			}
			remoteIP := clientIP(request.In)
			if remoteIP != "" {
				request.Out.Header.Set("X-Forwarded-For", remoteIP)
			}
			proto := g.deps.Domains.Scheme
			if request.In.TLS != nil {
				proto = "https"
			} else if g.deps.Domains.Scheme == "http" {
				proto = "http"
			}
			request.Out.Header.Set("X-Forwarded-Proto", proto)
			request.Out.Header.Set("X-Forwarded-Host", request.In.Host)
		},
		ModifyResponse: func(response *http.Response) error {
			c.Set("gateway.upstream_ms", time.Since(startedAt(c)).Milliseconds())
			if !preview && port == 8080 && response.StatusCode == http.StatusNotFound {
				g.markMissingTerminalSession(c, c.Request)
			}
			if response.StatusCode >= http.StatusInternalServerError {
				g.deps.Runtime.InvalidateUpstream(hostID)
				g.upstreamErrs.WithLabelValues("status_5xx").Inc()
			}
			if response.StatusCode == http.StatusSwitchingProtocols {
				if body, ok := response.Body.(io.ReadWriteCloser); ok {
					tracked := g.trackWebSocket(body, hostID)
					if isRFBWebSocketRequest(c.Request, port, preview) {
						response.Header.Del("Sec-WebSocket-Extensions")
						memberView, _ := c.Get("gateway.member_view")
						readOnly := memberView == true
						response.Body = newRFBFilteringBody(tracked, c.Request.Context(), hostRefFrom(c), g.deps.InputPermits, !readOnly, func(err error) {
							g.deps.Logger.Warn("gateway RFB input rejected", zap.String("host_id", hostID), zap.Error(err))
						})
					} else {
						response.Body = tracked
					}
					c.Set(wsKey, true)
					c.Set("gateway.ws", true)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() != nil {
				return
			}
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				httpserver.WriteError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
				return
			}
			g.deps.Runtime.InvalidateUpstream(hostID)
			kind := upstreamErrorKind(err)
			g.upstreamErrs.WithLabelValues(kind).Inc()
			if attempt == 0 && safeRetryRequest(req) {
				fresh, resolveErr := g.deps.Runtime.Resolve(req.Context(), hostID, port)
				if resolveErr == nil {
					if preview {
						fresh.APIKey = ""
					}
					g.serveProxy(c, fresh, port, preview, 1)
					return
				}
			}
			g.deps.Logger.Warn("gateway upstream request failed", zap.String("host_id", hostID), zap.String("kind", kind))
			httpserver.WriteError(c, upstreamError(err))
		},
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}

func startedAt(c *gin.Context) time.Time {
	value, ok := c.Get("gateway.started_at")
	if !ok {
		started := time.Now()
		c.Set("gateway.started_at", started)
		return started
	}
	started, _ := value.(time.Time)
	return started
}

func safeRetryRequest(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.Body == nil || r.Body == http.NoBody) && r.ContentLength == 0 && len(r.TransferEncoding) == 0
}

func removeGatewayCookie(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != hostCookieName {
			r.AddCookie(cookie)
		}
	}
}

func clearDeadlines(c *gin.Context) error {
	controller := http.NewResponseController(c.Writer)
	readErr := controller.SetReadDeadline(time.Time{})
	writeErr := controller.SetWriteDeadline(time.Time{})
	if errors.Is(readErr, http.ErrNotSupported) {
		readErr = nil
	}
	if errors.Is(writeErr, http.ErrNotSupported) {
		writeErr = nil
	}
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func upstreamPrefix(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch first {
	case "v1", "vnc":
		return "/" + first
	default:
		return "/other"
	}
}

func allowMainAIORequest(controlState host.ControlState, request *http.Request) bool {
	requestPath := request.URL.Path
	cleanPath := path.Clean(requestPath)
	if requestPath == "" || strings.HasPrefix(requestPath, "//") || strings.Contains(requestPath, "//") ||
		(cleanPath != requestPath && (cleanPath == "/" || cleanPath+"/" != requestPath)) {
		return false
	}
	websocket := strings.EqualFold(request.Header.Get("Upgrade"), "websocket") && hasHeaderToken(request.Header.Get("Connection"), "upgrade")
	upgradeAttempt := request.Header.Get("Upgrade") != "" || hasHeaderToken(request.Header.Get("Connection"), "upgrade")
	if upgradeAttempt {
		if !websocket || request.Method != http.MethodGet {
			return false
		}
		switch requestPath {
		case "/ws", "/websockify":
			return controlState == host.ControlAI || controlState == host.ControlUser
		case "/v1/shell/ws":
			return controlState == host.ControlUser
		default:
			return controlState == host.ControlUser && (strings.HasPrefix(requestPath, "/cdp/devtools/") || strings.HasPrefix(requestPath, "/code-server/"))
		}
	}
	if controlState == host.ControlAI {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			return false
		}
		return requestPath == "/vnc" || strings.HasPrefix(requestPath, "/vnc/")
	}
	return controlState == host.ControlUser
}

func isRFBWebSocketRequest(request *http.Request, port int, preview bool) bool {
	if preview || port != 8080 || request == nil || request.Method != http.MethodGet ||
		!strings.EqualFold(request.Header.Get("Upgrade"), "websocket") || !hasHeaderToken(request.Header.Get("Connection"), "upgrade") {
		return false
	}
	return request.URL.Path == "/ws" || request.URL.Path == "/websockify"
}

func upstreamError(err error) error {
	var domainErr *domain.Error
	if errors.As(err, &domainErr) && (domainErr.Code == domain.CodeUpstreamError || domainErr.Code == domain.CodeUpstreamTimeout) {
		return domainErr
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return domain.NewError(domain.CodeUpstreamTimeout, "upstream request timed out")
	}
	return domain.NewError(domain.CodeUpstreamError, "upstream unavailable")
}

func upstreamErrorKind(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "unreachable"
}

func removedEndpoint(path string) bool {
	for _, prefix := range []string{"/mcp", "/devtools", "/v1/browser/info"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
