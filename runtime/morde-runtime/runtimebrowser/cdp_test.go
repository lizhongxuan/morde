package runtimebrowser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestSameCDPAuthority(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		port       string
		webSocket  string
		wantAccept bool
	}{
		{name: "exact endpoint", host: "127.0.0.1", port: "9222", webSocket: "ws://127.0.0.1:9222/devtools/browser/id", wantAccept: true},
		{name: "localhost alias", host: "127.0.0.1", port: "9222", webSocket: "ws://localhost:9222/devtools/browser/id", wantAccept: true},
		{name: "canonical ipv6 alias", host: "localhost", port: "9222", webSocket: "ws://[::1]:9222/devtools/browser/id", wantAccept: true},
		{name: "wrong port", host: "127.0.0.1", port: "9222", webSocket: "ws://127.0.0.1:9223/devtools/browser/id"},
		{name: "different loopback", host: "127.0.0.1", port: "9222", webSocket: "ws://127.0.0.2:9222/devtools/browser/id"},
		{name: "non-loopback", host: "127.0.0.1", port: "9222", webSocket: "ws://192.0.2.10:9222/devtools/browser/id"},
		{name: "wrong scheme", host: "127.0.0.1", port: "9222", webSocket: "http://127.0.0.1:9222/devtools/browser/id"},
		{name: "credentials", host: "127.0.0.1", port: "9222", webSocket: "ws://user@127.0.0.1:9222/devtools/browser/id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.webSocket)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameCDPAuthority(test.host, test.port, parsed); got != test.wantAccept {
				t.Fatalf("sameCDPAuthority(%q, %q, %q) = %v, want %v", test.host, test.port, test.webSocket, got, test.wantAccept)
			}
		})
	}
}

func TestEditableRoleAllowlist(t *testing.T) {
	for _, role := range []string{"textbox", "searchbox", "combobox"} {
		if !validEditableRole(role) {
			t.Errorf("editable role %q was rejected", role)
		}
	}
	for _, role := range []string{"button", "link", "checkbox", "radio", "listbox", "option", "menuitem", "tab", "switch"} {
		if validEditableRole(role) {
			t.Errorf("non-editable role %q was accepted", role)
		}
	}
}

func TestConnectUntilReadyRetriesStartupRaceAndHonorsDeadline(t *testing.T) {
	state := &fakeBrowserState{
		contexts:  make(map[string]uint64),
		targets:   map[string]targetInfo{"user-tab-1": {TargetID: "user-tab-1", Type: "page"}},
		available: true,
	}
	connector := &retryingBrowserConnector{delegate: &fakeConnector{state: state}, failures: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := connectUntilReady(ctx, connector, time.Millisecond)
	if err != nil {
		t.Fatalf("connectUntilReady() error = %v", err)
	}
	if connector.attempts != 3 {
		t.Fatalf("connection attempts = %d, want 3", connector.attempts)
	}
	_ = connection.Close()

	state.setUnavailable(true)
	connector = &retryingBrowserConnector{delegate: &fakeConnector{state: state}}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if connection, err := connectUntilReady(ctx, connector, time.Millisecond); err == nil {
		if connection != nil {
			_ = connection.Close()
		}
		t.Fatal("connectUntilReady() succeeded while CDP remained unavailable")
	}
	if connector.attempts < 2 {
		t.Fatalf("connection attempts = %d, want retries before deadline", connector.attempts)
	}
}

type retryingBrowserConnector struct {
	delegate browserConnector
	failures int
	attempts int
}

func (c *retryingBrowserConnector) Connect(ctx context.Context) (browserConnection, error) {
	c.attempts++
	if c.attempts <= c.failures {
		return nil, errors.New("CDP startup is not ready")
	}
	return c.delegate.Connect(ctx)
}

func TestDialWebSocketHandshakeHonorsContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(io.Discard, connection)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	connection, err := dialWebSocket(ctx, "ws://"+listener.Addr().String()+"/devtools/browser/test")
	if connection != nil {
		_ = connection.close()
	}
	if err == nil {
		t.Fatal("dialWebSocket() unexpectedly succeeded against a stalled handshake")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("dialWebSocket() ignored context deadline for %s", elapsed)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled WebSocket handshake did not close its socket")
	}
}
