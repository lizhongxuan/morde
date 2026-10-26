package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
)

type testTerminalSessionStore struct {
	current *app.TerminalSession
	history []app.TerminalSession
}

func (s *testTerminalSessionStore) EnsureTerminalSession(_ context.Context, value app.TerminalSession, replace bool) (app.TerminalSession, bool, error) {
	if replace && s.current != nil && s.current.RuntimeGeneration == value.RuntimeGeneration && s.current.State == app.TerminalSessionCreating && time.Since(s.current.CreatedAt) < time.Minute {
		return *s.current, false, nil
	}
	if replace && s.current != nil {
		old := *s.current
		old.State = app.TerminalSessionAbandoned
		s.history = append(s.history, old)
		s.current = nil
	}
	if s.current != nil {
		if s.current.RuntimeGeneration != value.RuntimeGeneration && s.current.State != app.TerminalSessionInterrupted {
			s.current.State = app.TerminalSessionInterrupted
			s.current.AIOSessionID = ""
		} else if s.current.State == app.TerminalSessionCreating && time.Since(s.current.CreatedAt) >= time.Minute {
			s.current.State = app.TerminalSessionUnknown
		}
		return *s.current, false, nil
	}
	value.CreatedAt = time.Now().UTC()
	value.UpdatedAt = value.CreatedAt
	s.current = &value
	return value, true, nil
}

func (s *testTerminalSessionStore) CompleteTerminalSession(_ context.Context, reservation app.TerminalSession, sessionID string) (bool, error) {
	if s.current == nil || s.current.AttemptID != reservation.AttemptID || s.current.State != app.TerminalSessionCreating {
		return false, nil
	}
	s.current.State = app.TerminalSessionReady
	s.current.AIOSessionID = sessionID
	return true, nil
}

func (s *testTerminalSessionStore) MarkTerminalSessionUnknown(_ context.Context, reservation app.TerminalSession) error {
	if s.current != nil && s.current.AttemptID == reservation.AttemptID && s.current.State == app.TerminalSessionCreating {
		s.current.State = app.TerminalSessionUnknown
	}
	return nil
}

func (s *testTerminalSessionStore) MarkTerminalSessionInterrupted(_ context.Context, hostID, actorID string, generation, controlVersion int64, sessionID string) (bool, error) {
	if s.current == nil || s.current.HostID != hostID || s.current.ActorID != actorID ||
		s.current.RuntimeGeneration != generation || s.current.ControlVersion != controlVersion ||
		s.current.AIOSessionID != sessionID || s.current.State != app.TerminalSessionReady {
		return false, nil
	}
	s.current.State = app.TerminalSessionInterrupted
	s.current.AIOSessionID = ""
	return true, nil
}

type testTerminalSessionAIO struct {
	calls   int
	ids     []string
	err     error
	started chan struct{}
	release chan struct{}
}

func (a *testTerminalSessionAIO) TerminalSessionID(ctx context.Context, _ app.Upstream) (string, error) {
	a.calls++
	if a.started != nil {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if a.err != nil {
		return "", a.err
	}
	if len(a.ids) > 0 {
		id := a.ids[0]
		a.ids = a.ids[1:]
		return id, nil
	}
	return "session-1", nil
}

func terminalSessionRequest(handler http.Handler, g *gateway, method, body string) *testRecorder {
	return terminalSessionRequestAs(handler, g, method, body, testUserID)
}

func terminalSessionRequestAs(handler http.Handler, g *gateway, method, body, userID string) *testRecorder {
	req := httptest.NewRequest(method, "/__ch/api/terminal-session", strings.NewReader(body))
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+hostCookie(g, testHostID, userID, time.Hour))
	req.Header.Set("Origin", "https://"+hostAuthority())
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	response := newTestRecorder()
	handler.ServeHTTP(response, req)
	close(response.closed)
	return response
}

func terminalWebSocketStatusRequest(handler http.Handler, g *gateway, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = hostAuthority()
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Cookie", hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour))
	req.Header.Set("Origin", "https://"+hostAuthority())
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "MDEyMzQ1Njc4OWFiY2RlZg==")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func terminalSessionFixture(t *testing.T) (*gatewayHandler, *gateway, *testRuntime, *testTerminalSessionStore, *testTerminalSessionAIO) {
	t.Helper()
	runtime := &testRuntime{ref: host.Ref{
		ID: testHostID, OwnerID: testUserID, State: host.StateRunning,
		RuntimeGeneration: 3, ControlVersion: 7, ControlState: host.ControlUser,
	}, upstreams: []app.Upstream{{BaseURL: "http://aio.example.test"}}}
	store := &testTerminalSessionStore{}
	aio := &testTerminalSessionAIO{ids: []string{"session-1", "session-2"}}
	handler, g := newTestGateway(t, runtime, &testVerifier{}, &testHostService{}, func(d *Deps) {
		d.TerminalSessions = store
		d.TerminalAIO = aio
	})
	return handler, g, runtime, store, aio
}

func TestTerminalSessionPUTCreatesThenReusesAIOHandle(t *testing.T) {
	handler, g, _, store, aio := terminalSessionFixture(t)
	first := terminalSessionRequest(handler, g, http.MethodPut, "")
	second := terminalSessionRequest(handler, g, http.MethodPut, "")
	var firstBody, secondBody terminalSessionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusOK || second.Code != http.StatusOK || firstBody.State != app.TerminalSessionReady ||
		firstBody.URL != "/terminal?session_id=session-1" || secondBody != firstBody || aio.calls != 1 || store.current == nil || store.current.ControlVersion != 7 {
		t.Fatalf("PUT results = %d %+v / %d %+v; AIO calls=%d session=%+v", first.Code, firstBody, second.Code, secondBody, aio.calls, store.current)
	}
	if first.Header().Get("Cache-Control") != "no-store" || first.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("terminal session cache headers = %q / %q", first.Header().Get("Cache-Control"), first.Header().Get("Pragma"))
	}
}

func TestTerminalSessionWebSocketNotFoundInterruptsOnlyMatchingCurrentSession(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler, g, runtime, store, _ := terminalSessionFixture(t)
	runtime.upstreams = []app.Upstream{{BaseURL: upstream.URL}}
	store.current = &app.TerminalSession{
		HostID: testHostID, OwnerID: testUserID, ActorID: testUserID,
		RuntimeGeneration: 3, ControlVersion: 7, AIOSessionID: "session-1", State: app.TerminalSessionReady,
	}
	plainHTTP := request(handler, http.MethodGet, hostAuthority(), "/v1/shell/ws?session_id=session-1", nil,
		hostCookieName+"="+hostCookie(g, testHostID, testUserID, time.Hour), "")
	if plainHTTP.Code != http.StatusNotFound || store.current.State != app.TerminalSessionReady || store.current.AIOSessionID != "session-1" {
		t.Fatalf("non-WebSocket response changed terminal session = %d %+v", plainHTTP.Code, store.current)
	}
	wrongSession := terminalWebSocketStatusRequest(handler, g, "/v1/shell/ws?session_id=other")
	if wrongSession.Code != http.StatusNotFound || store.current.State != app.TerminalSessionReady || store.current.AIOSessionID != "session-1" {
		t.Fatalf("unmatched AIO session response/state = %d %+v", wrongSession.Code, store.current)
	}
	response := terminalWebSocketStatusRequest(handler, g, "/v1/shell/ws?session_id=session-1")
	if response.Code != http.StatusNotFound || store.current.State != app.TerminalSessionInterrupted || store.current.AIOSessionID != "" {
		t.Fatalf("missing AIO session response/state = %d %+v", response.Code, store.current)
	}
}

func TestTerminalSessionWebSocketFailureDoesNotInferSessionLoss(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	handler, g, runtime, store, _ := terminalSessionFixture(t)
	runtime.upstreams = []app.Upstream{{BaseURL: upstream.URL}}
	store.current = &app.TerminalSession{
		HostID: testHostID, OwnerID: testUserID, ActorID: testUserID,
		RuntimeGeneration: 3, ControlVersion: 7, AIOSessionID: "session-1", State: app.TerminalSessionReady,
	}
	response := terminalWebSocketStatusRequest(handler, g, "/v1/shell/ws?session_id=session-1")
	if response.Code != http.StatusServiceUnavailable || store.current.State != app.TerminalSessionReady || store.current.AIOSessionID != "session-1" {
		t.Fatalf("AIO failure response/state = %d %+v", response.Code, store.current)
	}
}

func TestTerminalSessionUnknownAndGenerationChangeNeverAutoCreate(t *testing.T) {
	handler, g, runtime, store, aio := terminalSessionFixture(t)
	aio.err = errors.New("upstream response lost")
	first := terminalSessionRequest(handler, g, http.MethodPut, "")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"state":"unknown"`) || store.current.State != app.TerminalSessionUnknown || aio.calls != 1 {
		t.Fatalf("uncertain create = %d %s calls=%d session=%+v", first.Code, first.Body, aio.calls, store.current)
	}
	aio.err = nil
	retry := terminalSessionRequest(handler, g, http.MethodPut, "")
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"state":"unknown"`) || aio.calls != 1 {
		t.Fatalf("PUT retried unknown create = %d %s calls=%d", retry.Code, retry.Body, aio.calls)
	}

	store.current.State = app.TerminalSessionReady
	store.current.AIOSessionID = "known"
	runtime.ref.RuntimeGeneration++
	interrupted := terminalSessionRequest(handler, g, http.MethodPut, "")
	if interrupted.Code != http.StatusOK || !strings.Contains(interrupted.Body.String(), `"state":"interrupted"`) || aio.calls != 1 || store.current.State != app.TerminalSessionInterrupted {
		t.Fatalf("generation-change PUT = %d %s calls=%d session=%+v", interrupted.Code, interrupted.Body, aio.calls, store.current)
	}
}

func TestTerminalSessionExplicitCreateAndInputValidation(t *testing.T) {
	handler, g, _, store, aio := terminalSessionFixture(t)
	if response := terminalSessionRequest(handler, g, http.MethodPost, `{}`); response.Code != http.StatusBadRequest || aio.calls != 0 {
		t.Fatalf("unconfirmed POST = %d calls=%d body=%s", response.Code, aio.calls, response.Body)
	}
	first := terminalSessionRequest(handler, g, http.MethodPut, "")
	second := terminalSessionRequest(handler, g, http.MethodPost, `{"confirm_new":true}`)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "session_id=session-2") ||
		aio.calls != 2 || len(store.history) != 1 || store.history[0].State != app.TerminalSessionAbandoned {
		t.Fatalf("explicit create = %d %s calls=%d history=%+v", second.Code, second.Body, aio.calls, store.history)
	}
}

func TestTerminalSessionCreatingIsDistinctAndExpiresToUnknown(t *testing.T) {
	handler, g, _, store, aio := terminalSessionFixture(t)
	store.current = &app.TerminalSession{
		HostID: testHostID, OwnerID: testUserID, ActorID: testUserID,
		RuntimeGeneration: 3, ControlVersion: 7, AttemptID: "o1234567",
		State: app.TerminalSessionCreating, CreatedAt: time.Now().UTC(),
	}
	creating := terminalSessionRequest(handler, g, http.MethodPut, "")
	confirmedEarly := terminalSessionRequest(handler, g, http.MethodPost, `{"confirm_new":true}`)
	if creating.Code != http.StatusOK || !strings.Contains(creating.Body.String(), `"state":"creating"`) ||
		confirmedEarly.Code != http.StatusOK || !strings.Contains(confirmedEarly.Body.String(), `"state":"creating"`) || aio.calls != 0 || len(store.history) != 0 {
		t.Fatalf("fresh creating state was replaceable: PUT=%d %s POST=%d %s calls=%d history=%+v", creating.Code, creating.Body, confirmedEarly.Code, confirmedEarly.Body, aio.calls, store.history)
	}
	store.current.CreatedAt = time.Now().Add(-2 * time.Minute)
	unknown := terminalSessionRequest(handler, g, http.MethodPut, "")
	if unknown.Code != http.StatusOK || !strings.Contains(unknown.Body.String(), `"state":"unknown"`) || aio.calls != 0 || store.current.State != app.TerminalSessionUnknown {
		t.Fatalf("stale creating state = %d %s calls=%d session=%+v", unknown.Code, unknown.Body, aio.calls, store.current)
	}
	newSession := terminalSessionRequest(handler, g, http.MethodPost, `{"confirm_new":true}`)
	if newSession.Code != http.StatusOK || !strings.Contains(newSession.Body.String(), "session_id=session-1") || aio.calls != 1 {
		t.Fatalf("explicit create after unknown = %d %s calls=%d", newSession.Code, newSession.Body, aio.calls)
	}
}

func TestTerminalSessionConcurrentPutAndExplicitPostWaitForFirstCreate(t *testing.T) {
	handler, g, _, store, aio := terminalSessionFixture(t)
	aio.started = make(chan struct{})
	aio.release = make(chan struct{})
	firstResult := make(chan *testRecorder, 1)
	go func() {
		firstResult <- terminalSessionRequest(handler, g, http.MethodPut, "")
	}()
	<-aio.started

	concurrentPut := terminalSessionRequest(handler, g, http.MethodPut, "")
	confirmedPost := terminalSessionRequest(handler, g, http.MethodPost, `{"confirm_new":true}`)
	if concurrentPut.Code != http.StatusOK || !strings.Contains(concurrentPut.Body.String(), `"state":"creating"`) ||
		confirmedPost.Code != http.StatusOK || !strings.Contains(confirmedPost.Body.String(), `"state":"creating"`) ||
		aio.calls != 1 || len(store.history) != 0 || store.current == nil || store.current.State != app.TerminalSessionCreating {
		t.Fatalf("concurrent requests changed in-flight creation: PUT=%d %s POST=%d %s calls=%d history=%+v current=%+v",
			concurrentPut.Code, concurrentPut.Body, confirmedPost.Code, confirmedPost.Body, aio.calls, store.history, store.current)
	}

	close(aio.release)
	first := <-firstResult
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"state":"ready"`) || !strings.Contains(first.Body.String(), "session_id=session-1") {
		t.Fatalf("initial create after release = %d %s", first.Code, first.Body)
	}
	reused := terminalSessionRequest(handler, g, http.MethodPut, "")
	if reused.Code != http.StatusOK || !strings.Contains(reused.Body.String(), "session_id=session-1") || aio.calls != 1 || len(store.history) != 0 {
		t.Fatalf("ready session was not reused: %d %s calls=%d history=%+v", reused.Code, reused.Body, aio.calls, store.history)
	}
}

func TestTerminalSessionRequiresUserControlAndStrictInput(t *testing.T) {
	handler, g, runtime, _, aio := terminalSessionFixture(t)
	runtime.ref.ControlState = host.ControlAI
	denied := terminalSessionRequest(handler, g, http.MethodPut, "")
	if denied.Code != http.StatusConflict || aio.calls != 0 {
		t.Fatalf("AI-control terminal = %d calls=%d body=%s", denied.Code, aio.calls, denied.Body)
	}
	runtime.ref.ControlState = host.ControlUser
	badBody := terminalSessionRequest(handler, g, http.MethodPut, `unexpected`)
	if badBody.Code != http.StatusBadRequest || aio.calls != 0 {
		t.Fatalf("PUT body accepted = %d calls=%d body=%s", badBody.Code, aio.calls, badBody.Body)
	}
	badJSON := terminalSessionRequest(handler, g, http.MethodPost, `{"confirm_new":true,"extra":1}`)
	if badJSON.Code != http.StatusBadRequest || aio.calls != 0 {
		t.Fatalf("unknown POST field accepted = %d calls=%d body=%s", badJSON.Code, aio.calls, badJSON.Body)
	}
}

func TestTerminalSessionRequiresTerminalControl(t *testing.T) {
	handler, g, _, _, aio := terminalSessionFixture(t)
	setMemberPermissionGrants(g, host.PermissionDesktopView)
	response := terminalSessionRequestAs(handler, g, http.MethodPut, "", testMemberID)
	if response.Code != http.StatusNotFound || aio.calls != 0 {
		t.Fatalf("non-owner terminal request = %d %s calls=%d", response.Code, response.Body, aio.calls)
	}
}

func TestTerminalControlMemberCanOnlyReachTerminalSurface(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "terminal page")
	}))
	defer upstream.Close()
	handler, g, runtime, _, aio := terminalSessionFixture(t)
	runtime.upstreams = []app.Upstream{{BaseURL: upstream.URL}}
	setMemberPermissionGrants(g, host.PermissionTerminalControl)

	created := terminalSessionRequestAs(handler, g, http.MethodPut, "", testMemberID)
	if created.Code != http.StatusOK || aio.calls != 1 {
		t.Fatalf("member terminal-session request = %d %s, AIO calls=%d", created.Code, created.Body, aio.calls)
	}
	memberCookie := hostCookieName + "=" + hostCookie(g, testHostID, testMemberID, time.Hour)
	page := request(handler, http.MethodGet, hostAuthority(), "/terminal?session_id=session-1", nil, memberCookie, "")
	if page.Code != http.StatusOK || page.Body.String() != "terminal page" {
		t.Fatalf("member terminal page = %d %q", page.Code, page.Body.String())
	}
	for _, path := range []string{"/__ch/api/control", "/__ch/api/host", "/v1/shell/exec", "/v1/file/list", "/vnc/index.html"} {
		response := request(handler, http.MethodGet, hostAuthority(), path, nil, memberCookie, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("terminal-only member path %s response = %d, want 404", path, response.Code)
		}
	}
}
