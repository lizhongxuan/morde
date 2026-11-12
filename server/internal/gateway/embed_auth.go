package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

const (
	embedCookieName      = "ch_embed"
	embedTicketBodyLimit = 4096
)

type embedClaims struct {
	HostID               string                     `json:"h"`
	ActorID              string                     `json:"a"`
	PortalTokenID        string                     `json:"t"`
	ClientInstallationID string                     `json:"i,omitempty"`
	ConversationID       string                     `json:"c"`
	ServiceInstanceID    string                     `json:"s"`
	PortalOrigin         string                     `json:"o"`
	InitialTarget        string                     `json:"v"`
	TargetGrants         []app.HostEmbedTargetGrant `json:"r"`
	EmbedNonce           string                     `json:"n"`
	RuntimeGeneration    int64                      `json:"g"`
	ExpiresAt            int64                      `json:"e"`
}

type hostEmbedCredentialValidator interface {
	ValidateHostEmbedCredential(context.Context, string, string, string) error
}

func (g *gateway) exchangeEmbedTicket(c *gin.Context) {
	g.setEmbedResponseHeaders(c)
	if parsedHostFrom(c).PreviewPort != nil || c.Request.URL.RawQuery != "" || c.GetHeader("Authorization") != "" ||
		c.GetHeader("Origin") != g.portalOrigin() || g.deps.HostEmbedTickets == nil ||
		!domain.ValidServiceInstanceID(g.deps.ServiceInstanceID) {
		writeNotFound(c)
		return
	}
	if !g.rateLimit(c) {
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeNotFound(c)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, embedTicketBodyLimit))
	if err != nil {
		writeNotFound(c)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || len(form) != 2 || len(form["ticket"]) != 1 || len(form["embed_nonce"]) != 1 ||
		form.Get("ticket") == "" || !validGatewayEmbedNonce(form.Get("embed_nonce")) {
		writeNotFound(c)
		return
	}
	record, err := g.deps.HostEmbedTickets.ConsumeEmbedTicket(c.Request.Context(), form.Get("ticket"), form.Get("embed_nonce"),
		c.GetString(hostIDKey), g.deps.ServiceInstanceID, g.portalOrigin())
	if err != nil || !validConsumedEmbedTicket(record, c.GetString(hostIDKey), g.deps.ServiceInstanceID, g.portalOrigin(), form.Get("embed_nonce")) {
		g.authFailure(c, "embed_ticket")
		return
	}
	expiresAt := time.Now().Add(g.deps.Security.HostCookieTTL)
	claims := embedClaims{
		HostID: record.HostID, ActorID: record.ActorID, PortalTokenID: record.PortalTokenID,
		ClientInstallationID: record.ClientInstallationID, ConversationID: record.ConversationID,
		ServiceInstanceID: record.ServiceInstanceID, PortalOrigin: record.PortalOrigin,
		InitialTarget: record.InitialTarget, TargetGrants: record.TargetGrants, EmbedNonce: record.EmbedNonce,
		RuntimeGeneration: record.RuntimeGeneration,
		ExpiresAt:         expiresAt.Unix(),
	}
	g.setEmbedCookie(c, claims, expiresAt)
	c.Redirect(http.StatusSeeOther, embedTargetPath(record.InitialTarget, record.EmbedNonce))
}

func validConsumedEmbedTicket(record app.HostEmbedTicketRecord, hostID, serviceInstanceID, portalOrigin, nonce string) bool {
	if record.UsedAt == nil || !record.ExpiresAt.After(time.Now()) || record.HostID != hostID ||
		record.ServiceInstanceID != serviceInstanceID || record.PortalOrigin != portalOrigin ||
		!domain.ValidHostID(record.HostID) || !domain.ValidConversationID(record.ConversationID) ||
		!domain.ValidUserID(record.ActorID) || !domain.ValidTokenID(record.PortalTokenID) ||
		(record.ClientInstallationID != "" && !domain.ValidClientInstallationID(record.ClientInstallationID)) || record.RuntimeGeneration < 1 ||
		record.EmbedNonce != nonce || !validEmbedGrantSet(record.InitialTarget, record.TargetGrants, nonce) {
		return false
	}
	return true
}

func validEmbedGrantSet(initialTarget string, grants []app.HostEmbedTargetGrant, nonce string) bool {
	if !validGatewayEmbedNonce(nonce) || len(grants) == 0 || len(grants) > 5 {
		return false
	}
	initialAllowed := false
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if _, exists := seen[grant.Target]; exists || !validEmbedTargetGrant(grant) {
			return false
		}
		seen[grant.Target] = struct{}{}
		initialAllowed = initialAllowed || grant.Target == initialTarget
	}
	return initialAllowed
}

func validEmbedTargetGrant(grant app.HostEmbedTargetGrant) bool {
	switch grant.Target {
	case app.HostEmbedTargetBrowser:
		return grant.PermissionCeiling == host.PermissionDesktopView || grant.PermissionCeiling == host.PermissionDesktopControl
	case app.HostEmbedTargetTerminal:
		return grant.PermissionCeiling == host.PermissionTerminalControl
	case app.HostEmbedTargetWorkspace:
		return grant.PermissionCeiling == host.PermissionWorkspaceFileRead
	case app.HostEmbedTargetFiles, app.HostEmbedTargetVSCode:
		return grant.PermissionCeiling == host.PermissionDesktopControl
	default:
		return false
	}
}

func validGatewayEmbedNonce(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 24
}

func embedTargetPath(target, nonce string) string {
	query := url.Values{"embed": {"1"}, "embed_nonce": {nonce}, "tab": {target}}
	return "/__ch/wb/?" + query.Encode()
}

func (g *gateway) portalOrigin() string {
	return g.deps.Domains.Scheme + "://" + g.deps.Domains.Portal
}

func (g *gateway) setEmbedResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
	if g.deps.Domains.CookieSecure {
		c.Header("Cache-Control", "no-store, private")
	}
}

func (g *gateway) setEmbedCookie(c *gin.Context, claims embedClaims, expiresAt time.Time) {
	value := g.signEmbedClaims(claims)
	c.Writer.Header().Add("Set-Cookie", (&http.Cookie{
		Name: embedCookieName, Value: value, Path: "/", Expires: expiresAt.UTC(),
		MaxAge: int(time.Until(expiresAt).Seconds()), HttpOnly: true,
		Secure: g.deps.Domains.CookieSecure, SameSite: http.SameSiteStrictMode,
	}).String())
}

func (g *gateway) clearEmbedCookie(c *gin.Context) {
	c.Writer.Header().Add("Set-Cookie", (&http.Cookie{
		Name: embedCookieName, Value: "", Path: "/", Expires: time.Unix(1, 0), MaxAge: -1,
		HttpOnly: true, Secure: g.deps.Domains.CookieSecure, SameSite: http.SameSiteStrictMode,
	}).String())
}

func (g *gateway) signEmbedClaims(claims embedClaims) string {
	data, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(data)
	message := "v1." + payload
	mac := hmac.New(sha256.New, g.cookieKey)
	_, _ = mac.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *gateway) verifyEmbedClaims(raw, hostID string, now time.Time) (embedClaims, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[2]) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return embedClaims{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size {
		return embedClaims{}, false
	}
	mac := hmac.New(sha256.New, g.cookieKey)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return embedClaims{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(data) > 2048 {
		return embedClaims{}, false
	}
	var claims embedClaims
	if json.Unmarshal(data, &claims) != nil || claims.HostID != hostID || claims.ServiceInstanceID != g.deps.ServiceInstanceID ||
		claims.PortalOrigin != g.portalOrigin() || claims.ExpiresAt <= now.Unix() || claims.RuntimeGeneration < 1 ||
		!domain.ValidConversationID(claims.ConversationID) || !domain.ValidUserID(claims.ActorID) || !domain.ValidTokenID(claims.PortalTokenID) ||
		(claims.ClientInstallationID != "" && !domain.ValidClientInstallationID(claims.ClientInstallationID)) ||
		!validPortalOrigin(claims.PortalOrigin, g.deps.Domains.Scheme) ||
		!validEmbedGrantSet(claims.InitialTarget, claims.TargetGrants, claims.EmbedNonce) {
		return embedClaims{}, false
	}
	return claims, true
}

func (g *gateway) authenticateEmbed(c *gin.Context, raw string) bool {
	if c.GetHeader("Authorization") != "" || parsedHostFrom(c).PreviewPort != nil || !g.validEmbedRequestOrigin(c) {
		g.authFailure(c, "embed")
		g.clearEmbedCookie(c)
		return false
	}
	claims, valid := g.verifyEmbedClaims(raw, c.GetString(hostIDKey), time.Now())
	if !valid {
		g.authFailure(c, "embed")
		g.clearEmbedCookie(c)
		return false
	}
	validator, ok := g.deps.Auth.(hostEmbedCredentialValidator)
	if !ok || validator.ValidateHostEmbedCredential(c.Request.Context(), claims.PortalTokenID, claims.ActorID,
		claims.ClientInstallationID) != nil {
		g.authFailure(c, "embed")
		g.clearEmbedCookie(c)
		return false
	}
	ref, err := g.deps.Runtime.Lookup(c.Request.Context(), claims.HostID)
	if err != nil || ref.ID != claims.HostID || ref.State != host.StateRunning || ref.RuntimeGeneration != claims.RuntimeGeneration ||
		!validHostOwner(ref.OwnerID) {
		g.authFailure(c, "embed")
		g.clearEmbedCookie(c)
		return false
	}
	required, allowed := embedRequestPermission(c.Request, claims)
	if !allowed {
		writeNotFound(c)
		return false
	}
	owner, err := g.deps.HostSharing.Authorize(c.Request.Context(), claims.HostID, claims.ActorID, required)
	if err != nil || owner != (claims.ActorID == ref.OwnerID) {
		g.authFailure(c, "embed")
		g.clearEmbedCookie(c)
		return false
	}
	principal := identity.Principal{UserID: claims.ActorID, Role: identity.RoleUser, Kind: identity.KindHostCookie,
		HostScope: claims.HostID, ClientInstallationID: claims.ClientInstallationID, TokenID: claims.PortalTokenID}
	c.Set(hostRefKey, ref)
	c.Set(principalKey, principal)
	c.Set("gateway.embed_claims", claims)
	c.Set("gateway.member", !owner)
	c.Set("gateway.member_permission", string(required))
	c.Set("gateway.member_view", required == host.PermissionDesktopView)
	c.Set("gateway.embed_required_permission", string(required))
	c.Set("auth_kind", "embed")
	c.Set("gateway.auth_kind", "embed")
	return true
}

func (g *gateway) validEmbedRequestOrigin(c *gin.Context) bool {
	origin := c.GetHeader("Origin")
	if origin == "" && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
		return true
	}
	parsed := parsedHostFrom(c)
	want := g.deps.Domains.Scheme + "://" + parsed.HostID + "." + strings.TrimPrefix(g.deps.Domains.HostSuffix, ".")
	return origin != "" && strings.EqualFold(origin, want)
}

func validHostOwner(value string) bool { return domain.ValidUserID(value) }

func embedRequestPermission(request *http.Request, claims embedClaims) (host.Permission, bool) {
	requestPath := request.URL.Path
	if strings.Contains(requestPath, "//") || (path.Clean(requestPath) != requestPath && path.Clean(requestPath)+"/" != requestPath) {
		return "", false
	}
	if requestPath == "/__ch/wb/" || requestPath == "/__ch/wb" {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			return "", false
		}
		grant, valid := embedWorkbenchQueryGrant(request, claims)
		if !valid {
			return "", false
		}
		return grant.PermissionCeiling, true
	}
	if strings.HasPrefix(requestPath, "/__ch/wb/") {
		if request.Method != http.MethodGet && request.Method != http.MethodHead || request.URL.RawQuery != "" {
			return "", false
		}
		grant, ok := embedGrant(claims, claims.InitialTarget)
		return grant.PermissionCeiling, ok
	}
	if requestPath == "/__ch/api/workspaces" || strings.HasPrefix(requestPath, "/__ch/api/workspaces/") {
		if request.Method != http.MethodGet || request.URL.RawQuery != "" ||
			(requestPath != "/__ch/api/workspaces" && !validHostWorkspaceReadRequest(request)) {
			return "", false
		}
		return embedGrantPermission(claims, app.HostEmbedTargetWorkspace)
	}
	if requestPath == "/vnc" || strings.HasPrefix(requestPath, "/vnc/") {
		if request.Method != http.MethodGet && request.Method != http.MethodHead || request.URL.RawQuery != "" {
			return "", false
		}
		return embedBrowserPermission(claims, false)
	}
	if requestPath == "/ws" || requestPath == "/websockify" {
		if request.Method != http.MethodGet || !websocketRequest(request) || request.URL.RawQuery != "" {
			return "", false
		}
		return embedBrowserPermission(claims, false)
	}
	switch requestPath {
	case "/__ch/api/control":
		if (request.Method == http.MethodGet || request.Method == http.MethodHead) && request.URL.RawQuery == "" {
			return embedBrowserPermission(claims, false)
		}
	case "/__ch/api/control/actions", "/__ch/api/control/lease/renew":
		if request.Method == http.MethodPost && request.URL.RawQuery == "" {
			return embedBrowserPermission(claims, true)
		}
	case "/__ch/api/applications":
		if request.Method == http.MethodGet && request.URL.RawQuery == "" {
			return embedBrowserPermission(claims, false)
		}
	default:
		if strings.HasPrefix(requestPath, "/__ch/api/applications/") && request.URL.RawQuery == "" {
			if request.Method == http.MethodGet {
				return embedBrowserPermission(claims, false)
			}
			if request.Method == http.MethodPost {
				return embedBrowserPermission(claims, true)
			}
		}
	}
	if requestPath == "/__ch/api/terminal-session" {
		if request.URL.RawQuery != "" || request.Method != http.MethodPut && request.Method != http.MethodPost {
			return "", false
		}
		return embedGrantPermission(claims, app.HostEmbedTargetTerminal)
	}
	if requestPath == "/__ch/terminal" || requestPath == "/__ch/terminal/" {
		if request.URL.RawQuery != "" || request.Method != http.MethodGet {
			return "", false
		}
		return embedGrantPermission(claims, app.HostEmbedTargetTerminal)
	}
	if requestPath == "/terminal" || requestPath == "/v1/shell/ws" {
		if request.Method != http.MethodGet || !validTerminalSessionQuery(request) ||
			(requestPath == "/v1/shell/ws" && !websocketRequest(request)) {
			return "", false
		}
		return embedGrantPermission(claims, app.HostEmbedTargetTerminal)
	}
	if requestPath == "/__ch/api/host" && request.Method == http.MethodGet {
		if request.URL.RawQuery != "" {
			return "", false
		}
		return anyEmbedGrantPermission(claims)
	}
	if strings.HasPrefix(requestPath, "/v1/file/") {
		valid := request.Method == http.MethodPost && request.URL.RawQuery == ""
		switch requestPath {
		case "/v1/file/list", "/v1/file/read", "/v1/file/write", "/v1/file/upload":
		case "/v1/file/download":
			valid = request.Method == http.MethodGet && validFileDownloadQuery(request)
		default:
			valid = false
		}
		if !valid {
			return "", false
		}
		return embedGrantPermission(claims, app.HostEmbedTargetFiles)
	}
	if strings.HasPrefix(requestPath, "/code-server/") || strings.HasPrefix(requestPath, "/cdp/devtools/") {
		if request.Method != http.MethodGet && request.Method != http.MethodHead ||
			request.Method == http.MethodGet && request.Header.Get("Upgrade") != "" && !websocketRequest(request) {
			return "", false
		}
		permission, ok := embedGrantPermission(claims, app.HostEmbedTargetVSCode)
		return permission, ok && request.Method == http.MethodGet
	}
	return "", false
}

func embedWorkbenchQueryGrant(request *http.Request, claims embedClaims) (app.HostEmbedTargetGrant, bool) {
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(values) != 3 || len(values["embed"]) != 1 || values.Get("embed") != "1" ||
		len(values["embed_nonce"]) != 1 || values.Get("embed_nonce") != claims.EmbedNonce ||
		len(values["tab"]) != 1 {
		return app.HostEmbedTargetGrant{}, false
	}
	return embedGrant(claims, values.Get("tab"))
}

func embedGrant(claims embedClaims, target string) (app.HostEmbedTargetGrant, bool) {
	for _, grant := range claims.TargetGrants {
		if grant.Target == target && validEmbedTargetGrant(grant) {
			return grant, true
		}
	}
	return app.HostEmbedTargetGrant{}, false
}

func embedGrantPermission(claims embedClaims, target string) (host.Permission, bool) {
	grant, ok := embedGrant(claims, target)
	return grant.PermissionCeiling, ok
}

func embedBrowserPermission(claims embedClaims, requireControl bool) (host.Permission, bool) {
	grant, ok := embedGrant(claims, app.HostEmbedTargetBrowser)
	if !ok {
		return "", false
	}
	if requireControl && grant.PermissionCeiling != host.PermissionDesktopControl {
		return "", false
	}
	return grant.PermissionCeiling, true
}

func anyEmbedGrantPermission(claims embedClaims) (host.Permission, bool) {
	for _, grant := range claims.TargetGrants {
		if validEmbedTargetGrant(grant) {
			return grant.PermissionCeiling, true
		}
	}
	return "", false
}

func validFileDownloadQuery(request *http.Request) bool {
	values, err := url.ParseQuery(request.URL.RawQuery)
	return err == nil && len(values) == 1 && len(values["path"]) == 1 && values.Get("path") != ""
}

func (g *gateway) embedClaimsFrom(c *gin.Context) (embedClaims, bool) {
	claims, ok := c.Get("gateway.embed_claims")
	value, valid := claims.(embedClaims)
	return value, ok && valid
}

func (g *gateway) embedAuthorizationCheck(c *gin.Context, ref host.Ref, required host.Permission) func(context.Context) error {
	claims, _ := g.embedClaimsFrom(c)
	return func(ctx context.Context) error {
		validator, ok := g.deps.Auth.(hostEmbedCredentialValidator)
		if !ok {
			return domain.NewError(domain.CodeUnauthenticated, "embedded Host credential is unavailable")
		}
		if err := validator.ValidateHostEmbedCredential(ctx, claims.PortalTokenID, claims.ActorID, claims.ClientInstallationID); err != nil {
			return err
		}
		current, err := g.deps.Runtime.Lookup(ctx, claims.HostID)
		if err != nil || current.ID != claims.HostID || current.State != host.StateRunning || current.RuntimeGeneration != claims.RuntimeGeneration {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		granted := false
		for _, grant := range claims.TargetGrants {
			if grant.PermissionCeiling == required || required == host.PermissionDesktopView &&
				grant.Target == app.HostEmbedTargetBrowser && grant.PermissionCeiling == host.PermissionDesktopControl {
				granted = true
				break
			}
		}
		if !granted {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		owner, err := g.deps.HostSharing.Authorize(ctx, claims.HostID, claims.ActorID, required)
		if err != nil || owner != (claims.ActorID == current.OwnerID) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if ref.RuntimeGeneration != claims.RuntimeGeneration {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		return nil
	}
}

func validPortalOrigin(value, scheme string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == scheme && parsed.Host != "" && parsed.User == nil && parsed.Path == "" &&
		parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}
