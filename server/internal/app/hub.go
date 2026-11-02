package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
)

const (
	HubMaxConnections            = 2048
	HubMaxConnectionsPerAccount  = 32
	HubMaxDevicesInPresence      = 256
	HubMaxQueuedFrames           = 32
	HubMaxQueuedBytes            = 128 << 10
	HubMaxPendingCalls           = 4096
	HubMaxPendingCallsPerAccount = 128
	HubMaxPendingCallsPerDevice  = 16
	hubHelloTimeout              = 15 * time.Second
	hubCallTimeout               = 30 * time.Second
	hubFrameRatePerSecond        = 60
	hubFrameRateBurst            = 120
)

type HubPeer interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close(int, string) error
}

type HubConnectionService interface {
	Serve(context.Context, identity.Principal, HubPeer) error
}

type HubInstallationStore interface {
	GetClientInstallation(context.Context, string) (identity.ClientInstallation, error)
	ListClientInstallations(context.Context, string) ([]identity.ClientInstallation, error)
}

type HubDeviceConnections interface {
	InstallationOnline(ownerID, installationID string) bool
	RenameInstallation(installationID, name string)
	CloseInstallation(installationID string)
	CloseOwner(ownerID string)
}

type HubService struct {
	store HubInstallationStore

	mu               sync.Mutex
	sessions         map[string]map[string]*hubSession
	pending          map[string]*hubPendingCall
	pendingByAccount map[string]int
	pendingByCaller  map[string]int
	connectionCount  int
	closed           bool
}

type hubSession struct {
	ownerID      string
	installation identity.ClientInstallation
	principal    identity.Principal
	peer         HubPeer
	queue        chan []byte
	done         chan struct{}
	closeOnce    sync.Once
	queueMu      sync.Mutex
	queuedBytes  int
	rateMu       sync.Mutex
	rateTokens   float64
	rateUpdated  time.Time
}

type hubPendingCall struct {
	key         string
	ownerID     string
	callerKey   string
	id          string
	action      string
	caller      *hubSession
	target      *hubSession
	taskContext *hub.TaskContext
	taskResult  chan HubTaskCallResult
	timer       *time.Timer
}

type hubDelivery struct {
	target         *hubSession
	frame          any
	taskResult     chan HubTaskCallResult
	taskResultBody HubTaskCallResult
}

func NewHubService(store HubInstallationStore) (*HubService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("store", "hub installation store is required")
	}
	return &HubService{
		store: store, sessions: make(map[string]map[string]*hubSession),
		pending: make(map[string]*hubPendingCall), pendingByAccount: make(map[string]int),
		pendingByCaller: make(map[string]int),
	}, nil
}

func (s *HubService) Serve(ctx context.Context, principal identity.Principal, peer HubPeer) error {
	if peer == nil {
		return domain.InvalidArgument("peer", "hub peer is required")
	}
	installation, err := s.authorizeInstallation(ctx, principal)
	if err != nil {
		_ = peer.Close(4001, "unauthorized")
		return err
	}
	helloCtx, cancel := context.WithTimeout(ctx, hubHelloTimeout)
	raw, err := peer.Read(helloCtx)
	cancel()
	if err != nil {
		_ = peer.Close(4000, "hello expected")
		return fmt.Errorf("read Hub hello: %w", err)
	}
	frame, err := hub.DecodeFrame(raw)
	if err != nil || hub.ValidateHello(frame) != nil {
		_ = s.writeBeforeAttach(peer, hub.Frame{Type: "error", Code: "bad_hello", Message: "a valid hello frame is required"})
		_ = peer.Close(4000, "bad hello")
		return nil
	}

	session := &hubSession{
		ownerID: principal.UserID, installation: installation, principal: principal,
		peer: peer, queue: make(chan []byte, HubMaxQueuedFrames), done: make(chan struct{}),
		rateTokens: hubFrameRateBurst, rateUpdated: time.Now(),
	}
	if err := s.attach(session); err != nil {
		_ = s.writeBeforeAttach(peer, hub.Frame{Type: "error", Code: "too_many_connections", Message: "hub connection limit reached"})
		_ = peer.Close(4008, "too many connections")
		return nil
	}
	installation, err = s.authorizeInstallation(ctx, principal)
	if err != nil {
		s.terminateUnannounced(session, 4001, "unauthorized")
		return err
	}
	session.installation = installation
	go s.writeLoop(session)
	defer s.terminate(session, 1000, "disconnected")

	devices, err := s.listDevices(ctx, principal.UserID)
	if err != nil {
		return s.failSession(session, "internal_error", "device presence is unavailable")
	}
	if !s.send(session, map[string]any{
		"type": "welcome", "device_id": installation.ID, "devices": devices,
		"server": map[string]any{"frame_limit": hub.MaxFrameBytes, "time": time.Now().Unix()},
	}) {
		return nil
	}
	s.broadcastDevices(ctx, principal.UserID)

	for {
		raw, err := peer.Read(ctx)
		if err != nil {
			return nil
		}
		if !session.allowFrame(time.Now()) {
			s.sendError(session, "", "rate_limited", "hub frame rate limit exceeded")
			return nil
		}
		frame, err := hub.DecodeFrame(raw)
		if err != nil {
			s.sendError(session, "", protocolCode(err, "bad_frame"), protocolMessage(err, "invalid frame"))
			continue
		}
		if err := s.route(ctx, session, frame); err != nil {
			return err
		}
	}
}

func (s *HubService) Close() {
	s.mu.Lock()
	s.closed = true
	var sessions []*hubSession
	for _, account := range s.sessions {
		for _, session := range account {
			sessions = append(sessions, session)
		}
	}
	s.sessions = make(map[string]map[string]*hubSession)
	s.connectionCount = 0
	pending := make([]*hubPendingCall, 0, len(s.pending))
	for key := range s.pending {
		pending = append(pending, s.removePendingLocked(key))
	}
	s.mu.Unlock()
	for _, call := range pending {
		if call.taskResult != nil {
			call.taskResult <- HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "Hub closed before the target confirmed the result"}
		}
	}
	for _, session := range sessions {
		session.close(1001, "server shutdown")
	}
}

func (s *HubService) InstallationOnline(ownerID, installationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[ownerID][installationID] != nil
}

func (s *HubService) OnlineInstallationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, installations := range s.sessions {
		count += len(installations)
	}
	return count
}

func (s *HubService) RenameInstallation(installationID, name string) {
	s.mu.Lock()
	var owners []string
	for ownerID, account := range s.sessions {
		if session := account[installationID]; session != nil {
			session.installation.DisplayName = name
			owners = append(owners, ownerID)
		}
	}
	s.mu.Unlock()
	for _, ownerID := range owners {
		s.broadcastDevices(context.Background(), ownerID)
	}
}

func (s *HubService) CloseInstallation(installationID string) {
	s.mu.Lock()
	var sessions []*hubSession
	for _, account := range s.sessions {
		if session := account[installationID]; session != nil {
			sessions = append(sessions, session)
		}
	}
	s.mu.Unlock()
	for _, session := range sessions {
		s.terminate(session, 4001, "installation revoked")
	}
}

func (s *HubService) CloseOwner(ownerID string) {
	s.mu.Lock()
	account := s.sessions[ownerID]
	sessions := make([]*hubSession, 0, len(account))
	for _, session := range account {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	for _, session := range sessions {
		s.terminate(session, 4001, "account authorization revoked")
	}
}

func (s *HubService) route(ctx context.Context, source *hubSession, frame hub.Frame) error {
	switch frame.Type {
	case "ping":
		if !emptyFrame(frame) {
			s.sendError(source, "", "bad_frame", "ping accepts no fields")
			return nil
		}
		s.send(source, map[string]any{"type": "pong", "time": time.Now().Unix()})
		return nil
	case "devices":
		if !emptyFrame(frame) {
			s.sendError(source, "", "bad_frame", "devices accepts no fields")
			return nil
		}
		devices, err := s.listDevices(ctx, source.ownerID)
		if err != nil {
			s.sendError(source, "", "internal_error", "device presence is unavailable")
			return nil
		}
		s.send(source, map[string]any{"type": "devices", "devices": devices})
		return nil
	case "call":
		return s.call(ctx, source, frame)
	case "result":
		return s.result(ctx, source, frame)
	case "cancel":
		return s.cancel(source, frame)
	default:
		s.sendError(source, frame.ID, "bad_frame", "unknown Hub frame type")
		return nil
	}
}

func (s *HubService) call(ctx context.Context, caller *hubSession, frame hub.Frame) error {
	if err := hub.ValidateCall(frame); err != nil {
		s.sendError(caller, frame.ID, protocolCode(err, "bad_frame"), protocolMessage(err, "invalid call"))
		return nil
	}
	if frame.To == caller.installation.ID {
		s.sendError(caller, frame.ID, "self_call", "a device cannot call itself")
		return nil
	}
	if _, err := s.authorizeInstallation(ctx, caller.principal); err != nil {
		s.sendError(caller, frame.ID, "unauthenticated", "source installation is unavailable")
		s.terminate(caller, 4001, "installation revoked")
		return nil
	}
	s.mu.Lock()
	target := s.sessions[caller.ownerID][frame.To]
	callerInstallation := caller.installation
	s.mu.Unlock()
	if target == nil {
		s.sendError(caller, frame.ID, "device_offline", "target device is offline")
		return nil
	}
	if _, err := s.authorizeInstallation(ctx, target.principal); err != nil {
		s.terminate(target, 4001, "installation revoked")
		s.sendError(caller, frame.ID, "device_offline", "target device is unavailable")
		return nil
	}
	key := pendingKey(caller.ownerID, frame.ID)
	callerKey := pendingCallerKey(caller.ownerID, caller.installation.ID)
	pending := &hubPendingCall{key: key, ownerID: caller.ownerID, callerKey: callerKey, id: frame.ID, action: frame.Action, caller: caller, target: target}
	s.mu.Lock()
	if s.closed || s.sessions[caller.ownerID][caller.installation.ID] != caller ||
		s.sessions[caller.ownerID][target.installation.ID] != target {
		s.mu.Unlock()
		s.sendError(caller, frame.ID, "device_offline", "source or target device is offline")
		return nil
	}
	if _, exists := s.pending[key]; exists {
		s.mu.Unlock()
		s.sendError(caller, frame.ID, "duplicate_call", "call id is already pending")
		return nil
	}
	if len(s.pending) >= HubMaxPendingCalls || s.pendingByAccount[caller.ownerID] >= HubMaxPendingCallsPerAccount ||
		s.pendingByCaller[callerKey] >= HubMaxPendingCallsPerDevice {
		s.mu.Unlock()
		s.sendError(caller, frame.ID, "too_many_calls", "pending call limit reached")
		return nil
	}
	s.pending[key] = pending
	s.pendingByAccount[caller.ownerID]++
	s.pendingByCaller[callerKey]++
	pending.timer = time.AfterFunc(hubCallTimeout, func() { s.expireCall(pending) })
	s.mu.Unlock()

	args := frame.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	from := map[string]string{
		"id": callerInstallation.ID, "name": callerInstallation.DisplayName, "kind": callerInstallation.Platform,
	}
	if !s.send(target, map[string]any{"type": "call", "id": frame.ID, "from": from, "action": frame.Action, "args": args}) {
		s.removePending(key)
		s.sendError(caller, frame.ID, "device_offline", "target device could not receive the call")
	}
	return nil
}

func (s *HubService) result(ctx context.Context, target *hubSession, frame hub.Frame) error {
	if err := hub.ValidateResult(frame); err != nil {
		s.sendError(target, frame.ID, protocolCode(err, "bad_frame"), protocolMessage(err, "invalid result"))
		return nil
	}
	if _, err := s.authorizeInstallation(ctx, target.principal); err != nil {
		s.sendError(target, frame.ID, "unauthenticated", "target installation is unavailable")
		s.terminate(target, 4001, "installation revoked")
		return nil
	}
	key := pendingKey(target.ownerID, frame.ID)
	s.mu.Lock()
	pending := s.pending[key]
	if pending == nil || pending.target != target {
		s.mu.Unlock()
		s.sendError(target, frame.ID, "unknown_call", "no matching call is pending for this installation")
		return nil
	}
	if pending.caller != nil && s.sessions[pending.ownerID][pending.caller.installation.ID] != pending.caller {
		s.removePendingLocked(key)
		s.mu.Unlock()
		s.sendError(target, frame.ID, "unknown_call", "the requesting installation disconnected")
		return nil
	}
	if err := hub.ValidateActionResult(pending.action, frame); err != nil {
		s.removePendingLocked(key)
		caller := pending.caller
		s.mu.Unlock()
		if pending.taskResult != nil {
			s.deliverTaskResult(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "target returned an unrecognized result; outcome is unknown"})
		} else if caller != nil {
			s.sendError(caller, frame.ID, "unknown_result", "target returned an unrecognized result; outcome is unknown")
		}
		s.sendError(target, frame.ID, "bad_result", "result did not match the action contract")
		return nil
	}
	if pending.taskContext != nil && frame.OK != nil && *frame.OK {
		var result struct {
			TargetGrantVersion int64 `json:"target_grant_version"`
		}
		if json.Unmarshal(frame.Body, &result) != nil || result.TargetGrantVersion != pending.taskContext.TargetGrantVersion {
			s.removePendingLocked(key)
			s.mu.Unlock()
			s.deliverTaskResult(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "target authorization changed while the action was in flight"})
			s.sendError(target, frame.ID, "bad_result", "result did not match the requested target authorization")
			return nil
		}
	}
	s.removePendingLocked(key)
	caller := pending.caller
	s.mu.Unlock()
	if pending.taskResult != nil {
		s.deliverTaskResult(pending, HubTaskCallResult{
			OK: frame.OK != nil && *frame.OK, Body: frame.Body, Error: frame.Error, Message: safeResultMessage(frame.Message),
		})
		return nil
	}
	if caller == nil {
		s.sendError(target, frame.ID, "unknown_call", "the requesting installation is unavailable")
		return nil
	}
	if _, err := s.authorizeInstallation(ctx, caller.principal); err != nil {
		s.sendError(target, frame.ID, "unknown_call", "the requesting installation is unavailable")
		return nil
	}
	if !s.send(caller, hub.Frame{Type: "result", ID: frame.ID, OK: frame.OK, Body: frame.Body, Error: frame.Error, Message: safeResultMessage(frame.Message)}) {
		s.sendError(target, frame.ID, "unknown_call", "the requesting installation disconnected")
	}
	return nil
}

func (s *HubService) cancel(caller *hubSession, frame hub.Frame) error {
	if frame.ID == "" || !hub.ValidCallID(frame.ID) || frame.To != "" || frame.Action != "" || len(frame.Args) > 0 ||
		frame.OK != nil || len(frame.Body) > 0 || frame.Error != "" || frame.Message != "" || frame.Code != "" {
		s.sendError(caller, frame.ID, "bad_frame", "cancel requires only a valid call id")
		return nil
	}
	key := pendingKey(caller.ownerID, frame.ID)
	s.mu.Lock()
	pending := s.pending[key]
	if pending == nil || pending.caller != caller {
		s.mu.Unlock()
		s.sendError(caller, frame.ID, "unknown_call", "no matching call is pending for this installation")
		return nil
	}
	target := pending.target
	s.mu.Unlock()
	if !s.send(target, map[string]string{"type": "cancel", "id": frame.ID}) {
		s.finishUnknown(pending, "cancel could not reach target; outcome unknown")
		return nil
	}
	s.send(caller, map[string]string{"type": "cancel_pending", "id": frame.ID, "state": "requested"})
	return nil
}

func (s *HubService) expireCall(pending *hubPendingCall) {
	s.mu.Lock()
	if s.pending[pending.key] != pending {
		s.mu.Unlock()
		return
	}
	s.removePendingLocked(pending.key)
	s.mu.Unlock()
	s.send(pending.target, map[string]string{"type": "cancel", "id": pending.id})
	if pending.taskResult != nil {
		s.deliverTaskResult(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "target response timed out; outcome is unknown"})
	} else if pending.caller != nil {
		s.sendError(pending.caller, pending.id, "timeout", "target response timed out; outcome is unknown")
	}
}

func (s *HubService) finishUnknown(pending *hubPendingCall, message string) {
	s.mu.Lock()
	if s.pending[pending.key] != pending {
		s.mu.Unlock()
		return
	}
	s.removePendingLocked(pending.key)
	s.mu.Unlock()
	if pending.taskResult != nil {
		s.deliverTaskResult(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: message})
	} else if pending.caller != nil {
		s.sendError(pending.caller, pending.id, "unknown_result", message)
	}
}

func (s *HubService) completeTaskPending(pending *hubPendingCall, result HubTaskCallResult) {
	s.mu.Lock()
	if s.pending[pending.key] != pending {
		s.mu.Unlock()
		return
	}
	s.removePendingLocked(pending.key)
	s.mu.Unlock()
	s.deliverTaskResult(pending, result)
}

func (s *HubService) cancelTaskPending(pending *hubPendingCall) {
	s.mu.Lock()
	if s.pending[pending.key] != pending {
		s.mu.Unlock()
		return
	}
	s.removePendingLocked(pending.key)
	s.mu.Unlock()
	s.send(pending.target, map[string]string{"type": "cancel", "id": pending.id})
	s.deliverTaskResult(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "task Hub call was interrupted"})
}

func (s *HubService) deliverTaskResult(pending *hubPendingCall, result HubTaskCallResult) {
	if pending.taskResult == nil {
		return
	}
	select {
	case pending.taskResult <- result:
	default:
	}
}

func (s *HubService) attach(session *hubSession) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("hub service is closed")
	}
	account := s.sessions[session.ownerID]
	if account == nil {
		account = make(map[string]*hubSession)
		s.sessions[session.ownerID] = account
	}
	old := account[session.installation.ID]
	if old == nil && (s.connectionCount >= HubMaxConnections || len(account) >= HubMaxConnectionsPerAccount) {
		if len(account) == 0 {
			delete(s.sessions, session.ownerID)
		}
		s.mu.Unlock()
		return errors.New("hub connection limit reached")
	}
	var deliveries []hubDelivery
	if old != nil {
		deliveries = append(deliveries, s.dropPendingLocked(old)...)
	} else {
		s.connectionCount++
	}
	account[session.installation.ID] = session
	s.mu.Unlock()
	if old != nil {
		old.close(4003, "replaced by a newer connection")
	}
	s.deliver(deliveries)
	return nil
}

func (s *HubService) terminate(session *hubSession, code int, reason string) {
	s.terminateSession(session, code, reason, true)
}

func (s *HubService) terminateUnannounced(session *hubSession, code int, reason string) {
	s.terminateSession(session, code, reason, false)
}

func (s *HubService) terminateSession(session *hubSession, code int, reason string, broadcast bool) {
	s.mu.Lock()
	account := s.sessions[session.ownerID]
	removed := account != nil && account[session.installation.ID] == session
	var deliveries []hubDelivery
	if removed {
		delete(account, session.installation.ID)
		s.connectionCount--
		if len(account) == 0 {
			delete(s.sessions, session.ownerID)
		}
		deliveries = append(deliveries, s.dropPendingLocked(session)...)
	}
	s.mu.Unlock()
	session.close(code, reason)
	s.deliver(deliveries)
	if removed && broadcast {
		s.broadcastDevices(context.Background(), session.ownerID)
	}
}

func (s *HubService) dropPendingLocked(session *hubSession) []hubDelivery {
	var deliveries []hubDelivery
	for key, pending := range s.pending {
		if pending.caller == session {
			s.removePendingLocked(key)
			if s.sessions[pending.ownerID][pending.target.installation.ID] == pending.target {
				deliveries = append(deliveries, hubDelivery{target: pending.target, frame: map[string]string{"type": "cancel", "id": pending.id}})
			}
		} else if pending.target == session {
			s.removePendingLocked(key)
			if pending.taskResult != nil {
				deliveries = append(deliveries, hubDelivery{taskResult: pending.taskResult, taskResultBody: HubTaskCallResult{
					Unknown: true, Error: "unknown_result", Message: "target disconnected before confirming the result; outcome is unknown",
				}})
			} else if pending.caller != nil && s.sessions[pending.ownerID][pending.caller.installation.ID] == pending.caller {
				deliveries = append(deliveries, hubDelivery{target: pending.caller, frame: map[string]string{
					"type": "error", "id": pending.id, "code": "unknown_result",
					"message": "target disconnected before confirming the result; outcome is unknown",
				}})
			}
		}
	}
	return deliveries
}

func (s *HubService) deliver(deliveries []hubDelivery) {
	for _, delivery := range deliveries {
		if delivery.taskResult != nil {
			select {
			case delivery.taskResult <- delivery.taskResultBody:
			default:
			}
		} else if delivery.target != nil {
			s.send(delivery.target, delivery.frame)
		}
	}
}

func (s *HubService) removePending(key string) {
	s.mu.Lock()
	s.removePendingLocked(key)
	s.mu.Unlock()
}

func (s *HubService) removePendingLocked(key string) *hubPendingCall {
	pending := s.pending[key]
	if pending == nil {
		return nil
	}
	delete(s.pending, key)
	pending.timer.Stop()
	s.pendingByAccount[pending.ownerID]--
	if s.pendingByAccount[pending.ownerID] == 0 {
		delete(s.pendingByAccount, pending.ownerID)
	}
	callerKey := pending.callerKey
	s.pendingByCaller[callerKey]--
	if s.pendingByCaller[callerKey] == 0 {
		delete(s.pendingByCaller, callerKey)
	}
	return pending
}

func (s *HubService) listDevices(ctx context.Context, ownerID string) ([]hub.Device, error) {
	installations, err := s.store.ListClientInstallations(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list Hub installations: %w", err)
	}
	s.mu.Lock()
	account := s.sessions[ownerID]
	devices := make([]hub.Device, 0, min(len(installations), HubMaxDevicesInPresence))
	now := time.Now()
	for _, installation := range installations {
		if installation.OwnerID != ownerID || installation.RevokedAt != nil || !installation.ExpiresAt.After(now) {
			continue
		}
		if len(devices) == HubMaxDevicesInPresence {
			break
		}
		devices = append(devices, hub.Device{
			ID: installation.ID, Name: installation.DisplayName, Platform: installation.Platform,
			Online: account[installation.ID] != nil,
		})
	}
	s.mu.Unlock()
	return devices, nil
}

func (s *HubService) broadcastDevices(ctx context.Context, ownerID string) {
	devices, err := s.listDevices(ctx, ownerID)
	if err != nil {
		return
	}
	s.mu.Lock()
	account := s.sessions[ownerID]
	sessions := make([]*hubSession, 0, len(account))
	for _, session := range account {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	frame := map[string]any{"type": "devices", "devices": devices}
	for _, session := range sessions {
		s.send(session, frame)
	}
}

func (s *HubService) send(session *hubSession, frame any) bool {
	raw, err := json.Marshal(frame)
	if err != nil || len(raw) == 0 || len(raw) > hub.MaxFrameBytes {
		s.terminate(session, 1009, "outbound frame too large")
		return false
	}
	if err := session.enqueue(raw); err != nil {
		s.terminate(session, 4009, "slow consumer")
		return false
	}
	return true
}

func (s *HubService) sendError(session *hubSession, id, code, message string) {
	frame := map[string]string{"type": "error", "code": code, "message": message}
	if id != "" {
		frame["id"] = id
	}
	s.send(session, frame)
}

func emptyFrame(frame hub.Frame) bool {
	return frame.ID == "" && frame.To == "" && frame.Action == "" && len(frame.Args) == 0 && frame.OK == nil &&
		len(frame.Body) == 0 && frame.Error == "" && frame.Message == "" && frame.Code == ""
}

func (s *HubService) failSession(session *hubSession, code, message string) error {
	s.sendError(session, "", code, message)
	s.terminate(session, 1011, code)
	return nil
}

func (s *HubService) writeBeforeAttach(peer HubPeer, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return peer.Write(ctx, raw)
}

func (s *HubService) authorizeInstallation(ctx context.Context, principal identity.Principal) (identity.ClientInstallation, error) {
	if principal.Kind != identity.KindClient || principal.Role != identity.RoleUser ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) || len(principal.WorkspaceIDs) != 0 {
		return identity.ClientInstallation{}, domain.NewError(domain.CodeUnauthenticated, "client installation required")
	}
	installation, err := s.store.GetClientInstallation(ctx, principal.ClientInstallationID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return identity.ClientInstallation{}, domain.NewError(domain.CodeUnauthenticated, "client installation is unavailable")
		}
		return identity.ClientInstallation{}, domain.NewError(domain.CodeInternal, "client installation could not be checked")
	}
	if installation.ID != principal.ClientInstallationID || installation.OwnerID != principal.UserID ||
		installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return identity.ClientInstallation{}, domain.NewError(domain.CodeUnauthenticated, "client installation is unavailable")
	}
	return installation, nil
}

func protocolCode(err error, fallback string) string {
	var protocolErr hub.ProtocolError
	if errors.As(err, &protocolErr) && protocolErr.Code != "" {
		return protocolErr.Code
	}
	return fallback
}

func protocolMessage(err error, fallback string) string {
	var protocolErr hub.ProtocolError
	if errors.As(err, &protocolErr) && protocolErr.Message != "" {
		return protocolErr.Message
	}
	return fallback
}

func safeResultMessage(message string) string {
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		return ""
	}
	return "target action failed"
}

func pendingKey(ownerID, id string) string { return ownerID + "\x00" + id }

func pendingCallerKey(ownerID, installationID string) string {
	return ownerID + "\x00" + installationID
}

func (s *hubSession) enqueue(raw []byte) error {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	select {
	case <-s.done:
		return errors.New("hub session is closed")
	default:
	}
	if len(s.queue) >= HubMaxQueuedFrames || s.queuedBytes+len(raw) > HubMaxQueuedBytes {
		return errors.New("hub output queue is full")
	}
	select {
	case s.queue <- raw:
		s.queuedBytes += len(raw)
		return nil
	default:
		return errors.New("hub output queue is full")
	}
}

func (s *hubSession) allowFrame(now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateUpdated.IsZero() {
		s.rateTokens = hubFrameRateBurst
		s.rateUpdated = now
	}
	s.rateTokens = min(float64(hubFrameRateBurst), s.rateTokens+now.Sub(s.rateUpdated).Seconds()*hubFrameRatePerSecond)
	s.rateUpdated = now
	if s.rateTokens < 1 {
		return false
	}
	s.rateTokens--
	return true
}

func (s *hubSession) close(code int, reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.peer.Close(code, reason)
	})
}

func (s *HubService) writeLoop(session *hubSession) {
	for {
		select {
		case <-session.done:
			return
		default:
		}
		select {
		case <-session.done:
			return
		case raw := <-session.queue:
			select {
			case <-session.done:
				return
			default:
			}
			session.queueMu.Lock()
			session.queuedBytes -= len(raw)
			session.queueMu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := session.peer.Write(ctx, raw)
			cancel()
			if err != nil {
				s.terminate(session, 1001, "write failed")
				return
			}
		}
	}
}
