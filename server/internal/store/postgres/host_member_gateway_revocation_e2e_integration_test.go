package postgres

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/gateway"
)

type hostMemberRevocationAudit struct{ store *Store }

func (w hostMemberRevocationAudit) Write(ctx context.Context, entry audit.Entry) error {
	return w.store.WriteAudit(ctx, entry)
}

type hostMemberRevocationDefaults struct{}

func (hostMemberRevocationDefaults) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return host.Host{}, false, nil
}

type hostMemberRevocationAPIHost struct{ app.HostService }
type hostMemberRevocationAPIAuth struct{ api.AuthService }
type hostMemberRevocationGatewayHost struct{ app.HostService }
type hostMemberRevocationGatewayControl struct{ gateway.ControlService }

type hostMemberRevocationTLS struct{}

func (hostMemberRevocationTLS) AuthorizeTLS(context.Context, string) bool { return true }

type hostMemberRevocationGatewayRuntime struct {
	ref      host.Ref
	upstream app.Upstream
}

func (r hostMemberRevocationGatewayRuntime) Lookup(_ context.Context, hostID string) (host.Ref, error) {
	if hostID != r.ref.ID {
		return host.Ref{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return r.ref, nil
}

func (r hostMemberRevocationGatewayRuntime) Resolve(context.Context, string, int) (app.Upstream, error) {
	return r.upstream, nil
}

func (hostMemberRevocationGatewayRuntime) InvalidateUpstream(string) {}
func (hostMemberRevocationGatewayRuntime) MarkActive(string)         {}

type hostMemberRevocationTerminalAIO struct{}

func (hostMemberRevocationTerminalAIO) TerminalSessionID(context.Context, app.Upstream) (string, error) {
	return "", errors.New("terminal AIO is unused in this test")
}

func TestHostMemberRevocationAPIClosesExistingGatewayWebSocket(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	member := createTestUser(t, store, 2, 10_000, 20_000)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	hostValue, err := store.TransitionHost(ctx, hostValue.ID, host.StateCreating, host.StateRunning, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostMember(ctx, host.Member{
		HostID: hostValue.ID, OwnerID: owner.ID, UserID: member.ID,
		Permissions: []host.Permission{host.PermissionDesktopView, host.PermissionDesktopControl},
	}); err != nil {
		t.Fatal(err)
	}
	auditor := hostMemberRevocationAudit{store: store}
	sharing, err := app.NewHostSharingService(store, store, store, store, auditor,
		"https", "example.test", time.Minute, zap.NewNop(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditor, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, hostMemberRevocationDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	ownerSession := hostMemberRevocationCreateSession(t, store, owner.ID)
	memberSession := hostMemberRevocationCreateSession(t, store, member.ID)

	apiHandler, err := api.New(api.Deps{
		Hosts: hostMemberRevocationAPIHost{}, HostSharing: sharing,
		TLSAuthorizer: hostMemberRevocationTLS{}, Verifier: credentials,
		Auth: hostMemberRevocationAPIAuth{},
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
		APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20,
		CookieSecure: true, SessionTTL: time.Hour, HostScheme: "https",
		PortalDomain: "portal.example.test", HostSuffix: "example.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "websocket upgrade unavailable", http.StatusInternalServerError)
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		acceptDigest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		if _, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(acceptDigest[:])); err != nil {
			_ = connection.Close()
			return
		}
		if err := buffered.Flush(); err != nil {
			_ = connection.Close()
			return
		}
		go func() {
			_, _ = io.Copy(io.Discard, connection)
			_ = connection.Close()
		}()
	}))
	defer echo.Close()

	keyPath := filepath.Join(t.TempDir(), "gateway-cookie.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	workbench := t.TempDir()
	if err := os.WriteFile(filepath.Join(workbench, "index.html"), []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := host.Ref{
		ID: hostValue.ID, OwnerID: owner.ID, State: host.StateRunning,
		RuntimeGeneration: hostValue.RuntimeGeneration, ControlVersion: hostValue.ControlVersion,
		ControlState: hostValue.ControlState,
	}
	gatewayHandler, err := gateway.New(gateway.Deps{
		Config: config.Gateway{
			WorkbenchDir: workbench, UpstreamCacheTTL: time.Minute, CredentialCacheTTL: time.Minute,
			PreviewPortMin: 1024, PreviewPortDeny: []int{8080}, MaxBodyBytes: 1 << 20,
		},
		Domains:  config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: "example.test", CookieSecure: true},
		Security: config.Security{CookieKeyFile: keyPath, HostCookieTTL: time.Hour},
		Hosts:    hostMemberRevocationGatewayHost{}, HostSharing: sharing,
		Control:        hostMemberRevocationGatewayControl{},
		Runtime:        hostMemberRevocationGatewayRuntime{ref: ref, upstream: app.Upstream{BaseURL: echo.URL}},
		ActivityLeases: store, InputPermits: store, TerminalSessions: store,
		TerminalAIO: hostMemberRevocationTerminalAIO{}, Auth: credentials,
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ticketRequest := httptest.NewRequest(http.MethodPost, "/api/hosts/"+hostValue.ID+"/observation-ticket", nil)
	ticketRequest.Host = "portal.example.test"
	ticketRequest.Header.Set("Origin", "https://portal.example.test")
	ticketRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: memberSession})
	ticketResponse := httptest.NewRecorder()
	apiHandler.ServeHTTP(ticketResponse, ticketRequest)
	if ticketResponse.Code != http.StatusOK {
		t.Fatalf("issue member observation ticket = %d: %s", ticketResponse.Code, ticketResponse.Body.String())
	}
	var ticketBody struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(ticketResponse.Body.Bytes(), &ticketBody); err != nil {
		t.Fatal(err)
	}
	enterURL, err := url.Parse(ticketBody.URL)
	if err != nil || enterURL.RawQuery == "" {
		t.Fatalf("observation ticket URL = %q, err=%v", ticketBody.URL, err)
	}

	parsedHostURL, err := url.Parse(ticketBody.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The share URL names the Host; use that authority for both ticket exchange and the upgraded stream.
	requestAuthority := parsedHostURL.Host
	if !strings.HasPrefix(requestAuthority, hostValue.ID+".") {
		t.Fatalf("ticket host = %q, want Host %q", requestAuthority, hostValue.ID)
	}

	enterRequest := httptest.NewRequest(http.MethodGet, parsedHostURL.RequestURI(), nil)
	enterRequest.Host = requestAuthority
	enterResponse := httptest.NewRecorder()
	gatewayHandler.ServeHTTP(enterResponse, enterRequest)
	if enterResponse.Code != http.StatusFound {
		t.Fatalf("consume observation ticket = %d: %s", enterResponse.Code, enterResponse.Body.String())
	}
	var hostCookie *http.Cookie
	for _, candidate := range enterResponse.Result().Cookies() {
		if candidate.Name != "" && candidate.Value != "" {
			hostCookie = candidate
			break
		}
	}
	if hostCookie == nil {
		t.Fatal("Gateway did not issue a Host cookie")
	}

	gatewayServer := httptest.NewServer(gatewayHandler)
	defer gatewayServer.Close()
	serverAddress := strings.TrimPrefix(gatewayServer.URL, "http://")
	connection, err := net.Dial("tcp", serverAddress)
	if err != nil {
		t.Fatalf("connect member desktop websocket: %v", err)
	}
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	if err := connection.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		t.Fatal(err)
	}
	websocketKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	if _, err := fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		requestAuthority, websocketKey, requestAuthority, hostCookie.Name, hostCookie.Value); err != nil {
		t.Fatal(err)
	}
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("member websocket handshake = %q, %v", status, err)
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

	revokeRequest := httptest.NewRequest(http.MethodDelete, "/api/hosts/"+hostValue.ID+"/members/"+member.ID, nil)
	revokeRequest.Host = "portal.example.test"
	revokeRequest.Header.Set("Origin", "https://portal.example.test")
	revokeRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: ownerSession})
	revokeResponse := httptest.NewRecorder()
	apiHandler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke Host member = %d: %s", revokeResponse.Code, revokeResponse.Body.String())
	}

	revokedAt := time.Now()
	if err := connection.SetReadDeadline(revokedAt.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("Gateway websocket remained open after API member revocation")
	}
	if elapsed := time.Since(revokedAt); elapsed >= 5*time.Second {
		t.Fatalf("Gateway websocket closed after %s, want within five seconds", elapsed)
	}
	access, err := store.GetHostActorAccess(ctx, hostValue.ID, member.ID)
	if err != nil || len(access.Permissions) != 0 {
		t.Fatalf("persisted member access after API revoke = %#v, %v", access, err)
	}
}

func hostMemberRevocationCreateSession(t *testing.T, store *Store, userID string) string {
	t.Helper()
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	credential, secretHash, err := identity.NewCredential(identity.KindSession, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(context.Background(), identity.Token{
		ID: tokenID, OwnerID: userID, Kind: identity.KindSession,
		SecretHash: secretHash, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return credential
}
