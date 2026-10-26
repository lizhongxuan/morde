package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type deviceWorkspaceSyncProgressResponse struct {
	Items []deviceWorkspaceSyncProgressItem `json:"items"`
}

type deviceWorkspaceSyncProgressItem struct {
	WorkspaceID          string    `json:"workspace_id"`
	DeviceID             string    `json:"device_id"`
	SessionID            string    `json:"session_id"`
	Direction            string    `json:"direction"`
	State                string    `json:"state"`
	Stage                string    `json:"stage"`
	PlannedBytes         int64     `json:"planned_bytes"`
	ConfirmedBytes       int64     `json:"confirmed_bytes"`
	DownloadActionsTotal int64     `json:"download_actions_total"`
	DownloadActionsDone  int64     `json:"download_actions_done"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (h *api) registerWorkspaceSync(group *gin.RouterGroup, service WorkspaceSyncService) {
	group.GET("/workspaces/:id/manifest", h.getWorkspaceManifest(service))
	group.GET("/workspaces/:id/changes", h.getWorkspaceChanges(service))
	group.POST("/workspaces/:id/sync-sessions", h.createWorkspaceSyncSession(service))
	group.GET("/workspaces/:id/sync-sessions/:session_id", h.getWorkspaceSyncSession(service))
	group.GET("/workspaces/:id/sync-progress", h.listDeviceWorkspaceSyncProgress(service))
	group.POST("/workspaces/:id/sync-sessions/:session_id/commit", h.commitWorkspaceSyncSession(service))
	group.POST("/workspaces/:id/sync-sessions/:session_id/cancel", h.cancelWorkspaceSyncSession(service))
	group.PUT("/workspaces/:id/sync-sessions/:session_id/blocks/:digest", h.putWorkspaceSyncBlock(service))
	group.GET("/workspaces/:id/sync-sessions/:session_id/blocks/:digest", h.getWorkspaceSyncBlock(service))
	group.POST("/workspaces/:id/sync-sessions/:session_id/blocks/:digest/ack", h.ackWorkspaceSyncBlock(service))
	group.POST("/workspaces/:id/sync-sessions/:session_id/download-actions/ack", h.ackWorkspaceSyncDownloadAction(service))
}

func (h *api) listDeviceWorkspaceSyncProgress(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		items, err := service.ListDeviceSyncProgress(c.Request.Context(), principal, workspaceID)
		if err != nil {
			writeError(c, err)
			return
		}
		response := deviceWorkspaceSyncProgressResponse{Items: make([]deviceWorkspaceSyncProgressItem, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, deviceWorkspaceSyncProgressItem{
				WorkspaceID: item.WorkspaceID, DeviceID: item.DeviceID, SessionID: item.SessionID,
				Direction: string(item.Direction), State: string(item.State), Stage: string(item.Stage),
				PlannedBytes: item.PlannedBytes, ConfirmedBytes: item.ConfirmedBytes,
				DownloadActionsTotal: item.DownloadActionsTotal, DownloadActionsDone: item.DownloadActionsDone,
				UpdatedAt: item.UpdatedAt,
			})
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) getWorkspaceManifest(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
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

		revision, err := optionalWorkspaceSyncInt(c, "revision", 0, 1, int64(^uint64(0)>>1))
		if err != nil {
			writeError(c, err)
			return
		}
		prefix, ok := workspaceSyncQueryText(c, "prefix", 1024, false)
		if !ok {
			return
		}
		cursor, ok := workspaceSyncQueryText(c, "cursor", 512, false)
		if !ok {
			return
		}
		limit, err := optionalWorkspaceSyncInt(c, "limit", 500, 1, 1000)
		if err != nil {
			writeError(c, err)
			return
		}

		page, err := service.GetManifest(c.Request.Context(), principal, workspaceID, revision, prefix, cursor, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

func (h *api) getWorkspaceChanges(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		after, ok := workspaceSyncQueryText(c, "after", 512, true)
		if !ok {
			return
		}
		waitSeconds, err := optionalWorkspaceSyncInt(c, "wait_seconds", 20, 0, 25)
		if err != nil {
			writeError(c, err)
			return
		}
		limit, err := optionalWorkspaceSyncInt(c, "limit", 200, 1, 500)
		if err != nil {
			writeError(c, err)
			return
		}

		page, err := service.GetChanges(c.Request.Context(), principal, workspaceID, after, time.Duration(waitSeconds)*time.Second, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

func (h *api) createWorkspaceSyncSession(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		idempotencyKey, ok := workspaceSyncIdempotencyKey(c)
		if !ok {
			return
		}
		var command workspace.CreateSyncSession
		if !decodeWorkspaceSyncJSON(c, &command) {
			return
		}
		session, err := service.CreateSyncSession(c.Request.Context(), principal, workspaceID, command, idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, session)
	}
}

func (h *api) getWorkspaceSyncSession(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		session, err := service.GetSyncSession(c.Request.Context(), principal, workspaceID, sessionID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

func (h *api) commitWorkspaceSyncSession(service WorkspaceSyncService) gin.HandlerFunc {
	return h.workspaceSyncSessionAction(service, func(ctx context.Context, principal identity.Principal, workspaceID, sessionID, key string) (workspace.SyncSession, error) {
		return service.CommitSyncSession(ctx, principal, workspaceID, sessionID, key)
	})
}

func (h *api) cancelWorkspaceSyncSession(service WorkspaceSyncService) gin.HandlerFunc {
	return h.workspaceSyncSessionAction(service, func(ctx context.Context, principal identity.Principal, workspaceID, sessionID, key string) (workspace.SyncSession, error) {
		return service.CancelSyncSession(ctx, principal, workspaceID, sessionID, key)
	})
}

func (h *api) workspaceSyncSessionAction(service WorkspaceSyncService, action func(context.Context, identity.Principal, string, string, string) (workspace.SyncSession, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		idempotencyKey, ok := workspaceSyncIdempotencyKey(c)
		if !ok {
			return
		}
		session, err := action(c.Request.Context(), principal, workspaceID, sessionID, idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

func (h *api) putWorkspaceSyncBlock(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		digest, ok := workspaceSyncDigest(c)
		if !ok {
			return
		}
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/octet-stream" {
			writeError(c, domain.InvalidArgument("body", "content type must be application/octet-stream"))
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			writeWorkspaceSyncBodyError(c, err)
			return
		}
		if len(body) == 0 {
			writeError(c, domain.InvalidArgument("body", "block body must not be empty"))
			return
		}
		if _, err := service.PutSyncBlock(c.Request.Context(), principal, workspaceID, sessionID, digest, bytes.NewReader(body)); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func (h *api) getWorkspaceSyncBlock(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		digest, ok := workspaceSyncDigest(c)
		if !ok {
			return
		}
		body, err := service.GetSyncBlock(c.Request.Context(), principal, workspaceID, sessionID, digest)
		if err != nil {
			writeError(c, err)
			return
		}
		if body == nil {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		defer func() { _ = body.Close() }()
		contents, err := io.ReadAll(io.LimitReader(body, workspace.MaxSyncBlockBytes+1))
		if err != nil || int64(len(contents)) > workspace.MaxSyncBlockBytes {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "application/octet-stream", contents)
	}
}

func (h *api) ackWorkspaceSyncBlock(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		digest, ok := workspaceSyncDigest(c)
		if !ok {
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if err != nil {
			writeWorkspaceSyncBodyError(c, err)
			return
		}
		if len(body) != 0 {
			writeError(c, domain.InvalidArgument("body", "request body must be empty"))
			return
		}
		if err := service.AckDownloadedBlock(c.Request.Context(), principal, workspaceID, sessionID, digest); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

type ackWorkspaceSyncDownloadActionRequest struct {
	Path      string                 `json:"path"`
	Action    workspace.ChangeAction `json:"action"`
	SHA256    *string                `json:"sha256"`
	SizeBytes *int64                 `json:"size_bytes"`
}

func (h *api) ackWorkspaceSyncDownloadAction(service WorkspaceSyncService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, sessionID, ok := workspaceSyncPathIDs(c)
		if !ok {
			return
		}
		var request ackWorkspaceSyncDownloadActionRequest
		if !decodeWorkspaceSyncJSON(c, &request) {
			return
		}
		if workspace.ValidateRelativePath(request.Path) != nil || request.SizeBytes == nil || *request.SizeBytes < 0 {
			writeError(c, domain.InvalidArgument("download_action", "download action is invalid"))
			return
		}
		digest := ""
		switch request.Action {
		case workspace.ChangeUpsert:
			if request.SHA256 == nil || !validWorkspaceSyncSHA256(*request.SHA256) {
				writeError(c, domain.InvalidArgument("download_action", "upsert action requires a lowercase SHA-256"))
				return
			}
			digest = *request.SHA256
		case workspace.ChangeDelete:
			if request.SHA256 != nil || *request.SizeBytes != 0 {
				writeError(c, domain.InvalidArgument("download_action", "delete action must not include a hash or size"))
				return
			}
		default:
			writeError(c, domain.InvalidArgument("download_action", "download action is invalid"))
			return
		}
		if err := service.AckDownloadAction(c.Request.Context(), principal, workspaceID, sessionID, request.Path, request.Action, digest, *request.SizeBytes); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func validWorkspaceSyncSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func workspaceSyncPathIDs(c *gin.Context) (string, string, bool) {
	workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
	if !ok {
		return "", "", false
	}
	sessionID := c.Param("session_id")
	if !workspace.ValidSessionID(sessionID) {
		writeError(c, domain.InvalidArgument("session_id", "invalid session id"))
		return "", "", false
	}
	return workspaceID, sessionID, true
}

func workspaceSyncDigest(c *gin.Context) (string, bool) {
	digest := c.Param("digest")
	if !validWorkspaceSyncSHA256(digest) {
		writeError(c, domain.InvalidArgument("digest", "digest must be a lowercase SHA-256 value"))
		return "", false
	}
	return digest, true
}

func workspaceSyncIdempotencyKey(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) != 1 || !validWorkspaceSyncText(values[0], 128) || strings.TrimSpace(values[0]) == "" {
		writeError(c, domain.InvalidArgument("Idempotency-Key", "exactly one valid idempotency key is required"))
		return "", false
	}
	return values[0], true
}

func decodeWorkspaceSyncJSON(c *gin.Context, target any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeWorkspaceSyncBodyError(c, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		writeWorkspaceSyncBodyError(c, err)
		return false
	}
	return true
}

func writeWorkspaceSyncBodyError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
		return
	}
	writeError(c, domain.InvalidArgument("body", "invalid request body"))
}

func workspaceSyncQueryText(c *gin.Context, key string, maxRunes int, required bool) (string, bool) {
	value, ok := singleQuery(c, key)
	if !ok {
		return "", false
	}
	if (required && value == "") || !validWorkspaceSyncText(value, maxRunes) {
		writeError(c, domain.InvalidArgument(key, "query parameter is invalid"))
		return "", false
	}
	return value, true
}

func validWorkspaceSyncText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes
}

func optionalWorkspaceSyncInt(c *gin.Context, key string, defaultValue, min, max int64) (int64, error) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return defaultValue, nil
	}
	if len(values) != 1 {
		return 0, domain.InvalidArgument(key, "query parameter is invalid")
	}
	parsed, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, domain.InvalidArgument(key, "query parameter is invalid")
	}
	return parsed, nil
}
