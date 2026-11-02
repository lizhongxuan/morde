package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"

	"morde/server/internal/domain/identity"
)

func TestRegisteredHubRouteAuthenticatesClientInstallation(t *testing.T) {
	deps := newServices().deps()
	service := newHubSessionStub()
	deps.Hub = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()

	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/hub"
	config, err := websocket.NewConfig(endpoint, "https://portal.example.test")
	if err != nil {
		t.Fatal(err)
	}
	config.Header = make(http.Header)
	config.Header.Set("Authorization", "Bearer "+credential)
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatalf("dial registered Hub route: %v", err)
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
	if principal.Kind != identity.KindClient || principal.UserID != testUserID || principal.ClientInstallationID != "i1234567" {
		t.Fatalf("Hub principal = %#v", principal)
	}
}
