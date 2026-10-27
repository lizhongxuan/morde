package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type clientLoginHTTPAuth struct {
	api.AuthService
	credentials *app.CredentialService
}

type clientLoginHTTPAdmin struct{ api.AdminService }

func (clientLoginHTTPAdmin) ListUsers(context.Context, identity.Principal, *identity.AccountStatus) ([]api.AdminUserRecord, error) {
	return []api.AdminUserRecord{}, nil
}

func (a clientLoginHTTPAuth) Logout(ctx context.Context, principal identity.Principal) error {
	return a.credentials.Logout(ctx, principal)
}

func clientLoginFixture(t *testing.T) (*Store, *app.CredentialService, *app.ClientLoginService, identity.User) {
	t.Helper()
	store, pool := newTemporaryStore(t)
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	hash, err := bcrypt.GenerateFromPassword([]byte("client-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE users SET password_hash=$2 WHERE id=$1`, owner.ID, string(hash)); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: 12 * time.Hour, TicketTTL: time.Minute, LoginLimit: 50, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	clients, err := app.NewClientLoginService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return store, credentials, clients, owner
}

func TestClientLoginAtomicCredentialsReplayRenewalAndLogout(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login := func(key, renewalID string) app.ClientLoginResult {
		t.Helper()
		result, err := clients.Login(ctx, owner.Name, "client-password-123", "desktop", "laptop", renewalID, key, "192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := login("login-key-0001", "")
	if first.InstallationID == "" || first.ClientCredential == "" || first.SyncCredential == "" || first.WebSessionCredential == "" {
		t.Fatalf("desktop credentials incomplete: %#v", first)
	}
	var ordinarySessions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tokens WHERE owner_id=$1 AND kind='session' AND client_installation_id IS NULL`, owner.ID).Scan(&ordinarySessions); err != nil || ordinarySessions != 0 {
		t.Fatalf("client login created %d ordinary Portal sessions: %v", ordinarySessions, err)
	}
	if _, err := clients.Login(ctx, owner.Name, "client-password-123", "desktop", "different laptop", "", "login-key-0001", "192.0.2.1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("idempotency key reuse for changed request = %v", err)
	}
	clientPrincipal, err := credentials.VerifyClient(ctx, first.ClientCredential)
	if err != nil || clientPrincipal.ClientInstallationID != first.InstallationID {
		t.Fatalf("client verification = %#v, %v", clientPrincipal, err)
	}
	devicePrincipal, err := credentials.VerifyDevice(ctx, first.SyncCredential)
	if err != nil || devicePrincipal.ClientInstallationID != first.InstallationID || !devicePrincipal.AllowsWorkspace("w1234567") {
		t.Fatalf("dynamic Sync verification = %#v, %v", devicePrincipal, err)
	}
	webPrincipal, err := credentials.VerifySession(ctx, first.WebSessionCredential)
	if err != nil || webPrincipal.ClientInstallationID != first.InstallationID || webPrincipal.Role != identity.RoleUser {
		t.Fatalf("Web session verification = %#v, %v", webPrincipal, err)
	}
	replay := login("login-key-0001", "")
	if replay.InstallationID != first.InstallationID || replay.ClientCredential == first.ClientCredential {
		t.Fatalf("idempotent replay failed to rotate secret: %#v", replay)
	}
	if _, err := credentials.VerifyClient(ctx, first.ClientCredential); err == nil {
		t.Fatal("old Client credential survived retry")
	}
	renewed := login("login-key-0002", first.InstallationID)
	if renewed.InstallationID != first.InstallationID {
		t.Fatalf("renewal created duplicate installation: %s", renewed.InstallationID)
	}
	lateRetry := login("login-key-0001", "")
	if lateRetry.InstallationID != first.InstallationID {
		t.Fatalf("old idempotency key created a duplicate installation: %s", lateRetry.InstallationID)
	}
	current, err := credentials.VerifyClient(ctx, lateRetry.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	web, err := clients.WebSession(ctx, current)
	if err != nil || web.Credential == "" || web.ExpiresAt.After(time.Now().Add(time.Hour+time.Second)) {
		t.Fatalf("Web renewal = %#v, %v", web, err)
	}
	principal, err := credentials.VerifySession(ctx, web.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.LogoutWebSession(ctx, principal, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	var actorKind string
	if err := store.pool.QueryRow(ctx, `SELECT actor_kind FROM audit_logs WHERE action='client.revoke' AND target=$1`, first.InstallationID).Scan(&actorKind); err != nil || actorKind != "session" {
		t.Fatalf("Web revocation actor = %q, %v", actorKind, err)
	}
	for _, raw := range []string{lateRetry.ClientCredential, lateRetry.SyncCredential, web.Credential} {
		kind, _, _, err := identity.ParseCredential(raw)
		if err != nil {
			t.Fatal(err)
		}
		var verifyErr error
		switch kind {
		case identity.KindClient:
			_, verifyErr = credentials.VerifyClient(ctx, raw)
		case identity.KindDevice:
			_, verifyErr = credentials.VerifyDevice(ctx, raw)
		case identity.KindSession:
			_, verifyErr = credentials.VerifySession(ctx, raw)
		}
		if verifyErr == nil {
			t.Fatalf("%s remained active after logout", kind)
		}
	}
	if _, err := clients.WebSession(ctx, current); err == nil {
		t.Fatal("revoked installation renewed a Web session")
	}
	items, err := store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(items) != 1 || items[0].RevokedAt == nil {
		t.Fatalf("installation list = %#v, %v", items, err)
	}
}

func TestClientLoginMobileAndCrossAccountRenewal(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	result, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "login-mobile-001", "192.0.2.1")
	if err != nil || result.ClientCredential == "" || result.SyncCredential != "" || result.WebSessionCredential != "" {
		t.Fatalf("mobile login = %#v, %v", result, err)
	}
	principal, err := credentials.VerifyClient(ctx, result.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	web, err := clients.WebSession(ctx, principal)
	if err != nil || web.Credential == "" || web.ExpiresAt.After(time.Now().Add(time.Hour+time.Second)) {
		t.Fatalf("iOS Web session = %#v, %v", web, err)
	}
	webPrincipal, err := credentials.VerifySession(ctx, web.Credential)
	if err != nil || webPrincipal.ClientInstallationID != result.InstallationID || webPrincipal.Role != identity.RoleUser {
		t.Fatalf("iOS Web principal = %#v, %v", webPrincipal, err)
	}
	_, webTokenID, _, err := identity.ParseCredential(web.Credential)
	if err != nil {
		t.Fatal(err)
	}
	var webActorKind string
	if err := store.pool.QueryRow(ctx, `SELECT actor_kind FROM audit_logs WHERE action='client.web_session' AND target=$1`, webTokenID).Scan(&webActorKind); err != nil || webActorKind != "client" {
		t.Fatalf("Web session actor = %q, %v", webActorKind, err)
	}
	other := createTestUser(t, store, 3, 10_000, 20_000)
	hash, err := bcrypt.GenerateFromPassword([]byte("client-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, other.ID, string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Login(ctx, other.Name, "client-password-123", "ios", "phone", result.InstallationID, "login-mobile-002", "192.0.2.2"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-account renewal = %v", err)
	}
	items, err := store.ListClientInstallations(ctx, other.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-account installation rows = %#v, %v", items, err)
	}
	otherLogin, err := clients.Login(ctx, other.Name, "client-password-123", "ios", "other phone", "", "login-mobile-003", "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.Revoke(ctx, principal, otherLogin.InstallationID, "192.0.2.1"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-account revoke = %v", err)
	}
	listed, err := clients.List(ctx, principal)
	if err != nil || len(listed) != 1 || listed[0].ID != result.InstallationID {
		t.Fatalf("client installation list = %#v, %v", listed, err)
	}
	if err := clients.Revoke(ctx, principal, result.InstallationID, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	var actorKind string
	if err := store.pool.QueryRow(ctx, `SELECT actor_kind FROM audit_logs WHERE action='client.revoke' AND target=$1`, result.InstallationID).Scan(&actorKind); err != nil || actorKind != "client" {
		t.Fatalf("Client revocation actor = %q, %v", actorKind, err)
	}
	if _, err := credentials.VerifyClient(ctx, result.ClientCredential); err == nil {
		t.Fatal("revoked iOS Client credential remained active")
	}
	if _, err := credentials.VerifySession(ctx, web.Credential); err == nil {
		t.Fatal("revoked iOS Web credential remained active")
	}
	if _, err := credentials.VerifyClient(ctx, otherLogin.ClientCredential); err != nil {
		t.Fatalf("other account's installation was affected: %v", err)
	}
	android, err := clients.Login(ctx, owner.Name, "client-password-123", "android", "android", "", "login-mobile-004", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	androidPrincipal, err := credentials.VerifyClient(ctx, android.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.WebSession(ctx, androidPrincipal); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Android Client unexpectedly obtained a Web session: %v", err)
	}
}

func TestClientLoginRejectsInvalidAccountAndPasswordWithoutInstallation(t *testing.T) {
	store, _, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login := func(key, password string) error {
		_, err := clients.Login(ctx, owner.Name, password, "desktop", "laptop", "", key, "192.0.2.1")
		return err
	}
	if err := login("login-wrong-0001", "bad password"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("wrong password = %v", err)
	}
	for _, state := range []struct {
		status string
		code   domain.ErrorCode
	}{
		{status: "pending", code: domain.CodeAccountPending},
		{status: "disabled", code: domain.CodeAccountDisabled},
	} {
		if _, err := store.pool.Exec(ctx, `UPDATE users SET status=$2 WHERE id=$1`, owner.ID, state.status); err != nil {
			t.Fatal(err)
		}
		if err := login("login-status-"+state.status, "client-password-123"); domain.CodeOf(err) != state.code {
			t.Fatalf("%s account login = %v", state.status, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE users SET status='approved',must_change_password=true WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := login("login-forced-password", "client-password-123"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("forced password change login = %v", err)
	}
	items, err := store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("rejected login created installations: %#v, %v", items, err)
	}
}

func TestClientLoginConcurrentRetryAndAuditFailureRollsBack(t *testing.T) {
	store, _, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	results := make(chan app.ClientLoginResult, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := clients.Login(ctx, owner.Name, "client-password-123", "desktop", "laptop", "", "login-concurrent-1", "192.0.2.1")
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var installationID string
	for result := range results {
		if installationID != "" && result.InstallationID != installationID {
			t.Fatalf("concurrent retry duplicated installations: %s / %s", installationID, result.InstallationID)
		}
		installationID = result.InstallationID
	}
	items, err := store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("installations = %#v, %v", items, err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT reject_client_login_for_test CHECK (action <> 'client.login') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Login(ctx, owner.Name, "client-password-123", "android", "phone", "", "login-rollback-1", "192.0.2.1"); err == nil {
		t.Fatal("audit failure did not fail login")
	}
	items, err = store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("audit failure left an orphan installation: %#v, %v", items, err)
	}
}

func TestClientLoginHTTPPostgresSessionFenceAndLogout(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	admin, err := store.GetUserByID(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin.Role = identity.RoleAdmin
	if _, err := store.UpdateUser(ctx, "system", admin.Status, admin); err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts: unusedWorkspaceSyncHostService{}, TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier: credentials, Auth: clientLoginHTTPAuth{credentials: credentials}, ClientLogin: clients, Admin: clientLoginHTTPAdmin{},
		MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20,
		SessionTTL: 12 * time.Hour, HostScheme: "https", PortalDomain: "portal.example.test",
		HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
		PortalFiles: fstest.MapFS{"admin/index.html": &fstest.MapFile{Data: []byte("<html>admin</html>")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, bearer, cookie, origin, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "portal.example.test"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "ch_portal", Value: cookie})
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	loginBody := `{"name":"` + owner.Name + `","password":"client-password-123","platform":"desktop","display_name":"laptop"}`
	login := request(http.MethodPost, "/api/auth/client-login", loginBody, "", "", "", "http-login-001")
	if login.Code != http.StatusOK || login.Header().Get("Set-Cookie") != "" {
		t.Fatalf("client login = %d %s", login.Code, login.Body)
	}
	var logged struct {
		ClientCredential     string `json:"client_credential"`
		WebSessionCredential string `json:"web_session_credential"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &logged); err != nil || logged.ClientCredential == "" || logged.WebSessionCredential == "" {
		t.Fatalf("login body = %s, %v", login.Body, err)
	}
	if res := request(http.MethodGet, "/api/clients", "", "", logged.WebSessionCredential, "", ""); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"items"`) {
		t.Fatalf("user Web clients = %d %s", res.Code, res.Body)
	}
	for _, path := range []string{"/api/admin/users", "/admin/"} {
		if res := request(http.MethodGet, path, "", "", logged.WebSessionCredential, "", ""); res.Code != http.StatusForbidden {
			t.Fatalf("installed Web admin route %s = %d %s", path, res.Code, res.Body)
		}
	}
	_, webTokenID, _, err := identity.ParseCredential(logged.WebSessionCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET expires_at=now()-interval '1 second' WHERE id=$1`, webTokenID); err != nil {
		t.Fatal(err)
	}
	if res := request(http.MethodGet, "/api/clients", "", "", logged.WebSessionCredential, "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("expired Web session accessed user route = %d %s", res.Code, res.Body)
	}
	renewal := request(http.MethodPost, "/api/auth/client-web-session", "", logged.ClientCredential, "", "", "")
	var renewed struct {
		Credential string `json:"web_session_credential"`
	}
	if renewal.Code != http.StatusOK || json.Unmarshal(renewal.Body.Bytes(), &renewed) != nil || renewed.Credential == "" || renewed.Credential == logged.WebSessionCredential {
		t.Fatalf("expired Web session renewal = %d %s", renewal.Code, renewal.Body)
	}
	if res := request(http.MethodGet, "/api/clients", "", "", renewed.Credential, "", ""); res.Code != http.StatusOK {
		t.Fatalf("renewed Web session user route = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodPost, "/api/auth/logout", "", "", renewed.Credential, "", ""); res.Code != http.StatusForbidden {
		t.Fatalf("missing Origin logout = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodPost, "/api/auth/logout", "", "", renewed.Credential, "https://portal.example.test", ""); res.Code != http.StatusNoContent {
		t.Fatalf("installed Web logout = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodPost, "/api/auth/client-web-session", "", logged.ClientCredential, "", "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked client renewed Web = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodGet, "/api/clients", "", "", renewed.Credential, "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked Web session accessed user route = %d %s", res.Code, res.Body)
	}
	iosBody := `{"name":"` + owner.Name + `","password":"client-password-123","platform":"ios","display_name":"phone"}`
	iosLogin := request(http.MethodPost, "/api/auth/client-login", iosBody, "", "", "", "http-login-ios-01")
	var ios struct {
		Credential     string `json:"client_credential"`
		InstallationID string `json:"installation_id"`
	}
	if iosLogin.Code != http.StatusOK || json.Unmarshal(iosLogin.Body.Bytes(), &ios) != nil || ios.Credential == "" || ios.InstallationID == "" {
		t.Fatalf("iOS login = %d %s", iosLogin.Code, iosLogin.Body)
	}
	iosWebResult := request(http.MethodPost, "/api/auth/client-web-session", "", ios.Credential, "", "", "")
	var iosWeb struct {
		Credential string `json:"web_session_credential"`
	}
	if iosWebResult.Code != http.StatusOK || json.Unmarshal(iosWebResult.Body.Bytes(), &iosWeb) != nil || iosWeb.Credential == "" {
		t.Fatalf("iOS Web session = %d %s", iosWebResult.Code, iosWebResult.Body)
	}
	if res := request(http.MethodGet, "/api/admin/users", "", "", iosWeb.Credential, "", ""); res.Code != http.StatusForbidden {
		t.Fatalf("iOS Web session accessed admin = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodGet, "/api/clients", "", ios.Credential, "", "", ""); res.Code != http.StatusOK {
		t.Fatalf("iOS installation list = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodDelete, "/api/clients/"+ios.InstallationID, "", ios.Credential, "", "", ""); res.Code != http.StatusNoContent {
		t.Fatalf("iOS installation revoke = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodGet, "/api/clients", "", ios.Credential, "", "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked iOS Client still works = %d %s", res.Code, res.Body)
	}
	if res := request(http.MethodGet, "/api/clients", "", "", iosWeb.Credential, "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked iOS Web session still works = %d %s", res.Code, res.Body)
	}
	portal, err := credentials.Login(context.Background(), owner.Name, "client-password-123", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/users", "/admin/"} {
		if res := request(http.MethodGet, path, "", "", portal.Credential, "", ""); res.Code != http.StatusOK {
			t.Fatalf("independent admin session route %s = %d %s", path, res.Code, res.Body)
		}
	}
	if res := request(http.MethodPost, "/api/auth/logout", "", "", portal.Credential, "https://portal.example.test", ""); res.Code != http.StatusNoContent {
		t.Fatalf("ordinary Portal logout = %d %s", res.Code, res.Body)
	}
	if _, err := credentials.VerifySession(context.Background(), portal.Credential); err == nil {
		t.Fatal("ordinary Portal logout did not revoke its session")
	}
}

func TestClientLoginMigrationRetiresLegacyPairingAndCredentials(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 46); err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	legacy := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	code := sha256.Sum256([]byte("old-code"))
	if _, err := pool.Exec(ctx, `INSERT INTO client_authorizations
  (id,display_name,platform,wants_workspace_sync,secret_hash,user_code_hash,state,expires_at)
  VALUES ($1,'old phone','android',false,$2,$3,'pending',now()+interval '10 minutes')`, "t7654321", code[:], code[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 47); err != nil {
		t.Fatal(err)
	}
	var codes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_authorizations`).Scan(&codes); err != nil || codes != 0 {
		t.Fatalf("old pairing codes = %d, %v", codes, err)
	}
	installation, err := store.GetClientInstallation(ctx, installationID)
	if err != nil || installation.RevokedAt == nil {
		t.Fatalf("old installation = %#v, %v", installation, err)
	}
	token, err := store.GetTokenByID(ctx, legacy.ID)
	if err != nil || token.RevokedAt == nil {
		t.Fatalf("old Client credential = %#v, %v", token, err)
	}
}
