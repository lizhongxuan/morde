package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
)

func (h *api) authenticateMCP(c *gin.Context) {
	for _, cookie := range c.Request.Cookies() {
		if cookie.Name == portalCookieName {
			h.authFailure(c, "session")
			return
		}
	}
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		h.authFailure(c, "api_key")
		return
	}
	kind, _, _, err := identity.ParseCredential(parts[1])
	if err != nil || kind != identity.KindAPIKey {
		failureKind := string(kind)
		if !validCredentialMetricKind(failureKind) {
			failureKind = "api_key"
		}
		h.authFailure(c, failureKind)
		return
	}
	principal, err := h.deps.Verifier.VerifyAPIKey(c.Request.Context(), parts[1])
	if err != nil {
		h.authFailure(c, "api_key")
		return
	}
	principal.Kind = identity.KindAPIKey
	c.Set(principalKey, principal)
	c.Set("auth_kind", "api_key")
	c.Next()
}

func (h *api) mcpDisabled(c *gin.Context) {
	httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
}

func (h *api) limitMCPBody(c *gin.Context) {
	body := c.Request.Body
	defer func() { _ = body.Close() }()
	limited := http.MaxBytesReader(c.Writer, body, h.options.MCPMaxBodyBytes)
	contents, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
			c.Abort()
			return
		}
		writeError(c, domain.InvalidArgument("body", "unable to read request body"))
		c.Abort()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(contents))
	c.Next()
}

func (h *api) serveMCP(c *gin.Context) {
	if h.deps.MCP == nil {
		serviceUnavailable(c)
		return
	}
	gin.WrapH(h.deps.MCP)(c)
}
