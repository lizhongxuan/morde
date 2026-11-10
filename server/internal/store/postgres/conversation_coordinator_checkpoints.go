package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) SaveConversationCoordinatorCheckpoint(ctx context.Context, claim app.ConversationRouteClaim,
	checkpoint app.StoredConversationCoordinatorCheckpoint) (bool, error) {
	identity := checkpoint.Identity
	if err := app.ValidateConversationCoordinatorIdentity(claim, identity); err != nil || checkpoint.From < 0 ||
		checkpoint.Through < checkpoint.From || len(checkpoint.ContextDigest) != 32 ||
		len(checkpoint.Digest) != 32 || len(checkpoint.Ciphertext) == 0 {
		return false, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint is invalid")
	}
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO conversation_coordinator_checkpoint_heads
(service_instance_id,owner_id,actor_id,conversation_id,message_id,agent_id,generation,authorization_version,session_id,context_digest)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, identity.ServiceInstanceID,
			identity.OwnerID, identity.ActorID, identity.ConversationID, identity.MessageID, identity.AgentID,
			identity.Generation, identity.AuthorizationVersion, identity.SessionID, checkpoint.ContextDigest)
		if err != nil {
			return fmt.Errorf("create coordinator checkpoint head: %w", err)
		}
		var agentID string
		var generation, authorizationVersion, through int64
		var headDigest, headContextDigest []byte
		err = tx.QueryRow(ctx, `SELECT agent_id,generation,authorization_version,through_sequence,digest,context_digest
FROM conversation_coordinator_checkpoint_heads WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND message_id=$5 AND session_id=$6 FOR UPDATE`, identity.ServiceInstanceID,
			identity.OwnerID, identity.ActorID, identity.ConversationID, identity.MessageID, identity.SessionID).
			Scan(&agentID, &generation, &authorizationVersion, &through, &headDigest, &headContextDigest)
		if err != nil {
			return fmt.Errorf("lock coordinator checkpoint head: %w", err)
		}
		if agentID != identity.AgentID || generation != identity.Generation || authorizationVersion != identity.AuthorizationVersion {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint identity changed")
		}
		if len(headContextDigest) != 32 {
			return domain.NewError(domain.CodeInvalidState, "coordinator checkpoint has no bound route context")
		}
		if !bytes.Equal(headContextDigest, checkpoint.ContextDigest) {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint route context changed")
		}
		var existingThrough int64
		var existingDigest, existingContextDigest []byte
		err = tx.QueryRow(ctx, `SELECT through_sequence,digest,context_digest FROM conversation_coordinator_checkpoint_pages
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND message_id=$5
 AND session_id=$6 AND from_sequence=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.MessageID, identity.SessionID, checkpoint.From).Scan(&existingThrough, &existingDigest, &existingContextDigest)
		if err == nil {
			if existingThrough == checkpoint.Through && bytes.Equal(existingDigest, checkpoint.Digest) &&
				bytes.Equal(existingContextDigest, checkpoint.ContextDigest) {
				replayed = true
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "coordinator checkpoint sequence was saved differently")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find coordinator checkpoint page: %w", err)
		}
		if checkpoint.From != through+1 {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint sequence is not contiguous")
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_coordinator_checkpoint_pages
			(service_instance_id,owner_id,actor_id,conversation_id,message_id,session_id,from_sequence,through_sequence,digest,context_digest,ciphertext)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.MessageID, identity.SessionID, checkpoint.From, checkpoint.Through,
			checkpoint.Digest, checkpoint.ContextDigest, checkpoint.Ciphertext)
		if err != nil {
			return fmt.Errorf("insert coordinator checkpoint page: %w", err)
		}
		updated, err := tx.Exec(ctx, `UPDATE conversation_coordinator_checkpoint_heads SET through_sequence=$7,digest=$8,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND message_id=$5 AND session_id=$6
 AND through_sequence=$9`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.MessageID, identity.SessionID, checkpoint.Through, checkpoint.Digest, through)
		if err != nil {
			return fmt.Errorf("advance coordinator checkpoint head: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint head changed")
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ReadConversationCoordinatorCheckpointHead(ctx context.Context, claim app.ConversationRouteClaim,
	identity app.ConversationCoordinatorIdentity, expectedContextDigest []byte) (app.ConversationCoordinatorCheckpointHead, error) {
	var head app.ConversationCoordinatorCheckpointHead
	if err := app.ValidateConversationCoordinatorIdentity(claim, identity); err != nil {
		return head, err
	}
	if len(expectedContextDigest) != 32 {
		return head, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint context digest is invalid")
	}
	head.Through = -1
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var agentID string
		var generation, authorizationVersion int64
		var contextDigest []byte
		err := tx.QueryRow(ctx, `SELECT agent_id,generation,authorization_version,through_sequence,digest,context_digest
FROM conversation_coordinator_checkpoint_heads WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND message_id=$5 AND session_id=$6`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.MessageID, identity.SessionID).
			Scan(&agentID, &generation, &authorizationVersion, &head.Through, &head.Digest, &contextDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read coordinator checkpoint head: %w", err)
		}
		if agentID != identity.AgentID || generation != identity.Generation || authorizationVersion != identity.AuthorizationVersion {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint identity changed")
		}
		if len(contextDigest) != 32 {
			return domain.NewError(domain.CodeInvalidState, "coordinator checkpoint has no bound route context")
		}
		if !bytes.Equal(contextDigest, expectedContextDigest) {
			return domain.NewError(domain.CodeConflict, "coordinator checkpoint route context changed")
		}
		head.ContextDigest = append([]byte(nil), contextDigest...)
		return nil
	})
	return head, err
}

func (s *Store) ReadConversationCoordinatorCheckpointPage(ctx context.Context, claim app.ConversationRouteClaim,
	identity app.ConversationCoordinatorIdentity, expectedContextDigest []byte, from, through int64,
	throughDigest []byte) (app.StoredConversationCoordinatorCheckpoint, error) {
	var stored app.StoredConversationCoordinatorCheckpoint
	if err := app.ValidateConversationCoordinatorIdentity(claim, identity); err != nil {
		return stored, err
	}
	if len(expectedContextDigest) != 32 || from < 0 || through < from || len(throughDigest) != 32 {
		return stored, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint page request is invalid")
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var agentID string
		var generation, authorizationVersion, headThrough int64
		var headDigest, headContextDigest []byte
		err := tx.QueryRow(ctx, `SELECT agent_id,generation,authorization_version,through_sequence,digest,context_digest
FROM conversation_coordinator_checkpoint_heads WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND message_id=$5 AND session_id=$6`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.MessageID, identity.SessionID).
			Scan(&agentID, &generation, &authorizationVersion, &headThrough, &headDigest, &headContextDigest)
		if err != nil || agentID != identity.AgentID || generation != identity.Generation ||
			authorizationVersion != identity.AuthorizationVersion || headThrough != through || !bytes.Equal(headDigest, throughDigest) ||
			!bytes.Equal(headContextDigest, expectedContextDigest) {
			return domain.NewError(domain.CodeInvalidState, "coordinator checkpoint head is unavailable")
		}
		if len(headContextDigest) != 32 {
			return domain.NewError(domain.CodeInvalidState, "coordinator checkpoint has no bound route context")
		}
		stored.Identity = identity
		stored.ContextDigest = append([]byte(nil), headContextDigest...)
		err = tx.QueryRow(ctx, `SELECT from_sequence,through_sequence,digest,context_digest,ciphertext
FROM conversation_coordinator_checkpoint_pages WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
			AND conversation_id=$4 AND message_id=$5 AND session_id=$6 AND from_sequence=$7 AND through_sequence<=$8 AND context_digest=$9`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.MessageID, identity.SessionID, from, through, expectedContextDigest).
			Scan(&stored.From, &stored.Through, &stored.Digest, &stored.ContextDigest, &stored.Ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "coordinator checkpoint page was not found")
		}
		if err != nil {
			return fmt.Errorf("read coordinator checkpoint page: %w", err)
		}
		return nil
	})
	return stored, err
}
