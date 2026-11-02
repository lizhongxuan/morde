package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

const maxPendingRecordCapturesPerOwner int64 = 100

func (s *Store) EnqueueRecordCapture(ctx context.Context, principal identity.Principal, input record.CaptureInput) (record.QueueItem, error) {
	return s.enqueueRecordCapture(ctx, principal, input, 0)
}

func (s *Store) EnqueueRecordCaptureAtConsentVersion(ctx context.Context, principal identity.Principal, input record.CaptureInput,
	expectedConsentVersion int64) (record.QueueItem, error) {
	if expectedConsentVersion < 1 {
		return record.QueueItem{}, domain.InvalidArgument("consent_version", "record consent version is invalid")
	}
	if err := record.ValidateCapture(input); err != nil {
		if domain.ValidTaskID(input.TaskID) && input.Purpose.Valid() && input.Kind.Valid() &&
			((input.Purpose == record.PurposeFullRecord && input.Kind == record.ItemTurn) ||
				(input.Purpose == record.PurposeTrainingContribution && input.Kind == record.ItemSample)) {
			item, rejectErr := s.RejectRecordCaptureAtConsentVersion(ctx, principal, input.TaskID, input.Purpose, input.Kind, expectedConsentVersion)
			return item, errors.Join(err, rejectErr)
		}
		return record.QueueItem{}, err
	}
	return s.enqueueRecordCapture(ctx, principal, input, expectedConsentVersion)
}

func (s *Store) enqueueRecordCapture(ctx context.Context, principal identity.Principal, input record.CaptureInput,
	expectedConsentVersion int64) (record.QueueItem, error) {
	if err := record.ValidateCapture(input); err != nil {
		return record.QueueItem{}, err
	}
	queueID, err := record.NewQueueItemID()
	if err != nil {
		return record.QueueItem{}, err
	}
	attachmentIDs := make([]string, len(input.Attachments))
	for index := range input.Attachments {
		attachmentIDs[index], err = record.NewAttachmentID()
		if err != nil {
			return record.QueueItem{}, err
		}
	}
	totalBytes := int64(len(input.Payload))
	for _, attachment := range input.Attachments {
		totalBytes += int64(len(attachment.Content))
	}
	var item record.QueueItem
	var enqueueFailure error
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return err
		}
		if input.TaskID != "" {
			if existing, found, err := recordCaptureForTaskTx(ctx, tx, principal.UserID, input.TaskID, input.Purpose); err != nil {
				return err
			} else if found {
				item = existing
				return nil
			}
		}
		var enabled bool
		var consentVersion int64
		err := tx.QueryRow(ctx, `SELECT enabled,version FROM record_consents
WHERE owner_id=$1 AND purpose=$2 FOR SHARE`, principal.UserID, string(input.Purpose)).Scan(&enabled, &consentVersion)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!enabled ||
			(expectedConsentVersion != 0 && consentVersion != expectedConsentVersion))) {
			item = record.QueueItem{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read record consent for capture: %w", err)
		}
		if err := ensureRecordQuotaTx(ctx, tx, principal.UserID); err != nil {
			return err
		}
		var retentionDays int32
		var limitBytes, usedBytes, reservedBytes int64
		if err := tx.QueryRow(ctx, `SELECT retention_days,limit_bytes,used_bytes,reserved_bytes
FROM record_quotas WHERE owner_id=$1 FOR UPDATE`, principal.UserID).Scan(&retentionDays, &limitBytes, &usedBytes, &reservedBytes); err != nil {
			return fmt.Errorf("lock record quota for capture: %w", err)
		}
		if input.TaskID != "" {
			if existing, found, err := recordCaptureForTaskTx(ctx, tx, principal.UserID, input.TaskID, input.Purpose); err != nil {
				return err
			} else if found {
				item = existing
				return nil
			}
		}
		capturedAt := time.Now().UTC()
		expiresAt := capturedAt.AddDate(0, 0, int(retentionDays))
		var installationID any
		if principal.Kind == identity.KindClient {
			installationID = principal.ClientInstallationID
		}
		var pendingCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM record_capture_queue
WHERE owner_id=$1 AND state IN ('queued','leased')`, principal.UserID).Scan(&pendingCount); err != nil {
			return fmt.Errorf("count pending record captures: %w", err)
		}
		if pendingCount >= maxPendingRecordCapturesPerOwner {
			enqueueFailure = domain.NewError(domain.CodeQuotaExceeded, "optional record capture queue is full")
			if input.TaskID == "" {
				return enqueueFailure
			}
			return insertFailedRecordCaptureTx(ctx, tx, &item, queueID, principal, input.TaskID, input.Purpose,
				input.Kind, consentVersion, installationID, capturedAt, expiresAt, "queue_full")
		}
		if usedBytes > limitBytes || reservedBytes > limitBytes-usedBytes || totalBytes > limitBytes-usedBytes-reservedBytes {
			enqueueFailure = domain.NewError(domain.CodeQuotaExceeded, "optional record capture quota is full")
			if input.TaskID == "" {
				return enqueueFailure
			}
			return insertFailedRecordCaptureTx(ctx, tx, &item, queueID, principal, input.TaskID, input.Purpose,
				input.Kind, consentVersion, installationID, capturedAt, expiresAt, "quota_exceeded")
		}
		_, err = tx.Exec(ctx, `INSERT INTO record_capture_queue
  (id,owner_id,purpose,item_kind,source_task_id,consent_version,captured_by_installation,captured_at,expires_at,payload,reserved_bytes,state)
VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,'queued')`, queueID, principal.UserID, string(input.Purpose),
			string(input.Kind), input.TaskID, consentVersion, installationID, capturedAt, expiresAt, input.Payload, totalBytes)
		if err != nil {
			return fmt.Errorf("enqueue optional record capture: %w", err)
		}
		for index, attachment := range input.Attachments {
			digest := sha256.Sum256(attachment.Content)
			if _, err := tx.Exec(ctx, `INSERT INTO record_capture_queue_attachments
  (id,queue_item_id,file_name,media_type,content,size_bytes,sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7)`, attachmentIDs[index], queueID, attachment.FileName,
				attachment.MediaType, attachment.Content, len(attachment.Content), hex.EncodeToString(digest[:])); err != nil {
				return fmt.Errorf("save optional record queue attachment: %w", err)
			}
		}
		result, err := tx.Exec(ctx, `UPDATE record_quotas SET reserved_bytes=reserved_bytes+$2,
  updated_at=clock_timestamp() WHERE owner_id=$1`, principal.UserID, totalBytes)
		if err != nil {
			return fmt.Errorf("reserve optional record quota: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeInternal, "record quota reservation is missing")
		}
		item = record.QueueItem{
			ID: queueID, OwnerID: principal.UserID, Purpose: input.Purpose, Kind: input.Kind,
			TaskID:         input.TaskID,
			ConsentVersion: consentVersion, CapturedAt: capturedAt, ExpiresAt: expiresAt,
			ReservedBytes: totalBytes, State: record.QueueQueued,
		}
		if principal.Kind == identity.KindClient {
			item.CapturedByInstallation = principal.ClientInstallationID
		}
		return nil
	})
	if err != nil {
		return record.QueueItem{}, err
	}
	return item, enqueueFailure
}

func (s *Store) RejectRecordCapture(ctx context.Context, principal identity.Principal, taskID string, purpose record.Purpose, kind record.ItemKind) (record.QueueItem, error) {
	return s.rejectRecordCapture(ctx, principal, taskID, purpose, kind, 0)
}

func (s *Store) RejectRecordCaptureAtConsentVersion(ctx context.Context, principal identity.Principal, taskID string,
	purpose record.Purpose, kind record.ItemKind, expectedConsentVersion int64) (record.QueueItem, error) {
	if expectedConsentVersion < 1 {
		return record.QueueItem{}, domain.InvalidArgument("consent_version", "record consent version is invalid")
	}
	return s.rejectRecordCapture(ctx, principal, taskID, purpose, kind, expectedConsentVersion)
}

func (s *Store) rejectRecordCapture(ctx context.Context, principal identity.Principal, taskID string, purpose record.Purpose,
	kind record.ItemKind, expectedConsentVersion int64) (record.QueueItem, error) {
	if !domain.ValidTaskID(taskID) || !purpose.Valid() || !kind.Valid() ||
		(purpose == record.PurposeFullRecord && kind != record.ItemTurn) ||
		(purpose == record.PurposeTrainingContribution && kind != record.ItemSample) {
		return record.QueueItem{}, domain.InvalidArgument("record_capture", "record capture failure identity is invalid")
	}
	queueID, err := record.NewQueueItemID()
	if err != nil {
		return record.QueueItem{}, err
	}
	var item record.QueueItem
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return err
		}
		if existing, found, err := recordCaptureForTaskTx(ctx, tx, principal.UserID, taskID, purpose); err != nil {
			return err
		} else if found {
			item = existing
			return nil
		}
		var enabled bool
		var consentVersion int64
		err := tx.QueryRow(ctx, `SELECT enabled,version FROM record_consents
WHERE owner_id=$1 AND purpose=$2 FOR SHARE`, principal.UserID, string(purpose)).Scan(&enabled, &consentVersion)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!enabled ||
			(expectedConsentVersion != 0 && consentVersion != expectedConsentVersion))) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read record consent for rejected capture: %w", err)
		}
		if err := ensureRecordQuotaTx(ctx, tx, principal.UserID); err != nil {
			return err
		}
		var retentionDays int32
		if err := tx.QueryRow(ctx, `SELECT retention_days FROM record_quotas WHERE owner_id=$1 FOR UPDATE`, principal.UserID).Scan(&retentionDays); err != nil {
			return fmt.Errorf("lock record quota for rejected capture: %w", err)
		}
		if existing, found, err := recordCaptureForTaskTx(ctx, tx, principal.UserID, taskID, purpose); err != nil {
			return err
		} else if found {
			item = existing
			return nil
		}
		capturedAt := time.Now().UTC()
		var installationID any
		if principal.Kind == identity.KindClient {
			installationID = principal.ClientInstallationID
		}
		return insertFailedRecordCaptureTx(ctx, tx, &item, queueID, principal, taskID, purpose, kind,
			consentVersion, installationID, capturedAt, capturedAt.AddDate(0, 0, int(retentionDays)), "content_rejected")
	})
	if err != nil {
		return record.QueueItem{}, err
	}
	return item, nil
}

func recordCaptureForTaskTx(ctx context.Context, tx pgx.Tx, ownerID, taskID string, purpose record.Purpose) (record.QueueItem, bool, error) {
	var item record.QueueItem
	err := tx.QueryRow(ctx, `SELECT id,owner_id,purpose,item_kind,source_task_id,failure_code,consent_version,
COALESCE(captured_by_installation,''),captured_at,expires_at,reserved_bytes,state,COALESCE(lease_owner,''),lease_version,attempts
FROM record_capture_queue WHERE owner_id=$1 AND source_task_id=$2 AND purpose=$3 FOR SHARE`, ownerID, taskID, string(purpose)).Scan(
		&item.ID, &item.OwnerID, &item.Purpose, &item.Kind, &item.TaskID, &item.FailureCode, &item.ConsentVersion,
		&item.CapturedByInstallation, &item.CapturedAt, &item.ExpiresAt, &item.ReservedBytes, &item.State,
		&item.LeaseOwner, &item.LeaseVersion, &item.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return record.QueueItem{}, false, nil
	}
	if err != nil {
		return record.QueueItem{}, false, fmt.Errorf("read prior task record capture: %w", err)
	}
	return item, true, nil
}

func insertFailedRecordCaptureTx(ctx context.Context, tx pgx.Tx, item *record.QueueItem, queueID string, principal identity.Principal,
	taskID string, purpose record.Purpose, kind record.ItemKind, consentVersion int64, installationID any,
	capturedAt, expiresAt time.Time, failureCode string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO record_capture_queue
  (id,owner_id,purpose,item_kind,source_task_id,consent_version,captured_by_installation,captured_at,expires_at,payload,reserved_bytes,state,failure_code)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,decode('','hex'),0,'failed',$10)`, queueID, principal.UserID, string(purpose),
		string(kind), taskID, consentVersion, installationID, capturedAt, expiresAt, failureCode); err != nil {
		return fmt.Errorf("record failed optional capture status: %w", err)
	}
	*item = record.QueueItem{
		ID: queueID, OwnerID: principal.UserID, Purpose: purpose, Kind: kind, TaskID: taskID,
		FailureCode: failureCode, ConsentVersion: consentVersion, CapturedAt: capturedAt,
		ExpiresAt: expiresAt, State: record.QueueFailed,
	}
	if principal.Kind == identity.KindClient {
		item.CapturedByInstallation = principal.ClientInstallationID
	}
	return nil
}

func (s *Store) ListRecordCaptureStatuses(ctx context.Context, principal identity.Principal, query record.CaptureStatusQuery) (record.CaptureStatusPage, error) {
	if query.Limit < 1 || query.Limit > 100 || (query.BeforeID != "" && !record.ValidQueueItemID(query.BeforeID)) {
		return record.CaptureStatusPage{}, domain.InvalidArgument("record_capture_status", "capture status query is invalid")
	}
	page := record.CaptureStatusPage{Items: make([]record.CaptureStatus, 0, query.Limit)}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return err
		}
		var beforeAt *time.Time
		if query.BeforeID != "" {
			var capturedAt time.Time
			if err := tx.QueryRow(ctx, `SELECT captured_at FROM record_capture_queue
WHERE id=$1 AND owner_id=$2 AND source_task_id IS NOT NULL`, query.BeforeID, principal.UserID).Scan(&capturedAt); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeNotFound, "record capture status cursor was not found")
				}
				return fmt.Errorf("read record capture status cursor: %w", err)
			}
			beforeAt = &capturedAt
		}
		rows, err := tx.Query(ctx, `SELECT id,source_task_id,purpose,item_kind,state,failure_code,captured_at,expires_at
FROM record_capture_queue WHERE owner_id=$1 AND source_task_id IS NOT NULL
  AND ($2::timestamptz IS NULL OR (captured_at,id) < ($2,$3))
ORDER BY captured_at DESC,id DESC LIMIT $4`, principal.UserID, beforeAt, query.BeforeID, query.Limit+1)
		if err != nil {
			return fmt.Errorf("list record capture statuses: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item record.CaptureStatus
			if err := rows.Scan(&item.ID, &item.TaskID, &item.Purpose, &item.Kind, &item.State,
				&item.FailureCode, &item.CapturedAt, &item.ExpiresAt); err != nil {
				return fmt.Errorf("scan record capture status: %w", err)
			}
			page.Items = append(page.Items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read record capture statuses: %w", err)
		}
		rows.Close()
		if len(page.Items) > int(query.Limit) {
			page.Items = page.Items[:query.Limit]
			page.NextBefore = page.Items[len(page.Items)-1].ID
		}
		return nil
	})
	if err != nil {
		return record.CaptureStatusPage{}, err
	}
	return page, nil
}

func (s *Store) ClaimRecordCaptures(ctx context.Context, worker string, now time.Time, lease time.Duration, limit int32) ([]record.QueueItem, error) {
	if worker == "" || len(worker) > 128 || now.IsZero() || lease <= 0 || lease > 5*time.Minute || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("record_capture_lease", "record capture lease is invalid")
	}
	items := make([]record.QueueItem, 0, limit)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM record_capture_queue
WHERE state='queued' OR (state='leased' AND lease_expires_at <= $1)
ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT $2`, now, limit)
		if err != nil {
			return fmt.Errorf("select record captures for lease: %w", err)
		}
		ids := make([]string, 0, limit)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan record capture lease candidate: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read record capture lease candidates: %w", err)
		}
		rows.Close()
		for _, id := range ids {
			var item record.QueueItem
			if err := tx.QueryRow(ctx, `UPDATE record_capture_queue SET state='leased',lease_owner=$2,
  lease_expires_at=$3,lease_version=lease_version+1,attempts=attempts+1,updated_at=clock_timestamp()
				WHERE id=$1 RETURNING id,owner_id,purpose,item_kind,consent_version,COALESCE(captured_by_installation,''),
				captured_at,expires_at,reserved_bytes,state,lease_owner,lease_version,attempts`, id, worker, now.Add(lease)).Scan(
				&item.ID, &item.OwnerID, &item.Purpose, &item.Kind, &item.ConsentVersion, &item.CapturedByInstallation,
				&item.CapturedAt, &item.ExpiresAt, &item.ReservedBytes, &item.State, &item.LeaseOwner, &item.LeaseVersion, &item.Attempts); err != nil {
				return fmt.Errorf("lease record capture: %w", err)
			}
			items = append(items, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) CommitRecordCapture(ctx context.Context, queueID, worker string, leaseVersion int64) (record.RecordMetadata, error) {
	if !record.ValidQueueItemID(queueID) || worker == "" || len(worker) > 128 || leaseVersion < 1 {
		return record.RecordMetadata{}, domain.InvalidArgument("record_capture_lease", "record capture lease is invalid")
	}
	var metadata record.RecordMetadata
	var rejected bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, purpose string
		if err := tx.QueryRow(ctx, `SELECT owner_id,purpose FROM record_capture_queue WHERE id=$1`, queueID).Scan(&ownerID, &purpose); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record capture was not found")
			}
			return fmt.Errorf("read record capture identity: %w", err)
		}
		var consentEnabled bool
		var currentVersion int64
		err := tx.QueryRow(ctx, `SELECT enabled,version FROM record_consents
WHERE owner_id=$1 AND purpose=$2 FOR UPDATE`, ownerID, purpose).Scan(&consentEnabled, &currentVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			consentEnabled = false
			currentVersion = 0
		} else if err != nil {
			return fmt.Errorf("lock record capture consent: %w", err)
		}
		var item captureQueueRow
		if err := tx.QueryRow(ctx, `SELECT id,owner_id,purpose,item_kind,consent_version,COALESCE(captured_by_installation,''),
captured_at,expires_at,payload,reserved_bytes,state,COALESCE(lease_owner,''),lease_version,attempts,COALESCE(record_id,'')
FROM record_capture_queue WHERE id=$1 FOR UPDATE`, queueID).Scan(
			&item.ID, &item.OwnerID, &item.Purpose, &item.Kind, &item.ConsentVersion, &item.InstallationID,
			&item.CapturedAt, &item.ExpiresAt, &item.Payload, &item.ReservedBytes, &item.State, &item.LeaseOwner,
			&item.LeaseVersion, &item.Attempts, &item.RecordID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record capture was not found")
			}
			return fmt.Errorf("lock record capture: %w", err)
		}
		if item.State == string(record.QueueStored) && item.RecordID != "" {
			return loadRecordMetadataTx(ctx, tx, ownerID, item.RecordID, &metadata)
		}
		if item.State == string(record.QueueRejected) || item.State == string(record.QueueFailed) {
			rejected = true
			return nil
		}
		if item.State != string(record.QueueLeased) || item.LeaseOwner != worker || item.LeaseVersion != leaseVersion {
			return domain.NewError(domain.CodeConflict, "record capture lease changed")
		}
		if !consentEnabled || currentVersion != item.ConsentVersion || !item.ExpiresAt.After(time.Now()) {
			if err := rejectRecordCaptureTx(ctx, tx, item); err != nil {
				return err
			}
			rejected = true
			return nil
		}
		if err := validateCaptureOwnerBindingTx(ctx, tx, item.OwnerID, item.InstallationID); err != nil {
			if code := domain.CodeOf(err); code != domain.CodeUnauthenticated && code != domain.CodeForbidden {
				return err
			}
			if rejectErr := rejectRecordCaptureTx(ctx, tx, item); rejectErr != nil {
				return rejectErr
			}
			rejected = true
			return nil
		}
		var attachmentBytes int64
		var attachmentCount int32
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes),0),count(*) FROM record_capture_queue_attachments
WHERE queue_item_id=$1`, queueID).Scan(&attachmentBytes, &attachmentCount); err != nil {
			return fmt.Errorf("size record queue attachments: %w", err)
		}
		if int64(len(item.Payload))+attachmentBytes != item.ReservedBytes {
			return domain.NewError(domain.CodeInternal, "record capture reservation does not match its contents")
		}
		recordID, err := record.NewRecordID()
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO record_entries
  (id,owner_id,purpose,item_kind,consent_version,captured_at,expires_at,payload_bytes,attachment_bytes,attachment_count)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
RETURNING id,owner_id,purpose,item_kind,consent_version,created_at,expires_at,payload_bytes,attachment_bytes,attachment_count,state`,
			recordID, item.OwnerID, item.Purpose, item.Kind, item.ConsentVersion, item.CapturedAt, item.ExpiresAt,
			len(item.Payload), attachmentBytes, attachmentCount).Scan(
			&metadata.ID, &metadata.OwnerID, &metadata.Purpose, &metadata.Kind, &metadata.ConsentVersion,
			&metadata.CreatedAt, &metadata.ExpiresAt, &metadata.PayloadBytes, &metadata.AttachmentBytes,
			&metadata.AttachmentCount, &metadata.State); err != nil {
			return fmt.Errorf("create record metadata: %w", err)
		}
		if item.Kind == string(record.ItemTurn) {
			if _, err := tx.Exec(ctx, `INSERT INTO record_turn_bodies (record_id,body) VALUES ($1,$2)`, recordID, item.Payload); err != nil {
				return fmt.Errorf("store record turn body: %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO record_sample_bodies (record_id,body) VALUES ($1,$2)`, recordID, item.Payload); err != nil {
				return fmt.Errorf("store record sample body: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO record_attachments
  (id,record_id,owner_id,file_name,media_type,content,size_bytes,sha256)
SELECT id,$2,$3,file_name,media_type,content,size_bytes,sha256
FROM record_capture_queue_attachments WHERE queue_item_id=$1`, queueID, recordID, item.OwnerID); err != nil {
			return fmt.Errorf("store dedicated record attachments: %w", err)
		}
		result, err := tx.Exec(ctx, `UPDATE record_quotas SET reserved_bytes=reserved_bytes-$2,
  used_bytes=used_bytes+$2,updated_at=clock_timestamp()
WHERE owner_id=$1 AND reserved_bytes >= $2`, item.OwnerID, item.ReservedBytes)
		if err != nil {
			return fmt.Errorf("convert record reservation to usage: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeInternal, "record quota reservation is inconsistent")
		}
		if _, err := tx.Exec(ctx, `UPDATE record_capture_queue SET state='stored',record_id=$2,lease_owner=NULL,
  lease_expires_at=NULL,payload=decode('','hex'),updated_at=clock_timestamp() WHERE id=$1`, queueID, recordID); err != nil {
			return fmt.Errorf("complete record capture queue item: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM record_capture_queue_attachments WHERE queue_item_id=$1`, queueID); err != nil {
			return fmt.Errorf("clear stored record queue attachments: %w", err)
		}
		return nil
	})
	if err != nil {
		return record.RecordMetadata{}, err
	}
	if rejected {
		return record.RecordMetadata{}, domain.NewError(domain.CodeInvalidState, "record capture was rejected after consent or identity changed")
	}
	return metadata, nil
}

func (s *Store) FailRecordCapture(ctx context.Context, queueID, worker string, leaseVersion int64, failureCode string) error {
	if !record.ValidQueueItemID(queueID) || worker == "" || len(worker) > 128 || leaseVersion < 1 || !validRecordFailureCode(failureCode) {
		return domain.InvalidArgument("record_capture_failure", "record capture failure is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, purpose string
		if err := tx.QueryRow(ctx, `SELECT owner_id,purpose FROM record_capture_queue WHERE id=$1`, queueID).Scan(&ownerID, &purpose); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record capture was not found")
			}
			return fmt.Errorf("read record capture identity: %w", err)
		}
		var ignoredVersion int64
		if err := tx.QueryRow(ctx, `SELECT version FROM record_consents WHERE owner_id=$1 AND purpose=$2 FOR UPDATE`, ownerID, purpose).Scan(&ignoredVersion); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock record capture consent before failure: %w", err)
		}
		var state, leaseOwner string
		var currentLeaseVersion int64
		var reservedBytes int64
		if err := tx.QueryRow(ctx, `SELECT state,COALESCE(lease_owner,''),lease_version,reserved_bytes
FROM record_capture_queue WHERE id=$1 FOR UPDATE`, queueID).Scan(&state, &leaseOwner, &currentLeaseVersion, &reservedBytes); err != nil {
			return fmt.Errorf("lock record capture failure: %w", err)
		}
		if state == string(record.QueueFailed) {
			return nil
		}
		if state != string(record.QueueLeased) || leaseOwner != worker || currentLeaseVersion != leaseVersion {
			return domain.NewError(domain.CodeConflict, "record capture lease changed")
		}
		if _, err := tx.Exec(ctx, `UPDATE record_capture_queue SET state='failed',failure_code=$2,lease_owner=NULL,
  lease_expires_at=NULL,payload=decode('','hex'),updated_at=clock_timestamp() WHERE id=$1`, queueID, failureCode); err != nil {
			return fmt.Errorf("fail record capture queue item: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM record_capture_queue_attachments WHERE queue_item_id=$1`, queueID); err != nil {
			return fmt.Errorf("clear failed record queue attachments: %w", err)
		}
		return releaseRecordQuotaTx(ctx, tx, ownerID, reservedBytes)
	})
}

type captureQueueRow struct {
	ID             string
	OwnerID        string
	Purpose        string
	Kind           string
	ConsentVersion int64
	InstallationID string
	CapturedAt     time.Time
	ExpiresAt      time.Time
	Payload        []byte
	ReservedBytes  int64
	State          string
	LeaseOwner     string
	LeaseVersion   int64
	Attempts       int32
	RecordID       string
}

func rejectRecordCaptureTx(ctx context.Context, tx pgx.Tx, item captureQueueRow) error {
	result, err := tx.Exec(ctx, `UPDATE record_capture_queue SET state='rejected',failure_code='capture_invalidated',
lease_owner=NULL,lease_expires_at=NULL,payload=decode('','hex'),updated_at=clock_timestamp() WHERE id=$1 AND state='leased'`, item.ID)
	if err != nil {
		return fmt.Errorf("reject invalid record capture: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "record capture state changed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM record_capture_queue_attachments WHERE queue_item_id=$1`, item.ID); err != nil {
		return fmt.Errorf("clear rejected record queue attachments: %w", err)
	}
	return releaseRecordQuotaTx(ctx, tx, item.OwnerID, item.ReservedBytes)
}

func releaseRecordQuotaTx(ctx context.Context, tx pgx.Tx, ownerID string, reservedBytes int64) error {
	if reservedBytes == 0 {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE record_quotas SET reserved_bytes=reserved_bytes-$2,
  updated_at=clock_timestamp() WHERE owner_id=$1 AND reserved_bytes >= $2`, ownerID, reservedBytes)
	if err != nil {
		return fmt.Errorf("release record quota reservation: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInternal, "record quota reservation is inconsistent")
	}
	return nil
}

func validateCaptureOwnerBindingTx(ctx context.Context, tx pgx.Tx, ownerID, installationID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, ownerID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "record capture owner is no longer active")
		}
		return fmt.Errorf("read record capture owner: %w", err)
	}
	if status != string(identity.StatusApproved) {
		return domain.NewError(domain.CodeForbidden, "record capture owner is not active")
	}
	if installationID == "" {
		return nil
	}
	if !domain.ValidClientInstallationID(installationID) {
		return domain.NewError(domain.CodeUnauthenticated, "record capture installation is no longer active")
	}
	var activeID string
	err := tx.QueryRow(ctx, `SELECT id FROM client_installations
WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > clock_timestamp()) FOR SHARE`, installationID, ownerID).Scan(&activeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "record capture installation is no longer active")
	}
	if err != nil {
		return fmt.Errorf("recheck record capture installation: %w", err)
	}
	return nil
}

func validRecordFailureCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func loadRecordMetadataTx(ctx context.Context, tx pgx.Tx, ownerID, recordID string, value *record.RecordMetadata) error {
	err := tx.QueryRow(ctx, `SELECT id,owner_id,purpose,item_kind,consent_version,created_at,expires_at,
payload_bytes,attachment_bytes,attachment_count,state FROM record_entries WHERE id=$1 AND owner_id=$2`,
		recordID, ownerID).Scan(&value.ID, &value.OwnerID, &value.Purpose, &value.Kind, &value.ConsentVersion,
		&value.CreatedAt, &value.ExpiresAt, &value.PayloadBytes, &value.AttachmentBytes, &value.AttachmentCount, &value.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "record was not found")
	}
	if err != nil {
		return fmt.Errorf("read record metadata: %w", err)
	}
	return nil
}
