package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) GetAdminModelDefault(ctx context.Context) (app.AdminModelDefaultRecord, error) {
	row, err := s.q.GetAdminModelDefault(ctx)
	if err != nil {
		return app.AdminModelDefaultRecord{}, fmt.Errorf("get admin model default: %w", err)
	}
	return app.AdminModelDefaultRecord{
		ModelID: row.ModelConnectionID.String, Version: row.Version, UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

func (s *Store) SetAdminModelDefault(ctx context.Context, principal identity.Principal, expectedVersion int64, modelID, ip string) (app.AdminModelDefaultRecord, error) {
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || len(ip) > 64 ||
		(modelID != "" && !domain.ValidModelConnectionID(modelID)) {
		return app.AdminModelDefaultRecord{}, domain.InvalidArgument("default_model", "invalid default model setting")
	}
	var value app.AdminModelDefaultRecord
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		var currentID pgtype.Text
		var currentVersion int64
		if err := tx.QueryRow(ctx, `SELECT model_connection_id,version FROM admin_model_defaults WHERE id=1 FOR UPDATE`).
			Scan(&currentID, &currentVersion); err != nil {
			return fmt.Errorf("lock admin model default: %w", err)
		}
		if currentVersion != expectedVersion {
			conflict := domain.NewError(domain.CodeConflict, "default model version changed")
			conflict.Details["expected_version"] = expectedVersion
			conflict.Details["current_version"] = currentVersion
			return conflict
		}
		if currentVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "default model version is exhausted")
		}
		if modelID != "" {
			var ownerID pgtype.Text
			var checkedAt pgtype.Timestamptz
			var supportsText bool
			err := tx.QueryRow(ctx, `SELECT owner_id,checked_at,capability_text
FROM model_connections WHERE id=$1 FOR SHARE`, modelID).
				Scan(&ownerID, &checkedAt, &supportsText)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownerID.Valid) {
				return domain.NewError(domain.CodeNotFound, "model connection not found")
			}
			if err != nil {
				return fmt.Errorf("lock default model connection: %w", err)
			}
			if !checkedAt.Valid || !supportsText {
				return domain.NewError(domain.CodeUnsupported, "default model must be a checked shared text model")
			}
		}
		updated, err := sqlcgen.New(tx).SetAdminModelDefault(ctx, sqlcgen.SetAdminModelDefaultParams{
			ModelID: modelID, ExpectedVersion: expectedVersion,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "default model version changed")
		}
		if err != nil {
			return fmt.Errorf("update admin model default: %w", err)
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionModelDefaultUpdate,
			Target: "admin_model_defaults", IP: ip, Detail: map[string]any{
				"model_id_before": currentID.String, "model_id": modelID, "expected_version": expectedVersion,
				"version": updated.Version,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("audit admin model default update: %w", err)
		}
		value = app.AdminModelDefaultRecord{
			ModelID: updated.ModelConnectionID.String, Version: updated.Version, UpdatedAt: updated.UpdatedAt.Time,
		}
		return nil
	})
	if err != nil {
		return app.AdminModelDefaultRecord{}, err
	}
	return value, nil
}

var _ app.AdminModelDefaultStore = (*Store)(nil)
