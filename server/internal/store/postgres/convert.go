package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5/pgtype"
)

func hostDomain(row sqlcgen.Host) host.Host {
	return host.Host{
		ID: row.ID, OwnerID: row.OwnerID, Name: row.Name, TemplateID: row.TemplateID,
		RuntimeProfile: row.RuntimeProfile, Workspaces: make([]workspace.Binding, 0),
		SandboxID: row.SandboxID.String, CreateAttemptedAt: timePointer(row.CreateAttemptedAt),
		State: host.State(row.State), StateReason: row.StateReason,
		RuntimeGeneration: row.RuntimeGeneration, ControlVersion: row.ControlVersion,
		ControlState: host.ControlState(row.ControlState),
		CPUMilli:     int(row.CpuMilli),
		MemMiB:       int(row.MemMib), CPULimitMilli: int(row.CpuLimitMilli),
		MemLimitMiB: int(row.MemLimitMib), AIOKeyEnc: row.AioKeyEnc,
		RuntimeCredentialEnc: row.RuntimeCredentialEnc, RuntimeInstanceID: row.RuntimeInstanceID.String,
		ExpiresAt:    timePointer(row.ExpiresAt),
		LastActiveAt: timePointer(row.LastActiveAt), CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time, DeletedAt: timePointer(row.DeletedAt),
	}
}

func workspaceDomain(row sqlcgen.Workspace) workspace.Workspace {
	return workspace.Workspace{
		ID: row.ID, OwnerID: row.OwnerID, Name: row.Name, State: workspace.State(row.State),
		Revision: row.Revision, UsedBytes: row.UsedBytes, QuotaBytes: row.QuotaBytes, LastError: row.LastError,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, DeletedAt: timePointer(row.DeletedAt),
	}
}

func workspaceBindingDomain(row sqlcgen.GetWorkspaceBindingRow) workspace.Binding {
	return workspace.Binding{
		ID: row.BindingID, OwnerID: row.OwnerID, HostID: row.HostID,
		Workspace: workspace.Workspace{
			ID: row.WorkspaceID, OwnerID: row.OwnerID, Name: row.WorkspaceName,
			State: workspace.State(row.WorkspaceState), Revision: row.WorkspaceRevision,
			UsedBytes: row.WorkspaceUsedBytes, QuotaBytes: row.WorkspaceQuotaBytes,
			LastError: row.WorkspaceLastError, CreatedAt: row.WorkspaceCreatedAt.Time,
			UpdatedAt: row.WorkspaceUpdatedAt.Time, DeletedAt: timePointer(row.WorkspaceDeletedAt),
		},
		Directory: row.DirectoryName, State: workspace.BindingState(row.BindingState),
		BindingVersion: row.BindingVersion, DesiredRevision: row.DesiredRevision,
		AppliedRevision: row.AppliedRevision, SyncState: workspace.BindingSyncState(row.SyncState),
		SyncConflicts: append([]string(nil), row.SyncConflicts...), CreatedAt: row.BindingCreatedAt.Time,
		UpdatedAt: row.BindingUpdatedAt.Time,
	}
}

func workspaceBindingDomainFromList(row sqlcgen.ListWorkspaceBindingsByHostRow) workspace.Binding {
	return workspace.Binding{
		ID: row.BindingID, OwnerID: row.OwnerID, HostID: row.HostID,
		Workspace: workspace.Workspace{
			ID: row.WorkspaceID, OwnerID: row.OwnerID, Name: row.WorkspaceName,
			State: workspace.State(row.WorkspaceState), Revision: row.WorkspaceRevision,
			UsedBytes: row.WorkspaceUsedBytes, QuotaBytes: row.WorkspaceQuotaBytes,
			LastError: row.WorkspaceLastError, CreatedAt: row.WorkspaceCreatedAt.Time,
			UpdatedAt: row.WorkspaceUpdatedAt.Time, DeletedAt: timePointer(row.WorkspaceDeletedAt),
		},
		Directory: row.DirectoryName, State: workspace.BindingState(row.BindingState),
		BindingVersion: row.BindingVersion, DesiredRevision: row.DesiredRevision,
		AppliedRevision: row.AppliedRevision, SyncState: workspace.BindingSyncState(row.SyncState),
		SyncConflicts: append([]string(nil), row.SyncConflicts...), CreatedAt: row.BindingCreatedAt.Time,
		UpdatedAt: row.BindingUpdatedAt.Time,
	}
}

func workspaceBindingDomainFromHostIDs(row sqlcgen.ListWorkspaceBindingsByHostIDsRow) workspace.Binding {
	return workspace.Binding{
		ID: row.BindingID, OwnerID: row.OwnerID, HostID: row.HostID,
		Workspace: workspace.Workspace{
			ID: row.WorkspaceID, OwnerID: row.OwnerID, Name: row.WorkspaceName,
			State: workspace.State(row.WorkspaceState), Revision: row.WorkspaceRevision,
			UsedBytes: row.WorkspaceUsedBytes, QuotaBytes: row.WorkspaceQuotaBytes,
			LastError: row.WorkspaceLastError, CreatedAt: row.WorkspaceCreatedAt.Time,
			UpdatedAt: row.WorkspaceUpdatedAt.Time, DeletedAt: timePointer(row.WorkspaceDeletedAt),
		},
		Directory: row.DirectoryName, State: workspace.BindingState(row.BindingState),
		BindingVersion: row.BindingVersion, DesiredRevision: row.DesiredRevision,
		AppliedRevision: row.AppliedRevision, SyncState: workspace.BindingSyncState(row.SyncState),
		SyncConflicts: append([]string(nil), row.SyncConflicts...), CreatedAt: row.BindingCreatedAt.Time,
		UpdatedAt: row.BindingUpdatedAt.Time,
	}
}

func workspaceBindingDomainFromWorkspaceIDs(row sqlcgen.ListWorkspaceBindingsByWorkspaceRow) workspace.Binding {
	return workspace.Binding{
		ID: row.BindingID, OwnerID: row.OwnerID, HostID: row.HostID,
		Workspace: workspace.Workspace{
			ID: row.WorkspaceID, OwnerID: row.OwnerID, Name: row.WorkspaceName,
			State: workspace.State(row.WorkspaceState), Revision: row.WorkspaceRevision,
			UsedBytes: row.WorkspaceUsedBytes, QuotaBytes: row.WorkspaceQuotaBytes,
			LastError: row.WorkspaceLastError, CreatedAt: row.WorkspaceCreatedAt.Time,
			UpdatedAt: row.WorkspaceUpdatedAt.Time, DeletedAt: timePointer(row.WorkspaceDeletedAt),
		},
		Directory: row.DirectoryName, State: workspace.BindingState(row.BindingState),
		BindingVersion: row.BindingVersion, DesiredRevision: row.DesiredRevision,
		AppliedRevision: row.AppliedRevision, SyncState: workspace.BindingSyncState(row.SyncState),
		SyncConflicts: append([]string(nil), row.SyncConflicts...), CreatedAt: row.BindingCreatedAt.Time,
		UpdatedAt: row.BindingUpdatedAt.Time,
	}
}

func userDomain(row sqlcgen.User) identity.User {
	return identity.User{
		ID: row.ID, Name: row.Name, PasswordHash: row.PasswordHash, Role: identity.Role(row.Role),
		Status: identity.AccountStatus(row.Status), MustChangePassword: row.MustChangePassword,
		QuotaHosts: int(row.QuotaHosts), QuotaCPUMilli: int(row.QuotaCpuMilli),
		QuotaMemMiB: int(row.QuotaMemMib),
		CreatedAt:   row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func tokenDomain(row sqlcgen.Token) identity.Token {
	scopes := make([]identity.Scope, 0, len(row.Scopes))
	for _, scope := range row.Scopes {
		scopes = append(scopes, identity.Scope(scope))
	}
	return identity.Token{
		ID: row.ID, OwnerID: row.OwnerID, Kind: identity.Kind(row.Kind), HostID: row.HostID.String, PreviewPort: int(row.PreviewPort.Int32),
		ActorUserID:          row.ActorUserID.String,
		ClientInstallationID: row.ClientInstallationID.String,
		HostIDs:              append([]string(nil), row.HostIds...), WorkspaceIDs: append([]string(nil), row.WorkspaceIds...), Scopes: scopes,
		Name: row.Name, SecretHash: row.SecretHash, ExpiresAt: row.ExpiresAt.Time,
		UsedAt: timePointer(row.UsedAt), LastUsedAt: timePointer(row.LastUsedAt),
		RevokedAt: timePointer(row.RevokedAt), CreatedAt: row.CreatedAt.Time,
	}
}

func auditDomain(row sqlcgen.AuditLog) (app.AuditLog, error) {
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		return app.AuditLog{}, fmt.Errorf("decode audit detail: %w", err)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	return app.AuditLog{
		ID: row.ID, At: row.At.Time,
		Entry: audit.Entry{
			ActorID: row.ActorID, ActorKind: audit.ActorKind(row.ActorKind), Action: audit.Action(row.Action),
			Target: row.Target, Detail: detail, IP: row.Ip,
		},
	}, nil
}

func auditParams(entry audit.Entry) (sqlcgen.CreateAuditLogParams, error) {
	detail := entry.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return sqlcgen.CreateAuditLogParams{}, fmt.Errorf("encode audit detail: %w", err)
	}
	return sqlcgen.CreateAuditLogParams{
		ActorID: entry.ActorID, ActorKind: string(entry.ActorKind), Action: string(entry.Action),
		Target: entry.Target, Detail: encoded, Ip: entry.IP,
	}, nil
}

func nullableText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func nullableTimestamp(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return timestamp(*value)
}

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func mapRows[T, U any](rows []T, convert func(T) U) []U {
	result := make([]U, 0, len(rows))
	for _, row := range rows {
		result = append(result, convert(row))
	}
	return result
}

func mapRowsWithError[T, U any](rows []T, convert func(T) (U, error)) ([]U, error) {
	result := make([]U, 0, len(rows))
	for _, row := range rows {
		value, err := convert(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
