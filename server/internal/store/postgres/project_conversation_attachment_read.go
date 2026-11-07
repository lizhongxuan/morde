package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

// Keep the authorization locks until the reader is closed. A membership or
// task revision change therefore cannot race a partially authorized download.
type projectAttachmentReader struct {
	reader *personalAttachmentReader
	tx     pgx.Tx
	mu     sync.Mutex
	once   sync.Once
}

func (r *projectAttachmentReader) Read(output []byte) (int, error) {
	r.mu.Lock()
	n, err := r.reader.Read(output)
	r.mu.Unlock()
	if err != nil {
		_ = r.Close()
	}
	return n, err
}

func (r *projectAttachmentReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	r.once.Do(func() {
		err = r.reader.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := r.tx.Rollback(ctx); err == nil && rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = rollbackErr
		}
	})
	return err
}

func projectAttachmentReaderInTx(ctx context.Context, tx pgx.Tx, scope app.ProjectConversationAttachmentScope, metadata app.ProjectConversationAttachmentMetadata) (io.ReadCloser, error) {
	rows, err := tx.Query(ctx, `SELECT sequence,size_bytes,sha256,body FROM project_conversation_attachment_chunks
WHERE service_instance_id=$1::uuid AND project_id=$2 AND message_id=$3 AND client_attachment_id=$4 ORDER BY sequence`,
		scope.ServiceInstanceID, scope.ProjectID, scope.MessageID, scope.ClientAttachmentID)
	if err != nil {
		return nil, fmt.Errorf("open project attachment stream: %w", err)
	}
	return &projectAttachmentReader{tx: tx, reader: &personalAttachmentReader{rows: rows,
		metadata: personal.AttachmentMetadata{AttachmentID: metadata.AttachmentID, SizeBytes: metadata.SizeBytes, SHA256: metadata.SHA256}}}, nil
}

// A derivative is bounded to 512 KiB and is read atomically from the database.
// Keep its authorization locks until the consumer closes the reader.
type projectDerivativeReader struct {
	reader *bytes.Reader
	tx     pgx.Tx
	mu     sync.Mutex
	once   sync.Once
}

func (r *projectDerivativeReader) Read(output []byte) (int, error) {
	r.mu.Lock()
	n, err := r.reader.Read(output)
	r.mu.Unlock()
	if err != nil {
		_ = r.Close()
	}
	return n, err
}

func (r *projectDerivativeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	r.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = r.tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrTxClosed) {
			err = nil
		}
	})
	return err
}

func (s *Store) OpenProjectConversationAttachment(ctx context.Context, principal identity.Principal, scope app.ProjectConversationAttachmentScope) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, _, err := lockProjectAttachmentScope(ctx, tx, principal, scope, false); err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	row, err := loadProjectAttachment(ctx, tx, scope)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && row.state != "committed" {
		return nil, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "project attachment not found")
	}
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	reader, err := projectAttachmentReaderInTx(ctx, tx, scope, row.metadata)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep = true
	return reader, row.metadata, nil
}

func resolveProjectAttachmentReference(ctx context.Context, tx pgx.Tx, principal identity.Principal, reference app.ProjectConversationAttachmentReference) (app.ProjectConversationAttachmentScope, app.ProjectConversationAttachmentMetadata, error) {
	if err := app.ValidateProjectConversationAttachmentReference(principal, reference); err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, err
	}
	if err := requireConversationServiceInstance(ctx, tx, reference.ServiceInstanceID); err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, err
	}
	if _, _, err := lockProjectForActorInTx(ctx, tx, principal.UserID, reference.ProjectID, project.RoleViewer, true); err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, err
	}
	conversation, err := loadProjectConversationForActorInTx(ctx, tx, principal.UserID, reference.ProjectID, reference.ConversationID, true)
	if err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, err
	}
	if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, err
	}
	scope := app.ProjectConversationAttachmentScope{ProjectID: reference.ProjectID, ConversationID: reference.ConversationID,
		MessageID: reference.MessageID, ServiceInstanceID: reference.ServiceInstanceID}
	var metadata app.ProjectConversationAttachmentMetadata
	err = tx.QueryRow(ctx, `SELECT a.client_attachment_id,a.attachment_id,a.name,a.media_type,a.size_bytes,a.sha256
FROM project_conversation_attachments a
JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id
 AND r.owner_id=a.owner_id AND r.actor_id=a.actor_id AND r.conversation_id=a.conversation_id
JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.conversation_id=$4 AND a.message_id=$5
 AND a.attachment_id=$6 AND a.state='committed' AND m.role='user'
 AND COALESCE(r.request_metadata->'client_attachment_ids','[]'::jsonb) ? a.client_attachment_id
FOR SHARE OF a,r`, reference.ServiceInstanceID, reference.ProjectID, conversation.OwnerID, reference.ConversationID,
		reference.MessageID, reference.AttachmentID).Scan(&scope.ClientAttachmentID, &metadata.AttachmentID, &metadata.Name,
		&metadata.MediaType, &metadata.SizeBytes, &metadata.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "project attachment not found")
	}
	if err != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, fmt.Errorf("resolve project attachment reference: %w", err)
	}
	if app.ValidateProjectConversationAttachmentMetadata(metadata) != nil {
		return app.ProjectConversationAttachmentScope{}, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeInternal, "stored project attachment metadata is invalid")
	}
	return scope, metadata, nil
}

func (s *Store) GetProjectConversationAttachmentByID(ctx context.Context, principal identity.Principal, reference app.ProjectConversationAttachmentReference) (app.ProjectConversationAttachmentMetadata, error) {
	var metadata app.ProjectConversationAttachmentMetadata
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		_, metadata, err = resolveProjectAttachmentReference(ctx, tx, principal, reference)
		return err
	})
	return metadata, err
}

func (s *Store) OpenProjectConversationAttachmentByID(ctx context.Context, principal identity.Principal, reference app.ProjectConversationAttachmentReference) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = tx.Rollback(context.Background())
		}
	}()
	scope, metadata, err := resolveProjectAttachmentReference(ctx, tx, principal, reference)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	reader, err := projectAttachmentReaderInTx(ctx, tx, scope, metadata)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep = true
	return reader, metadata, nil
}

func lockProjectAttachmentTask(ctx context.Context, tx pgx.Tx, claimed task.Task) (string, error) {
	if !domain.ValidTaskID(claimed.ID) || !domain.ValidUserID(claimed.OwnerID) || !domain.ValidUserID(claimed.InitiatorID) ||
		!domain.ValidConversationID(claimed.ConversationID) || !project.ValidID(claimed.ProjectID) || claimed.AgentInstanceID == "" ||
		claimed.AgentGeneration < 1 || claimed.AgentSessionID == "" || claimed.AgentEngineVersion == "" || claimed.AgentProfileVersion == "" ||
		claimed.Lease.Owner == "" || claimed.Lease.Version < 1 || claimed.Authorization.Version < 1 {
		return "", domain.NewError(domain.CodeUnauthenticated, "project task attachment authorization is unavailable")
	}
	var instanceID string
	if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true FOR SHARE`).Scan(&instanceID); err != nil {
		return "", fmt.Errorf("lock project attachment service instance: %w", err)
	}
	fence := app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: claimed.AgentInstanceID, AgentGeneration: claimed.AgentGeneration, AgentSessionID: claimed.AgentSessionID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: claimed.AgentEngineVersion, ProfileVersion: claimed.AgentProfileVersion}
	current, err := lockAuthorizedAgentTask(ctx, tx, fence)
	if err != nil {
		return "", err
	}
	if current.ProjectID != claimed.ProjectID || current.ConversationID != claimed.ConversationID || current.InstanceID != fence.AgentInstanceID ||
		current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID || current.EngineVersion != fence.EngineVersion ||
		current.ProfileVersion != fence.ProfileVersion {
		return "", domain.NewError(domain.CodeUnauthenticated, "project task attachment authorization changed")
	}
	if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
		return "", err
	}
	var currentRevision, desiredRevision int64
	if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision FROM tasks WHERE id=$1 AND owner_id=$2`, claimed.ID, claimed.OwnerID).
		Scan(&currentRevision, &desiredRevision); err != nil {
		return "", fmt.Errorf("read project attachment task revision: %w", err)
	}
	if currentRevision != desiredRevision {
		return "", domain.NewError(domain.CodeUnauthenticated, "project task revision has changed")
	}
	return instanceID, nil
}

const projectTaskAttachmentFrom = `FROM project_conversation_attachments a
JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id AND r.actor_id=a.actor_id
WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.actor_id=$4 AND a.conversation_id=$5
 AND m.task_id=$6 AND m.role='user' AND a.state='committed'`

func (s *Store) ListProjectTaskAttachments(ctx context.Context, claimed task.Task) ([]app.ProjectConversationAttachmentMetadata, error) {
	var result []app.ProjectConversationAttachmentMetadata
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		instanceID, err := lockProjectAttachmentTask(ctx, tx, claimed)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT attachment_id,name,media_type,size_bytes,sha256,parent_id,kind,page FROM (
 SELECT a.attachment_id,a.name,a.media_type,a.size_bytes,a.sha256,''::text AS parent_id,''::text AS kind,0 AS page,
        COALESCE((SELECT expected_revision+1 FROM task_revision_intents i WHERE i.message_id=m.id AND i.task_id=m.task_id AND i.state='applied'),0) AS task_revision,
        m.sequence,a.client_attachment_id,-1 AS position,0 AS priority `+projectTaskAttachmentFrom+`
 UNION ALL
 SELECT d.attachment_id,d.name,d.media_type,d.size_bytes,d.sha256,a.attachment_id,d.kind,d.page,
        COALESCE((SELECT expected_revision+1 FROM task_revision_intents i WHERE i.message_id=m.id AND i.task_id=m.task_id AND i.state='applied'),0) AS task_revision,
        m.sequence,a.client_attachment_id,d.position,CASE WHEN d.kind='extracted_text' THEN 1 ELSE 2 END AS priority
 FROM project_conversation_attachment_derivatives d
 JOIN project_conversation_attachments a ON a.service_instance_id=d.service_instance_id AND a.project_id=d.project_id
  AND a.message_id=d.message_id AND a.client_attachment_id=d.client_attachment_id
 JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
 JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id AND r.actor_id=a.actor_id
 WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.actor_id=$4 AND a.conversation_id=$5
  AND m.task_id=$6 AND m.role='user' AND a.state='committed' AND a.derivative_state='completed'
) files ORDER BY task_revision DESC,sequence DESC,priority,client_attachment_id,position LIMIT 64`, instanceID, claimed.ProjectID, claimed.OwnerID,
			claimed.InitiatorID, claimed.ConversationID, claimed.ID)
		if err != nil {
			return fmt.Errorf("list project task attachments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var metadata app.ProjectConversationAttachmentMetadata
			if err := rows.Scan(&metadata.AttachmentID, &metadata.Name, &metadata.MediaType, &metadata.SizeBytes, &metadata.SHA256,
				&metadata.ParentAttachmentID, &metadata.DerivativeKind, &metadata.DerivativePage); err != nil {
				return fmt.Errorf("scan project task attachment: %w", err)
			}
			if app.ValidateProjectConversationAttachmentMetadata(metadata) != nil {
				return domain.NewError(domain.CodeInternal, "stored project attachment metadata is invalid")
			}
			result = append(result, metadata)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) OpenProjectTaskAttachment(ctx context.Context, claimed task.Task, attachmentID string) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	if !app.ValidProjectConversationAttachmentID(attachmentID) {
		return nil, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "project attachment not found")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = tx.Rollback(context.Background())
		}
	}()
	instanceID, err := lockProjectAttachmentTask(ctx, tx, claimed)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	var metadata app.ProjectConversationAttachmentMetadata
	scope := app.ProjectConversationAttachmentScope{ProjectID: claimed.ProjectID, ConversationID: claimed.ConversationID, ServiceInstanceID: instanceID}
	err = tx.QueryRow(ctx, `SELECT a.attachment_id,a.name,a.media_type,a.size_bytes,a.sha256,a.message_id,a.client_attachment_id `+
		projectTaskAttachmentFrom+` AND a.attachment_id=$7 FOR SHARE OF a`, instanceID, claimed.ProjectID, claimed.OwnerID,
		claimed.InitiatorID, claimed.ConversationID, claimed.ID, attachmentID).Scan(&metadata.AttachmentID, &metadata.Name,
		&metadata.MediaType, &metadata.SizeBytes, &metadata.SHA256, &scope.MessageID, &scope.ClientAttachmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		var content []byte
		err = tx.QueryRow(ctx, `SELECT d.attachment_id,d.name,d.media_type,d.size_bytes,d.sha256,a.attachment_id,d.kind,d.page,d.body
FROM project_conversation_attachment_derivatives d
JOIN project_conversation_attachments a ON a.service_instance_id=d.service_instance_id AND a.project_id=d.project_id
 AND a.message_id=d.message_id AND a.client_attachment_id=d.client_attachment_id
JOIN conversation_messages m ON m.id=a.message_id AND m.owner_id=a.owner_id AND m.conversation_id=a.conversation_id AND m.actor_id=a.actor_id
JOIN conversation_message_routes r ON r.message_id=a.message_id AND r.service_instance_id=a.service_instance_id AND r.actor_id=a.actor_id
WHERE a.service_instance_id=$1::uuid AND a.project_id=$2 AND a.owner_id=$3 AND a.actor_id=$4 AND a.conversation_id=$5
 AND m.task_id=$6 AND m.role='user' AND a.state='committed' AND a.derivative_state='completed' AND d.attachment_id=$7
FOR SHARE OF a,d`, instanceID, claimed.ProjectID, claimed.OwnerID, claimed.InitiatorID, claimed.ConversationID, claimed.ID, attachmentID).
			Scan(&metadata.AttachmentID, &metadata.Name, &metadata.MediaType, &metadata.SizeBytes, &metadata.SHA256,
				&metadata.ParentAttachmentID, &metadata.DerivativeKind, &metadata.DerivativePage, &content)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "project task attachment not found")
		}
		if err != nil {
			return nil, app.ProjectConversationAttachmentMetadata{}, fmt.Errorf("open project task derivative: %w", err)
		}
		digest := sha256.Sum256(content)
		if app.ValidateProjectConversationAttachmentMetadata(metadata) != nil || metadata.SizeBytes != int64(len(content)) ||
			metadata.SHA256 != hex.EncodeToString(digest[:]) {
			return nil, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeInternal, "stored project derivative is invalid")
		}
		keep = true
		return &projectDerivativeReader{tx: tx, reader: bytes.NewReader(content)}, metadata, nil
	}
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, fmt.Errorf("open project task attachment: %w", err)
	}
	if app.ValidateProjectConversationAttachmentMetadata(metadata) != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeInternal, "stored project attachment metadata is invalid")
	}
	reader, err := projectAttachmentReaderInTx(ctx, tx, scope, metadata)
	if err != nil {
		return nil, app.ProjectConversationAttachmentMetadata{}, err
	}
	keep = true
	return reader, metadata, nil
}

var _ app.ProjectConversationAttachmentStore = (*Store)(nil)
