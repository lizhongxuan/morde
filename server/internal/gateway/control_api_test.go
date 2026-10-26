package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

type testGatewayControlService struct {
	status                   app.ControlStatus
	statusErr                error
	statusCalls              int
	statusHostID             string
	statusActor              identity.Principal
	result                   app.ControlActionResult
	actErr                   error
	actCalls                 int
	actHostID                string
	actActor                 identity.Principal
	command                  app.ControlActionCommand
	applications             app.ApplicationsStatus
	applicationsErr          error
	applicationsCalls        int
	applicationsHostID       string
	applicationsActor        identity.Principal
	applicationInstallResult app.ApplicationInstallResult
	applicationInstallErr    error
	applicationInstallCalls  int
	applicationInstallHostID string
	applicationInstallAppID  string
	applicationInstallID     string
	applicationInstallActor  identity.Principal
	applicationLaunchResult  app.ApplicationLaunchResult
	applicationLaunchErr     error
	applicationLaunchCalls   int
	applicationLaunchHostID  string
	applicationLaunchAppID   string
	applicationLaunchID      string
	applicationLaunchActor   identity.Principal
	currentLaunchResult      app.CurrentApplicationLaunchResult
	currentLaunchErr         error
	currentLaunchCalls       int
	currentLaunchHostID      string
	currentLaunchAppID       string
	currentLaunchActor       identity.Principal
	getLaunchResult          app.ApplicationLaunchResult
	getLaunchErr             error
	getLaunchCalls           int
	getLaunchHostID          string
	getLaunchAppID           string
	getLaunchID              string
	getLaunchActor           identity.Principal
	stopLaunchResult         app.ApplicationLaunchResult
	stopLaunchErr            error
	stopLaunchCalls          int
	stopLaunchHostID         string
	stopLaunchAppID          string
	stopLaunchID             string
	stopLaunchActor          identity.Principal
}

func (s *testGatewayControlService) Status(_ context.Context, principal identity.Principal, hostID string) (app.ControlStatus, error) {
	s.statusCalls++
	s.statusHostID, s.statusActor = hostID, principal
	return s.status, s.statusErr
}

func (s *testGatewayControlService) Act(_ context.Context, principal identity.Principal, hostID string, command app.ControlActionCommand) (app.ControlActionResult, error) {
	s.actCalls++
	s.actHostID, s.actActor, s.command = hostID, principal, command
	return s.result, s.actErr
}

func (s *testGatewayControlService) Applications(_ context.Context, principal identity.Principal, hostID string) (app.ApplicationsStatus, error) {
	s.applicationsCalls++
	s.applicationsHostID, s.applicationsActor = hostID, principal
	return s.applications, s.applicationsErr
}

func (s *testGatewayControlService) InstallApplication(_ context.Context, principal identity.Principal, hostID, appID, requestID string) (app.ApplicationInstallResult, error) {
	s.applicationInstallCalls++
	s.applicationInstallHostID, s.applicationInstallAppID, s.applicationInstallID = hostID, appID, requestID
	s.applicationInstallActor = principal
	return s.applicationInstallResult, s.applicationInstallErr
}

func (s *testGatewayControlService) LaunchApplication(_ context.Context, principal identity.Principal, hostID, appID, launchID string) (app.ApplicationLaunchResult, error) {
	s.applicationLaunchCalls++
	s.applicationLaunchHostID, s.applicationLaunchAppID, s.applicationLaunchID, s.applicationLaunchActor = hostID, appID, launchID, principal
	return s.applicationLaunchResult, s.applicationLaunchErr
}

func (s *testGatewayControlService) CurrentApplicationLaunch(_ context.Context, principal identity.Principal, hostID, appID string) (app.CurrentApplicationLaunchResult, error) {
	s.currentLaunchCalls++
	s.currentLaunchHostID, s.currentLaunchAppID, s.currentLaunchActor = hostID, appID, principal
	return s.currentLaunchResult, s.currentLaunchErr
}

func (s *testGatewayControlService) GetApplicationLaunch(_ context.Context, principal identity.Principal, hostID, appID, launchID string) (app.ApplicationLaunchResult, error) {
	s.getLaunchCalls++
	s.getLaunchHostID, s.getLaunchAppID, s.getLaunchID, s.getLaunchActor = hostID, appID, launchID, principal
	return s.getLaunchResult, s.getLaunchErr
}

func (s *testGatewayControlService) StopApplicationLaunch(_ context.Context, principal identity.Principal, hostID, appID, launchID string) (app.ApplicationLaunchResult, error) {
	s.stopLaunchCalls++
	s.stopLaunchHostID, s.stopLaunchAppID, s.stopLaunchID, s.stopLaunchActor = hostID, appID, launchID, principal
	return s.stopLaunchResult, s.stopLaunchErr
}

func performControlActionRequest(handler http.Handler, g *gateway, body string, contentType string) *testRecorder {
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/__ch/api/control/actions", strings.NewReader(body))
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+cookie)
	req.Header.Set("Origin", "https://"+hostAuthority())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	return response
}

func performApplicationInstallRequest(handler http.Handler, g *gateway, path, body string, contentType string) *testRecorder {
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+cookie)
	req.Header.Set("Origin", "https://"+hostAuthority())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	return response
}

func performApplicationLaunchRequest(handler http.Handler, g *gateway, method, path, body, contentType string) *testRecorder {
	cookie := hostCookie(g, testHostID, testUserID, time.Hour)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+cookie)
	req.Header.Set("Origin", "https://"+hostAuthority())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	return response
}

func TestGatewayControlStatusUsesHostCookieAndOmitsRuntimeSecrets(t *testing.T) {
	now := time.Date(2026, time.October, 21, 1, 2, 3, 0, time.UTC)
	service := &testGatewayControlService{status: app.ControlStatus{
		HostID: testHostID, ControlState: host.ControlTakingOver, ControlVersion: 1,
		ActiveTransition: &control.Transition{
			HostID: testHostID, OwnerID: testUserID, RequestID: "handoff-42", Action: control.ActionTakeOver,
			Phase: control.PhaseDraining, FailureReason: "operations_pending", ExpectedControlVersion: 0,
			BlockedControlVersion: 1, TargetControlVersion: 2, TaskIDs: []string{"qsecret01"},
			DrainedReceipt: json.RawMessage(`{"credential":"do-not-return"}`), CreatedAt: now, UpdatedAt: now,
		},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/control", nil,
		hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if response.Code != http.StatusOK || service.statusCalls != 1 || service.statusHostID != testHostID {
		t.Fatalf("GET control = %d calls=%d host=%q body=%s", response.Code, service.statusCalls, service.statusHostID, response.Body)
	}
	if service.statusActor.UserID != testUserID || service.statusActor.Kind != identity.KindSession {
		t.Fatalf("control actor = %+v; want verified host owner session", service.statusActor)
	}
	for _, secret := range []string{"qsecret01", "do-not-return", "drained_receipt", "task_ids", "owner_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("control status leaked %q: %s", secret, response.Body)
		}
	}
	for _, field := range []string{`"host_id":"` + testHostID + `"`, `"control_state":"taking_over"`, `"phase":"draining"`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("control status omitted %s: %s", field, response.Body)
		}
	}
}

func TestGatewayApplicationsStatusIsOwnerAuthorizedValidatedAndNotCached(t *testing.T) {
	checkedAt := time.Date(2026, time.October, 21, 1, 2, 3, 0, time.UTC)
	installOperation := runtimeclient.ApplicationInstallOperation{
		ID: "app_1234", ApplicationID: "wechat", Status: runtimeclient.OperationAccepted,
		ProgressPercent: 0, CreatedAt: checkedAt, UpdatedAt: checkedAt,
	}
	service := &testGatewayControlService{applications: app.ApplicationsStatus{
		HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7,
		CheckedAt: checkedAt, Items: []runtimeclient.ApplicationStatusItem{{
			ID: "codex", Installed: true, Version: "0.164.0", LoginStatus: runtimeclient.ApplicationLoginLoggedIn,
		}, {
			ID: "wechat", Installable: true, LoginStatus: runtimeclient.ApplicationLoginNotApplicable,
			InstallRequestID: "private-install-request-01", InstallOperation: &installOperation,
		}},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications", nil,
		hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if response.Code != http.StatusOK || service.applicationsCalls != 1 || service.applicationsHostID != testHostID {
		t.Fatalf("GET applications = %d calls=%d host=%q body=%s", response.Code, service.applicationsCalls, service.applicationsHostID, response.Body)
	}
	if service.applicationsActor.UserID != testUserID || service.applicationsActor.Kind != identity.KindSession {
		t.Fatalf("applications actor = %+v; want verified Host owner session", service.applicationsActor)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("application cache headers = %q / %q", response.Header().Get("Cache-Control"), response.Header().Get("Pragma"))
	}
	for _, field := range []string{`"host_id":"` + testHostID + `"`, `"runtime_generation":3`, `"runtime_instance_id":"ri_current"`,
		`"control_version":7`, `"checked_at":"2026-10-21T01:02:03Z"`, `"id":"codex"`, `"installed":true`,
		`"version":"0.164.0"`, `"login_status":"logged_in"`, `"id":"wechat"`, `"installable":true`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("applications response omitted %s: %s", field, response.Body)
		}
	}
	for _, secret := range []string{"stdout", "stderr", "auth.json", "account", "token", "Authorization", "install_request_id", "private-install-request-01"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("applications response leaked %q: %s", secret, response.Body)
		}
	}
	withCommandArgument := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications?command=whoami", nil,
		hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if withCommandArgument.Code != http.StatusBadRequest || service.applicationsCalls != 1 {
		t.Fatalf("application query arguments were accepted: status=%d calls=%d body=%s", withCommandArgument.Code, service.applicationsCalls, withCommandArgument.Body)
	}

	unauthorized := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications", nil, "", "")
	if unauthorized.Code != http.StatusNotFound || service.applicationsCalls != 1 {
		t.Fatalf("unauthorized GET applications = %d calls=%d body=%s", unauthorized.Code, service.applicationsCalls, unauthorized.Body)
	}
}

func TestGatewayApplicationsRejectsUnvalidatedServiceResponse(t *testing.T) {
	service := &testGatewayControlService{applications: app.ApplicationsStatus{
		HostID: testHostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_current", ControlVersion: 0,
		CheckedAt: time.Now().UTC(), Items: []runtimeclient.ApplicationStatusItem{{
			ID: "codex", Installed: true, Version: "not-semver", LoginStatus: runtimeclient.ApplicationLoginLoggedIn,
		}},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications", nil,
		hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "not-semver") {
		t.Fatalf("invalid applications response = %d %s", response.Code, response.Body)
	}
}

func TestGatewayApplicationInstallReturnsFencedSafeOperation(t *testing.T) {
	now := time.Date(2026, time.October, 25, 1, 2, 3, 0, time.UTC)
	service := &testGatewayControlService{applicationInstallResult: app.ApplicationInstallResult{
		HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7,
		Operation: runtimeclient.ApplicationInstallOperation{
			ID: "app_1234", ApplicationID: "wechat", Status: runtimeclient.OperationAccepted,
			ProgressPercent: 0, CreatedAt: now, UpdatedAt: now,
		},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := performApplicationInstallRequest(handler, g, "/__ch/api/applications/wechat/install", `{"request_id":"install-wechat-01"}`, "application/json; charset=utf-8")
	if response.Code != http.StatusAccepted || service.applicationInstallCalls != 1 || service.applicationInstallHostID != testHostID ||
		service.applicationInstallAppID != "wechat" || service.applicationInstallID != "install-wechat-01" {
		t.Fatalf("POST application install = %d calls=%d host=%q app=%q request=%q body=%s", response.Code,
			service.applicationInstallCalls, service.applicationInstallHostID, service.applicationInstallAppID, service.applicationInstallID, response.Body)
	}
	if service.applicationInstallActor.UserID != testUserID || service.applicationInstallActor.Kind != identity.KindSession {
		t.Fatalf("install actor = %+v; want verified Host owner session", service.applicationInstallActor)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("application install cache headers = %q / %q", response.Header().Get("Cache-Control"), response.Header().Get("Pragma"))
	}
	for _, field := range []string{`"host_id":"` + testHostID + `"`, `"runtime_generation":3`, `"runtime_instance_id":"ri_current"`,
		`"control_version":7`, `"id":"app_1234"`, `"application_id":"wechat"`, `"status":"accepted"`,
		`"progress_percent":0`, `"error_code":""`, `"created_at":"2026-10-25T01:02:03Z"`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("application install response omitted %s: %s", field, response.Body)
		}
	}
	for _, secret := range []string{"stdout", "stderr", "account", "token", "Authorization", "command"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("application install response leaked %q: %s", secret, response.Body)
		}
	}

	unauthorized := request(handler, http.MethodPost, hostAuthority(), "/__ch/api/applications/wechat/install", strings.NewReader(`{"request_id":"install-wechat-02"}`), "", "")
	if unauthorized.Code != http.StatusNotFound || service.applicationInstallCalls != 1 {
		t.Fatalf("unauthorized install = %d calls=%d body=%s", unauthorized.Code, service.applicationInstallCalls, unauthorized.Body)
	}
}

func TestGatewayApplicationInstallRejectsInvalidRequestsBeforeService(t *testing.T) {
	service := &testGatewayControlService{}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	for _, test := range []struct {
		name        string
		path        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "unknown app", path: "/__ch/api/applications/codex/install", body: `{"request_id":"install-wechat-03"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "query", path: "/__ch/api/applications/wechat/install?request_id=install-wechat-04", body: `{"request_id":"install-wechat-04"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "wrong content type", path: "/__ch/api/applications/wechat/install", body: `{}`, contentType: "text/plain", wantStatus: http.StatusBadRequest},
		{name: "unknown field", path: "/__ch/api/applications/wechat/install", body: `{"request_id":"install-wechat-05","command":"id"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "multiple values", path: "/__ch/api/applications/wechat/install", body: `{"request_id":"install-wechat-06"} {}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing id", path: "/__ch/api/applications/wechat/install", body: `{}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "bad id", path: "/__ch/api/applications/wechat/install", body: `{"request_id":"bad key"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "oversized body", path: "/__ch/api/applications/wechat/install", body: `{"request_id":"install-wechat-07","padding":"` + strings.Repeat("x", maxControlActionBytes) + `"}`, contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performApplicationInstallRequest(handler, g, test.path, test.body, test.contentType)
			if response.Code != test.wantStatus || service.applicationInstallCalls != 0 {
				t.Fatalf("invalid application install = %d calls=%d body=%s; want status %d and no service call", response.Code, service.applicationInstallCalls, response.Body, test.wantStatus)
			}
		})
	}
}

func TestGatewayApplicationInstallRejectsInvalidServiceResponse(t *testing.T) {
	now := time.Now().UTC()
	service := &testGatewayControlService{applicationInstallResult: app.ApplicationInstallResult{
		HostID: testHostID, RuntimeGeneration: 1, RuntimeInstanceID: "ri_current", ControlVersion: 0,
		Operation: runtimeclient.ApplicationInstallOperation{
			ID: "bad", ApplicationID: "wechat", Status: runtimeclient.OperationRunning,
			ProgressPercent: 200, CreatedAt: now, UpdatedAt: now,
		},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := performApplicationInstallRequest(handler, g, "/__ch/api/applications/wechat/install", `{"request_id":"install-wechat-08"}`, "application/json")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), `"progress_percent":200`) {
		t.Fatalf("invalid install result = %d %s", response.Code, response.Body)
	}
}

func TestGatewayWechatLaunchRoutesReturnFencedStatusAndRemainPrivate(t *testing.T) {
	now := time.Date(2026, time.October, 25, 2, 3, 4, 0, time.UTC)
	operation := runtimeclient.ApplicationLaunchOperation{
		ID: "launch_test-01", ApplicationID: "wechat", Status: runtimeclient.ApplicationLaunchRunning,
		CreatedAt: now, UpdatedAt: now,
	}
	service := &testGatewayControlService{
		applicationLaunchResult: app.ApplicationLaunchResult{HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7, Operation: operation},
		currentLaunchResult:     app.CurrentApplicationLaunchResult{HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7},
		getLaunchResult:         app.ApplicationLaunchResult{HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7, Operation: operation},
		stopLaunchResult:        app.ApplicationLaunchResult{HostID: testHostID, RuntimeGeneration: 3, RuntimeInstanceID: "ri_current", ControlVersion: 7, Operation: operation},
	}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning, ControlState: host.ControlAI, RuntimeGeneration: 3, ControlVersion: 7}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	launch := performApplicationLaunchRequest(handler, g, http.MethodPost, "/__ch/api/applications/wechat/launch", `{"launch_id":"launch_test-01"}`, "application/json")
	if launch.Code != http.StatusAccepted || service.applicationLaunchCalls != 1 || service.applicationLaunchHostID != testHostID || service.applicationLaunchAppID != app.WechatApplicationID || service.applicationLaunchID != operation.ID {
		t.Fatalf("POST WeChat launch = %d calls=%d host=%q id=%q body=%s", launch.Code, service.applicationLaunchCalls, service.applicationLaunchHostID, service.applicationLaunchID, launch.Body)
	}
	if service.applicationLaunchActor.UserID != testUserID || service.applicationLaunchActor.Kind != identity.KindSession {
		t.Fatalf("launch actor = %+v", service.applicationLaunchActor)
	}
	if launch.Header().Get("Cache-Control") != "no-store" || launch.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("launch cache headers = %q / %q", launch.Header().Get("Cache-Control"), launch.Header().Get("Pragma"))
	}
	for _, field := range []string{`"host_id":"` + testHostID + `"`, `"runtime_generation":3`, `"runtime_instance_id":"ri_current"`,
		`"control_version":7`, `"id":"launch_test-01"`, `"application_id":"wechat"`, `"status":"running"`,
		`"error_code":""`, `"created_at":"2026-10-25T02:03:04Z"`} {
		if !strings.Contains(launch.Body.String(), field) {
			t.Fatalf("launch response omitted %s: %s", field, launch.Body)
		}
	}
	for _, secret := range []string{"stdout", "stderr", "token", "password", "command", "pid"} {
		if strings.Contains(launch.Body.String(), secret) {
			t.Fatalf("launch response leaked %q: %s", secret, launch.Body)
		}
	}

	current := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications/wechat/launches/current", nil, hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if current.Code != http.StatusOK || service.currentLaunchCalls != 1 || service.currentLaunchAppID != app.WechatApplicationID || !strings.Contains(current.Body.String(), `"operation":null`) {
		t.Fatalf("GET current launch = %d calls=%d body=%s", current.Code, service.currentLaunchCalls, current.Body)
	}
	if current.Header().Get("Cache-Control") != "no-store" || current.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("current launch cache headers = %q / %q", current.Header().Get("Cache-Control"), current.Header().Get("Pragma"))
	}

	exact := request(handler, http.MethodGet, hostAuthority(), "/__ch/api/applications/wechat/launches/launch_test-01", nil, hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if exact.Code != http.StatusOK || service.getLaunchCalls != 1 || service.getLaunchAppID != app.WechatApplicationID || service.getLaunchID != operation.ID {
		t.Fatalf("GET exact launch = %d calls=%d id=%q body=%s", exact.Code, service.getLaunchCalls, service.getLaunchID, exact.Body)
	}

	stop := performApplicationLaunchRequest(handler, g, http.MethodPost, "/__ch/api/applications/wechat/launches/launch_test-01/stop", "", "")
	if stop.Code != http.StatusAccepted || service.stopLaunchCalls != 1 || service.stopLaunchAppID != app.WechatApplicationID || service.stopLaunchID != operation.ID {
		t.Fatalf("POST launch stop = %d calls=%d id=%q body=%s", stop.Code, service.stopLaunchCalls, service.stopLaunchID, stop.Body)
	}
	if service.stopLaunchActor.UserID != testUserID || service.stopLaunchActor.Kind != identity.KindSession {
		t.Fatalf("stop actor = %+v", service.stopLaunchActor)
	}
	leases := g.deps.ActivityLeases.(*testActivityLeaseStore)
	if len(leases.acquired) != 2 || len(leases.released) != 2 {
		t.Fatalf("launch/stop activity leases acquired=%d released=%d", len(leases.acquired), len(leases.released))
	}
}

func TestGatewayWechatLaunchRejectsMalformedInputBeforeService(t *testing.T) {
	service := &testGatewayControlService{}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning, ControlState: host.ControlUser, RuntimeGeneration: 1}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	for _, test := range []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "bad ID", method: http.MethodPost, path: "/__ch/api/applications/wechat/launch", body: `{"launch_id":"bad"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "unknown field", method: http.MethodPost, path: "/__ch/api/applications/wechat/launch", body: `{"launch_id":"launch_test-02","command":"ps"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "wrong media type", method: http.MethodPost, path: "/__ch/api/applications/wechat/launch", body: `{"launch_id":"launch_test-02"}`, contentType: "text/plain", wantStatus: http.StatusBadRequest},
		{name: "unexpected query", method: http.MethodPost, path: "/__ch/api/applications/wechat/launch?app=wechat", body: `{"launch_id":"launch_test-02"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "stop body", method: http.MethodPost, path: "/__ch/api/applications/wechat/launches/launch_test-02/stop", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "unsupported application", method: http.MethodGet, path: "/__ch/api/applications/qq/launches/current", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performApplicationLaunchRequest(handler, g, test.method, test.path, test.body, test.contentType)
			if response.Code != test.wantStatus || service.applicationLaunchCalls+service.currentLaunchCalls+service.getLaunchCalls+service.stopLaunchCalls != 0 {
				t.Fatalf("invalid launch request = %d calls=%d body=%s; want %d and no service call", response.Code,
					service.applicationLaunchCalls+service.currentLaunchCalls+service.getLaunchCalls+service.stopLaunchCalls, response.Body, test.wantStatus)
			}
		})
	}
}

func TestGatewayControlActionReturnsPendingAndUsesVerifiedHost(t *testing.T) {
	now := time.Now().UTC()
	service := &testGatewayControlService{result: app.ControlActionResult{
		Status: app.ControlStatus{HostID: testHostID, ControlState: host.ControlTakingOver, ControlVersion: 1,
			ActiveTransition: &control.Transition{HostID: testHostID, RequestID: "handoff-1", Action: control.ActionTakeOver,
				Phase: control.PhaseDraining, ExpectedControlVersion: 0, BlockedControlVersion: 1, TargetControlVersion: 2,
				CreatedAt: now, UpdatedAt: now}},
		PendingOperationIDs: []string{"o1234567"},
	}}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	response := performControlActionRequest(handler, g, `{"action":"take_over","request_id":"handoff-1","expected_control_version":0}`, "application/json; charset=utf-8")
	if response.Code != http.StatusAccepted || service.actCalls != 1 || service.actHostID != testHostID {
		t.Fatalf("POST control = %d calls=%d host=%q body=%s", response.Code, service.actCalls, service.actHostID, response.Body)
	}
	if service.actActor.UserID != testUserID || service.actActor.Kind != identity.KindSession || service.command.Action != control.ActionTakeOver || service.command.RequestID != "handoff-1" || service.command.ExpectedControlVersion != 0 {
		t.Fatalf("control request mapping = actor=%+v command=%+v", service.actActor, service.command)
	}
	if !strings.Contains(response.Body.String(), `"pending_operation_ids":["o1234567"]`) {
		t.Fatalf("POST control omitted pending operation: %s", response.Body)
	}
}

func TestGatewayControlActionRejectsMalformedAndOversizedRequests(t *testing.T) {
	service := &testGatewayControlService{}
	handler, g := newTestGateway(t, &testRuntime{ref: host.Ref{ID: testHostID, OwnerID: testUserID, State: host.StateRunning}},
		&testVerifier{}, &testHostService{}, func(d *Deps) { d.Control = service })
	for _, test := range []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "wrong content type", body: `{}`, contentType: "text/plain", wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"action":"take_over","request_id":"h1","expected_control_version":0,"extra":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "multiple values", body: `{"action":"take_over","request_id":"h1","expected_control_version":0} {}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing version", body: `{"action":"take_over","request_id":"h1"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "body too large", body: `{"action":"take_over","request_id":"h1","expected_control_version":0,"padding":"` + strings.Repeat("x", maxControlActionBytes) + `"}`, contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performControlActionRequest(handler, g, test.body, test.contentType)
			if response.Code != test.wantStatus || service.actCalls != 0 {
				t.Fatalf("invalid action = %d calls=%d body=%s; want status %d and no service call", response.Code, service.actCalls, response.Body, test.wantStatus)
			}
		})
	}
}
