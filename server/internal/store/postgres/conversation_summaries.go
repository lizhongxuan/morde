package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *Store) ReadConversationSummaryHead(ctx context.Context, claim app.ConversationRouteClaim) (app.ConversationSummaryHead, error) {
	var head app.ConversationSummaryHead
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, claim)
		if err != nil {
			return err
		}
		head, err = ensureConversationSummaryHead(ctx, tx, identity, false)
		return err
	})
	return head, err
}

func (s *Store) ReadConversationSummary(ctx context.Context, claim app.ConversationRouteClaim) (app.StoredConversationSummary, bool, error) {
	var summary app.StoredConversationSummary
	var found bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, claim)
		if err != nil {
			return err
		}
		head, err := ensureConversationSummaryHead(ctx, tx, identity, false)
		if err != nil || head.Version == 0 {
			return err
		}
		var previous *int64
		var previousValue *int64
		summary.Identity = head.Identity
		err = tx.QueryRow(ctx, `SELECT version,previous_version,operation_id,summary_digest,source_digest,ciphertext
FROM conversation_summary_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND version=$7 AND authorization_version=$8 AND scope_version=$9 AND state='valid'`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.ScopeKind, identity.ScopeID, head.Version, identity.AuthorizationVersion, identity.ScopeVersion).
			Scan(&summary.Version, &previousValue, &summary.OperationID, &summary.SummarySHA256, &summary.SourcesSHA256, &summary.Ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read conversation summary version: %w", err)
		}
		if previousValue != nil {
			previous = previousValue
			summary.Previous = previous
		}
		rows, err := tx.Query(ctx, `SELECT source_kind,source_id,source_object_type,source_version,source_sequence,source_digest
FROM conversation_summary_sources WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND summary_version=$7 ORDER BY source_order`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.ScopeKind, identity.ScopeID, summary.Version)
		if err != nil {
			return fmt.Errorf("read conversation summary sources: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var source app.ConversationSummarySource
			if err := rows.Scan(&source.Kind, &source.ID, &source.ObjectType, &source.Version, &source.Sequence, &source.SHA256); err != nil {
				return fmt.Errorf("scan conversation summary source: %w", err)
			}
			summary.Sources = append(summary.Sources, source)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read conversation summary sources: %w", err)
		}
		if len(summary.Sources) == 0 {
			return domain.NewError(domain.CodeInvalidState, "conversation summary has no source proof")
		}
		found = true
		return nil
	})
	return summary, found, err
}

func lockConversationSummaryScope(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (app.ConversationSummaryIdentity, error) {
	identity, err := app.ConversationSummaryScopeForClaim(claim)
	if err != nil {
		return app.ConversationSummaryIdentity{}, err
	}
	if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
		return app.ConversationSummaryIdentity{}, err
	}
	var conversationProjectID string
	err = tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM conversations
WHERE id=$1 AND owner_id=$2 FOR SHARE`, identity.ConversationID, identity.OwnerID).Scan(&conversationProjectID)
	if err != nil || conversationProjectID != identity.ScopeID {
		return app.ConversationSummaryIdentity{}, domain.NewError(domain.CodeForbidden, "conversation summary scope is unavailable")
	}
	if identity.ScopeKind == app.ConversationSummaryProjectScope {
		var memberRole string
		if err := tx.QueryRow(ctx, `SELECT role,version FROM project_members
WHERE project_id=$1 AND user_id=$2 AND role IN ('owner','member') FOR SHARE`, identity.ScopeID, identity.ActorID).
			Scan(&memberRole, &identity.ScopeVersion); err != nil || memberRole == "" || identity.ScopeVersion < 1 {
			return app.ConversationSummaryIdentity{}, domain.NewError(domain.CodeForbidden, "conversation summary scope is unavailable")
		}
	}
	return identity, nil
}

func ensureConversationSummaryHead(ctx context.Context, tx pgx.Tx, identity app.ConversationSummaryIdentity, write bool) (app.ConversationSummaryHead, error) {
	if !write {
		var head app.ConversationSummaryHead
		head.Identity = identity
		err := tx.QueryRow(ctx, `SELECT authorization_version,scope_version,head_version,head_digest,
lease_operation_id,lease_owner,lease_version,lease_expires_at FROM conversation_summary_heads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.ScopeKind, identity.ScopeID).Scan(&head.Identity.AuthorizationVersion, &head.Identity.ScopeVersion,
			&head.Version, &head.Digest, &head.LeaseOperationID, &head.LeaseOwner, &head.LeaseVersion, &head.LeaseExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ConversationSummaryHead{Identity: identity}, nil
		}
		if err != nil {
			return app.ConversationSummaryHead{}, fmt.Errorf("read conversation summary head: %w", err)
		}
		return head, nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO conversation_summary_heads
(service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id,authorization_version,scope_version)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, identity.ServiceInstanceID,
		identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID,
		identity.AuthorizationVersion, identity.ScopeVersion)
	if err != nil {
		return app.ConversationSummaryHead{}, fmt.Errorf("create conversation summary head: %w", err)
	}
	var head app.ConversationSummaryHead
	head.Identity = identity
	var storedAuthorization, storedScopeVersion int64
	err = tx.QueryRow(ctx, `SELECT authorization_version,scope_version,head_version,head_digest,
lease_operation_id,lease_owner,lease_version,lease_expires_at FROM conversation_summary_heads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 FOR UPDATE`,
		identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
		identity.ScopeKind, identity.ScopeID).Scan(&storedAuthorization, &storedScopeVersion, &head.Version, &head.Digest,
		&head.LeaseOperationID, &head.LeaseOwner, &head.LeaseVersion, &head.LeaseExpiresAt)
	if err != nil {
		return app.ConversationSummaryHead{}, fmt.Errorf("lock conversation summary head: %w", err)
	}
	if storedAuthorization != identity.AuthorizationVersion || storedScopeVersion != identity.ScopeVersion {
		if head.LeaseOperationID != "" {
			if _, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='invalidated',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7
 AND state IN ('claimed','checkpointed','unknown')`, identity.ServiceInstanceID, identity.OwnerID,
				identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, head.LeaseOperationID); err != nil {
				return app.ConversationSummaryHead{}, fmt.Errorf("invalidate stale conversation compaction operation: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET authorization_version=$7,scope_version=$8,
lease_operation_id='',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
			identity.ScopeKind, identity.ScopeID, identity.AuthorizationVersion, identity.ScopeVersion)
		if err != nil {
			return app.ConversationSummaryHead{}, fmt.Errorf("refresh conversation summary authorization: %w", err)
		}
		head.Identity = identity
		head.LeaseOperationID, head.LeaseOwner, head.LeaseExpiresAt = "", "", nil
	}
	head.Identity.AuthorizationVersion = identity.AuthorizationVersion
	head.Identity.ScopeVersion = identity.ScopeVersion
	return head, nil
}

func (s *Store) ClaimConversationSummaryOperation(ctx context.Context, command app.ClaimConversationSummaryOperation) (app.ConversationSummaryOperation, bool, error) {
	if ctx == nil || command.OperationID != "" && !domain.ValidOperationID(command.OperationID) || len(command.InputSHA256) != sha256.Size ||
		strings.TrimSpace(command.LeaseOwner) != command.LeaseOwner || command.LeaseOwner == "" || len(command.LeaseOwner) > 128 ||
		command.BaselineVersion < 0 || command.LeaseDuration < time.Second || command.LeaseDuration > 30*time.Minute || command.LeaseDuration%time.Second != 0 {
		return app.ConversationSummaryOperation{}, false, domain.InvalidArgument("conversation_summary_operation", "conversation summary operation is invalid")
	}
	var operation app.ConversationSummaryOperation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, command.Claim)
		if err != nil {
			return err
		}
		head, err := ensureConversationSummaryHead(ctx, tx, identity, true)
		if err != nil {
			return err
		}
		if command.OperationID == "" {
			if head.LeaseOperationID != "" {
				candidate, candidateErr := readSummaryOperation(ctx, tx, identity, head.LeaseOperationID, false)
				if candidateErr != nil && !errors.Is(candidateErr, pgx.ErrNoRows) {
					return candidateErr
				}
				if candidateErr == nil && candidate.BaselineVersion == command.BaselineVersion &&
					bytes.Equal(candidate.InputSHA256, command.InputSHA256) &&
					(candidate.State == "claimed" || candidate.State == "checkpointed") {
					command.OperationID = candidate.OperationID
				}
			}
			if command.OperationID == "" {
				command.OperationID, err = domain.NewOperationID()
				if err != nil {
					return err
				}
			}
		}
		operation, err = readSummaryOperation(ctx, tx, identity, command.OperationID, true)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		existing := err == nil
		if existing && (!bytes.Equal(operation.InputSHA256, command.InputSHA256) || operation.BaselineVersion != command.BaselineVersion ||
			operation.Identity.AuthorizationVersion != identity.AuthorizationVersion || operation.Identity.ScopeVersion != identity.ScopeVersion) {
			return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary operation was reused with different input")
		}
		if existing && operation.State == "committed" {
			replayed = true
			return nil
		}
		if existing && operation.State == "invalidated" {
			return domain.NewError(domain.CodeConflict, "conversation summary operation was invalidated")
		}
		if head.Version != command.BaselineVersion {
			return domain.NewError(domain.CodeConflict, "conversation summary baseline changed")
		}
		if existing && operation.State == "unknown" {
			return domain.NewError(domain.CodeConflict, "conversation summary model call requires reconciliation")
		}
		if head.LeaseOperationID != "" && head.LeaseExpiresAt != nil {
			var active bool
			if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM conversation_summary_heads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
				identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID).Scan(&active); err != nil {
				return fmt.Errorf("check conversation summary lease: %w", err)
			}
			if active {
				if head.LeaseOperationID == command.OperationID && head.LeaseOwner == command.LeaseOwner && existing {
					replayed = true
					return nil
				}
				return domain.NewError(domain.CodeConflict, "conversation summary is leased by another operation")
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='expired',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND state IN ('claimed','checkpointed')`, identity.ServiceInstanceID, identity.OwnerID,
				identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, head.LeaseOperationID); err != nil {
				return fmt.Errorf("expire conversation summary lease: %w", err)
			}
		}
		var unresolved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_compaction_operations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND state='unknown')`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID).Scan(&unresolved); err != nil {
			return fmt.Errorf("check unresolved conversation summary operation: %w", err)
		}
		if unresolved {
			return domain.NewError(domain.CodeConflict, "conversation summary model call requires reconciliation")
		}
		leaseVersion := head.LeaseVersion + 1
		if existing && operation.LeaseVersion >= leaseVersion {
			leaseVersion = operation.LeaseVersion + 1
		}
		if existing {
			_, err = tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state=CASE WHEN checkpoint_through_sequence>=0 THEN 'checkpointed' ELSE 'claimed' END,
lease_owner=$8,lease_version=$9,lease_expires_at=clock_timestamp()+($10::bigint*interval '1 second'),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7`,
				identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind,
				identity.ScopeID, command.OperationID, command.LeaseOwner, leaseVersion, int64(command.LeaseDuration/time.Second))
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO conversation_compaction_operations
(service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id,operation_id,authorization_version,scope_version,
 baseline_version,input_digest,state,lease_owner,lease_version,lease_expires_at)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'claimed',$12,$13,clock_timestamp()+($14::bigint*interval '1 second'))`,
				identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind,
				identity.ScopeID, command.OperationID, identity.AuthorizationVersion, identity.ScopeVersion,
				command.BaselineVersion, command.InputSHA256, command.LeaseOwner, leaseVersion, int64(command.LeaseDuration/time.Second))
		}
		if err != nil {
			return fmt.Errorf("claim conversation summary operation: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE conversation_summary_heads SET lease_operation_id=$7,lease_owner=$8,
lease_version=$9,lease_expires_at=clock_timestamp()+($10::bigint*interval '1 second'),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind,
			identity.ScopeID, command.OperationID, command.LeaseOwner, leaseVersion, int64(command.LeaseDuration/time.Second))
		if err != nil {
			return fmt.Errorf("lease conversation summary head: %w", err)
		}
		operation, err = readSummaryOperation(ctx, tx, identity, command.OperationID, false)
		return err
	})
	return operation, replayed, err
}

func readSummaryOperation(ctx context.Context, tx pgx.Tx, identity app.ConversationSummaryIdentity, operationID string, lock bool) (app.ConversationSummaryOperation, error) {
	var operation app.ConversationSummaryOperation
	operation.Identity = identity
	query := `SELECT operation_id,authorization_version,scope_version,baseline_version,input_digest,state,lease_owner,lease_version,
lease_expires_at,checkpoint_session_id,checkpoint_through_sequence,checkpoint_digest,model_call_id
FROM conversation_compaction_operations WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7`
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
		identity.ConversationID, identity.ScopeKind, identity.ScopeID, operationID).Scan(
		&operation.OperationID, &operation.Identity.AuthorizationVersion, &operation.Identity.ScopeVersion,
		&operation.BaselineVersion, &operation.InputSHA256, &operation.State, &operation.LeaseOwner,
		&operation.LeaseVersion, &operation.LeaseExpiresAt, &operation.CheckpointSessionID,
		&operation.CheckpointThrough, &operation.CheckpointSHA256, &operation.ModelCallID)
	return operation, err
}

func (s *Store) SaveConversationSummaryCheckpoint(ctx context.Context, checkpoint app.ConversationSummaryCheckpoint) (bool, error) {
	if ctx == nil || !domain.ValidOperationID(checkpoint.OperationID) || strings.TrimSpace(checkpoint.LeaseOwner) != checkpoint.LeaseOwner ||
		checkpoint.LeaseOwner == "" || checkpoint.LeaseVersion < 1 || strings.TrimSpace(checkpoint.SessionID) != checkpoint.SessionID ||
		len(checkpoint.SessionID) == 0 || len(checkpoint.SessionID) > 128 || checkpoint.Through < 0 ||
		checkpoint.Through > 1<<53-1 || len(checkpoint.SHA256) != sha256.Size {
		return false, domain.InvalidArgument("conversation_summary_checkpoint", "conversation summary checkpoint is invalid")
	}
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, checkpoint.Claim)
		if err != nil {
			return err
		}
		_, operation, err := lockConversationSummaryOperation(ctx, tx, identity, checkpoint.OperationID, checkpoint.LeaseOwner, checkpoint.LeaseVersion)
		if err != nil {
			return err
		}
		if operation.CheckpointSessionID != "" {
			if operation.CheckpointSessionID == checkpoint.SessionID && operation.CheckpointThrough == checkpoint.Through && bytes.Equal(operation.CheckpointSHA256, checkpoint.SHA256) {
				replayed = true
				return nil
			}
			if operation.CheckpointSessionID != checkpoint.SessionID || checkpoint.Through <= operation.CheckpointThrough {
				return domain.NewError(domain.CodeConflict, "conversation summary checkpoint is not contiguous")
			}
		} else if checkpoint.Through < 0 {
			return domain.InvalidArgument("conversation_summary_checkpoint", "conversation summary checkpoint is invalid")
		}
		_, err = tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='checkpointed',checkpoint_session_id=$8,
checkpoint_through_sequence=$9,checkpoint_digest=$10,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND lease_owner=$11 AND lease_version=$12`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, checkpoint.OperationID,
			checkpoint.SessionID, checkpoint.Through, checkpoint.SHA256, checkpoint.LeaseOwner, checkpoint.LeaseVersion)
		if err != nil {
			return fmt.Errorf("save conversation summary checkpoint identity: %w", err)
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND lease_operation_id=$7 AND lease_owner=$8 AND lease_version=$9 AND lease_expires_at>clock_timestamp()`,
			identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind,
			identity.ScopeID, checkpoint.OperationID, checkpoint.LeaseOwner, checkpoint.LeaseVersion)
		if err != nil {
			return fmt.Errorf("confirm conversation summary checkpoint lease: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation summary lease is no longer current")
		}
		return nil
	})
	return replayed, err
}

func lockConversationSummaryOperation(ctx context.Context, tx pgx.Tx, identity app.ConversationSummaryIdentity,
	operationID, leaseOwner string, leaseVersion int64) (app.ConversationSummaryHead, app.ConversationSummaryOperation, error) {
	head, err := ensureConversationSummaryHead(ctx, tx, identity, true)
	if err != nil {
		return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, err
	}
	operation, err := readSummaryOperation(ctx, tx, identity, operationID, true)
	if err != nil {
		return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, err
	}
	var leaseActive bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at>clock_timestamp(),false) FROM conversation_compaction_operations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7`,
		identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, operationID).Scan(&leaseActive); err != nil {
		return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, fmt.Errorf("check conversation summary operation lease: %w", err)
	}
	if !leaseActive || head.LeaseOperationID != operationID || head.LeaseOwner != leaseOwner || head.LeaseVersion != leaseVersion ||
		operation.LeaseOwner != leaseOwner || operation.LeaseVersion != leaseVersion || operation.State != "claimed" && operation.State != "checkpointed" {
		return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, domain.NewError(domain.CodeConflict, "conversation summary operation lease is no longer current")
	}
	return head, operation, nil
}

func (s *Store) AttachConversationSummaryModelCall(ctx context.Context, claim app.ConversationRouteClaim,
	operationID, leaseOwner string, leaseVersion int64, callID string) error {
	if ctx == nil || !domain.ValidOperationID(operationID) || strings.TrimSpace(leaseOwner) != leaseOwner || leaseOwner == "" ||
		leaseVersion < 1 || strings.TrimSpace(callID) != callID || callID == "" || len(callID) > 128 {
		return domain.InvalidArgument("conversation_summary_model_call", "conversation summary model call is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, claim)
		if err != nil {
			return err
		}
		if _, _, err := lockConversationSummaryOperation(ctx, tx, identity, operationID, leaseOwner, leaseVersion); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET model_call_id=$8,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND (model_call_id='' OR model_call_id=$8)`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, operationID, callID)
		if err != nil {
			return fmt.Errorf("attach conversation summary model call: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary operation already has a different model call")
		}
		return nil
	})
}

func lockConversationSummaryModelFence(ctx context.Context, tx pgx.Tx, fence app.ConversationSummaryModelFence,
	requireRouteAuthorization bool) (app.ConversationSummaryHead, app.ConversationSummaryOperation, error) {
	identity := fence.Identity
	if requireRouteAuthorization {
		current, err := lockConversationSummaryScope(ctx, tx, fence.Claim)
		if err != nil {
			return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, err
		}
		if current != identity {
			return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, domain.NewError(domain.CodeConflict,
				"conversation summary authorization identity changed")
		}
	} else {
		claimIdentity, err := app.ConversationSummaryScopeForClaim(fence.Claim)
		if err != nil || identity.ServiceInstanceID != claimIdentity.ServiceInstanceID || identity.OwnerID != claimIdentity.OwnerID ||
			identity.ActorID != claimIdentity.ActorID || identity.ConversationID != claimIdentity.ConversationID ||
			identity.ScopeKind != claimIdentity.ScopeKind || identity.ScopeID != claimIdentity.ScopeID ||
			identity.AuthorizationVersion != claimIdentity.AuthorizationVersion ||
			(identity.ScopeKind == app.ConversationSummaryPersonalScope && identity.ScopeVersion != 0) ||
			(identity.ScopeKind == app.ConversationSummaryProjectScope && identity.ScopeVersion < 1) {
			return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, domain.NewError(domain.CodeConflict,
				"conversation summary authorization identity changed")
		}
	}
	head, operation, err := lockConversationSummaryOperation(ctx, tx, identity, fence.OperationID, fence.LeaseOwner, fence.LeaseVersion)
	if err != nil {
		return head, operation, err
	}
	if head.Version != fence.BaselineVersion || operation.BaselineVersion != fence.BaselineVersion ||
		!bytes.Equal(operation.InputSHA256, fence.InputSHA256) || operation.State != "checkpointed" ||
		operation.CheckpointSessionID != fence.SessionID || operation.CheckpointThrough != fence.ThroughSequence ||
		fence.ThroughSequence < 0 || fence.ThroughSequence > 1<<53-1 {
		return app.ConversationSummaryHead{}, app.ConversationSummaryOperation{}, domain.NewError(domain.CodeConflict,
			"conversation summary model checkpoint changed")
	}
	return head, operation, nil
}

func checkConversationSummaryModelFence(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation) error {
	fence := request.Summary
	if fence == nil || request.Task != nil || request.Route != nil || request.Source != app.ModelCallCompaction ||
		request.OwnerID != fence.Identity.ActorID || request.Principal.UserID != fence.Identity.ActorID ||
		request.Principal.Kind != identity.Kind(fence.Claim.Authorization.Kind) || request.Principal.TokenID != fence.Claim.Authorization.TokenID ||
		request.Principal.ClientInstallationID != fence.Claim.Authorization.ClientInstallationID ||
		request.ProjectID != fence.Identity.ScopeID || fence.Claim.Message.OwnerID != fence.Identity.OwnerID ||
		fence.Claim.Message.ActorID != fence.Identity.ActorID {
		return domain.NewError(domain.CodeUnauthenticated, "conversation summary model authorization is unavailable")
	}
	_, _, err := lockConversationSummaryModelFence(ctx, tx, *fence, true)
	return err
}

func attachConversationSummaryModelCallInTx(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation) error {
	if request.Summary == nil {
		return domain.NewError(domain.CodeUnauthenticated, "conversation summary model authorization is unavailable")
	}
	_, operation, err := lockConversationSummaryModelFence(ctx, tx, *request.Summary, true)
	if err != nil {
		return err
	}
	if operation.ModelCallID != "" && operation.ModelCallID != request.CallID {
		return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary operation already has a different model call")
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET model_call_id=$8,model_id=$9,provider_model=$10,
connection_version=$11,model_request_digest=$12,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND (model_call_id='' OR model_call_id=$8)
 AND (model_id='' OR (model_id=$9 AND provider_model=$10 AND connection_version=$11 AND model_request_digest=$12))`,
		request.Summary.Identity.ServiceInstanceID, request.Summary.Identity.OwnerID, request.Summary.Identity.ActorID,
		request.Summary.Identity.ConversationID, request.Summary.Identity.ScopeKind, request.Summary.Identity.ScopeID,
		request.Summary.OperationID, request.CallID, request.ModelID, request.ProviderModel, request.ConnectionVersion, request.RequestDigest)
	if err != nil {
		return fmt.Errorf("attach conversation summary model call: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary model request changed")
	}
	return nil
}

func (s *Store) RecoverConversationSummaryModelOutput(ctx context.Context,
	request app.ModelCallReservation) (app.ConversationSummaryModelOutput, bool, error) {
	if ctx == nil || request.Source != app.ModelCallCompaction || request.Summary == nil ||
		!domain.ValidUserID(request.OwnerID) || request.OwnerID != request.Summary.Identity.ActorID ||
		!domain.ValidModelConnectionID(request.ModelID) || len(request.RequestDigest) != 32 {
		return app.ConversationSummaryModelOutput{}, false, domain.InvalidArgument("conversation_summary_model_call", "conversation summary model recovery is invalid")
	}
	var output app.ConversationSummaryModelOutput
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		_, operation, err := lockConversationSummaryModelFence(ctx, tx, *request.Summary, true)
		if err != nil {
			return err
		}
		if operation.ModelCallID == "" {
			return nil
		}
		var modelID, providerModel string
		var connectionVersion *time.Time
		var requestDigest, ciphertext, digest []byte
		if err := tx.QueryRow(ctx, `SELECT model_id,provider_model,connection_version,model_request_digest,response_ciphertext,response_digest
FROM conversation_compaction_operations WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3
 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7 FOR UPDATE`,
			request.Summary.Identity.ServiceInstanceID, request.Summary.Identity.OwnerID, request.Summary.Identity.ActorID,
			request.Summary.Identity.ConversationID, request.Summary.Identity.ScopeKind, request.Summary.Identity.ScopeID,
			request.Summary.OperationID).Scan(&modelID, &providerModel, &connectionVersion, &requestDigest, &ciphertext, &digest); err != nil {
			return fmt.Errorf("read conversation summary model result: %w", err)
		}
		if operation.ModelCallID != request.CallID {
			return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary model request changed")
		}
		if modelID == "" && providerModel == "" && connectionVersion == nil && len(requestDigest) == 0 {
			return nil
		}
		if modelID != request.ModelID || providerModel != request.ProviderModel || connectionVersion == nil ||
			!connectionVersion.Equal(request.ConnectionVersion) || !bytes.Equal(requestDigest, request.RequestDigest) {
			return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary model request changed")
		}
		var state, ledgerModelID, ledgerProviderModel string
		var inputTokens, outputTokens *int64
		var ledgerDigest []byte
		err = tx.QueryRow(ctx, `SELECT state,model_id,provider_model,request_digest,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='compaction' AND call_id=$2`, request.OwnerID, request.CallID).
			Scan(&state, &ledgerModelID, &ledgerProviderModel, &ledgerDigest, &inputTokens, &outputTokens)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read recovered conversation summary model usage: %w", err)
		}
		if state != "settled" {
			return nil
		}
		if ledgerModelID != request.ModelID || ledgerProviderModel != request.ProviderModel || !bytes.Equal(ledgerDigest, request.RequestDigest) ||
			inputTokens == nil || outputTokens == nil || len(ciphertext) == 0 || len(digest) != sha256.Size {
			return domain.NewError(domain.CodeInvalidState, "settled conversation summary model result is unavailable")
		}
		output = app.ConversationSummaryModelOutput{Identity: request.Summary.Identity, OperationID: request.Summary.OperationID,
			CallID: request.CallID, ModelID: request.ModelID, Ciphertext: ciphertext, Digest: digest,
			Usage: app.ModelUsage{Known: true, InputTokens: *inputTokens, OutputTokens: *outputTokens}}
		found = true
		return nil
	})
	return output, found, err
}

func finishReconciledConversationSummaryModelCallInTx(ctx context.Context, tx pgx.Tx, ownerID, callID string) error {
	var identity app.ConversationSummaryIdentity
	var operationID, state string
	err := tx.QueryRow(ctx, `SELECT service_instance_id::text,owner_id,actor_id,conversation_id,scope_kind,scope_id,
authorization_version,scope_version,operation_id,state FROM conversation_compaction_operations
WHERE actor_id=$1 AND model_call_id=$2 FOR UPDATE`, ownerID, callID).Scan(&identity.ServiceInstanceID, &identity.OwnerID,
		&identity.ActorID, &identity.ConversationID, &identity.ScopeKind, &identity.ScopeID,
		&identity.AuthorizationVersion, &identity.ScopeVersion, &operationID, &state)
	if err != nil {
		return storeError("lock reconciled conversation summary", "conversation summary model call", err)
	}
	if state != "unknown" {
		return domain.NewError(domain.CodeInvalidState, "conversation summary model call is not unknown")
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='failed',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7 AND state='unknown'`, identity.ServiceInstanceID,
		identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, operationID)
	if err != nil {
		return fmt.Errorf("finish reconciled conversation summary model call: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "conversation summary model call changed during reconciliation")
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET lease_operation_id='',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND lease_operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID,
		identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, operationID); err != nil {
		return fmt.Errorf("release reconciled conversation summary lease: %w", err)
	}
	return nil
}

func recordConversationSummaryModelResultInTx(ctx context.Context, tx pgx.Tx, settlement app.ModelCallSettlement) error {
	if settlement.Summary == nil {
		return domain.NewError(domain.CodeUnauthenticated, "conversation summary model authorization is unavailable")
	}
	identity := settlement.Summary.Identity
	if _, err := ensureConversationSummaryHead(ctx, tx, identity, true); err != nil {
		return err
	}
	operation, err := readSummaryOperation(ctx, tx, identity, settlement.Summary.OperationID, true)
	if err != nil {
		return err
	}
	var modelCallID, modelID, providerModel string
	var modelDigest, ledgerDigest []byte
	if err := tx.QueryRow(ctx, `SELECT model_call_id,model_id,provider_model,model_request_digest FROM conversation_compaction_operations
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
		identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID).Scan(&modelCallID, &modelID, &providerModel, &modelDigest); err != nil {
		return fmt.Errorf("read settled conversation summary model identity: %w", err)
	}
	var ledgerModelID, ledgerProviderModel string
	if err := tx.QueryRow(ctx, `SELECT model_id,provider_model,request_digest FROM model_call_ledger
WHERE owner_id=$1 AND source='compaction' AND call_id=$2`, identity.ActorID, settlement.CallID).
		Scan(&ledgerModelID, &ledgerProviderModel, &ledgerDigest); err != nil {
		return fmt.Errorf("read settled conversation summary account ledger: %w", err)
	}
	if operation.Identity != identity || modelCallID != settlement.CallID || operation.ModelCallID != settlement.CallID ||
		modelID != ledgerModelID || providerModel != ledgerProviderModel || !bytes.Equal(modelDigest, ledgerDigest) {
		return domain.NewError(domain.CodeIdempotencyConflict, "conversation summary model call identity changed")
	}
	if settlement.Unknown {
		if operation.State == "unknown" {
			return nil
		}
		if operation.State != "checkpointed" {
			return domain.NewError(domain.CodeInvalidState, "conversation summary operation is not awaiting model settlement")
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='unknown',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID); err != nil {
			return fmt.Errorf("mark conversation summary model result unknown: %w", err)
		}
		_, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET lease_operation_id='',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND lease_operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID)
		if err != nil {
			return fmt.Errorf("release unknown conversation summary lease: %w", err)
		}
		return nil
	}
	if operation.State != "checkpointed" {
		return domain.NewError(domain.CodeInvalidState, "conversation summary operation is not awaiting model settlement")
	}
	if settlement.ResponseInvalid {
		if _, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='failed',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID); err != nil {
			return fmt.Errorf("reject invalid conversation summary model result: %w", err)
		}
		_, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET lease_operation_id='',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND lease_operation_id=$7`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID)
		if err != nil {
			return fmt.Errorf("release invalid conversation summary lease: %w", err)
		}
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET response_ciphertext=$8,response_digest=$9,
updated_at=clock_timestamp() WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND operation_id=$7 AND state='checkpointed'`, identity.ServiceInstanceID, identity.OwnerID,
		identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, settlement.Summary.OperationID,
		settlement.ResponseCiphertext, settlement.ResponseDigest)
	if err != nil {
		return fmt.Errorf("store encrypted conversation summary model result: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "conversation summary model result changed")
	}
	return nil
}

func (s *Store) CommitConversationSummary(ctx context.Context, command app.ConversationSummaryCommit) error {
	if ctx == nil || !domain.ValidOperationID(command.OperationID) || command.BaselineVersion < 0 ||
		strings.TrimSpace(command.LeaseOwner) != command.LeaseOwner || command.LeaseOwner == "" || command.LeaseVersion < 1 ||
		len(command.SummarySHA256) != sha256.Size || len(command.SourcesSHA256) != sha256.Size ||
		len(command.Ciphertext) == 0 || len(command.Ciphertext) > app.MaxConversationSummaryCiphertext ||
		len(command.Sources) == 0 || len(command.Sources) > app.MaxConversationSummarySources {
		return domain.InvalidArgument("conversation_summary", "conversation summary commit is invalid")
	}
	sourceDigest, err := app.ConversationSummarySourcesDigest(command.Sources)
	if err != nil || !bytes.Equal(sourceDigest, command.SourcesSHA256) {
		return domain.InvalidArgument("conversation_summary", "conversation summary source proof is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, command.Claim)
		if err != nil {
			return err
		}
		if identity != command.Identity {
			return domain.NewError(domain.CodeConflict, "conversation summary authorization changed")
		}
		previousMessageID, err := conversationSummaryPredecessorMessageID(ctx, tx, command.Claim, identity)
		if err != nil {
			return err
		}
		descendants, err := conversationRouteDescendantIDs(ctx, tx, command.Claim)
		if err != nil {
			return err
		}
		descendantSet := make(map[string]struct{}, len(descendants))
		for _, id := range descendants {
			descendantSet[id] = struct{}{}
		}
		for _, source := range command.Sources {
			if source.Kind != app.ConversationSummarySourceMessage {
				continue
			}
			if source.ID == command.Claim.Message.ID {
				return domain.NewError(domain.CodeForbidden, "conversation summary includes its current message")
			}
			if _, exists := descendantSet[source.ID]; exists {
				return domain.NewError(domain.CodeForbidden, "conversation summary includes a causal descendant")
			}
			if source.Sequence >= command.Claim.Message.Sequence && source.ID != previousMessageID {
				return domain.NewError(domain.CodeForbidden, "conversation summary includes an unproven future message")
			}
		}
		head, operation, err := lockConversationSummaryOperation(ctx, tx, identity, command.OperationID, command.LeaseOwner, command.LeaseVersion)
		if err != nil {
			return err
		}
		if operation.State != "checkpointed" || operation.BaselineVersion != command.BaselineVersion || head.Version != command.BaselineVersion ||
			operation.CheckpointSessionID == "" || operation.CheckpointThrough < 0 || len(operation.CheckpointSHA256) != sha256.Size {
			return domain.NewError(domain.CodeConflict, "conversation summary checkpoint or baseline changed")
		}
		if err := validateConversationSummarySources(ctx, tx, command.Claim, identity, previousMessageID, descendants, command.Sources); err != nil {
			return err
		}
		version := command.BaselineVersion + 1
		var previous any
		var previousSummaryExists bool
		if command.BaselineVersion > 0 {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_summary_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND version=$7 AND authorization_version=$8 AND scope_version=$9 AND state='valid')`, identity.ServiceInstanceID,
				identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID,
				command.BaselineVersion, identity.AuthorizationVersion, identity.ScopeVersion).Scan(&previousSummaryExists); err != nil {
				return fmt.Errorf("check prior conversation summary: %w", err)
			}
			if previousSummaryExists {
				previous = command.BaselineVersion
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_summary_versions
(service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id,version,previous_version,operation_id,
 authorization_version,scope_version,summary_digest,source_digest,ciphertext)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, identity.ServiceInstanceID,
			identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID,
			version, previous, command.OperationID, identity.AuthorizationVersion, identity.ScopeVersion,
			command.SummarySHA256, command.SourcesSHA256, command.Ciphertext)
		if err != nil {
			return fmt.Errorf("insert conversation summary version: %w", err)
		}
		if err := insertConversationSummarySources(ctx, tx, identity, version, command.Sources); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_summary_heads SET head_version=$10,head_digest=$11,
lease_operation_id='',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND authorization_version=$7 AND scope_version=$8 AND head_version=$9 AND lease_operation_id=$12 AND lease_owner=$13 AND lease_version=$14
 AND lease_expires_at>clock_timestamp()`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
			identity.ConversationID, identity.ScopeKind, identity.ScopeID, identity.AuthorizationVersion, identity.ScopeVersion,
			command.BaselineVersion, version, command.SummarySHA256, command.OperationID, command.LeaseOwner, command.LeaseVersion)
		if err != nil {
			return fmt.Errorf("advance conversation summary head: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation summary head changed")
		}
		result, err = tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='committed',response_ciphertext=''::bytea,response_digest=''::bytea,
lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND state='checkpointed' AND lease_owner=$8 AND lease_version=$9`, identity.ServiceInstanceID,
			identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID,
			command.OperationID, command.LeaseOwner, command.LeaseVersion)
		if err != nil {
			return fmt.Errorf("complete conversation summary operation: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation summary operation changed")
		}
		return nil
	})
}

func validateConversationSummarySources(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	identity app.ConversationSummaryIdentity, previousMessageID string, descendants []string, sources []app.ConversationSummarySource) error {
	if descendants == nil {
		descendants = []string{}
	}
	kinds := make([]string, len(sources))
	ids := make([]string, len(sources))
	objectTypes := make([]string, len(sources))
	versions := make([]int64, len(sources))
	sequences := make([]int64, len(sources))
	for index, source := range sources {
		if err := app.ValidateConversationSummarySource(source); err != nil {
			return err
		}
		kinds[index], ids[index], objectTypes[index], versions[index], sequences[index] = source.Kind, source.ID, source.ObjectType, source.Version, source.Sequence
	}
	rows, err := tx.Query(ctx, `WITH requested AS (
 SELECT * FROM unnest($1::text[],$2::text[],$3::text[],$4::bigint[],$5::bigint[]) WITH ORDINALITY
 AS source(kind,id,object_type,version,sequence,source_order)
)
SELECT source.source_order,source.kind,
 CASE WHEN source.kind='message' THEN m.id IS NOT NULL
      WHEN source.kind='task_result' THEN result.task_id IS NOT NULL
      WHEN source.kind='memory' THEN object.object_id IS NOT NULL AND object_version.revision IS NOT NULL
      ELSE false END,
 CASE WHEN source.kind='message' THEN m.content
      WHEN source.kind='task_result' THEN result.content ELSE '' END,
 CASE WHEN source.kind='message' THEN sha256(convert_to(m.content,'UTF8'))
      WHEN source.kind='task_result' THEN result.payload_sha256 ELSE NULL END
FROM requested source
LEFT JOIN conversation_messages m ON source.kind='message' AND m.id=source.id AND m.owner_id=$6
 AND m.conversation_id=$7 AND m.sequence=source.sequence AND m.delivery_status='applied'
 AND m.id<>$13 AND m.id<>ALL($15::text[]) AND (m.sequence<$12 OR m.id=$14)
 AND NOT EXISTS (SELECT 1 FROM personal_history_suppressions suppression
   WHERE suppression.service_instance_id=$11::uuid AND suppression.owner_id=$6 AND suppression.message_id=m.id)
 AND (($8='personal' AND m.actor_id=$9) OR ($8='project' AND EXISTS (
   SELECT 1 FROM project_members source_member JOIN users source_user ON source_user.id=source_member.user_id AND source_user.status='approved'
   WHERE source_member.project_id=$10 AND source_member.user_id=m.actor_id AND source_member.role IN ('owner','member'))))
LEFT JOIN tasks message_task ON message_task.id=m.task_id AND message_task.owner_id=m.owner_id AND message_task.conversation_id=m.conversation_id
LEFT JOIN conversation_result_sources result ON source.kind='task_result' AND result.task_id=source.id
 AND result.task_revision=source.version AND result.event_sequence=source.sequence AND result.owner_id=$6
 AND result.conversation_id=$7 AND result.state='published'
 AND EXISTS (SELECT 1 FROM conversation_result_batches batch JOIN conversation_messages published
   ON published.id=batch.published_message_id AND published.owner_id=batch.owner_id AND published.actor_id=batch.actor_id
     AND published.conversation_id=batch.conversation_id
   JOIN conversation_messages origin ON origin.id=result.user_message_id AND origin.owner_id=result.owner_id
     AND origin.actor_id=result.actor_id AND origin.conversation_id=result.conversation_id
   WHERE batch.message_id=result.batch_message_id AND batch.owner_id=result.owner_id AND batch.actor_id=result.actor_id
     AND batch.conversation_id=result.conversation_id AND batch.service_instance_id=$11::uuid AND batch.state='published'
     AND batch.authorization_version=result.authorization_version AND published.delivery_status='applied'
     AND published.source_kind<>'task_result_review' AND published.sequence<$12
     AND origin.role='user' AND origin.delivery_status='applied' AND origin.source_kind<>'task_result_review'
     AND (origin.sequence<$12 OR origin.id=$14) AND origin.id<>$13 AND origin.id<>ALL($15::text[])
     AND NOT EXISTS (SELECT 1 FROM personal_history_suppressions suppression
       WHERE suppression.service_instance_id=$11::uuid AND suppression.owner_id=result.owner_id
         AND suppression.message_id IN (origin.id,published.id)))
 AND EXISTS (SELECT 1 FROM tasks result_task WHERE result_task.id=result.task_id AND result_task.owner_id=$6
   AND result_task.initiator_id=result.actor_id AND result_task.conversation_id=$7 AND result_task.status='completed'
   AND result_task.current_revision=result.task_revision AND result_task.desired_revision=result.task_revision
   AND result_task.result IS NOT DISTINCT FROM result.content AND result_task.current_step IS NOT DISTINCT FROM result.turn_id
   AND result_task.auth_version=result.authorization_version
   AND result.payload_sha256=sha256(convert_to(result.content,'UTF8'))
   AND (($8='personal' AND result.actor_id=$9 AND result_task.project_id IS NULL) OR ($8='project' AND EXISTS (
     SELECT 1 FROM project_members result_member JOIN users result_user ON result_user.id=result_member.user_id AND result_user.status='approved'
     WHERE result_member.project_id=$10 AND result_member.user_id=result.actor_id AND result_member.role IN ('owner','member'))
       AND result_task.project_id=$10)))
LEFT JOIN personal_objects object ON source.kind='memory' AND source.object_type=object.object_type
 AND source.id=object.object_id AND object.service_instance_id=$11::uuid AND object.owner_id=$6
 AND $8='personal' AND $9=$6
LEFT JOIN account_object_versions object_version ON object_version.service_instance_id=$11::uuid AND object_version.owner_id=$6
 AND object_version.object_type=source.object_type AND object_version.object_id=source.id
 AND object_version.revision=source.version AND NOT object_version.is_deleted
WHERE CASE WHEN source.kind='message' THEN m.id IS NOT NULL AND (
             (m.actor_id=$9 AND (m.task_id IS NULL OR (message_task.initiator_id=$9 AND
               (($8='personal' AND message_task.project_id IS NULL) OR ($8='project' AND message_task.project_id=$10))))
              AND (m.role='user' OR (m.role='assistant' AND (
                (m.output_status IS NULL AND ((m.source_kind='conversation' AND m.task_id IS NULL) OR message_task.status='completed' OR EXISTS (
                  SELECT 1 FROM conversation_result_batches batch WHERE batch.message_id=m.id AND batch.published_message_id=m.id
                    AND batch.state='published' AND batch.service_instance_id=$11::uuid AND batch.owner_id=m.owner_id
                    AND batch.actor_id=m.actor_id AND batch.conversation_id=m.conversation_id
                    AND EXISTS (SELECT 1 FROM conversation_result_sources published_source
                      WHERE published_source.batch_message_id=batch.message_id AND published_source.state='published')
                ))) OR (m.output_status='completed' AND message_task.id=m.task_id AND message_task.initiator_id=m.actor_id AND EXISTS (
                  SELECT 1 FROM task_events result_event WHERE result_event.task_id=m.task_id
                    AND result_event.sequence=m.output_result_event_sequence AND result_event.task_revision=m.output_task_revision
                    AND result_event.event_type='task.result'
                    AND result_event.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
                ))))))
             OR ($8='project' AND m.actor_id<>$9 AND m.source_kind NOT IN ('history_import','legacy_instant')
                AND (`+projectSharedHistoryEvidenceSQL+`))
           )
           WHEN source.kind='task_result' THEN result.task_id IS NOT NULL
           WHEN source.kind='memory' THEN object.object_id IS NOT NULL AND object_version.revision IS NOT NULL
           ELSE false END
ORDER BY source.source_order`, kinds, ids, objectTypes, versions, sequences, identity.OwnerID,
		identity.ConversationID, identity.ScopeKind, identity.ActorID, identity.ScopeID, identity.ServiceInstanceID,
		claim.Message.Sequence, claim.Message.ID, previousMessageID, descendants)
	if err != nil {
		return fmt.Errorf("validate conversation summary sources: %w", err)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var order int64
		var kind string
		var content string
		var digest []byte
		var found bool
		if err := rows.Scan(&order, &kind, &found, &content, &digest); err != nil {
			return fmt.Errorf("scan conversation summary source: %w", err)
		}
		if !found || order != int64(index+1) || kind != sources[index].Kind {
			return domain.NewError(domain.CodeInvalidState, "conversation summary source order changed")
		}
		switch kind {
		case app.ConversationSummarySourceMemory:
			digest = conversationSummaryMemoryDigest(identity, sources[index])
		case app.ConversationSummarySourceMessage:
			messageDigest := sha256.Sum256([]byte(content))
			digest = messageDigest[:]
		}
		if !bytes.Equal(digest, sources[index].SHA256) {
			return domain.NewError(domain.CodeConflict, "conversation summary source changed")
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read conversation summary sources: %w", err)
	}
	if index != len(sources) {
		return domain.NewError(domain.CodeForbidden, "conversation summary source is unavailable")
	}
	return nil
}

func conversationSummaryMemoryDigest(identity app.ConversationSummaryIdentity, source app.ConversationSummarySource) []byte {
	return app.ConversationSummaryMemorySourceDigest(identity, source)
}

func (s *Store) ReleaseConversationSummaryOperation(ctx context.Context, release app.ConversationSummaryOperationRelease) error {
	if ctx == nil || !domain.ValidOperationID(release.OperationID) || strings.TrimSpace(release.LeaseOwner) != release.LeaseOwner ||
		release.LeaseOwner == "" || release.LeaseVersion < 1 ||
		(release.State != "failed" && release.State != "cancelled" && release.State != "unknown") {
		return domain.InvalidArgument("conversation_summary_operation", "conversation summary operation release is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, release.Claim)
		if err != nil {
			return err
		}
		if _, _, err := lockConversationSummaryOperation(ctx, tx, identity, release.OperationID, release.LeaseOwner, release.LeaseVersion); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state=$8,lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND operation_id=$7 AND lease_owner=$9 AND lease_version=$10`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, release.OperationID,
			release.State, release.LeaseOwner, release.LeaseVersion)
		if err != nil {
			return fmt.Errorf("release conversation summary operation: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation summary operation changed")
		}
		result, err = tx.Exec(ctx, `UPDATE conversation_summary_heads SET lease_operation_id='',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6
 AND lease_operation_id=$7 AND lease_owner=$8 AND lease_version=$9`, identity.ServiceInstanceID, identity.OwnerID,
			identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID,
			release.OperationID, release.LeaseOwner, release.LeaseVersion)
		if err != nil {
			return fmt.Errorf("release conversation summary head: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation summary lease is no longer current")
		}
		return nil
	})
}

func (s *Store) InvalidateConversationSummarySource(ctx context.Context, invalidation app.ConversationSummarySourceInvalidation) (int64, error) {
	if ctx == nil || !domain.ValidUserID(invalidation.OwnerID) || strings.TrimSpace(invalidation.ID) != invalidation.ID || invalidation.ID == "" ||
		invalidation.Version < 0 || invalidation.Sequence < 0 ||
		(invalidation.Reason != "source_deleted" && invalidation.Reason != "source_suppressed" &&
			invalidation.Reason != "scope_revoked" && invalidation.Reason != "memory_changed") {
		return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary invalidation is invalid")
	}
	switch invalidation.Kind {
	case app.ConversationSummarySourceMessage:
		if !domain.ValidMessageID(invalidation.ID) || invalidation.ObjectType != "" || invalidation.Version != 0 || invalidation.Sequence < 1 {
			return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary invalidation is invalid")
		}
	case app.ConversationSummarySourceTask:
		if !domain.ValidTaskID(invalidation.ID) || invalidation.ObjectType != "" || invalidation.Sequence < 1 {
			return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary invalidation is invalid")
		}
	case app.ConversationSummarySourceMemory:
		if invalidation.ObjectType == "" || len(invalidation.ObjectType) > 64 || invalidation.Version < 1 || invalidation.Sequence != 0 {
			return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary invalidation is invalid")
		}
	default:
		return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary invalidation is invalid")
	}
	var invalidated int64
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		invalidated, err = invalidateConversationSummarySourcesInTx(ctx, tx, invalidation)
		return err
	})
	return invalidated, err
}

func invalidateConversationSummarySourcesInTx(ctx context.Context, tx pgx.Tx,
	invalidation app.ConversationSummarySourceInvalidation) (int64, error) {
	invalidated := int64(0)
	err := tx.QueryRow(ctx, `WITH RECURSIVE affected(service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id,version) AS (
 SELECT version.service_instance_id,version.owner_id,version.actor_id,version.conversation_id,version.scope_kind,version.scope_id,version.version
 FROM conversation_summary_versions version JOIN conversation_summary_sources source
  ON source.service_instance_id=version.service_instance_id AND source.owner_id=version.owner_id
  AND source.actor_id=version.actor_id AND source.conversation_id=version.conversation_id
  AND source.scope_kind=version.scope_kind AND source.scope_id=version.scope_id AND source.summary_version=version.version
 WHERE source.owner_id=$1 AND source.source_kind=$2 AND source.source_id=$3 AND source.source_object_type=$4
  AND source.source_version=$5 AND source.source_sequence=$6
 UNION
 SELECT child.service_instance_id,child.owner_id,child.actor_id,child.conversation_id,child.scope_kind,child.scope_id,child.version
 FROM conversation_summary_versions child JOIN affected parent
  ON child.service_instance_id=parent.service_instance_id AND child.owner_id=parent.owner_id AND child.actor_id=parent.actor_id
  AND child.conversation_id=parent.conversation_id AND child.scope_kind=parent.scope_kind AND child.scope_id=parent.scope_id
  AND child.previous_version=parent.version
), invalidated AS (
 UPDATE conversation_summary_versions version SET state='invalid',invalid_reason=$7,invalidated_at=clock_timestamp()
 FROM affected WHERE version.service_instance_id=affected.service_instance_id AND version.owner_id=affected.owner_id
  AND version.actor_id=affected.actor_id AND version.conversation_id=affected.conversation_id
  AND version.scope_kind=affected.scope_kind AND version.scope_id=affected.scope_id AND version.version=affected.version
  AND version.state='valid' RETURNING version.service_instance_id,version.owner_id,version.actor_id,
   version.conversation_id,version.scope_kind,version.scope_id,version.version
), scopes AS (
 SELECT DISTINCT service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id FROM affected
), released AS (
 UPDATE conversation_compaction_operations operation SET state='invalidated',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
 FROM scopes WHERE operation.service_instance_id=scopes.service_instance_id AND operation.owner_id=scopes.owner_id
  AND operation.actor_id=scopes.actor_id AND operation.conversation_id=scopes.conversation_id
  AND operation.scope_kind=scopes.scope_kind AND operation.scope_id=scopes.scope_id
  AND operation.state IN ('claimed','checkpointed','unknown')
 RETURNING operation.service_instance_id,operation.owner_id,operation.actor_id,operation.conversation_id,operation.scope_kind,operation.scope_id
), moved_heads AS (
 UPDATE conversation_summary_heads head SET
  head_version=COALESCE((SELECT max(version.version) FROM conversation_summary_versions version
   WHERE version.service_instance_id=head.service_instance_id AND version.owner_id=head.owner_id AND version.actor_id=head.actor_id
    AND version.conversation_id=head.conversation_id AND version.scope_kind=head.scope_kind AND version.scope_id=head.scope_id
    AND version.authorization_version=head.authorization_version AND version.scope_version=head.scope_version AND version.state='valid'
    AND NOT EXISTS (SELECT 1 FROM affected WHERE affected.service_instance_id=version.service_instance_id
     AND affected.owner_id=version.owner_id AND affected.actor_id=version.actor_id AND affected.conversation_id=version.conversation_id
     AND affected.scope_kind=version.scope_kind AND affected.scope_id=version.scope_id AND affected.version=version.version)),0),
  head_digest=COALESCE((SELECT version.summary_digest FROM conversation_summary_versions version
   WHERE version.service_instance_id=head.service_instance_id AND version.owner_id=head.owner_id AND version.actor_id=head.actor_id
    AND version.conversation_id=head.conversation_id AND version.scope_kind=head.scope_kind AND version.scope_id=head.scope_id
    AND version.authorization_version=head.authorization_version AND version.scope_version=head.scope_version AND version.state='valid'
    AND NOT EXISTS (SELECT 1 FROM affected WHERE affected.service_instance_id=version.service_instance_id
     AND affected.owner_id=version.owner_id AND affected.actor_id=version.actor_id AND affected.conversation_id=version.conversation_id
     AND affected.scope_kind=version.scope_kind AND affected.scope_id=version.scope_id AND affected.version=version.version)
   ORDER BY version.version DESC LIMIT 1),''::bytea),
  lease_operation_id='',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
 FROM scopes WHERE head.service_instance_id=scopes.service_instance_id AND head.owner_id=scopes.owner_id
  AND head.actor_id=scopes.actor_id AND head.conversation_id=scopes.conversation_id
  AND head.scope_kind=scopes.scope_kind AND head.scope_id=scopes.scope_id
 RETURNING head.owner_id
)
SELECT count(*) FROM invalidated`, invalidation.OwnerID, invalidation.Kind, invalidation.ID,
		invalidation.ObjectType, invalidation.Version, invalidation.Sequence, invalidation.Reason).Scan(&invalidated)
	if err != nil {
		return 0, fmt.Errorf("invalidate conversation summary source: %w", err)
	}
	return invalidated, nil
}

func invalidateAllPersonalConversationSummariesInTx(ctx context.Context, tx pgx.Tx, ownerID string) error {
	return invalidateConversationSummaryScopeInTx(ctx, tx, "personal", "", ownerID, ownerID, "memory_changed")
}

func invalidateProjectConversationSummariesInTx(ctx context.Context, tx pgx.Tx, projectID, actorID string) error {
	return invalidateConversationSummaryScopeInTx(ctx, tx, "project", projectID, "", actorID, "scope_revoked")
}

func invalidateConversationSummaryScopeInTx(ctx context.Context, tx pgx.Tx,
	scopeKind, scopeID, ownerID, actorID, reason string) error {
	_, err := tx.Exec(ctx, `WITH scopes AS (
 SELECT service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id
 FROM conversation_summary_heads
 WHERE scope_kind=$1 AND scope_id=$2 AND ($3='' OR owner_id=$3) AND actor_id=$4
 UNION
 SELECT service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id
 FROM conversation_compaction_operations
 WHERE scope_kind=$1 AND scope_id=$2 AND ($3='' OR owner_id=$3) AND actor_id=$4
 UNION
 SELECT service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id
 FROM conversation_summary_versions
 WHERE scope_kind=$1 AND scope_id=$2 AND ($3='' OR owner_id=$3) AND actor_id=$4
), affected AS (
 SELECT version.service_instance_id,version.owner_id,version.actor_id,version.conversation_id,version.scope_kind,version.scope_id,version.version
 FROM conversation_summary_versions version JOIN scopes
  ON scopes.service_instance_id=version.service_instance_id AND scopes.owner_id=version.owner_id
  AND scopes.actor_id=version.actor_id AND scopes.conversation_id=version.conversation_id
  AND scopes.scope_kind=version.scope_kind AND scopes.scope_id=version.scope_id
 WHERE version.state='valid'
), invalidated AS (
 UPDATE conversation_summary_versions version SET state='invalid',invalid_reason=$5,invalidated_at=clock_timestamp()
 FROM affected WHERE version.service_instance_id=affected.service_instance_id AND version.owner_id=affected.owner_id
  AND version.actor_id=affected.actor_id AND version.conversation_id=affected.conversation_id
  AND version.scope_kind=affected.scope_kind AND version.scope_id=affected.scope_id AND version.version=affected.version
  AND version.state='valid'
 RETURNING version.service_instance_id,version.owner_id,version.actor_id,version.conversation_id,version.scope_kind,version.scope_id
), released AS (
 UPDATE conversation_compaction_operations operation SET state='invalidated',lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
 FROM scopes WHERE operation.service_instance_id=scopes.service_instance_id AND operation.owner_id=scopes.owner_id
  AND operation.actor_id=scopes.actor_id AND operation.conversation_id=scopes.conversation_id
  AND operation.scope_kind=scopes.scope_kind AND operation.scope_id=scopes.scope_id
  AND operation.state IN ('claimed','checkpointed','unknown')
 RETURNING operation.service_instance_id
), moved_heads AS (
 UPDATE conversation_summary_heads head SET head_version=0,head_digest='',lease_operation_id='',lease_owner='',
  lease_expires_at=NULL,updated_at=clock_timestamp()
 FROM scopes WHERE head.service_instance_id=scopes.service_instance_id AND head.owner_id=scopes.owner_id
  AND head.actor_id=scopes.actor_id AND head.conversation_id=scopes.conversation_id
  AND head.scope_kind=scopes.scope_kind AND head.scope_id=scopes.scope_id
 RETURNING head.owner_id
)
	SELECT count(*) FROM invalidated`, scopeKind, scopeID, ownerID, actorID, reason)
	if err != nil {
		return fmt.Errorf("invalidate revoked project conversation summaries: %w", err)
	}
	return nil
}

func insertConversationSummarySources(ctx context.Context, tx pgx.Tx, identity app.ConversationSummaryIdentity,
	version int64, sources []app.ConversationSummarySource) error {
	kinds := make([]string, len(sources))
	ids := make([]string, len(sources))
	objectTypes := make([]string, len(sources))
	versions := make([]int64, len(sources))
	sequences := make([]int64, len(sources))
	digests := make([][]byte, len(sources))
	for index, source := range sources {
		kinds[index], ids[index], objectTypes[index], versions[index], sequences[index], digests[index] =
			source.Kind, source.ID, source.ObjectType, source.Version, source.Sequence, source.SHA256
	}
	_, err := tx.Exec(ctx, `INSERT INTO conversation_summary_sources
(service_instance_id,owner_id,actor_id,conversation_id,scope_kind,scope_id,summary_version,source_order,source_kind,
 source_id,source_object_type,source_version,source_sequence,source_digest)
SELECT $1::uuid,$2,$3,$4,$5,$6,$7,source.source_order,source.kind,source.id,source.object_type,source.version,source.sequence,source.digest
FROM unnest($8::text[],$9::text[],$10::text[],$11::bigint[],$12::bigint[],$13::bytea[]) WITH ORDINALITY
 AS source(kind,id,object_type,version,sequence,digest,source_order)`, identity.ServiceInstanceID,
		identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID, version,
		kinds, ids, objectTypes, versions, sequences, digests)
	if err != nil {
		return fmt.Errorf("insert conversation summary sources: %w", err)
	}
	return nil
}
