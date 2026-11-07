package api

import (
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func (h *api) getTaskMediaPreview(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	taskID, ok := safeID(c, "id", domain.ValidTaskID)
	if !ok {
		return
	}
	operationID, ok := safeIDParam(c, "operation_id", "operation_id", domain.ValidOperationID)
	if !ok {
		return
	}
	preview, err := h.deps.TaskMedia.OpenPreview(c.Request.Context(), principal, taskID, operationID)
	if err != nil {
		writeError(c, err)
		return
	}
	if preview.Body == nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := preview.Body.Close(); err != nil {
			h.options.Logger.Warn("close task media preview body", zap.Error(err))
		}
	}()
	mediaType, params, err := mime.ParseMediaType(preview.MediaType)
	if err != nil || len(params) != 0 || mediaType != preview.MediaType || !taskMediaPreviewTypeAllowed(mediaType) ||
		preview.SizeBytes < 1 || preview.SizeBytes > workspace.MaxSyncFileBytes {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "inline")
	c.DataFromReader(http.StatusOK, preview.SizeBytes, mediaType, preview.Body, nil)
}

func taskMediaPreviewTypeAllowed(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "video/mp4", "audio/mpeg", "audio/ogg", "audio/opus", "audio/wav", "audio/aac":
		return true
	default:
		return false
	}
}
