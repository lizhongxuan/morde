package postgres

import (
	"bytes"
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
)

func TestHostRuntimeIdentityRoundTripAndLegacyNulls(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	params := testCreateHostParams(t, owner.ID, capacity, nil)
	params.Host.RuntimeInstanceID = "runtime-instance"
	created, _, err := store.CreateHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.GetHost(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []host.Host{created, read} {
		if value.RuntimeGeneration != 1 {
			t.Fatalf("initial runtime generation = %d, want 1", value.RuntimeGeneration)
		}
		if !bytes.Equal(value.RuntimeCredentialEnc, params.Host.RuntimeCredentialEnc) || value.RuntimeInstanceID != params.Host.RuntimeInstanceID {
			t.Fatal("runtime identity did not round trip")
		}
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, created.RuntimeGeneration, created.ControlVersion, "runtime-instance", "ri_during_create"); err != nil {
		t.Fatalf("SetRuntimeInstanceID(during create) error = %v", err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, created.RuntimeGeneration, created.ControlVersion, "runtime-instance", "ri_stale"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale create handshake error = %v", err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, "runtime ready"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, created.RuntimeGeneration, created.ControlVersion, "ri_during_create", "ri_current"); err != nil {
		t.Fatalf("SetRuntimeInstanceID() error = %v", err)
	}
	updated, err := store.GetHost(ctx, created.ID)
	if err != nil || updated.RuntimeInstanceID != "ri_current" {
		t.Fatalf("runtime instance after handshake = %q, %v", updated.RuntimeInstanceID, err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, created.RuntimeGeneration, created.ControlVersion, "ri_during_create", "ri_stale"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("late handshake from prior runtime instance error = %v", err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, created.RuntimeGeneration+1, created.ControlVersion, "ri_current", "ri_stale"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale runtime handshake error = %v", err)
	}

	withoutInstance, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, nil))
	if err != nil {
		t.Fatal(err)
	}
	var instanceIsNull bool
	if err := pool.QueryRow(ctx, `SELECT runtime_instance_id IS NULL FROM hosts WHERE id=$1`, withoutInstance.ID).Scan(&instanceIsNull); err != nil || !instanceIsNull {
		t.Fatalf("empty runtime instance must be SQL NULL: null=%t err=%v", instanceIsNull, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_credential_enc=NULL, runtime_instance_id=NULL WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.GetHost(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.RuntimeCredentialEnc != nil || legacy.RuntimeInstanceID != "" {
		t.Fatal("legacy nullable runtime identity was not empty")
	}
}

func TestStoreRoundTrips(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)

	if got, err := store.GetUserByID(ctx, owner.ID); err != nil || got.Name != owner.Name {
		t.Fatalf("GetUserByID() = %#v, %v", got, err)
	}
	if got, err := store.GetUserByName(ctx, owner.Name); err != nil || got.ID != owner.ID {
		t.Fatalf("GetUserByName() = %#v, %v", got, err)
	}
	if users, err := store.ListUsers(ctx); err != nil || len(users) != 1 {
		t.Fatalf("ListUsers() = %d, %v", len(users), err)
	}
	owner.Name = owner.Name + "-updated"
	owner.Disabled = true
	if got, err := store.UpdateUser(ctx, "", owner); err != nil || got.Name != owner.Name || !got.Disabled {
		t.Fatalf("UpdateUser() = %#v, %v", got, err)
	}
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatalf("CheckSchema() error = %v", err)
	}
	if err := store.WithTx(ctx, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("WithTx() error = %v", err)
	}

	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "standalone")
	workspaceID := workspaceValue.ID
	if values, err := store.ListWorkspacesByOwner(ctx, owner.ID); err != nil || len(values) != 1 {
		t.Fatalf("ListWorkspacesByOwner() = %d, %v", len(values), err)
	}
	if err := store.SetWorkspaceError(ctx, workspaceID, workspace.StateReady, "retry later"); err != nil {
		t.Fatalf("SetWorkspaceError() error = %v", err)
	}
	if err := store.SetWorkspaceError(ctx, workspaceID, workspace.StateDeleting, "stale"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("SetWorkspaceError(stale) error = %v", err)
	}
	if _, err := store.ClaimWorkspaceForDelete(ctx, workspaceID, workspaceValue.Revision+1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("ClaimWorkspaceForDelete(stale revision) error = %v", err)
	}
	claim, err := store.ClaimWorkspaceForDelete(ctx, workspaceID, workspaceValue.Revision)
	if err != nil || claim.State != workspace.StateDeleting {
		t.Fatalf("ClaimWorkspaceForDelete() = %#v, %v", claim, err)
	}
	if _, err := store.CompleteWorkspaceDelete(ctx, workspaceID, claim.Revision); err != nil {
		t.Fatalf("CompleteWorkspaceDelete() error = %v", err)
	}

	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	created := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	if values, err := store.ListHosts(ctx); err != nil || len(values) != 1 {
		t.Fatalf("ListHosts() = %d, %v", len(values), err)
	}
	if values, err := store.ListHostsByOwner(ctx, owner.ID); err != nil || len(values) != 1 {
		t.Fatalf("ListHostsByOwner() = %d, %v", len(values), err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ListHosts(canceled); err == nil {
		t.Fatal("ListHosts(canceled) error = nil")
	}
	if err := store.TouchHosts(ctx, []string{created.ID}, time.Time{}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("TouchHosts(zero time) error = %v", err)
	}
	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatalf("MarkCreateAttempt() error = %v", err)
	}
	if current, changed, err := store.FailUnattemptedCreate(ctx, created.ID, "timeout"); err != nil || changed || current.State != host.StateCreating {
		t.Fatalf("FailUnattemptedCreate(attempted) = %#v, %t, %v", current, changed, err)
	}
	sandboxID := "sandbox-round-trip"
	if err := store.SetSandboxID(ctx, created.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatalf("SetSandboxID() error = %v", err)
	}
	if err := store.SetSandboxID(ctx, created.ID, host.StateRunning, &sandboxID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("SetSandboxID(stale) error = %v", err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if got, err := store.SetHostExpiry(ctx, created.ID, host.StateCreating, expiresAt); err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("SetHostExpiry() = %#v, %v", got.ExpiresAt, err)
	}
	if _, err := store.SetHostExpiry(ctx, "h1234567", host.StateCreating, expiresAt); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("SetHostExpiry(missing) error = %v", err)
	}

	unattempted := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	failed, changed, err := store.FailUnattemptedCreate(ctx, unattempted.ID, "create_timeout")
	if err != nil || !changed || failed.State != host.StateFailed {
		t.Fatalf("FailUnattemptedCreate() = %#v, %t, %v", failed, changed, err)
	}
	if _, changed, err := store.FailUnattemptedCreate(ctx, unattempted.ID, "create_timeout"); changed || domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("FailUnattemptedCreate(stale) = %t, %v", changed, err)
	}
	if _, _, err := store.FailUnattemptedCreate(ctx, "h1234567", "create_timeout"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("FailUnattemptedCreate(missing) error = %v", err)
	}

	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	token := identity.Token{
		ID: tokenID, OwnerID: owner.ID, Kind: identity.KindAPIKey, Name: "round-trip",
		SecretHash: []byte("hash"), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if _, err := store.CreateToken(ctx, token); err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	if got, err := store.GetTokenByID(ctx, tokenID); err != nil || got.Name != token.Name {
		t.Fatalf("GetTokenByID() = %#v, %v", got, err)
	}
	if tokens, err := store.ListTokensByOwner(ctx, owner.ID); err != nil || len(tokens) != 1 {
		t.Fatalf("ListTokensByOwner() = %d, %v", len(tokens), err)
	}
	if err := store.RevokeToken(ctx, tokenID, owner.ID); err != nil {
		t.Fatalf("RevokeToken() error = %v", err)
	}
	if err := store.RevokeToken(ctx, tokenID, owner.ID); err != nil {
		t.Fatalf("RevokeToken(idempotent) error = %v", err)
	}
	if err := store.RevokeToken(ctx, "t1234567", owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("RevokeToken(unknown token) error = %v", err)
	}

	entry := audit.Entry{ActorID: owner.ID, ActorKind: audit.ActorAPIKey, Action: audit.ActionMCPCall, Target: created.ID,
		Detail: map[string]any{"tool": "host_get", "ok": true}, IP: "127.0.0.1"}
	if err := store.WriteAudit(ctx, entry); err != nil {
		t.Fatalf("WriteAudit() error = %v", err)
	}
	logs, err := store.ListAuditLogs(ctx, app.AuditFilter{Actor: owner.ID, Action: string(audit.ActionMCPCall), Target: created.ID, Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].Entry.Detail["tool"] != "host_get" {
		t.Fatalf("ListAuditLogs() = %#v, %v", logs, err)
	}
	if err := store.WriteAudit(ctx, audit.Entry{ActorID: owner.ID, ActorKind: "invalid", Action: audit.ActionMCPCall}); err == nil {
		t.Fatal("WriteAudit(invalid actor) error = nil")
	}
}
