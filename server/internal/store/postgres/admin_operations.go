package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) GetOrCreateUserInviteCode(ctx context.Context, ownerID, code string) (string, error) {
	if !domain.ValidUserID(ownerID) || !validInviteCode(code) {
		return "", domain.InvalidArgument("invite_code", "invitation code request is invalid")
	}
	var stored string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, ownerID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "user not found")
		}
		if err != nil {
			return fmt.Errorf("authorize invitation code owner: %w", err)
		}
		if status != string(identity.StatusApproved) {
			return domain.NewError(domain.CodeForbidden, "approved account required for invitation code")
		}
		stored, err = sqlcgen.New(tx).CreateUserInviteCode(ctx, sqlcgen.CreateUserInviteCodeParams{OwnerID: ownerID, Code: code})
		if isUniqueViolation(err) {
			return domain.NewError(domain.CodeConflict, "invitation code already exists")
		}
		if err != nil {
			return fmt.Errorf("create user invitation code: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return stored, nil
}

func (s *Store) CreateInvitedUser(ctx context.Context, value identity.User, inviteCode string) (identity.User, error) {
	if err := validateNewUser(value); err != nil {
		return identity.User{}, err
	}
	if !validInviteCode(inviteCode) {
		return identity.User{}, domain.InvalidArgument("invite_code", "invitation code is invalid")
	}
	var created identity.User
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var inviterID string
		err := tx.QueryRow(ctx, `SELECT code.owner_id FROM user_invite_codes AS code
JOIN users AS owner ON owner.id=code.owner_id
WHERE code.code=$1 AND owner.status='approved'
FOR SHARE OF owner`, inviteCode).Scan(&inviterID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.InvalidArgument("invite_code", "invitation code is invalid")
		}
		if err != nil {
			return fmt.Errorf("find invitation code owner: %w", err)
		}
		row, err := sqlcgen.New(tx).CreateUser(ctx, sqlcgen.CreateUserParams{
			ID: value.ID, Name: value.Name, PasswordHash: value.PasswordHash, Role: string(value.Role),
			Status: string(value.Status), MustChangePassword: value.MustChangePassword,
			QuotaHosts: int32(value.QuotaHosts), QuotaCpuMilli: int32(value.QuotaCPUMilli), QuotaMemMib: int32(value.QuotaMemMiB),
			DefaultHostCpuMilli: int32(value.DefaultHostCPUMilli), DefaultHostMemMib: int32(value.DefaultHostMemMiB),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "username already exists")
			}
			return fmt.Errorf("create invited user: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_invites(inviter_id,invitee_id,invite_code) VALUES($1,$2,$3)`, inviterID, value.ID, inviteCode); err != nil {
			return fmt.Errorf("record user invitation: %w", err)
		}
		created = userDomain(row)
		return nil
	})
	if err != nil {
		return identity.User{}, err
	}
	return created, nil
}

func (s *Store) ListAdminInvites(ctx context.Context, filter app.AdminInviteFilter) (app.AdminInvitePage, error) {
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 || filter.Search != strings.TrimSpace(filter.Search) {
		return app.AdminInvitePage{}, domain.InvalidArgument("invites", "invitation query is invalid")
	}
	page := app.AdminInvitePage{Items: []app.AdminInvite{}, Limit: filter.Limit, Offset: filter.Offset}
	queries := sqlcgen.New(s.pool)
	stats, err := queries.AdminInviteStats(ctx, filter.Search)
	if err != nil {
		return app.AdminInvitePage{}, fmt.Errorf("read invitation summary: %w", err)
	}
	page.Total, page.TotalInviters = stats.Total, stats.TotalInviters
	rows, err := queries.ListAdminInvites(ctx, sqlcgen.ListAdminInvitesParams{
		Search: filter.Search, PageOffset: int32(filter.Offset), PageLimit: int32(filter.Limit),
	})
	if err != nil {
		return app.AdminInvitePage{}, fmt.Errorf("list admin invitations: %w", err)
	}
	for _, row := range rows {
		item := app.AdminInvite{InviterID: row.InviterID, InviterName: row.InviterName, InviteeID: row.InviteeID,
			InviteeName: row.InviteeName, InviteeStatus: identity.AccountStatus(row.InviteeStatus), InviterInvitees: row.InviterInvitees}
		if row.CreatedAt.Valid {
			item.CreatedAt = row.CreatedAt.Time
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func (s *Store) ListAdminSites(ctx context.Context, limit, offset int) (app.AdminSitePage, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return app.AdminSitePage{}, domain.InvalidArgument("sites", "site query is invalid")
	}
	page := app.AdminSitePage{Items: []app.AdminSite{}, Limit: limit, Offset: offset}
	queries := sqlcgen.New(s.pool)
	summary, err := queries.AdminSiteSummary(ctx)
	if err != nil {
		return app.AdminSitePage{}, fmt.Errorf("read admin site summary: %w", err)
	}
	page.Summary = app.AdminSiteSummary{Sites: summary.Sites, ActiveSites: summary.ActiveSites, Releases: summary.Releases, HealthyReleases: summary.HealthyReleases}
	page.Total, err = queries.CountAdminSites(ctx)
	if err != nil {
		return app.AdminSitePage{}, fmt.Errorf("count admin sites: %w", err)
	}
	rows, err := queries.ListAdminSites(ctx, sqlcgen.ListAdminSitesParams{PageOffset: int32(offset), PageLimit: int32(limit)})
	if err != nil {
		return app.AdminSitePage{}, fmt.Errorf("list admin sites: %w", err)
	}
	for _, row := range rows {
		item := app.AdminSite{ID: row.ID, OwnerID: row.OwnerID, OwnerName: row.OwnerName, HostID: row.HostID,
			State: row.State, ReleaseCount: row.ReleaseCount, HealthyReleaseCount: row.HealthyReleaseCount}
		if row.CreatedAt.Valid {
			item.CreatedAt = row.CreatedAt.Time
		}
		if row.LatestReleaseAt.Valid {
			latest := row.LatestReleaseAt.Time
			item.LatestReleaseAt = &latest
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func (s *Store) GetAdminNudgePolicy(ctx context.Context) (app.AdminNudgePolicy, error) {
	row, err := s.q.GetAdminNudgePolicy(ctx)
	if err != nil {
		return app.AdminNudgePolicy{}, fmt.Errorf("read admin nudge policy: %w", err)
	}
	value := app.AdminNudgePolicy{Enabled: row.Enabled, URL: row.Url, TextZH: row.TextZh, TextEN: row.TextEn, Version: row.Version}
	if row.UpdatedAt.Valid {
		updated := row.UpdatedAt.Time
		value.UpdatedAt = &updated
	}
	return value, nil
}

func (s *Store) UpdateAdminNudgePolicy(ctx context.Context, principal identity.Principal, command app.UpdateAdminNudgeCommand) (app.AdminNudgePolicy, error) {
	var value app.AdminNudgePolicy
	// CAS update and audit share one transaction so a saved nudge can never lose its audit record.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		var current app.AdminNudgePolicy
		if err := tx.QueryRow(ctx, `SELECT enabled,url,text_zh,text_en,version FROM admin_nudge_policy WHERE id=1 FOR UPDATE`).
			Scan(&current.Enabled, &current.URL, &current.TextZH, &current.TextEN, &current.Version); err != nil {
			return fmt.Errorf("lock admin nudge policy: %w", err)
		}
		if current.Version != command.ExpectedVersion {
			conflict := domain.NewError(domain.CodeConflict, "nudge policy version changed")
			conflict.Details["expected_version"] = command.ExpectedVersion
			conflict.Details["current_version"] = current.Version
			return conflict
		}
		if current.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "nudge policy version is exhausted")
		}
		if _, err := tx.Exec(ctx, `UPDATE admin_nudge_policy SET enabled=$1,url=$2,text_zh=$3,text_en=$4,
version=version+1,updated_at=clock_timestamp() WHERE id=1 AND version=$5`, command.Enabled, command.URL, command.TextZH, command.TextEN, command.ExpectedVersion); err != nil {
			return fmt.Errorf("update admin nudge policy: %w", err)
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionAdminNudgePolicyUpdate,
			Target: "admin_nudge_policy", IP: command.IP, Detail: map[string]any{
				"enabled_before": current.Enabled, "enabled_after": command.Enabled,
				"url_changed": current.URL != command.URL, "text_zh_changed": current.TextZH != command.TextZH,
				"text_en_changed": current.TextEN != command.TextEN, "expected_version": command.ExpectedVersion,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("audit admin nudge policy update: %w", err)
		}
		row, err := sqlcgen.New(tx).GetAdminNudgePolicy(ctx)
		if err != nil {
			return fmt.Errorf("read updated admin nudge policy: %w", err)
		}
		value = app.AdminNudgePolicy{Enabled: row.Enabled, URL: row.Url, TextZH: row.TextZh, TextEN: row.TextEn, Version: row.Version}
		if row.UpdatedAt.Valid {
			updated := row.UpdatedAt.Time
			value.UpdatedAt = &updated
		}
		return nil
	})
	if err != nil {
		return app.AdminNudgePolicy{}, err
	}
	return value, nil
}

func validInviteCode(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
