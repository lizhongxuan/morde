package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/workspace"
)

const (
	testUserID      = "u1234567"
	testHostID      = "h1234567"
	testWorkspaceID = "w1234567"
	testTokenID     = "t1234567"
)

type hostServiceStub struct {
	item             host.Host
	err              error
	certificates     map[string]bool
	created          app.CreateHostCommand
	deleted          app.DeleteHostCommand
	principal        identity.Principal
	listResult       []host.Host
	enterResult      app.EnterInfo
	previewPort      int
	previewCalls     int
	bindingResult    workspace.Binding
	bindHostID       string
	bindWorkspaceID  string
	bindRevision     *int64
	bindVersion      int64
	unbindVersion    int64
	idempotencyKey   string
	writersStopped   bool
	unbindCalls      int
	releaseConfirmed bool
}

func (s *hostServiceStub) Create(_ context.Context, p identity.Principal, cmd app.CreateHostCommand) (host.Host, error) {
	s.principal, s.created = p, cmd
	return s.item, s.err
}
func (s *hostServiceStub) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return s.item, false, s.err
}
func (s *hostServiceStub) List(_ context.Context, p identity.Principal) ([]host.Host, error) {
	s.principal = p
	return s.listResult, s.err
}
func (s *hostServiceStub) Get(_ context.Context, p identity.Principal, _ string) (host.Host, error) {
	s.principal = p
	return s.item, s.err
}
func (s *hostServiceStub) BindWorkspace(_ context.Context, p identity.Principal, hostID, workspaceID string, revision *int64, bindingVersion int64, key string) (workspace.Binding, error) {
	s.principal = p
	s.bindHostID, s.bindWorkspaceID, s.bindRevision, s.idempotencyKey = hostID, workspaceID, revision, key
	s.bindVersion = bindingVersion
	return s.bindingResult, s.err
}
func (s *hostServiceStub) UnbindWorkspace(_ context.Context, p identity.Principal, hostID, workspaceID string, bindingVersion int64, writersStopped bool, key string) (workspace.Binding, error) {
	s.principal = p
	s.bindHostID, s.bindWorkspaceID, s.idempotencyKey = hostID, workspaceID, key
	s.unbindVersion = bindingVersion
	s.writersStopped = writersStopped
	s.unbindCalls++
	return s.bindingResult, s.err
}
func (s *hostServiceStub) RetryWorkspaceSync(_ context.Context, p identity.Principal, hostID, workspaceID string, bindingVersion int64) (workspace.Binding, error) {
	s.principal = p
	s.bindHostID, s.bindWorkspaceID, s.unbindVersion = hostID, workspaceID, bindingVersion
	return s.bindingResult, s.err
}
func (s *hostServiceStub) AuthorizeTLS(_ context.Context, hostID string) bool {
	return s.certificates[hostID]
}
func (s *hostServiceStub) Delete(_ context.Context, p identity.Principal, cmd app.DeleteHostCommand) (host.Host, error) {
	s.principal, s.deleted = p, cmd
	return s.item, s.err
}
func (s *hostServiceStub) Release(_ context.Context, p identity.Principal, _ string, confirmProcessTermination bool) (host.Host, error) {
	s.principal = p
	s.releaseConfirmed = confirmProcessTermination
	return s.item, s.err
}
func (s *hostServiceStub) Restore(_ context.Context, p identity.Principal, _ string) (host.Host, error) {
	s.principal = p
	return s.item, s.err
}
func (s *hostServiceStub) Renew(_ context.Context, p identity.Principal, _ string) (host.Host, error) {
	s.principal = p
	return s.item, s.err
}
func (s *hostServiceStub) IssueTicket(_ context.Context, p identity.Principal, _ string) (app.EnterInfo, error) {
	s.principal = p
	return s.enterResult, s.err
}

func (s *hostServiceStub) IssuePreviewTicket(_ context.Context, principal identity.Principal, _ string, port int) (app.EnterInfo, error) {
	s.principal, s.previewPort = principal, port
	s.previewCalls++
	return s.enterResult, s.err
}

type verifierStub struct {
	err        error
	role       identity.Role
	mustChange bool
}

func (s verifierStub) VerifySession(context.Context, string) (identity.Principal, error) {
	return identity.Principal{UserID: testUserID, Role: s.roleOrUser(), MustChangePassword: s.mustChange, Kind: identity.KindSession, TokenID: testTokenID}, s.err
}
func (s verifierStub) VerifyAPIKey(context.Context, string) (identity.Principal, error) {
	return identity.Principal{UserID: testUserID, Role: s.roleOrUser(), Kind: identity.KindAPIKey, TokenID: testTokenID}, s.err
}
func (s verifierStub) VerifyDevice(context.Context, string) (identity.Principal, error) {
	return identity.Principal{
		UserID: testUserID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: testTokenID,
		WorkspaceIDs: []string{testWorkspaceID}, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}, s.err
}
func (s verifierStub) VerifyClient(context.Context, string) (identity.Principal, error) {
	return identity.Principal{UserID: testUserID, Role: s.roleOrUser(), Kind: identity.KindClient,
		TokenID: testTokenID, ClientInstallationID: "i1234567"}, s.err
}
func (s verifierStub) ConsumeTicket(context.Context, string) (app.Ticket, error) {
	return app.Ticket{}, errors.New("unexpected ticket consumption")
}

func (s verifierStub) roleOrUser() identity.Role {
	if s.role == "" {
		return identity.RoleUser
	}
	return s.role
}

type authServiceStub struct {
	registerUser UserView
	registerErr  error
	registerName string
	registerPass string
	registerIP   string
	loginResult  LoginResult
	loginErr     error
	logoutErr    error
	changeErr    error
	changeCalls  int
	changeOld    string
	changeNew    string
	changeP      identity.Principal
	current      UserView
	currentErr   error
	logoutCalls  int
	loginName    string
	loginPass    string
	loginIP      string
}

func (s *authServiceStub) Register(_ context.Context, name, password, ip string) (UserView, error) {
	s.registerName, s.registerPass, s.registerIP = name, password, ip
	return s.registerUser, s.registerErr
}

func (s *authServiceStub) Login(_ context.Context, name, password, ip string) (LoginResult, error) {
	s.loginName, s.loginPass, s.loginIP = name, password, ip
	return s.loginResult, s.loginErr
}
func (s *authServiceStub) Logout(context.Context, identity.Principal) error {
	s.logoutCalls++
	return s.logoutErr
}
func (s *authServiceStub) ChangePassword(_ context.Context, principal identity.Principal, current, next string) error {
	s.changeCalls++
	s.changeP, s.changeOld, s.changeNew = principal, current, next
	return s.changeErr
}
func (s *authServiceStub) CurrentUser(context.Context, identity.Principal) (UserView, error) {
	return s.current, s.currentErr
}

type workspaceServiceStub struct {
	items       []workspace.Workspace
	deviceItems []app.DeviceWorkspaceInfo
	item        workspace.Workspace
	created     workspace.Workspace
	deleted     workspace.Workspace
	err         error
	listP       identity.Principal
	createP     identity.Principal
	createCmd   app.CreateWorkspaceCommand
	createKey   string
	getID       string
	delID       string
}

func (s *workspaceServiceStub) CreateWorkspace(_ context.Context, p identity.Principal, command app.CreateWorkspaceCommand, key string) (workspace.Workspace, error) {
	s.createP, s.createCmd, s.createKey = p, command, key
	if key == "" {
		return workspace.Workspace{}, domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	return s.created, s.err
}
func (s *workspaceServiceStub) ListWorkspaces(_ context.Context, p identity.Principal) ([]workspace.Workspace, error) {
	s.listP = p
	return s.items, s.err
}
func (s *workspaceServiceStub) ListDeviceWorkspaces(_ context.Context, p identity.Principal) ([]app.DeviceWorkspaceInfo, error) {
	s.listP = p
	return s.deviceItems, s.err
}
func (s *workspaceServiceStub) GetWorkspace(_ context.Context, p identity.Principal, id string) (workspace.Workspace, error) {
	s.listP, s.getID = p, id
	return s.item, s.err
}
func (s *workspaceServiceStub) DeleteWorkspace(_ context.Context, p identity.Principal, id string) (workspace.Workspace, error) {
	s.listP, s.delID = p, id
	return s.deleted, s.err
}

type templateReaderStub struct {
	items []TemplateRecord
	err   error
}

func (s templateReaderStub) ListTemplates(context.Context) ([]TemplateRecord, error) {
	return s.items, s.err
}

type tokenServiceStub struct {
	apiKeys    []identity.Token
	created    CreatedAPIKey
	err        error
	principal  identity.Principal
	tokenID    string
	apiKeyName string
	apiKeyTTL  time.Duration
}

func (s *tokenServiceStub) ListAPIKeys(_ context.Context, p identity.Principal) ([]identity.Token, error) {
	s.principal = p
	if p.Kind != identity.KindSession {
		return nil, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return s.apiKeys, s.err
}
func (s *tokenServiceStub) CreateAPIKey(_ context.Context, p identity.Principal, name string, ttl time.Duration) (CreatedAPIKey, error) {
	s.principal, s.apiKeyName, s.apiKeyTTL = p, name, ttl
	if p.Kind != identity.KindSession {
		return CreatedAPIKey{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return s.created, s.err
}
func (s *tokenServiceStub) DeleteAPIKey(_ context.Context, p identity.Principal, id string) error {
	s.principal, s.tokenID = p, id
	if p.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return s.err
}

type adminServiceStub struct {
	users     []AdminUserRecord
	user      AdminUserRecord
	capacity  CapacityRecord
	audits    []AuditRecord
	err       error
	principal identity.Principal
	status    *identity.AccountStatus
	updatedID string
	updated   UpdateAdminUserCommand
	auditQ    AuditQuery
}

type adminWorkspaceServiceStub struct {
	page      workspace.AdminPage
	item      workspace.AdminSummary
	err       error
	principal identity.Principal
	pageReq   app.PageRequest
	getID     string
}

func (s *adminWorkspaceServiceStub) List(_ context.Context, principal identity.Principal, page app.PageRequest) (workspace.AdminPage, error) {
	s.principal, s.pageReq = principal, page
	return s.page, s.err
}

func (s *adminWorkspaceServiceStub) Get(_ context.Context, principal identity.Principal, id string) (workspace.AdminSummary, error) {
	s.principal, s.getID = principal, id
	return s.item, s.err
}

func (s *adminServiceStub) ListUsers(_ context.Context, p identity.Principal, status *identity.AccountStatus) ([]AdminUserRecord, error) {
	s.principal, s.status = p, status
	return s.users, s.err
}
func (s *adminServiceStub) UpdateUser(_ context.Context, p identity.Principal, id string, command UpdateAdminUserCommand) (AdminUserRecord, error) {
	s.principal, s.updatedID, s.updated = p, id, command
	return s.user, s.err
}
func (s *adminServiceStub) Capacity(_ context.Context, p identity.Principal) (CapacityRecord, error) {
	s.principal = p
	return s.capacity, s.err
}
func (s *adminServiceStub) ListAudit(_ context.Context, p identity.Principal, query AuditQuery) ([]AuditRecord, error) {
	s.principal, s.auditQ = p, query
	return s.audits, s.err
}

type readinessStub struct{ err error }

func (s readinessStub) CheckReady(context.Context) error { return s.err }

func TestHostCreateMapsDTOAndDoesNotExposeCredentialMaterial(t *testing.T) {
	services := newServices()
	services.hosts.item.SandboxID = "private-sandbox-id"
	services.hosts.item.AIOKeyEnc = []byte("encrypted-aio-key")
	router := newRouter(t, services, Options{CookieSecure: true})
	body := `{"name":"work"}`
	response := requestWithHeader(router, http.MethodPost, "/api/hosts", body, "Bearer "+apiKeyCredential(), "", "Idempotency-Key", "route-host-create-01")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if services.hosts.created.Name != "work" || len(services.hosts.created.WorkspaceIDs) != 0 || services.hosts.created.IdempotencyKey != "route-host-create-01" {
		t.Fatalf("create command = %#v", services.hosts.created)
	}
	if services.hosts.principal.Kind != identity.KindAPIKey || services.hosts.principal.UserID != testUserID {
		t.Fatalf("principal = %#v", services.hosts.principal)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["url"] != "https://h1234567.example-hosts.test" {
		t.Fatalf("host url = %v", result["url"])
	}
	if result["runtime_generation"] != float64(1) || result["control_version"] != float64(0) || result["control_state"] != string(host.ControlAI) {
		t.Fatalf("host runtime/control contract = %#v", result)
	}
	if workspaces, ok := result["workspaces"].([]any); !ok || len(workspaces) != 1 {
		t.Fatalf("host workspaces = %#v", result["workspaces"])
	} else if binding, ok := workspaces[0].(map[string]any); !ok || binding["directory"] != testWorkspaceID || binding["binding_version"] != float64(1) {
		t.Fatalf("host binding = %#v", workspaces[0])
	}
	if strings.Contains(response.Body.String(), "private-sandbox-id") || strings.Contains(response.Body.String(), "encrypted-aio-key") || strings.Contains(response.Body.String(), "SANDBOX_API_KEY") {
		t.Fatalf("host response leaked credential material: %s", response.Body)
	}
}

func TestHostCreateRequiresOneValidIdempotencyKey(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	body := `{"template_id":"std-2c4g"}`
	key := "Bearer " + apiKeyCredential()

	assertError(t, request(router, http.MethodPost, "/api/hosts", body, key, ""), http.StatusBadRequest, "invalid_argument")
	if services.hosts.created.IdempotencyKey != "" {
		t.Fatal("host service was called without an idempotency key")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/hosts", strings.NewReader(body))
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Idempotency-Key", "duplicate-key-001")
	req.Header.Add("Idempotency-Key", "duplicate-key-002")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	assertError(t, response, http.StatusBadRequest, "invalid_argument")
	if services.hosts.created.IdempotencyKey != "" {
		t.Fatal("host service was called with duplicate idempotency keys")
	}
}

func TestHostViewRejectsInvalidControlState(t *testing.T) {
	services := newServices()
	services.hosts.item.ControlState = "unknown"
	response := request(newRouter(t, services, Options{}), http.MethodGet, "/api/hosts/"+testHostID, "", "Bearer "+apiKeyCredential(), "")
	assertError(t, response, http.StatusInternalServerError, "internal")
}

func TestListHostWorkspaceBindingsUsesAuthorizedHostView(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/hosts/"+testHostID+"/workspace-bindings", cookie, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var result struct {
		Items []workspaceBindingResponse `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode Workspace bindings: %v; body = %s", err, response.Body)
	}
	if len(result.Items) != 1 || result.Items[0].Workspace.ID != testWorkspaceID ||
		result.Items[0].State != workspace.BindingActive || result.Items[0].AppliedRevision != 2 {
		t.Fatalf("bindings = %#v", result.Items)
	}
	if services.hosts.principal.Kind != identity.KindSession || services.hosts.principal.UserID != testUserID {
		t.Fatalf("principal = %#v", services.hosts.principal)
	}
	apiKeyResponse := request(router, http.MethodGet, "/api/hosts/"+testHostID+"/workspace-bindings", "", "Bearer "+apiKeyCredential(), "")
	if apiKeyResponse.Code != http.StatusOK || services.hosts.principal.Kind != identity.KindAPIKey {
		t.Fatalf("API key response = %d %s, principal = %#v", apiKeyResponse.Code, apiKeyResponse.Body, services.hosts.principal)
	}
	assertError(t, request(router, http.MethodGet, "/api/hosts/"+testHostID+"/workspace-bindings", "", "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestBindAndUnbindHostWorkspaceRoutes(t *testing.T) {
	services := newServices()
	services.hosts.bindingResult = services.hosts.item.Workspaces[0]
	services.hosts.bindingResult.State = workspace.BindingPreparing
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	bind := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/hosts/"+testHostID+"/workspace-bindings",
		`{"workspace_id":"`+testWorkspaceID+`","expected_revision":2,"expected_binding_version":0}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "bind-workspace-01")
	if bind.Code != http.StatusCreated || services.hosts.bindHostID != testHostID || services.hosts.bindWorkspaceID != testWorkspaceID ||
		services.hosts.bindRevision == nil || *services.hosts.bindRevision != 2 || services.hosts.bindVersion != 0 || services.hosts.idempotencyKey != "bind-workspace-01" {
		t.Fatalf("bind status/body/call = %d %s / %#v", bind.Code, bind.Body, services.hosts)
	}

	services.hosts.bindingResult.State = workspace.BindingUnbinding
	missingConfirmation := requestWithCookieBodyAndHeader(router, http.MethodDelete,
		"/api/hosts/"+testHostID+"/workspace-bindings/"+testWorkspaceID, `{"expected_binding_version":2}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "unbind-workspace-00")
	if missingConfirmation.Code != http.StatusBadRequest || services.hosts.unbindCalls != 0 {
		t.Fatalf("unbind without writer-stop confirmation status/calls = %d / %d", missingConfirmation.Code, services.hosts.unbindCalls)
	}
	unbind := requestWithCookieBodyAndHeader(router, http.MethodDelete,
		"/api/hosts/"+testHostID+"/workspace-bindings/"+testWorkspaceID, `{"expected_binding_version":2,"writers_stopped":true}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "unbind-workspace-01")
	if unbind.Code != http.StatusAccepted || services.hosts.bindingResult.State != workspace.BindingUnbinding ||
		services.hosts.idempotencyKey != "unbind-workspace-01" || services.hosts.unbindVersion != 2 || !services.hosts.writersStopped || services.hosts.unbindCalls != 1 {
		t.Fatalf("unbind status/body/call = %d %s / %#v", unbind.Code, unbind.Body, services.hosts)
	}
}

func TestRetryHostWorkspaceSyncRoutePassesCurrentBindingVersion(t *testing.T) {
	services := newServices()
	services.hosts.bindingResult = services.hosts.item.Workspaces[0]
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBody(router, http.MethodPost,
		"/api/hosts/"+testHostID+"/workspace-bindings/"+testWorkspaceID+"/retry-sync",
		`{"expected_binding_version":1}`, cookie, "https://portal.example.test")
	if response.Code != http.StatusOK || services.hosts.principal.Kind != identity.KindSession ||
		services.hosts.bindHostID != testHostID || services.hosts.bindWorkspaceID != testWorkspaceID || services.hosts.unbindVersion != 1 {
		t.Fatalf("retry status/body/call = %d %s / %#v", response.Code, response.Body, services.hosts)
	}
	var result workspaceBindingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.SyncState != workspace.BindingSyncHealthy || len(result.SyncConflicts) != 0 {
		t.Fatalf("retry response = %#v, %v", result, err)
	}
}

func TestAuthRequiredAndApplicationErrorsAreSafe(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	response := request(router, http.MethodGet, "/api/hosts", "", "", "")
	assertError(t, response, http.StatusUnauthorized, "unauthenticated")

	services.hosts.err = &domain.Error{Code: domain.CodeQuotaExceeded, Message: "private-aio-key must not appear", Details: map[string]any{"key": "private-aio-key"}}
	response = requestWithHeader(router, http.MethodPost, "/api/hosts", `{}`, "Bearer "+apiKeyCredential(), "", "Idempotency-Key", "route-error-host-01")
	assertError(t, response, http.StatusConflict, "quota_exceeded")
	if strings.Contains(response.Body.String(), "private-aio-key") || strings.Contains(response.Body.String(), "key\"") {
		t.Fatalf("error response leaked secret details: %s", response.Body)
	}
}

func TestDomainErrorResponsesAreMappedAndSanitized(t *testing.T) {
	tests := []struct {
		code    domain.ErrorCode
		status  int
		message string
	}{
		{domain.CodeInvalidArgument, http.StatusBadRequest, "invalid request"},
		{domain.CodeUnauthenticated, http.StatusUnauthorized, "authentication required"},
		{domain.CodeForbidden, http.StatusForbidden, "forbidden"},
		{domain.CodeNotFound, http.StatusNotFound, "not found"},
		{domain.CodeQuotaExceeded, http.StatusConflict, "quota exceeded"},
		{domain.CodeCapacityExceeded, http.StatusConflict, "capacity exceeded"},
		{domain.CodeWorkspaceBusy, http.StatusConflict, "workspace busy"},
		{domain.CodeInvalidState, http.StatusConflict, "invalid state"},
		{domain.CodeConflict, http.StatusConflict, "conflict"},
		{domain.CodeIdempotencyConflict, http.StatusConflict, "request conflicts with an earlier operation"},
		{domain.CodeEventCursorExpired, http.StatusConflict, "event cursor expired; fetch a fresh snapshot"},
		{domain.CodeUnsupported, http.StatusUnprocessableEntity, "operation unsupported"},
		{domain.CodeRequestTooLarge, http.StatusRequestEntityTooLarge, "request body too large"},
		{domain.CodeRateLimited, http.StatusTooManyRequests, "rate limit exceeded"},
		{domain.CodeUpstreamError, http.StatusBadGateway, "upstream error"},
		{domain.CodeHostNotReady, http.StatusServiceUnavailable, "host is not ready"},
		{domain.CodeUpstreamTimeout, http.StatusGatewayTimeout, "upstream timeout"},
		{domain.CodeInternal, http.StatusInternalServerError, "internal error"},
		{domain.ErrorCode("unknown"), http.StatusInternalServerError, "internal error"},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			services := newServices()
			services.hosts.err = &domain.Error{
				Code: tt.code, Message: "private credential must not be returned",
				Details: map[string]any{"field": "workspace_ids", "secret": "private credential"},
			}
			response := request(newRouter(t, services, Options{}), http.MethodGet, "/api/hosts", "", "Bearer "+apiKeyCredential(), "")
			if response.Code != tt.status {
				t.Fatalf("status = %d, body = %s; want %d", response.Code, response.Body, tt.status)
			}
			if strings.Contains(response.Body.String(), "private credential") {
				t.Fatalf("error response leaked details: %s", response.Body)
			}
			var body struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantCode := string(tt.code)
			if tt.code == domain.ErrorCode("unknown") {
				wantCode = string(domain.CodeInternal)
			}
			if body.Error.Code != wantCode || body.Error.Message != tt.message {
				t.Fatalf("error = %#v; want code=%q message=%q", body.Error, wantCode, tt.message)
			}
			if tt.status < http.StatusInternalServerError && body.Error.Details["field"] != "workspace_ids" {
				t.Fatalf("safe field detail was lost: %#v", body.Error.Details)
			}
		})
	}
}

func TestAuthenticationAndHandlerFailureBranches(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})

	invalidBearer := request(router, http.MethodGet, "/api/hosts", "", "Basic invalid", "")
	assertError(t, invalidBearer, http.StatusUnauthorized, "unauthenticated")
	malformedCredential := request(router, http.MethodGet, "/api/hosts", "", "Bearer chk_bad", "")
	assertError(t, malformedCredential, http.StatusUnauthorized, "unauthenticated")
	conflictingAuth := requestWithConflictingCredentials(router)
	assertError(t, conflictingAuth, http.StatusUnauthorized, "unauthenticated")

	services.auth.loginErr = errors.New("private password detail")
	loginErr := request(router, http.MethodPost, "/api/auth/login", `{"name":"alice","password":"secret"}`, "", "")
	if loginErr.Code != http.StatusInternalServerError || strings.Contains(loginErr.Body.String(), "private password detail") {
		t.Fatalf("login error response = %d %s", loginErr.Code, loginErr.Body)
	}
	services.auth.loginErr = nil
	badLogin := request(router, http.MethodPost, "/api/auth/login", `{"name":" ","password":"secret"}`, "", "")
	assertError(t, badLogin, http.StatusBadRequest, "invalid_argument")
	badJSON := request(router, http.MethodPost, "/api/auth/login", `{"name":`, "", "")
	assertError(t, badJSON, http.StatusBadRequest, "invalid_argument")
	wrongContentType := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"name":"alice"}`))
	wrongContentType.Header.Set("Content-Type", "text/plain")
	wrongContentResponse := httptest.NewRecorder()
	router.ServeHTTP(wrongContentResponse, wrongContentType)
	assertError(t, wrongContentResponse, http.StatusBadRequest, "invalid_argument")

	services.auth.loginErr = nil
	services.auth.loginResult = LoginResult{
		Credential: sessionCredential(), ExpiresAt: time.Now().Add(time.Hour),
		User: UserView{ID: testUserID, Name: "alice", Role: identity.RoleUser},
	}
	loginThroughTrustedProxy := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"name":"alice","password":"secret"}`))
	loginThroughTrustedProxy.RemoteAddr = "127.0.0.1:12345"
	loginThroughTrustedProxy.Header.Set("Content-Type", "application/json")
	loginThroughTrustedProxy.Header.Set("X-Forwarded-For", "203.0.113.9")
	proxyResponse := httptest.NewRecorder()
	router.ServeHTTP(proxyResponse, loginThroughTrustedProxy)
	if proxyResponse.Code != http.StatusOK {
		t.Fatalf("proxy login status = %d, body = %s", proxyResponse.Code, proxyResponse.Body)
	}
	if services.auth.loginIP != "203.0.113.9" {
		t.Fatalf("trusted proxy client IP = %q", services.auth.loginIP)
	}

	verificationServices := newServices()
	verificationServices.verifier.err = errors.New("private verifier detail")
	verifyError := request(newRouter(t, verificationServices, Options{}), http.MethodGet, "/api/hosts", "", "Bearer "+apiKeyCredential(), "")
	assertError(t, verifyError, http.StatusUnauthorized, "unauthenticated")
	if strings.Contains(verifyError.Body.String(), "private verifier detail") {
		t.Fatalf("verifier error leaked: %s", verifyError.Body)
	}

	services.auth.logoutErr = errors.New("private logout detail")
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	missingJSONType := httptest.NewRequest(http.MethodPost, "/api/auth/logout", strings.NewReader(`{}`))
	missingJSONType.Host = "portal.example.test"
	missingJSONType.AddCookie(cookie)
	missingJSONType.Header.Set("Origin", "https://portal.example.test")
	missingJSONTypeResponse := httptest.NewRecorder()
	router.ServeHTTP(missingJSONTypeResponse, missingJSONType)
	assertError(t, missingJSONTypeResponse, http.StatusForbidden, "forbidden")

	logoutErr := requestWithCookie(router, http.MethodPost, "/api/auth/logout", cookie, "https://portal.example.test")
	if logoutErr.Code != http.StatusInternalServerError || strings.Contains(logoutErr.Body.String(), "private logout detail") {
		t.Fatalf("logout error response = %d %s", logoutErr.Code, logoutErr.Body)
	}
	apiKeyCookie := &http.Cookie{Name: portalCookieName, Value: apiKeyCredential()}
	logoutAPIKey := requestWithCookie(router, http.MethodPost, "/api/auth/logout", apiKeyCookie, "https://portal.example.test")
	assertError(t, logoutAPIKey, http.StatusUnauthorized, "unauthenticated")

	services.auth.currentErr = errors.New("private user detail")
	meErr := requestWithCookie(router, http.MethodGet, "/api/me", cookie, "")
	if meErr.Code != http.StatusInternalServerError || strings.Contains(meErr.Body.String(), "private user detail") {
		t.Fatalf("me error response = %d %s", meErr.Code, meErr.Body)
	}
}

func TestHostValidationAndServiceErrors(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	key := "Bearer " + apiKeyCredential()

	checks := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/hosts", `{"template_id":"std-2c4g","workspace_ids":["bad"]}`},
		{http.MethodPost, "/api/hosts", `{"template_id":"std-2c4g","workspace_ids":["w1234567","w1234567"]}`},
		{http.MethodPost, "/api/hosts", `{"template_id":"std-2c4g","workspace":{"mode":"new"}}`},
		{http.MethodDelete, "/api/hosts/" + testHostID + "?keepWorkspace=true", ""},
	}
	for _, tt := range checks {
		response := request(router, tt.method, tt.path, tt.body, key, "")
		if tt.method == http.MethodPost && tt.path == "/api/hosts" {
			response = requestWithHeader(router, tt.method, tt.path, tt.body, key, "", "Idempotency-Key", "route-invalid-host-01")
		}
		assertError(t, response, http.StatusBadRequest, "invalid_argument")
	}

	services.hosts.err = domain.NewError(domain.CodeNotFound, "private not found context")
	for _, path := range []string{
		"/api/hosts",
		"/api/hosts/" + testHostID,
		"/api/hosts/" + testHostID + "/renew",
		"/api/hosts/" + testHostID + "/enter",
		"/api/hosts/" + testHostID + "/pause",
		"/api/hosts/" + testHostID + "/resume",
		"/api/hosts/" + testHostID + "/tokens",
		"/api/hosts/" + testHostID + "/previews/8080",
		"/api/tokens/" + testTokenID,
		"/api/notifications",
	} {
		method, body := http.MethodPost, ""
		if path == "/api/hosts" {
			method, body = http.MethodGet, ""
		} else if strings.HasSuffix(path, testHostID) {
			method = http.MethodGet
		}
		response := request(router, method, path, body, key, "")
		assertError(t, response, http.StatusNotFound, "not_found")
	}
}

func TestTemplateWorkspaceAndAPIKeyRoutes(t *testing.T) {
	services := newServices()
	services.tokens.apiKeys = []identity.Token{{
		ID: testTokenID, Kind: identity.KindAPIKey, OwnerID: testUserID,
		Name: "automation", SecretHash: []byte("must-not-leak"), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}}
	services.tokens.created = CreatedAPIKey{ID: testTokenID, Key: apiKeyCredential(), ExpiresAt: time.Now().Add(time.Hour)}
	router := newRouter(t, services, Options{})
	key := "Bearer " + apiKeyCredential()

	if response := request(router, http.MethodGet, "/api/templates", "", key, ""); response.Code != http.StatusOK {
		t.Fatalf("GET templates status = %d, body=%s", response.Code, response.Body)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, route := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/api/workspaces", http.StatusOK},
		{http.MethodDelete, "/api/workspaces/" + testWorkspaceID, http.StatusAccepted},
	} {
		origin := ""
		if route.method != http.MethodGet {
			origin = "https://portal.example.test"
		}
		response := requestWithCookie(router, route.method, route.path, cookie, origin)
		if response.Code != route.status {
			t.Fatalf("%s %s status = %d, body=%s; want %d", route.method, route.path, response.Code, response.Body, route.status)
		}
		if strings.Contains(response.Body.String(), "must-not-leak") {
			t.Fatalf("response exposed a stored token secret: %s", response.Body)
		}
	}
	if response := requestWithCookie(router, http.MethodGet, "/api/apikeys", cookie, ""); response.Code != http.StatusOK {
		t.Fatalf("GET API keys as Session = %d %s", response.Code, response.Body)
	}
	createBody := `{"name":"automation","ttl_seconds":3600}`
	if response := requestWithCookieBody(router, http.MethodPost, "/api/apikeys", createBody, cookie, "https://portal.example.test"); response.Code != http.StatusCreated {
		t.Fatalf("POST API key as Session = %d %s", response.Code, response.Body)
	}
	if response := requestWithCookie(router, http.MethodDelete, "/api/apikeys/"+testTokenID, cookie, "https://portal.example.test"); response.Code != http.StatusNoContent {
		t.Fatalf("DELETE API key as Session = %d %s", response.Code, response.Body)
	}
	if services.workspaceOps.delID != testWorkspaceID || services.tokens.apiKeyName != "automation" || services.tokens.apiKeyTTL != time.Hour {
		t.Fatalf("service arguments: workspace=%q key=%q ttl=%s", services.workspaceOps.delID, services.tokens.apiKeyName, services.tokens.apiKeyTTL)
	}
}

func TestWorkspaceCRUDUsesNewContractAndIdempotencyKey(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/workspaces", `{"name":"项目代码"}`, cookie, "https://portal.example.test", "Idempotency-Key", "workspace-create-01")
	if response.Code != http.StatusCreated {
		t.Fatalf("POST workspace = %d %s", response.Code, response.Body)
	}
	if services.workspaceOps.createP.Kind != identity.KindSession || services.workspaceOps.createCmd.Name != "项目代码" || services.workspaceOps.createKey != "workspace-create-01" {
		t.Fatalf("create workspace arguments = principal:%#v command:%#v key:%q", services.workspaceOps.createP, services.workspaceOps.createCmd, services.workspaceOps.createKey)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "name", "revision", "used_bytes", "quota_bytes", "state", "created_at", "updated_at"} {
		if _, ok := result[field]; !ok {
			t.Fatalf("workspace response missing %q: %#v", field, result)
		}
	}
	for _, obsolete := range []string{"size_mib", "attached_host_id", "last_error"} {
		if _, ok := result[obsolete]; ok {
			t.Fatalf("workspace response exposed obsolete field %q: %#v", obsolete, result)
		}
	}
	if response = requestWithCookie(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID, cookie, ""); response.Code != http.StatusOK || services.workspaceOps.getID != testWorkspaceID {
		t.Fatalf("GET workspace = %d %s; getID=%q", response.Code, response.Body, services.workspaceOps.getID)
	}
	if response = requestWithCookie(router, http.MethodGet, "/api/workspaces/bad", cookie, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("GET invalid workspace = %d %s", response.Code, response.Body)
	}
	if response = requestWithCookieBody(router, http.MethodPost, "/api/workspaces", `{"name":"no key"}`, cookie, "https://portal.example.test"); response.Code != http.StatusBadRequest {
		t.Fatalf("POST workspace without key = %d %s", response.Code, response.Body)
	}
}

func TestRetiredTaskRoutesAreNotRegistered(t *testing.T) {
	router := newRouter(t, newServices(), Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/tasks"},
		{http.MethodGet, "/api/tasks/q1234567"},
		{http.MethodPost, "/api/tasks/q1234567/actions"},
		{http.MethodGet, "/api/tasks/q1234567/events"},
		{http.MethodPost, "/api/schedules"},
		{http.MethodGet, "/api/schedules"},
		{http.MethodGet, "/api/conversations"},
		{http.MethodPost, "/api/conversations/c1234567/messages"},
		{http.MethodGet, "/api/execution"},
		{http.MethodPost, "/api/execution/enable"},
	} {
		response := requestWithCookie(router, route.method, route.path, cookie, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("retired route %s %s = %d %s", route.method, route.path, response.Code, response.Body)
		}
	}
}

func TestAdminRoutesAndValidation(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	services.admin.audits = append(services.admin.audits, AuditRecord{ID: 2, Entry: audit.Entry{ActorID: "i1234567", ActorKind: audit.ActorClient, Action: audit.ActionHostEnter}})
	services.admin.user = AdminUserRecord{
		ID: testUserID, Name: "alice", Role: identity.RoleUser, Status: identity.StatusDisabled, Quota: TotalsView{Hosts: 2},
		Usage: TotalsView{Hosts: 1}, DefaultHostResources: HostResourcesView{CPUMilli: 1000, MemMiB: 2048}, CreatedAt: time.Now().UTC(),
	}
	router := newRouter(t, services, Options{})
	key := "Bearer " + apiKeyCredential()
	for _, route := range []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodGet, "/api/admin/hosts", "", http.StatusOK},
		{http.MethodGet, "/api/admin/users?status=pending", "", http.StatusOK},
		{http.MethodPost, "/api/admin/users", `{"name":"alice","password":"secret","role":"user"}`, http.StatusNotFound},
		{http.MethodPatch, "/api/admin/users/" + testUserID, `{"expected_status":"approved","status":"disabled"}`, http.StatusOK},
		{http.MethodGet, "/api/admin/capacity", "", http.StatusOK},
		{http.MethodGet, "/api/admin/audit?actor=u1234567&action=host.create&target=h1234567&since=2026-10-01T00:00:00Z&limit=5", "", http.StatusOK},
	} {
		response := request(router, route.method, route.path, route.body, key, "")
		if response.Code != route.status {
			t.Fatalf("%s %s status = %d, body=%s; want %d", route.method, route.path, response.Code, response.Body, route.status)
		}
		if strings.Contains(response.Body.String(), `"password"`) || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("admin response leaked sensitive detail: %s", response.Body)
		}
	}
	if services.admin.principal.Role != identity.RoleAdmin || services.admin.status == nil || *services.admin.status != identity.StatusPending || services.admin.updated.Status == nil || *services.admin.updated.Status != identity.StatusDisabled || services.admin.updated.ExpectedStatus != identity.StatusApproved || services.admin.updatedID != testUserID {
		t.Fatalf("admin service commands not mapped: %#v", services.admin)
	}
	if services.admin.auditQ.Limit != 5 || services.admin.auditQ.Actor != testUserID || services.admin.auditQ.Action != "host.create" || services.admin.auditQ.Since == nil {
		t.Fatalf("audit query = %#v", services.admin.auditQ)
	}
	auditResponse := request(router, http.MethodGet, "/api/admin/audit", "", key, "")
	if auditResponse.Code != http.StatusOK || strings.Contains(auditResponse.Body.String(), "secret") || !strings.Contains(auditResponse.Body.String(), `"template_id"`) || !strings.Contains(auditResponse.Body.String(), `"actor_kind":"client"`) {
		t.Fatalf("audit details were not safely projected: %s", auditResponse.Body)
	}

	userServices := newServices()
	userRouter := newRouter(t, userServices, Options{})
	for _, path := range []string{"/api/admin/hosts", "/api/admin/users", "/api/admin/capacity", "/api/admin/audit"} {
		assertError(t, request(userRouter, http.MethodGet, path, "", key, ""), http.StatusForbidden, "forbidden")
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/admin/users/" + testUserID, `{}`},
		{http.MethodPatch, "/api/admin/users/" + testUserID, `{"password":""}`},
		{http.MethodGet, "/api/admin/audit?limit=501", ""},
		{http.MethodGet, "/api/admin/audit?since=not-a-date", ""},
	} {
		assertError(t, request(router, route.method, route.path, route.body, key, ""), http.StatusBadRequest, "invalid_argument")
	}
}

func TestHealthMCPAndPortalStaticRoutes(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.MCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: ping\n\n"))
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0"}`))
	})
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	if response := request(router, http.MethodGet, "/healthz", "", "", ""); response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", response.Code, response.Body)
	}
	if response := request(router, http.MethodGet, "/readyz", "", "", ""); response.Code != http.StatusOK {
		t.Fatalf("readyz = %d %s", response.Code, response.Body)
	}
	services.readiness.err = errors.New("database unavailable")
	deps = services.deps()
	deps.Readiness = services.readiness
	failedReadinessRouter, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	if response := request(failedReadinessRouter, http.MethodGet, "/readyz", "", "", ""); response.Code != http.StatusServiceUnavailable || response.Body.Len() != 0 {
		t.Fatalf("unready status = %d body=%q", response.Code, response.Body)
	}

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		body := ""
		if method == http.MethodPost {
			body = `{"jsonrpc":"2.0"}`
		}
		response := request(router, method, "/mcp", body, "Bearer "+apiKeyCredential(), "")
		want := http.StatusOK
		if method == http.MethodDelete {
			want = http.StatusNoContent
		}
		if response.Code != want {
			t.Fatalf("MCP %s = %d %s, want %d", method, response.Code, response.Body, want)
		}
	}
	largeMCP := request(newRouter(t, services, Options{MCPMaxBodyBytes: 8}), http.MethodPost, "/mcp", strings.Repeat("x", 32), "Bearer "+apiKeyCredential(), "")
	assertError(t, largeMCP, http.StatusRequestEntityTooLarge, "request_too_large")
	assertError(t, request(router, http.MethodGet, "/mcp", "", "", ""), http.StatusUnauthorized, "unauthenticated")
	assertError(t, requestWithCookie(router, http.MethodGet, "/mcp", &http.Cookie{Name: portalCookieName, Value: sessionCredential()}, ""), http.StatusUnauthorized, "unauthenticated")

	portal := requestHTML(router, "/hosts/new")
	if portal.Code != http.StatusOK || !strings.Contains(portal.Body.String(), "<title>portal</title>") || portal.Header().Get("Content-Security-Policy") != portalCSP || portal.Header().Get("X-Content-Type-Options") != "nosniff" || portal.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("portal SPA response = %d headers=%v body=%q", portal.Code, portal.Header(), portal.Body)
	}
	admin := requestHTML(router, "/admin/workspaces/"+testWorkspaceID)
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), "<title>admin</title>") || admin.Header().Get("Content-Security-Policy") != portalCSP || admin.Header().Get("X-Content-Type-Options") != "nosniff" || admin.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("admin SPA response = %d headers=%v body=%q", admin.Code, admin.Header(), admin.Body)
	}
	if root := requestHTML(router, "/admin/"); root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "<title>admin</title>") {
		t.Fatalf("admin root = %d %s", root.Code, root.Body)
	}
	adminAsset := request(router, http.MethodGet, "/admin/assets/admin.js", "", "", "")
	if adminAsset.Code != http.StatusOK || !strings.Contains(adminAsset.Body.String(), "admin") {
		t.Fatalf("admin asset = %d %s", adminAsset.Code, adminAsset.Body)
	}
	missingAdminAsset := requestHTML(router, "/admin/assets/missing.js")
	if missingAdminAsset.Code != http.StatusNotFound || strings.Contains(missingAdminAsset.Body.String(), "<title>admin</title>") {
		t.Fatalf("missing admin asset fell through to SPA: %d %s", missingAdminAsset.Code, missingAdminAsset.Body)
	}
	appPage := requestHTML(router, "/app/hosts/"+testHostID)
	if appPage.Code != http.StatusOK || !strings.Contains(appPage.Body.String(), "<title>app</title>") || appPage.Header().Get("Content-Security-Policy") != portalCSP || appPage.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("app SPA response = %d headers=%v body=%q", appPage.Code, appPage.Header(), appPage.Body)
	}
	for _, path := range []string{"/app/", "/app/foo"} {
		page := requestHTML(router, path)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<title>app</title>") {
			t.Fatalf("app route %s = %d %s", path, page.Code, page.Body)
		}
	}
	appAsset := request(router, http.MethodGet, "/app/assets/app.js", "", "", "")
	if appAsset.Code != http.StatusOK || !strings.Contains(appAsset.Body.String(), "mobile") {
		t.Fatalf("app asset = %d %s", appAsset.Code, appAsset.Body)
	}
	missingAppAsset := requestHTML(router, "/app/assets/missing.js")
	if missingAppAsset.Code != http.StatusNotFound || strings.Contains(missingAppAsset.Body.String(), "<title>app</title>") {
		t.Fatalf("missing app asset fell through to SPA: %d %s", missingAppAsset.Code, missingAppAsset.Body)
	}
	asset := request(router, http.MethodGet, "/assets/app.js", "", "", "")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("portal asset = %d headers=%v", asset.Code, asset.Header())
	}
	unknownAPI := request(router, http.MethodGet, "/api/unknown", "", "Bearer "+apiKeyCredential(), "")
	if unknownAPI.Code != http.StatusNotFound || strings.Contains(unknownAPI.Body.String(), "<title>portal</title>") {
		t.Fatalf("unknown API fell through to SPA: %d %s", unknownAPI.Code, unknownAPI.Body)
	}
}

func TestDisabledMCPReturnsNotFoundWithoutAuthentication(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.MCPDisabled = true
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions} {
		response := request(router, method, "/mcp", `{"jsonrpc":"2.0"}`, "", "")
		assertError(t, response, http.StatusNotFound, "not_found")
	}
}

func TestLoginMeAndLogoutKeepCredentialOutOfJSON(t *testing.T) {
	services := newServices()
	services.auth.loginResult = LoginResult{
		Credential: sessionCredential(), ExpiresAt: time.Now().Add(time.Hour),
		User: UserView{ID: testUserID, Name: "alice", Role: identity.RoleUser, Quota: TotalsView{Hosts: 2}},
	}
	router := newRouter(t, services, Options{CookieSecure: true})
	login := request(router, http.MethodPost, "/api/auth/login", `{"name":"alice","password":"secret-password"}`, "", "")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body)
	}
	if strings.Contains(login.Body.String(), services.auth.loginResult.Credential) || strings.Contains(login.Body.String(), "secret-password") {
		t.Fatalf("login response leaked a secret: %s", login.Body)
	}
	if services.auth.loginName != "alice" || services.auth.loginPass != "secret-password" || services.auth.loginIP != "192.0.2.1" {
		t.Fatalf("login arguments = %q, %q, %q", services.auth.loginName, services.auth.loginPass, services.auth.loginIP)
	}
	cookie := login.Result().Cookies()[0]
	if cookie.Name != portalCookieName || cookie.Value != services.auth.loginResult.Credential || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("session cookie = %#v", cookie)
	}

	services.auth.current = UserView{ID: testUserID, Name: "alice", Role: identity.RoleUser}
	me := requestWithCookie(router, http.MethodGet, "/api/me", cookie, "")
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"name":"alice"`) {
		t.Fatalf("me response = %d %s", me.Code, me.Body)
	}

	logout := requestWithCookie(router, http.MethodPost, "/api/auth/logout", cookie, "https://portal.example.test")
	if logout.Code != http.StatusNoContent || services.auth.logoutCalls != 1 {
		t.Fatalf("logout = %d calls=%d body=%s", logout.Code, services.auth.logoutCalls, logout.Body)
	}
	cleared := logout.Result().Cookies()[0]
	if cleared.Name != portalCookieName || cleared.MaxAge >= 0 || !cleared.HttpOnly || !cleared.Secure || cleared.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cleared cookie = %#v", cleared)
	}
}

func TestInitialSuperAdminSessionIsRestrictedToPasswordChange(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleSuperAdmin
	services.verifier.mustChange = true
	router := newRouter(t, services, Options{CookieSecure: true})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	if me := requestWithCookie(router, http.MethodGet, "/api/me", cookie, ""); me.Code != http.StatusOK {
		t.Fatalf("restricted session GET /api/me = %d %s", me.Code, me.Body)
	}
	assertError(t, requestWithCookie(router, http.MethodGet, "/api/admin/users", cookie, ""), http.StatusForbidden, "forbidden")
	changed := requestWithCookieBody(router, http.MethodPost, "/api/me/password", `{"current_password":"admin","new_password":"new-admin-password"}`, cookie, "https://portal.example.test")
	if changed.Code != http.StatusNoContent || services.auth.changeCalls != 1 || services.auth.changeP.Role != identity.RoleSuperAdmin ||
		services.auth.changeOld != "admin" || services.auth.changeNew != "new-admin-password" {
		t.Fatalf("password change = %d %s, calls=%d principal=%#v", changed.Code, changed.Body, services.auth.changeCalls, services.auth.changeP)
	}
	cleared := changed.Result().Cookies()[0]
	if cleared.Name != portalCookieName || cleared.MaxAge >= 0 || !cleared.HttpOnly || !cleared.Secure {
		t.Fatalf("password change did not clear the old session cookie: %#v", cleared)
	}
}

func TestPublicRegistrationCreatesAccountWithoutSession(t *testing.T) {
	services := newServices()
	services.auth.registerUser = UserView{ID: testUserID, Name: "new-user", Role: identity.RoleUser}
	router := newRouter(t, services, Options{})
	response := request(router, http.MethodPost, "/api/auth/register", `{"name":"new-user","password":"long-secret"}`, "", "")
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"role":"user"`) || len(response.Result().Cookies()) != 0 {
		t.Fatalf("registration response = %d %s, cookies=%v", response.Code, response.Body, response.Result().Cookies())
	}
	if services.auth.registerName != "new-user" || services.auth.registerPass != "long-secret" || services.auth.registerIP != "192.0.2.1" || strings.Contains(response.Body.String(), "long-secret") {
		t.Fatal("registration arguments or response were invalid")
	}
	services.auth.registerErr = domain.NewError(domain.CodeRateLimited, "registration rate limit reached")
	assertError(t, request(router, http.MethodPost, "/api/auth/register", `{"name":"new-user","password":"long-secret"}`, "", ""), http.StatusTooManyRequests, "rate_limited")
}

func TestHostOperationsAndMissingDependencyResponses(t *testing.T) {
	services := newServices()
	services.hosts.enterResult = app.EnterInfo{URL: "https://h1234567.example-hosts.test/__ch/auth?ticket=" + ticketCredential(), ExpiresAt: time.Now().Add(time.Minute)}
	router := newRouter(t, services, Options{})
	tests := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodGet, "/api/hosts", "", http.StatusOK},
		{http.MethodGet, "/api/hosts/" + testHostID, "", http.StatusOK},
		{http.MethodDelete, "/api/hosts/" + testHostID, "", http.StatusAccepted},
		{http.MethodPost, "/api/hosts/" + testHostID + "/release", "", http.StatusAccepted},
		{http.MethodPost, "/api/hosts/" + testHostID + "/restore", "", http.StatusAccepted},
		{http.MethodPost, "/api/hosts/" + testHostID + "/renew", "", http.StatusOK},
		{http.MethodPost, "/api/hosts/" + testHostID + "/enter", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := request(router, tt.method, tt.path, tt.body, "Bearer "+apiKeyCredential(), "")
			if response.Code != tt.status {
				t.Fatalf("status = %d, body = %s; want %d", response.Code, response.Body, tt.status)
			}
		})
	}
	if services.hosts.deleted.HostID != testHostID {
		t.Fatalf("delete host command = %#v", services.hosts.deleted)
	}

	apiKeys := request(router, http.MethodGet, "/api/apikeys", "", "Bearer "+apiKeyCredential(), "")
	if apiKeys.Code != http.StatusForbidden {
		t.Fatalf("API key bearer should not manage keys: %d %s", apiKeys.Code, apiKeys.Body)
	}
}

func TestReleaseHostPassesOptionalProcessTerminationConfirmation(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	path := "/api/hosts/" + testHostID + "/release"
	response := request(router, http.MethodPost, path, "", "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusAccepted || services.hosts.releaseConfirmed {
		t.Fatalf("release without body = %d, confirmed=%t, body=%s", response.Code, services.hosts.releaseConfirmed, response.Body)
	}
	response = request(router, http.MethodPost, path, `{"confirm_process_termination":true}`, "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusAccepted || !services.hosts.releaseConfirmed {
		t.Fatalf("confirmed release = %d, confirmed=%t, body=%s", response.Code, services.hosts.releaseConfirmed, response.Body)
	}
	response = request(router, http.MethodPost, path, `{"confirm_process_termination":"yes"}`, "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid release confirmation type status = %d, body=%s", response.Code, response.Body)
	}
}

func TestInvalidRequestsAndOriginAreRejected(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{APIMaxBodyBytes: 80})
	response := requestWithHeader(router, http.MethodPost, "/api/hosts", `{"template_id":"Bad"}`, "Bearer "+apiKeyCredential(), "", "Idempotency-Key", "route-invalid-host-02")
	assertError(t, response, http.StatusBadRequest, "invalid_argument")
	largeBody := `{"template_id":"std-2c4g","pad":"` + strings.Repeat("x", 100) + `"}`
	response = requestWithHeader(router, http.MethodPost, "/api/hosts", largeBody, "Bearer "+apiKeyCredential(), "", "Idempotency-Key", "route-invalid-host-03")
	assertError(t, response, http.StatusRequestEntityTooLarge, "request_too_large")
	response = request(router, http.MethodGet, "/api/hosts/bad", "", "Bearer "+apiKeyCredential(), "")
	assertError(t, response, http.StatusBadRequest, "invalid_argument")
	response = requestWithHeader(router, http.MethodPost, "/api/hosts", `{"template_id":"std-2c4g"}`, "Bearer "+apiKeyCredential(), "https://evil.example", "Idempotency-Key", "route-invalid-host-04")
	assertError(t, response, http.StatusForbidden, "forbidden")
}

func TestSessionUnsafeRequestsRequireContentTypeOnlyWhenBodyMayExist(t *testing.T) {
	const path = "/api/sites/site_0123456789abcdef/releases/rel_0123456789abcdef/materialize"
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	services := newServices()
	router := newRouter(t, services, Options{})
	noBody := httptest.NewRequest(http.MethodPost, path, nil)
	noBody.Host = "portal.example.test"
	noBody.RemoteAddr = "192.0.2.1:12345"
	noBody.AddCookie(cookie)
	noBody.Header.Set("Origin", "https://portal.example.test")
	noBodyResponse := httptest.NewRecorder()
	router.ServeHTTP(noBodyResponse, noBody)
	if noBodyResponse.Code != http.StatusOK || string(services.sites.siteID) != "site_0123456789abcdef" || string(services.sites.releaseID) != "rel_0123456789abcdef" {
		t.Fatalf("bodyless materialize = %d %s; handler site=%q release=%q", noBodyResponse.Code, noBodyResponse.Body, services.sites.siteID, services.sites.releaseID)
	}

	for _, test := range []struct {
		name        string
		contentType string
		streaming   bool
	}{
		{name: "body without content type"},
		{name: "body with wrong content type", contentType: "text/plain"},
		{name: "stream without content type", streaming: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			services := newServices()
			router := newRouter(t, services, Options{})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			request.Host = "portal.example.test"
			request.RemoteAddr = "192.0.2.1:12345"
			request.AddCookie(cookie)
			request.Header.Set("Origin", "https://portal.example.test")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.streaming {
				request.ContentLength = -1
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assertError(t, response, http.StatusForbidden, "forbidden")
			if services.sites.siteID != "" {
				t.Fatalf("request with invalid content type reached handler: site=%q", services.sites.siteID)
			}
		})
	}
}

func TestConstructorAndWorkspaceFailure(t *testing.T) {
	services := newServices()
	options := validOptions()
	options.HostSuffix = "https://example.test/"
	if _, err := New(services.deps(), options); err == nil {
		t.Fatal("New accepted an invalid host suffix")
	}
	options = validOptions()
	options.APIMaxBodyBytes = 0
	if _, err := New(services.deps(), options); err == nil {
		t.Fatal("New accepted a non-positive body limit")
	}
	services.hosts.item.Workspaces[0].Workspace.Revision = -1
	router := newRouter(t, services, Options{})
	response := request(router, http.MethodGet, "/api/hosts/"+testHostID, "", "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "revision") {
		t.Fatalf("invalid workspace binding response = %d %s", response.Code, response.Body)
	}
}

func TestAuthenticationFailuresIncrementBoundedMetric(t *testing.T) {
	services := newServices()
	registry := prometheus.NewRegistry()
	options := validOptions()
	options.Metrics = registry
	router, err := New(services.deps(), options)
	if err != nil {
		t.Fatal(err)
	}
	assertError(t, request(router, http.MethodGet, "/api/hosts", "", "", ""), http.StatusUnauthorized, "unauthenticated")
	assertError(t, request(router, http.MethodGet, "/api/hosts", "", "Bearer "+ticketCredential(), ""), http.StatusUnauthorized, "unauthenticated")

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]float64{}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "cloudhost_auth_failures") {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			counts[labels["server"]+":"+labels["kind"]] = metric.GetCounter().GetValue()
		}
	}
	if counts["api:session"] != 1 || counts["api:ticket"] != 1 {
		t.Fatalf("authentication failure metrics = %#v", counts)
	}
}

func TestMissingOptionalPortsReturnSanitizedUnavailable(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	deps := services.deps()
	deps.WorkspaceOps = nil
	deps.Templates = nil
	deps.Tokens = nil
	deps.Admin = nil
	deps.Readiness = nil
	deps.MCP = nil
	options := validOptions()
	options.PortalFiles = nil
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/templates", ""},
		{http.MethodGet, "/api/workspaces", ""},
		{http.MethodPost, "/api/workspaces", `{"name":"new"}`},
		{http.MethodGet, "/api/workspaces/" + testWorkspaceID, ""},
		{http.MethodDelete, "/api/workspaces/" + testWorkspaceID, ""},
		{http.MethodGet, "/api/admin/users", ""},
		{http.MethodPatch, "/api/admin/users/" + testUserID, `{"expected_status":"approved","status":"disabled"}`},
		{http.MethodGet, "/api/admin/capacity", ""},
		{http.MethodGet, "/api/admin/audit", ""},
	} {
		var response *httptest.ResponseRecorder
		if route.method == http.MethodGet {
			response = requestWithCookie(router, route.method, route.path, cookie, "")
		} else {
			response = requestWithCookieBody(router, route.method, route.path, route.body, cookie, "https://portal.example.test")
		}
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("%s %s = %d %s; want sanitized 503", route.method, route.path, response.Code, response.Body)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/apikeys", ""},
		{http.MethodPost, "/api/apikeys", `{"name":"automation"}`},
		{http.MethodDelete, "/api/apikeys/" + testTokenID, ""},
	} {
		var response *httptest.ResponseRecorder
		if route.method == http.MethodGet {
			response = requestWithCookie(router, route.method, route.path, cookie, "")
		} else {
			response = requestWithCookieBody(router, route.method, route.path, route.body, cookie, "https://portal.example.test")
		}
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("%s %s = %d %s; want sanitized 503", route.method, route.path, response.Code, response.Body)
		}
	}
	if response := request(router, http.MethodGet, "/mcp", "", "Bearer "+apiKeyCredential(), ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /mcp without a handler = %d %s; want 503", response.Code, response.Body)
	}
	if response := request(router, http.MethodGet, "/readyz", "", "", ""); response.Code != http.StatusServiceUnavailable || response.Body.Len() != 0 {
		t.Fatalf("readiness without checker = %d body=%q", response.Code, response.Body)
	}
	if response := requestHTML(router, "/"); response.Code != http.StatusNotFound {
		t.Fatalf("static site without files = %d %s", response.Code, response.Body)
	}
}

func TestPreviewAuthRedirectUsesPortalSessionAndFixedTarget(t *testing.T) {
	services := newServices()
	services.hosts.enterResult = app.EnterInfo{
		URL:       "https://5173-" + testHostID + ".example-hosts.test/__ch/auth?t=" + ticketCredential(),
		ExpiresAt: time.Now().Add(time.Minute),
	}
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet,
		"/api/hosts/"+testHostID+"/preview-auth?port=5173&next=%2Fpost%3Fpage%3D2", cookie, "")
	want := "https://5173-" + testHostID + ".example-hosts.test/__ch/auth?next=%2Fpost%3Fpage%3D2&t=" + url.QueryEscape(ticketCredential())
	if response.Code != http.StatusFound || response.Header().Get("Location") != want || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("preview auth = %d location=%q cache=%q referrer=%q", response.Code, response.Header().Get("Location"), response.Header().Get("Cache-Control"), response.Header().Get("Referrer-Policy"))
	}
	if services.hosts.previewCalls != 1 || services.hosts.previewPort != 5173 || services.hosts.principal.Kind != identity.KindSession {
		t.Fatalf("preview ticket request = calls:%d port:%d principal:%#v", services.hosts.previewCalls, services.hosts.previewPort, services.hosts.principal)
	}
	for _, port := range []string{"8080", "8090", "900", "bad"} {
		denied := requestWithCookie(router, http.MethodGet, "/api/hosts/"+testHostID+"/preview-auth?port="+port, cookie, "")
		if denied.Code != http.StatusNotFound {
			t.Errorf("preview auth port %q = %d, want 404", port, denied.Code)
		}
	}
	if services.hosts.previewCalls != 1 {
		t.Fatalf("invalid preview port reached HostService: calls=%d", services.hosts.previewCalls)
	}
}

func TestTLSAuthorizationAllowsOnlyPortalAndActiveHostDomains(t *testing.T) {
	services := newServices()
	services.hosts.certificates = map[string]bool{testHostID: true}
	router := newRouter(t, services, Options{})
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "portal", status: http.StatusNoContent},
		{name: "active host", status: http.StatusNoContent},
		{name: "active preview", status: http.StatusNoContent},
		{name: "unknown host", status: http.StatusForbidden},
		{name: "denied preview port", status: http.StatusForbidden},
		{name: "reserved RFB port", status: http.StatusForbidden},
		{name: "reserved CDP port", status: http.StatusForbidden},
		{name: "below minimum preview port", status: http.StatusForbidden},
		{name: "nested subdomain", status: http.StatusForbidden},
		{name: "other suffix", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := map[string]string{
				"portal":                     "portal.example.test",
				"active host":                testHostID + ".example-hosts.test",
				"active preview":             "5173-" + testHostID + ".example-hosts.test",
				"unknown host":               "h7654321.example-hosts.test",
				"denied preview port":        "8080-" + testHostID + ".example-hosts.test",
				"reserved RFB port":          "5900-" + testHostID + ".example-hosts.test",
				"reserved CDP port":          "9222-" + testHostID + ".example-hosts.test",
				"below minimum preview port": "900-" + testHostID + ".example-hosts.test",
				"nested subdomain":           "nested." + testHostID + ".example-hosts.test",
				"other suffix":               testHostID + ".elsewhere.test",
			}[test.name]
			response := request(router, http.MethodGet, "/__ch/cert-authorize?domain="+url.QueryEscape(name), "", "", "")
			if response.Code != test.status {
				t.Fatalf("certificate authorization for %q = %d, want %d", name, response.Code, test.status)
			}
		})
	}
}

func TestServiceErrorsAreSanitizedAcrossRoutes(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	privateErr := errors.New("private credential is not safe to return")
	services.hosts.err = privateErr
	services.templates.err = privateErr
	services.workspaceOps.err = privateErr
	services.tokens.err = privateErr
	services.admin.err = privateErr
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/hosts", ""},
		{http.MethodPost, "/api/hosts", `{}`},
		{http.MethodDelete, "/api/hosts/" + testHostID, ""},
		{http.MethodPost, "/api/hosts/" + testHostID + "/renew", ""},
		{http.MethodPost, "/api/hosts/" + testHostID + "/enter", ""},
		{http.MethodGet, "/api/templates", ""},
		{http.MethodGet, "/api/workspaces", ""},
		{http.MethodGet, "/api/workspaces/" + testWorkspaceID, ""},
		{http.MethodDelete, "/api/workspaces/" + testWorkspaceID, ""},
		{http.MethodPost, "/api/workspaces", `{"name":"workspace"}`},
		{http.MethodGet, "/api/admin/hosts", ""},
		{http.MethodGet, "/api/admin/users", ""},
		{http.MethodPatch, "/api/admin/users/" + testUserID, `{"expected_status":"approved","status":"disabled"}`},
		{http.MethodGet, "/api/admin/capacity", ""},
		{http.MethodGet, "/api/admin/audit", ""},
	} {
		var response *httptest.ResponseRecorder
		if route.method == http.MethodGet {
			response = requestWithCookie(router, route.method, route.path, cookie, "")
		} else if route.path == "/api/workspaces" {
			response = requestWithCookieBodyAndHeader(router, route.method, route.path, route.body, cookie, "https://portal.example.test", "Idempotency-Key", "workspace-create-error")
		} else if route.path == "/api/hosts" && route.method == http.MethodPost {
			response = requestWithCookieBodyAndHeader(router, route.method, route.path, route.body, cookie, "https://portal.example.test", "Idempotency-Key", "host-create-error")
		} else {
			response = requestWithCookieBody(router, route.method, route.path, route.body, cookie, "https://portal.example.test")
		}
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private credential") {
			t.Fatalf("%s %s = %d %s; want sanitized 500", route.method, route.path, response.Code, response.Body)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/apikeys", ""},
		{http.MethodPost, "/api/apikeys", `{"name":"automation"}`},
		{http.MethodDelete, "/api/apikeys/" + testTokenID, ""},
	} {
		var response *httptest.ResponseRecorder
		if route.method == http.MethodPost {
			response = requestWithCookieBody(router, route.method, route.path, route.body, cookie, "https://portal.example.test")
		} else {
			response = requestWithCookie(router, route.method, route.path, cookie, "https://portal.example.test")
		}
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private credential") {
			t.Fatalf("%s %s = %d %s; want sanitized 500", route.method, route.path, response.Code, response.Body)
		}
	}
}

type services struct {
	hosts           *hostServiceStub
	hostReplica     *hostReplicaServiceStub
	tasks           TaskService
	conversations   ConversationService
	verifier        verifierStub
	auth            *authServiceStub
	workspaceOps    *workspaceServiceStub
	sites           *siteServiceStub
	templates       templateReaderStub
	tokens          *tokenServiceStub
	admin           *adminServiceStub
	adminWorkspaces *adminWorkspaceServiceStub
	readiness       readinessStub
}

func newServices() *services {
	now := time.Now().UTC().Truncate(time.Second)
	workspaceValue := workspace.Workspace{
		ID: testWorkspaceID, OwnerID: testUserID, Name: "workspace", State: workspace.StateReady,
		Revision: 2, UsedBytes: 4096, QuotaBytes: 1 << 30, CreatedAt: now, UpdatedAt: now,
	}
	return &services{
		hosts: &hostServiceStub{item: host.Host{
			ID: testHostID, OwnerID: testUserID, Name: "worker", TemplateID: "std-2c4g", RuntimeProfile: "container",
			State: host.StateRunning, RuntimeGeneration: 1, ControlVersion: 0, ControlState: host.ControlAI,
			CPUMilli: 500, MemMiB: 1536, CPULimitMilli: 2000, MemLimitMiB: 4096, CreatedAt: now,
			Workspaces: []workspace.Binding{{
				OwnerID: testUserID, HostID: testHostID, Workspace: workspaceValue, Directory: testWorkspaceID,
				State: workspace.BindingActive, BindingVersion: 1, DesiredRevision: 2, AppliedRevision: 2,
				SyncState: workspace.BindingSyncHealthy,
			}},
		}},
		verifier: verifierStub{},
		auth:     &authServiceStub{},
		sites:    &siteServiceStub{},
		workspaceOps: &workspaceServiceStub{
			items: []workspace.Workspace{workspaceValue}, item: workspaceValue, created: workspaceValue,
			deviceItems: []app.DeviceWorkspaceInfo{{ID: workspaceValue.ID, Name: workspaceValue.Name, Revision: workspaceValue.Revision, State: workspaceValue.State}},
			deleted: workspace.Workspace{
				ID: testWorkspaceID, OwnerID: testUserID, Name: "workspace", State: workspace.StateDeleting,
				Revision: 2, UsedBytes: 4096, QuotaBytes: 1 << 30, CreatedAt: now, UpdatedAt: now,
			},
		},
		templates: templateReaderStub{items: []TemplateRecord{{
			ID: "std-2c4g", Name: "Standard", Isolation: "container", Resources: ResourceRecord{CPUMilli: 500, MemMiB: 1536, CPULimitMilli: 2000, MemLimitMiB: 4096},
			WorkspaceSizeMiB: 20480, TTLSeconds: 86400, IdleTimeoutSeconds: 3600, OnIdle: "disabled", KeepWorkspaceDefault: true,
		}}},
		tokens: &tokenServiceStub{},
		admin: &adminServiceStub{
			users:    []AdminUserRecord{{ID: testUserID, Name: "alice", Role: identity.RoleUser, Status: identity.StatusPending, Quota: TotalsView{Hosts: 2}, DefaultHostResources: HostResourcesView{CPUMilli: 1000, MemMiB: 2048}, CreatedAt: time.Now().UTC()}},
			user:     AdminUserRecord{ID: testUserID, Name: "alice", Role: identity.RoleUser, Status: identity.StatusPending, Quota: TotalsView{Hosts: 2}, DefaultHostResources: HostResourcesView{CPUMilli: 1000, MemMiB: 2048}, CreatedAt: time.Now().UTC()},
			capacity: CapacityRecord{Limit: TotalsView{CPUMilli: 10000, MemMiB: 32768}, Allocated: TotalsView{CPUMilli: 500, MemMiB: 1536}, HostsByState: map[host.State]int{host.StateRunning: 1}},
			audits: []AuditRecord{{ID: 1, At: time.Now().UTC(), Entry: audit.Entry{
				ActorID: testUserID, ActorKind: audit.ActorAPIKey, Action: audit.ActionHostCreate, Target: testHostID,
				Detail: map[string]any{
					"template_id": "std-2c4g", "api_key": "secret",
					"metadata": map[string]any{"visible": "ok", "nested_api_key": "leaked-secret-value"},
					"extra":    []any{map[string]any{"bearer_token": "second-secret"}},
				}, IP: "192.0.2.1",
			}}},
		},
		adminWorkspaces: &adminWorkspaceServiceStub{
			page: workspace.AdminPage{Items: []workspace.AdminSummary{{
				ID: testWorkspaceID, OwnerID: testUserID, OwnerName: "alice", Name: "workspace", State: workspace.StateReady,
				Revision: 2, UsedBytes: 4096, QuotaBytes: 1 << 30, CreatedAt: now, UpdatedAt: now,
				Bindings: []workspace.AdminBinding{{
					HostID: testHostID, HostName: "worker", State: workspace.BindingActive, SyncState: workspace.BindingSyncHealthy,
					DesiredRevision: 2, AppliedRevision: 2,
				}},
			}}, NextCursor: "next-page"},
			item: workspace.AdminSummary{
				ID: testWorkspaceID, OwnerID: testUserID, OwnerName: "alice", Name: "workspace", State: workspace.StateReady,
				Revision: 2, UsedBytes: 4096, QuotaBytes: 1 << 30, CreatedAt: now, UpdatedAt: now,
				Bindings: []workspace.AdminBinding{{
					HostID: testHostID, HostName: "worker", State: workspace.BindingActive, SyncState: workspace.BindingSyncHealthy,
					DesiredRevision: 2, AppliedRevision: 2,
				}},
			},
		},
	}
}

func (s *services) deps() Deps {
	return Deps{
		Hosts: s.hosts, TLSAuthorizer: s.hosts, Verifier: s.verifier, Auth: s.auth,
		HostReplicas: s.hostReplica,
		Tasks:        s.tasks, Conversations: s.conversations,
		WorkspaceOps: s.workspaceOps, Sites: s.sites, Templates: s.templates, Tokens: s.tokens,
		Admin: s.admin, AdminWorkspaces: s.adminWorkspaces, Readiness: s.readiness,
	}
}

func TestSyncOnlyCredentialFence(t *testing.T) {
	services := newServices()
	router := newRouter(t, services, Options{})
	for _, path := range []string{"/api/device-authorizations", "/api/device-authorizations/token", "/api/devices"} {
		response := request(router, http.MethodGet, path, "", "", "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("old pairing route %s = %d", path, response.Code)
		}
	}
	deviceCode, _, err := identity.NewCredential(identity.KindDevice, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	fenced := request(router, http.MethodGet, "/api/workspaces", "", "Bearer "+deviceCode, "")
	if fenced.Code != http.StatusForbidden {
		t.Fatalf("device credential accessed general workspace API: %d %s", fenced.Code, fenced.Body)
	}
	listed := request(router, http.MethodGet, "/api/device/workspaces", "", "Bearer "+deviceCode, "")
	if listed.Code != http.StatusOK || listed.Body.String() != `{"items":[{"id":"w1234567","name":"workspace","revision":2,"state":"ready"}]}` {
		t.Fatalf("device workspace list = %d %s", listed.Code, listed.Body)
	}
	if services.workspaceOps.listP.Kind != identity.KindDevice || services.workspaceOps.listP.TokenID != testTokenID {
		t.Fatalf("device workspace list principal = %#v", services.workspaceOps.listP)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/hosts"},
		{http.MethodPost, "/api/hosts/" + testHostID + "/enter"},
		{http.MethodPost, "/api/hosts/" + testHostID + "/observation-ticket"},
	} {
		response := request(router, tc.method, tc.path, `{"host_id":"h1234567","model_id":"model-a","objective":"test","action":"pause","expected_status":"queued"}`, "Bearer "+deviceCode, "")
		if response.Code != http.StatusForbidden {
			t.Errorf("device credential accessed forbidden route %s %s: %d %s; want 403", tc.method, tc.path, response.Code, response.Body)
		}
	}
	if services.hosts.principal.Kind == identity.KindDevice {
		t.Fatalf("device Host route reached Host service: %#v", services.hosts.principal)
	}
	services.workspaceOps.deviceItems = nil
	listed = request(router, http.MethodGet, "/api/device/workspaces", "", "Bearer "+deviceCode, "")
	if listed.Code != http.StatusOK || listed.Body.String() != `{"items":[]}` {
		t.Fatalf("empty device workspace list = %d %s", listed.Code, listed.Body)
	}
}

type clientRouteHostSharing struct {
	app.HostSharingService
	principal identity.Principal
	calls     int
}

func (s *clientRouteHostSharing) IssueObservationTicket(_ context.Context, principal identity.Principal, _ string) (app.EnterInfo, error) {
	s.principal = principal
	s.calls++
	return app.EnterInfo{URL: "https://h1234567.example-hosts.test/__ch/auth?t=observation", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func TestClientCredentialPositiveRouteContract(t *testing.T) {
	services := newServices()
	tasks := &taskServiceStub{}
	conversations := &conversationServiceStub{}
	clientCredential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	authorization := "Bearer " + clientCredential
	services.hosts.enterResult = app.EnterInfo{
		URL: "https://h1234567.example-hosts.test/__ch/auth?t=entry", ExpiresAt: time.Now().Add(time.Minute),
	}
	notices := &notificationAPITestService{page: app.NotificationPage{Items: []notification.Notification{{
		ID: "n1234567", OwnerID: testUserID, TaskID: "q1234567", EventSequence: 1,
		Kind: notification.KindCompleted, Summary: "任务已完成", CreatedAt: time.Now().UTC(),
	}}}}
	sharing := &clientRouteHostSharing{}
	deps := services.deps()
	deps.Notifications, deps.HostSharing = notices, sharing
	deps.Tasks, deps.Conversations = tasks, conversations
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}

	listedNotices := request(router, http.MethodGet, "/api/notifications", "", authorization, "")
	if listedNotices.Code != http.StatusOK || notices.principal.Kind != identity.KindClient || notices.principal.TokenID != testTokenID {
		t.Fatalf("client notification list = %d %s, principal=%#v", listedNotices.Code, listedNotices.Body, notices.principal)
	}
	markedNotice := request(router, http.MethodPost, "/api/notifications/n1234567/read", "", authorization, "")
	if markedNotice.Code != http.StatusNoContent || notices.principal.Kind != identity.KindClient || notices.markedID != "n1234567" {
		t.Fatalf("client notification read = %d %s, principal=%#v id=%q", markedNotice.Code, markedNotice.Body, notices.principal, notices.markedID)
	}

	entryTicket := request(router, http.MethodPost, "/api/hosts/"+testHostID+"/enter", "", authorization, "")
	if entryTicket.Code != http.StatusOK || services.hosts.principal.Kind != identity.KindClient || services.hosts.principal.TokenID != testTokenID {
		t.Fatalf("client Host entry ticket = %d %s, principal=%#v", entryTicket.Code, entryTicket.Body, services.hosts.principal)
	}
	observationTicket := request(router, http.MethodPost, "/api/hosts/"+testHostID+"/observation-ticket", "", authorization, "")
	if observationTicket.Code != http.StatusOK || sharing.calls != 1 || sharing.principal.Kind != identity.KindClient || sharing.principal.TokenID != testTokenID {
		t.Fatalf("client Host observation ticket = %d %s, calls=%d principal=%#v", observationTicket.Code, observationTicket.Body, sharing.calls, sharing.principal)
	}
	listedTasks := request(router, http.MethodGet, "/api/tasks", "", authorization, "")
	if listedTasks.Code != http.StatusOK || tasks.listCalls != 1 {
		t.Fatalf("client task list = %d %s, calls=%d", listedTasks.Code, listedTasks.Body, tasks.listCalls)
	}
	createdTask := requestWithHeader(router, http.MethodPost, "/api/tasks", `{"model_id":"model-a","objective":"continue in cloud"}`, authorization, "", "Idempotency-Key", "client-task-001")
	if createdTask.Code != http.StatusAccepted || tasks.submitCalls != 1 || tasks.submitCommand.HostID != "" {
		t.Fatalf("client cloud task = %d %s, calls=%d command=%#v", createdTask.Code, createdTask.Body, tasks.submitCalls, tasks.submitCommand)
	}
	listedConversations := request(router, http.MethodGet, "/api/conversations", "", authorization, "")
	if listedConversations.Code != http.StatusOK || conversations.listCalls != 1 {
		t.Fatalf("client conversation list = %d %s, calls=%d", listedConversations.Code, listedConversations.Body, conversations.listCalls)
	}
}

func TestClientCredentialRouteFence(t *testing.T) {
	services := newServices()
	services.verifier = verifierStub{role: identity.RoleAdmin}
	router := newRouter(t, services, Options{})
	client, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + client
	allowed := request(router, http.MethodGet, "/api/hosts", "", auth, "")
	if allowed.Code != http.StatusOK || services.hosts.principal.Kind != identity.KindClient || services.hosts.principal.Role != identity.RoleUser {
		t.Fatalf("client host list = %d %s, principal = %#v", allowed.Code, allowed.Body, services.hosts.principal)
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/users", ""},
		{http.MethodGet, "/api/apikeys", ""},
		{http.MethodGet, "/api/model-connections", ""},
		{http.MethodGet, "/api/device/workspaces", ""},
		{http.MethodPost, "/api/workspaces", `{}`},
		{http.MethodDelete, "/api/hosts/" + testHostID, ""},
	} {
		response := request(router, tc.method, tc.path, tc.body, auth, "")
		if response.Code != http.StatusForbidden {
			t.Errorf("client %s %s = %d %s, want 403", tc.method, tc.path, response.Code, response.Body)
		}
	}
}

type clientLoginHTTPStub struct {
	ClientLoginService
	principal identity.Principal
	key       string
}

func (s *clientLoginHTTPStub) Login(_ context.Context, _, _, _, _, _, key, _ string) (app.ClientLoginResult, error) {
	s.key = key
	return app.ClientLoginResult{InstallationID: "i1234567", ClientCredential: "client-secret",
		ClientExpiresAt: time.Now().Add(time.Hour), SyncCredential: "sync-secret", SyncExpiresAt: time.Now().Add(time.Hour),
		WebSessionCredential: "web-secret", WebSessionExpiresAt: time.Now().Add(time.Hour),
		User: app.UserSummary{ID: testUserID, Name: "alice", Role: identity.RoleUser, Status: identity.StatusApproved}}, nil
}
func (s *clientLoginHTTPStub) WebSession(_ context.Context, principal identity.Principal) (app.ClientWebSessionResult, error) {
	s.principal = principal
	return app.ClientWebSessionResult{Credential: "web-secret", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (s *clientLoginHTTPStub) Logout(_ context.Context, principal identity.Principal, _ string) error {
	s.principal = principal
	return nil
}
func (s *clientLoginHTTPStub) LogoutWebSession(_ context.Context, principal identity.Principal, _ string) error {
	s.principal = principal
	return nil
}
func (s *clientLoginHTTPStub) List(_ context.Context, principal identity.Principal) ([]app.ClientInfo, error) {
	s.principal = principal
	return []app.ClientInfo{}, nil
}
func (s *clientLoginHTTPStub) Revoke(_ context.Context, principal identity.Principal, _, _ string) error {
	s.principal = principal
	return nil
}

func TestClientLoginHTTPContract(t *testing.T) {
	services := newServices()
	stub := &clientLoginHTTPStub{}
	deps := services.deps()
	deps.ClientLogin = stub
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	started := requestWithHeader(router, http.MethodPost, "/api/auth/client-login",
		`{"name":"alice","password":"secret","platform":"desktop","display_name":"laptop"}`, "", "", "Idempotency-Key", "login-12345678")
	if started.Code != http.StatusOK || !strings.Contains(started.Body.String(), `"web_session_credential":"web-secret"`) ||
		stub.key != "login-12345678" || started.Header().Get("Set-Cookie") != "" {
		t.Fatalf("login = %d %s, key=%q cookie=%q", started.Code, started.Body, stub.key, started.Header().Get("Set-Cookie"))
	}
	client, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + client
	web := request(router, http.MethodPost, "/api/auth/client-web-session", "", auth, "")
	if web.Code != http.StatusOK || !strings.Contains(web.Body.String(), `"web_session_credential":"web-secret"`) ||
		stub.principal.Kind != identity.KindClient {
		t.Fatalf("web session = %d %s, principal=%#v", web.Code, web.Body, stub.principal)
	}
	logout := request(router, http.MethodPost, "/api/auth/client-logout", "", auth, "")
	if logout.Code != http.StatusNoContent || stub.principal.Kind != identity.KindClient {
		t.Fatalf("logout = %d %s", logout.Code, logout.Body)
	}
	for _, path := range []string{"/api/client-authorizations", "/api/client-authorizations/token",
		"/api/client-authorizations/inspect", "/api/client-authorizations/approve", "/api/client-authorizations/deny"} {
		if response := request(router, http.MethodPost, path, "{}", "", ""); response.Code != http.StatusNotFound {
			t.Errorf("old pairing %s = %d", path, response.Code)
		}
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	listed := requestWithCookie(router, http.MethodGet, "/api/clients", cookie, "")
	if listed.Code != http.StatusOK || listed.Body.String() != `{"items":[]}` {
		t.Fatalf("clients = %d %s", listed.Code, listed.Body)
	}
	revoked := requestWithCookie(router, http.MethodDelete, "/api/clients/i1234567", cookie, origin)
	if revoked.Code != http.StatusNoContent || stub.principal.Kind != identity.KindSession {
		t.Fatalf("revoke = %d %s, principal %#v", revoked.Code, revoked.Body, stub.principal)
	}
	listed = request(router, http.MethodGet, "/api/clients", "", auth, "")
	if listed.Code != http.StatusOK || stub.principal.Kind != identity.KindClient || stub.principal.UserID != testUserID {
		t.Fatalf("native clients = %d %s, principal %#v", listed.Code, listed.Body, stub.principal)
	}
	revoked = request(router, http.MethodDelete, "/api/clients/i1234567", "", auth, "")
	if revoked.Code != http.StatusNoContent || stub.principal.Kind != identity.KindClient || stub.principal.Role != identity.RoleUser {
		t.Fatalf("native revoke = %d %s, principal %#v", revoked.Code, revoked.Body, stub.principal)
	}
}

func validOptions() Options {
	return Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: 12 * time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test",
		PreviewPortMin: 1024, PreviewPortDeny: []int{8080},
		PortalFiles: fstest.MapFS{
			"index.html":            &fstest.MapFile{Data: []byte("<!doctype html><title>portal</title>")},
			"assets/app.js":         &fstest.MapFile{Data: []byte("console.log('portal')")},
			"app/index.html":        &fstest.MapFile{Data: []byte("<!doctype html><title>app</title>")},
			"app/assets/app.js":     &fstest.MapFile{Data: []byte("console.log('mobile')")},
			"admin/index.html":      &fstest.MapFile{Data: []byte("<!doctype html><title>admin</title>")},
			"admin/assets/admin.js": &fstest.MapFile{Data: []byte("console.log('admin')")},
		},
	}
}

func newRouter(t *testing.T, s *services, override Options) *gin.Engine {
	t.Helper()
	options := validOptions()
	if override.Logger != nil {
		options.Logger = override.Logger
	}
	if override.Metrics != nil {
		options.Metrics = override.Metrics
	}
	if override.APIMaxBodyBytes != 0 {
		options.APIMaxBodyBytes = override.APIMaxBodyBytes
	}
	if override.MCPMaxBodyBytes != 0 {
		options.MCPMaxBodyBytes = override.MCPMaxBodyBytes
	}
	options.CookieSecure = override.CookieSecure
	if override.PortalFiles != nil {
		options.PortalFiles = override.PortalFiles
	}
	router, err := New(s.deps(), options)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func request(router http.Handler, method, path, body, authorization, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestWithHeader(router http.Handler, method, path, body, authorization, origin, name, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set(name, value)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestHTML(router http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestWithCookie(router http.Handler, method, path string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	req.AddCookie(cookie)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestWithCookieBody(router http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestWithCookieBodyAndHeader(router http.Handler, method, path, body string, cookie *http.Cookie, origin, name, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(name, value)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requestWithConflictingCredentials(router http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+apiKeyCredential())
	req.AddCookie(&http.Cookie{Name: portalCookieName, Value: sessionCredential()})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, body = %s; want %d", response.Code, response.Body, status)
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v; body = %s", err, response.Body)
	}
	if body.Error.Code != code || body.Error.Details == nil {
		t.Fatalf("error = %#v, want code %q and non-null details", body.Error, code)
	}
}

func apiKeyCredential() string  { return "chk_" + testTokenID + "_" + strings.Repeat("A", 43) }
func sessionCredential() string { return "chs_" + testTokenID + "_" + strings.Repeat("A", 43) }
func ticketCredential() string  { return "chx_" + testTokenID + "_" + strings.Repeat("A", 43) }
