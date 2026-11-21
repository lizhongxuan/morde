package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
)

const testServiceInstanceID = "00000000-0000-4000-a000-000000000001"

type testEmbedTicketExchanger struct {
	record app.HostEmbedTicketRecord
	calls  int
}

func (s *testEmbedTicketExchanger) ConsumeEmbedTicket(_ context.Context, raw, nonce, hostID, instanceID, origin string) (app.HostEmbedTicketRecord, error) {
	s.calls++
	if raw != "one-time-ticket" || nonce != s.record.EmbedNonce || hostID != s.record.HostID || instanceID != s.record.ServiceInstanceID || origin != s.record.PortalOrigin || s.calls > 1 {
		return app.HostEmbedTicketRecord{}, context.Canceled
	}
	used := time.Now()
	s.record.UsedAt = &used
	return s.record, nil
}

func TestEmbedTicketExchangeCreatesSeparateSessionAndRevocationBlocksIt(t *testing.T) {
	handler, g, _, verifier, _ := newTestDeps(t)
	g.deps.ServiceInstanceID = testServiceInstanceID
	nonce := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	exchanger := &testEmbedTicketExchanger{record: app.HostEmbedTicketRecord{
		HostID: testHostID, ConversationID: "c1234567", ActorID: testUserID, PortalTokenID: "t1234567",
		ServiceInstanceID: testServiceInstanceID, PortalOrigin: "https://portal.example.test", InitialTarget: app.HostEmbedTargetBrowser,
		TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetBrowser, PermissionCeiling: host.PermissionDesktopControl}},
		EmbedNonce:   nonce, RuntimeGeneration: 1, ExpiresAt: time.Now().Add(time.Minute),
	}}
	g.deps.HostEmbedTickets = exchanger

	form := url.Values{"ticket": {"one-time-ticket"}, "embed_nonce": {nonce}}
	req := httptest.NewRequest(http.MethodPost, "/__ch/embed-auth", strings.NewReader(form.Encode()))
	req.Host = hostAuthority()
	req.Header.Set("Origin", "https://portal.example.test")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != embedTargetPath(app.HostEmbedTargetBrowser, nonce) {
		t.Fatalf("exchange = %d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store, private" || response.Header().Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Fatalf("exchange security headers = %v", response.Header())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != embedCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("exchange cookie = %#v", cookies)
	}
	claims, ok := g.verifyEmbedClaims(cookies[0].Value, testHostID, time.Now())
	if !ok || claims.ActorID != testUserID || claims.PortalTokenID != "t1234567" || claims.ConversationID != "c1234567" ||
		claims.ServiceInstanceID != testServiceInstanceID || claims.PortalOrigin != "https://portal.example.test" ||
		claims.EmbedNonce != nonce || claims.InitialTarget != app.HostEmbedTargetBrowser || len(claims.TargetGrants) != 1 ||
		claims.TargetGrants[0].PermissionCeiling != host.PermissionDesktopControl || claims.RuntimeGeneration != 1 {
		t.Fatalf("exchanged claims = %#v valid=%v", claims, ok)
	}

	assetReq := httptest.NewRequest(http.MethodGet, embedTargetPath(app.HostEmbedTargetBrowser, nonce), nil)
	assetReq.Host = hostAuthority()
	assetReq.AddCookie(cookies[0])
	assetResponse := newTestRecorder()
	handler.ServeHTTP(assetResponse, assetReq)
	if assetResponse.Code != http.StatusOK || !strings.Contains(assetResponse.Body.String(), "workbench") || assetResponse.Header().Get("Content-Security-Policy") != "frame-ancestors 'self' https://portal.example.test" {
		t.Fatalf("embedded workbench = %d headers=%v body=%q", assetResponse.Code, assetResponse.Header(), assetResponse.Body)
	}

	verifier.mu.Lock()
	verifier.embedRevoked = true
	verifier.mu.Unlock()
	blockedReq := httptest.NewRequest(http.MethodGet, embedTargetPath(app.HostEmbedTargetBrowser, nonce), nil)
	blockedReq.Host = hostAuthority()
	blockedReq.AddCookie(cookies[0])
	blocked := newTestRecorder()
	handler.ServeHTTP(blocked, blockedReq)
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("revoked embedded session = %d %s", blocked.Code, blocked.Body)
	}
}

func TestEmbeddedActivityLeaseClosesOnAuthorizationRevocation(t *testing.T) {
	_, g, _, verifier, _ := newTestDeps(t)
	g.deps.ServiceInstanceID = testServiceInstanceID
	g.memberAuthorizationCheckInterval = 5 * time.Millisecond
	g.activityRenewInterval = time.Second
	nonce := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	req := httptest.NewRequest(http.MethodGet, embedTargetPath(app.HostEmbedTargetBrowser, nonce), nil)
	req.Host = hostAuthority()
	ctx, _ := ginContextForEmbed(t, g, req)
	claims := embedClaims{HostID: testHostID, ActorID: testUserID, PortalTokenID: "t1234567", ConversationID: "c1234567",
		ServiceInstanceID: testServiceInstanceID, PortalOrigin: "https://portal.example.test", InitialTarget: app.HostEmbedTargetBrowser,
		TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetBrowser, PermissionCeiling: host.PermissionDesktopView}},
		EmbedNonce:   nonce, RuntimeGeneration: 1, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	ctx.Request.AddCookie(&http.Cookie{Name: embedCookieName, Value: g.signEmbedClaims(claims)})
	cookie, _ := ctx.Request.Cookie(embedCookieName)
	if !g.authenticateEmbed(ctx, cookie.Value) {
		t.Fatal("embedded session was not authenticated")
	}
	check := g.memberAuthorizationCheck(ctx)
	guard, leaseCtx, err := g.acquireHostActivityLease(ctx.Request.Context(), hostRefFrom(ctx), app.HostActivityLeaseKindGateway, check)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close Host activity lease: %v", err)
		}
	}()
	verifier.mu.Lock()
	verifier.embedRevoked = true
	verifier.mu.Unlock()
	select {
	case <-leaseCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("existing activity lease remained active after credential revocation")
	}
}

func TestEmbeddedWorkbenchRequiresNonceAndAuthorizedTarget(t *testing.T) {
	claims := embedClaims{
		InitialTarget: app.HostEmbedTargetBrowser,
		TargetGrants: []app.HostEmbedTargetGrant{
			{Target: app.HostEmbedTargetBrowser, PermissionCeiling: host.PermissionDesktopView},
			{Target: app.HostEmbedTargetWorkspace, PermissionCeiling: host.PermissionWorkspaceFileRead},
		},
		EmbedNonce: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for _, test := range []struct {
		name string
		url  string
		want bool
	}{
		{name: "authorized workspace tab", url: embedTargetPath(app.HostEmbedTargetWorkspace, claims.EmbedNonce), want: true},
		{name: "unauthorized terminal tab", url: embedTargetPath(app.HostEmbedTargetTerminal, claims.EmbedNonce)},
		{name: "wrong nonce", url: embedTargetPath(app.HostEmbedTargetBrowser, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")},
		{name: "duplicate tab", url: "/__ch/wb/?embed=1&embed_nonce=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&tab=browser&tab=workspace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.url, nil)
			_, ok := embedRequestPermission(request, claims)
			if ok != test.want {
				t.Fatalf("embedRequestPermission = %v, want %v", ok, test.want)
			}
		})
	}
}

func TestEmbeddedFileRoutesRequireFilesTargetAndExactAIOOperations(t *testing.T) {
	files := embedClaims{TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetFiles, PermissionCeiling: host.PermissionDesktopControl}}}
	otherTarget := embedClaims{TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetVSCode, PermissionCeiling: host.PermissionDesktopControl}}}
	for _, test := range []struct {
		name   string
		method string
		path   string
		claims embedClaims
		want   bool
	}{
		{name: "list", method: http.MethodPost, path: "/v1/file/list", claims: files, want: true},
		{name: "read", method: http.MethodPost, path: "/v1/file/read", claims: files, want: true},
		{name: "write", method: http.MethodPost, path: "/v1/file/write", claims: files, want: true},
		{name: "upload", method: http.MethodPost, path: "/v1/file/upload", claims: files, want: true},
		{name: "make directory", method: http.MethodPost, path: "/v1/file/mkdir", claims: files, want: true},
		{name: "move", method: http.MethodPost, path: "/v1/file/move", claims: files, want: true},
		{name: "delete", method: http.MethodPost, path: "/v1/file/delete", claims: files, want: true},
		{name: "download", method: http.MethodGet, path: "/v1/file/download?path=/home/gem/workspace/file.txt", claims: files, want: true},
		{name: "files grant does not authorize VS Code target", method: http.MethodPost, path: "/v1/file/mkdir", claims: otherTarget},
		{name: "mutation query", method: http.MethodPost, path: "/v1/file/delete?recursive=true", claims: files},
		{name: "wrong method", method: http.MethodGet, path: "/v1/file/mkdir", claims: files},
		{name: "unknown file operation", method: http.MethodPost, path: "/v1/file/restore", claims: files},
		{name: "general shell execution remains denied", method: http.MethodPost, path: "/v1/shell/exec", claims: files},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			_, ok := embedRequestPermission(request, test.claims)
			if ok != test.want {
				t.Fatalf("embedRequestPermission(%s %s) = %v, want %v", test.method, test.path, ok, test.want)
			}
		})
	}
}

func TestEmbeddedFileMutationPassesGatewayWithFilesGrantOnly(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	g.deps.ServiceInstanceID = testServiceInstanceID
	g.deps.Config.MaxBodyBytes = 1024
	claims := embedClaims{HostID: testHostID, ActorID: testUserID, PortalTokenID: "t1234567", ConversationID: "c1234567",
		ServiceInstanceID: testServiceInstanceID, PortalOrigin: "https://portal.example.test", InitialTarget: app.HostEmbedTargetFiles,
		TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetFiles, PermissionCeiling: host.PermissionDesktopControl}},
		EmbedNonce:   "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", RuntimeGeneration: 1, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	cookie := embedCookieName + "=" + g.signEmbedClaims(claims)

	allowed := request(handler, http.MethodPost, hostAuthority(), "/v1/file/mkdir", strings.NewReader(`{"path":"/home/gem/workspace/new"}`), cookie, "")
	if allowed.Code != http.StatusNoContent || <-seen != "POST /v1/file/mkdir" {
		t.Fatalf("embedded file mutation response = %d %q", allowed.Code, allowed.Body.String())
	}
	denied := request(handler, http.MethodPost, hostAuthority(), "/v1/shell/exec", strings.NewReader(`{"command":"id"}`), cookie, "")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("embedded general shell execution response = %d %q", denied.Code, denied.Body.String())
	}
	select {
	case unexpected := <-seen:
		t.Fatalf("unexpected upstream request: %q", unexpected)
	default:
	}
}

func TestEmbeddedVSCodeRequestsPassGatewayWithVSCodeGrant(t *testing.T) {
	seen := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	handler, g, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
	runtime.ref.ControlState = host.ControlUser
	g.deps.ServiceInstanceID = testServiceInstanceID
	g.deps.Config.MaxBodyBytes = 1024
	claims := embedClaims{HostID: testHostID, ActorID: testUserID, PortalTokenID: "t1234567", ConversationID: "c1234567",
		ServiceInstanceID: testServiceInstanceID, PortalOrigin: "https://portal.example.test", InitialTarget: app.HostEmbedTargetVSCode,
		TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetVSCode, PermissionCeiling: host.PermissionDesktopControl}},
		EmbedNonce:   "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", RuntimeGeneration: 1, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	cookie := embedCookieName + "=" + g.signEmbedClaims(claims)

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/code-server/?folder=%2Fhome%2Fgem%2Fworkspace"},
		{method: http.MethodPost, path: "/code-server/mint-key", body: `{}`},
	} {
		response := request(handler, test.method, hostAuthority(), test.path, strings.NewReader(test.body), cookie, "")
		if response.Code != http.StatusNoContent {
			t.Fatalf("embedded VS Code request %s %s = %d %q", test.method, test.path, response.Code, response.Body.String())
		}
		if got := <-seen; got != test.method+" "+strings.Split(test.path, "?")[0] {
			t.Fatalf("upstream request = %q", got)
		}
	}
}

func TestEmbeddedTerminalRoutesRequireTerminalTargetAndValidSession(t *testing.T) {
	claims := embedClaims{TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetTerminal, PermissionCeiling: host.PermissionTerminalControl}}}
	for _, test := range []struct {
		name      string
		method    string
		path      string
		websocket bool
		want      bool
	}{
		{name: "create or reuse terminal session", method: http.MethodPut, path: "/__ch/api/terminal-session", want: true},
		{name: "terminal page", method: http.MethodGet, path: "/terminal?session_id=s-1", want: true},
		{name: "terminal websocket", method: http.MethodGet, path: "/v1/shell/ws?session_id=s-1", websocket: true, want: true},
		{name: "shell execution API remains denied", method: http.MethodPost, path: "/v1/shell/exec"},
		{name: "missing session is denied", method: http.MethodGet, path: "/terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.websocket {
				request.Header.Set("Upgrade", "websocket")
				request.Header.Set("Connection", "keep-alive, Upgrade")
			}
			_, ok := embedRequestPermission(request, claims)
			if ok != test.want {
				t.Fatalf("embedRequestPermission(%s %s) = %v, want %v", test.method, test.path, ok, test.want)
			}
		})
	}
}

func TestEmbeddedVSCodeRoutesRequireVSCodeTargetAndBrowserMethods(t *testing.T) {
	vscode := embedClaims{TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetVSCode, PermissionCeiling: host.PermissionDesktopControl}}}
	files := embedClaims{TargetGrants: []app.HostEmbedTargetGrant{{Target: app.HostEmbedTargetFiles, PermissionCeiling: host.PermissionDesktopControl}}}
	for _, test := range []struct {
		name       string
		method     string
		path       string
		upgrade    string
		connection string
		claims     embedClaims
		want       bool
	}{
		{name: "editor page", method: http.MethodGet, path: "/code-server/", claims: vscode, want: true},
		{name: "editor asset", method: http.MethodGet, path: "/code-server/static/main.js", claims: vscode, want: true},
		{name: "mint key", method: http.MethodPost, path: "/code-server/mint-key", claims: vscode, want: true},
		{name: "editor API write", method: http.MethodPut, path: "/code-server/api/resource", claims: vscode, want: true},
		{name: "editor API patch", method: http.MethodPatch, path: "/code-server/api/resource", claims: vscode, want: true},
		{name: "editor API delete", method: http.MethodDelete, path: "/code-server/api/resource", claims: vscode, want: true},
		{name: "editor preflight", method: http.MethodOptions, path: "/code-server/api/resource", claims: vscode, want: true},
		{name: "editor websocket", method: http.MethodGet, path: "/code-server/", upgrade: "websocket", connection: "keep-alive, Upgrade", claims: vscode, want: true},
		{name: "files target cannot open VS Code", method: http.MethodGet, path: "/code-server/", claims: files},
		{name: "invalid websocket", method: http.MethodGet, path: "/code-server/", connection: "Upgrade", claims: vscode},
		{name: "POST websocket", method: http.MethodPost, path: "/code-server/", upgrade: "websocket", connection: "Upgrade", claims: vscode},
		{name: "trace", method: http.MethodTrace, path: "/code-server/", claims: vscode},
		{name: "connect", method: http.MethodConnect, path: "/code-server/", claims: vscode},
		{name: "path lookalike", method: http.MethodGet, path: "/code-server.evil/", claims: vscode},
		{name: "general shell execution remains denied", method: http.MethodPost, path: "/v1/shell/exec", claims: vscode},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.upgrade != "" {
				request.Header.Set("Upgrade", test.upgrade)
			}
			if test.connection != "" {
				request.Header.Set("Connection", test.connection)
			}
			_, ok := embedRequestPermission(request, test.claims)
			if ok != test.want {
				t.Fatalf("embedRequestPermission(%s %s) = %v, want %v", test.method, test.path, ok, test.want)
			}
		})
	}
}

func ginContextForEmbed(t *testing.T, g *gateway, request *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	ctx.Set(hostIDKey, testHostID)
	ctx.Set(parsedHostKey, ParsedHost{HostID: testHostID})
	return ctx, writer
}
