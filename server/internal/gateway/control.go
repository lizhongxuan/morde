package gateway

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/httpserver"
)

var hashedAssetName = regexp.MustCompile(`(?:^|[._-])[A-Za-z0-9_-]{8,}(?:[._-]|$)`)

func (g *gateway) consumeTicket(c *gin.Context) {
	if !g.rateLimit(c) {
		return
	}
	token := c.Query("t")
	if token == "" {
		writeNotFound(c)
		return
	}
	ticket, err := g.deps.Auth.ConsumeTicket(c.Request.Context(), token)
	previewPort := 0
	if parsedHostFrom(c).PreviewPort != nil {
		previewPort = *parsedHostFrom(c).PreviewPort
	}
	if err != nil || ticket.HostID != c.GetString(hostIDKey) || ticket.PreviewPort != previewPort || !domain.ValidUserID(ticket.UserID) || !domain.ValidUserID(ticket.ActorUserID) {
		g.authFailure(c, "ticket")
		return
	}
	ref, err := g.deps.Runtime.Lookup(c.Request.Context(), ticket.HostID)
	if err != nil || ref.OwnerID != ticket.UserID {
		g.authFailure(c, "ticket")
		return
	}
	next := safeNext(c.Query("next"))
	permission := host.PermissionDesktopView
	if previewPort != 0 && ticket.ActorUserID != ticket.UserID {
		g.authFailure(c, "ticket")
		return
	}
	if next == "/__ch/terminal" || next == "/__ch/terminal/" {
		permission = host.PermissionTerminalControl
	}
	owner, authErr := g.deps.HostSharing.Authorize(c.Request.Context(), ticket.HostID, ticket.ActorUserID, permission)
	if authErr != nil || owner != (ticket.ActorUserID == ticket.UserID) {
		g.authFailure(c, "ticket")
		return
	}
	g.setHostCookie(c, ticket.HostID, ticket.ActorUserID, ticket.ClientInstallationID)
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Set("auth_kind", "ticket")
	if parsedHostFrom(c).PreviewPort == nil && next == "/" {
		if ticket.ActorUserID == ticket.UserID {
			next = "/__ch/wb/"
		} else {
			next = "/vnc/"
		}
	}
	c.Redirect(http.StatusFound, next)
}

func (g *gateway) logout(c *gin.Context) {
	g.clearHostCookie(c)
	c.Status(http.StatusNoContent)
}

func (g *gateway) workbench(c *gin.Context) {
	if parsedHostFrom(c).PreviewPort != nil {
		writeNotFound(c)
		return
	}
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	name = filepath.Clean(name)
	if name == "." || name == "" {
		name = "index.html"
	}
	if name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		writeNotFound(c)
		return
	}
	filename := filepath.Join(g.deps.Config.WorkbenchDir, name)
	info, err := os.Stat(filename)
	if err != nil || info.IsDir() {
		filename = filepath.Join(g.deps.Config.WorkbenchDir, "index.html")
		info, err = os.Stat(filename)
		if err != nil || info.IsDir() {
			httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "workbench unavailable"))
			return
		}
		name = "index.html"
	}
	data, err := os.Open(filename)
	if err != nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "workbench unavailable"))
		return
	}
	defer func() { _ = data.Close() }()
	c.Header("Content-Security-Policy", "frame-ancestors 'self'")
	c.Header("X-Content-Type-Options", "nosniff")
	if name == "index.html" {
		c.Header("Cache-Control", "no-cache")
	} else if hashedAssetName.MatchString(filepath.Base(name)) {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	if ext := filepath.Ext(name); ext != "" {
		c.Header("Content-Type", mimeType(ext))
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(name), info.ModTime(), data)
}

func (g *gateway) getHost(c *gin.Context) {
	value, err := g.deps.Hosts.Get(c.Request.Context(), principalFrom(c), c.GetString(hostIDKey))
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostResponseFrom(value, g.deps.Domains))
}

func (g *gateway) renewHost(c *gin.Context) {
	value, err := g.deps.Hosts.Renew(c.Request.Context(), principalFrom(c), c.GetString(hostIDKey))
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostResponseFrom(value, g.deps.Domains))
}

func safeNext(next string) string {
	parsed, err := url.ParseRequestURI(next)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || strings.ContainsAny(parsed.Path, "\\\r\n") {
		return "/"
	}
	return parsed.RequestURI()
}

type hostResponse struct {
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	PortalOrigin string                     `json:"portal_origin"`
	OwnerID      string                     `json:"owner_id"`
	TemplateID   string                     `json:"template_id"`
	Isolation    string                     `json:"isolation"`
	State        string                     `json:"state"`
	StateReason  string                     `json:"state_reason"`
	URL          string                     `json:"url"`
	Workspaces   []workspaceBindingResponse `json:"workspaces"`
	Resources    resourcesResponse          `json:"resources"`
	ExpiresAt    *time.Time                 `json:"expires_at"`
	LastActiveAt *time.Time                 `json:"last_active_at"`
	CreatedAt    time.Time                  `json:"created_at"`
}

type workspaceRef struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Revision  int64           `json:"revision"`
	UsedBytes int64           `json:"used_bytes"`
	State     workspace.State `json:"state"`
}

type workspaceBindingResponse struct {
	Workspace       workspaceRef           `json:"workspace"`
	Directory       string                 `json:"directory"`
	State           workspace.BindingState `json:"state"`
	BindingVersion  int64                  `json:"binding_version"`
	DesiredRevision int64                  `json:"desired_revision"`
	AppliedRevision int64                  `json:"applied_revision"`
}

type resourcesResponse struct {
	CPUMilli      int `json:"cpu_milli"`
	MemMiB        int `json:"mem_mib"`
	CPULimitMilli int `json:"cpu_limit_milli"`
	MemLimitMiB   int `json:"mem_limit_mib"`
}

func hostResponseFrom(value host.Host, domains config.Domains) hostResponse {
	isolation := value.RuntimeProfile
	if isolation == "" {
		isolation = "container"
	}
	workspaces := make([]workspaceBindingResponse, 0, len(value.Workspaces))
	for _, binding := range value.Workspaces {
		workspaces = append(workspaces, workspaceBindingResponse{
			Workspace: workspaceRef{
				ID: binding.Workspace.ID, Name: binding.Workspace.Name, Revision: binding.Workspace.Revision,
				UsedBytes: binding.Workspace.UsedBytes, State: binding.Workspace.State,
			},
			Directory: binding.Directory, State: binding.State, BindingVersion: binding.BindingVersion,
			DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		})
	}
	return hostResponse{
		ID:           value.ID,
		Name:         value.Name,
		PortalOrigin: domains.Scheme + "://" + domains.Portal,
		OwnerID:      value.OwnerID,
		TemplateID:   value.TemplateID,
		Isolation:    isolation,
		State:        string(value.State),
		StateReason:  value.StateReason,
		URL:          hostURL(domains.Scheme, value.ID, domains.HostSuffix),
		Workspaces:   workspaces,
		Resources: resourcesResponse{
			CPUMilli: value.CPUMilli, MemMiB: value.MemMiB,
			CPULimitMilli: value.CPULimitMilli, MemLimitMiB: value.MemLimitMiB,
		},
		ExpiresAt:    value.ExpiresAt,
		LastActiveAt: value.LastActiveAt,
		CreatedAt:    value.CreatedAt,
	}
}

func mimeType(ext string) string {
	if value := mime.TypeByExtension(ext); value != "" {
		return value
	}
	return "application/octet-stream"
}
