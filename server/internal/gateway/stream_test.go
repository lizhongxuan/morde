package gateway

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func TestSSEStreamsPastServerWriteTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message\ndata: first\n\n")
		flusher.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "event: message\ndata: second\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 200 * time.Millisecond
	server.Config.WriteTimeout = 150 * time.Millisecond
	server.Start()
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = hostAuthority()
	request.Header.Set("Cookie", hostCookieName+"="+cookie)
	request.Header.Set("Accept", "text/event-stream")
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "data: first") || !strings.Contains(string(body), "data: second") || time.Since(started) < 250*time.Millisecond {
		t.Fatalf("SSE stream was cut short: status=%d duration=%s body=%q", response.StatusCode, time.Since(started), body)
	}
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.acquired) != 1 || leases.acquired[0].Kind != app.HostActivityLeaseKindGateway || len(leases.released) != 1 {
		t.Fatalf("SSE activity lease acquired=%#v released=%#v", leases.acquired, leases.released)
	}
}

func TestSSERenewalFailureClosesStreamAndReleasesLease(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: first\ndata: ready\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	g.activityRenewInterval = 500 * time.Millisecond
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	leases.renewErr = fmt.Errorf("simulated lease renewal failure")
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	server := httptest.NewServer(handler)
	defer server.Close()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = hostAuthority()
	request.Header.Set("Cookie", hostCookieName+"="+cookie)
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: first\n" {
		t.Fatalf("first SSE event = %q, %v", line, err)
	}
	remaining, _ := io.ReadAll(reader)
	if time.Since(started) >= 2500*time.Millisecond {
		t.Fatal("SSE stream did not close promptly after lease renewal failed")
	}
	if !strings.Contains(line+string(remaining), "data: ready") {
		t.Fatalf("SSE stream ended before its initial event: %q%q", line, remaining)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("lease renewal failure did not cancel the upstream SSE request")
	}
	assertLeaseRenewalAndRelease(t, leases)
}

func TestSlowUploadStreamsPastServerReadTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 150 * time.Millisecond
	server.Config.WriteTimeout = 150 * time.Millisecond
	server.Start()
	defer server.Close()

	reader, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/upload", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 4
	request.Host = hostAuthority()
	request.Header.Set("Cookie", hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour))
	request.Header.Set("Origin", "https://"+hostAuthority())
	writeErr := make(chan error, 1)
	go func() {
		for _, char := range []byte("slow") {
			if _, err := writer.Write([]byte{char}); err != nil {
				writeErr <- err
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		writeErr <- writer.Close()
	}()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "slow" {
		t.Fatalf("slow upload response = %d %q", response.StatusCode, body)
	}
}

func TestWebSocketEchoTracksActivityAndClosesOnShutdown(t *testing.T) {
	upstreamQuery := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamQuery <- r.URL.RawQuery
		websocketEcho(w, r)
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 2 * time.Second
	server.Config.WriteTimeout = 2 * time.Second
	server.Start()
	defer server.Close()
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err = fmt.Fprintf(connection,
		"GET /v1/shell/ws?session_id=terminal-session HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	select {
	case query := <-upstreamQuery:
		if query != "session_id=terminal-session" {
			t.Fatalf("upstream WebSocket query = %q, want session ID preserved", query)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream WebSocket did not receive the request")
	}
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	leases.mu.Lock()
	if len(leases.acquired) != 1 || leases.acquired[0].Kind != app.HostActivityLeaseKindGateway || len(leases.released) != 0 {
		leases.mu.Unlock()
		t.Fatalf("active WebSocket lease acquired=%#v released=%#v", leases.acquired, leases.released)
	}
	leases.mu.Unlock()
	runtime.mu.Lock()
	activityBefore := runtime.markActive
	runtime.mu.Unlock()
	g.activityMu.Lock()
	g.activityAt[testHostID] = time.Now().Add(-activityMinimumInterval - time.Second)
	g.activityMu.Unlock()
	time.Sleep(10 * time.Second)
	writeMaskedTextFrame(t, connection, "ping")
	if err := readTextFrame(reader, "ping"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	activityAfter := runtime.markActive
	runtime.mu.Unlock()
	if activityAfter <= activityBefore {
		t.Fatal("websocket payload did not mark the host active")
	}
	if got := websocketGauge(g); got != 1 {
		t.Fatalf("active websocket gauge = %v, want 1", got)
	}
	if activityCount(runtime) == 0 {
		t.Fatal("websocket traffic did not mark the host active")
	}
	_ = connection.Close()
	deadline := time.Now().Add(time.Second)
	for websocketGauge(g) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := websocketGauge(g); got != 0 {
		t.Fatalf("closed websocket gauge = %v, want 0", got)
	}
	if trackedConnectionCount(g) != 0 {
		t.Fatalf("closed websocket remains registered: %d", trackedConnectionCount(g))
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 {
		t.Fatalf("closed WebSocket did not release its activity lease: %#v", leases.released)
	}
}

func TestWebSocketRenewalFailureClosesConnectionAndReleasesLease(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	g.activityRenewInterval = 500 * time.Millisecond
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	leases.renewErr = fmt.Errorf("simulated lease renewal failure")
	server := httptest.NewServer(handler)
	defer server.Close()
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err = fmt.Fprintf(connection,
		"GET /v1/shell/ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("websocket remained open after lease renewal failed")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("websocket did not close after lease renewal failed")
		}
	}
	assertLeaseRenewalAndRelease(t, leases)
}

func TestRevokingMemberClosesExistingDesktopWebSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, _, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	g.memberAuthorizationCheckInterval = 20 * time.Millisecond
	setMemberPermissionGrants(g, host.PermissionDesktopView, host.PermissionDesktopControl)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 2 * time.Second
	server.Config.WriteTimeout = 2 * time.Second
	server.Start()
	defer server.Close()
	cookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err = fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	setMemberPermissionGrants(g, host.PermissionDesktopView)
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("member desktop websocket remained open after access was revoked")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("member desktop websocket was not closed after access was revoked")
		}
	}
	sharing := g.deps.HostSharing.(*testHostSharingService)
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	if sharing.calls < 2 {
		t.Fatalf("authorization calls = %d, want an initial check and a revoke check", sharing.calls)
	}
}

func TestRevokingClientClosesExistingDesktopWebSocketWithinFiveSeconds(t *testing.T) {
	if hostMemberAuthorizationCheckInterval+hostAuthorizationCheckTimeout >= 5*time.Second {
		t.Fatal("authorization polling and timeout exceed the five-second revocation budget")
	}
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, _, verifier, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	cookie := g.signClientCookie(testHostID, testUserID, "i1234567", time.Now().Add(time.Hour))
	_, err = fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("client websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	verifier.mu.Lock()
	verifier.clientRevoked = true
	verifier.mu.Unlock()
	revokedAt := time.Now()
	if err := connection.SetReadDeadline(revokedAt.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("revoked client websocket remained open")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("revoked client websocket was not closed within five seconds")
		}
	}
	if elapsed := time.Since(revokedAt); elapsed >= 5*time.Second {
		t.Fatalf("revoked client websocket closed after %s", elapsed)
	}
}

func TestDisablingOwnerClosesExistingDesktopWebSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, _, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	g.memberAuthorizationCheckInterval = 20 * time.Millisecond
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 2 * time.Second
	server.Config.WriteTimeout = 2 * time.Second
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	ownerCookie := hostCookie(g, testHostID, testUserID, time.Hour)
	_, err = fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, ownerCookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("owner websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	sharing := g.deps.HostSharing.(*testHostSharingService)
	sharing.mu.Lock()
	sharing.ownerDenied = true
	sharing.mu.Unlock()
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("owner desktop websocket remained open after account disable")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("owner desktop websocket was not closed after account disable")
		}
	}
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	if sharing.calls < 2 {
		t.Fatalf("owner authorization calls = %d, want an initial check and a disable check", sharing.calls)
	}
}

func TestRevokingMemberTerminalPermissionClosesExistingShellWebSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	g.memberAuthorizationCheckInterval = 20 * time.Millisecond
	setMemberPermissionGrants(g, host.PermissionTerminalControl)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 2 * time.Second
	server.Config.WriteTimeout = 2 * time.Second
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	memberCookie := hostCookie(g, testHostID, testMemberID, time.Hour)
	_, err = fmt.Fprintf(connection,
		"GET /v1/shell/ws?session_id=terminal-session HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, memberCookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("terminal websocket handshake = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	setMemberPermissionGrants(g)
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("member terminal websocket remained open after terminal:control was revoked")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("member terminal websocket was not closed after terminal:control was revoked")
		}
	}
}

func assertLeaseRenewalAndRelease(t *testing.T, leases *testActivityLeaseStore) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		leases.mu.Lock()
		acquired := append([]app.HostActivityLease(nil), leases.acquired...)
		renewed := append([]app.HostActivityLease(nil), leases.renewed...)
		released := append([]app.HostActivityLease(nil), leases.released...)
		leases.mu.Unlock()
		if len(renewed) == 1 && len(released) == 1 {
			if len(acquired) != 1 || released[0].LeaseID != acquired[0].LeaseID || renewed[0].LeaseID != acquired[0].LeaseID {
				t.Fatalf("activity lease lifecycle does not match: acquired=%#v renewed=%#v released=%#v", acquired, renewed, released)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	t.Fatalf("activity lease was not renewed and released: acquired=%#v renewed=%#v released=%#v", leases.acquired, leases.renewed, leases.released)
}

func TestWebSocketUpgradeIsRejectedDuringControlTransition(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(websocketEcho))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = "taking_over"
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/shell/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = hostAuthority()
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
	request.Header.Set("Origin", "https://"+hostAuthority())
	request.AddCookie(&http.Cookie{Name: hostCookieName, Value: hostCookie(g, testHostID, testUserID, time.Hour)})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("websocket transition response = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if runtime.resolveCall != 0 {
		t.Fatalf("resolved %d upstreams during handoff", runtime.resolveCall)
	}
}

func websocketEcho(w http.ResponseWriter, r *http.Request) {
	connection, buffer, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
	_, _ = fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if buffer.Flush() != nil {
		_ = connection.Close()
		return
	}
	defer func() { _ = connection.Close() }()
	for {
		var header [2]byte
		if _, err := io.ReadFull(buffer, header[:]); err != nil {
			return
		}
		length := int(header[1] & 0x7f)
		if length == 126 {
			var extended [2]byte
			if _, err := io.ReadFull(buffer, extended[:]); err != nil {
				return
			}
			length = int(extended[0])<<8 | int(extended[1])
		}
		if length > 1024 || header[1]&0x80 == 0 {
			return
		}
		var mask [4]byte
		if _, err := io.ReadFull(buffer, mask[:]); err != nil {
			return
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(buffer, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%len(mask)]
		}
		response := append([]byte{0x81, byte(len(payload))}, payload...)
		if _, err := connection.Write(response); err != nil {
			return
		}
	}
}

func websocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash[:])
}

func writeMaskedTextFrame(t *testing.T, connection net.Conn, value string) {
	t.Helper()
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(value)), mask[0], mask[1], mask[2], mask[3]}
	for i, char := range []byte(value) {
		frame = append(frame, char^mask[i%len(mask)])
	}
	if _, err := connection.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func readTextFrame(reader *bufio.Reader, want string) error {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	if header[0] != 0x81 || header[1]&0x80 != 0 {
		return fmt.Errorf("unexpected websocket frame header %v", header)
	}
	payload := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return err
	}
	if string(payload) != want {
		return fmt.Errorf("websocket payload %q, want %q", payload, want)
	}
	return nil
}

func websocketGauge(g *gateway) float64 {
	metric := &io_prometheus_client.Metric{}
	_ = g.wsGauge.Write(metric)
	if metric.Gauge == nil {
		return 0
	}
	return metric.Gauge.GetValue()
}

func trackedConnectionCount(g *gateway) int {
	g.wsMu.Lock()
	defer g.wsMu.Unlock()
	return len(g.wsBodies)
}

func activityCount(runtime *testRuntime) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.markActive
}

func TestRetryHelperAndStatusMetrics(t *testing.T) {
	handler, g, runtime, _, _ := newTestDeps(t)
	runtime.ref.State = "deleting"
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	response := request(handler, http.MethodGet, hostAuthority(), "/v1", nil, hostCookieName+"="+cookie, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("deleting host response = %d", response.Code)
	}
	if upstreamErrorKind(&timeoutError{}) != "timeout" || upstreamErrorKind(nil) != "unreachable" {
		t.Fatal("upstream error kind mapping failed")
	}
	if domainErr := upstreamError(fmt.Errorf("upstream: %w", &timeoutError{})); domain.CodeOf(domainErr) != domain.CodeUpstreamTimeout {
		t.Fatalf("upstream timeout code = %s", domain.CodeOf(domainErr))
	}
	if clientIP(&http.Request{RemoteAddr: "invalid-address"}) != "invalid-address" {
		t.Fatal("client IP fallback failed")
	}
	if _, err := urlForHost(app.Upstream{BaseURL: "https://example.test"}, "//evil.test/path"); err == nil {
		t.Fatal("accepted an absolute upstream request path")
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestControlRequestsKeepServerWriteTimeout(t *testing.T) {
	handler, g, _, _, hosts := newTestDeps(t)
	hosts.getDelay = 150 * time.Millisecond
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/__ch/api/host", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = hostAuthority()
	req.Header.Set("Cookie", hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour))
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(req)
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err == nil && response.StatusCode == http.StatusOK {
		t.Fatal("control-plane request bypassed the server write timeout")
	}
}
