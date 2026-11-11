package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (h *api) registerTaskFileFetchTransferRoutes(engine *gin.Engine, service TaskFileFetchEndpointService) {
	base := "/api/tasks/:id/file-fetches/:fetch_id"
	engine.POST(base+"/manifest", h.checkOrigin, h.taskFileFetchManifest(service))
	engine.PUT(base+"/chunks/:sequence", h.checkOrigin, h.taskFileFetchChunk(service))
	engine.POST(base+"/commit", h.checkOrigin, h.taskFileFetchCommit(service))
}

func (h *api) registerTaskFileFetchOwnerRoutes(group *gin.RouterGroup, service TaskFileFetchEndpointService) {
	group.GET("/tasks/:id/file-fetches", h.listTaskFileFetches(service))
	group.POST("/tasks/:id/file-fetches/:fetch_id/cancel", h.cancelTaskFileFetch(service))
	group.GET("/tasks/:id/file-fetches/:fetch_id/content", h.getTaskFileFetchContent(service))
	group.DELETE("/tasks/:id/file-fetches/:fetch_id/content", h.deleteTaskFileFetchContent(service))
}

func (h *api) taskFileFetchManifest(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !emptyTaskFileFetchQuery(c) || !taskFileFetchContentType(c, "application/json") {
			writeError(c, domain.InvalidArgument("request", "file fetch manifest request is invalid"))
			return
		}
		ticket, ok := taskFileFetchTicket(c)
		if !ok {
			writeError(c, domain.NewError(domain.CodeNotFound, "file fetch not found"))
			return
		}
		taskID, fetchID, ok := taskFileFetchIDs(c)
		if !ok {
			return
		}
		var request app.TaskFileFetchManifestRequest
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
		if !decodeStrictJSON(c, &request) {
			return
		}
		result, err := service.ManifestTaskFileFetch(c.Request.Context(), taskID, fetchID, ticket, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	}
}

func (h *api) taskFileFetchChunk(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !emptyTaskFileFetchQuery(c) || !taskFileFetchContentType(c, "application/octet-stream") || c.GetHeader("Content-Encoding") != "" {
			writeError(c, domain.InvalidArgument("request", "file fetch chunk request is invalid"))
			return
		}
		ticket, ok := taskFileFetchTicket(c)
		if !ok {
			writeError(c, domain.NewError(domain.CodeNotFound, "file fetch not found"))
			return
		}
		taskID, fetchID, ok := taskFileFetchIDs(c)
		if !ok {
			return
		}
		sequence64, err := strconv.ParseInt(c.Param("sequence"), 10, 32)
		if err != nil || sequence64 < 0 {
			writeError(c, domain.InvalidArgument("sequence", "file fetch chunk sequence is invalid"))
			return
		}
		digest := c.GetHeader("X-Chunk-SHA256")
		if len(digest) != 64 {
			writeError(c, domain.InvalidArgument("digest", "file fetch chunk digest is invalid"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, app.TaskFileFetchChunkBytes+1)
		result, err := service.PutTaskFileFetchChunk(c.Request.Context(), taskID, fetchID, ticket, int32(sequence64), digest, c.Request.Body)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	}
}

func (h *api) taskFileFetchCommit(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !emptyTaskFileFetchQuery(c) || !taskFileFetchContentType(c, "application/json") {
			writeError(c, domain.InvalidArgument("request", "file fetch commit request is invalid"))
			return
		}
		ticket, ok := taskFileFetchTicket(c)
		if !ok {
			writeError(c, domain.NewError(domain.CodeNotFound, "file fetch not found"))
			return
		}
		taskID, fetchID, ok := taskFileFetchIDs(c)
		if !ok {
			return
		}
		var request app.TaskFileFetchCommitRequest
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
		if !decodeStrictJSON(c, &request) {
			return
		}
		result, err := service.CommitTaskFileFetch(c.Request.Context(), taskID, fetchID, ticket, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	}
}

func (h *api) listTaskFileFetches(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "file fetch list request must not include a query string or body"))
			return
		}
		principal, ok := taskFileFetchSession(c)
		if !ok {
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		items, err := service.ListTaskFileFetches(c.Request.Context(), principal, taskID)
		if err != nil {
			writeError(c, err)
			return
		}
		response := taskFileFetchListResponse{Items: make([]taskFileFetchResponse, 0, len(items))}
		for _, value := range items {
			item, err := toTaskFileFetchResponse(value)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, item)
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) cancelTaskFileFetch(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "file fetch cancel request must not include a query string or body"))
			return
		}
		principal, ok := taskFileFetchSession(c)
		if !ok {
			return
		}
		taskID, fetchID, ok := taskFileFetchOwnerIDs(c)
		if !ok {
			return
		}
		result, err := service.CancelTaskFileFetch(c.Request.Context(), principal, taskID, fetchID)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toTaskFileFetchResponse(result)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) getTaskFileFetchContent(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "file fetch content request must not include a query string or body"))
			return
		}
		principal, ok := taskFileFetchSession(c)
		if !ok {
			return
		}
		taskID, fetchID, ok := taskFileFetchOwnerIDs(c)
		if !ok {
			return
		}
		content, err := service.OpenTaskFileFetchContent(c.Request.Context(), principal, taskID, fetchID)
		if err != nil {
			writeError(c, err)
			return
		}
		defer func() { _ = content.Body.Close() }()
		if !taskFileFetchResponseMediaTypeAllowed(content.MediaType) || content.SizeBytes < 1 || content.SizeBytes > app.TaskFileFetchMaxFileBytes {
			writeError(c, domain.NewError(domain.CodeInternal, "Task file fetch content metadata is invalid"))
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Header("Pragma", "no-cache")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Disposition", `attachment; filename="`+content.FileName+`"`)
		c.DataFromReader(http.StatusOK, content.SizeBytes, content.MediaType, content.Body, nil)
	}
}

func (h *api) deleteTaskFileFetchContent(service TaskFileFetchEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			writeError(c, domain.InvalidArgument("request", "file fetch delete request must not include a query string or body"))
			return
		}
		principal, ok := taskFileFetchSession(c)
		if !ok {
			return
		}
		taskID, fetchID, ok := taskFileFetchOwnerIDs(c)
		if !ok {
			return
		}
		if _, err := service.DeleteTaskFileFetchContent(c.Request.Context(), principal, taskID, fetchID); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func taskFileFetchTicket(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func taskFileFetchIDs(c *gin.Context) (string, string, bool) {
	taskID, ok := safeIDParam(c, "id", "id", domain.ValidTaskID)
	if !ok {
		return "", "", false
	}
	fetchID, ok := safeIDParam(c, "fetch_id", "fetch_id", domain.ValidOperationID)
	return taskID, fetchID, ok
}

func taskFileFetchOwnerIDs(c *gin.Context) (string, string, bool) {
	taskID, fetchID, ok := taskFileFetchIDs(c)
	return taskID, fetchID, ok
}

func taskFileFetchSession(c *gin.Context) (identity.Principal, bool) {
	principal, ok := principalFrom(c)
	if !ok || principal.Kind != identity.KindSession || principal.ClientInstallationID != "" {
		writeError(c, domain.NewError(domain.CodeNotFound, "file fetch not found"))
		return identity.Principal{}, false
	}
	return principal, true
}

func emptyTaskFileFetchQuery(c *gin.Context) bool {
	return c.Request.URL.RawQuery == ""
}

func taskFileFetchContentType(c *gin.Context, expected string) bool {
	return strings.EqualFold(strings.TrimSpace(c.GetHeader("Content-Type")), expected)
}

func taskFileFetchResponseMediaTypeAllowed(value string) bool {
	return value == "text/plain" || value == "text/markdown" || value == "text/csv" || value == "application/json" ||
		value == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		value == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		value == "application/vnd.openxmlformats-officedocument.presentationml.presentation" || value == "application/pdf"
}

func isTaskFileFetchOwnerRoute(method, path string) bool {
	if !strings.HasPrefix(path, "/api/tasks/") {
		return false
	}
	switch method {
	case http.MethodGet:
		return strings.HasSuffix(path, "/file-fetches") || strings.Contains(path, "/file-fetches/")
	case http.MethodPost:
		return strings.HasSuffix(path, "/file-fetches/:fetch_id/cancel")
	case http.MethodDelete:
		return strings.HasSuffix(path, "/file-fetches/:fetch_id/content")
	default:
		return false
	}
}
