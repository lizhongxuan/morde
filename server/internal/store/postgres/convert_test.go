package postgres

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDomainConversions(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	deletedAt := pgtype.Timestamptz{Time: now, Valid: true}
	gotHost := hostDomain(sqlcgen.Host{
		ID: "h1234567", OwnerID: "u1234567", Name: "host", TemplateID: "std-2c4g",
		RuntimeProfile: "container",
		SandboxID:      pgtype.Text{String: "sandbox", Valid: true}, CreateAttemptedAt: deletedAt,
		State: "running", RuntimeGeneration: 3, ControlVersion: 2, ControlState: "user",
		CpuMilli: 500, MemMib: 1024,
		CpuLimitMilli: 1000, MemLimitMib: 2048,
		RuntimeCredentialEnc: []byte("runtime ciphertext"), RuntimeInstanceID: pgtype.Text{String: "runtime-instance", Valid: true},
		ExpiresAt: deletedAt, LastActiveAt: deletedAt, CreatedAt: deletedAt,
		UpdatedAt: deletedAt, DeletedAt: deletedAt,
	})
	if gotHost.State != "running" || gotHost.RuntimeGeneration != 3 || gotHost.ControlVersion != 2 || gotHost.ControlState != host.ControlUser || gotHost.SandboxID != "sandbox" ||
		gotHost.CreateAttemptedAt == nil || len(gotHost.Workspaces) != 0 ||
		gotHost.MemLimitMiB != 2048 || gotHost.DeletedAt == nil {
		t.Fatalf("host conversion lost fields: %+v", gotHost)
	}
	if !bytes.Equal(gotHost.RuntimeCredentialEnc, []byte("runtime ciphertext")) || gotHost.RuntimeInstanceID != "runtime-instance" {
		t.Fatal("host conversion lost runtime identity")
	}
	if legacy := hostDomain(sqlcgen.Host{}); legacy.RuntimeCredentialEnc != nil || legacy.RuntimeInstanceID != "" {
		t.Fatal("nullable runtime identity was not empty")
	}

	gotWorkspace := workspaceDomain(sqlcgen.Workspace{
		ID: "w1234567", OwnerID: "u1234567", Name: "workspace", State: "ready",
		Revision: 4, UsedBytes: 500, QuotaBytes: 2048,
		CreatedAt: deletedAt, UpdatedAt: deletedAt, DeletedAt: deletedAt,
	})
	if gotWorkspace.Name != "workspace" || gotWorkspace.State != workspace.StateReady || gotWorkspace.Revision != 4 ||
		gotWorkspace.UsedBytes != 500 || gotWorkspace.QuotaBytes != 2048 || gotWorkspace.DeletedAt == nil {
		t.Fatalf("workspace conversion lost fields: %+v", gotWorkspace)
	}

	gotBinding := workspaceBindingDomain(sqlcgen.GetWorkspaceBindingRow{
		BindingID: "b123", OwnerID: "u1234567", HostID: "h1234567", WorkspaceID: "w1234567",
		DirectoryName: "w1234567", BindingState: "active", BindingVersion: 2,
		DesiredRevision: 4, AppliedRevision: 3, SyncState: "conflict", SyncConflicts: []string{"main.go"},
		BindingCreatedAt: deletedAt, BindingUpdatedAt: deletedAt,
		WorkspaceName: "workspace", WorkspaceState: "ready", WorkspaceRevision: 4,
		WorkspaceUsedBytes: 500, WorkspaceQuotaBytes: 2048, WorkspaceLastError: "",
		WorkspaceCreatedAt: deletedAt, WorkspaceUpdatedAt: deletedAt,
	})
	if gotBinding.State != workspace.BindingActive || gotBinding.BindingVersion != 2 || gotBinding.Workspace.ID != "w1234567" ||
		gotBinding.DesiredRevision != 4 || gotBinding.AppliedRevision != 3 || gotBinding.SyncState != workspace.BindingSyncConflict ||
		len(gotBinding.SyncConflicts) != 1 || gotBinding.SyncConflicts[0] != "main.go" {
		t.Fatalf("workspace binding conversion lost fields: %+v", gotBinding)
	}

	gotUser := userDomain(sqlcgen.User{
		ID: "u1234567", Name: "operator", PasswordHash: "hash", Role: "admin",
		QuotaHosts: 3, QuotaCpuMilli: 4000, QuotaMemMib: 8192, Disabled: true,
		CreatedAt: deletedAt, UpdatedAt: deletedAt,
	})
	if gotUser.Role != "admin" || gotUser.QuotaCPUMilli != 4000 || !gotUser.Disabled {
		t.Fatalf("user conversion lost fields: %+v", gotUser)
	}

	gotToken := tokenDomain(sqlcgen.Token{
		ID: "t1234567", OwnerID: "u1234567", Kind: "api_key",
		HostID: pgtype.Text{String: "h1234567", Valid: true}, SecretHash: []byte("hash"),
		ExpiresAt: deletedAt, UsedAt: deletedAt, LastUsedAt: deletedAt,
		RevokedAt: deletedAt, CreatedAt: deletedAt,
	})
	if gotToken.Kind != "api_key" || gotToken.HostID != "h1234567" || gotToken.UsedAt == nil || gotToken.RevokedAt == nil {
		t.Fatalf("token conversion lost fields: %+v", gotToken)
	}
	if got := tokenDomain(sqlcgen.Token{}); got.HostID != "" || got.UsedAt != nil || got.LastUsedAt != nil || got.RevokedAt != nil {
		t.Fatalf("nullable token fields were not empty: %+v", got)
	}

	gotAudit, err := auditDomain(sqlcgen.AuditLog{
		ID: 42, At: deletedAt, ActorID: "u1234567", ActorKind: "session",
		Action: string(audit.ActionHostCreate), Target: "h1234567", Detail: []byte(`{"ok":true}`), Ip: "127.0.0.1",
	})
	if err != nil || gotAudit.ID != 42 || gotAudit.At != now || gotAudit.Entry.Detail["ok"] != true {
		t.Fatalf("audit conversion got %+v, err %v", gotAudit, err)
	}
	if got, err := auditDomain(sqlcgen.AuditLog{Detail: []byte(`{}`)}); err != nil || got.Entry.Detail == nil {
		t.Fatalf("empty audit detail must be an object: %+v, err %v", got, err)
	}
	if _, err := auditDomain(sqlcgen.AuditLog{Detail: []byte(`not-json`)}); err == nil {
		t.Fatal("invalid audit detail must be rejected")
	}
	params, err := auditParams(audit.Entry{Action: audit.ActionHostCreate})
	if err != nil || string(params.Detail) != `{}` {
		t.Fatalf("empty audit detail must be encoded as object: %s, err %v", params.Detail, err)
	}
	if _, err := auditParams(audit.Entry{Detail: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Fatal("unserializable audit detail must be rejected")
	}

	if got := mapRows([]int(nil), func(value int) int { return value }); got == nil {
		t.Fatal("empty query results must convert to a non-nil slice")
	}
	if _, err := mapRowsWithError([]int{1}, func(int) (int, error) { return 0, domain.NewError(domain.CodeInternal, "conversion failed") }); err == nil {
		t.Fatal("conversion failures must propagate")
	}
}

func TestStoreError(t *testing.T) {
	if got := storeError("get host", "host", pgx.ErrNoRows); domain.CodeOf(got) != domain.CodeNotFound {
		t.Fatalf("no rows mapped to %q, want not_found", domain.CodeOf(got))
	}
	if got := storeError("get host", "host", errors.New("connection reset")); got == nil || domain.CodeOf(got) != domain.CodeInternal {
		t.Fatalf("database failure not wrapped: %v", got)
	}
}
