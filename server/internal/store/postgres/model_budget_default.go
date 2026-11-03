package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) GetModelBudgetDefault(ctx context.Context) (app.ModelBudgetDefault, error) {
	value, configured, err := readModelBudgetDefault(ctx, s.pool)
	if err != nil || !configured {
		return value, err
	}
	return value, nil
}

func (s *Store) SetModelBudgetDefault(ctx context.Context, principal identity.Principal, command app.UpdateModelBudgetDefaultCommand, ip string) (app.ModelBudgetDefault, error) {
	if command.ExpectedVersion < 0 || command.TokenLimit <= 0 || command.MaxConcurrent < 1 || command.MaxConcurrent > 100 || len(ip) > 64 {
		return app.ModelBudgetDefault{}, domain.InvalidArgument("model_budget_default", "invalid default model budget")
	}
	var value app.ModelBudgetDefault
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:admin-model-budget-default',0))`); err != nil {
			return fmt.Errorf("lock default model budget update: %w", err)
		}
		current, configured, err := readModelBudgetDefault(ctx, tx)
		if err != nil {
			return err
		}
		if current.Version != command.ExpectedVersion {
			conflict := domain.NewError(domain.CodeConflict, "default model budget version changed")
			conflict.Details["expected_version"] = command.ExpectedVersion
			conflict.Details["current_version"] = current.Version
			return conflict
		}
		if current.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "default model budget version is exhausted")
		}
		if configured {
			result, err := tx.Exec(ctx, `UPDATE admin_model_budget_defaults
SET token_limit=$1,max_concurrent=$2,version=version+1,updated_at=clock_timestamp()
WHERE id=1 AND version=$3`, command.TokenLimit, command.MaxConcurrent, command.ExpectedVersion)
			if err != nil {
				return fmt.Errorf("update default model budget: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "default model budget version changed")
			}
		} else {
			result, err := tx.Exec(ctx, `INSERT INTO admin_model_budget_defaults(id,token_limit,max_concurrent,version)
VALUES (1,$1,$2,1) ON CONFLICT (id) DO NOTHING`, command.TokenLimit, command.MaxConcurrent)
			if err != nil {
				return fmt.Errorf("create default model budget: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "default model budget version changed")
			}
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionModelBudgetUpdate,
			Target: "admin_model_budget_defaults", IP: ip, Detail: map[string]any{
				"configured_before": configured, "token_limit_before": current.TokenLimit,
				"max_concurrent_before": current.MaxConcurrent, "expected_version": command.ExpectedVersion,
				"token_limit": command.TokenLimit, "max_concurrent": command.MaxConcurrent,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("audit default model budget update: %w", err)
		}
		value, _, err = readModelBudgetDefault(ctx, tx)
		return err
	})
	if err != nil {
		return app.ModelBudgetDefault{}, err
	}
	return value, nil
}

func loadModelBudgetDefaultInTx(ctx context.Context, tx pgx.Tx) (app.ModelBudgetDefault, error) {
	value, configured, err := readModelBudgetDefault(ctx, tx)
	if err != nil {
		return app.ModelBudgetDefault{}, err
	}
	if !configured || value.TokenLimit <= 0 || value.MaxConcurrent < 1 || value.MaxConcurrent > 100 || value.Version < 1 {
		return app.ModelBudgetDefault{}, domain.NewError(domain.CodeInvalidState, "default model budget must be configured before approving new accounts")
	}
	return value, nil
}

func readModelBudgetDefault(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (app.ModelBudgetDefault, bool, error) {
	var value app.ModelBudgetDefault
	var maxConcurrent int32
	err := db.QueryRow(ctx, `SELECT token_limit,max_concurrent,version FROM admin_model_budget_defaults WHERE id=1 FOR SHARE`).
		Scan(&value.TokenLimit, &maxConcurrent, &value.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ModelBudgetDefault{}, false, nil
	}
	if err != nil {
		return app.ModelBudgetDefault{}, false, fmt.Errorf("read default model budget: %w", err)
	}
	value.Configured = true
	value.MaxConcurrent = maxConcurrent
	return value, true, nil
}
