package api

import (
	"context"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type AgentWorkspaceFilesService interface {
	GetContext(context.Context, identity.Principal) (app.AgentWorkspaceView, error)
	List(context.Context, identity.Principal, string, string, string, int) (app.AgentWorkspaceFilePage, error)
	Open(context.Context, identity.Principal, string, string) (*os.File, os.FileInfo, error)
}

func (h *api) registerAgentWorkspaceFiles(group *gin.RouterGroup, service AgentWorkspaceFilesService) {
	group.GET("/me/agent-workspace", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		value, err := service.GetContext(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, value)
	})
	group.GET("/workspaces/:id/entries", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		query := c.Request.URL.Query()
		if len(query["path"]) > 1 || len(query["cursor"]) > 1 || len(query["limit"]) > 1 {
			writeError(c, domain.InvalidArgument("pagination", "Workspace directory query is invalid"))
			return
		}
		limit := 100
		if values := query["limit"]; len(values) == 1 {
			parsed, err := strconv.Atoi(values[0])
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(c, domain.InvalidArgument("limit", "Workspace directory limit must be between 1 and 200"))
				return
			}
			limit = parsed
		}
		var relativePath, cursor string
		if values := query["path"]; len(values) == 1 {
			relativePath = values[0]
		}
		if values := query["cursor"]; len(values) == 1 {
			cursor = values[0]
		}
		page, err := service.List(c.Request.Context(), principal, workspaceID, relativePath, cursor, limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	})
	group.GET("/workspaces/:id/files/*path", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		relativePath := strings.TrimPrefix(c.Param("path"), "/")
		file, info, err := service.Open(c.Request.Context(), principal, workspaceID, relativePath)
		if err != nil {
			writeError(c, err)
			return
		}
		defer func() { _ = file.Close() }()
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(relativePath)}))
		c.Header("Content-Type", "application/octet-stream")
		http.ServeContent(c.Writer, c.Request, filepath.Base(relativePath), info.ModTime(), file)
	})
}
