package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

const personalDataQuotaReservationIDPrefix = "pq_"

func (s *Store) GetAdminPersonalDataQuota(ctx context.Context, principal identity.Principal, ownerID string) (app.PersonalDataQuota, error) {
	if !domain.ValidUserID(ownerID) {
		return app.PersonalDataQuota{}, domain.InvalidArgument("owner_id", "personal data quota owner is invalid")
	}
	var value app.PersonalDataQuota
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := lockPersonalDataQuotaOwner(ctx, tx, ownerID); err != nil {
			return err
		}
		value, err = loadPersonalDataQuota(ctx, tx, instanceID, ownerID)
		return err
	})
	if err != nil {
		return app.PersonalDataQuota{}, err
	}
	return value, nil
}

func (s *Store) SetAdminPersonalDataQuota(ctx context.Context, principal identity.Principal, ownerID string, expectedVersion, quotaBytes, maxAttachmentBytes int64) (app.PersonalDataQuota, error) {
	if !domain.ValidUserID(ownerID) || expectedVersion < 1 || quotaBytes < 1 || maxAttachmentBytes < 1 ||
		maxAttachmentBytes > personal.MaxAttachmentBytes {
		return app.PersonalDataQuota{}, domain.InvalidArgument("personal_data_quota", "personal data quota limits or version are invalid")
	}
	var value app.PersonalDataQuota
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := lockPersonalDataQuotaOwner(ctx, tx, ownerID); err != nil {
			return err
		}
		if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID); err != nil {
			return err
		}
		current, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID)
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			conflict := domain.NewError(domain.CodeConflict, "personal data quota version changed")
			conflict.Details["expected_version"] = expectedVersion
			conflict.Details["current_version"] = current.Version
			return conflict
		}
		if current.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "personal data quota version is exhausted")
		}
		if err := tx.QueryRow(ctx, `UPDATE personal_data_quotas
SET quota_bytes=$4,max_attachment_bytes=$5,version=version+1,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND version=$3
RETURNING owner_id,used_bytes,reserved_bytes,quota_bytes,max_attachment_bytes,version`,
			instanceID, ownerID, expectedVersion, quotaBytes, maxAttachmentBytes).Scan(
			&value.OwnerID, &value.UsedBytes, &value.ReservedBytes, &value.QuotaBytes, &value.MaxAttachmentBytes, &value.Version); err != nil {
			return fmt.Errorf("update personal data quota: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.PersonalDataQuota{}, err
	}
	return value, nil
}

// ReservePersonalAttachment locks the canonical pending upload before its quota row,
// so concurrent activation, cancellation, and reservation cleanup serialize on the
// upload and a pre-activation reservation remains discoverable by cancellation.
func (s *Store) ReservePersonalAttachment(ctx context.Context, principal identity.Principal, uploadID string, sizeBytes int64) (string, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return "", domain.NewError(domain.CodeForbidden, "personal attachment owner is not authorized")
	}
	if !personal.ValidPersonalAttachmentUploadID(uploadID) || sizeBytes < 0 || sizeBytes > personal.MaxAttachmentBytes {
		return "", domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	var reservationID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		var declaredSize int64
		var uploadState string
		var uploadReservationID string
		var uploadReservationReleased bool
		var uploadExpiresAt time.Time
		err = tx.QueryRow(ctx, `SELECT size_bytes,state,COALESCE(reservation_id,''),reservation_released,expires_at
FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 FOR UPDATE`,
			instanceID, principal.UserID, uploadID).Scan(&declaredSize, &uploadState, &uploadReservationID, &uploadReservationReleased, &uploadExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
		}
		if err != nil {
			return fmt.Errorf("lock pending personal attachment upload: %w", err)
		}
		if uploadState != string(personal.AttachmentUploadPending) || uploadReservationReleased || !uploadExpiresAt.After(time.Now().UTC()) {
			return domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer pending")
		}
		if declaredSize != sizeBytes {
			return domain.NewError(domain.CodeIdempotencyConflict, "personal attachment reservation size changed")
		}
		if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, principal.UserID); err != nil {
			return err
		}
		quota, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, principal.UserID)
		if err != nil {
			return err
		}
		var existingID string
		var existingSize int64
		var existingState string
		err = tx.QueryRow(ctx, `SELECT reservation_id,size_bytes,state FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, principal.UserID, uploadID).
			Scan(&existingID, &existingSize, &existingState)
		if err == nil {
			if existingSize != sizeBytes {
				return domain.NewError(domain.CodeIdempotencyConflict, "personal attachment reservation size changed")
			}
			if existingState != "reserved" {
				return domain.NewError(domain.CodeTransferExpired, "personal attachment quota reservation is no longer active")
			}
			if uploadReservationID != "" && uploadReservationID != existingID {
				return domain.NewError(domain.CodeInternal, "personal attachment reservation identity is inconsistent")
			}
			if err := persistPersonalAttachmentReservationID(ctx, tx, instanceID, principal.UserID, uploadID, existingID); err != nil {
				return err
			}
			reservationID = existingID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check personal attachment quota replay: %w", err)
		}
		if uploadReservationID != "" {
			return domain.NewError(domain.CodeInternal, "personal attachment quota reservation is missing")
		}
		if sizeBytes > quota.MaxAttachmentBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "personal attachment exceeds the account attachment limit")
		}
		projected, ok := addPersonalQuotaBytes(quota.UsedBytes, quota.ReservedBytes)
		if !ok || projected > math.MaxInt64-sizeBytes || projected+sizeBytes > quota.QuotaBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "personal data quota exceeded")
		}
		reservationID, err = newPersonalDataQuotaReservationID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO personal_data_quota_reservations
(service_instance_id,owner_id,reservation_id,upload_id,size_bytes,state,expires_at)
VALUES ($1::uuid,$2,$3,$4,$5,'reserved',$6)`,
			instanceID, principal.UserID, reservationID, uploadID, sizeBytes, uploadExpiresAt); err != nil {
			return fmt.Errorf("create personal data quota reservation: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE personal_data_quotas
SET reserved_bytes=reserved_bytes+$3,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, principal.UserID, sizeBytes); err != nil {
			return fmt.Errorf("increase personal data reserved bytes: %w", err)
		}
		if err := persistPersonalAttachmentReservationID(ctx, tx, instanceID, principal.UserID, uploadID, reservationID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return reservationID, nil
}

func (s *Store) ReleasePersonalAttachmentReservation(ctx context.Context, ownerID, reservationID string) error {
	if !domain.ValidUserID(ownerID) || !validPersonalDataQuotaReservationID(reservationID) {
		return domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		var uploadID string
		var sizeBytes int64
		err = tx.QueryRow(ctx, `SELECT upload_id,size_bytes FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3`, instanceID, ownerID, reservationID).
			Scan(&uploadID, &sizeBytes)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read personal data quota reservation: %w", err)
		}
		if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID); err != nil {
			return err
		}
		quota, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID)
		if err != nil {
			return err
		}
		var currentUploadID string
		var currentSize int64
		var state string
		err = tx.QueryRow(ctx, `SELECT upload_id,size_bytes,state FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 FOR UPDATE`, instanceID, ownerID, reservationID).
			Scan(&currentUploadID, &currentSize, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock personal data quota reservation: %w", err)
		}
		if currentUploadID != uploadID || currentSize != sizeBytes {
			return domain.NewError(domain.CodeInternal, "personal data quota reservation changed")
		}
		switch state {
		case "released":
			return nil
		case "committed":
			return domain.NewError(domain.CodeInvalidState, "committed personal attachment reservation cannot be released")
		case "reserved":
		default:
			return domain.NewError(domain.CodeInternal, "personal data quota reservation state is invalid")
		}
		if sizeBytes < 0 || quota.ReservedBytes < sizeBytes {
			return domain.NewError(domain.CodeInternal, "personal data reserved byte count is inconsistent")
		}
		if _, err := tx.Exec(ctx, `UPDATE personal_data_quotas
SET reserved_bytes=reserved_bytes-$3,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID, sizeBytes); err != nil {
			return fmt.Errorf("release personal data reserved bytes: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE personal_data_quota_reservations
SET state='released',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 AND state='reserved'`,
			instanceID, ownerID, reservationID); err != nil {
			return fmt.Errorf("mark personal data quota reservation released: %w", err)
		}
		return nil
	})
}

func (s *Store) CleanupExpiredPersonalDataQuotaReservations(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, domain.InvalidArgument("limit", "personal data quota cleanup request is invalid")
	}
	instanceID, err := s.ServiceInstanceID(ctx)
	if err != nil {
		return 0, err
	}
	var released int32
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT owner_id,reservation_id,upload_id,size_bytes
FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND state='reserved' AND expires_at <= $2
ORDER BY expires_at,owner_id,upload_id LIMIT $3`, instanceID, now.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select expired personal data quota reservations: %w", err)
		}
		type candidate struct {
			ownerID, reservationID, uploadID string
			sizeBytes                        int64
		}
		candidates := make([]candidate, 0, limit)
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.ownerID, &item.reservationID, &item.uploadID, &item.sizeBytes); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired personal data quota reservation: %w", err)
			}
			candidates = append(candidates, item)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("read expired personal data quota reservations: %w", rowsErr)
		}
		for _, item := range candidates {
			if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, item.ownerID); err != nil {
				return err
			}
			quota, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, item.ownerID)
			if err != nil {
				return err
			}
			var currentUploadID, state string
			var currentSize int64
			var expiresAt time.Time
			err = tx.QueryRow(ctx, `SELECT upload_id,size_bytes,state,expires_at FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 FOR UPDATE`,
				instanceID, item.ownerID, item.reservationID).Scan(&currentUploadID, &currentSize, &state, &expiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("lock expired personal data quota reservation: %w", err)
			}
			if state != "reserved" || expiresAt.After(now.UTC()) {
				continue
			}
			if currentUploadID != item.uploadID || currentSize != item.sizeBytes || currentSize < 0 || quota.ReservedBytes < currentSize {
				return domain.NewError(domain.CodeInternal, "expired personal data quota reservation is inconsistent")
			}
			if _, err := tx.Exec(ctx, `UPDATE personal_data_quotas
SET reserved_bytes=reserved_bytes-$3,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, item.ownerID, currentSize); err != nil {
				return fmt.Errorf("expire personal data quota reservation bytes: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE personal_data_quota_reservations
SET state='released',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 AND state='reserved'`,
				instanceID, item.ownerID, item.reservationID); err != nil {
				return fmt.Errorf("mark expired personal data quota reservation released: %w", err)
			}
			released++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// applyPersonalObjectUsageDeltaInTx accounts UTF-8 octets of a canonical JSONB
// text representation supplied by the caller. It is deliberately transaction-
// scoped so a personal object or schedule change and its usage delta commit together.
func applyPersonalObjectUsageDeltaInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, delta int64) error {
	if !domain.ValidUserID(ownerID) {
		return domain.InvalidArgument("owner_id", "personal data quota owner is invalid")
	}
	if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID); err != nil {
		return err
	}
	quota, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID)
	if err != nil {
		return err
	}
	newUsed, ok := addPersonalQuotaBytes(quota.UsedBytes, delta)
	if !ok || newUsed < 0 {
		return domain.NewError(domain.CodeInternal, "personal data usage delta would underflow or overflow")
	}
	if delta > 0 {
		projected, ok := addPersonalQuotaBytes(newUsed, quota.ReservedBytes)
		if !ok || projected > quota.QuotaBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "personal data quota exceeded")
		}
	}
	if delta == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE personal_data_quotas
SET used_bytes=$3,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID, newUsed); err != nil {
		return fmt.Errorf("apply personal data usage delta: %w", err)
	}
	return nil
}

// commitPersonalAttachmentQuotaReservationInTx settles an attachment reservation
// inside its caller's business transaction. It uses current account and per-file
// limits, so a limit reduction after Reserve is honored at commit time.
func commitPersonalAttachmentQuotaReservationInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID, reservationID, uploadID string, sizeBytes int64) error {
	if !domain.ValidUserID(ownerID) || !validPersonalDataQuotaReservationID(reservationID) ||
		!personal.ValidPersonalAttachmentUploadID(uploadID) || sizeBytes < 0 || sizeBytes > personal.MaxAttachmentBytes {
		return domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	var uploadSize int64
	var uploadState string
	var uploadReservation string
	var uploadReleased bool
	var uploadExpiresAt time.Time
	err := tx.QueryRow(ctx, `SELECT size_bytes,state,COALESCE(reservation_id,''),reservation_released,expires_at
FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 FOR UPDATE`,
		instanceID, ownerID, uploadID).Scan(&uploadSize, &uploadState, &uploadReservation, &uploadReleased, &uploadExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
	}
	if err != nil {
		return fmt.Errorf("lock personal attachment quota upload: %w", err)
	}
	if uploadSize != sizeBytes || uploadReservation != reservationID || uploadReleased {
		return domain.NewError(domain.CodeInvalidState, "personal attachment quota reservation does not match the upload")
	}
	if uploadState == string(personal.AttachmentUploadCommitted) {
		var committedSize int64
		var committedUpload string
		var committedState string
		if err := tx.QueryRow(ctx, `SELECT upload_id,size_bytes,state FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3`, instanceID, ownerID, reservationID).
			Scan(&committedUpload, &committedSize, &committedState); err != nil {
			return fmt.Errorf("read committed personal data quota reservation: %w", err)
		}
		if committedUpload == uploadID && committedSize == sizeBytes && committedState == "committed" {
			return nil
		}
		return domain.NewError(domain.CodeInternal, "committed personal attachment quota reservation is inconsistent")
	}
	if uploadState != string(personal.AttachmentUploadActive) || !uploadExpiresAt.After(time.Now().UTC()) {
		return domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer active")
	}
	if err := ensurePersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID); err != nil {
		return err
	}
	quota, err := lockPersonalDataQuotaRowInTx(ctx, tx, instanceID, ownerID)
	if err != nil {
		return err
	}
	var reservationUpload string
	var reservationSize int64
	var reservationState string
	err = tx.QueryRow(ctx, `SELECT upload_id,size_bytes,state FROM personal_data_quota_reservations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 FOR UPDATE`, instanceID, ownerID, reservationID).
		Scan(&reservationUpload, &reservationSize, &reservationState)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeInvalidState, "personal attachment quota reservation is missing")
	}
	if err != nil {
		return fmt.Errorf("lock personal data quota reservation for commit: %w", err)
	}
	if reservationUpload != uploadID || reservationSize != sizeBytes || reservationState != "reserved" {
		return domain.NewError(domain.CodeInvalidState, "personal attachment quota reservation is not active")
	}
	if sizeBytes > quota.MaxAttachmentBytes {
		return domain.NewError(domain.CodeQuotaExceeded, "personal attachment exceeds the account attachment limit")
	}
	projected, ok := addPersonalQuotaBytes(quota.UsedBytes, quota.ReservedBytes)
	if !ok || projected > quota.QuotaBytes || quota.ReservedBytes < sizeBytes || quota.UsedBytes > math.MaxInt64-sizeBytes {
		return domain.NewError(domain.CodeQuotaExceeded, "personal data quota exceeded")
	}
	newUsed := quota.UsedBytes + sizeBytes
	newReserved := quota.ReservedBytes - sizeBytes
	if _, err := tx.Exec(ctx, `UPDATE personal_data_quotas
SET used_bytes=$3,reserved_bytes=$4,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID, newUsed, newReserved); err != nil {
		return fmt.Errorf("settle personal data attachment quota bytes: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE personal_data_quota_reservations
SET state='committed',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND reservation_id=$3 AND state='reserved'`,
		instanceID, ownerID, reservationID); err != nil {
		return fmt.Errorf("mark personal data attachment quota committed: %w", err)
	}
	return nil
}

func lockPersonalDataQuotaOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "user not found")
	}
	if err != nil {
		return fmt.Errorf("lock personal data quota owner: %w", err)
	}
	return nil
}

func loadPersonalDataQuota(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) (app.PersonalDataQuota, error) {
	var value app.PersonalDataQuota
	err := tx.QueryRow(ctx, `SELECT u.id,COALESCE(q.used_bytes,0),COALESCE(q.reserved_bytes,0),
COALESCE(q.quota_bytes,$3),COALESCE(q.max_attachment_bytes,$4),COALESCE(q.version,1)
FROM users u LEFT JOIN personal_data_quotas q
ON q.service_instance_id=$1::uuid AND q.owner_id=u.id WHERE u.id=$2`,
		instanceID, ownerID, app.DefaultPersonalDataQuotaBytes, app.DefaultPersonalDataMaxAttachmentBytes).Scan(
		&value.OwnerID, &value.UsedBytes, &value.ReservedBytes, &value.QuotaBytes, &value.MaxAttachmentBytes, &value.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.PersonalDataQuota{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	if err != nil {
		return app.PersonalDataQuota{}, fmt.Errorf("load personal data quota: %w", err)
	}
	return value, nil
}

func ensurePersonalDataQuotaRowInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO personal_data_quotas (service_instance_id,owner_id)
SELECT $1::uuid,id FROM users WHERE id=$2 ON CONFLICT (service_instance_id,owner_id) DO NOTHING`, instanceID, ownerID)
	if err != nil {
		return fmt.Errorf("ensure personal data quota row: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, ownerID).Scan(&exists); err != nil {
			return fmt.Errorf("check personal data quota owner: %w", err)
		}
		if !exists {
			return domain.NewError(domain.CodeNotFound, "user not found")
		}
	}
	return nil
}

func lockPersonalDataQuotaRowInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) (app.PersonalDataQuota, error) {
	var value app.PersonalDataQuota
	err := tx.QueryRow(ctx, `SELECT owner_id,used_bytes,reserved_bytes,quota_bytes,max_attachment_bytes,version
FROM personal_data_quotas WHERE service_instance_id=$1::uuid AND owner_id=$2 FOR UPDATE`, instanceID, ownerID).Scan(
		&value.OwnerID, &value.UsedBytes, &value.ReservedBytes, &value.QuotaBytes, &value.MaxAttachmentBytes, &value.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.PersonalDataQuota{}, domain.NewError(domain.CodeNotFound, "personal data quota not found")
	}
	if err != nil {
		return app.PersonalDataQuota{}, fmt.Errorf("lock personal data quota: %w", err)
	}
	if value.UsedBytes < 0 || value.ReservedBytes < 0 || value.QuotaBytes < 1 || value.MaxAttachmentBytes < 1 || value.Version < 1 {
		return app.PersonalDataQuota{}, domain.NewError(domain.CodeInternal, "personal data quota row is invalid")
	}
	return value, nil
}

func persistPersonalAttachmentReservationID(ctx context.Context, tx pgx.Tx, instanceID, ownerID, uploadID, reservationID string) error {
	tag, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads SET reservation_id=$4,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND state='pending'
AND reservation_released=false AND (reservation_id IS NULL OR reservation_id=$4)`,
		instanceID, ownerID, uploadID, reservationID)
	if err != nil {
		return fmt.Errorf("persist personal attachment reservation identity: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "personal attachment upload changed before quota reservation")
	}
	return nil
}

func addPersonalQuotaBytes(left, right int64) (int64, bool) {
	if (right > 0 && left > math.MaxInt64-right) || (right < 0 && left < math.MinInt64-right) {
		return 0, false
	}
	return left + right, true
}

func newPersonalDataQuotaReservationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate personal data quota reservation id: %w", err)
	}
	return personalDataQuotaReservationIDPrefix + hex.EncodeToString(random[:]), nil
}

func validPersonalDataQuotaReservationID(value string) bool {
	if len(value) != len(personalDataQuotaReservationIDPrefix)+32 ||
		value[:len(personalDataQuotaReservationIDPrefix)] != personalDataQuotaReservationIDPrefix || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value[len(personalDataQuotaReservationIDPrefix):])
	return err == nil
}

var _ app.PersonalDataQuotaStore = (*Store)(nil)
