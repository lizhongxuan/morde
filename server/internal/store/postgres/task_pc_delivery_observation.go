package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func (s *Store) RecordPCDeliveryObservation(ctx context.Context, observerID, tokenID, installationID, taskID, messageID string, taskRevision int64) error {
	if ctx == nil || !domain.ValidUserID(observerID) || !domain.ValidTokenID(tokenID) ||
		!domain.ValidClientInstallationID(installationID) || !domain.ValidTaskID(taskID) ||
		!domain.ValidMessageID(messageID) || taskRevision < 0 {
		return domain.InvalidArgument("delivery_observation", "PC delivery observation is invalid")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PC delivery observation: %w", err)
	}
	defer tx.Rollback(ctx)

	var activeInstallationID string
	err = tx.QueryRow(ctx, `SELECT installation.id
FROM client_installations installation
JOIN tokens session ON session.client_installation_id=installation.id
JOIN users account ON account.id=installation.owner_id
WHERE installation.id=$1 AND installation.owner_id=$2 AND installation.platform='desktop'
  AND installation.revoked_at IS NULL AND (installation.expires_at IS NULL OR installation.expires_at>clock_timestamp())
  AND session.id=$3 AND session.owner_id=$2 AND session.kind='session' AND session.revoked_at IS NULL
  AND session.expires_at>clock_timestamp() AND cardinality(session.scopes)=0 AND account.status='approved'
FOR SHARE OF installation,session,account`, installationID, observerID, tokenID).Scan(&activeInstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "authenticated desktop session required")
	}
	if err != nil {
		return fmt.Errorf("verify PC delivery observer installation: %w", err)
	}

	var ownerID, conversationID, initiatorID, projectID string
	err = tx.QueryRow(ctx, `SELECT task.owner_id,task.conversation_id,task.initiator_id,COALESCE(task.project_id,'')
FROM tasks task WHERE task.id=$1`, taskID).Scan(&ownerID, &conversationID, &initiatorID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && initiatorID != observerID {
		return domain.NewError(domain.CodeNotFound, "completed Task reply not found")
	}
	if err != nil {
		return fmt.Errorf("resolve PC-observed Task conversation: %w", err)
	}
	if projectID != "" {
		if _, _, err := lockProjectForActorInTx(ctx, tx, observerID, projectID, project.RoleViewer, false); err != nil {
			return domain.NewError(domain.CodeNotFound, "completed Task reply not found")
		}
		conversationValue, err := loadProjectConversationForActorInTx(ctx, tx, observerID, projectID, conversationID, false)
		if err != nil || conversationValue.OwnerID != ownerID {
			return domain.NewError(domain.CodeNotFound, "completed Task reply not found")
		}
	} else if ownerID != observerID {
		return domain.NewError(domain.CodeNotFound, "completed Task reply not found")
	}

	err = tx.QueryRow(ctx, `SELECT message.conversation_id
FROM conversation_messages message
JOIN tasks task ON task.id=message.task_id AND task.owner_id=message.owner_id
  AND task.conversation_id=message.conversation_id AND task.initiator_id=message.actor_id
JOIN task_events result ON result.task_id=message.task_id AND result.sequence=message.output_result_event_sequence
  AND result.task_revision=message.output_task_revision AND result.event_type='task.result'
WHERE message.id=$1 AND message.task_id=$2 AND message.owner_id=$3
  AND message.actor_id=$5 AND message.role='assistant' AND message.delivery_status='applied'
  AND message.output_status='completed' AND message.output_task_revision=$4
  AND message.output_result_event_sequence IS NOT NULL`, messageID, taskID, ownerID, taskRevision, observerID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "completed Task reply not found")
	}
	if err != nil {
		return fmt.Errorf("verify published Task reply for PC observation: %w", err)
	}

	_, err = tx.Exec(ctx, `INSERT INTO task_pc_delivery_observations
(task_id,owner_id,conversation_id,client_installation_id,message_id,task_revision)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (task_id,owner_id,client_installation_id,message_id,task_revision) DO NOTHING`,
		taskID, ownerID, conversationID, activeInstallationID, messageID, taskRevision)
	if err != nil {
		return fmt.Errorf("persist PC delivery observation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PC delivery observation: %w", err)
	}
	return nil
}
