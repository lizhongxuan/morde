package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

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

	page, err := store.GetCodexEvents(ctx, owner.ID, session.ID, codex.ScopeRead, 0, 100)
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
