package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ApplyPersonalMutation(ctx context.Context, principal identity.Principal, mutation personal.Mutation) (personal.MutationReceipt, error) {
	if principal.UserID != mutation.OwnerID {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeForbidden, "personal mutation owner is not authorized")
	}
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err := personal.ValidateMutation(mutation); err != nil {
		return personal.MutationReceipt{}, err
	}
	expectedHash, err := personal.HashMutationContent(mutation)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	if len(mutation.RequestHash) != 32 || !bytes.Equal(mutation.RequestHash, expectedHash) {
		return personal.MutationReceipt{}, domain.InvalidArgument("request_hash", "personal mutation hash is invalid")
	}
	var receipt personal.MutationReceipt
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var applyErr error
		receipt, applyErr = applyPersonalMutationInTx(ctx, tx, principal, mutation, "")
		return applyErr
	})
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	return receipt, nil
}

// applyPersonalMutationInTx lets attachment commit combine a library-item
// reference, its version/change event, and this minimal idempotency receipt
// with the committed upload in one PostgreSQL transaction.
func applyPersonalMutationInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal, mutation personal.Mutation, allowedNewAttachmentID string) (personal.MutationReceipt, error) {
	if principal.UserID != mutation.OwnerID {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeForbidden, "personal mutation owner is not authorized")
	}
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err := personal.ValidateMutation(mutation); err != nil {
		return personal.MutationReceipt{}, err
	}
	expectedHash, err := personal.HashMutationContent(mutation)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	if len(mutation.RequestHash) != 32 || !bytes.Equal(mutation.RequestHash, expectedHash) {
		return personal.MutationReceipt{}, domain.InvalidArgument("request_hash", "personal mutation hash is invalid")
	}
	if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
		return personal.MutationReceipt{}, err
	}
	instanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	if replay, found, err := loadPersonalMutationReceipt(ctx, tx, instanceID, mutation); err != nil {
		return personal.MutationReceipt{}, err
	} else if found {
		return replay, nil
	}
	if _, err := lockAccountChangeCounter(ctx, tx, instanceID, mutation.OwnerID); err != nil {
		return personal.MutationReceipt{}, err
	}
	if replay, found, err := loadPersonalMutationReceipt(ctx, tx, instanceID, mutation); err != nil {
		return personal.MutationReceipt{}, err
	} else if found {
		return replay, nil
	}
	var storedObjectID string
	var previousObjectBytes int64
	objectErr := tx.QueryRow(ctx, `SELECT object_id, COALESCE(octet_length(body::text),0) FROM personal_objects
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4 FOR UPDATE`,
		instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID).Scan(&storedObjectID, &previousObjectBytes)
	objectExists := objectErr == nil
	if objectErr != nil && !errors.Is(objectErr, pgx.ErrNoRows) {
		return personal.MutationReceipt{}, fmt.Errorf("lock personal object: %w", objectErr)
	}
	var currentRevision int64
	var deleted bool
	versionErr := tx.QueryRow(ctx, `SELECT revision, is_deleted FROM account_object_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4 FOR UPDATE`,
		instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID).Scan(&currentRevision, &deleted)
	versionExists := versionErr == nil
	if versionErr != nil && !errors.Is(versionErr, pgx.ErrNoRows) {
		return personal.MutationReceipt{}, fmt.Errorf("read personal object version: %w", versionErr)
	}
	if (objectExists && !versionExists) || (versionExists && !deleted && !objectExists) {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeInternal, "personal object and version index are inconsistent")
	}
	if deleted {
		return personal.MutationReceipt{}, personalRevisionConflict(mutation.BaseRevision, currentRevision, true)
	}
	if (objectExists && currentRevision != mutation.BaseRevision) || (!objectExists && mutation.BaseRevision != 0) {
		return personal.MutationReceipt{}, personalRevisionConflict(mutation.BaseRevision, currentRevision, false)
	}
	if !objectExists && mutation.Delete {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeNotFound, "personal object not found")
	}
	if mutation.ObjectType == personal.ObjectTypeGoal && !mutation.Delete {
		if err := validatePersonalGoalMaterialOwnershipInTx(ctx, tx, instanceID, mutation.OwnerID, mutation.Body); err != nil {
			return personal.MutationReceipt{}, err
		}
	}
	if err := validatePersonalAttachmentReferenceInTx(ctx, tx, instanceID, mutation.OwnerID, mutation, allowedNewAttachmentID); err != nil {
		return personal.MutationReceipt{}, err
	}
	previousAttachmentBytes, err := personalAttachmentUsageBytesInTx(ctx, tx, instanceID, mutation.OwnerID)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	if mutation.Delete {
		if _, err := tx.Exec(ctx, `UPDATE personal_objects SET body=NULL, updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
			instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID); err != nil {
			return personal.MutationReceipt{}, fmt.Errorf("delete personal object content: %w", err)
		}
	} else if objectExists {
		if _, err := tx.Exec(ctx, `UPDATE personal_objects SET body=$5::jsonb, updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
			instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID, string(mutation.Body)); err != nil {
			return personal.MutationReceipt{}, fmt.Errorf("update personal object: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO personal_objects
(service_instance_id, owner_id, object_type, object_id, body)
VALUES ($1::uuid,$2,$3,$4,$5::jsonb)`,
			instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID, string(mutation.Body)); err != nil {
			return personal.MutationReceipt{}, fmt.Errorf("create personal object: %w", err)
		}
	}
	var nextObjectBytes int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(octet_length(body::text),0) FROM personal_objects
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
		instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID).Scan(&nextObjectBytes); err != nil {
		return personal.MutationReceipt{}, fmt.Errorf("measure personal object content: %w", err)
	}
	nextAttachmentBytes, err := personalAttachmentUsageBytesInTx(ctx, tx, instanceID, mutation.OwnerID)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	objectDelta, attachmentDelta := nextObjectBytes-previousObjectBytes, nextAttachmentBytes-previousAttachmentBytes
	if (attachmentDelta > 0 && objectDelta > math.MaxInt64-attachmentDelta) ||
		(attachmentDelta < 0 && objectDelta < math.MinInt64-attachmentDelta) {
		return personal.MutationReceipt{}, domain.NewError(domain.CodeInternal, "personal data usage delta overflowed")
	}
	if err := applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, mutation.OwnerID, objectDelta+attachmentDelta); err != nil {
		return personal.MutationReceipt{}, err
	}
	operation := "upsert"
	if mutation.Delete {
		operation = "delete"
	}
	if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{
		OwnerID: mutation.OwnerID, ObjectType: mutation.ObjectType, ObjectID: mutation.ObjectID,
		Operation: operation, MutationID: mutation.MutationID,
	}); err != nil {
		return personal.MutationReceipt{}, err
	}
	var receipt personal.MutationReceipt
	if err := tx.QueryRow(ctx, `SELECT revision FROM account_object_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
		instanceID, mutation.OwnerID, mutation.ObjectType, mutation.ObjectID).Scan(&receipt.ResultRevision); err != nil {
		return personal.MutationReceipt{}, fmt.Errorf("read personal object revision: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT last_sequence FROM account_change_counters
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, mutation.OwnerID).Scan(&receipt.AccountChangeSequence); err != nil {
		return personal.MutationReceipt{}, fmt.Errorf("read personal account change sequence: %w", err)
	}
	receipt.ObjectType, receipt.ObjectID = mutation.ObjectType, mutation.ObjectID
	if _, err := tx.Exec(ctx, `INSERT INTO personal_mutation_receipts
(service_instance_id, owner_id, mutation_id, request_hash, object_type, object_id, result_revision, change_sequence)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`, instanceID, mutation.OwnerID, mutation.MutationID,
		mutation.RequestHash, mutation.ObjectType, mutation.ObjectID, receipt.ResultRevision, receipt.AccountChangeSequence); err != nil {
		return personal.MutationReceipt{}, fmt.Errorf("write personal mutation receipt: %w", err)
	}
	return receipt, nil
}

func validatePersonalGoalMaterialOwnershipInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, body []byte) error {
	var value struct {
		RelatedMaterialIDs []string `json:"related_material_ids"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return domain.InvalidArgument("body", "goal body is invalid")
	}
	if len(value.RelatedMaterialIDs) == 0 {
		return nil
	}
	var owned int64
	if err := tx.QueryRow(ctx, `SELECT count(*)
FROM unnest($3::text[]) AS materials(material_id)
JOIN personal_objects p ON p.service_instance_id=$1::uuid AND p.owner_id=$2
	AND p.object_type='library_item' AND p.object_id=materials.material_id AND p.body IS NOT NULL
JOIN account_object_versions v ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id
  AND v.object_type=p.object_type AND v.object_id=p.object_id AND NOT v.is_deleted`,
		instanceID, ownerID, value.RelatedMaterialIDs).Scan(&owned); err != nil {
		return fmt.Errorf("check goal material ownership: %w", err)
	}
	if owned != int64(len(value.RelatedMaterialIDs)) {
		return domain.InvalidArgument("body.related_material_ids", "goal materials must be available to this account")
	}
	return nil
}

func loadPersonalMutationReceipt(ctx context.Context, tx pgx.Tx, instanceID string, mutation personal.Mutation) (personal.MutationReceipt, bool, error) {
	var receipt personal.MutationReceipt
	var storedHash []byte
	err := tx.QueryRow(ctx, `SELECT request_hash, object_type, object_id, result_revision, change_sequence
FROM personal_mutation_receipts WHERE service_instance_id=$1::uuid AND owner_id=$2 AND mutation_id=$3 FOR SHARE`,
		instanceID, mutation.OwnerID, mutation.MutationID).Scan(
		&storedHash, &receipt.ObjectType, &receipt.ObjectID, &receipt.ResultRevision, &receipt.AccountChangeSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return personal.MutationReceipt{}, false, nil
	}
	if err != nil {
		return personal.MutationReceipt{}, false, fmt.Errorf("load personal mutation receipt: %w", err)
	}
	if !bytes.Equal(storedHash, mutation.RequestHash) || receipt.ObjectType != mutation.ObjectType || receipt.ObjectID != mutation.ObjectID {
		return personal.MutationReceipt{}, false, domain.NewError(domain.CodeIdempotencyConflict, "mutation id was used for a different request")
	}
	if receipt.ResultRevision < 1 || receipt.AccountChangeSequence < 1 {
		return personal.MutationReceipt{}, false, domain.NewError(domain.CodeInternal, "personal mutation receipt is incomplete")
	}
	receipt.Replay = true
	return receipt, true, nil
}

func personalRevisionConflict(baseRevision, currentRevision int64, deleted bool) *domain.Error {
	err := domain.NewError(domain.CodeConflict, "personal object revision changed")
	err.Details["reason"] = "personal_object_revision_changed"
	err.Details["base_revision"] = baseRevision
	err.Details["current_revision"] = currentRevision
	err.Details["deleted"] = deleted
	return err
}

func authorizePersonalMutationInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	var userID string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND status='approved' FOR SHARE`, principal.UserID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err != nil {
		return fmt.Errorf("recheck personal mutation account: %w", err)
	}
	if principal.ClientInstallationID != "" {
		if !domain.ValidClientInstallationID(principal.ClientInstallationID) {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		var installationID string
		err := tx.QueryRow(ctx, `SELECT id FROM client_installations
WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL
AND (expires_at IS NULL OR expires_at > clock_timestamp()) FOR SHARE`,
			principal.ClientInstallationID, principal.UserID).Scan(&installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		if err != nil {
			return fmt.Errorf("recheck personal mutation installation: %w", err)
		}
	}
	if !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	var tokenID string
	err = tx.QueryRow(ctx, `SELECT id FROM tokens WHERE id=$1 AND owner_id=$2 AND kind=$3
AND revoked_at IS NULL AND expires_at > clock_timestamp()
AND COALESCE(client_installation_id,'')=$4 FOR SHARE`,
		principal.TokenID, principal.UserID, string(principal.Kind), principal.ClientInstallationID).Scan(&tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err != nil {
		return fmt.Errorf("recheck personal mutation credential: %w", err)
	}
	return nil
}

func validatePersonalAttachmentReferenceInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, mutation personal.Mutation, allowedNewAttachmentID string) error {
	if mutation.Delete || mutation.ObjectType != personal.ObjectTypeLibraryItem {
		return nil
	}
	var body struct {
		AttachmentID string `json:"attachment_id"`
	}
	if err := json.Unmarshal(mutation.Body, &body); err != nil {
		return domain.InvalidArgument("body", "personal object body is invalid")
	}
	if body.AttachmentID == "" || body.AttachmentID == allowedNewAttachmentID {
		return nil
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM personal_attachment_uploads
  WHERE service_instance_id=$1::uuid AND owner_id=$2 AND attachment_id=$3 AND state='committed'
)`, instanceID, ownerID, body.AttachmentID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("validate personal attachment reference: %w", err)
	}
	if !exists {
		return domain.NewError(domain.CodeNotFound, "personal attachment not found")
	}
	return nil
}

func personalAttachmentUsageBytesInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) (int64, error) {
	var usedBytes int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(attachment.size_bytes),0)::bigint
FROM (
  SELECT DISTINCT upload.attachment_id, upload.size_bytes
  FROM personal_objects object
  JOIN personal_attachment_uploads upload
    ON upload.service_instance_id=object.service_instance_id AND upload.owner_id=object.owner_id
   AND upload.attachment_id=object.body->>'attachment_id' AND upload.state='committed'
  WHERE object.service_instance_id=$1::uuid AND object.owner_id=$2 AND object.body IS NOT NULL
) AS attachment`, instanceID, ownerID).Scan(&usedBytes)
	if err != nil {
		return 0, fmt.Errorf("measure personal attachment usage: %w", err)
	}
	if usedBytes < 0 {
		return 0, domain.NewError(domain.CodeInternal, "personal attachment usage is invalid")
	}
	return usedBytes, nil
}
