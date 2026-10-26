package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const (
	testHostID   = "h1234567"
	testUserID   = "u1234567"
	testMemberID = "u7654321"
	testSuffix   = "example.test"
)

type testHostService struct {
	app.HostService
	value      host.Host
	ticket     app.EnterInfo
	getDelay   time.Duration
	getErr     error
	renewErr   error
	ticketErr  error
	issueCalls int
}

type testHostSharingService struct {
	app.HostSharingService
	mu          sync.Mutex
	allowed     bool
	ownerDenied bool
	permissions map[host.Permission]bool
	calls       int
	requested   []host.Permission
}

func (s *testHostSharingService) Authorize(_ context.Context, _ string, actorID string, permission host.Permission) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.requested = append(s.requested, permission)
	if actorID == testUserID {
		if s.ownerDenied {
			return false, domain.NewError(domain.CodeNotFound, "host not found")
		}
		return true, nil
	}
	if s.permissions != nil {
		if s.permissions[permission] || permission == host.PermissionDesktopView && s.permissions[host.PermissionDesktopControl] {
			return false, nil
		}
		return false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if s.allowed {
		return false, nil
	}
	return false, domain.NewError(domain.CodeNotFound, "host not found")
}

func (s *testHostService) Get(context.Context, identity.Principal, string) (host.Host, error) {
	time.Sleep(s.getDelay)
	return s.value, s.getErr
}

func (s *testHostService) Renew(context.Context, identity.Principal, string) (host.Host, error) {
	return s.value, s.renewErr
}

func (s *testHostService) IssueTicket(context.Context, identity.Principal, string) (app.EnterInfo, error) {
	s.issueCalls++
	return s.ticket, s.ticketErr
}

type testRuntime struct {
	app.HostRuntime
	mu          sync.Mutex
	ref         host.Ref
	upstreams   []app.Upstream
	resolveErr  error
	lookupErr   error
	resolveCall int
	invalidate  int
	markActive  int
}

func (r *testRuntime) Lookup(_ context.Context, id string) (host.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lookupErr != nil {
		return host.Ref{}, r.lookupErr
	}
	if id != r.ref.ID {
		return host.Ref{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return r.ref, nil
}

func (r *testRuntime) Resolve(_ context.Context, _ string, _ int) (app.Upstream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveCall++
	if r.resolveErr != nil {
		return app.Upstream{}, r.resolveErr
	}
	if len(r.upstreams) == 0 {
		return app.Upstream{}, errors.New("no upstream configured")
	}
	index := r.resolveCall - 1
	if index >= len(r.upstreams) {
		index = len(r.upstreams) - 1
	}
	return r.upstreams[index], nil
}

func (r *testRuntime) InvalidateUpstream(string) {
	r.mu.Lock()
	r.invalidate++
	r.mu.Unlock()
}

func (r *testRuntime) MarkActive(string) {
	r.mu.Lock()
	r.markActive++
	r.mu.Unlock()
}

type testVerifier struct {
	app.Verifier
	mu            sync.Mutex
	ticket        app.Ticket
	consumed      map[string]int
	clientRevoked bool
}

type testActivityLeaseStore struct {
	mu         sync.Mutex
	acquireErr error
	renewErr   error
	releaseErr error
	inputErr   error
	acquired   []app.HostActivityLease
	renewed    []app.HostActivityLease
	released   []app.HostActivityLease
	inputRefs  []host.Ref
}

func (s *testActivityLeaseStore) AcquireHostActivityLease(_ context.Context, lease app.HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquired = append(s.acquired, lease)
	return s.acquireErr
}

func (s *testActivityLeaseStore) RenewHostActivityLease(_ context.Context, lease app.HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewed = append(s.renewed, lease)
	return s.renewErr
}

func (s *testActivityLeaseStore) ReleaseHostActivityLease(_ context.Context, lease app.HostActivityLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, lease)
	return s.releaseErr
}

func (s *testActivityLeaseStore) HasActiveHostActivityLease(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (s *testActivityLeaseStore) WithHostInputPermit(_ context.Context, ref host.Ref, forward func() error) error {
	s.mu.Lock()
	s.inputRefs = append(s.inputRefs, ref)
	err := s.inputErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if ref.ControlState != host.ControlUser {
		return domain.NewError(domain.CodeConflict, "host is not in user control mode")
	}
	return forward()
}

func (v *testVerifier) ConsumeTicket(_ context.Context, raw string) (app.Ticket, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.consumed == nil {
		v.consumed = make(map[string]int)
	}
	v.consumed[raw]++
	if raw != "chx_valid" || v.consumed[raw] != 1 {
		return app.Ticket{}, identity.ErrInvalidCredential
	}
	return v.ticket, nil
}

func (v *testVerifier) ClientInstallationActive(_ context.Context, installationID, userID string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return installationID == "i1234567" && userID == testUserID && !v.clientRevoked, nil
}

func newTestGateway(t *testing.T, runtime *testRuntime, verifier *testVerifier, hosts *testHostService, mutate func(*Deps)) (*gatewayHandler, *gateway) {
	t.Helper()
	keyPath := t.TempDir() + "/cookie.key"
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	workbench := t.TempDir()
	if err := os.WriteFile(workbench+"/index.html", []byte("<html>workbench</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workbench+"/app.12345678.js", []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	leases := &testActivityLeaseStore{}
	sharing := &testHostSharingService{allowed: true}
	d := Deps{
		Config: config.Gateway{
			WorkbenchDir:       workbench,
			UpstreamCacheTTL:   time.Minute,
			CredentialCacheTTL: 10 * time.Second,
			PreviewPortMin:     1024,
			PreviewPortDeny:    []int{8080},
			MaxBodyBytes:       8,
		},
		Domains:          config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: testSuffix, CookieSecure: true},
		Security:         config.Security{CookieKeyFile: keyPath, HostCookieTTL: time.Hour},
		Hosts:            hosts,
		HostSharing:      sharing,
		Control:          &testGatewayControlService{},
		Runtime:          runtime,
		ActivityLeases:   leases,
		InputPermits:     leases,
		TerminalSessions: &testTerminalSessionStore{},
		TerminalAIO:      &testTerminalSessionAIO{},
		Auth:             verifier,
		Logger:           zap.NewNop(),
		Metrics:          prometheus.NewRegistry(),
	}
	if mutate != nil {
		mutate(&d)
	}
	handler, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	engine, ok := handler.(*gatewayHandler)
	if !ok {
		t.Fatalf("New() returned %T, want *gatewayHandler", handler)
	}
	return engine, engine.gateway
}

func newTestDeps(t *testing.T, upstreams ...app.Upstream) (*gatewayHandler, *gateway, *testRuntime, *testVerifier, *testHostService) {
	t.Helper()
	runtime := &testRuntime{ref: host.Ref{
		ID: testHostID, OwnerID: testUserID, State: host.StateRunning,
		RuntimeGeneration: 1, ControlVersion: 0, ControlState: host.ControlAI,
	}, upstreams: upstreams}
	verifier := &testVerifier{ticket: app.Ticket{TokenID: "t1234567", UserID: testUserID, ActorUserID: testUserID, HostID: testHostID}}
	hosts := &testHostService{
		value:  host.Host{ID: testHostID, OwnerID: testUserID, Name: "demo", TemplateID: "std", RuntimeProfile: "container", State: host.StateRunning, AIOKeyEnc: []byte("must-not-serialize"), CreatedAt: time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)},
		ticket: app.EnterInfo{URL: "https://h1234567.example.test/__ch/auth?t=chx_valid", ExpiresAt: time.Now().Add(time.Minute)},
	}
	handler, g := newTestGateway(t, runtime, verifier, hosts, nil)
	return handler, g, runtime, verifier, hosts
}

func request(handler http.Handler, method, hostName, path string, body io.Reader, cookie string, auth string) *testRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Host = hostName
	req.RemoteAddr = "192.0.2.10:12345"
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Origin", "https://"+hostName)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	return response
}

func hostAuthority() string { return testHostID + "." + testSuffix }

func setMemberPermissionGrants(g *gateway, permissions ...host.Permission) {
	sharing := g.deps.HostSharing.(*testHostSharingService)
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	sharing.allowed = false
	sharing.permissions = make(map[host.Permission]bool, len(permissions))
	for _, permission := range permissions {
		sharing.permissions[permission] = true
	}
}

func TestNewValidatesCookieKeyAndDependencies(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New() accepted missing dependencies")
	}
	handler, g, _, _, _ := newTestDeps(t)
	_ = handler
	d := g.deps
	d.Security.CookieKeyFile = t.TempDir() + "/missing"
	_, err := New(d)
	if err == nil {
		t.Fatal("New() accepted a missing cookie key")
	}
	d = g.deps
	badKeyPath := t.TempDir() + "/cookie.key"
	if err := os.WriteFile(badKeyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Security.CookieKeyFile = badKeyPath
	if _, err := New(d); err == nil {
		t.Fatal("New() accepted a cookie key with the wrong size")
	}
}

func TestNewReusesSharedRegistryAndShutdownClosesWebSockets(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	if _, err := New(g.deps); err != nil {
		t.Fatalf("second gateway with shared metrics registry: %v", err)
	}
	g.trackWebSocket(&testReadWriteCloser{}, testHostID)
	if len(g.wsBodies) != 1 {
		t.Fatalf("registered websocket count = %d", len(g.wsBodies))
	}
	handler.CloseConnections()
	if len(g.wsBodies) != 0 {
		t.Fatalf("shutdown left %d websocket connections registered", len(g.wsBodies))
	}
}

func TestTicketCookieControlRoutesAndFixedNotFound(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	valid := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=chx_valid", nil, "", "")
	if valid.Code != http.StatusFound || valid.Header().Get("Location") != "/__ch/wb/" {
		t.Fatalf("ticket response = %d %q", valid.Code, valid.Header().Get("Location"))
	}
	cookies := valid.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hostCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Domain != "" || cookies[0].Path != "/" {
		t.Fatalf("host cookie attributes = %#v", cookies)
	}
	second := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=chx_valid", nil, "", "")
	if second.Code != http.StatusNotFound || second.Body.String() != validNotFoundBody {
		t.Fatalf("replayed ticket = %d %q", second.Code, second.Body.String())
	}
	bad := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=wrong", nil, "", "")
	if bad.Code != http.StatusNotFound || bad.Body.String() != validNotFoundBody {
		t.Fatalf("invalid ticket = %d %q", bad.Code, bad.Body.String())
	}
	value, _, ok := g.verifyCookie(cookies[0].Value, testHostID, time.Now())
	if !ok || value.UserID != testUserID || value.HostScope != testHostID {
		t.Fatalf("issued cookie did not verify: %#v", value)
	}
}

func TestMemberDesktopViewIsCheckedOnEveryRequestAndCannotReachOtherPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>desktop</html>"))
	}))
	defer upstream.Close()
	handler, g, _, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL})
	sharing := g.deps.HostSharing.(*testHostSharingService)
	cookie := hostCookie(g, testHostID, testMemberID, time.Hour)

	desktop := request(handler, http.MethodGet, hostAuthority(), "/vnc/index.html", nil, hostCookieName+"="+cookie, "")
	if desktop.Code != http.StatusOK || sharing.calls != 1 || !strings.Contains(desktop.Body.String(), "desktop") {
		t.Fatalf("member desktop response/calls = %d/%d, want 200/1", desktop.Code, sharing.calls)
	}
	for _, path := range []string{"/", "/__ch/wb/", "/v1/shell/sessions", "/v1/file/list", "/__ch/api/host", "/mcp", "/preview"} {
		response := request(handler, http.MethodGet, hostAuthority(), path, nil, hostCookieName+"="+cookie, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("member path %s response = %d, want 404", path, response.Code)
		}
	}

	sharing.mu.Lock()
	sharing.allowed = false // model owner revocation after the viewer has a signed cookie
	sharing.mu.Unlock()
	revoked := request(handler, http.MethodGet, hostAuthority(), "/vnc/index.html", nil, hostCookieName+"="+cookie, "")
	if revoked.Code != http.StatusNotFound || sharing.calls != 2 {
		t.Fatalf("revoked member response/calls = %d/%d, want 404/2", revoked.Code, sharing.calls)
	}
}

func TestOwnerDisabledAfterCookieIssueLosesGatewayAccess(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	sharing := g.deps.HostSharing.(*testHostSharingService)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	header := hostCookieName + "=" + cookie

	first := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, header, "")
	if first.Code != http.StatusOK {
		t.Fatalf("authorized owner response = %d, want 200", first.Code)
	}

	sharing.mu.Lock()
	sharing.ownerDenied = true
	sharing.mu.Unlock()
	second := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, header, "")
	if second.Code != http.StatusNotFound {
		t.Fatalf("disabled owner response = %d, want 404", second.Code)
	}
	if sharing.calls != 2 {
		t.Fatalf("owner authorization calls = %d, want 2 despite cached cookie", sharing.calls)
	}
}

func TestClientTicketCookieRevocationAndTamper(t *testing.T) {
	handler, g, _, verifier, _ := newTestDeps(t)
	verifier.ticket.ClientInstallationID = "i1234567"
	entered := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=chx_valid", nil, "", "")
	if entered.Code != http.StatusFound {
		t.Fatalf("client ticket response = %d", entered.Code)
	}
	cookies := entered.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != hostCookieName {
		t.Fatalf("missing host cookie: %#v", cookies)
	}
	value := cookies[0].Value
	principal, _, ok := g.verifyCookie(value, testHostID, time.Now())
	if !ok || principal.ClientInstallationID != "i1234567" {
		t.Fatalf("client cookie principal = %#v, valid = %v", principal, ok)
	}
	good := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, hostCookieName+"="+value, "")
	if good.Code != http.StatusOK {
		t.Fatalf("client cookie response = %d", good.Code)
	}
	for _, raw := range []string{
		strings.Replace(value, "i1234567", "i7654321", 1),
		strings.Replace(value, ".i1234567.", "..", 1),
	} {
		if _, _, valid := g.verifyCookie(raw, testHostID, time.Now()); valid {
			t.Fatal("accepted tampered client cookie")
		}
	}
	verifier.mu.Lock()
	verifier.clientRevoked = true
	verifier.mu.Unlock()
	revoked := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, hostCookieName+"="+value, "")
	if revoked.Code != http.StatusNotFound {
		t.Fatalf("revoked client cookie response = %d", revoked.Code)
	}
}

func TestMemberGatewayPermissionAllowlistIsExact(t *testing.T) {
	for _, test := range []struct {
		name        string
		method      string
		path        string
		websocket   bool
		preview     bool
		permissions []host.Permission
		want        bool
	}{
		{name: "desktop assets", method: http.MethodGet, path: "/vnc/index.html", permissions: []host.Permission{host.PermissionDesktopView}, want: true},
		{name: "desktop asset traversal", method: http.MethodGet, path: "/vnc/../v1/shell/exec"},
		{name: "desktop asset mutation", method: http.MethodPost, path: "/vnc/index.html"},
		{name: "desktop control websocket prefers control", method: http.MethodGet, path: "/ws", websocket: true,
			permissions: []host.Permission{host.PermissionDesktopControl, host.PermissionDesktopView}, want: true},
		{name: "terminal session API", method: http.MethodPut, path: "/__ch/api/terminal-session", permissions: []host.Permission{host.PermissionTerminalControl}, want: true},
		{name: "terminal-only entry page", method: http.MethodGet, path: "/__ch/terminal/", permissions: []host.Permission{host.PermissionTerminalControl}, want: true},
		{name: "terminal entry query denied", method: http.MethodGet, path: "/__ch/terminal/?next=/vnc"},
		{name: "terminal page", method: http.MethodGet, path: "/terminal?session_id=s-1", permissions: []host.Permission{host.PermissionTerminalControl}, want: true},
		{name: "shell websocket", method: http.MethodGet, path: "/v1/shell/ws?session_id=s-1", websocket: true,
			permissions: []host.Permission{host.PermissionTerminalControl}, want: true},
		{name: "terminal session API query", method: http.MethodPut, path: "/__ch/api/terminal-session?replace=true"},
		{name: "terminal page extra query", method: http.MethodGet, path: "/terminal?session_id=s-1&token=x"},
		{name: "shell websocket missing session", method: http.MethodGet, path: "/v1/shell/ws", websocket: true},
		{name: "shell websocket plain HTTP", method: http.MethodGet, path: "/v1/shell/ws?session_id=s-1"},
		{name: "shell websocket extra query", method: http.MethodGet, path: "/v1/shell/ws?session_id=s-1&other=x", websocket: true},
		{name: "shell websocket duplicate session", method: http.MethodGet, path: "/v1/shell/ws?session_id=s-1&session_id=s-2", websocket: true},
		{name: "control management API", method: http.MethodPost, path: "/__ch/api/control/actions"},
		{name: "host management API", method: http.MethodGet, path: "/__ch/api/host"},
		{name: "shell execution API", method: http.MethodPost, path: "/v1/shell/exec"},
		{name: "file API", method: http.MethodGet, path: "/v1/file/list"},
		{name: "preview app page", method: http.MethodGet, path: "/", preview: true},
		{name: "preview mutation", method: http.MethodPost, path: "/api/update", preview: true},
		{name: "preview forbidden method", method: http.MethodConnect, path: "/", preview: true},
		{name: "preview path traversal", method: http.MethodGet, path: "/../v1/shell/exec", preview: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.websocket {
				request.Header.Set("Upgrade", "websocket")
				request.Header.Set("Connection", "keep-alive, Upgrade")
			}
			parsed := ParsedHost{}
			if test.preview {
				port := 3000
				parsed.PreviewPort = &port
			}
			permissions, allowed := memberGatewayPermissions(request, parsed)
			if allowed != test.want || !reflect.DeepEqual(permissions, test.permissions) {
				t.Fatalf("memberGatewayPermissions() = %#v, %t; want %#v, %t", permissions, allowed, test.permissions, test.want)
			}
		})
	}
}

func TestTerminalOnlyTicketOpensRestrictedTerminalEntry(t *testing.T) {
	handler, g, _, verifier, _ := newTestDeps(t)
	verifier.ticket.ActorUserID = testMemberID
	setMemberPermissionGrants(g, host.PermissionTerminalControl)
	issued := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=chx_valid&next=%2F__ch%2Fterminal%2F", nil, "", "")
	if issued.Code != http.StatusFound || issued.Header().Get("Location") != "/__ch/terminal/" || len(issued.Result().Cookies()) != 1 {
		t.Fatalf("terminal ticket consume = %d %q", issued.Code, issued.Header().Get("Location"))
	}
	cookie := hostCookieName + "=" + issued.Result().Cookies()[0].Value
	entry := request(handler, http.MethodGet, hostAuthority(), "/__ch/terminal/", nil, cookie, "")
	if entry.Code != http.StatusOK || !strings.Contains(entry.Body.String(), "此入口只开放终端会话") || !strings.Contains(entry.Body.String(), "主机当前由 AI 控制") || entry.Header().Get("Cache-Control") != "no-store" || !strings.Contains(entry.Header().Get("Content-Security-Policy"), "script-src 'sha256-") {
		t.Fatalf("terminal entry response = %d headers=%#v body=%q", entry.Code, entry.Header(), entry.Body.String())
	}
	workbench := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/", nil, cookie, "")
	if workbench.Code != http.StatusNotFound {
		t.Fatalf("terminal-only member reached workbench: %d", workbench.Code)
	}
	setMemberPermissionGrants(g)
	revoked := request(handler, http.MethodGet, hostAuthority(), "/__ch/terminal/", nil, cookie, "")
	if revoked.Code != http.StatusNotFound {
		t.Fatalf("terminal page after revocation = %d", revoked.Code)
	}
}

func TestTicketRateLimitReturns429(t *testing.T) {
	handler, _, _, _, _ := newTestDeps(t)
	for i := 0; i < 30; i++ {
		response := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=bad", nil, "", "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("request %d before limit returned %d", i+1, response.Code)
		}
	}
	limited := request(handler, http.MethodGet, hostAuthority(), "/__ch/auth?t=bad", nil, "", "")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit response = %d Retry-After=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
}

const validNotFoundBody = "{\"error\":{\"code\":\"not_found\",\"message\":\"not found\",\"details\":{}}}"

func TestCookieAuthHostAPIAndLogout(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	hostResponse := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, hostCookieName+"="+cookie, "")
	if hostResponse.Code != http.StatusOK {
		t.Fatalf("GET host status = %d, body %s", hostResponse.Code, hostResponse.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(hostResponse.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["url"] != "https://h1234567.example.test" || strings.Contains(hostResponse.Body.String(), "must-not-serialize") || strings.Contains(hostResponse.Body.String(), "aio_key_enc") {
		t.Fatalf("host response has incorrect or private fields: %s", hostResponse.Body.String())
	}
	logout := request(handler, http.MethodPost, hostAuthority(), "/__ch/logout", nil, hostCookieName+"="+cookie, "")
	if logout.Code != http.StatusNoContent || len(logout.Result().Cookies()) != 1 || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout response = %d, %#v", logout.Code, logout.Result().Cookies())
	}
}

func TestMissingAndWrongScopeCredentialsReturnSameNotFound(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	missing := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, "", "")
	wrongHostCookie := g.signCookie("h7654321", testUserID, time.Now().Add(time.Hour))
	wrong := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, hostCookieName+"="+wrongHostCookie, "")
	invalidBearer := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, "", "Bearer cht_wrong")
	for _, response := range []*testRecorder{missing, wrong, invalidBearer} {
		if response.Code != http.StatusNotFound || response.Body.String() != validNotFoundBody {
			t.Errorf("auth failure = %d %q", response.Code, response.Body.String())
		}
	}
	if got := g.authFailures.WithLabelValues("gateway", "host_cookie"); got == nil {
		t.Fatal("host_cookie auth failure counter missing")
	}
}

func TestProxyStripsPlatformCredentialsAndInjectsAIOKey(t *testing.T) {
	type observed struct{ auth, cookie, key, xff, xhost, secure string }
	seen := make(chan observed, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{auth: r.Header.Get("Authorization"), cookie: r.Header.Get("Cookie"), key: r.Header.Get("X-AIO-API-Key"), xff: r.Header.Get("X-Forwarded-For"), xhost: r.Header.Get("X-Forwarded-Host"), secure: r.Header.Get("X-Secure")}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: server.URL, APIKey: "aio-secret", Headers: map[string]string{"X-Secure": "trusted"}})
	runtime.ref.ControlState = host.ControlUser
	cookie := g.signCookie(testHostID, testUserID, time.Now().Add(time.Hour))
	response := request(handler, http.MethodGet, hostAuthority(), "/v1/shell/sessions", nil, hostCookieName+"="+cookie+"; session=keep", "")
	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
	got := <-seen
	if got.auth != "" || strings.Contains(got.cookie, hostCookieName) || !strings.Contains(got.cookie, "session=keep") || got.key != "aio-secret" || got.secure != "trusted" || got.xff != "192.0.2.10" || got.xhost != hostAuthority() {
		t.Fatalf("upstream saw unexpected headers: %#v", got)
	}
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	if len(leases.acquired) != 1 || leases.acquired[0].Kind != app.HostActivityLeaseKindGateway ||
		len(leases.released) != 1 || leases.released[0].LeaseID != leases.acquired[0].LeaseID {
		t.Fatalf("ordinary HTTP proxy activity lease acquired=%#v released=%#v", leases.acquired, leases.released)
	}

	requestWithBearer := httptest.NewRequest(http.MethodGet, "/v1/shell/sessions", nil)
	requestWithBearer.Host = hostAuthority()
	requestWithBearer.RemoteAddr = "192.0.2.11:12345"
	requestWithBearer.Header.Set("Authorization", "Bearer cht_valid")
	responseBearer := newTestRecorder()
	handler.ServeHTTP(responseBearer, requestWithBearer)
	close(responseBearer.closed)
	if responseBearer.Code != http.StatusNotFound {
		t.Fatalf("unsupported Bearer credential was accepted: %d", responseBearer.Code)
	}

}

func TestProxyAdmissionFollowsStableHostControlState(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     host.ControlState
		wantCode  int
		wantCalls int
	}{
		{name: "AI mode", state: host.ControlAI, wantCode: http.StatusNotFound, wantCalls: 0},
		{name: "human mode", state: host.ControlUser, wantCode: http.StatusOK, wantCalls: 1},
		{name: "taking over", state: host.ControlTakingOver, wantCode: http.StatusServiceUnavailable},
		{name: "returning", state: host.ControlReturning, wantCode: http.StatusServiceUnavailable},
		{name: "unknown mode", state: "unknown", wantCode: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
			runtime.ref.ControlState = test.state
			cookie := hostCookie(g, testHostID, testUserID, time.Hour)
			response := request(handler, http.MethodGet, hostAuthority(), "/v1/file/list", nil, hostCookieName+"="+cookie, "")
			if response.Code != test.wantCode {
				t.Fatalf("response status = %d, want %d: %s", response.Code, test.wantCode, response.Body.String())
			}
			if upstreamCalls != test.wantCalls || runtime.resolveCall != test.wantCalls {
				t.Fatalf("upstream calls=%d resolve calls=%d, want %d", upstreamCalls, runtime.resolveCall, test.wantCalls)
			}
		})
	}
}

func TestLongLivedProxyFailsClosedWhenActivityLeaseIsStale(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = io.WriteString(w, "unexpected")
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	leases.acquireErr = domain.NewError(domain.CodeConflict, "stale fence")
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+cookie)
	req.Header.Set("Accept", "text/event-stream")
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale activity lease response = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if runtime.resolveCall != 0 || upstreamCalls != 0 {
		t.Fatalf("stale lease reached upstream: resolves=%d calls=%d", runtime.resolveCall, upstreamCalls)
	}
}

func TestProxyBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: server.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	response := request(handler, http.MethodPost, hostAuthority(), "/v1/upload", strings.NewReader("0123456789"), hostCookieName+"="+cookie, "")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request = %d %s", response.Code, response.Body.String())
	}
}

func TestPrivatePreviewTicketAndPortValidation(t *testing.T) {
	handler, g, runtime, verifier, hosts := newTestDeps(t)
	verifier.ticket.PreviewPort = 5174
	cookie := g.signCookie(testHostID, testUserID, time.Now().Add(time.Hour))
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-AIO-API-Key") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("private preview received a platform credential")
		}
		_, _ = io.WriteString(w, "preview")
	}))
	defer previewServer.Close()
	runtime.upstreams = []app.Upstream{{BaseURL: previewServer.URL, APIKey: "must-not-inject"}}
	response := request(handler, http.MethodGet, "5173-"+hostAuthority(), "/", nil, hostCookieName+"="+cookie, "")
	if response.Code != http.StatusOK || response.Body.String() != "preview" {
		t.Fatalf("authenticated preview = %d %q", response.Code, response.Body.String())
	}
	private := request(handler, http.MethodGet, "5174-"+hostAuthority(), "/app?x=1", nil, "", "")
	wantAuth := "https://portal.example.test/api/hosts/" + testHostID + "/preview-auth?next=%2Fapp%3Fx%3D1&port=5174"
	if private.Code != http.StatusFound || private.Header().Get("Location") != wantAuth {
		t.Fatalf("private preview redirect = %d %q", private.Code, private.Header().Get("Location"))
	}
	previewTicket := request(handler, http.MethodGet, "5174-"+hostAuthority(), "/__ch/auth?t=chx_valid&next=%2Fapp%3Fx%3D1", nil, "", "")
	if previewTicket.Code != http.StatusFound || previewTicket.Header().Get("Location") != "/app?x=1" || len(previewTicket.Result().Cookies()) != 1 {
		t.Fatalf("preview ticket consume = %d %q %#v", previewTicket.Code, previewTicket.Header().Get("Location"), previewTicket.Result().Cookies())
	}
	if hosts.issueCalls != 0 {
		t.Fatalf("gateway issued tickets directly: calls=%d", hosts.issueCalls)
	}
	_ = verifier
	denied := request(handler, http.MethodGet, "8080-"+hostAuthority(), "/", nil, "", "")
	if denied.Code != http.StatusNotFound || denied.Body.String() != validNotFoundBody {
		t.Fatalf("denied preview port = %d %q", denied.Code, denied.Body.String())
	}
	for _, port := range []string{"5900", "6080", "8090", "8091", "8100", "9222"} {
		response := request(handler, http.MethodGet, port+"-"+hostAuthority(), "/", nil, "", "")
		if response.Code != http.StatusNotFound || response.Body.String() != validNotFoundBody {
			t.Errorf("reserved preview port %s = %d %q, want hidden 404", port, response.Code, response.Body.String())
		}
	}
}

func TestSharedMemberCannotConsumePreviewTicket(t *testing.T) {
	handler, g, _, verifier, _ := newTestDeps(t)
	verifier.ticket.ActorUserID = testMemberID
	verifier.ticket.PreviewPort = 5174
	setMemberPermissionGrants(g, host.PermissionMCPControl)
	cookie := hostCookieName + "=" + g.signCookie(testHostID, testMemberID, time.Now().Add(time.Hour))
	memberPage := request(handler, http.MethodGet, "5174-"+hostAuthority(), "/app", nil, cookie, "")
	if memberPage.Code != http.StatusNotFound || memberPage.Body.String() != validNotFoundBody {
		t.Fatalf("shared member preview page = %d %q", memberPage.Code, memberPage.Body.String())
	}

	response := request(handler, http.MethodGet, "5174-"+hostAuthority(), "/__ch/auth?t=chx_valid", nil, "", "")
	if response.Code != http.StatusNotFound || len(response.Result().Cookies()) != 0 {
		t.Fatalf("shared member preview ticket = %d cookies=%#v", response.Code, response.Result().Cookies())
	}
}

func TestPreviewRootHTMLAcceptUsesPrivatePreview(t *testing.T) {
	var upstreamCalls int
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = io.WriteString(w, "preview-root")
	}))
	defer previewServer.Close()

	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: previewServer.URL, APIKey: "must-not-inject"})
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	previewHost := "5173-" + hostAuthority()
	serveHTMLRoot := func(hostName, rawCookie string) *testRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = hostName
		req.RemoteAddr = "192.0.2.10:12345"
		req.Header.Set("Accept", "text/html")
		if rawCookie != "" {
			req.Header.Set("Cookie", rawCookie)
		}
		response := newTestRecorder()
		handler.ServeHTTP(response, req)
		close(response.closed)
		return response
	}

	private := serveHTMLRoot(previewHost, hostCookieName+"="+cookie)
	if private.Code != http.StatusOK || private.Body.String() != "preview-root" {
		t.Fatalf("authenticated preview root = %d %q, want upstream response", private.Code, private.Body.String())
	}
	if upstreamCalls != 1 || runtime.resolveCall != 1 {
		t.Fatalf("authenticated preview root calls: upstream=%d resolve=%d, want 1 each", upstreamCalls, runtime.resolveCall)
	}

	crossOriginRequest := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	crossOriginRequest.Host = previewHost
	crossOriginRequest.RemoteAddr = "192.0.2.10:12345"
	crossOriginRequest.Header.Set("Accept", "text/html")
	crossOriginRequest.Header.Set("Cookie", hostCookieName+"="+cookie)
	crossOriginRequest.Header.Set("Origin", "https://evil.example.test")
	crossOrigin := newTestRecorder()
	handler.ServeHTTP(crossOrigin, crossOriginRequest)
	close(crossOrigin.closed)
	if crossOrigin.Code != http.StatusNotFound || upstreamCalls != 1 || runtime.resolveCall != 1 {
		t.Fatalf("cross-origin preview root write = %d, upstream=%d resolves=%d; want rejection before proxy", crossOrigin.Code, upstreamCalls, runtime.resolveCall)
	}

	unauthenticated := serveHTMLRoot(previewHost, "")
	wantLocation := "https://portal.example.test/api/hosts/" + testHostID + "/preview-auth?next=%2F&port=5173"
	if unauthenticated.Code != http.StatusFound || unauthenticated.Header().Get("Location") != wantLocation {
		t.Fatalf("unauthenticated preview root = %d %q, want redirect %q", unauthenticated.Code, unauthenticated.Header().Get("Location"), wantLocation)
	}
	if upstreamCalls != 1 || runtime.resolveCall != 1 {
		t.Fatalf("unauthenticated preview root reached upstream: calls=%d resolves=%d", upstreamCalls, runtime.resolveCall)
	}

	control := serveHTMLRoot(hostAuthority(), hostCookieName+"="+cookie)
	if control.Code != http.StatusFound || control.Header().Get("Location") != "/__ch/wb/" {
		t.Fatalf("control host root = %d %q, want Workbench redirect", control.Code, control.Header().Get("Location"))
	}
}

func TestCreatingHostWaitsAndFailedHostIsNotFound(t *testing.T) {
	handler, g, runtime, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	runtime.ref.State = host.StateCreating
	response := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, hostCookieName+"="+cookie, "")
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" || !strings.Contains(response.Body.String(), "host_not_ready") {
		t.Fatalf("creating response = %d %q", response.Code, response.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sandbox", nil)
	req.Host = hostAuthority()
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", hostCookieName+"="+cookie)
	page := newTestRecorder()
	handler.ServeHTTP(page, req)
	close(page.closed)
	if page.Code != http.StatusServiceUnavailable || !strings.Contains(page.Body.String(), "创建中") {
		t.Fatalf("creating page = %d %q", page.Code, page.Body.String())
	}
	runtime.ref.State = host.StateFailed
	failed := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, hostCookieName+"="+cookie, "")
	if failed.Code != http.StatusNotFound {
		t.Fatalf("failed host = %d", failed.Code)
	}
}

func TestWorkbenchSecurityHeadersAndSPA(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	index := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/", nil, hostCookieName+"="+cookie, "")
	if index.Code != http.StatusOK || index.Header().Get("Cache-Control") != "no-cache" || index.Header().Get("Content-Security-Policy") != "frame-ancestors 'self'" || index.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("workbench response = %d, headers %#v", index.Code, index.Header())
	}
	asset := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/app.12345678.js", nil, hostCookieName+"="+cookie, "")
	if asset.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset cache = %q", asset.Header().Get("Cache-Control"))
	}
	fallback := request(handler, http.MethodGet, hostAuthority(), "/__ch/wb/route/subroute", nil, hostCookieName+"="+cookie, "")
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "workbench") {
		t.Fatalf("SPA fallback = %d %q", fallback.Code, fallback.Body.String())
	}
}

func TestRootRedirectAndRenew(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	root := httptest.NewRequest(http.MethodGet, "/", nil)
	root.Host = hostAuthority()
	root.Header.Set("Accept", "text/html")
	root.Header.Set("Cookie", hostCookieName+"="+cookie)
	recorder := newTestRecorder()
	handler.ServeHTTP(recorder, root)
	close(recorder.closed)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/__ch/wb/" {
		t.Fatalf("root redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	renew := request(handler, http.MethodPost, hostAuthority(), "/__ch/api/renew", nil, hostCookieName+"="+cookie, "")
	if renew.Code != http.StatusOK {
		t.Fatalf("renew = %d %s", renew.Code, renew.Body.String())
	}
}

func TestConnectionRetryAndNoWriteReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "recovered") }))
	defer server.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: dead.URL}, app.Upstream{BaseURL: server.URL})
	runtime.ref.ControlState = host.ControlUser
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	retried := request(handler, http.MethodGet, hostAuthority(), "/v1/sandbox", nil, hostCookieName+"="+cookie, "")
	if retried.Code != http.StatusOK || retried.Body.String() != "recovered" || runtime.resolveCall != 2 || runtime.invalidate != 1 {
		t.Fatalf("GET retry = %d %q resolves=%d invalidates=%d", retried.Code, retried.Body.String(), runtime.resolveCall, runtime.invalidate)
	}
	runtime.resolveCall = 0
	runtime.invalidate = 0
	runtime.upstreams = []app.Upstream{{BaseURL: dead.URL}, {BaseURL: server.URL}}
	failedWrite := request(handler, http.MethodPost, hostAuthority(), "/v1/sandbox", strings.NewReader("x"), hostCookieName+"="+cookie, "")
	if failedWrite.Code != http.StatusBadGateway || runtime.resolveCall != 1 {
		t.Fatalf("POST was replayed or returned unexpected status: %d resolves=%d", failedWrite.Code, runtime.resolveCall)
	}
}

func TestCredentialCacheCookieValidationLimiterAndHelpers(t *testing.T) {
	cache := newCredentialCache(10 * time.Second)
	now := time.Now()
	p := identity.Principal{UserID: testUserID, Kind: identity.KindHostCookie, HostScope: testHostID}
	cache.put("secret", p, time.Minute, now)
	if got, ok := cache.get("secret", now.Add(9*time.Second)); !ok || !reflect.DeepEqual(got, p) {
		t.Fatalf("cached principal = %#v, %v", got, ok)
	}
	if _, ok := cache.get("secret", now.Add(10*time.Second)); ok {
		t.Fatal("expired credential remained cached")
	}
	cache.data[sha256.Sum256([]byte("expired"))] = cachedCredential{principal: p, expiresAt: now.Add(-time.Second)}
	cache.put("next", p, time.Second, now)
	if _, ok := cache.data[sha256.Sum256([]byte("expired"))]; ok {
		t.Fatal("put did not evict expired credential")
	}
	for i := 0; i < maxCachedCredentials; i++ {
		cache.data[[sha256.Size]byte{byte(i), byte(i >> 8)}] = cachedCredential{principal: p, expiresAt: now.Add(time.Minute)}
	}
	cache.put("over-limit", p, time.Second, now)
	if _, ok := cache.get("over-limit", now); ok {
		t.Fatal("cache exceeded its entry cap")
	}
	limiter := newIPLimiter(2, time.Minute)
	if ok, _ := limiter.allow("ip", now); !ok {
		t.Fatal("first limited request denied")
	}
	if ok, _ := limiter.allow("ip", now.Add(time.Second)); !ok {
		t.Fatal("second limited request denied")
	}
	if ok, retry := limiter.allow("ip", now.Add(2*time.Second)); ok || retry <= 0 {
		t.Fatalf("rate limit result = %v, %v", ok, retry)
	}
	if ok, _ := limiter.allow("ip", now.Add(time.Minute)); !ok {
		t.Fatal("expired IP window not reset")
	}
	if got := upstreamPrefix("/v1/shell/exec"); got != "/v1" || upstreamPrefix("/other/path") != "/other" {
		t.Fatalf("upstream prefix = %q", got)
	}
	if !safeRetryRequest(httptest.NewRequest(http.MethodGet, "/", nil)) || safeRetryRequest(httptest.NewRequest(http.MethodPost, "/", nil)) {
		t.Fatal("safe retry method policy is incorrect")
	}
	if safeNext("https://evil.test/") != "/" || safeNext("/%2f%2fevil.test") != "/" || safeNext("/safe?q=1") != "/safe?q=1" {
		t.Fatal("safe next path policy is incorrect")
	}
}

func TestCredentialCookieTamperExpiryAndScope(t *testing.T) {
	handler, g, _, _, _ := newTestDeps(t)
	_ = handler
	now := time.Now()
	valid := g.signCookie(testHostID, testUserID, now.Add(time.Hour))
	alteredLast := "0"
	if valid[len(valid)-1] == '0' {
		alteredLast = "1"
	}
	for _, raw := range []string{
		"",
		"bad.value",
		g.signCookie("h7654321", testUserID, now.Add(time.Hour)),
		g.signCookie(testHostID, "bad-user", now.Add(time.Hour)),
		g.signCookie(testHostID, testUserID, now.Add(-time.Second)),
		valid[:len(valid)-1] + alteredLast,
		valid[:strings.LastIndex(valid, ".")] + ".not-hex",
	} {
		if _, _, ok := g.verifyCookie(raw, testHostID, now); ok {
			t.Errorf("verifyCookie accepted %q", raw)
		}
	}
}

func hostCookie(g *gateway, hostID, userID string, ttl time.Duration) string {
	return g.signCookie(hostID, userID, time.Now().Add(ttl))
}

func TestUpstreamURLJoinPreservesEscapesAndPrefix(t *testing.T) {
	target, err := urlForHost(app.Upstream{BaseURL: "http://127.0.0.1:8080/sandboxes/id/port/8080"}, "/v1/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	if target.Path != "/sandboxes/id/port/8080/v1/a/b" {
		t.Fatalf("joined path = %q", target.Path)
	}
	if target.RawPath != "/sandboxes/id/port/8080/v1/a%2Fb" {
		t.Fatalf("joined raw path = %q", target.RawPath)
	}
	if _, err := urlForHost(app.Upstream{BaseURL: "file:///tmp"}, "/"); err == nil {
		t.Fatal("accepted non-HTTP upstream")
	}
}

func TestHostResponseOmitsInternalKeyAndRetiredFields(t *testing.T) {
	value := hostResponseFrom(host.Host{
		ID: testHostID, AIOKeyEnc: []byte("secret"), Workspaces: []workspace.Binding{{
			Workspace: workspace.Workspace{ID: "w1234567", Name: "project", State: workspace.StateReady},
			HostID:    testHostID, OwnerID: testUserID, Directory: "w1234567", State: workspace.BindingActive,
			BindingVersion: 1,
		}},
	}, config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: testSuffix})
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "aio_key_enc") || strings.Contains(string(encoded), "public_ports") ||
		strings.Contains(string(encoded), "size_mib") || !strings.Contains(string(encoded), `"workspaces":[{"workspace":`) ||
		!strings.Contains(string(encoded), `"binding_version":1`) ||
		!strings.Contains(string(encoded), `"portal_origin":"https://portal.example.test"`) {
		t.Fatalf("host response JSON = %s", encoded)
	}
}

func TestWebSocketOriginAndTrackedConnection(t *testing.T) {
	handler, g, runtime, _, _ := newTestDeps(t)
	_ = handler
	req := httptest.NewRequest(http.MethodGet, "/v1/shell", nil)
	req.Host = hostAuthority()
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://"+hostAuthority())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(parsedHostKey, ParsedHost{HostID: testHostID})
	if !g.validRequestOrigin(c) {
		t.Fatal("valid cookie websocket origin rejected")
	}
	req.Header.Set("Origin", "https://evil.test")
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(parsedHostKey, ParsedHost{HostID: testHostID})
	if g.validRequestOrigin(c) {
		t.Fatal("cross-origin cookie websocket accepted")
	}
	req.Header.Del("Upgrade")
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(parsedHostKey, ParsedHost{HostID: testHostID})
	if !g.validRequestOrigin(c) {
		t.Fatal("ordinary HTTP request required a websocket Origin")
	}
	body := &testReadWriteCloser{}
	tracked := g.trackWebSocket(body, testHostID)
	_, _ = tracked.Write([]byte("in"))
	_, _ = tracked.Read(make([]byte, 1))
	if len(g.wsBodies) != 1 || runtime.markActive == 0 {
		t.Fatalf("tracked websocket not registered or active: connections=%d activity=%d", len(g.wsBodies), runtime.markActive)
	}
	_ = tracked.Close()
	if len(g.wsBodies) != 0 {
		t.Fatalf("tracked websocket remained after close: %d", len(g.wsBodies))
	}
	before := runtime.markActive
	for i := 0; i < 5; i++ {
		g.markActive(testHostID)
	}
	if runtime.markActive != before {
		t.Fatalf("activity limiter allowed repeated marks: before=%d after=%d", before, runtime.markActive)
	}
	g.activityAt[testHostID] = time.Now().Add(-activityMinimumInterval - time.Second)
	g.markActive(testHostID)
	if runtime.markActive != before+1 {
		t.Fatalf("activity was not marked after interval: before=%d after=%d", before, runtime.markActive)
	}
}

type testReadWriteCloser struct{}

func (*testReadWriteCloser) Read(p []byte) (int, error)  { p[0] = 'x'; return 1, nil }
func (*testReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (*testReadWriteCloser) Close() error                { return nil }

func TestHTMLRootAndStatusLookupErrors(t *testing.T) {
	handler, _, runtime, _, _ := newTestDeps(t)
	runtime.ref.State = host.StateCreating
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = hostAuthority()
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Cookie", hostCookieName+"="+hostCookieFromHandler(t, handler))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("HTML root should redirect to workbench, status %d", response.Code)
	}
	runtime.lookupErr = errors.New("unexpected runtime failure")
	noCredential := requestTo(handler, http.MethodGet, hostAuthority(), "/v1", "")
	if noCredential.Code != http.StatusNotFound {
		t.Fatalf("host lookup failure changed fixed auth response: %d", noCredential.Code)
	}
}

func TestControlServiceErrorsAndRemovedPreviewAuthRoute(t *testing.T) {
	handler, g, _, _, hosts := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	hosts.getErr = domain.NewError(domain.CodeNotFound, "not found")
	if response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/host", nil, hostCookieName+"="+cookie, ""); response.Code != http.StatusNotFound {
		t.Fatalf("host service error response = %d", response.Code)
	}
	hosts.getErr = nil
	hosts.renewErr = domain.NewError(domain.CodeInvalidState, "cannot renew")
	if response := request(handler, http.MethodPost, hostAuthority(), "/__ch/api/renew", nil, hostCookieName+"="+cookie, ""); response.Code != http.StatusConflict {
		t.Fatalf("renew error response = %d", response.Code)
	}
	hosts.renewErr = nil
	if response := request(handler, http.MethodGet, hostAuthority(), "/__ch/preview-auth?port=5174", nil, hostCookieName+"="+cookie, ""); response.Code != http.StatusNotFound {
		t.Fatalf("removed preview auth route response = %d", response.Code)
	}
}

func hostCookieFromHandler(t *testing.T, handler http.Handler) string {
	t.Helper()
	concrete, ok := handler.(*gatewayHandler)
	if !ok {
		t.Fatalf("handler is %T", handler)
	}
	return hostCookie(concrete.gateway, testHostID, testUserID, time.Hour)
}

func requestTo(handler http.Handler, method, hostName, path, cookie string) *testRecorder {
	return request(handler, method, hostName, path, nil, cookie, "")
}

type testRecorder struct {
	*httptest.ResponseRecorder
	closed chan bool
}

func newTestRecorder() *testRecorder {
	return &testRecorder{ResponseRecorder: httptest.NewRecorder(), closed: make(chan bool)}
}

func (r *testRecorder) CloseNotify() <-chan bool { return r.closed }

func TestRetiredEndpointsAreNotProxied(t *testing.T) {
	handler, g, runtime, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	for _, path := range []string{"/__ch/api/agent-info", "/mcp", "/devtools/browser/id", "/v1/browser/info"} {
		response := request(handler, http.MethodGet, hostAuthority(), path, nil, hostCookieName+"="+cookie, "")
		if response.Code != http.StatusNotFound || runtime.resolveCall != 0 {
			t.Errorf("retired endpoint %s = %d, resolves=%d", path, response.Code, runtime.resolveCall)
		}
	}
}

func TestCookieWritesRejectMissingAndCrossHostOrigins(t *testing.T) {
	handler, g, runtime, _, _ := newTestDeps(t)
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	for _, path := range []string{"/__ch/api/renew", "/__ch/api/control/actions", "/v1/shell/exec"} {
		for _, origin := range []string{"", "https://h7654321." + testSuffix} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			req.Host = hostAuthority()
			req.Header.Set("Cookie", hostCookieName+"="+cookie)
			req.Header.Set("Origin", origin)
			response := newTestRecorder()
			handler.ServeHTTP(response, req)
			close(response.closed)
			if response.Code != http.StatusNotFound || runtime.resolveCall != 0 {
				t.Errorf("write %s with origin %q = %d, resolves=%d", path, origin, response.Code, runtime.resolveCall)
			}
		}
	}
}
