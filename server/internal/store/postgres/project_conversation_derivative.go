package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

// PutProjectConversationDerivatives atomically commits the complete derived
// set to the original actor-owned slot. The route lock also prevents a worker
// from claiming a message while its representations are being committed.
func (s *Store) PutProjectConversationDerivatives(ctx context.Context, principal identity.Principal,
	scope app.ProjectConversationAttachmentScope, value app.ProjectConversationDerivatives) (app.ProjectConversationAttachmentStatus, error) {
	var result app.ProjectConversationAttachmentStatus
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		_, routeState, err := lockProjectAttachmentScope(ctx, tx, principal, scope, true)
		if err != nil {
			return err
		}
		row, err := loadProjectAttachment(ctx, tx, scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "project attachment upload not found")
		}
		if err != nil {
			return err
		}
		fingerprint, size, err := app.ValidateProjectConversationDerivatives(row.metadata.DerivativeMode, value)
		if err != nil {
			return err
		}
		if row.state != "committed" {
			return domain.NewError(domain.CodeConflict, "original project attachment is not committed")
		}
		if row.derivativeState == "completed" || row.derivativeState == "skipped" {
			if row.derivativeState != value.Outcome || !bytes.Equal(row.derivativeFingerprint, fingerprint) {
				return domain.NewError(domain.CodeIdempotencyConflict, "project attachment derivatives already have different content")
			}
			result, err = projectAttachmentStatus(ctx, tx, scope, row)
			result.Replay = true
			return err
		}
		if row.derivativeState != "awaiting" || routeState != "pending" {
			return domain.NewError(domain.CodeConflict, "project attachment derivatives cannot be changed")
		}
		var reserved int64
		if err := tx.QueryRow(ctx, `SELECT
 COALESCE((SELECT SUM(size_bytes) FROM project_conversation_attachments WHERE service_instance_id=$1::uuid AND project_id=$2 AND state IN ('uploading','committed')),0)::bigint +
 COALESCE((SELECT SUM(size_bytes) FROM project_conversation_attachment_derivatives WHERE service_instance_id=$1::uuid AND project_id=$2),0)::bigint`, scope.ServiceInstanceID, scope.ProjectID).Scan(&reserved); err != nil {
			return fmt.Errorf("measure project derivative storage: %w", err)
		}
		if reserved > app.ProjectConversationAttachmentQuota-size {
			return domain.NewError(domain.CodeQuotaExceeded, "project attachment storage limit is reached")
		}
		for position, file := range value.Files {
			seed := "project-attachment-derivative\x00" + row.metadata.AttachmentID + "\x00" + hex.EncodeToString(fingerprint) + "\x00" + strconv.Itoa(position)
			id := sha256.Sum256([]byte(seed))
			attachmentID := "pca_" + hex.EncodeToString(id[:16])
			if _, err := tx.Exec(ctx, `INSERT INTO project_conversation_attachment_derivatives
(service_instance_id,project_id,message_id,client_attachment_id,attachment_id,position,kind,page,name,media_type,size_bytes,sha256,body)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, scope.ServiceInstanceID, scope.ProjectID,
				scope.MessageID, scope.ClientAttachmentID, attachmentID, position, file.Kind, file.Page, file.Name,
				file.MediaType, len(file.Content), file.SHA256, file.Content); err != nil {
				return fmt.Errorf("commit project attachment derivative: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE project_conversation_attachments
SET derivative_state=$3,derivative_fingerprint=$4,updated_at=clock_timestamp()
WHERE message_id=$1 AND client_attachment_id=$2`, scope.MessageID, scope.ClientAttachmentID, value.Outcome, fingerprint); err != nil {
			return fmt.Errorf("complete project attachment derivatives: %w", err)
		}
		row.derivativeState, row.derivativeFingerprint = value.Outcome, fingerprint
		result, err = projectAttachmentStatus(ctx, tx, scope, row)
		return err
	})
	return result, err
}
