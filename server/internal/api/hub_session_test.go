package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/websocket"

	"morde/server/internal/app"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
)

type hubSessionStub struct {
	called    chan struct{}
	principal chan identity.Principal
	message   chan string
	readError chan error
}

func newHubSessionStub() *hubSessionStub {
	return &hubSessionStub{
		called:    make(chan struct{}, 1),
		principal: make(chan identity.Principal, 1),
		message:   make(chan string, 1),
		readError: make(chan error, 1),
	}
}

func (s *hubSessionStub) Serve(ctx context.Context, principal identity.Principal, peer app.HubPeer) error {
	s.called <- struct{}{}
	s.principal <- principal
	message, err := peer.Read(ctx)
	if err != nil {
		s.readError <- err
		return err
	}
	s.message <- string(message)
	return peer.Write(ctx, []byte(`{"type":"pong"}`))
}

func hubTestServer(t *testing.T, service app.HubConnectionService) (*httptest.Server, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/hub", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{
			UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
			TokenID: "t1234567", ClientInstallationID: "i1234567",
		})
		hubConnectionHandler(service, "portal.example.test")(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, "ws" + strings.TrimPrefix(server.URL, "http") + "/api/hub"
}

func dialHub(t *testing.T, endpoint, origin string) (*websocket.Conn, error) {
	t.Helper()
	config, err := websocket.NewConfig(endpoint, origin)
	if err != nil {
		return nil, err
	}
	return websocket.DialConfig(config)
}

func TestHubConnectionHandlerReadsAndWritesTextFrames(t *testing.T) {
	service := newHubSessionStub()
	_, endpoint := hubTestServer(t, service)
	connection, err := dialHub(t, endpoint, "https://portal.example.test")
	if err != nil {
		t.Fatalf("dial Hub: %v", err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Errorf("close Hub connection: %v", err)
		}
	}()
	if err := websocket.Message.Send(connection, `{"type":"ping"}`); err != nil {
		t.Fatalf("send Hub frame: %v", err)
	}
	var response string
	if err := websocket.Message.Receive(connection, &response); err != nil {
		t.Fatalf("receive Hub response: %v", err)
	}
	if response != `{"type":"pong"}` {
		t.Fatalf("Hub response = %q", response)
	}
	principal := <-service.principal
	if principal.ClientInstallationID != "i1234567" || principal.Kind != identity.KindClient {
		t.Fatalf("service principal = %#v", principal)
	}
	select {
	case message := <-service.message:
		if message != `{"type":"ping"}` {
			t.Fatalf("service received %q", message)
		}
	default:
		t.Fatal("Hub service did not receive the text frame")
	}
}

func TestHubConnectionHandlerRejectsQueryAndForeignOrigin(t *testing.T) {
	service := newHubSessionStub()
	_, endpoint := hubTestServer(t, service)
	response, err := http.Get(strings.Replace(endpoint, "ws://", "http://", 1) + "?token=credential")
	if err != nil {
		t.Fatalf("GET Hub with query: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close query response: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("query status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if len(service.called) != 0 {
		t.Fatal("query request reached the Hub service")
	}

	if _, err := dialHub(t, endpoint, "https://attacker.example"); err == nil {
		t.Fatal("foreign Origin completed the WebSocket handshake")
	}
	if len(service.called) != 0 {
		t.Fatal("foreign Origin reached the Hub service")
	}
}

func TestHubConnectionHandlerEnforcesFrameLimit(t *testing.T) {
	service := newHubSessionStub()
	_, endpoint := hubTestServer(t, service)
	connection, err := dialHub(t, endpoint, "https://portal.example.test")
	if err != nil {
		t.Fatalf("dial Hub without browser Origin: %v", err)
	}
	defer func() {
		if err := connection.Close(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("close Hub connection: %v", err)
		}
	}()
	if err := websocket.Message.Send(connection, strings.Repeat("x", hub.MaxFrameBytes+1)); err != nil {
		t.Fatalf("send oversized frame: %v", err)
	}
	select {
	case <-service.called:
	case <-time.After(time.Second):
		t.Fatal("Hub service did not process the connection")
	}
	select {
	case err := <-service.readError:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("oversized frame error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized frame was not rejected")
	}
}
