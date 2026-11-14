package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
)

const codexEventBatchBodyLimit int64 = codex.MaxEventBatchBytes + (4 << 10)

type CodexService interface {
	Discover(context.Context, identity.Principal, codex.Discover) (codex.Session, error)
	List(context.Context, identity.Principal, string, string, int32) (codex.SessionPage, error)
	Events(context.Context, identity.Principal, string, int64, int32) (codex.EventPage, error)
	LiveEvents(context.Context, identity.Principal, string, int64, int32) (codex.EventPage, error)
	CreateCommand(context.Context, identity.Principal, string, string, codex.CommandInput) (codex.Command, error)
	Claim(context.Context, identity.Principal) ([]codex.Command, error)
	Result(context.Context, identity.Principal, string, codex.CommandResult) (codex.Command, error)
	AppendEvents(context.Context, identity.Principal, string, codex.EventBatch) (codex.EventBatchAck, error)
	Delete(context.Context, identity.Principal, string) error
}

func registerCodexRoutes(group *gin.RouterGroup, service CodexService) {
	if service == nil {
		return
	}
	group.POST("/codex/sessions/discover", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request codex.Discover
		if !decodeStrictJSON(c, &request) {
			return
		}
		value, err := service.Discover(c.Request.Context(), principal, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"session": value})
	})

	group.GET("/codex/sessions", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		installationID := c.Query("installation_id")
		limit, ok := codexQueryLimit(c, "limit", 50, codex.MaxSessionPageSize)
		if !ok {
			return
		}
		page, err := service.List(c.Request.Context(), principal, installationID, c.Query("cursor"), limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	})

	group.GET("/codex/sessions/:id/events", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "session_id", domain.ValidCodexSessionID)
		if !ok {
			return
		}
		afterSequence, ok := codexAfterSequence(c)
		if !ok {
			return
		}
		limit, ok := codexQueryLimit(c, "limit", 100, codex.MaxEventPageSize)
		if !ok {
			return
		}
		page, err := service.Events(c.Request.Context(), principal, id, afterSequence, limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	})

	group.GET("/codex/sessions/:id/events/stream", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "session_id", domain.ValidCodexSessionID)
		if !ok {
			return
		}
		afterSequence, ok := codexAfterSequence(c)
		if !ok {
			return
		}
		initial, err := service.LiveEvents(c.Request.Context(), principal, id, afterSequence, 100)
		if err != nil {
			writeError(c, err)
			return
		}
		streamCodexEvents(c, service, principal, id, afterSequence, initial)
	})

	group.POST("/codex/sessions/:id/commands", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "session_id", domain.ValidCodexSessionID)
		if !ok {
			return
		}
		var request codex.CommandInput
		if !decodeStrictJSON(c, &request) {
			return
		}
		key := c.GetHeader("Idempotency-Key")
		if key == "" {
			writeError(c, domain.InvalidArgument("Idempotency-Key", "Idempotency-Key is required"))
			return
		}
		value, err := service.CreateCommand(c.Request.Context(), principal, id, key, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, value)
	})

	group.POST("/codex/commands/claim", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var empty struct{}
		if !decodeStrictJSON(c, &empty) {
			return
		}
		items, err := service.Claim(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	group.POST("/codex/commands/:id/result", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "command_id", domain.ValidCodexCommandID)
		if !ok {
			return
		}
		var request codex.CommandResult
		if !decodeStrictJSON(c, &request) {
			return
		}
		value, err := service.Result(c.Request.Context(), principal, id, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, value)
	})

	group.POST("/codex/sessions/:id/events", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "session_id", domain.ValidCodexSessionID)
		if !ok {
			return
		}
		var request codex.EventBatch
		if !decodeStrictJSON(c, &request) {
			return
		}
		value, err := service.AppendEvents(c.Request.Context(), principal, id, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, value)
	})

	group.DELETE("/codex/sessions/:id", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "session_id", domain.ValidCodexSessionID)
		if !ok {
			return
		}
		if err := service.Delete(c.Request.Context(), principal, id); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func codexQueryLimit(c *gin.Context, name string, defaultValue, maximum int32) (int32, bool) {
	value := c.Query(name)
	if value == "" {
		return defaultValue, true
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(maximum) {
		writeError(c, domain.InvalidArgument(name, "Codex page limit is invalid"))
		return 0, false
	}
	return int32(parsed), true
}

func codexAfterSequence(c *gin.Context) (int64, bool) {
	value := c.Query("after_sequence")
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		writeError(c, domain.InvalidArgument("after_sequence", "Codex event cursor is invalid"))
		return 0, false
	}
	return parsed, true
}

func streamCodexEvents(c *gin.Context, service CodexService, principal identity.Principal, sessionID string, afterSequence int64, page codex.EventPage) {
	flusher, err := beginTaskEventStream(c)
	if err != nil {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	flusher.Flush()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		for _, event := range page.Items {
			if event.Sequence != afterSequence+1 {
				return
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(c.Writer, "id: %d\nevent: codex\ndata: %s\n\n", event.Sequence, encoded); err != nil {
				return
			}
			flusher.Flush()
			afterSequence = event.Sequence
		}
		if page.NextCursor != nil {
			var err error
			page, err = service.LiveEvents(c.Request.Context(), principal, sessionID, afterSequence, 100)
			if err != nil {
				return
			}
			continue
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			var err error
			page, err = service.LiveEvents(c.Request.Context(), principal, sessionID, afterSequence, 100)
			if err != nil {
				return
			}
		}
	}
}

var _ CodexService = (*app.CodexService)(nil)
