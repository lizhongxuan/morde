package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *Store) ReserveModelCall(ctx context.Context, request app.ModelCallReservation) error {
	if !domain.ValidUserID(request.OwnerID) || !domain.ValidModelConnectionID(request.ModelID) || len(request.RequestDigest) != 32 ||
		request.ReservedTokens <= 0 || request.ProviderModel == "" || len(request.ProviderModel) > 256 ||
		len(request.CallID) < 8 || len(request.CallID) > 128 {
		return domain.InvalidArgument("model_call", "invalid model reservation")
	}
	// The account budget row serializes reservations and settlement for every source.
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var limit int64
		var maxConcurrent int32
		if err := tx.QueryRow(ctx, `SELECT token_limit,max_concurrent FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, request.OwnerID).Scan(&limit, &maxConcurrent); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeQuotaExceeded, "model budget is not configured")
			}
			return fmt.Errorf("lock model budget: %w", err)
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var priorDigest []byte
		err := tx.QueryRow(ctx, `SELECT request_digest FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3`,
			request.OwnerID, string(request.Source), request.CallID).Scan(&priorDigest)
		if err == nil {
			if !bytes.Equal(priorDigest, request.RequestDigest) {
				return domain.NewError(domain.CodeIdempotencyConflict, "model call key was used with different content")
			}
			return domain.NewError(domain.CodeConflict, "model call was already dispatched; reconcile its result")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find model call: %w", err)
		}
		var used int64
		var running int32
		var unknown bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN input_tokens+output_tokens ELSE reserved_tokens END),0),
  count(*) FILTER (WHERE state='dispatched'), COALESCE(bool_or(state='unknown'),false)
  FROM model_call_ledger WHERE owner_id=$1`, request.OwnerID).Scan(&used, &running, &unknown)
		if err != nil {
			return fmt.Errorf("read model usage: %w", err)
		}
		if unknown {
			return domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation")
		}
		if running >= maxConcurrent {
			return domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
		}
		if used > limit || request.ReservedTokens > limit-used {
			return domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")
		}
		var taskID any
		if request.Task != nil {
			var taskUsed, taskLimit int64
			if err := tx.QueryRow(ctx, `SELECT usage_model_tokens,budget_model_tokens FROM tasks WHERE id=$1`, request.Task.TaskID).Scan(&taskUsed, &taskLimit); err != nil {
				return domain.NewError(domain.CodeInvalidState, "task model usage is unavailable")
			}
			var pending int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_tokens),0) FROM model_call_ledger WHERE task_id=$1 AND state='dispatched'`, request.Task.TaskID).Scan(&pending); err != nil {
				return fmt.Errorf("read task model reservations: %w", err)
			}
			if taskUsed > taskLimit || pending > taskLimit-taskUsed || request.ReservedTokens > taskLimit-taskUsed-pending {
				return domain.NewError(domain.CodeQuotaExceeded, "task model token budget exceeded")
			}
			taskID = request.Task.TaskID
		}
		_, err = tx.Exec(ctx, `INSERT INTO model_call_ledger
 (owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,created_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'dispatched',clock_timestamp())`, request.OwnerID, string(request.Source), request.CallID,
			request.RequestDigest, request.ModelID, request.ProviderModel, taskID, request.ReservedTokens)
		if err != nil {
			return fmt.Errorf("reserve model call: %w", err)
		}
		return nil
	})
}

func (s *Store) BeginModelCallDispatch(ctx context.Context, request app.ModelCallReservation) error {
	if !domain.ValidUserID(request.OwnerID) || len(request.CallID) < 8 || len(request.CallID) > 128 {
		return domain.InvalidArgument("model_call", "invalid model dispatch")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, request.OwnerID).Scan(&ownerID); err != nil {
			return storeError("lock model dispatch budget", "model budget", err)
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var state, modelID string
		var priorDigest []byte
		if err := tx.QueryRow(ctx, `SELECT state,model_id,request_digest FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
			request.OwnerID, string(request.Source), request.CallID).Scan(&state, &modelID, &priorDigest); err != nil {
			return storeError("find model dispatch", "model call", err)
		}
		if state != "dispatched" || modelID != request.ModelID || !bytes.Equal(priorDigest, request.RequestDigest) {
			return domain.NewError(domain.CodeInvalidState, "model call is no longer awaiting dispatch")
		}
		if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='dispatched'`, request.OwnerID, string(request.Source), request.CallID); err != nil {
			return fmt.Errorf("mark model dispatch start: %w", err)
		}
		return nil
	})
}

func checkModelCaller(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation) error {
	var status, role string
	if err := tx.QueryRow(ctx, `SELECT status,role FROM users WHERE id=$1 FOR SHARE`, request.OwnerID).Scan(&status, &role); err != nil || status != "approved" {
		return domain.NewError(domain.CodeUnauthenticated, "model caller is unavailable")
	}
	switch request.Source {
	case app.ModelCallClient, app.ModelCallChat, app.ModelCallProbe:
		if request.Task != nil || request.Principal.UserID != request.OwnerID ||
			(request.Principal.Kind != identity.KindSession && request.Principal.Kind != identity.KindClient) {
			return domain.NewError(domain.CodeUnauthenticated, "model caller is unavailable")
		}
		if request.Source == app.ModelCallProbe && request.Principal.Kind != identity.KindSession {
			return domain.NewError(domain.CodeUnauthenticated, "model probe requires a portal session")
		}
		if request.Principal.ClientInstallationID != "" {
			if err := checkModelInstallation(ctx, tx, request.Principal.ClientInstallationID, request.OwnerID); err != nil {
				return err
			}
		} else if request.Principal.Kind == identity.KindClient {
			return domain.NewError(domain.CodeUnauthenticated, "model caller installation is unavailable")
		}
		var active bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3 AND COALESCE(client_installation_id,'')=$4
 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, request.Principal.TokenID, request.OwnerID, string(request.Principal.Kind), request.Principal.ClientInstallationID).Scan(&active)
		if err != nil || !active {
			return domain.NewError(domain.CodeUnauthenticated, "model caller credential is unavailable")
		}
	case app.ModelCallTask:
		if request.Task == nil || request.Task.OwnerID != request.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "task model authorization is unavailable")
		}
		if err := checkTaskModelFence(ctx, tx, *request.Task, request.ModelID); err != nil {
			return err
		}
	default:
		return domain.InvalidArgument("model_call", "invalid model call source")
	}
	var modelAllowed bool
	probe := request.Source == app.ModelCallProbe
	admin := request.Principal.Kind == identity.KindSession && request.Principal.ClientInstallationID == "" &&
		request.Principal.Role.IsAdmin() && identity.Role(role).IsAdmin()
	err := tx.QueryRow(ctx, `SELECT (CASE WHEN $7 THEN (owner_id=$2 OR (owner_id IS NULL AND $8))
 ELSE checked_at IS NOT NULL AND capability_text AND (NOT $4 OR capability_tools) AND (NOT $5 OR capability_vision)
   AND (owner_id IS NULL OR owner_id=$2) END) AND model_name=$3 AND updated_at=$6
 FROM model_connections WHERE id=$1 FOR SHARE`, request.ModelID, request.OwnerID, request.ProviderModel,
		request.NeedsTools, request.NeedsVision, request.ConnectionVersion, probe, admin).Scan(&modelAllowed)
	if err != nil || !modelAllowed {
		return domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	return nil
}

func checkModelInstallation(ctx context.Context, tx pgx.Tx, id, ownerID string) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() FROM client_installations
 WHERE id=$1 AND owner_id=$2 FOR SHARE`, id, ownerID).Scan(&active)
	if err != nil || !active {
		return domain.NewError(domain.CodeUnauthenticated, "model caller installation is unavailable")
	}
	return nil
}

func checkTaskModelFence(ctx context.Context, tx pgx.Tx, fence app.ModelTaskFence, modelID string) error {
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
	}
	var gateState string
	var gateEpoch int64
	if err := tx.QueryRow(ctx, `SELECT state,epoch FROM owner_execution_controls WHERE owner_id=$1 FOR SHARE`, fence.OwnerID).Scan(&gateState, &gateEpoch); err != nil || gateState != "enabled" {
		return domain.NewError(domain.CodeUnauthenticated, "task execution authorization is unavailable")
	}
	var status, location, leaseOwner, agentID, sessionID, engineVersion, profileVersion, taskModel, authKind, tokenID, installationID string
	var leaseVersion, generation, authVersion, through, executionEpoch int64
	var leaseValid, authValid bool
	var checkpointDigest []byte
	var hostID, workspaceID *string
	err := tx.QueryRow(ctx, `SELECT status,execution_location,lease_owner,lease_version,
 agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,auth_version,
 checkpoint_through_seq,checkpoint_digest,model_id,
 auth_kind,auth_token_id,COALESCE(auth_client_installation_id,''),execution_epoch,host_id,workspace_id
 FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, fence.TaskID, fence.OwnerID).Scan(&status, &location, &leaseOwner, &leaseVersion,
		&agentID, &generation, &sessionID, &engineVersion, &profileVersion, &authVersion, &through, &checkpointDigest, &taskModel,
		&authKind, &tokenID, &installationID, &executionEpoch, &hostID, &workspaceID)
	if err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "task model execution fence is unavailable")
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at>clock_timestamp(),false),COALESCE(auth_expires_at>clock_timestamp(),false)
 FROM tasks WHERE id=$1`, fence.TaskID).Scan(&leaseValid, &authValid); err != nil {
		return fmt.Errorf("check task model execution expiry: %w", err)
	}
	if status != "running" || location != "server" || !leaseValid || !authValid ||
		leaseOwner != fence.LeaseOwner || leaseVersion != fence.LeaseVersion || agentID != fence.AgentInstanceID ||
		generation != fence.AgentGeneration || sessionID != rootSessionID || authVersion != fence.AuthorizationVersion ||
		engineVersion != fence.EngineVersion || profileVersion != fence.ProfileVersion ||
		taskModel != modelID || len(checkpointDigest) != 32 || gateEpoch != executionEpoch {
		return domain.NewError(domain.CodeUnauthenticated, "task model execution fence is unavailable")
	}
	if err := lockActiveAgentLease(ctx, tx, app.AgentInstanceFence{
		TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AgentSessionID: rootSessionID,
		LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion,
		ProfileVersion: fence.ProfileVersion,
	}); err != nil {
		return err
	}
	var parentSessionID, stepID string
	var sessionThrough int64
	var sessionDigest []byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),COALESCE(step_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
		fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&parentSessionID, &stepID, &sessionThrough, &sessionDigest); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "task model Agent Session is unavailable")
	} else if err != nil {
		return fmt.Errorf("lock task model Agent Session: %w", err)
	}
	if fence.AgentSessionID == rootSessionID {
		if parentSessionID != "" || stepID != "" || sessionThrough != through || !bytes.Equal(sessionDigest, checkpointDigest) {
			return domain.NewError(domain.CodeInvalidState, "task model root checkpoint is not committed")
		}
	} else {
		underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentSessionID, rootSessionID)
		if err != nil {
			return fmt.Errorf("verify task model Agent Session ancestry: %w", err)
		}
		if parentSessionID == "" || stepID == "" || !underRoot {
			return domain.NewError(domain.CodeUnauthenticated, "task model Agent Session is outside the active tree")
		}
		var stepStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`, fence.TaskID, stepID).Scan(&stepStatus); err != nil {
			return fmt.Errorf("lock task model child Agent TaskStep: %w", err)
		}
		if stepStatus != "running" {
			return domain.NewError(domain.CodeUnauthenticated, "task model child Agent Session is not running")
		}
	}
	if sessionThrough != fence.ThroughSequence || len(sessionDigest) != 32 {
		return domain.NewError(domain.CodeInvalidState, "task model Agent Session checkpoint head is not committed")
	}
	var checkpoint bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_checkpoints c
JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
 AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
 AND i.agent_session_id=c.agent_root_session_id AND i.engine_version=c.engine_version
 AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.agent_root_session_id=$4
 AND c.through_seq=$5 AND c.digest=$6 AND c.auth_version=$7 AND c.engine_version=$8 AND c.profile_version=$9
 AND ((c.agent_generation=$10 AND i.state='active') OR (c.agent_generation<$10 AND i.state='stopped')))`,
		fence.TaskID, fence.OwnerID, fence.AgentSessionID, rootSessionID, fence.ThroughSequence, sessionDigest,
		fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration).Scan(&checkpoint)
	if err != nil || !checkpoint {
		return domain.NewError(domain.CodeInvalidState, "task checkpoint is not committed")
	}
	// Resource ownership and revocation are checked before installation credentials.
	if hostID != nil {
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT owner_id=$2 AND state NOT IN ('deleting','deleted') AND deleted_at IS NULL
 FROM hosts WHERE id=$1 FOR SHARE`, *hostID, fence.OwnerID).Scan(&allowed)
		if err != nil || !allowed {
			return domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
		}
	}
	if workspaceID != nil {
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT owner_id=$2 AND state='ready' AND deleted_at IS NULL
 FROM workspaces WHERE id=$1 FOR SHARE`, *workspaceID, fence.OwnerID).Scan(&allowed)
		if err != nil || !allowed {
			return domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization is unavailable")
		}
	}
	switch authKind {
	case string(identity.KindClient):
		if !domain.ValidClientInstallationID(installationID) {
			return domain.NewError(domain.CodeUnauthenticated, "task installation is unavailable")
		}
		return checkModelInstallation(ctx, tx, installationID, fence.OwnerID)
	case string(identity.KindSession), string(identity.KindAPIKey):
		if !domain.ValidTokenID(tokenID) {
			return domain.NewError(domain.CodeUnauthenticated, "task credential is unavailable")
		}
		var active bool
		query := `SELECT revoked_at IS NULL AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		if authKind == string(identity.KindAPIKey) {
			query = `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		}
		if err := tx.QueryRow(ctx, query, tokenID, fence.OwnerID, authKind).Scan(&active); err != nil || !active {
			return domain.NewError(domain.CodeUnauthenticated, "task credential is unavailable")
		}
		if installationID != "" {
			return checkModelInstallation(ctx, tx, installationID, fence.OwnerID)
		}
		return nil
	default:
		return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
}

func (s *Store) SettleModelCall(ctx context.Context, settlement app.ModelCallSettlement) error {
	if !domain.ValidUserID(settlement.OwnerID) || settlement.CallID == "" ||
		(!settlement.Unknown && (!settlement.Usage.Known || settlement.Usage.InputTokens < 0 || settlement.Usage.OutputTokens < 0 ||
			settlement.Usage.InputTokens > math.MaxInt64-settlement.Usage.OutputTokens)) {
		return domain.InvalidArgument("model_call", "invalid model settlement")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var limit int64
		if err := tx.QueryRow(ctx, `SELECT token_limit FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, settlement.OwnerID).Scan(&limit); err != nil {
			return fmt.Errorf("lock model settlement budget: %w", err)
		}
		var state string
		var taskID *string
		var input, output *int64
		err := tx.QueryRow(ctx, `SELECT state,task_id,input_tokens,output_tokens FROM model_call_ledger
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, settlement.OwnerID, string(settlement.Source), settlement.CallID).
			Scan(&state, &taskID, &input, &output)
		if err != nil {
			return storeError("find model settlement", "model call", err)
		}
		if state != "dispatched" {
			if state == "unknown" && !settlement.Unknown {
				_, err = tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$4,output_tokens=$5,settled_at=clock_timestamp()
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='unknown'`, settlement.OwnerID, string(settlement.Source), settlement.CallID,
					settlement.Usage.InputTokens, settlement.Usage.OutputTokens)
				if err != nil {
					return fmt.Errorf("settle late model usage: %w", err)
				}
				if taskID != nil {
					var unknown bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_call_ledger WHERE task_id=$1 AND state='unknown')`, *taskID).Scan(&unknown); err != nil {
						return fmt.Errorf("check unknown task model usage: %w", err)
					}
					if unknown {
						_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL,updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, *taskID, settlement.OwnerID)
					} else {
						_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=COALESCE((SELECT SUM(input_tokens::numeric+output_tokens::numeric)
 FROM model_call_ledger WHERE task_id=$1 AND state='settled'),0),updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, *taskID, settlement.OwnerID)
					}
					if err != nil {
						return fmt.Errorf("rebuild late task model usage: %w", err)
					}
				}
				return nil
			}
			if (state == "unknown" && settlement.Unknown) || (state == "settled" && !settlement.Unknown &&
				input != nil && output != nil && *input == settlement.Usage.InputTokens && *output == settlement.Usage.OutputTokens) {
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "model settlement was already recorded differently")
		}
		if settlement.Unknown {
			_, err = tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
 WHERE owner_id=$1 AND source=$2 AND call_id=$3`, settlement.OwnerID, string(settlement.Source), settlement.CallID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$4,output_tokens=$5,settled_at=clock_timestamp()
 WHERE owner_id=$1 AND source=$2 AND call_id=$3`, settlement.OwnerID, string(settlement.Source), settlement.CallID,
				settlement.Usage.InputTokens, settlement.Usage.OutputTokens)
		}
		if err != nil {
			return fmt.Errorf("settle model usage: %w", err)
		}
		if taskID != nil {
			if settlement.Unknown {
				_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL,updated_at=now() WHERE id=$1 AND owner_id=$2`, *taskID, settlement.OwnerID)
			} else {
				_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=usage_model_tokens+$3,updated_at=now() WHERE id=$1 AND owner_id=$2`,
					*taskID, settlement.OwnerID, settlement.Usage.InputTokens+settlement.Usage.OutputTokens)
			}
			if err != nil {
				return fmt.Errorf("settle task model usage: %w", err)
			}
		}
		return nil
	})
}

var _ app.ModelCallStore = (*Store)(nil)
