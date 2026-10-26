package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/httpserver"
)

const (
	terminalSessionCreateTimeout  = 30 * time.Second
	terminalSessionUnknownTimeout = 2 * time.Second
	terminalSessionBodyLimit      = 1024
)

type terminalSessionAIO interface {
	TerminalSessionID(context.Context, app.Upstream) (string, error)
}

type terminalSessionResponse struct {
	State string `json:"state"`
	URL   string `json:"url"`
}

type terminalSessionCreateRequest struct {
	ConfirmNew bool `json:"confirm_new"`
}

func (g *gateway) ensureTerminalSession(c *gin.Context) {
	if c.Request.URL.RawQuery != "" || !terminalSessionRequestBodyEmpty(c) {
		if !c.IsAborted() {
			httpserver.WriteError(c, domain.InvalidArgument("body", "terminal session ensure does not accept a body or query"))
		}
		return
	}
	g.manageTerminalSession(c, false)
}

func (g *gateway) createTerminalSession(c *gin.Context) {
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "terminal session create does not accept query parameters"))
		return
	}
	var request terminalSessionCreateRequest
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpserver.WriteError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, terminalSessionBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeTerminalSessionDecodeError(c, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		writeTerminalSessionDecodeError(c, err)
		return
	}
	if !request.ConfirmNew {
		httpserver.WriteError(c, domain.InvalidArgument("confirm_new", "confirm_new must be true"))
		return
	}
	g.manageTerminalSession(c, true)
}

func writeTerminalSessionDecodeError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpserver.WriteError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
		return
	}
	httpserver.WriteError(c, domain.InvalidArgument("body", "request body is invalid"))
}

func terminalSessionRequestBodyEmpty(c *gin.Context) bool {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1))
	if err != nil || len(body) != 0 {
		httpserver.WriteError(c, domain.InvalidArgument("body", "terminal session ensure does not accept a body"))
		return false
	}
	return true
}

func (g *gateway) manageTerminalSession(c *gin.Context, replace bool) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	ref := hostRefFrom(c)
	if !g.requireRunning(c, ref) {
		return
	}
	if !g.requireStableControl(c, ref) {
		return
	}
	if ref.ControlState != host.ControlUser {
		httpserver.WriteError(c, domain.NewError(domain.CodeInvalidState, "terminal requires user control"))
		return
	}

	guard, leaseCtx, err := g.acquireHostActivityLease(c.Request.Context(), ref, app.HostActivityLeaseKindGateway, g.memberAuthorizationCheck(c))
	if err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			err = domain.NewError(domain.CodeHostNotReady, "host control or runtime state changed")
		}
		httpserver.WriteError(c, err)
		return
	}
	defer func() {
		if err := guard.Close(); err != nil {
			g.deps.Logger.Warn("gateway activity lease release failed", zap.String("host_id", ref.ID), zap.String("kind", app.HostActivityLeaseKindGateway))
		}
	}()
	c.Request = c.Request.WithContext(leaseCtx)
	g.markActive(ref.ID)

	attemptID, err := domain.NewOperationID()
	if err != nil {
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "could not create terminal session reservation"))
		return
	}
	reservation, created, err := g.deps.TerminalSessions.EnsureTerminalSession(leaseCtx, app.TerminalSession{
		HostID: ref.ID, OwnerID: ref.OwnerID, ActorID: principalFrom(c).UserID,
		RuntimeGeneration: ref.RuntimeGeneration, ControlVersion: ref.ControlVersion,
		AttemptID: attemptID, State: app.TerminalSessionCreating,
	}, replace)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	if !created {
		writeTerminalSession(c, reservation)
		return
	}

	createCtx, cancel := context.WithTimeout(leaseCtx, terminalSessionCreateTimeout)
	defer cancel()
	upstream, err := g.deps.Runtime.Resolve(createCtx, ref.ID, 8080)
	if err != nil {
		g.markTerminalUnknown(reservation)
		writeUnknownTerminalSession(c)
		return
	}
	aioSessionID, err := g.deps.TerminalAIO.TerminalSessionID(createCtx, upstream)
	if err != nil || createCtx.Err() != nil {
		g.markTerminalUnknown(reservation)
		writeUnknownTerminalSession(c)
		return
	}
	completed, err := g.deps.TerminalSessions.CompleteTerminalSession(createCtx, reservation, aioSessionID)
	if err != nil || !completed {
		g.markTerminalUnknown(reservation)
		writeUnknownTerminalSession(c)
		return
	}
	reservation.State = app.TerminalSessionReady
	reservation.AIOSessionID = aioSessionID
	writeTerminalSession(c, reservation)
}

func (g *gateway) markTerminalUnknown(reservation app.TerminalSession) {
	ctx, cancel := context.WithTimeout(context.Background(), terminalSessionUnknownTimeout)
	defer cancel()
	if err := g.deps.TerminalSessions.MarkTerminalSessionUnknown(ctx, reservation); err != nil {
		g.deps.Logger.Warn("terminal session state could not be reconciled", zap.String("host_id", reservation.HostID))
	}
}

func writeTerminalSession(c *gin.Context, value app.TerminalSession) {
	switch value.State {
	case app.TerminalSessionReady:
		query := url.Values{"session_id": []string{value.AIOSessionID}}
		c.JSON(http.StatusOK, terminalSessionResponse{State: app.TerminalSessionReady, URL: "/terminal?" + query.Encode()})
	case app.TerminalSessionInterrupted:
		c.JSON(http.StatusOK, terminalSessionResponse{State: app.TerminalSessionInterrupted, URL: ""})
	case app.TerminalSessionCreating:
		c.JSON(http.StatusOK, terminalSessionResponse{State: app.TerminalSessionCreating, URL: ""})
	case app.TerminalSessionUnknown:
		writeUnknownTerminalSession(c)
	default:
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "internal error"))
	}
}

func writeUnknownTerminalSession(c *gin.Context) {
	c.JSON(http.StatusOK, terminalSessionResponse{State: app.TerminalSessionUnknown, URL: ""})
}

func (g *gateway) markMissingTerminalSession(c *gin.Context, request *http.Request) {
	if request == nil || request.Method != http.MethodGet || request.URL.Path != "/v1/shell/ws" ||
		!strings.EqualFold(request.Header.Get("Upgrade"), "websocket") || !hasHeaderToken(request.Header.Get("Connection"), "upgrade") {
		return
	}
	ids := request.URL.Query()["session_id"]
	if len(ids) != 1 || ids[0] == "" {
		return
	}
	ref := hostRefFrom(c)
	principal := principalFrom(c)
	ctx, cancel := context.WithTimeout(context.Background(), terminalSessionUnknownTimeout)
	defer cancel()
	_, err := g.deps.TerminalSessions.MarkTerminalSessionInterrupted(ctx, ref.ID, principal.UserID, ref.RuntimeGeneration, ref.ControlVersion, ids[0])
	if err != nil {
		g.deps.Logger.Warn("terminal session state could not be reconciled", zap.String("host_id", ref.ID))
	}
}
