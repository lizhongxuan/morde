package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
)

type codexServiceTestStore struct {
	CodexStore
	connectCalls        int
	revokeCalls         int
	lastProfileRequest  codex.ProfileConnectionRequest
	lastRevokedProfile  string
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
	lastEventScopes     []codex.Scope
	eventPage           codex.EventPage
	auditEntries        []audit.Entry
	auditErr            error
}

type codexServiceTestCommand struct {
	hash    []byte
	command codex.Command
}

func (s *codexServiceTestStore) ConnectCodexProfile(_ context.Context, principal identity.Principal, request codex.ProfileConnectionRequest) (codex.ProfileConnection, error) {
	s.connectCalls++
	s.lastProfileRequest = request
	return codex.ProfileConnection{InstallationID: principal.ClientInstallationID, ProfileKey: request.ProfileKey,
		Scopes: append([]codex.Scope(nil), request.Scopes...), GrantVersion: 3, Connected: true}, nil
}

func (s *codexServiceTestStore) RevokeCodexProfile(_ context.Context, _ identity.Principal, profileKey string) error {
	s.revokeCalls++
	s.lastRevokedProfile = profileKey
	return nil
}

func TestCodexProfileConnectionRequiresClientAndExactReadLiveGrant(t *testing.T) {
	store := &codexServiceTestStore{}
	service, err := NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	client := codexServiceTestClientPrincipal("i1234567")
	request := codex.ProfileConnectionRequest{ProfileKey: "profile-1", Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeLive}}
	connection, err := service.Connect(context.Background(), client, request)
	if err != nil || connection.ProfileKey != request.ProfileKey || connection.InstallationID != client.ClientInstallationID ||
		connection.GrantVersion != 3 || !connection.Connected || store.connectCalls != 1 {
		t.Fatalf("connect Codex profile = %#v, err=%v, calls=%d", connection, err, store.connectCalls)
	}
	if err := service.Revoke(context.Background(), client, request.ProfileKey); err != nil ||
		store.revokeCalls != 1 || store.lastRevokedProfile != request.ProfileKey {
		t.Fatalf("revoke Codex profile err=%v, calls=%d, key=%q", err, store.revokeCalls, store.lastRevokedProfile)
	}

	for _, invalidRequest := range []codex.ProfileConnectionRequest{
		{ProfileKey: "profile-1", Scopes: []codex.Scope{codex.ScopeRead}},
		{ProfileKey: "../CODEX_HOME", Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeLive}},
	} {
		if _, err := service.Connect(context.Background(), client, invalidRequest); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Connect(%#v) error=%v, want invalid argument", invalidRequest, err)
		}
	}
	if _, err := service.Connect(context.Background(), codexServiceTestSessionPrincipal(), request); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Portal Session connected a Codex profile: %v", err)
	}
	if err := service.Revoke(context.Background(), codexServiceTestSessionPrincipal(), request.ProfileKey); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Portal Session revoked a PC Codex profile: %v", err)
	}
	if store.connectCalls != 1 || store.revokeCalls != 1 {
		t.Fatalf("invalid caller reached store: connect=%d revoke=%d", store.connectCalls, store.revokeCalls)
	}
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
	requiredScopes []codex.Scope, _ int64, _ int32) (codex.EventPage, error) {
	s.lastOwnerID, s.lastSessionID = ownerID, sessionID
	s.lastEventScopes = append([]codex.Scope(nil), requiredScopes...)
	if ownerID != s.eventSessionOwnerID {
		return codex.EventPage{}, domain.NewError(domain.CodeNotFound, "Codex session not found")
	}
	for _, requiredScope := range requiredScopes {
		if !s.grantScopes[requiredScope] {
			return codex.EventPage{}, domain.NewError(domain.CodeForbidden, "Codex event scope is not granted")
		}
	}
	return s.eventPage, nil
}

func (s *codexServiceTestStore) WriteAudit(_ context.Context, entry audit.Entry) error {
	if s.auditErr != nil {
		return s.auditErr
	}
	s.auditEntries = append(s.auditEntries, entry)
	return nil
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
	if len(store.lastEventScopes) != 1 || store.lastEventScopes[0] != codex.ScopeRead {
		t.Fatalf("history page required scopes = %v, want only read", store.lastEventScopes)
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
	store.grantScopes[codex.ScopeRead] = false
	store.grantScopes[codex.ScopeLive] = true
	if _, err := service.LiveEvents(context.Background(), principal, "l1234567", 0, 10); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("live event read without read grant error = %v, want forbidden", err)
	}
	store.grantScopes[codex.ScopeRead] = true
	if _, err := service.LiveEvents(context.Background(), principal, "l1234567", 0, 10); err != nil {
		t.Fatalf("live event read with read and live grants: %v", err)
	}
	if len(store.lastEventScopes) != 2 || store.lastEventScopes[0] != codex.ScopeRead || store.lastEventScopes[1] != codex.ScopeLive {
		t.Fatalf("live stream required scopes = %v, want read and live", store.lastEventScopes)
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

func TestCodexAnswerQuestionRequiresReadAndSendScopes(t *testing.T) {
	input := codex.CommandInput{Type: codex.CommandAnswerQuestion, QuestionRequestID: "request-1", QuestionVersion: strings.Repeat("a", 64),
		Answers: map[string][]string{" choice ": {"Option A"}}}
	for _, test := range []struct {
		name   string
		scopes map[codex.Scope]bool
		wantOK bool
	}{
		{name: "read only", scopes: map[codex.Scope]bool{codex.ScopeRead: true}},
		{name: "send only", scopes: map[codex.Scope]bool{codex.ScopeSend: true}},
		{name: "read and send", scopes: map[codex.Scope]bool{codex.ScopeRead: true, codex.ScopeSend: true}, wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &codexServiceTestStore{grantScopes: test.scopes}
			service, err := NewCodexService(store)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.CreateCommand(context.Background(), codexServiceTestSessionPrincipal(), "l1234567", "answer-question-0001", input)
			if test.wantOK && err != nil {
				t.Fatalf("answer question with read and send scopes: %v", err)
			}
			if !test.wantOK && domain.CodeOf(err) != domain.CodeForbidden {
				t.Fatalf("answer question without both scopes error = %v, want forbidden", err)
			}
		})
	}
}

func TestCodexListAndHistoryReadAuditsContainOnlyMetadata(t *testing.T) {
	if !audit.ActionCodexSessionListRead.Valid() || !audit.ActionCodexEventPageRead.Valid() || !audit.ActionCodexEventStreamRead.Valid() {
		t.Fatal("Codex read audit actions are not in the action allowlist")
	}
	for _, test := range []struct {
		name      string
		principal identity.Principal
		actorKind audit.ActorKind
	}{
		{name: "session", principal: codexServiceTestSessionPrincipal(), actorKind: audit.ActorSession},
		{name: "client", principal: codexServiceTestClientPrincipal("i1234567"), actorKind: audit.ActorClient},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &codexServiceTestStore{
				grantScopes:         map[codex.Scope]bool{codex.ScopeRead: true, codex.ScopeLive: true},
				eventSessionOwnerID: test.principal.UserID,
				eventPage: codex.EventPage{Items: []codex.Event{{Sequence: 8, Kind: codex.EventUserMessage,
					Payload: map[string]any{"text": "private-message", "path_label": "private/path"}}}},
			}
			service, err := NewCodexService(store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.List(context.Background(), test.principal, "i7654321", "l1234567", 10); err != nil {
				t.Fatalf("list Codex sessions: %v", err)
			}
			if len(store.auditEntries) != 1 {
				t.Fatalf("list audit count = %d, want 1", len(store.auditEntries))
			}
			listAudit := store.auditEntries[0]
			wantListDetailCount := 2
			if test.principal.Kind == identity.KindClient {
				wantListDetailCount++
			}
			if listAudit.ActorID != test.principal.UserID || listAudit.ActorKind != test.actorKind ||
				listAudit.Action != audit.ActionCodexSessionListRead || listAudit.Target != "i7654321" ||
				len(listAudit.Detail) != wantListDetailCount || listAudit.Detail["cursor"] != "l1234567" || listAudit.Detail["row_count"] != 0 {
				t.Fatalf("unexpected list audit metadata: %#v", listAudit)
			}
			if test.principal.Kind == identity.KindClient && listAudit.Detail["actor_installation_id"] != test.principal.ClientInstallationID {
				t.Fatalf("list audit actor installation = %v, want %q", listAudit.Detail["actor_installation_id"], test.principal.ClientInstallationID)
			}

			if _, err := service.Events(context.Background(), test.principal, "l1234567", 7, 10); err != nil {
				t.Fatalf("read Codex history page: %v", err)
			}
			if len(store.auditEntries) != 2 {
				t.Fatalf("history audit count = %d, want 2 total", len(store.auditEntries))
			}
			historyAudit := store.auditEntries[1]
			wantHistoryDetailCount := 2
			if test.principal.Kind == identity.KindClient {
				wantHistoryDetailCount++
			}
			if historyAudit.ActorID != test.principal.UserID || historyAudit.ActorKind != test.actorKind ||
				historyAudit.Action != audit.ActionCodexEventPageRead || historyAudit.Target != "l1234567" ||
				len(historyAudit.Detail) != wantHistoryDetailCount || historyAudit.Detail["after_sequence"] != int64(7) || historyAudit.Detail["row_count"] != 1 {
				t.Fatalf("unexpected history audit metadata: %#v", historyAudit)
			}
			if test.principal.Kind == identity.KindClient && historyAudit.Detail["actor_installation_id"] != test.principal.ClientInstallationID {
				t.Fatalf("history audit actor installation = %v, want %q", historyAudit.Detail["actor_installation_id"], test.principal.ClientInstallationID)
			}

			if _, err := service.LiveEvents(context.Background(), test.principal, "l1234567", 7, 10); err != nil {
				t.Fatalf("read Codex live stream poll: %v", err)
			}
			if len(store.auditEntries) != 3 {
				t.Fatalf("live stream batch audit count = %d, want 3 total", len(store.auditEntries))
			}
			streamAudit := store.auditEntries[2]
			wantStreamDetailCount := 4
			if test.principal.Kind == identity.KindClient {
				wantStreamDetailCount++
			}
			if streamAudit.ActorID != test.principal.UserID || streamAudit.ActorKind != test.actorKind ||
				streamAudit.Action != audit.ActionCodexEventStreamRead || streamAudit.Target != "l1234567" ||
				len(streamAudit.Detail) != wantStreamDetailCount || streamAudit.Detail["after_sequence"] != int64(7) ||
				streamAudit.Detail["first_sequence"] != int64(8) || streamAudit.Detail["last_sequence"] != int64(8) ||
				streamAudit.Detail["row_count"] != 1 {
				t.Fatalf("unexpected live stream audit metadata: %#v", streamAudit)
			}
			if test.principal.Kind == identity.KindClient && streamAudit.Detail["actor_installation_id"] != test.principal.ClientInstallationID {
				t.Fatalf("stream audit actor installation = %v, want %q", streamAudit.Detail["actor_installation_id"], test.principal.ClientInstallationID)
			}
			store.eventPage.Items = nil
			if _, err := service.Events(context.Background(), test.principal, "l1234567", 8, 10); err != nil {
				t.Fatalf("read empty Codex history page: %v", err)
			}
			if _, err := service.LiveEvents(context.Background(), test.principal, "l1234567", 8, 10); err != nil {
				t.Fatalf("read empty Codex live poll: %v", err)
			}
			if len(store.auditEntries) != 3 {
				t.Fatalf("empty history page wrote an audit, count=%d", len(store.auditEntries))
			}
		})
	}
}

func TestCodexReadDoesNotSucceedWhenAuditWriteFails(t *testing.T) {
	store := &codexServiceTestStore{
		grantScopes:         map[codex.Scope]bool{codex.ScopeRead: true, codex.ScopeLive: true},
		eventSessionOwnerID: "u1234567",
		eventPage:           codex.EventPage{Items: []codex.Event{{Sequence: 1, Payload: map[string]any{"text": "private"}}}},
		auditErr:            errors.New("audit unavailable"),
	}
	service, err := NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := codexServiceTestSessionPrincipal()
	if _, err := service.List(context.Background(), principal, "i1234567", "", 10); err == nil {
		t.Fatal("session list succeeded without its required audit")
	}
	if _, err := service.Events(context.Background(), principal, "l1234567", 0, 10); err == nil {
		t.Fatal("nonempty history page succeeded without its required audit")
	}
	if _, err := service.LiveEvents(context.Background(), principal, "l1234567", 0, 10); err == nil {
		t.Fatal("nonempty live batch succeeded without its required audit")
	}
	if len(store.auditEntries) != 0 {
		t.Fatalf("failed audit writes were recorded in the test store: %d", len(store.auditEntries))
	}
}

func codexServiceTestSessionPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func codexServiceTestClientPrincipal(installationID string) identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: installationID}
}
