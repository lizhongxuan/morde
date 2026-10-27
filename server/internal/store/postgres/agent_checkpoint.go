package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *Store) SaveAgentCheckpoint(ctx context.Context, value app.StoredAgentCheckpoint) (bool, error) {
	fence := value.Fence
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || fence.AgentInstanceID == "" ||
		fence.AgentSessionID == "" || fence.AgentGeneration < 1 || fence.AuthorizationVersion < 1 ||
		fence.LeaseOwner == "" || fence.LeaseVersion < 1 || fence.EngineVersion == "" || fence.ProfileVersion == "" ||
		value.From < 0 || value.Through < value.From || len(value.Digest) != 32 || len(value.Ciphertext) == 0 || len(value.Ciphertext) > (1<<20)+64 {
		return false, domain.InvalidArgument("checkpoint", "checkpoint fields are invalid")
	}
	var replayed bool
	// Lock authorization and the task with the checkpoint row in one transaction.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := requireOwnerExecutionEnabled(ctx, tx, fence.OwnerID, nil)
		if err != nil {
			return err
		}
		var userStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, fence.OwnerID).Scan(&userStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
			}
			return fmt.Errorf("lock checkpoint owner: %w", err)
		}
		if userStatus != string(identity.StatusApproved) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		var through int64
		var authKind, tokenID, installationID string
		err = tx.QueryRow(ctx, `SELECT checkpoint_through_seq,auth_kind,auth_token_id,COALESCE(auth_client_installation_id,'')
FROM tasks
WHERE id=$1 AND owner_id=$2 AND status='running' AND execution_location='server' AND execution_epoch=$3
  AND lease_owner=$4 AND lease_version=$5 AND lease_expires_at>clock_timestamp()
  AND agent_instance_id=$6 AND agent_generation=$7 AND agent_session_id=$8
  AND auth_version=$9 AND auth_expires_at>clock_timestamp()
  AND agent_engine_version=$10 AND agent_profile_version=$11
FOR UPDATE`, fence.TaskID, fence.OwnerID, gate.epoch, fence.LeaseOwner, fence.LeaseVersion,
			fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID, fence.AuthorizationVersion,
			fence.EngineVersion, fence.ProfileVersion).Scan(&through, &authKind, &tokenID, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint task: %w", err)
		}
		if err := lockActiveAgentLease(ctx, tx, app.AgentInstanceFence{
			TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentInstanceID: fence.AgentInstanceID,
			AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
			AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion,
			ProfileVersion: fence.ProfileVersion,
		}); err != nil {
			return err
		}
		if err := checkAgentCheckpointAuthorization(ctx, tx, fence.OwnerID, authKind, tokenID, installationID); err != nil {
			return err
		}
		var previousFrom, previousThrough int64
		var previousDigest []byte
		var previousEngine, previousProfile string
		err = tx.QueryRow(ctx, `SELECT from_seq,through_seq,digest,engine_version,profile_version FROM task_agent_checkpoints
WHERE task_id=$1 AND agent_session_id=$2 AND (from_seq=$3 OR through_seq=$4)`,
			fence.TaskID, fence.AgentSessionID, value.From, value.Through).Scan(&previousFrom, &previousThrough, &previousDigest, &previousEngine, &previousProfile)
		if err == nil {
			if previousFrom != value.From || previousThrough != value.Through || !bytes.Equal(previousDigest, value.Digest) ||
				previousEngine != fence.EngineVersion || previousProfile != fence.ProfileVersion {
				return domain.NewError(domain.CodeIdempotencyConflict, "checkpoint sequence was reused with different content")
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check existing checkpoint: %w", err)
		}
		if through == int64(^uint64(0)>>1) || value.From != through+1 {
			return domain.NewError(domain.CodeConflict, "checkpoint event sequence is not contiguous")
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,from_seq,through_seq,digest,ciphertext,
 engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			value.From, value.Through, value.Digest, value.Ciphertext, fence.EngineVersion, fence.ProfileVersion,
			fence.AuthorizationVersion, fence.LeaseVersion)
		if err != nil {
			return fmt.Errorf("save agent checkpoint: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=$2,checkpoint_digest=$3,updated_at=clock_timestamp()
WHERE id=$1`, fence.TaskID, value.Through, value.Digest); err != nil {
			return fmt.Errorf("advance task checkpoint: %w", err)
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ReadAgentCheckpointPage(ctx context.Context, request app.AgentCheckpointPageRequest) (app.StoredAgentCheckpoint, error) {
	fence := request.Fence
	if err := fence.Validate(); err != nil || request.From < 0 || request.Through < request.From || len(request.ThroughDigest) != 32 {
		return app.StoredAgentCheckpoint{}, domain.InvalidArgument("checkpoint_page", "checkpoint page request is invalid")
	}
	var result app.StoredAgentCheckpoint
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion || current.CheckpointThrough < request.Through ||
			current.CheckpointThrough == request.Through && !bytes.Equal(current.CheckpointDigest, request.ThroughDigest) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint page authorization is unavailable")
		}
		instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != fence || instance.State != "active" {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint page authorization is unavailable")
		}
		if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
			return err
		}
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_agent_checkpoints c
 JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
  AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
  AND i.agent_session_id=c.agent_session_id AND i.engine_version=c.engine_version
  AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
 WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.through_seq=$4 AND c.digest=$5
  AND c.auth_version=$6 AND c.engine_version=$7 AND c.profile_version=$8
  AND c.agent_generation<$9 AND i.state='stopped'
)`, fence.TaskID, fence.OwnerID, fence.AgentSessionID, request.Through, request.ThroughDigest,
			fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration).Scan(&pinned); err != nil {
			return fmt.Errorf("verify pinned checkpoint history: %w", err)
		}
		if !pinned {
			return domain.NewError(domain.CodeInvalidState, "pinned checkpoint history is unavailable")
		}
		err = tx.QueryRow(ctx, `SELECT c.agent_instance_id,c.agent_generation,c.agent_session_id,c.from_seq,c.through_seq,c.digest,c.ciphertext,
 c.engine_version,c.profile_version,c.auth_version,c.lease_version,i.lease_owner
FROM task_agent_checkpoints c
JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
 AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
 AND i.agent_session_id=c.agent_session_id AND i.engine_version=c.engine_version
 AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.from_seq=$4
 AND c.through_seq<=$5 AND c.auth_version=$6 AND c.engine_version=$7 AND c.profile_version=$8
 AND c.agent_generation<$9 AND i.state='stopped'`, fence.TaskID, fence.OwnerID, fence.AgentSessionID,
			request.From, request.Through, fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration).
			Scan(&result.Fence.AgentInstanceID, &result.Fence.AgentGeneration, &result.Fence.AgentSessionID,
				&result.From, &result.Through, &result.Digest, &result.Ciphertext,
				&result.Fence.EngineVersion, &result.Fence.ProfileVersion, &result.Fence.AuthorizationVersion,
				&result.Fence.LeaseVersion, &result.Fence.LeaseOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeInvalidState, "checkpoint page is unavailable")
		}
		if err != nil {
			return fmt.Errorf("read checkpoint page: %w", err)
		}
		result.Fence.TaskID, result.Fence.OwnerID = fence.TaskID, fence.OwnerID
		return nil
	})
	return result, err
}

func checkAgentCheckpointAuthorization(ctx context.Context, tx pgx.Tx, ownerID, kind, tokenID, installationID string) error {
	if kind == string(identity.KindClient) {
		if !domain.ValidClientInstallationID(installationID) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint installation is unavailable")
		}
	} else if kind == string(identity.KindSession) || kind == string(identity.KindAPIKey) {
		var active bool
		query := `SELECT revoked_at IS NULL AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		if kind == string(identity.KindAPIKey) {
			query = `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		}
		err := tx.QueryRow(ctx, query, tokenID, ownerID, kind).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint credential is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint credential: %w", err)
		}
	} else {
		return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
	}
	if installationID != "" {
		var active bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp()
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID, ownerID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint installation is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint installation: %w", err)
		}
	}
	return nil
}
