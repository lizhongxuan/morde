package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
)

func TestCodexCommandStatusHistoryAndGrantVersionInvalidation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	token, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, installationID, identity.KindClient, nil))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: token.ID, ClientInstallationID: installationID}
	thread := codex.Discover{ProfileKey: "profile-1", ThreadID: "thread-1", Title: "Thread", CWDLabel: "project",
		Status: codex.SessionIdle, Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeSend}}
	session, err := store.DiscoverCodexSession(ctx, principal, thread)
	if err != nil {
		t.Fatal(err)
	}
	createCommand := func(key string) codex.Command {
		t.Helper()
		input := codex.CommandInput{Type: codex.CommandSend, Text: "continue"}
		hash := sha256.Sum256([]byte(key))
		command, err := store.CreateCodexCommand(ctx, owner.ID, "", session.ID, key, input, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return command
	}

	first := createCommand("codex-command-1")
	claimed, err := store.ClaimCodexCommand(ctx, owner.ID, installationID)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	for _, status := range []codex.CommandStatus{
		codex.CommandForwarded, codex.CommandRunning, codex.CommandWaitingApproval,
		codex.CommandRunning, codex.CommandWaitingApproval,
	} {
		if _, err := store.ApplyCodexCommandResult(ctx, owner.ID, installationID, first.ID,
			codex.CommandResult{Status: status}); err != nil {
			t.Fatalf("record command status %q: %v", status, err)
		}
	}
	second := createCommand("codex-command-2")

	thread.Scopes = []codex.Scope{codex.ScopeRead, codex.ScopeInterrupt}
	if _, err := store.DiscoverCodexSession(ctx, principal, thread); err != nil {
		t.Fatalf("change grant scopes: %v", err)
	}

	var firstStatus, secondStatus string
	var firstVersion, secondVersion int64
	if err := pool.QueryRow(ctx, `SELECT status,status_version FROM codex_commands WHERE id=$1`, first.ID).
		Scan(&firstStatus, &firstVersion); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,status_version FROM codex_commands WHERE id=$1`, second.ID).
		Scan(&secondStatus, &secondVersion); err != nil {
		t.Fatal(err)
	}
	if firstStatus != string(codex.CommandUnknown) || firstVersion != 8 {
		t.Fatalf("claimed command after grant change = %s/%d, want unknown/8", firstStatus, firstVersion)
	}
	if secondStatus != string(codex.CommandFailed) || secondVersion != 2 {
		t.Fatalf("unclaimed command after grant change = %s/%d, want failed/2", secondStatus, secondVersion)
	}

	thread.Scopes = []codex.Scope{codex.ScopeRead, codex.ScopeSend}
	if _, err := store.DiscoverCodexSession(ctx, principal, thread); err != nil {
		t.Fatalf("restore send grant: %v", err)
	}
	third := createCommand("codex-command-3")
	claimed, err = store.ClaimCodexCommand(ctx, owner.ID, installationID)
	if err != nil || len(claimed) != 1 || claimed[0].ID != third.ID {
		t.Fatalf("claim third command = %#v, err = %v", claimed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_commands SET claimed_at=statement_timestamp()-interval '61 seconds' WHERE id=$1`, third.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimCodexCommand(ctx, owner.ID, installationID)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("stale claimed command was reassigned = %#v, err = %v", claimed, err)
	}
	var thirdStatus string
	var thirdVersion int64
	if err := pool.QueryRow(ctx, `SELECT status,status_version FROM codex_commands WHERE id=$1`, third.ID).
		Scan(&thirdStatus, &thirdVersion); err != nil {
		t.Fatal(err)
	}
	if thirdStatus != string(codex.CommandUnknown) || thirdVersion != 3 {
		t.Fatalf("stale claim = %s/%d, want unknown/3", thirdStatus, thirdVersion)
	}

	page, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		first.ID:  {"waiting_pc", "claimed", "forwarded", "running", "waiting_approval", "running", "waiting_approval", "unknown"},
		second.ID: {"waiting_pc", "failed"},
		third.ID:  {"waiting_pc", "claimed", "unknown"},
	}
	seen := map[string][]string{}
	ids := map[string]bool{}
	for _, event := range page.Items {
		if event.Kind != codex.EventCommandStatus {
			continue
		}
		commandID, _ := event.Payload["command_id"].(string)
		status, _ := event.Payload["status"].(string)
		seen[commandID] = append(seen[commandID], status)
		if ids[event.ProviderEventID] {
			t.Fatalf("duplicate command status event ID %q", event.ProviderEventID)
		}
		ids[event.ProviderEventID] = true
	}
	for commandID, statuses := range want {
		got := seen[commandID]
		if len(got) != len(statuses) {
			t.Fatalf("command %s status events = %#v, want %#v", commandID, got, statuses)
		}
		for index, status := range statuses {
			if got[index] != status {
				t.Fatalf("command %s status events = %#v, want %#v", commandID, got, statuses)
			}
		}
	}
}

func TestCodexAnswerQuestionCommandPersistsClaimsAndCompletesWithSendScope(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	token, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, installationID, identity.KindClient, nil))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: token.ID, ClientInstallationID: installationID}
	thread := codex.Discover{ProfileKey: "profile-1", ThreadID: "thread-1", Title: "Thread", CWDLabel: "project",
		Status: codex.SessionIdle, Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeSend}}
	session, err := store.DiscoverCodexSession(ctx, principal, thread)
	if err != nil {
		t.Fatal(err)
	}
	input := codex.CommandInput{Type: codex.CommandAnswerQuestion, QuestionRequestID: "request-1", QuestionVersion: strings.Repeat("a", 64),
		Answers: map[string][]string{" choice ": {"Option A", "Freeform answer"}}}
	hash := sha256.Sum256([]byte("answer-question-request"))
	command, err := store.CreateCodexCommand(ctx, owner.ID, "", session.ID, "answer-question-0001", input, hash[:])
	if err != nil {
		t.Fatalf("create answer question command: %v", err)
	}
	if command.Type != codex.CommandAnswerQuestion || command.Input.Answers[" choice "][1] != "Freeform answer" {
		t.Fatalf("created command did not preserve answer payload: %#v", command)
	}
	claimed, err := store.ClaimCodexCommand(ctx, owner.ID, installationID)
	if err != nil || len(claimed) != 1 || claimed[0].ID != command.ID {
		t.Fatalf("claim answer question command = %#v, err=%v", claimed, err)
	}
	if claimed[0].Type != codex.CommandAnswerQuestion || claimed[0].Input.QuestionRequestID != "request-1" ||
		claimed[0].Input.QuestionVersion != strings.Repeat("a", 64) || claimed[0].Input.Answers[" choice "][0] != "Option A" {
		t.Fatalf("claimed answer question payload changed: %#v", claimed[0])
	}
	completed, err := store.ApplyCodexCommandResult(ctx, owner.ID, installationID, command.ID,
		codex.CommandResult{Status: codex.CommandCompleted})
	if err != nil {
		t.Fatalf("complete answer question command: %v", err)
	}
	if completed.Type != codex.CommandAnswerQuestion || completed.Status != codex.CommandCompleted {
		t.Fatalf("completed answer question command = %#v", completed)
	}
}

func TestCodexEventPagesRequireEveryRequestedGrantScope(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	token, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, installationID, identity.KindClient, nil))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: token.ID, ClientInstallationID: installationID}
	thread := codex.Discover{ProfileKey: "profile-1", ThreadID: "thread-1", Title: "Thread", CWDLabel: "project:notes",
		Status: codex.SessionIdle, Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeLive}}
	session, err := store.DiscoverCodexSession(ctx, principal, thread)
	if err != nil {
		t.Fatal(err)
	}
	event := codex.EventInput{ProviderEventID: "live-event-1", Kind: codex.EventUserMessage,
		Source: codex.EventSourceLive, OccurredAt: time.Now().UTC(), Payload: map[string]any{"text": "hello"}}
	if _, err := store.AppendCodexEvents(ctx, owner.ID, installationID, session.ID, codex.EventBatch{Events: []codex.EventInput{event}}); err != nil {
		t.Fatalf("append live event: %v", err)
	}

	readPage, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead}, 0, 100)
	if err != nil || len(readPage.Items) != 1 {
		t.Fatalf("read page with read grant = %d events, err=%v", len(readPage.Items), err)
	}
	livePage, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead, codex.ScopeLive}, 0, 100)
	if err != nil || len(livePage.Items) != 1 {
		t.Fatalf("live page with read and live grants = %d events, err=%v", len(livePage.Items), err)
	}

	thread.Scopes = []codex.Scope{codex.ScopeRead}
	if _, err := store.DiscoverCodexSession(ctx, principal, thread); err != nil {
		t.Fatalf("remove live grant: %v", err)
	}
	if _, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead, codex.ScopeLive}, 0, 100); err == nil {
		t.Fatal("live stream was readable with only the read grant")
	}

	thread.Scopes = []codex.Scope{codex.ScopeLive}
	if _, err := store.DiscoverCodexSession(ctx, principal, thread); err != nil {
		t.Fatalf("remove read grant: %v", err)
	}
	if _, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead}, 0, 100); err == nil {
		t.Fatal("history page was readable without the read grant")
	}
	if _, err := store.GetCodexEvents(ctx, owner.ID, session.ID, []codex.Scope{codex.ScopeRead, codex.ScopeLive}, 0, 100); err == nil {
		t.Fatal("live stream was readable with only the live grant")
	}
}

func TestCodexCrossPlatformReadAuditsPersistOnlyMetadata(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	targetInstallationID := createTestClientInstallation(t, pool, owner.ID, nil)
	actorInstallationID := createTestClientInstallation(t, pool, owner.ID, nil)
	if _, err := pool.Exec(ctx, `UPDATE client_installations SET platform='ios' WHERE id=$1`, actorInstallationID); err != nil {
		t.Fatal(err)
	}
	targetToken, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, targetInstallationID, identity.KindClient, nil))
	if err != nil {
		t.Fatal(err)
	}
	actorToken, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, actorInstallationID, identity.KindClient, nil))
	if err != nil {
		t.Fatal(err)
	}
	targetPrincipal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: targetToken.ID, ClientInstallationID: targetInstallationID}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: actorToken.ID, ClientInstallationID: actorInstallationID}
	service, err := app.NewCodexService(store)
	if err != nil {
		t.Fatal(err)
	}
	thread := codex.Discover{ProfileKey: "profile-1", ThreadID: "thread-1", Title: "Thread", CWDLabel: "project",
		Status: codex.SessionIdle, Scopes: []codex.Scope{codex.ScopeRead, codex.ScopeLive}}
	session, err := service.Discover(ctx, targetPrincipal, thread)
	if err != nil {
		t.Fatal(err)
	}
	if page, err := service.List(ctx, principal, targetInstallationID, "", 10); err != nil || len(page.Items) != 1 {
		t.Fatalf("list Codex sessions = %d, err=%v", len(page.Items), err)
	}
	var actorID, actorKind, action, target string
	var detailJSON []byte
	if err := pool.QueryRow(ctx, `SELECT actor_id,actor_kind,action,target,detail FROM audit_logs
WHERE action=$1 ORDER BY id DESC LIMIT 1`, string(audit.ActionCodexSessionListRead)).Scan(
		&actorID, &actorKind, &action, &target, &detailJSON); err != nil {
		t.Fatalf("read Codex session list audit: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatalf("decode Codex session list audit detail: %v", err)
	}
	if actorID != owner.ID || actorKind != string(audit.ActorClient) || action != string(audit.ActionCodexSessionListRead) ||
		target != targetInstallationID || len(detail) != 3 || detail["actor_installation_id"] != actorInstallationID ||
		detail["cursor"] != "" || detail["row_count"] != float64(1) {
		t.Fatalf("unexpected Codex session list audit: actor=%s/%s action=%s target=%s detail=%v",
			actorID, actorKind, action, target, detail)
	}

	privateContent, privatePath := "private-file-change-summary", "private/path"
	event := codex.EventInput{ProviderEventID: "snapshot-event-1", Kind: codex.EventFileChange,
		Source: codex.EventSourceSnapshot, OccurredAt: time.Now().UTC(), Payload: map[string]any{
			"path_label": privatePath, "change": "updated", "summary": privateContent,
		}}
	if _, err := store.AppendCodexEvents(ctx, owner.ID, targetInstallationID, session.ID, codex.EventBatch{Events: []codex.EventInput{event}}); err != nil {
		t.Fatalf("append snapshot event: %v", err)
	}
	if page, err := service.Events(ctx, principal, session.ID, 0, 10); err != nil || len(page.Items) != 1 {
		t.Fatalf("read Codex event history = %d, err=%v", len(page.Items), err)
	}
	if err := pool.QueryRow(ctx, `SELECT actor_id,actor_kind,action,target,detail FROM audit_logs
WHERE action=$1 ORDER BY id DESC LIMIT 1`, string(audit.ActionCodexEventPageRead)).Scan(
		&actorID, &actorKind, &action, &target, &detailJSON); err != nil {
		t.Fatalf("read Codex event page audit: %v", err)
	}
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatalf("decode Codex event page audit detail: %v", err)
	}
	if actorID != owner.ID || actorKind != string(audit.ActorClient) || action != string(audit.ActionCodexEventPageRead) ||
		target != session.ID || len(detail) != 3 || detail["actor_installation_id"] != actorInstallationID ||
		detail["after_sequence"] != float64(0) || detail["row_count"] != float64(1) ||
		strings.Contains(string(detailJSON), privateContent) || strings.Contains(string(detailJSON), privatePath) {
		t.Fatalf("unexpected or sensitive Codex event page audit: actor=%s/%s action=%s target=%s detail=%s",
			actorID, actorKind, action, target, detailJSON)
	}

	if page, err := service.LiveEvents(ctx, principal, session.ID, 0, 10); err != nil || len(page.Items) != 1 {
		t.Fatalf("read Codex event stream batch = %d, err=%v", len(page.Items), err)
	}
	if err := pool.QueryRow(ctx, `SELECT actor_id,actor_kind,action,target,detail FROM audit_logs
WHERE action=$1 ORDER BY id DESC LIMIT 1`, string(audit.ActionCodexEventStreamRead)).Scan(
		&actorID, &actorKind, &action, &target, &detailJSON); err != nil {
		t.Fatalf("read Codex event stream audit: %v", err)
	}
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatalf("decode Codex event stream audit detail: %v", err)
	}
	if actorID != owner.ID || actorKind != string(audit.ActorClient) || action != string(audit.ActionCodexEventStreamRead) ||
		target != session.ID || len(detail) != 5 || detail["actor_installation_id"] != actorInstallationID || detail["after_sequence"] != float64(0) ||
		detail["first_sequence"] != float64(1) || detail["last_sequence"] != float64(1) || detail["row_count"] != float64(1) ||
		strings.Contains(string(detailJSON), privateContent) || strings.Contains(string(detailJSON), privatePath) {
		t.Fatalf("unexpected or sensitive Codex event stream audit: actor=%s/%s action=%s target=%s detail=%s",
			actorID, actorKind, action, target, detailJSON)
	}

	if _, err := service.Events(ctx, principal, session.ID, 1, 10); err != nil {
		t.Fatalf("read empty history page: %v", err)
	}
	if _, err := service.LiveEvents(ctx, principal, session.ID, 1, 10); err != nil {
		t.Fatalf("read empty live poll: %v", err)
	}
	var eventReadAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action=$2 AND target=$3`,
		owner.ID, string(audit.ActionCodexEventPageRead), session.ID).Scan(&eventReadAuditCount); err != nil {
		t.Fatalf("count Codex event page audits: %v", err)
	}
	if eventReadAuditCount != 1 {
		t.Fatalf("event page audit count = %d, want only the nonempty history page", eventReadAuditCount)
	}
	var streamAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action=$2 AND target=$3`,
		owner.ID, string(audit.ActionCodexEventStreamRead), session.ID).Scan(&streamAuditCount); err != nil {
		t.Fatalf("count Codex event stream audits: %v", err)
	}
	if streamAuditCount != 1 {
		t.Fatalf("event stream audit count = %d, want only the nonempty event batch", streamAuditCount)
	}
}
