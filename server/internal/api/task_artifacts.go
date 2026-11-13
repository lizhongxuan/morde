package api

import (
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func (h *api) registerTaskArtifactContent(group *gin.RouterGroup, service TaskArtifactContentEndpointService) {
	if service != nil {
		group.GET("/tasks/:id/artifacts/:artifact_id/content", h.getTaskArtifactContent(service))
	}
}

func (h *api) getTaskArtifactContent(service TaskArtifactContentEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "task artifact download must not include a query string or body"))
			return
		}
		principal, ok := principalFrom(c)
		if !ok || principal.Kind != identity.KindSession {
			unauthenticated(c)
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		artifactID, ok := safeIDParam(c, "artifact_id", "artifact_id", domain.ValidArtifactID)
		if !ok {
			return
		}
		content, err := service.OpenTaskArtifactContent(c.Request.Context(), principal, taskID, artifactID)
		if err != nil {
			writeError(c, err)
			return
		}
		if content.Body == nil {
			writeError(c, domain.NewError(domain.CodeInternal, "task artifact content is unavailable"))
			return
		}
		defer func() { _ = content.Body.Close() }()
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": content.Filename})
		mediaType, _, mediaErr := mime.ParseMediaType(content.MediaType)
		if disposition == "" || strings.TrimSpace(content.Filename) == "" || content.Filename != path.Base(content.Filename) ||
			strings.ContainsAny(content.Filename, "\r\n\x00") || mediaErr != nil || mediaType == "" ||
			content.SizeBytes < 0 || content.SizeBytes > workspace.MaxSyncFileBytes || !validWorkspaceSyncSHA256(content.SHA256) {
			writeError(c, domain.NewError(domain.CodeInternal, "task artifact content metadata is invalid"))
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Header("Pragma", "no-cache")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Disposition", disposition)
		c.Header("X-Content-SHA256", content.SHA256)
		c.DataFromReader(http.StatusOK, content.SizeBytes, strings.TrimSpace(content.MediaType), content.Body, nil)
	}
}
