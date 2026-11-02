package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ListAdminControls(ctx context.Context) (app.AdminControls, error) {
	result := app.AdminControls{
		Rules: make([]app.AdminControlRule, 0), Notifications: make([]app.AdminControlNotification, 0),
		NotifyConfigured: true,
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&result.AccountsTotal); err != nil {
		return app.AdminControls{}, fmt.Errorf("count accounts for admin controls: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version
FROM admin_control_rules ORDER BY threshold,id`)
	if err != nil {
		return app.AdminControls{}, fmt.Errorf("list admin control rules: %w", err)
	}
	for rows.Next() {
		rule, err := scanAdminControlRule(rows)
		if err != nil {
			rows.Close()
			return app.AdminControls{}, fmt.Errorf("read admin control rule: %w", err)
		}
		if result.NextThreshold == nil && rule.Enabled && rule.LastFiredAt == nil {
			value := rule.Threshold
			result.NextThreshold = &value
		}
		result.Rules = append(result.Rules, rule)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.AdminControls{}, fmt.Errorf("read admin control rules: %w", err)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT kind,rule_id,accounts_total,created_at
FROM admin_control_notifications ORDER BY created_at DESC,id DESC LIMIT 20`)
	if err != nil {
		return app.AdminControls{}, fmt.Errorf("list local admin control notifications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminControlNotification
		var ruleID pgtype.Int8
		if err := rows.Scan(&item.Kind, &ruleID, &item.AccountsTotal, &item.CreatedAt); err != nil {
			return app.AdminControls{}, fmt.Errorf("read local admin control notification: %w", err)
		}
		if ruleID.Valid {
			id := ruleID.Int64
			item.RuleID = &id
		}
		result.Notifications = append(result.Notifications, item)
	}
	if err := rows.Err(); err != nil {
		return app.AdminControls{}, fmt.Errorf("read local admin control notifications: %w", err)
	}
	return result, nil
}

func (s *Store) CreateAdminControlRule(ctx context.Context, principal identity.Principal, threshold int64, note string) (app.AdminControlRule, error) {
	if err := validateStoredAdminControlFields(threshold, note); err != nil {
		return app.AdminControlRule{}, err
	}
	var result app.AdminControlRule
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rule, err := scanAdminControlRule(tx.QueryRow(ctx, `INSERT INTO admin_control_rules (threshold,note,created_by)
VALUES ($1,$2,$3) RETURNING id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version`, threshold, note, principal.UserID))
		if err != nil {
			return fmt.Errorf("create admin control rule: %w", err)
		}
		if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlRuleCreate, adminControlRuleTarget(rule.ID), map[string]any{
			"threshold": rule.Threshold, "enabled": rule.Enabled, "version": rule.Version,
		}); err != nil {
			return fmt.Errorf("audit admin control rule creation: %w", err)
		}
		result = rule
		return nil
	})
	return result, err
}

func (s *Store) UpdateAdminControlRule(ctx context.Context, principal identity.Principal, command app.UpdateAdminControlRuleCommand) (app.AdminControlRule, error) {
	if command.ID < 1 || command.ExpectedVersion < 1 ||
		(command.Threshold == nil && command.Enabled == nil && command.Note == nil && !command.Rearm) ||
		(command.Threshold != nil && *command.Threshold < 1) || (command.Note != nil && validateStoredAdminControlFields(1, *command.Note) != nil) {
		return app.AdminControlRule{}, domain.InvalidArgument("admin_control_rule", "invalid admin control rule update")
	}
	var result app.AdminControlRule
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		before, err := loadAdminControlRuleForUpdate(ctx, tx, command.ID)
		if err != nil {
			return err
		}
		if before.Version != command.ExpectedVersion {
			return domain.NewError(domain.CodeConflict, "admin control rule version changed")
		}
		after := before
		if command.Threshold != nil {
			after.Threshold = *command.Threshold
		}
		if command.Enabled != nil {
			after.Enabled = *command.Enabled
		}
		if command.Note != nil {
			after.Note = *command.Note
		}
		if command.Rearm {
			after.LastFiredAt, after.FiredAccounts = nil, 0
		}
		rule, err := scanAdminControlRule(tx.QueryRow(ctx, `UPDATE admin_control_rules
SET threshold=$2,enabled=$3,note=$4,last_fired_at=$5,fired_accounts=$6,version=version+1,updated_at=clock_timestamp()
WHERE id=$1 RETURNING id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version`, command.ID,
			after.Threshold, after.Enabled, after.Note, after.LastFiredAt, after.FiredAccounts))
		if err != nil {
			return fmt.Errorf("update admin control rule: %w", err)
		}
		if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlRuleUpdate, adminControlRuleTarget(rule.ID), map[string]any{
			"previous_version": before.Version, "version": rule.Version,
			"threshold_before": before.Threshold, "threshold_after": rule.Threshold,
			"enabled_before": before.Enabled, "enabled_after": rule.Enabled,
			"rearmed": command.Rearm,
		}); err != nil {
			return fmt.Errorf("audit admin control rule update: %w", err)
		}
		result = rule
		return nil
	})
	return result, err
}

func (s *Store) DeleteAdminControlRule(ctx context.Context, principal identity.Principal, id, expectedVersion int64) error {
	if id < 1 || expectedVersion < 1 {
		return domain.InvalidArgument("admin_control_rule", "rule id and expected version must be positive")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		before, err := loadAdminControlRuleForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if before.Version != expectedVersion {
			return domain.NewError(domain.CodeConflict, "admin control rule version changed")
		}
		if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlRuleDelete, adminControlRuleTarget(id), map[string]any{
			"threshold": before.Threshold, "enabled": before.Enabled, "version": before.Version,
		}); err != nil {
			return fmt.Errorf("audit admin control rule deletion: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM admin_control_rules WHERE id=$1`, id); err != nil {
			return fmt.Errorf("delete admin control rule: %w", err)
		}
		return nil
	})
}

func (s *Store) EvaluateAdminControlRules(ctx context.Context, principal identity.Principal) (app.AdminControlEvaluation, error) {
	result := app.AdminControlEvaluation{Fired: make([]app.AdminControlRule, 0)}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&result.AccountsTotal); err != nil {
			return fmt.Errorf("count accounts for admin control evaluation: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version
FROM admin_control_rules WHERE enabled AND last_fired_at IS NULL AND threshold <= $1 ORDER BY id FOR UPDATE`, result.AccountsTotal)
		if err != nil {
			return fmt.Errorf("select admin control rules to evaluate: %w", err)
		}
		candidates := make([]app.AdminControlRule, 0)
		for rows.Next() {
			rule, err := scanAdminControlRule(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("read admin control rule for evaluation: %w", err)
			}
			candidates = append(candidates, rule)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read admin control rules for evaluation: %w", err)
		}
		rows.Close()
		for _, candidate := range candidates {
			rule, err := scanAdminControlRule(tx.QueryRow(ctx, `UPDATE admin_control_rules
SET last_fired_at=clock_timestamp(),fired_accounts=$2,version=version+1,updated_at=clock_timestamp()
WHERE id=$1 AND enabled AND last_fired_at IS NULL
RETURNING id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version`, candidate.ID, result.AccountsTotal))
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("mark admin control rule fired: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO admin_control_notifications (kind,rule_id,accounts_total) VALUES ('rule',$1,$2)`, rule.ID, result.AccountsTotal); err != nil {
				return fmt.Errorf("write local admin control notification: %w", err)
			}
			if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlRuleFire, adminControlRuleTarget(rule.ID), map[string]any{
				"threshold": rule.Threshold, "accounts_total": result.AccountsTotal, "version": rule.Version,
			}); err != nil {
				return fmt.Errorf("audit admin control rule firing: %w", err)
			}
			result.Fired = append(result.Fired, rule)
		}
		if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlEvaluate, "rules", map[string]any{
			"accounts_total": result.AccountsTotal, "fired": len(result.Fired),
		}); err != nil {
			return fmt.Errorf("audit admin control evaluation: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) CreateAdminControlNotifyTest(ctx context.Context, principal identity.Principal) (app.AdminControlNotifyTest, error) {
	var result app.AdminControlNotifyTest
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var accountsTotal int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&accountsTotal); err != nil {
			return fmt.Errorf("count accounts for local admin notification test: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO admin_control_notifications (kind,accounts_total) VALUES ('test',$1) RETURNING created_at`, accountsTotal).Scan(&result.CreatedAt); err != nil {
			return fmt.Errorf("write local admin notification test: %w", err)
		}
		if err := writeAdminControlAudit(ctx, tx, principal, audit.ActionAdminControlNotifyTest, "local-receiver", map[string]any{
			"channel": "local_database", "accounts_total": accountsTotal,
		}); err != nil {
			return fmt.Errorf("audit local admin notification test: %w", err)
		}
		result.Sent, result.Configured = true, true
		return nil
	})
	return result, err
}

func scanAdminControlRule(row pgx.Row) (app.AdminControlRule, error) {
	var rule app.AdminControlRule
	var lastFired pgtype.Timestamptz
	err := row.Scan(&rule.ID, &rule.Threshold, &rule.Enabled, &rule.Note, &rule.CreatedAt, &lastFired, &rule.FiredAccounts, &rule.Version)
	if err != nil {
		return app.AdminControlRule{}, err
	}
	if lastFired.Valid {
		firedAt := lastFired.Time
		rule.LastFiredAt = &firedAt
	}
	return rule, nil
}

func loadAdminControlRuleForUpdate(ctx context.Context, tx pgx.Tx, id int64) (app.AdminControlRule, error) {
	rule, err := scanAdminControlRule(tx.QueryRow(ctx, `SELECT id,threshold,enabled,note,created_at,last_fired_at,fired_accounts,version
FROM admin_control_rules WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.AdminControlRule{}, domain.NewError(domain.CodeNotFound, "admin control rule not found")
	}
	if err != nil {
		return app.AdminControlRule{}, fmt.Errorf("lock admin control rule: %w", err)
	}
	return rule, nil
}

func writeAdminControlAudit(ctx context.Context, tx pgx.Tx, principal identity.Principal, action audit.Action, target string, detail map[string]any) error {
	entry, err := auditParams(audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: action, Target: target, Detail: detail})
	if err != nil {
		return err
	}
	if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
		return err
	}
	return nil
}

func validateStoredAdminControlFields(threshold int64, note string) error {
	if threshold < 1 || !utf8.ValidString(note) || len([]rune(note)) > 256 || strings.TrimSpace(note) != note || strings.IndexFunc(note, unicode.IsControl) >= 0 {
		return domain.InvalidArgument("admin_control_rule", "invalid threshold or note")
	}
	return nil
}

func adminControlRuleTarget(id int64) string { return fmt.Sprintf("rule:%d", id) }

var _ app.AdminControlsStore = (*Store)(nil)
