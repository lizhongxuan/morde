package api

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
)

const portalCSP = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'self'; object-src 'none'"

func (h *api) securityHeaders(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Next()
}

func (h *api) noRoute(c *gin.Context) {
	path := c.Request.URL.Path
	if path == "/api" || strings.HasPrefix(path, "/api/") {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		h.serveAdmin(c)
		return
	}
	if path == "/app" || strings.HasPrefix(path, "/app/") {
		h.serveApp(c)
		return
	}
	h.servePortal(c)
}

func (h *api) serveApp(c *gin.Context) {
	httpserver.SetRoute(c, "app_static")
	files := h.options.PortalFiles
	if files == nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	name := strings.TrimPrefix(c.Request.URL.Path, "/app/")
	if name == c.Request.URL.Path || name == "" || name == "." {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	fileName := path.Join("app", name)
	if file, err := files.Open(fileName); err == nil {
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr == nil && !info.IsDir() {
			h.servePortalFile(c, fileName)
			return
		}
	}
	if name == "index.html" || (path.Ext(name) == "" && acceptsHTML(c)) {
		h.servePortalFile(c, "app/index.html")
		return
	}
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func (h *api) serveAdmin(c *gin.Context) {
	httpserver.SetRoute(c, "admin_static")
	for _, cookie := range c.Request.Cookies() {
		if cookie.Name != portalCookieName {
			continue
		}
		kind, _, _, err := identity.ParseCredential(cookie.Value)
		if err != nil || kind != identity.KindSession {
			continue
		}
		principal, err := h.deps.Verifier.VerifySession(c.Request.Context(), cookie.Value)
		if err == nil && principal.ClientInstallationID != "" {
			httpserver.WriteError(c, domain.NewError(domain.CodeForbidden, "client Web session cannot access administration"))
			return
		}
	}
	files := h.options.PortalFiles
	if files == nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	name := strings.TrimPrefix(c.Request.URL.Path, "/admin/")
	if name == c.Request.URL.Path || name == "" || name == "." {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	file, err := files.Open(path.Join("admin", name))
	if err == nil {
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr == nil && !info.IsDir() {
			h.servePortalFile(c, path.Join("admin", name))
			return
		}
	}
	if name == "index.html" || (path.Ext(name) == "" && acceptsHTML(c)) {
		h.servePortalFile(c, "admin/index.html")
		return
	}
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func (h *api) servePortal(c *gin.Context) {
	httpserver.SetRoute(c, "portal_static")
	files := h.options.PortalFiles
	if files == nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	name := strings.TrimPrefix(c.Request.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	file, err := files.Open(name)
	if err == nil {
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr == nil && !info.IsDir() {
			h.servePortalFile(c, name)
			return
		}
	}
	if acceptsHTML(c) {
		h.servePortalFile(c, "index.html")
		return
	}
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func (h *api) servePortalFile(c *gin.Context, name string) {
	contents, err := fs.ReadFile(h.options.PortalFiles, name)
	if err != nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	if name == "index.html" || name == "admin/index.html" || name == "app/index.html" {
		c.Header("Content-Security-Policy", portalCSP)
		c.Header("Cache-Control", "no-cache")
	} else {
		c.Header("Cache-Control", "no-cache")
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = http.DetectContentType(contents)
	}
	c.Header("Content-Type", contentType)
	var modTime time.Time
	if info, err := fs.Stat(h.options.PortalFiles, name); err == nil {
		modTime = info.ModTime()
	}
	http.ServeContent(c.Writer, c.Request, path.Base(name), modTime, bytes.NewReader(contents))
}

func acceptsHTML(c *gin.Context) bool {
	for _, value := range strings.Split(c.GetHeader("Accept"), ",") {
		if strings.Contains(strings.TrimSpace(value), "text/html") {
			return true
		}
	}
	return false
}

func serviceUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"code": "internal", "message": "service temporarily unavailable", "details": gin.H{},
		},
	})
}
