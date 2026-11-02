package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
)

type hubTestStore struct {
	mu            sync.Mutex
	installations map[string]identity.ClientInstallation
}

func (s *hubTestStore) GetClientInstallation(_ context.Context, id string) (identity.ClientInstallation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, ok := s.installations[id]
	if !ok {
		return identity.ClientInstallation{}, domain.NewError(domain.CodeNotFound, "client installation not found")
	}
	return installation, nil
}

func (s *hubTestStore) ListClientInstallations(_ context.Context, ownerID string) ([]identity.ClientInstallation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]identity.ClientInstallation, 0, len(s.installations))
	for _, installation := range s.installations {
		if installation.OwnerID == ownerID {
			items = append(items, installation)
		}
	}
	return items, nil
}

func (s *hubTestStore) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	installation := s.installations[id]
	installation.RevokedAt = &now
	s.installations[id] = installation
}

type hubTestPeer struct {
	reads     chan []byte
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	readOnce  sync.Once
	readStart chan struct{}
}

func newHubTestPeer() *hubTestPeer {
	return &hubTestPeer{reads: make(chan []byte, 16), writes: make(chan []byte, 128), closed: make(chan struct{}), readStart: make(chan struct{})}
}

func (p *hubTestPeer) Read(ctx context.Context) ([]byte, error) {
	p.readOnce.Do(func() { close(p.readStart) })
	select {
	case raw := <-p.reads:
		return raw, nil
	case <-p.closed:
		return nil, errors.New("peer closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *hubTestPeer) Write(ctx context.Context, raw []byte) error {
	copyRaw := append([]byte(nil), raw...)
	select {
	case p.writes <- copyRaw:
		return nil
	case <-p.closed:
		return errors.New("peer closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *hubTestPeer) Close(_ int, _ string) error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (s *hubDeviceConnectionsStub) CloseOwner(string) {}

func (p *hubTestPeer) push(t *testing.T, frame any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p.reads <- raw:
	case <-p.closed:
		t.Fatal("peer closed before frame was sent")
	case <-time.After(time.Second):
		t.Fatal("timed out sending frame")
	}
}

func (p *hubTestPeer) next(t *testing.T, frameType string) map[string]any {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case raw := <-p.writes:
			var frame map[string]any
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatalf("decode outgoing frame %q: %v", raw, err)
			}
			if frame["type"] == frameType {
				return frame
			}
		case <-p.closed:
			t.Fatalf("peer closed while waiting for %q", frameType)
		case <-deadline:
			t.Fatalf("timed out waiting for %q frame", frameType)
		}
	}
}

func (p *hubTestPeer) maybeNext() map[string]any {
	select {
	case raw := <-p.writes:
		var frame map[string]any
		_ = json.Unmarshal(raw, &frame)
		return frame
	default:
		return nil
	}
}

func newHubFixture(t *testing.T) (*HubService, *hubTestStore) {
	t.Helper()
	store := &hubTestStore{installations: map[string]identity.ClientInstallation{
		"i1234567": {ID: "i1234567", OwnerID: "u1234567", DisplayName: "Desktop", Platform: "desktop", ExpiresAt: time.Now().Add(time.Hour)},
		"i7654321": {ID: "i7654321", OwnerID: "u1234567", DisplayName: "Phone", Platform: "ios", ExpiresAt: time.Now().Add(time.Hour)},
		"i2345678": {ID: "i2345678", OwnerID: "u1234567", DisplayName: "Tablet", Platform: "ios", ExpiresAt: time.Now().Add(time.Hour)},
		"i8765432": {ID: "i8765432", OwnerID: "u7654321", DisplayName: "Other account", Platform: "desktop", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	service, err := NewHubService(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service, store
}

func hubTestPrincipal(ownerID, installationID string) identity.Principal {
	return identity.Principal{
		UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: installationID,
	}
}

func connectHubTestPeer(t *testing.T, service *HubService, ownerID, installationID string) *hubTestPeer {
	t.Helper()
	peer := newHubTestPeer()
	peer.push(t, map[string]string{"type": "hello"})
	go func() { _ = service.Serve(context.Background(), hubTestPrincipal(ownerID, installationID), peer) }()
	welcome := peer.next(t, "welcome")
	if welcome["device_id"] != installationID {
		t.Fatalf("welcome device id = %#v, want authenticated installation %q", welcome["device_id"], installationID)
	}
	return peer
}

func TestHubCallRoutesOnlyAuthorizedBasicActionsAndCorrelatesResult(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	caller.push(t, map[string]any{"type": "call", "id": "call-001", "to": "i7654321", "action": "notify", "args": map[string]string{"text": "ready"}})
	call := target.next(t, "call")
	if call["id"] != "call-001" || call["action"] != "notify" || call["from"].(map[string]any)["id"] != "i1234567" {
		t.Fatalf("forwarded call = %#v", call)
	}
	target.push(t, map[string]any{"type": "result", "id": "call-001", "ok": true, "body": map[string]bool{"notified": true}})
	result := caller.next(t, "result")
	if result["id"] != "call-001" || result["ok"] != true || result["body"].(map[string]any)["notified"] != true {
		t.Fatalf("forwarded result = %#v", result)
	}
}

func TestHubRejectsUnrecognizedActionResultAsUnknown(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	caller.push(t, map[string]any{"type": "call", "id": "call-011", "to": "i7654321", "action": "notify", "args": map[string]string{"text": "ready"}})
	_ = target.next(t, "call")
	target.push(t, map[string]any{"type": "result", "id": "call-011", "ok": true, "body": map[string]any{"notified": true, "path": "/private"}})
	if got := caller.next(t, "error"); got["code"] != "unknown_result" {
		t.Fatalf("unrecognized result = %#v", got)
	}
	if got := target.next(t, "error"); got["code"] != "bad_result" {
		t.Fatalf("target result error = %#v", got)
	}
}

func TestHubRejectsCrossAccountSelfAndUnapprovedActions(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	other := connectHubTestPeer(t, service, "u7654321", "i8765432")
	_ = other.next(t, "devices")

	caller.push(t, map[string]any{"type": "call", "id": "call-101", "to": "i1234567", "action": "info", "args": map[string]any{}})
	if got := caller.next(t, "error"); got["code"] != "self_call" {
		t.Fatalf("self call error = %#v", got)
	}
	caller.push(t, map[string]any{"type": "call", "id": "call-102", "to": "i8765432", "action": "info", "args": map[string]any{}})
	if got := caller.next(t, "error"); got["code"] != "device_offline" {
		t.Fatalf("cross-account call error = %#v", got)
	}
	caller.push(t, map[string]any{"type": "call", "id": "call-103", "to": "i7654321", "action": "shell", "args": map[string]string{"command": "id"}})
	if got := caller.next(t, "error"); got["code"] != "unknown_action" {
		t.Fatalf("unknown action error = %#v", got)
	}
	caller.push(t, map[string]any{"type": "call", "id": "call-104", "to": "i7654321", "action": "open", "args": map[string]string{"url": "file:///etc/passwd"}})
	if got := caller.next(t, "error"); got["code"] != "invalid_argument" {
		t.Fatalf("file open error = %#v", got)
	}
	if frame := other.maybeNext(); frame != nil && frame["type"] == "call" {
		t.Fatalf("cross-account/invalid call reached another device: %#v", frame)
	}
}

func TestHubRejectsResultFromWrongTargetAndReportsUnknownLateResult(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	other := connectHubTestPeer(t, service, "u1234567", "i2345678")
	_ = target.next(t, "devices")
	_ = other.next(t, "devices")
	caller.push(t, map[string]any{"type": "call", "id": "call-201", "to": "i7654321", "action": "info", "args": map[string]any{}})
	_ = target.next(t, "call")
	other.push(t, map[string]any{"type": "result", "id": "call-201", "ok": true, "body": map[string]string{"name": "wrong"}})
	if got := other.next(t, "error"); got["code"] != "unknown_call" {
		t.Fatalf("wrong target result error = %#v", got)
	}

	service.mu.Lock()
	pending := service.pending[pendingKey("u1234567", "call-201")]
	service.mu.Unlock()
	if pending == nil {
		t.Fatal("call was not kept pending for its authenticated target")
	}
	service.expireCall(pending)
	if got := caller.next(t, "error"); got["code"] != "timeout" || !strings.Contains(got["message"].(string), "unknown") {
		t.Fatalf("timeout result = %#v", got)
	}
	if got := target.next(t, "cancel"); got["id"] != "call-201" {
		t.Fatalf("timeout cancellation = %#v", got)
	}
	target.push(t, map[string]any{"type": "result", "id": "call-201", "ok": true, "body": map[string]bool{"opened": true}})
	if got := target.next(t, "error"); got["code"] != "unknown_call" {
		t.Fatalf("late result error = %#v", got)
	}
	if frame := caller.maybeNext(); frame != nil && frame["type"] == "result" {
		t.Fatalf("late result was falsely accepted: %#v", frame)
	}
}

func TestHubCancelIsRequestUntilTargetConfirms(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	caller.push(t, map[string]any{"type": "call", "id": "call-301", "to": "i7654321", "action": "open", "args": map[string]string{"url": "https://example.com"}})
	_ = target.next(t, "call")
	caller.push(t, map[string]string{"type": "cancel", "id": "call-301"})
	if got := caller.next(t, "cancel_pending"); got["state"] != "requested" {
		t.Fatalf("cancel acknowledgement = %#v", got)
	}
	if got := target.next(t, "cancel"); got["id"] != "call-301" {
		t.Fatalf("target cancel = %#v", got)
	}
	target.push(t, map[string]any{"type": "result", "id": "call-301", "ok": false, "error": "cancelled"})
	if got := caller.next(t, "result"); got["error"] != "cancelled" || got["ok"] != false {
		t.Fatalf("target-confirmed cancel = %#v", got)
	}
}

func TestHubDisconnectAndRevocationFailPendingWithoutSuccess(t *testing.T) {
	service, store := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	caller.push(t, map[string]any{"type": "call", "id": "call-401", "to": "i7654321", "action": "notify", "args": map[string]string{"text": "hello"}})
	_ = target.next(t, "call")
	store.revoke("i7654321")
	service.CloseInstallation("i7654321")
	if got := caller.next(t, "error"); got["code"] != "unknown_result" {
		t.Fatalf("revoked target result = %#v", got)
	}
	if service.InstallationOnline("u1234567", "i7654321") {
		t.Fatal("revoked target remained online")
	}
}

func TestHubQueueAndConnectionBounds(t *testing.T) {
	session := &hubSession{queue: make(chan []byte, HubMaxQueuedFrames), done: make(chan struct{})}
	for i := 0; i < HubMaxQueuedFrames; i++ {
		if err := session.enqueue([]byte(fmt.Sprintf("frame-%02d", i))); err != nil {
			t.Fatalf("enqueue frame %d: %v", i, err)
		}
	}
	if err := session.enqueue([]byte("overflow")); err == nil {
		t.Fatal("queue accepted more than its frame bound")
	}
	byteBoundSession := &hubSession{queue: make(chan []byte, HubMaxQueuedFrames), done: make(chan struct{})}
	frame := []byte(strings.Repeat("x", HubMaxQueuedBytes/2))
	for range 2 {
		if err := byteBoundSession.enqueue(frame); err != nil {
			t.Fatalf("enqueue frame within byte bound: %v", err)
		}
	}
	if err := byteBoundSession.enqueue([]byte("x")); err == nil {
		t.Fatal("queue accepted bytes above its bound")
	}
	if HubMaxConnections <= 0 || HubMaxConnectionsPerAccount <= 0 || HubMaxPendingCalls <= 0 {
		t.Fatal("Hub resource limits must be positive")
	}
}

func TestHubLimitsPendingCallsPerDevice(t *testing.T) {
	service, _ := newHubFixture(t)
	caller := connectHubTestPeer(t, service, "u1234567", "i1234567")
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	for i := 0; i < HubMaxPendingCallsPerDevice; i++ {
		id := fmt.Sprintf("call-%03d", i)
		caller.push(t, map[string]any{"type": "call", "id": id, "to": "i7654321", "action": "info", "args": map[string]any{}})
		if got := target.next(t, "call"); got["id"] != id {
			t.Fatalf("forwarded call %d = %#v", i, got)
		}
	}
	caller.push(t, map[string]any{"type": "call", "id": "call-overflow", "to": "i7654321", "action": "info", "args": map[string]any{}})
	if got := caller.next(t, "error"); got["code"] != "too_many_calls" {
		t.Fatalf("call above pending limit = %#v", got)
	}
	service.mu.Lock()
	pending := service.pendingByCaller[pendingCallerKey("u1234567", "i1234567")]
	service.mu.Unlock()
	if pending != HubMaxPendingCallsPerDevice {
		t.Fatalf("pending calls = %d, want %d", pending, HubMaxPendingCallsPerDevice)
	}
}

func TestHubEnforcesPerAccountConnectionLimit(t *testing.T) {
	service, _ := newHubFixture(t)
	for i := 0; i < HubMaxConnectionsPerAccount; i++ {
		session := newHubTestSession("u1234567", i)
		if err := service.attach(session); err != nil {
			t.Fatalf("attach session %d: %v", i, err)
		}
	}
	if err := service.attach(newHubTestSession("u1234567", HubMaxConnectionsPerAccount)); err == nil {
		t.Fatal("account accepted a connection above its limit")
	}
	service.mu.Lock()
	got := len(service.sessions["u1234567"])
	count := service.connectionCount
	service.mu.Unlock()
	if got != HubMaxConnectionsPerAccount || count != HubMaxConnectionsPerAccount {
		t.Fatalf("connections after rejection: account=%d total=%d", got, count)
	}
	if got := service.OnlineInstallationCount(); got != HubMaxConnectionsPerAccount {
		t.Fatalf("online installation count = %d, want %d", got, HubMaxConnectionsPerAccount)
	}
	if err := service.attach(newHubTestSession("u1234567", 0)); err != nil {
		t.Fatalf("replace an existing installation session: %v", err)
	}
	if got := service.OnlineInstallationCount(); got != HubMaxConnectionsPerAccount {
		t.Fatalf("replaced installation changed online count to %d", got)
	}
}

func TestHubEnforcesGlobalConnectionLimit(t *testing.T) {
	service, _ := newHubFixture(t)
	service.mu.Lock()
	service.connectionCount = HubMaxConnections
	service.mu.Unlock()
	if err := service.attach(newHubTestSession("u7654321", 0)); err == nil {
		t.Fatal("Hub accepted a connection above its global limit")
	}
	service.mu.Lock()
	count, accounts := service.connectionCount, len(service.sessions)
	service.mu.Unlock()
	if count != HubMaxConnections || accounts != 0 {
		t.Fatalf("state after global rejection: total=%d accounts=%d", count, accounts)
	}
}

func newHubTestSession(ownerID string, index int) *hubSession {
	id := fmt.Sprintf("i%07x", index)
	return &hubSession{
		ownerID: ownerID, installation: identity.ClientInstallation{ID: id, OwnerID: ownerID},
		peer: newHubTestPeer(), queue: make(chan []byte, HubMaxQueuedFrames), done: make(chan struct{}),
	}
}

func TestHubConnectionRequiresActiveClientInstallation(t *testing.T) {
	service, store := newHubFixture(t)
	peer := newHubTestPeer()
	store.revoke("i1234567")
	if err := service.Serve(context.Background(), hubTestPrincipal("u1234567", "i1234567"), peer); err == nil {
		t.Fatal("revoked installation connected to Hub")
	}
	select {
	case <-peer.closed:
	default:
		t.Fatal("rejected installation connection was not closed")
	}
}

func TestHubRejectsInstallationRevokedBetweenAuthorizationAndAttach(t *testing.T) {
	service, store := newHubFixture(t)
	watcher := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = watcher.next(t, "devices")

	peer := newHubTestPeer()
	served := make(chan error, 1)
	go func() { served <- service.Serve(context.Background(), hubTestPrincipal("u1234567", "i1234567"), peer) }()
	select {
	case <-peer.readStart:
	case <-time.After(2 * time.Second):
		t.Fatal("Hub Serve did not reach hello read after initial authorization")
	}

	store.revoke("i1234567")
	service.CloseInstallation("i1234567")
	peer.push(t, map[string]string{"type": "hello"})
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("revoked installation connected after attach")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hub Serve did not reject the revoked installation")
	}
	select {
	case <-peer.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked installation peer remained open")
	}
	if len(peer.writes) != 0 {
		t.Fatalf("revoked installation received frames before rejection: %d", len(peer.writes))
	}
	if frame := watcher.maybeNext(); frame != nil {
		t.Fatalf("revoked, unannounced installation was broadcast to another device: %#v", frame)
	}
	if service.InstallationOnline("u1234567", "i1234567") {
		t.Fatal("revoked installation remained online")
	}
}

func TestHubCloseOwnerClosesOnlyThatOwnersSessions(t *testing.T) {
	service, _ := newHubFixture(t)
	first := connectHubTestPeer(t, service, "u1234567", "i1234567")
	second := connectHubTestPeer(t, service, "u1234567", "i7654321")
	other := connectHubTestPeer(t, service, "u7654321", "i8765432")

	service.CloseOwner("u1234567")
	for name, peer := range map[string]*hubTestPeer{"first": first, "second": second} {
		select {
		case <-peer.closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s owner's Hub session remained open", name)
		}
	}
	if service.InstallationOnline("u1234567", "i1234567") || service.InstallationOnline("u1234567", "i7654321") {
		t.Fatal("owner session remained online after CloseOwner")
	}
	if !service.InstallationOnline("u7654321", "i8765432") {
		t.Fatal("CloseOwner affected another owner's session")
	}
	other.push(t, map[string]string{"type": "ping"})
	if got := other.next(t, "pong"); got["type"] != "pong" {
		t.Fatalf("other owner's Hub session stopped responding: %#v", got)
	}
}

func TestHubRateLimitAndDevicePresenceStayBounded(t *testing.T) {
	service, _ := newHubFixture(t)
	_ = connectHubTestPeer(t, service, "u1234567", "i1234567")
	if devices := service.InstallationOnline("u1234567", "i1234567"); !devices {
		t.Fatal("connected installation is not online")
	}
	for i := 0; i < hubFrameRateBurst; i++ {
		if !callerSessionAllowed(service, "u1234567", "i1234567") {
			t.Fatalf("frame %d was rejected within the burst", i)
		}
	}
	if callerSessionAllowed(service, "u1234567", "i1234567") {
		t.Fatal("frame beyond the burst was accepted")
	}
	if HubMaxDevicesInPresence*256 > hub.MaxFrameBytes {
		t.Fatal("presence cap may exceed the WebSocket frame bound")
	}
}

func callerSessionAllowed(service *HubService, ownerID, installationID string) bool {
	service.mu.Lock()
	session := service.sessions[ownerID][installationID]
	service.mu.Unlock()
	return session != nil && session.allowFrame(time.Now())
}
