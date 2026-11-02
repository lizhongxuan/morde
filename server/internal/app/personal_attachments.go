package app

import (
	"context"
	"errors"
	"io"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

const maxPersonalAttachmentCleanupLimit int32 = 1000

type PersonalAttachmentUploadCommand struct {
	IdempotencyKey string
	ObjectID       string
	BaseRevision   int64
	SizeBytes      int64
	SHA256         string
	MediaType      string
}

type PersonalAttachmentChunkCommand struct {
	UploadID  string
	Sequence  int32
	SizeBytes int64
	SHA256    string
	Body      io.Reader
}

type PersonalAttachmentQuota interface {
	// Reserve must atomically verify that the canonical upload remains pending,
	// enforce current personal-data limits, and reserve idempotently by owner and
	// upload ID. This lets cancel find a reservation even if activation failed.
	// Commit settlement is performed by the store adapter in the same transaction
	// as the attachment reference.
	ReservePersonalAttachment(context.Context, identity.Principal, string, int64) (string, error)
	// Release must be idempotent; cancel and expiry cleanup may retry it.
	ReleasePersonalAttachmentReservation(context.Context, string, string) error
}

type PersonalAttachmentStore interface {
	// Create persists or replays a pending session under the stable idempotency key.
	CreatePersonalAttachmentUpload(context.Context, identity.Principal, personal.AttachmentUpload, string) (personal.AttachmentUploadReceipt, error)
	// Activate attaches an idempotent DATA-05 reservation to the canonical pending
	// session. Only active sessions may accept chunks. It must also accept a retry
	// with the same reservation after an interrupted activation.
	ActivatePersonalAttachmentUpload(context.Context, identity.Principal, string, string) (personal.AttachmentUploadReceipt, error)
	// Put must accept active sessions only; stream at most MaxAttachmentChunkBytes,
	// verify the planned size and SHA256, and persist a receipt only after bytes
	// are durable. An identical sequence and digest replays its receipt; different
	// bytes for an existing sequence conflict.
	PutPersonalAttachmentChunk(context.Context, identity.Principal, personal.AttachmentChunk, io.Reader) (personal.AttachmentChunkReceipt, error)
	// Commit must settle the DATA-05 reservation and atomically write the
	// library reference, revision/change event, and idempotency receipt.
	CommitPersonalAttachment(context.Context, identity.Principal, string, string) (personal.AttachmentCommitReceipt, error)
	// Cancel is idempotent and returns any reservation for this upload even if
	// Reserve succeeded but Activate did not. It returns an empty reservation if
	// none was created, so cancellation can safely race the pending state.
	CancelPersonalAttachmentUpload(context.Context, identity.Principal, string) (personal.AttachmentReservation, error)
	OpenPersonalAttachment(context.Context, identity.Principal, string) (io.ReadCloser, personal.AttachmentMetadata, error)
	// Cleanup must lock each candidate, recheck references, remove only
	// unreferenced staging, and return reservations that still need release.
	// Repeating the call after a release failure must return that reservation.
	CleanupExpiredPersonalAttachmentUploads(context.Context, time.Time, int32) ([]personal.AttachmentReservation, error)
	// Acknowledge clears a release-pending reservation after DATA-05 release
	// succeeds. It must be idempotent so a lost response can safely be retried.
	AcknowledgePersonalAttachmentReservationReleased(context.Context, personal.AttachmentReservation) error
}

type PersonalAttachmentService struct {
	store PersonalAttachmentStore
	quota PersonalAttachmentQuota
}

func NewPersonalAttachmentService(store PersonalAttachmentStore, quota PersonalAttachmentQuota) (*PersonalAttachmentService, error) {
	if store == nil || quota == nil {
		return nil, errors.New("personal attachment store and quota are required")
	}
	return &PersonalAttachmentService{store: store, quota: quota}, nil
}

func (s *PersonalAttachmentService) CreatePersonalAttachmentUpload(ctx context.Context, principal identity.Principal, command PersonalAttachmentUploadCommand) (personal.AttachmentUploadReceipt, error) {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	if err := personal.ValidateAttachmentIdempotencyKey(command.IdempotencyKey); err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	uploadID, err := personal.NewPersonalAttachmentUploadID()
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	upload := personal.AttachmentUpload{
		UploadID: uploadID, OwnerID: principal.UserID, ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: command.ObjectID, BaseRevision: command.BaseRevision, SizeBytes: command.SizeBytes,
		SHA256: command.SHA256, MediaType: command.MediaType,
	}
	upload.RequestHash, err = personal.HashAttachmentUploadContent(upload)
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	if err := personal.ValidateAttachmentUpload(upload); err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	created, err := s.store.CreatePersonalAttachmentUpload(ctx, principal, upload, command.IdempotencyKey)
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	if !personal.ValidPersonalAttachmentUploadID(created.UploadID) || created.ChunkCount != attachmentChunkCount(command.SizeBytes) ||
		!personal.ValidAttachmentUploadState(created.State) {
		return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment upload receipt is invalid")
	}
	if created.State == personal.AttachmentUploadCommitted {
		if created.ReservationID != "" && !validAttachmentReservationID(created.ReservationID) {
			return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment reservation is invalid")
		}
		return created, nil
	}
	if created.State == personal.AttachmentUploadActive {
		if !validAttachmentReservationID(created.ReservationID) {
			return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment reservation is missing")
		}
		return created, nil
	}
	if created.State != personal.AttachmentUploadPending {
		return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeTransferExpired, "personal attachment upload is no longer active")
	}
	upload.UploadID = created.UploadID
	reservationID := created.ReservationID
	if reservationID == "" {
		reservationID, err = s.quota.ReservePersonalAttachment(ctx, principal, upload.UploadID, upload.SizeBytes)
		if err != nil {
			return personal.AttachmentUploadReceipt{}, err
		}
	}
	if !validAttachmentReservationID(reservationID) {
		return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment reservation is invalid")
	}
	activated, err := s.store.ActivatePersonalAttachmentUpload(ctx, principal, upload.UploadID, reservationID)
	if err != nil {
		return personal.AttachmentUploadReceipt{}, err
	}
	if activated.UploadID != upload.UploadID || activated.ReservationID != reservationID ||
		activated.State != personal.AttachmentUploadActive || activated.ChunkCount != created.ChunkCount {
		return personal.AttachmentUploadReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment activation receipt is invalid")
	}
	return activated, nil
}

func (s *PersonalAttachmentService) PutPersonalAttachmentChunk(ctx context.Context, principal identity.Principal, command PersonalAttachmentChunkCommand) (personal.AttachmentChunkReceipt, error) {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return personal.AttachmentChunkReceipt{}, err
	}
	chunk := personal.AttachmentChunk{
		UploadID: command.UploadID, Sequence: command.Sequence, SizeBytes: command.SizeBytes, SHA256: command.SHA256,
	}
	if command.Body == nil {
		return personal.AttachmentChunkReceipt{}, domain.InvalidArgument("body", "personal attachment chunk body is required")
	}
	if err := personal.ValidateAttachmentChunk(chunk); err != nil {
		return personal.AttachmentChunkReceipt{}, err
	}
	boundedBody := io.LimitReader(command.Body, personal.MaxAttachmentChunkBytes+1)
	receipt, err := s.store.PutPersonalAttachmentChunk(ctx, principal, chunk, boundedBody)
	if err != nil {
		return personal.AttachmentChunkReceipt{}, err
	}
	if receipt.UploadID != chunk.UploadID || receipt.Sequence != chunk.Sequence {
		return personal.AttachmentChunkReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment chunk receipt is invalid")
	}
	return receipt, nil
}

func (s *PersonalAttachmentService) CommitPersonalAttachment(ctx context.Context, principal identity.Principal, uploadID, idempotencyKey string) (personal.AttachmentCommitReceipt, error) {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return personal.AttachmentCommitReceipt{}, err
	}
	if !personal.ValidPersonalAttachmentUploadID(uploadID) {
		return personal.AttachmentCommitReceipt{}, domain.InvalidArgument("upload_id", "personal attachment upload id is invalid")
	}
	if err := personal.ValidateAttachmentIdempotencyKey(idempotencyKey); err != nil {
		return personal.AttachmentCommitReceipt{}, err
	}
	receipt, err := s.store.CommitPersonalAttachment(ctx, principal, uploadID, idempotencyKey)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeQuotaExceeded {
			reservation, cancelErr := s.store.CancelPersonalAttachmentUpload(ctx, principal, uploadID)
			if cancelErr != nil {
				return personal.AttachmentCommitReceipt{}, errors.Join(err, cancelErr)
			}
			if reservation.ReservationID != "" {
				if releaseErr := s.releaseAndAcknowledgeReservation(ctx, reservation); releaseErr != nil {
					return personal.AttachmentCommitReceipt{}, errors.Join(err, releaseErr)
				}
			}
		}
		return personal.AttachmentCommitReceipt{}, err
	}
	if !personal.ValidPersonalAttachmentID(receipt.AttachmentID) || receipt.ObjectType != personal.ObjectTypeLibraryItem ||
		personal.ValidateObjectIdentifier(receipt.ObjectType, receipt.ObjectID) != nil ||
		receipt.ResultRevision < 1 || receipt.AccountChangeSequence < 1 {
		return personal.AttachmentCommitReceipt{}, domain.NewError(domain.CodeInternal, "personal attachment commit receipt is invalid")
	}
	return receipt, nil
}

func (s *PersonalAttachmentService) CancelPersonalAttachmentUpload(ctx context.Context, principal identity.Principal, uploadID string) error {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return err
	}
	if !personal.ValidPersonalAttachmentUploadID(uploadID) {
		return domain.InvalidArgument("upload_id", "personal attachment upload id is invalid")
	}
	reservation, err := s.store.CancelPersonalAttachmentUpload(ctx, principal, uploadID)
	if err != nil {
		return err
	}
	if reservation.ReservationID == "" {
		return nil
	}
	if err := personal.ValidateAttachmentReservation(reservation); err != nil || reservation.OwnerID != principal.UserID || reservation.UploadID != uploadID {
		return domain.NewError(domain.CodeInternal, "personal attachment cancellation reservation is invalid")
	}
	return s.releaseAndAcknowledgeReservation(ctx, reservation)
}

func (s *PersonalAttachmentService) OpenPersonalAttachment(ctx context.Context, principal identity.Principal, attachmentID string) (io.ReadCloser, personal.AttachmentMetadata, error) {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return nil, personal.AttachmentMetadata{}, err
	}
	if !personal.ValidPersonalAttachmentID(attachmentID) {
		return nil, personal.AttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "personal attachment not found")
	}
	reader, metadata, err := s.store.OpenPersonalAttachment(ctx, principal, attachmentID)
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, personal.AttachmentMetadata{}, err
	}
	if reader == nil || metadata.AttachmentID != attachmentID || personal.ValidateAttachmentMetadata(metadata) != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, personal.AttachmentMetadata{}, domain.NewError(domain.CodeInternal, "personal attachment metadata is invalid")
	}
	return reader, metadata, nil
}

func (s *PersonalAttachmentService) CleanupExpiredPersonalAttachmentUploads(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if ctx == nil || now.IsZero() || limit < 1 || limit > maxPersonalAttachmentCleanupLimit {
		return 0, domain.InvalidArgument("limit", "personal attachment cleanup request is invalid")
	}
	reservations, err := s.store.CleanupExpiredPersonalAttachmentUploads(ctx, now.UTC(), limit)
	if err != nil {
		return 0, err
	}
	if int32(len(reservations)) > limit {
		return 0, domain.NewError(domain.CodeInternal, "personal attachment cleanup returned too many reservations")
	}
	var released int32
	for _, reservation := range reservations {
		if err := personal.ValidateAttachmentReservation(reservation); err != nil {
			return released, domain.NewError(domain.CodeInternal, "personal attachment cleanup reservation is invalid")
		}
		if err := s.releaseAndAcknowledgeReservation(ctx, reservation); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

func (s *PersonalAttachmentService) releaseAndAcknowledgeReservation(ctx context.Context, reservation personal.AttachmentReservation) error {
	if err := s.quota.ReleasePersonalAttachmentReservation(ctx, reservation.OwnerID, reservation.ReservationID); err != nil {
		return err
	}
	return s.store.AcknowledgePersonalAttachmentReservationReleased(ctx, reservation)
}

func validatePersonalAttachmentPrincipal(principal identity.Principal) error {
	if (principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) || !principal.HasScope(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return validatePrincipal(principal)
}

func attachmentChunkCount(sizeBytes int64) int32 {
	count, _ := personal.AttachmentChunkCount(sizeBytes)
	return count
}

func validAttachmentReservationID(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && validateIdempotencyKey(value) == nil
}
