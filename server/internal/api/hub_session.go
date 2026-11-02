package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/websocket"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
)

const (
	hubReadTimeout  = 90 * time.Second
	hubWriteTimeout = 10 * time.Second
)

func hubConnectionHandler(service app.HubConnectionService, portalDomain string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok || principal.Kind != identity.KindClient || principal.Role != identity.RoleUser ||
			!domain.ValidUserID(principal.UserID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) {
			unauthenticated(c)
			return
		}
		if c.Request.URL.RawQuery != "" {
			writeError(c, domain.InvalidArgument("query", "Hub does not accept query parameters"))
			return
		}
		// Clear the API server deadlines only after authentication; the connection owns its limits.
		controller := http.NewResponseController(c.Writer)
		if err := controller.SetReadDeadline(time.Time{}); err != nil {
			writeError(c, domain.NewError(domain.CodeInternal, "Hub connection is unavailable"))
			return
		}
		if err := controller.SetWriteDeadline(time.Time{}); err != nil {
			writeError(c, domain.NewError(domain.CodeInternal, "Hub connection is unavailable"))
			return
		}

		websocket.Server{
			Handshake: func(config *websocket.Config, request *http.Request) error {
				origin := request.Header.Get("Origin")
				if origin == "" {
					return nil
				}
				parsed, err := url.Parse(origin)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
					parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
					!strings.EqualFold(parsed.Host, portalDomain) {
					return errors.New("origin is not allowed")
				}
				config.Origin = parsed
				return nil
			},
			Handler: websocket.Handler(func(connection *websocket.Conn) {
				connection.MaxPayloadBytes = hub.MaxFrameBytes
				_ = service.Serve(c.Request.Context(), principal, hubSocketPeer{connection: connection})
			}),
		}.ServeHTTP(c.Writer, c.Request)
	}
}

type hubSocketPeer struct {
	connection *websocket.Conn
}

func (p hubSocketPeer) Read(ctx context.Context) ([]byte, error) {
	deadline := time.Now().Add(hubReadTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := p.connection.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	var message string
	if err := websocket.Message.Receive(p.connection, &message); err != nil {
		return nil, err
	}
	return []byte(message), nil
}

func (p hubSocketPeer) Write(ctx context.Context, message []byte) error {
	if len(message) > hub.MaxFrameBytes || !utf8.Valid(message) {
		return errors.New("hub frame exceeds its limit")
	}
	deadline := time.Now().Add(hubWriteTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := p.connection.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return websocket.Message.Send(p.connection, string(message))
}

func (p hubSocketPeer) Close(_ int, _ string) error {
	return p.connection.Close()
}
