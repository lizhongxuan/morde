package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

const personalAttachmentUploadLifetime = 24 * time.Hour

type personalAttachmentUploadRow struct {
	Upload       personal.AttachmentUpload
	State        personal.AttachmentUploadState
	Idempotency  string
	Reservation  string
	Released     bool
	AttachmentID string
	CommitKey    string
	Revision     int64
	ChangeSeq    int64
	ExpiresAt    time.Time
}

func (s *Store) CreatePersonalAttachmentUpload(ctx context.Context, principal identity.Principal, upload personal.AttachmentUpload, idempotencyKey string) (personal.AttachmentUploadReceipt, error) {
	if principal.UserID != upload.OwnerID || (principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) {
		return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeForbidden, "personal attachment owner is not authorized")
	}
	if err := personal.ValidateAttachmentUpload(upload); err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	if err := personal.ValidateAttachmentIdempotencyKey(idempotencyKey); err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	requestHash, err := personal.HashAttachmentUploadContent(upload)
	if err != nil || !bytes.Equal(upload.RequestHash, requestHash) {
		return personal.AttachmentUploadReceipt{}, domain.InvalidArgument("request_hash", "personal attachment upload hash is invalid")
	}
	var receipt personal.AttachmentUploadReceipt
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO personal_attachment_uploads
(service_instance_id,owner_id,upload_id,object_type,object_id,base_revision,size_bytes,sha256,media_type,request_hash,idempotency_key,state,expires_at)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',clock_timestamp()+$12::interval)
ON CONFLICT (service_instance_id,owner_id,idempotency_key) DO NOTHING`,
			instanceID, upload.OwnerID, upload.UploadID, upload.ObjectType, upload.ObjectID, upload.BaseRevision,
			upload.SizeBytes, upload.SHA256, upload.MediaType, upload.RequestHash, idempotencyKey, personalAttachmentUploadLifetime.String())
		if err != nil {
			return fmt.Errorf("create personal attachment upload: %w", err)
		}
		row, err := loadPersonalAttachmentUpload(ctx, tx, instanceID, upload.OwnerID, upload.UploadID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = loadPersonalAttachmentUploadByIdempotencyKey(ctx, tx, instanceID, upload.OwnerID, idempotencyKey)
		}
		if err != nil {
			return fmt.Errorf("read personal attachment upload: %w", err)
		}
		if !bytes.Equal(row.Upload.RequestHash, upload.RequestHash) || row.Upload.OwnerID != upload.OwnerID {
			return domain.NewError(domain.CodeIdempotencyConflict, "upload idempotency key was used for a different request")
		}
		count, err := personal.AttachmentChunkCount(row.Upload.SizeBytes)
		if err != nil {
			return err
		}
		receipt = personal.AttachmentUploadReceipt{
			UploadID: row.Upload.UploadID, ReservationID: row.Reservation, State: row.State,
			ChunkCount: count, Replay: tag.RowsAffected() == 0,
		}
		return nil
	})
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ActivatePersonalAttachmentUpload(ctx context.Context, principal identity.Principal, uploadID, reservationID string) (personal.AttachmentUploadReceipt, error) {
	if !personal.ValidPersonalAttachmentUploadID(uploadID) || personal.ValidateAttachmentIdempotencyKey(reservationID) != nil {
		return personal.AttachmentUploadReceipt{}, domain.InvalidArgument("upload_id", "personal attachment activation is invalid")
	}
	var receipt personal.AttachmentUploadReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		row, err := loadPersonalAttachmentUpload(ctx, tx, instanceID, principal.UserID, uploadID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
		}
		if err != nil {
			return fmt.Errorf("lock personal attachment upload: %w", err)
		}
		switch row.State {
		case personal.AttachmentUploadActive:
			if row.Reservation != reservationID {
				return domain.NewError(domain.CodeIdempotencyConflict, "personal attachment reservation changed")
			}
			receipt.Replay = true
		case personal.AttachmentUploadPending:
			if !row.ExpiresAt.After(time.Now().UTC()) {
				return domain.NewError(domain.CodeTransferExpired, "personal attachment upload expired")
			}
			if _, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads SET state='active',reservation_id=$4,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, principal.UserID, uploadID, reservationID); err != nil {
				return fmt.Errorf("activate personal attachment upload: %w", err)
			}
		case personal.AttachmentUploadCommitted:
			return domain.NewError(domain.CodeInvalidState, "personal attachment upload is already committed")
		default:
			return domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer active")
		}
		count, err := personal.AttachmentChunkCount(row.Upload.SizeBytes)
		if err != nil {
			return err
		}
		receipt.UploadID, receipt.ReservationID, receipt.State, receipt.ChunkCount = uploadID, reservationID, personal.AttachmentUploadActive, count
		return nil
	})
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) PutPersonalAttachmentChunk(ctx context.Context, principal identity.Principal, chunk personal.AttachmentChunk, body io.Reader) (personal.AttachmentChunkReceipt, error) {
	if err := personal.ValidateAttachmentChunk(chunk); err != nil {
		return personal.AttachmentChunkReceipt{}, err
	}
	if body == nil {
		return personal.AttachmentChunkReceipt{}, domain.InvalidArgument("body", "personal attachment chunk body is required")
	}
	content, err := io.ReadAll(io.LimitReader(body, personal.MaxAttachmentChunkBytes+1))
	if err != nil {
		return personal.AttachmentChunkReceipt{}, fmt.Errorf("read personal attachment chunk: %w", err)
	}
	if int64(len(content)) > personal.MaxAttachmentChunkBytes {
		return personal.AttachmentChunkReceipt{}, domain.NewError(domain.CodeRequestTooLarge, "personal attachment chunk exceeds the size limit")
	}
	if int64(len(content)) != chunk.SizeBytes || digestAttachmentBytes(content) != chunk.SHA256 {
		return personal.AttachmentChunkReceipt{}, domain.InvalidArgument("body", "personal attachment chunk size or digest does not match")
	}
	var receipt personal.AttachmentChunkReceipt
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		row, err := loadPersonalAttachmentUpload(ctx, tx, instanceID, principal.UserID, chunk.UploadID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
		}
		if err != nil {
			return fmt.Errorf("lock personal attachment upload: %w", err)
		}
		if row.State != personal.AttachmentUploadActive || !row.ExpiresAt.After(time.Now().UTC()) {
			return domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer active")
		}
		if err := personal.ValidateAttachmentChunkForUpload(row.Upload, chunk); err != nil {
			return err
		}
		var storedSize int64
		var storedDigest string
		err = tx.QueryRow(ctx, `SELECT size_bytes,sha256 FROM personal_attachment_chunks
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND sequence=$4`,
			instanceID, principal.UserID, chunk.UploadID, chunk.Sequence).Scan(&storedSize, &storedDigest)
		if err == nil {
			if storedSize != chunk.SizeBytes || storedDigest != chunk.SHA256 {
				return domain.NewError(domain.CodeIdempotencyConflict, "personal attachment chunk sequence was used for different content")
			}
			receipt.Replay = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check personal attachment chunk replay: %w", err)
		} else if _, err := tx.Exec(ctx, `INSERT INTO personal_attachment_chunks
(service_instance_id,owner_id,upload_id,sequence,size_bytes,sha256,body)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)`, instanceID, principal.UserID, chunk.UploadID,
			chunk.Sequence, chunk.SizeBytes, chunk.SHA256, content); err != nil {
			return fmt.Errorf("store personal attachment chunk: %w", err)
		}
		receipt.UploadID, receipt.Sequence = chunk.UploadID, chunk.Sequence
		return nil
	})
	if err != nil {
		return personal.AttachmentChunkReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) CommitPersonalAttachment(ctx context.Context, principal identity.Principal, uploadID, idempotencyKey string) (personal.AttachmentCommitReceipt, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.AttachmentCommitReceipt{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !personal.ValidPersonalAttachmentUploadID(uploadID) {
		return personal.AttachmentCommitReceipt{}, domain.InvalidArgument("upload_id", "personal attachment upload id is invalid")
	}
	if err := personal.ValidateAttachmentIdempotencyKey(idempotencyKey); err != nil {
		return personal.AttachmentCommitReceipt{}, err
	}
	var receipt personal.AttachmentCommitReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		row, err := loadPersonalAttachmentUpload(ctx, tx, instanceID, principal.UserID, uploadID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
		}
		if err != nil {
			return fmt.Errorf("lock personal attachment upload: %w", err)
		}
		if row.State == personal.AttachmentUploadCommitted {
			if row.CommitKey != idempotencyKey {
				return domain.NewError(domain.CodeIdempotencyConflict, "personal attachment commit key changed")
			}
			if !personal.ValidPersonalAttachmentID(row.AttachmentID) || row.Revision < 1 || row.ChangeSeq < 1 {
				return domain.NewError(domain.CodeInternal, "personal attachment commit receipt is incomplete")
			}
			receipt = personal.AttachmentCommitReceipt{
				AttachmentID: row.AttachmentID, ObjectType: row.Upload.ObjectType, ObjectID: row.Upload.ObjectID,
				ResultRevision: row.Revision, AccountChangeSequence: row.ChangeSeq, Replay: true,
			}
			return nil
		}
		if row.State != personal.AttachmentUploadActive || !row.ExpiresAt.After(time.Now().UTC()) {
			return domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer active")
		}
		if row.Reservation == "" || row.Released {
			return domain.NewError(domain.CodeInvalidState, "personal attachment quota reservation is missing")
		}
		if err := verifyPersonalAttachmentChunks(ctx, tx, instanceID, principal.UserID, row.Upload); err != nil {
			return err
		}
		attachmentID, err := personal.NewPersonalAttachmentID()
		if err != nil {
			return fmt.Errorf("generate personal attachment id: %w", err)
		}
		body, err := personalLibraryItemWithAttachment(ctx, tx, instanceID, row.Upload, attachmentID)
		if err != nil {
			return err
		}
		mutation := personal.Mutation{
			OwnerID: principal.UserID, MutationID: uploadID, ObjectType: row.Upload.ObjectType,
			ObjectID: row.Upload.ObjectID, BaseRevision: row.Upload.BaseRevision, Body: body,
		}
		mutation.RequestHash, err = personal.HashMutationContent(mutation)
		if err != nil {
			return fmt.Errorf("hash personal attachment reference: %w", err)
		}
		mutationReceipt, err := applyPersonalMutationInTx(ctx, tx, principal, mutation, attachmentID)
		if err != nil {
			return err
		}
		if err := commitPersonalAttachmentQuotaReservationInTx(ctx, tx, instanceID, principal.UserID, row.Reservation, uploadID, row.Upload.SizeBytes); err != nil {
			return fmt.Errorf("settle personal attachment quota reservation: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads
SET state='committed',attachment_id=$4,commit_idempotency_key=$5,result_revision=$6,change_sequence=$7,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND state='active'`,
			instanceID, principal.UserID, uploadID, attachmentID, idempotencyKey,
			mutationReceipt.ResultRevision, mutationReceipt.AccountChangeSequence); err != nil {
			return fmt.Errorf("commit personal attachment upload: %w", err)
		}
		receipt = personal.AttachmentCommitReceipt{
			AttachmentID: attachmentID, ObjectType: row.Upload.ObjectType, ObjectID: row.Upload.ObjectID,
			ResultRevision: mutationReceipt.ResultRevision, AccountChangeSequence: mutationReceipt.AccountChangeSequence,
		}
		return nil
	})
	if err != nil {
		return personal.AttachmentCommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) CancelPersonalAttachmentUpload(ctx context.Context, principal identity.Principal, uploadID string) (personal.AttachmentReservation, error) {
	if !personal.ValidPersonalAttachmentUploadID(uploadID) {
		return personal.AttachmentReservation{}, domain.InvalidArgument("upload_id", "personal attachment upload id is invalid")
	}
	var reservation personal.AttachmentReservation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		row, err := loadPersonalAttachmentUpload(ctx, tx, instanceID, principal.UserID, uploadID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
		}
		if err != nil {
			return fmt.Errorf("lock personal attachment upload: %w", err)
		}
		switch row.State {
		case personal.AttachmentUploadCommitted:
			return domain.NewError(domain.CodeInvalidState, "committed personal attachment cannot be cancelled")
		case personal.AttachmentUploadPending, personal.AttachmentUploadActive:
			referenced, err := personalAttachmentReferenced(ctx, tx, instanceID, principal.UserID, row.AttachmentID)
			if err != nil {
				return err
			}
			if referenced {
				return domain.NewError(domain.CodeConflict, "referenced personal attachment cannot be cancelled")
			}
			if _, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads SET state='cancelled',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, principal.UserID, uploadID); err != nil {
				return fmt.Errorf("cancel personal attachment upload: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM personal_attachment_chunks
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, principal.UserID, uploadID); err != nil {
				return fmt.Errorf("delete cancelled personal attachment chunks: %w", err)
			}
		case personal.AttachmentUploadCancelled, personal.AttachmentUploadExpired:
		default:
			return domain.NewError(domain.CodeInternal, "personal attachment upload state is invalid")
		}
		if row.Reservation != "" && !row.Released {
			reservation = personal.AttachmentReservation{OwnerID: principal.UserID, UploadID: uploadID, ReservationID: row.Reservation, SizeBytes: row.Upload.SizeBytes}
		}
		return nil
	})
	if err != nil {
		return personal.AttachmentReservation{}, err
	}
	return reservation, nil
}

func (s *Store) OpenPersonalAttachment(ctx context.Context, principal identity.Principal, attachmentID string) (io.ReadCloser, personal.AttachmentMetadata, error) {
	if (principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) || !personal.ValidPersonalAttachmentID(attachmentID) {
		return nil, personal.AttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "personal attachment not found")
	}
	var uploadID string
	var instanceID string
	var metadata personal.AttachmentMetadata
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		currentInstanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		instanceID = currentInstanceID
		err = tx.QueryRow(ctx, `SELECT upload_id,object_type,object_id,media_type,size_bytes,sha256
FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND attachment_id=$3 AND state='committed'`,
			instanceID, principal.UserID, attachmentID).Scan(&uploadID, &metadata.ObjectType, &metadata.ObjectID, &metadata.MediaType, &metadata.SizeBytes, &metadata.SHA256)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal attachment not found")
		}
		if err != nil {
			return fmt.Errorf("load personal attachment: %w", err)
		}
		metadata.AttachmentID = attachmentID
		readable, err := lockPersonalAttachmentReference(ctx, tx, instanceID, principal.UserID, attachmentID)
		if err != nil {
			return err
		}
		if !readable {
			return domain.NewError(domain.CodeNotFound, "personal attachment not found")
		}
		return nil
	})
	if err != nil {
		return nil, personal.AttachmentMetadata{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT sequence,size_bytes,sha256,body FROM personal_attachment_chunks
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 ORDER BY sequence`, instanceID, principal.UserID, uploadID)
	if err != nil {
		return nil, personal.AttachmentMetadata{}, fmt.Errorf("open personal attachment chunks: %w", err)
	}
	return &personalAttachmentReader{rows: rows, metadata: metadata}, metadata, nil
}

func (s *Store) CleanupExpiredPersonalAttachmentUploads(ctx context.Context, now time.Time, limit int32) ([]personal.AttachmentReservation, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return nil, domain.InvalidArgument("limit", "personal attachment cleanup request is invalid")
	}
	instanceID, err := s.ServiceInstanceID(ctx)
	if err != nil {
		return nil, err
	}
	var reservations []personal.AttachmentReservation
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT owner_id,upload_id,COALESCE(reservation_id,''),state,reservation_released,size_bytes
FROM personal_attachment_uploads u
WHERE service_instance_id=$1::uuid AND (
  (state IN ('pending','active') AND expires_at <= $2 AND NOT EXISTS (
    SELECT 1 FROM personal_objects p WHERE p.service_instance_id=u.service_instance_id AND p.owner_id=u.owner_id
      AND p.object_type=u.object_type AND p.object_id=u.object_id AND p.body->>'attachment_id'=u.attachment_id
  ) AND NOT EXISTS (SELECT 1 FROM conversation_message_attachments r
    WHERE r.service_instance_id=u.service_instance_id AND r.owner_id=u.owner_id AND r.upload_id=u.upload_id))
  OR (state IN ('cancelled','expired') AND reservation_id IS NOT NULL AND NOT reservation_released)
)
ORDER BY expires_at,upload_id LIMIT $3 FOR UPDATE SKIP LOCKED`, instanceID, now.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select expired personal attachment uploads: %w", err)
		}
		type candidate struct {
			ownerID, uploadID, reservationID string
			state                            personal.AttachmentUploadState
			released                         bool
			sizeBytes                        int64
		}
		candidates := make([]candidate, 0, limit)
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.ownerID, &item.uploadID, &item.reservationID, &item.state, &item.released, &item.sizeBytes); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired personal attachment upload: %w", err)
			}
			candidates = append(candidates, item)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("read expired personal attachment uploads: %w", rowsErr)
		}
		for _, item := range candidates {
			if item.state == personal.AttachmentUploadPending || item.state == personal.AttachmentUploadActive {
				if _, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads SET state='expired',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, item.ownerID, item.uploadID); err != nil {
					return fmt.Errorf("expire personal attachment upload: %w", err)
				}
				if _, err := tx.Exec(ctx, `DELETE FROM personal_attachment_chunks WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, item.ownerID, item.uploadID); err != nil {
					return fmt.Errorf("delete expired personal attachment chunks: %w", err)
				}
			}
			if item.reservationID != "" && !item.released {
				reservations = append(reservations, personal.AttachmentReservation{
					OwnerID: item.ownerID, UploadID: item.uploadID, ReservationID: item.reservationID, SizeBytes: item.sizeBytes,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservations, nil
}

func (s *Store) AcknowledgePersonalAttachmentReservationReleased(ctx context.Context, reservation personal.AttachmentReservation) error {
	if err := personal.ValidateAttachmentReservation(reservation); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE personal_attachment_uploads SET reservation_released=true,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND reservation_id=$4
AND state IN ('cancelled','expired') AND reservation_released=false`,
			instanceID, reservation.OwnerID, reservation.UploadID, reservation.ReservationID)
		if err != nil {
			return fmt.Errorf("acknowledge released personal attachment reservation: %w", err)
		}
		if result.RowsAffected() == 0 {
			var released bool
			err := tx.QueryRow(ctx, `SELECT reservation_released FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND reservation_id=$4
AND state IN ('cancelled','expired')`, instanceID, reservation.OwnerID, reservation.UploadID, reservation.ReservationID).Scan(&released)
			if err == nil && released {
				return nil
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "personal attachment reservation not found")
			}
			if err != nil {
				return fmt.Errorf("read released personal attachment reservation: %w", err)
			}
			return domain.NewError(domain.CodeConflict, "personal attachment reservation is not awaiting release")
		}
		return nil
	})
}

func loadPersonalAttachmentUpload(ctx context.Context, tx pgx.Tx, instanceID, ownerID, uploadID string, lock bool) (personalAttachmentUploadRow, error) {
	query := `SELECT upload_id,object_type,object_id,base_revision,size_bytes,sha256,media_type,request_hash,
state,idempotency_key,COALESCE(reservation_id,''),reservation_released,COALESCE(attachment_id,''),
COALESCE(commit_idempotency_key,''),COALESCE(result_revision,0),COALESCE(change_sequence,0),expires_at
FROM personal_attachment_uploads WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`
	if lock {
		query += " FOR UPDATE"
	}
	var row personalAttachmentUploadRow
	var state string
	err := tx.QueryRow(ctx, query, instanceID, ownerID, uploadID).Scan(
		&row.Upload.UploadID, &row.Upload.ObjectType, &row.Upload.ObjectID, &row.Upload.BaseRevision,
		&row.Upload.SizeBytes, &row.Upload.SHA256, &row.Upload.MediaType, &row.Upload.RequestHash,
		&state, &row.Idempotency, &row.Reservation, &row.Released, &row.AttachmentID,
		&row.CommitKey, &row.Revision, &row.ChangeSeq, &row.ExpiresAt)
	if err != nil {
		return personalAttachmentUploadRow{}, err
	}
	row.Upload.OwnerID, row.State = ownerID, personal.AttachmentUploadState(state)
	if err := personal.ValidateAttachmentUpload(row.Upload); err != nil || !personal.ValidAttachmentUploadState(row.State) {
		return personalAttachmentUploadRow{}, domain.NewError(domain.CodeInternal, "personal attachment upload row is invalid")
	}
	return row, nil
}

func loadPersonalAttachmentUploadByIdempotencyKey(ctx context.Context, tx pgx.Tx, instanceID, ownerID, idempotencyKey string) (personalAttachmentUploadRow, error) {
	var uploadID string
	err := tx.QueryRow(ctx, `SELECT upload_id FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND idempotency_key=$3`, instanceID, ownerID, idempotencyKey).Scan(&uploadID)
	if err != nil {
		return personalAttachmentUploadRow{}, err
	}
	return loadPersonalAttachmentUpload(ctx, tx, instanceID, ownerID, uploadID, true)
}

func verifyPersonalAttachmentChunks(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, upload personal.AttachmentUpload) error {
	rows, err := tx.Query(ctx, `SELECT sequence,size_bytes,sha256,body FROM personal_attachment_chunks
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 ORDER BY sequence`, instanceID, ownerID, upload.UploadID)
	if err != nil {
		return fmt.Errorf("read personal attachment chunks for commit: %w", err)
	}
	defer rows.Close()
	wholeDigest := sha256.New()
	var total int64
	var sequence int32
	for rows.Next() {
		var actualSequence int32
		var sizeBytes int64
		var digest string
		var body []byte
		if err := rows.Scan(&actualSequence, &sizeBytes, &digest, &body); err != nil {
			return fmt.Errorf("scan personal attachment chunk for commit: %w", err)
		}
		expectedSize, sizeErr := personal.ExpectedAttachmentChunkSize(upload.SizeBytes, sequence)
		if sizeErr != nil || actualSequence != sequence || sizeBytes != expectedSize || int64(len(body)) != sizeBytes || digestAttachmentBytes(body) != digest {
			return domain.NewError(domain.CodeInvalidState, "personal attachment chunks are incomplete or corrupted")
		}
		_, _ = wholeDigest.Write(body)
		total += sizeBytes
		sequence++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read personal attachment chunk rows: %w", err)
	}
	chunkCount, err := personal.AttachmentChunkCount(upload.SizeBytes)
	if err != nil {
		return err
	}
	if sequence != chunkCount || total != upload.SizeBytes || hex.EncodeToString(wholeDigest.Sum(nil)) != upload.SHA256 {
		return domain.NewError(domain.CodeConflict, "personal attachment chunks do not match the upload plan")
	}
	return nil
}

func personalLibraryItemWithAttachment(ctx context.Context, tx pgx.Tx, instanceID string, upload personal.AttachmentUpload, attachmentID string) (json.RawMessage, error) {
	var body []byte
	err := tx.QueryRow(ctx, `SELECT p.body FROM personal_objects p
JOIN account_object_versions v ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id
 AND v.object_type=p.object_type AND v.object_id=p.object_id
WHERE p.service_instance_id=$1::uuid AND p.owner_id=$2 AND p.object_type=$3 AND p.object_id=$4
  AND p.body IS NOT NULL AND v.is_deleted=false`,
		instanceID, upload.OwnerID, upload.ObjectType, upload.ObjectID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NewError(domain.CodeNotFound, "personal library item not found")
	}
	if err != nil {
		return nil, fmt.Errorf("lock personal library item: %w", err)
	}
	if err := personal.ValidateObjectBody(upload.ObjectType, upload.ObjectID, body); err != nil {
		return nil, domain.NewError(domain.CodeInternal, "personal library item is invalid")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, domain.NewError(domain.CodeInternal, "personal library item is invalid")
	}
	var existingAttachmentID string
	if raw := item["attachment_id"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &existingAttachmentID); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "personal library item attachment is invalid")
		}
	}
	if existingAttachmentID != "" {
		return nil, domain.NewError(domain.CodeConflict, "personal library item already has an attachment")
	}
	for key, value := range map[string]any{
		"attachment_id": attachmentID, "size_bytes": upload.SizeBytes, "mime": upload.MediaType,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode personal attachment library item: %w", err)
		}
		item[key] = encoded
	}
	updated, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("encode personal attachment library item: %w", err)
	}
	if err := personal.ValidateObjectBody(upload.ObjectType, upload.ObjectID, updated); err != nil {
		return nil, domain.NewError(domain.CodeInternal, "personal attachment library item is invalid")
	}
	return updated, nil
}

func personalAttachmentReferenced(ctx context.Context, tx pgx.Tx, instanceID, ownerID, attachmentID string) (bool, error) {
	if attachmentID == "" {
		return false, nil
	}
	var referenced bool
	err := tx.QueryRow(ctx, `SELECT (
  EXISTS (SELECT 1 FROM personal_objects p WHERE p.service_instance_id=$1::uuid AND p.owner_id=$2
    AND p.object_type='library_item' AND p.body IS NOT NULL AND p.body->>'attachment_id'=$3)
  OR EXISTS (SELECT 1 FROM conversation_message_attachments r WHERE r.service_instance_id=$1::uuid
    AND r.owner_id=$2 AND r.attachment_id=$3)
)`, instanceID, ownerID, attachmentID).Scan(&referenced)
	if err != nil {
		return false, fmt.Errorf("check personal attachment references: %w", err)
	}
	return referenced, nil
}

func lockPersonalAttachmentReference(ctx context.Context, tx pgx.Tx, instanceID, ownerID, attachmentID string) (bool, error) {
	var objectID string
	err := tx.QueryRow(ctx, `SELECT object_id FROM personal_objects
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type='library_item'
  AND body IS NOT NULL AND body->>'attachment_id'=$3 FOR SHARE`, instanceID, ownerID, attachmentID).Scan(&objectID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock personal library attachment reference: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT r.message_id FROM conversation_message_attachments r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.attachment_id=$3
ORDER BY r.message_id LIMIT 1 FOR SHARE OF r,m,c`, instanceID, ownerID, attachmentID).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock personal message attachment reference: %w", err)
	}
	return true, nil
}

func digestAttachmentBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

type personalAttachmentReader struct {
	rows     pgx.Rows
	metadata personal.AttachmentMetadata
	chunk    []byte
	offset   int
	sequence int32
	total    int64
	digest   hash.Hash
	err      error
	closed   bool
	closeMu  sync.Mutex
}

func (r *personalAttachmentReader) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	r.closeMu.Lock()
	closed := r.closed
	r.closeMu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.digest == nil {
		r.digest = sha256.New()
	}
	for r.offset == len(r.chunk) {
		if !r.rows.Next() {
			if err := r.rows.Err(); err != nil {
				return 0, r.fail(fmt.Errorf("read personal attachment stream: %w", err))
			}
			count, err := personal.AttachmentChunkCount(r.metadata.SizeBytes)
			if err != nil || r.sequence != count || r.total != r.metadata.SizeBytes || hex.EncodeToString(r.digest.Sum(nil)) != r.metadata.SHA256 {
				return 0, r.fail(domain.NewError(domain.CodeInvalidState, "personal attachment stream is incomplete or corrupted"))
			}
			_ = r.Close()
			return 0, io.EOF
		}
		var sequence int32
		var sizeBytes int64
		var digest string
		if err := r.rows.Scan(&sequence, &sizeBytes, &digest, &r.chunk); err != nil {
			return 0, r.fail(fmt.Errorf("scan personal attachment stream: %w", err))
		}
		expectedSize, sizeErr := personal.ExpectedAttachmentChunkSize(r.metadata.SizeBytes, r.sequence)
		if sizeErr != nil || sequence != r.sequence || sizeBytes != expectedSize || int64(len(r.chunk)) != sizeBytes || digestAttachmentBytes(r.chunk) != digest {
			return 0, r.fail(domain.NewError(domain.CodeInvalidState, "personal attachment stream is incomplete or corrupted"))
		}
		r.offset, r.sequence = 0, r.sequence+1
	}
	n := copy(output, r.chunk[r.offset:])
	_, _ = r.digest.Write(r.chunk[r.offset : r.offset+n])
	r.total += int64(n)
	r.offset += n
	return n, nil
}

func (r *personalAttachmentReader) fail(err error) error {
	r.err = err
	_ = r.Close()
	return err
}

func (r *personalAttachmentReader) Close() error {
	r.closeMu.Lock()
	if r.closed {
		r.closeMu.Unlock()
		return nil
	}
	r.closed = true
	r.closeMu.Unlock()
	r.rows.Close()
	return nil
}

var _ app.PersonalAttachmentStore = (*Store)(nil)
