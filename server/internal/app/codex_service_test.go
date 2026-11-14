package app

import (
	"bytes"
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
)

type codexServiceTestStore struct {
	CodexStore
	listCalls           int
	createCalls         int
	grantScopes         map[codex.Scope]bool
	commands            map[string]codexServiceTestCommand
	lastOwnerID         string
	lastInstallationID  string
	lastSessionID       string
	lastIdempotencyKey  string
	lastRequestHash     []byte
	eventSessionOwnerID string
	pcSessionInstallID  string
}

type codexServiceTestCommand struct {
	hash    []byte
	command codex.Command
}

func (s *codexServiceTestStore) ListCodexSessions(_ context.Context, ownerID, installationID, _ string, _ int32) (codex.SessionPage, error) {
	s.listCalls++
	s.lastOwnerID, s.lastInstallationID = ownerID, installationID
	if !s.grantScopes[codex.ScopeRead] {
		return codex.SessionPage{}, domain.NewError(domain.CodeForbidden, "Codex read scope is not granted")
	}
	return codex.SessionPage{Items: []codex.Session{}}, nil
}

func (s *codexServiceTestStore) GetCodexEvents(_ context.Context, ownerID, sessionID string,
	requiredScope codex.Scope, _ int64, _ int32) (codex.EventPage, error) {
	s.lastOwnerID, s.lastSessionID = ownerID, sessionID
	if ownerID != s.eventSessionOwnerID {
		return codex.EventPage{}, domain.NewError(domain.CodeNotFound, "Codex session not found")
	}
	if !s.grantScopes[requiredScope] {
		return codex.EventPage{}, domain.NewError(domain.CodeForbidden, "Codex event scope is not granted")
	}
	return codex.EventPage{Items: []codex.Event{}}, nil
}

func (s *codexServiceTestStore) CreateCodexCommand(_ context.Context, ownerID, installationID, sessionID, key string,
	input codex.CommandInput, requestHash []byte) (codex.Command, error) {
	s.createCalls++
	s.lastOwnerID, s.lastInstallationID, s.lastSessionID = ownerID, installationID, sessionID
	s.lastIdempotencyKey, s.lastRequestHash = key, append([]byte(nil), requestHash...)
	if !s.grantScopes[codex.ScopeRead] || !s.grantScopes[input.RequiredScope()] {
		return codex.Command{}, domain.NewError(domain.CodeForbidden, "Codex command scope is not granted")
	}
	if s.commands == nil {
		s.commands = make(map[string]codexServiceTestCommand)
	}
	identityKey := ownerID + "\x00" + sessionID + "\x00" + key
	if previous, exists := s.commands[identityKey]; exists {
		if !bytes.Equal(previous.hash, requestHash) {
			return codex.Command{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Codex command")
		}
		return previous.command, nil
	}
	targetInstallationID := s.pcSessionInstallID
	if targetInstallationID == "" {
		targetInstallationID = installationID
	}
	command := codex.Command{ID: "g1234567", SessionID: sessionID, InstallationID: targetInstallationID,
		Type: input.Type, Status: codex.CommandWaitingPC, Input: input}
	s.commands[identityKey] = codexServiceTestCommand{hash: append([]byte(nil), requestHash...), command: command}
	return command, nil
}

func TestCodexCreateCommandUsesIdempotencyFingerprint(t *testing.T) {
	store := &codexServiceTestStore{grantScopes: map[codex.Scope]bool{codex.ScopeRead: true, codex.ScopeSend: true}}
	service, err := NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := codexServiceTestSessionPrincipal()
	input := codex.CommandInput{Type: codex.CommandSend, Text: "continue"}
	first, err := service.CreateCommand(context.Background(), principal, "l1234567", "send-key-0001", input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.CreateCommand(context.Background(), principal, "l1234567", "send-key-0001", input)
	if err != nil || replay.ID != first.ID || store.createCalls != 2 || len(store.lastRequestHash) != 32 {
		t.Fatalf("idempotent replay = %#v, first=%#v, calls=%d, hash bytes=%d, err=%v",
			replay, first, store.createCalls, len(store.lastRequestHash), err)
	}

	changed := input
	changed.Text = "run another command"
	if _, err := service.CreateCommand(context.Background(), principal, "l1234567", "send-key-0001", changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed request using same key error = %v, want idempotency conflict", err)
	}
	if store.lastOwnerID != principal.UserID || store.lastInstallationID != "" ||
		store.lastSessionID != "l1234567" || store.lastIdempotencyKey != "send-key-0001" {
		t.Fatalf("command identity forwarded incorrectly: owner=%q installation=%q session=%q key=%q",
			store.lastOwnerID, store.lastInstallationID, store.lastSessionID, store.lastIdempotencyKey)
	}

	if _, err := service.CreateCommand(context.Background(), principal, "l1234567", "short", input); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("short idempotency key error = %v, want invalid argument", err)
	}
	if store.createCalls != 3 {
		t.Fatalf("invalid idempotency key reached store: calls=%d, want 3", store.createCalls)
	}
}

func TestCodexClientReadsOwnerPCSessionAcrossInstallationsWithSessionGrant(t *testing.T) {
	store := &codexServiceTestStore{
		grantScopes: map[codex.Scope]bool{codex.ScopeRead: true}, eventSessionOwnerID: "u1234567",
	}
	service, err := NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := codexServiceTestClientPrincipal("i1234567")
	if _, err := service.List(context.Background(), principal, "i7654321", "", 10); err != nil {
		t.Fatalf("owner read using another bound PC installation filter = %v", err)
	}
	if store.listCalls != 1 || store.lastOwnerID != principal.UserID || store.lastInstallationID != "i7654321" {
		t.Fatalf("PC installation filter was not applied under reader owner: calls=%d owner=%q install=%q",
			store.listCalls, store.lastOwnerID, store.lastInstallationID)
	}

	if _, err := service.Events(context.Background(), principal, "l1234567", 0, 10); err != nil {
		t.Fatalf("owner read of PC session from iOS client error = %v", err)
	}
	if store.lastOwnerID != principal.UserID || store.lastSessionID != "l1234567" {
		t.Fatalf("event read was not bound to authenticated owner/session: owner=%q session=%q",
			store.lastOwnerID, store.lastSessionID)
	}
	foreignOwner := codexServiceTestClientPrincipal("i1234567")
	foreignOwner.UserID = "u7654321"
	foreignOwner.TokenID = "t7654321"
	if _, err := service.Events(context.Background(), foreignOwner, "l1234567", 0, 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("different owner read error = %v, want not found", err)
	}
	store.grantScopes[codex.ScopeRead] = false
	if _, err := service.List(context.Background(), principal, "i7654321", "", 10); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("list without read grant error = %v, want forbidden", err)
	}
	if _, err := service.Events(context.Background(), principal, "l1234567", 0, 10); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("event read without read grant error = %v, want forbidden", err)
	}
	store.grantScopes[codex.ScopeRead] = true

	input := codex.CommandInput{Type: codex.CommandSend, Text: "send"}
	if _, err := service.CreateCommand(context.Background(), principal, "l1234567", "send-key-0001", input); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("command without send grant error = %v, want forbidden", err)
	}
	if store.lastInstallationID != principal.ClientInstallationID || store.createCalls != 1 {
		t.Fatalf("scope check did not use the authenticated installation: installation=%q calls=%d",
			store.lastInstallationID, store.createCalls)
	}
	if _, err := service.LiveEvents(context.Background(), principal, "l1234567", 0, 10); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("live event read without live grant error = %v, want forbidden", err)
	}
}

func TestCodexIOSClientCanQueueCommandForPCSession(t *testing.T) {
	store := &codexServiceTestStore{
		grantScopes:        map[codex.Scope]bool{codex.ScopeRead: true, codex.ScopeSend: true},
		pcSessionInstallID: "i7654321",
	}
	service, err := NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	iosPrincipal := codexServiceTestClientPrincipal("i1234567")
	command, err := service.CreateCommand(context.Background(), iosPrincipal, "l1234567", "ios-send-0001",
		codex.CommandInput{Type: codex.CommandSend, Text: "continue on the PC"})
	if err != nil {
		t.Fatalf("iOS Client command for owner PC session: %v", err)
	}
	if store.lastOwnerID != iosPrincipal.UserID || store.lastInstallationID != iosPrincipal.ClientInstallationID ||
		command.InstallationID != "i7654321" || store.createCalls != 1 {
		t.Fatalf("cross-install command binding: owner=%q actor install=%q target install=%q calls=%d",
			store.lastOwnerID, store.lastInstallationID, command.InstallationID, store.createCalls)
	}
}

func codexServiceTestSessionPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func codexServiceTestClientPrincipal(installationID string) identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: installationID}
}
