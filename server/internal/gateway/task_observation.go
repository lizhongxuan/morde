package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	taskReadCookieName        = "ch_task_read"
	taskObservationSessionKey = "gateway.task_observation_session"
	taskObservationPollDelay  = 500 * time.Millisecond
)

type taskWorkbenchObservationService interface {
	ConsumeWorkbenchObservationTicket(context.Context, string, string, int64) (string, app.TaskWorkbenchObservationSession, error)
	ValidateWorkbenchObservationSession(context.Context, string, string, int64) (app.TaskWorkbenchObservationSession, error)
	GetWorkbenchObservationSnapshot(context.Context, string, string, int64) (app.TaskWorkbenchObservationSnapshot, error)
	ListWorkbenchObservationEvents(context.Context, string, string, int64, int64, int32) (app.TaskWorkbenchObservationEventPage, error)
}

func (g *gateway) exchangeTaskObservationTicket(c *gin.Context) {
	setTaskObservationResponseHeaders(c)
	if !g.rateLimit(c) {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["ticket"]) != 1 || query.Get("ticket") == "" {
		writeNotFound(c)
		return
	}
	if g.deps.TaskObservation == nil {
		writeNotFound(c)
		return
	}
	hostID := c.GetString(hostIDKey)
	ref, err := g.deps.Runtime.Lookup(c.Request.Context(), hostID)
	if err != nil || ref.ID != hostID || !domain.ValidUserID(ref.OwnerID) || ref.State != host.StateRunning || ref.RuntimeGeneration < 1 {
		writeNotFound(c)
		return
	}
	rawCookie, value, err := g.deps.TaskObservation.ConsumeWorkbenchObservationTicket(c.Request.Context(), query.Get("ticket"), hostID, ref.RuntimeGeneration)
	if err != nil || !domain.ValidTaskID(value.TaskID) || rawCookie == "" || value.HostID != hostID || value.ActorID != ref.OwnerID || value.RuntimeGeneration != ref.RuntimeGeneration || !value.ExpiresAt.After(time.Now()) {
		writeNotFound(c)
		return
	}
	maxAge := int(time.Until(value.ExpiresAt).Seconds())
	if maxAge < 1 {
		writeNotFound(c)
		return
	}
	cookie := &http.Cookie{
		Name: taskReadCookieName, Value: rawCookie, Path: "/__ch/",
		Expires: value.ExpiresAt.UTC(), MaxAge: maxAge, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode,
	}
	c.Writer.Header().Add("Set-Cookie", cookie.String())
	c.Redirect(http.StatusSeeOther, "/__ch/wb/?task_id="+url.QueryEscape(value.TaskID))
}

func (g *gateway) taskObservationReadAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		setTaskObservationResponseHeaders(c)
		if c.Request.Method != http.MethodGet || c.GetHeader("Authorization") != "" {
			writeNotFound(c)
			return
		}
		if !g.validRequestOrigin(c) {
			return
		}
		if !g.validateTaskObservationCookie(c) {
			return
		}
		if c.Param("task_id") != "" {
			session, _ := c.Get(taskObservationSessionKey)
			value, _ := session.(app.TaskWorkbenchObservationSession)
			if value.TaskID != c.Param("task_id") {
				writeNotFound(c)
				return
			}
		}
		c.Next()
	}
}

func (g *gateway) workbenchAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, err := c.Request.Cookie(taskReadCookieName); err == nil {
			setTaskObservationResponseHeaders(c)
			if !isTaskWorkbenchStaticRequest(c.Request) {
				writeNotFound(c)
				return
			}
			if !g.validRequestOrigin(c) || !g.validateTaskObservationCookie(c) {
				return
			}
			if c.Request.URL.Path == "/__ch/wb/" || c.Request.URL.Path == "/__ch/wb" {
				query, _ := url.ParseQuery(c.Request.URL.RawQuery)
				if query.Get("task_id") != taskObservationSessionFrom(c).TaskID {
					writeNotFound(c)
					return
				}
			}
			c.Next()
			return
		}
		if !g.authenticate(c) || !g.validRequestOrigin(c) {
			return
		}
		if principalFrom(c).Kind != identity.KindHostCookie {
			g.authFailure(c, "host_cookie")
			return
		}
		c.Next()
	}
}

func (g *gateway) validateTaskObservationCookie(c *gin.Context) bool {
	if g.deps.TaskObservation == nil {
		g.authFailure(c, "task_read")
		return false
	}
	cookie, err := c.Request.Cookie(taskReadCookieName)
	if err != nil || cookie.Value == "" {
		g.authFailure(c, "task_read")
		return false
	}
	hostID := c.GetString(hostIDKey)
	ref, err := g.deps.Runtime.Lookup(c.Request.Context(), hostID)
	if err != nil || ref.ID != hostID || ref.State != host.StateRunning || ref.RuntimeGeneration < 1 || !domain.ValidUserID(ref.OwnerID) {
		g.authFailure(c, "task_read")
		return false
	}
	session, err := g.deps.TaskObservation.ValidateWorkbenchObservationSession(c.Request.Context(), cookie.Value, hostID, ref.RuntimeGeneration)
	if err != nil || session.TaskID == "" || session.HostID != hostID || session.ActorID != ref.OwnerID ||
		session.RuntimeGeneration != ref.RuntimeGeneration || !session.ExpiresAt.After(time.Now()) {
		g.authFailure(c, "task_read")
		return false
	}
	c.Set(taskObservationSessionKey, session)
	c.Set("auth_kind", "task_read")
	c.Set("gateway.auth_kind", "task_read")
	return true
}

func (g *gateway) getTaskObservationSnapshot(c *gin.Context) {
	if c.Request.URL.RawQuery != "" {
		writeNotFound(c)
		return
	}
	session := taskObservationSessionFrom(c)
	cookie, err := c.Request.Cookie(taskReadCookieName)
	if err != nil {
		writeNotFound(c)
		return
	}
	ref, err := g.deps.Runtime.Lookup(c.Request.Context(), c.GetString(hostIDKey))
	if err != nil || ref.ID != c.GetString(hostIDKey) || ref.OwnerID != session.ActorID || ref.State != host.StateRunning || ref.RuntimeGeneration < 1 {
		writeNotFound(c)
		return
	}
	value, err := g.deps.TaskObservation.GetWorkbenchObservationSnapshot(c.Request.Context(), cookie.Value, c.GetString(hostIDKey), ref.RuntimeGeneration)
	if err != nil || value.TaskID != session.TaskID || value.HostID != c.GetString(hostIDKey) {
		writeNotFound(c)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (g *gateway) streamTaskObservationEvents(c *gin.Context) {
	session := taskObservationSessionFrom(c)
	cookie, err := c.Request.Cookie(taskReadCookieName)
	if err != nil {
		writeNotFound(c)
		return
	}
	if c.Request.URL.RawQuery != "" {
		query, parseErr := url.ParseQuery(c.Request.URL.RawQuery)
		if parseErr != nil || len(query) != 1 || len(query["after"]) != 1 || query.Get("after") == "" {
			writeNotFound(c)
			return
		}
	}
	var after int64
	if raw := c.Query("after"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeNotFound(c)
			return
		}
	}
	if c.GetHeader("Last-Event-ID") != "" {
		if c.Query("after") != "" {
			writeNotFound(c)
			return
		}
		after, err = strconv.ParseInt(c.GetHeader("Last-Event-ID"), 10, 64)
		if err != nil || after < 0 {
			writeNotFound(c)
			return
		}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		writeNotFound(c)
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher.Flush()
	for {
		if c.Request.Context().Err() != nil {
			return
		}
		ref, lookupErr := g.deps.Runtime.Lookup(c.Request.Context(), c.GetString(hostIDKey))
		if lookupErr != nil || ref.State != host.StateRunning || ref.RuntimeGeneration != session.RuntimeGeneration {
			return
		}
		page, listErr := g.deps.TaskObservation.ListWorkbenchObservationEvents(c.Request.Context(), cookie.Value, c.GetString(hostIDKey), ref.RuntimeGeneration, after, 100)
		if domain.CodeOf(listErr) == domain.CodeEventCursorExpired {
			if writeTaskObservationSSE(c.Writer, "task.snapshot_required", "", struct {
				CurrentSequence int64 `json:"current_sequence"`
			}{page.CurrentSequence}) != nil {
				return
			}
			flusher.Flush()
			return
		}
		if listErr != nil || page.TaskID != session.TaskID {
			return
		}
		for _, event := range page.Items {
			if event.Sequence != after+1 || event.Sequence > page.CurrentSequence {
				return
			}
			if err := writeTaskObservationSSE(c.Writer, "task.event", strconv.FormatInt(event.Sequence, 10), event); err != nil {
				return
			}
			after = event.Sequence
		}
		flusher.Flush()
		if terminalObservationTaskStatus(page.Status) && after >= page.CurrentSequence {
			return
		}
		if _, writeErr := c.Writer.Write([]byte(": keepalive\n\n")); writeErr != nil {
			return
		}
		flusher.Flush()
		timer := time.NewTimer(taskObservationPollDelay)
		select {
		case <-c.Request.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func writeTaskObservationSSE(writer http.ResponseWriter, name, id string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := writer.Write([]byte("id: " + id + "\n")); err != nil {
			return err
		}
	}
	if _, err := writer.Write([]byte("event: " + name + "\n")); err != nil {
		return err
	}
	_, err = writer.Write(append(append([]byte("data: "), encoded...), '\n', '\n'))
	return err
}

func isTaskWorkbenchStaticRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet {
		return false
	}
	requestPath := request.URL.Path
	if requestPath == "/__ch/wb/" || requestPath == "/__ch/wb" {
		query, err := url.ParseQuery(request.URL.RawQuery)
		return err == nil && len(query) == 1 && len(query["task_id"]) == 1 && domain.ValidTaskID(query.Get("task_id"))
	}
	if strings.Contains(requestPath, "//") || path.Clean(requestPath) != requestPath || request.URL.RawQuery != "" || !strings.HasPrefix(requestPath, "/__ch/wb/assets/") {
		return false
	}
	name := filepath.Base(requestPath)
	extension := strings.ToLower(filepath.Ext(name))
	if !hashedAssetName.MatchString(name) {
		return false
	}
	switch extension {
	case ".js", ".css", ".svg", ".png", ".webp", ".woff", ".woff2", ".ttf", ".eot":
		return true
	default:
		return false
	}
}

func terminalObservationTaskStatus(status task.Status) bool {
	return status == task.StatusCompleted || status == task.StatusFailed || status == task.StatusCancelled
}

func taskObservationSessionFrom(c *gin.Context) app.TaskWorkbenchObservationSession {
	value, _ := c.Get(taskObservationSessionKey)
	session, _ := value.(app.TaskWorkbenchObservationSession)
	return session
}

func setTaskObservationResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
}
