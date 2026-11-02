package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

func (s *Store) ListRecordConsents(ctx context.Context, principal identity.Principal) ([]record.Consent, error) {
	values := []record.Consent{
		{OwnerID: principal.UserID, Purpose: record.PurposeFullRecord},
		{OwnerID: principal.UserID, Purpose: record.PurposeTrainingContribution},
	}
	byPurpose := map[record.Purpose]int{
		record.PurposeFullRecord:           0,
		record.PurposeTrainingContribution: 1,
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT purpose, enabled, version, updated_at
FROM record_consents WHERE owner_id=$1`, principal.UserID)
		if err != nil {
			return fmt.Errorf("list record consents: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var purpose string
			var enabled bool
			var version int64
			var updatedAt time.Time
			if err := rows.Scan(&purpose, &enabled, &version, &updatedAt); err != nil {
				return fmt.Errorf("scan record consent: %w", err)
			}
			index, ok := byPurpose[record.Purpose(purpose)]
			if !ok {
				return domain.NewError(domain.CodeInternal, "stored record purpose is invalid")
			}
			values[index].Enabled = enabled
			values[index].Version = version
			values[index].UpdatedAt = updatedAt
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list record consent rows: %w", err)
		}
		rows.Close()
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordConsentRead, recordAuditTarget(principal.UserID, ""), map[string]any{
			"owner_id": principal.UserID,
		}); err != nil {
			return fmt.Errorf("audit record consent read: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) SetRecordConsent(ctx context.Context, principal identity.Principal, purpose record.Purpose, enabled bool) (record.Consent, error) {
	if !purpose.Valid() {
		return record.Consent{}, domain.InvalidArgument("purpose", "record consent purpose is invalid")
	}
	var value record.Consent
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO record_consents (owner_id,purpose,enabled,version)
VALUES ($1,$2,$3,1) ON CONFLICT (owner_id,purpose) DO NOTHING
RETURNING owner_id,purpose,enabled,version,updated_at`, principal.UserID, string(purpose), enabled).Scan(
			&value.OwnerID, &value.Purpose, &value.Enabled, &value.Version, &value.UpdatedAt)
		if err == nil {
			return writeRecordAudit(ctx, tx, principal, audit.ActionRecordConsentUpdate, recordAuditTarget(principal.UserID, ""), map[string]any{
				"owner_id": principal.UserID, "purpose": string(purpose), "enabled": enabled, "version": value.Version,
			})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("initialize record consent: %w", err)
		}
		var currentEnabled bool
		var version int64
		if err := tx.QueryRow(ctx, `SELECT enabled,version FROM record_consents
WHERE owner_id=$1 AND purpose=$2 FOR UPDATE`, principal.UserID, string(purpose)).Scan(&currentEnabled, &version); err != nil {
			return fmt.Errorf("lock record consent: %w", err)
		}
		if currentEnabled == enabled {
			if err := tx.QueryRow(ctx, `SELECT owner_id,purpose,enabled,version,updated_at
FROM record_consents WHERE owner_id=$1 AND purpose=$2`, principal.UserID, string(purpose)).Scan(
				&value.OwnerID, &value.Purpose, &value.Enabled, &value.Version, &value.UpdatedAt); err != nil {
				return err
			}
			return writeRecordAudit(ctx, tx, principal, audit.ActionRecordConsentUpdate, recordAuditTarget(principal.UserID, ""), map[string]any{
				"owner_id": principal.UserID, "purpose": string(purpose), "enabled": enabled, "version": value.Version,
			})
		}
		if err := tx.QueryRow(ctx, `UPDATE record_consents SET enabled=$3,version=version+1,updated_at=clock_timestamp()
WHERE owner_id=$1 AND purpose=$2
RETURNING owner_id,purpose,enabled,version,updated_at`, principal.UserID, string(purpose), enabled).Scan(
			&value.OwnerID, &value.Purpose, &value.Enabled, &value.Version, &value.UpdatedAt); err != nil {
			return fmt.Errorf("update record consent: %w", err)
		}
		if !enabled {
			if _, err := tx.Exec(ctx, `UPDATE record_capture_queue SET state='rejected',failure_code='consent_withdrawn',
  lease_owner=NULL,lease_expires_at=NULL,payload=decode('','hex'),updated_at=clock_timestamp()
WHERE owner_id=$1 AND purpose=$2 AND state IN ('queued','leased')`, principal.UserID, string(purpose)); err != nil {
				return fmt.Errorf("reject withdrawn record queue items: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM record_capture_queue_attachments a
USING record_capture_queue q WHERE a.queue_item_id=q.id AND q.owner_id=$1 AND q.purpose=$2
  AND q.state='rejected' AND q.failure_code='consent_withdrawn'`, principal.UserID, string(purpose)); err != nil {
				return fmt.Errorf("clear withdrawn record queue attachments: %w", err)
			}
			if err := ensureRecordQuotaTx(ctx, tx, principal.UserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE record_quotas SET reserved_bytes=(
  SELECT COALESCE(sum(reserved_bytes),0) FROM record_capture_queue
  WHERE owner_id=$1 AND state IN ('queued','leased')
),updated_at=clock_timestamp() WHERE owner_id=$1`, principal.UserID); err != nil {
				return fmt.Errorf("reconcile withdrawn record quota reservation: %w", err)
			}
		}
		return writeRecordAudit(ctx, tx, principal, audit.ActionRecordConsentUpdate, recordAuditTarget(principal.UserID, ""), map[string]any{
			"owner_id": principal.UserID, "purpose": string(purpose), "enabled": enabled, "version": value.Version,
		})
	})
	if err != nil {
		return record.Consent{}, err
	}
	return value, nil
}

func (s *Store) GetRecordQuota(ctx context.Context, principal identity.Principal, ownerID string) (record.Quota, error) {
	if !domain.ValidUserID(ownerID) {
		return record.Quota{}, domain.InvalidArgument("owner_id", "record quota owner is invalid")
	}
	var value record.Quota
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if ownerID == principal.UserID {
			if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
				return err
			}
		} else if err := recheckRecordAdminInTx(ctx, tx, principal, true); err != nil {
			return err
		}
		if err := ensureRecordQuotaTx(ctx, tx, ownerID); err != nil {
			return err
		}
		if err := scanRecordQuota(tx.QueryRow(ctx, `SELECT owner_id,retention_days,limit_bytes,used_bytes,
reserved_bytes,version,updated_at FROM record_quotas WHERE owner_id=$1`, ownerID), &value); err != nil {
			return err
		}
		return writeRecordAudit(ctx, tx, principal, audit.ActionRecordQuotaRead, recordAuditTarget(ownerID, ""), map[string]any{
			"owner_id": ownerID,
		})
	})
	if err != nil {
		return record.Quota{}, err
	}
	return value, nil
}

func (s *Store) SetRecordQuota(ctx context.Context, principal identity.Principal, ownerID string, expectedVersion int64, retentionDays int32, limitBytes int64) (record.Quota, error) {
	if !domain.ValidUserID(ownerID) || expectedVersion < 1 || retentionDays < 1 || retentionDays > record.MaxRetentionDays ||
		limitBytes < 1 || limitBytes > record.MaxQuotaBytes {
		return record.Quota{}, domain.InvalidArgument("record_quota", "record quota settings are invalid")
	}
	var value record.Quota
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := recheckRecordAdminInTx(ctx, tx, principal, true); err != nil {
			return err
		}
		if err := ensureRecordQuotaTx(ctx, tx, ownerID); err != nil {
			return err
		}
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM record_quotas WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&version); err != nil {
			return fmt.Errorf("lock record quota: %w", err)
		}
		if version != expectedVersion {
			return domain.NewError(domain.CodeConflict, "record quota version changed")
		}
		if err := scanRecordQuota(tx.QueryRow(ctx, `UPDATE record_quotas SET retention_days=$2,limit_bytes=$3,
version=version+1,updated_at=clock_timestamp() WHERE owner_id=$1
RETURNING owner_id,retention_days,limit_bytes,used_bytes,reserved_bytes,version,updated_at`,
			ownerID, retentionDays, limitBytes), &value); err != nil {
			return err
		}
		return writeRecordAudit(ctx, tx, principal, audit.ActionRecordQuotaUpdate, recordAuditTarget(ownerID, ""), map[string]any{
			"owner_id": ownerID, "retention_days": retentionDays, "limit_bytes": limitBytes,
			"expected_version": expectedVersion, "version": value.Version,
		})
	})
	if err != nil {
		return record.Quota{}, err
	}
	return value, nil
}

type rowScanner interface{ Scan(...any) error }

func scanRecordQuota(row rowScanner, value *record.Quota) error {
	if err := row.Scan(&value.OwnerID, &value.RetentionDays, &value.LimitBytes, &value.UsedBytes,
		&value.ReservedBytes, &value.Version, &value.UpdatedAt); err != nil {
		return fmt.Errorf("scan record quota: %w", err)
	}
	return nil
}

func ensureRecordQuotaTx(ctx context.Context, tx pgx.Tx, ownerID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO record_quotas (owner_id) VALUES ($1) ON CONFLICT (owner_id) DO NOTHING`, ownerID)
	if err != nil {
		return fmt.Errorf("initialize record quota: %w", err)
	}
	return nil
}

func authorizeRecordUserInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) ||
		(principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID)) ||
		(principal.Kind == identity.KindSession && principal.ClientInstallationID != "") {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return authorizePersonalMutationInTx(ctx, tx, principal)
}

func recheckRecordAdminInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal, superAdmin bool) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		principal.ClientInstallationID != "" {
		return domain.NewError(domain.CodeForbidden, "record administrator access is denied")
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT u.role FROM users u JOIN tokens t ON t.owner_id=u.id
WHERE u.id=$1 AND u.status='approved' AND t.id=$2 AND t.kind='session' AND t.revoked_at IS NULL
AND t.expires_at > clock_timestamp() AND COALESCE(t.client_installation_id,'')=''
FOR SHARE OF u,t`, principal.UserID, principal.TokenID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err != nil {
		return fmt.Errorf("recheck record administrator credential: %w", err)
	}
	if superAdmin && role != string(identity.RoleSuperAdmin) {
		return domain.NewError(domain.CodeForbidden, "record super administrator access is denied")
	}
	if !superAdmin && role != string(identity.RoleAdmin) && role != string(identity.RoleSuperAdmin) {
		return domain.NewError(domain.CodeForbidden, "record administrator access is denied")
	}
	return nil
}
