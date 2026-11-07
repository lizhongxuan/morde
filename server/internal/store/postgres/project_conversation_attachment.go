package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
)

const projectAttachmentLifetime = 24 * time.Hour

type projectAttachmentRow struct {
	metadata              app.ProjectConversationAttachmentMetadata
	state                 string
	derivativeState       string
	derivativeFingerprint []byte
	expiresAt             time.Time
}

// Scope is locked before the slot so project membership and current service
// identity cannot change between authorization and the associated write.
func lockProjectAttachmentScope(ctx context.Context, tx pgx.Tx, principal identity.Principal, scope app.ProjectConversationAttachmentScope, senderOnly bool) (string, string, error) {
	if err := app.ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return "", "", err
	}
	if err := requireConversationServiceInstance(ctx, tx, scope.ServiceInstanceID); err != nil {
		return "", "", err
	}
	minimumRole := project.RoleMember
	if !senderOnly {
		minimumRole = project.RoleViewer
	}
	if _, _, err := lockProjectForActorInTx(ctx, tx, principal.UserID, scope.ProjectID, minimumRole, true); err != nil {
		return "", "", err
	}
	conversation, err := loadProjectConversationForActorInTx(ctx, tx, principal.UserID, scope.ProjectID, scope.ConversationID, true)
	if err != nil {
		return "", "", err
	}
	if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
		return "", "", err
	}
	var actorID, state string
	var declared bool
	err = tx.QueryRow(ctx, `SELECT r.actor_id,r.state,COALESCE(r.request_metadata->'client_attachment_ids','[]'::jsonb) ? $5
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id AND m.actor_id=r.actor_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.conversation_id=$3 AND r.message_id=$4
 AND m.role='user' FOR UPDATE OF r`, scope.ServiceInstanceID, conversation.OwnerID, scope.ConversationID,
		scope.MessageID, scope.ClientAttachmentID).Scan(&actorID, &state, &declared)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!declared || senderOnly && actorID != principal.UserID) {
		return "", "", domain.NewError(domain.CodeNotFound, "project conversation attachment slot not found")
	}
	if err != nil {
		return "", "", fmt.Errorf("lock project attachment message: %w", err)
	}
	return conversation.OwnerID, state, nil
}

func loadProjectAttachment(ctx context.Context, tx pgx.Tx, scope app.ProjectConversationAttachmentScope) (projectAttachmentRow, error) {
	var row projectAttachmentRow
	err := tx.QueryRow(ctx, `SELECT attachment_id,name,media_type,size_bytes,sha256,state,expires_at,derivative_mode,derivative_state,derivative_fingerprint
FROM project_conversation_attachments WHERE service_instance_id=$1::uuid AND project_id=$2
 AND conversation_id=$3 AND message_id=$4 AND client_attachment_id=$5 FOR UPDATE`,
		scope.ServiceInstanceID, scope.ProjectID, scope.ConversationID, scope.MessageID, scope.ClientAttachmentID).
		Scan(&row.metadata.AttachmentID, &row.metadata.Name, &row.metadata.MediaType, &row.metadata.SizeBytes,
			&row.metadata.SHA256, &row.state, &row.expiresAt, &row.metadata.DerivativeMode, &row.derivativeState, &row.derivativeFingerprint)
	if err == nil && app.ValidateProjectConversationAttachmentMetadata(row.metadata) != nil {
		return projectAttachmentRow{}, domain.NewError(domain.CodeInternal, "stored project attachment metadata is invalid")
	}
	return row, err
}

func projectAttachmentStatus(ctx context.Context, tx pgx.Tx, scope app.ProjectConversationAttachmentScope, row projectAttachmentRow) (app.ProjectConversationAttachmentStatus, error) {
	count, err := personal.AttachmentChunkCount(row.metadata.SizeBytes)
	if err != nil {
		return app.ProjectConversationAttachmentStatus{}, err
	}
	state := row.state
	if state == "uploading" && !row.expiresAt.After(time.Now().UTC()) {
		state = "expired"
	}
	result := app.ProjectConversationAttachmentStatus{ProjectConversationAttachmentMetadata: row.metadata,
		State: state, TotalChunks: count, ReceivedSequences: []int32{}, DerivativeState: row.derivativeState}
	if state == "expired" {
		return result, nil
	}
	rows, err := tx.Query(ctx, `SELECT sequence FROM project_conversation_attachment_chunks
WHERE service_instance_id=$1::uuid AND project_id=$2 AND message_id=$3 AND client_attachment_id=$4 ORDER BY sequence`,
		scope.ServiceInstanceID, scope.ProjectID, scope.MessageID, scope.ClientAttachmentID)
	if err != nil {
		return app.ProjectConversationAttachmentStatus{}, fmt.Errorf("list project attachment chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int32
		if err := rows.Scan(&sequence); err != nil || sequence < 0 || sequence >= count {
			return app.ProjectConversationAttachmentStatus{}, domain.NewError(domain.CodeInternal, "project attachment chunk receipt is invalid")
		}
		result.ReceivedSequences = append(result.ReceivedSequences, sequence)
	}
	return result, rows.Err()
}

func (s *Store) InitializeProjectConversationAttachment(ctx context.Context, principal identity.Principal, scope app.ProjectConversationAttachmentScope, metadata app.ProjectConversationAttachmentMetadata) (app.ProjectConversationAttachmentStatus, error) {
	if metadata.DerivativeMode == "" {
		metadata.DerivativeMode = "none"
	}
	if err := app.ValidateProjectConversationAttachmentMetadata(metadata); err != nil {
		return app.ProjectConversationAttachmentStatus{}, err
	}
	var result app.ProjectConversationAttachmentStatus
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		ownerID, routeState, err := lockProjectAttachmentScope(ctx, tx, principal, scope, true)
		if err != nil {
			return err
		}
		// Release expired staging only under the project lock; the durable slot
		// remains so an exact retry can restart without changing its identity.
		if _, err := tx.Exec(ctx, `DELETE FROM project_conversation_attachment_chunks chunk USING project_conversation_attachments upload
WHERE upload.service_instance_id=$1::uuid AND upload.project_id=$2 AND upload.state='uploading' AND upload.expires_at<=clock_timestamp()
 AND chunk.service_instance_id=upload.service_instance_id AND chunk.project_id=upload.project_id
 AND chunk.message_id=upload.message_id AND chunk.client_attachment_id=upload.client_attachment_id`, scope.ServiceInstanceID, scope.ProjectID); err != nil {
			return fmt.Errorf("remove expired project attachment chunks: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE project_conversation_attachments SET state='expired',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND project_id=$2 AND state='uploading' AND expires_at<=clock_timestamp()`, scope.ServiceInstanceID, scope.ProjectID); err != nil {
			return fmt.Errorf("expire project attachment uploads: %w", err)
		}
		row, err := loadProjectAttachment(ctx, tx, scope)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if exists && (row.metadata.Name != metadata.Name || row.metadata.MediaType != metadata.MediaType ||
			row.metadata.SizeBytes != metadata.SizeBytes || row.metadata.SHA256 != metadata.SHA256 || row.metadata.DerivativeMode != metadata.DerivativeMode) {
			return domain.NewError(domain.CodeIdempotencyConflict, "project attachment slot already has different metadata")
		}
		if exists && row.state != "expired" {
			result, err = projectAttachmentStatus(ctx, tx, scope, row)
			result.Replay = true
			return err
		}
		if routeState != "pending" {
			return domain.NewError(domain.CodeConflict, "message is already being processed")
		}
		var reserved int64
		if err := tx.QueryRow(ctx, `SELECT
 COALESCE((SELECT SUM(size_bytes) FROM project_conversation_attachments WHERE service_instance_id=$1::uuid AND project_id=$2 AND state IN ('uploading','committed')),0)::bigint +
 COALESCE((SELECT SUM(size_bytes) FROM project_conversation_attachment_derivatives WHERE service_instance_id=$1::uuid AND project_id=$2),0)::bigint`, scope.ServiceInstanceID, scope.ProjectID).Scan(&reserved); err != nil {
			return fmt.Errorf("measure project attachment storage: %w", err)
		}
		if reserved > app.ProjectConversationAttachmentQuota-metadata.SizeBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "project attachment storage limit is reached")
		}
		if exists {
			if _, err := tx.Exec(ctx, `UPDATE project_conversation_attachments SET state='uploading',expires_at=clock_timestamp()+$3::interval,updated_at=clock_timestamp()
WHERE message_id=$1 AND client_attachment_id=$2`, scope.MessageID, scope.ClientAttachmentID, projectAttachmentLifetime.String()); err != nil {
				return fmt.Errorf("restart expired project attachment upload: %w", err)
			}
			metadata.AttachmentID = row.metadata.AttachmentID
		} else if _, err := tx.Exec(ctx, `INSERT INTO project_conversation_attachments
(service_instance_id,project_id,owner_id,actor_id,conversation_id,message_id,client_attachment_id,attachment_id,name,media_type,size_bytes,sha256,state,expires_at,derivative_mode,derivative_state)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'uploading',clock_timestamp()+$13::interval,$14,CASE WHEN $14='none' THEN 'none' ELSE 'awaiting' END)`,
			scope.ServiceInstanceID, scope.ProjectID, ownerID, principal.UserID, scope.ConversationID, scope.MessageID,
			scope.ClientAttachmentID, metadata.AttachmentID, metadata.Name, metadata.MediaType, metadata.SizeBytes, metadata.SHA256,
			projectAttachmentLifetime.String(), metadata.DerivativeMode); err != nil {
			return fmt.Errorf("initialize project attachment: %w", err)
		}
		row, err = loadProjectAttachment(ctx, tx, scope)
		if err != nil {
			return err
		}
		result, err = projectAttachmentStatus(ctx, tx, scope, row)
		result.Replay = exists
		return err
	})
	return result, err
}

func (s *Store) GetProjectConversationAttachment(ctx context.Context, principal identity.Principal, scope app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error) {
	var result app.ProjectConversationAttachmentStatus
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectAttachmentScope(ctx, tx, principal, scope, true); err != nil {
			return err
		}
		row, err := loadProjectAttachment(ctx, tx, scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "project attachment upload not found")
		}
		if err != nil {
			return err
		}
		result, err = projectAttachmentStatus(ctx, tx, scope, row)
		return err
	})
	return result, err
}

func (s *Store) PutProjectConversationAttachmentChunk(ctx context.Context, principal identity.Principal, scope app.ProjectConversationAttachmentScope, sequence int32, digest string, content []byte) (bool, error) {
	if sequence < 0 || sequence >= 75 || len(content) == 0 || int64(len(content)) > app.ProjectConversationAttachmentChunk || digestAttachmentBytes(content) != digest {
		return false, domain.InvalidArgument("attachment_chunk", "project attachment chunk size or digest is invalid")
	}
	var replay bool
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
		if row.state != "committed" && (row.state != "uploading" || !row.expiresAt.After(time.Now().UTC())) {
			return domain.NewError(domain.CodeTransferExpired, "project attachment upload is not active")
		}
		if row.state != "committed" && routeState != "pending" {
			return domain.NewError(domain.CodeConflict, "message is already being processed")
		}
		expected, err := personal.ExpectedAttachmentChunkSize(row.metadata.SizeBytes, sequence)
		if err != nil || expected != int64(len(content)) {
			return domain.InvalidArgument("attachment_chunk", "project attachment chunk does not match the upload plan")
		}
		var storedDigest string
		var storedSize int64
		err = tx.QueryRow(ctx, `SELECT size_bytes,sha256 FROM project_conversation_attachment_chunks
WHERE message_id=$1 AND client_attachment_id=$2 AND sequence=$3`, scope.MessageID, scope.ClientAttachmentID, sequence).Scan(&storedSize, &storedDigest)
		if err == nil {
			if storedSize != int64(len(content)) || storedDigest != digest {
				return domain.NewError(domain.CodeIdempotencyConflict, "project attachment chunk has different content")
			}
			replay = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check project attachment chunk: %w", err)
		}
		if row.state == "committed" {
			return domain.NewError(domain.CodeInvalidState, "committed project attachment is missing a chunk")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_conversation_attachment_chunks
(service_instance_id,project_id,message_id,client_attachment_id,sequence,size_bytes,sha256,body)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`, scope.ServiceInstanceID, scope.ProjectID, scope.MessageID, scope.ClientAttachmentID,
			sequence, len(content), digest, content); err != nil {
			return fmt.Errorf("store project attachment chunk: %w", err)
		}
		return nil
	})
	return replay, err
}

func (s *Store) CommitProjectConversationAttachment(ctx context.Context, principal identity.Principal, scope app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error) {
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
		if row.state == "committed" {
			result, err = projectAttachmentStatus(ctx, tx, scope, row)
			result.Replay = true
			return err
		}
		if row.state != "uploading" || !row.expiresAt.After(time.Now().UTC()) {
			return domain.NewError(domain.CodeTransferExpired, "project attachment upload is not active")
		}
		if routeState != "pending" {
			return domain.NewError(domain.CodeConflict, "message is already being processed")
		}
		if err := verifyProjectAttachmentChunks(ctx, tx, scope, row.metadata); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE project_conversation_attachments SET state='committed',updated_at=clock_timestamp()
WHERE message_id=$1 AND client_attachment_id=$2`, scope.MessageID, scope.ClientAttachmentID); err != nil {
			return fmt.Errorf("commit project attachment: %w", err)
		}
		row.state = "committed"
		result, err = projectAttachmentStatus(ctx, tx, scope, row)
		return err
	})
	return result, err
}

func verifyProjectAttachmentChunks(ctx context.Context, tx pgx.Tx, scope app.ProjectConversationAttachmentScope, metadata app.ProjectConversationAttachmentMetadata) error {
	rows, err := tx.Query(ctx, `SELECT sequence,size_bytes,sha256,body FROM project_conversation_attachment_chunks
WHERE service_instance_id=$1::uuid AND project_id=$2 AND message_id=$3 AND client_attachment_id=$4 ORDER BY sequence`,
		scope.ServiceInstanceID, scope.ProjectID, scope.MessageID, scope.ClientAttachmentID)
	if err != nil {
		return fmt.Errorf("read project attachment chunks: %w", err)
	}
	defer rows.Close()
	hash := sha256.New()
	var sequence int32
	var total int64
	for rows.Next() {
		var actual int32
		var size int64
		var digest string
		var content []byte
		if err := rows.Scan(&actual, &size, &digest, &content); err != nil {
			return fmt.Errorf("scan project attachment chunk: %w", err)
		}
		expected, sizeErr := personal.ExpectedAttachmentChunkSize(metadata.SizeBytes, sequence)
		if sizeErr != nil || actual != sequence || size != expected || size != int64(len(content)) || digestAttachmentBytes(content) != digest {
			return domain.NewError(domain.CodeInvalidState, "project attachment chunks are incomplete or corrupted")
		}
		_, _ = hash.Write(content)
		total += size
		sequence++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read project attachment chunks: %w", err)
	}
	count, err := personal.AttachmentChunkCount(metadata.SizeBytes)
	if err != nil || sequence != count || total != metadata.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != metadata.SHA256 {
		return domain.NewError(domain.CodeInvalidState, "project attachment chunks are incomplete or corrupted")
	}
	return nil
}
