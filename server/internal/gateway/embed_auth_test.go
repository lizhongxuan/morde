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

func ginContextForEmbed(t *testing.T, g *gateway, request *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	ctx.Set(hostIDKey, testHostID)
	ctx.Set(parsedHostKey, ParsedHost{HostID: testHostID})
	return ctx, writer
}
