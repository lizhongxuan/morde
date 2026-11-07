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
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ReserveModelCall(ctx context.Context, request app.ModelCallReservation) error {
	if request.Source == app.ModelCallRoute || !domain.ValidUserID(request.OwnerID) || !domain.ValidModelConnectionID(request.ModelID) || len(request.RequestDigest) != 32 ||
		request.ReservedTokens <= 0 || request.ProviderModel == "" || len(request.ProviderModel) > 256 ||
		len(request.CallID) < 8 || len(request.CallID) > 128 {
		return domain.InvalidArgument("model_call", "invalid model reservation")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error { return reserveModelCallInTx(ctx, tx, request, nil) })
}

func lockModelLedgerOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:model-ledger:' || $1,0))`, ownerID); err != nil {
		return fmt.Errorf("lock model ledger owner: %w", err)
	}
	return nil
}

func lockPlatformModelBudget(ctx context.Context, tx pgx.Tx, ownerID string) (int64, int32, error) {
	var limit int64
	var maxConcurrent int32
	if err := tx.QueryRow(ctx, `SELECT token_limit,max_concurrent FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&limit, &maxConcurrent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, domain.NewError(domain.CodeQuotaExceeded, "model budget is not configured")
		}
		return 0, 0, fmt.Errorf("lock model budget: %w", err)
	}
	return limit, maxConcurrent, nil
}

func modelCallIsPersonal(ctx context.Context, tx pgx.Tx, ownerID string, source app.ModelCallSource, callID string) (bool, error) {
	var personal bool
	err := tx.QueryRow(ctx, `SELECT personal_model FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3`,
		ownerID, string(source), callID).Scan(&personal)
	if err != nil {
		return false, storeError("find model call", "model call", err)
	}
	return personal, nil
}

// reserveModelCallInTx records every call; only platform models reserve platform budgets.
func reserveModelCallInTx(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation, projectReservation *app.ProjectModelReservation) error {
	if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
		return err
	}
	if err := releaseExpiredRouteReservationsInTx(ctx, tx, request.OwnerID); err != nil {
		return err
	}
	var limit int64
	var maxConcurrent int32
	if !request.PersonalModel {
		var err error
		limit, maxConcurrent, err = lockPlatformModelBudget(ctx, tx, request.OwnerID)
		if err != nil {
			return err
		}
	}
	if err := checkModelCaller(ctx, tx, request); err != nil {
		return err
	}
	var taskProjectID string
	if request.Source == app.ModelCallTask {
		if request.Task == nil {
			return domain.NewError(domain.CodeUnauthenticated, "task model authorization is unavailable")
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`,
			request.Task.TaskID, request.Task.OwnerID, request.Task.InitiatorID).Scan(&taskProjectID); err != nil {
			return domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
		}
	}
	if request.Source == app.ModelCallTask && request.ProjectID != taskProjectID {
		return domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
	}
	projectID := request.ProjectID
	if projectID != "" && (!project.ValidID(projectID) || (!request.PersonalModel && (projectReservation == nil || projectReservation.ProjectID != projectID)) ||
		(request.PersonalModel && (projectReservation != nil || (request.Source != app.ModelCallTask && request.Source != app.ModelCallRoute)))) {
		return domain.NewError(domain.CodeInvalidState, "project model budget reservation is required")
	}
	if projectID == "" && projectReservation != nil {
		return domain.NewError(domain.CodeUnauthenticated, "project model Task attribution is unavailable")
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
	if !request.PersonalModel {
		var used int64
		var running int32
		var unknown bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN input_tokens+output_tokens ELSE reserved_tokens END),0),
  count(*) FILTER (WHERE state='dispatched'), COALESCE(bool_or(state='unknown'),false)
	  FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, request.OwnerID).Scan(&used, &running, &unknown)
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
			budgetErr := domain.NewError(domain.CodeQuotaExceeded, "task model token budget exceeded")
			budgetErr.Details["reason"] = "task_model_budget_exhausted"
			return budgetErr
		}
		taskID = request.Task.TaskID
	}
	if projectReservation != nil {
		if err := reserveProjectBudgetLedgerInTx(ctx, tx, *projectReservation); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_call_ledger
	 (owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,created_at,personal_model)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'dispatched',clock_timestamp(),$9)`, request.OwnerID, string(request.Source), request.CallID,
		request.RequestDigest, request.ModelID, request.ProviderModel, taskID, request.ReservedTokens, request.PersonalModel); err != nil {
		return fmt.Errorf("reserve model call: %w", err)
	}
	return nil
}

func (s *Store) ModelTaskProjectID(ctx context.Context, fence app.ModelTaskFence) (string, error) {
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) {
		return "", domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
	}
	var projectID string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`,
		fence.TaskID, fence.OwnerID, fence.InitiatorID).Scan(&projectID); err != nil {
		return "", domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
	}
	if projectID != "" && !project.ValidID(projectID) {
		return "", domain.NewError(domain.CodeInternal, "task model project scope is invalid")
	}
	return projectID, nil
}

func projectTaskID(request app.ModelCallReservation) any {
	if request.Task == nil {
		return nil
	}
	return request.Task.TaskID
}

func (s *Store) BeginModelCallDispatch(ctx context.Context, request app.ModelCallReservation) error {
	_, err := s.beginModelCallDispatch(ctx, request, false)
	return err
}

func (s *Store) BeginModelCallDispatchWithTextStreaming(ctx context.Context, request app.ModelCallReservation) (bool, error) {
	if request.Source != app.ModelCallTask || request.Task == nil {
		return false, domain.InvalidArgument("model_call", "task streaming dispatch is invalid")
	}
	return s.beginModelCallDispatch(ctx, request, true)
}

func (s *Store) beginModelCallDispatch(ctx context.Context, request app.ModelCallReservation, requestStreaming bool) (bool, error) {
	if request.Source == app.ModelCallRoute || !domain.ValidUserID(request.OwnerID) || len(request.CallID) < 8 || len(request.CallID) > 128 {
		return false, domain.InvalidArgument("model_call", "invalid model dispatch")
	}
	var streaming bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		personal, err := modelCallIsPersonal(ctx, tx, request.OwnerID, request.Source, request.CallID)
		if err != nil {
			return err
		}
		if personal != request.PersonalModel {
			return domain.NewError(domain.CodeInvalidState, "model call attribution changed")
		}
		if !personal {
			if _, _, err := lockPlatformModelBudget(ctx, tx, request.OwnerID); err != nil {
				return err
			}
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		projectID := request.ProjectID
		if request.Source == app.ModelCallTask {
			if request.Task == nil {
				return domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
			}
			if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`,
				request.Task.TaskID, request.Task.OwnerID, request.Task.InitiatorID).Scan(&projectID); err != nil || projectID != request.ProjectID {
				return domain.NewError(domain.CodeUnauthenticated, "task model project scope is unavailable")
			}
		}
		if projectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, request.OwnerID, projectID, project.RoleMember, false); err != nil {
				return err
			}
			if !personal {
				var reserved bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger WHERE project_id=$1
AND source=$2 AND call_id=$3 AND actor_id=$4 AND task_id IS NOT DISTINCT FROM $5::text AND model_id=$6 AND status='reserved')`,
					projectID, string(request.Source), request.CallID, request.OwnerID, projectTaskID(request), request.ModelID).Scan(&reserved); err != nil || !reserved {
					return domain.NewError(domain.CodeInvalidState, "project model budget reservation is unavailable")
				}
			}
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
		if requestStreaming {
			err := tx.QueryRow(ctx, `SELECT state='verified' FROM model_text_streaming_capabilities
WHERE model_id=$1 AND connection_version=$2 FOR SHARE`, request.ModelID, request.ConnectionVersion).Scan(&streaming)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check task streaming dispatch proof: %w", err)
			}
			if streaming {
				fence := request.Task
				var taskRevision int64
				if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`, fence.TaskID, fence.OwnerID).Scan(&taskRevision); err != nil {
					return fmt.Errorf("read task streaming dispatch revision: %w", err)
				}
				root := fence.AgentRootSessionID
				if root == "" {
					root = fence.AgentSessionID
				}
				if _, err := tx.Exec(ctx, `INSERT INTO model_text_streaming_dispatches
(owner_id,source,call_id,task_id,task_owner_id,initiator_id,task_revision,model_id,connection_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,lease_owner,lease_version,authorization_version,through_sequence,engine_version,profile_version)
VALUES ($1,'task',$2,$3,$4,$1,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, request.OwnerID, request.CallID,
					fence.TaskID, fence.OwnerID, taskRevision, request.ModelID, request.ConnectionVersion, fence.AgentInstanceID, fence.AgentGeneration,
					fence.AgentSessionID, root, fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion, fence.ThroughSequence,
					fence.EngineVersion, fence.ProfileVersion); err != nil {
					return fmt.Errorf("record task streaming dispatch proof: %w", err)
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='dispatched'`, request.OwnerID, string(request.Source), request.CallID); err != nil {
			return fmt.Errorf("mark model dispatch start: %w", err)
		}
		return nil
	})
	return streaming, err
}

// shortcut: the existing ledger has no distinct pre-dispatch state; call only
// when the gateway can prove no provider request was sent.
func (s *Store) AbandonModelCallReservation(ctx context.Context, request app.ModelCallReservation) error {
	if !domain.ValidUserID(request.OwnerID) || !domain.ValidModelConnectionID(request.ModelID) ||
		len(request.RequestDigest) != 32 || request.ReservedTokens <= 0 || len(request.CallID) < 8 || len(request.CallID) > 128 ||
		(request.ProjectID != "" && !project.ValidID(request.ProjectID)) ||
		(request.Source != app.ModelCallClient && request.Source != app.ModelCallChat && request.Source != app.ModelCallTask && request.Source != app.ModelCallProbe) ||
		(request.Source == app.ModelCallTask) != (request.Task != nil) ||
		(request.Task != nil && request.Task.InitiatorID != request.OwnerID) ||
		(request.ProjectID != "" && request.Source == app.ModelCallProbe) {
		return domain.InvalidArgument("model_call", "invalid model reservation release")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		personal, err := modelCallIsPersonal(ctx, tx, request.OwnerID, request.Source, request.CallID)
		if err != nil {
			return err
		}
		if personal != request.PersonalModel {
			return domain.NewError(domain.CodeInvalidState, "model call attribution changed")
		}
		if !personal {
			if _, _, err := lockPlatformModelBudget(ctx, tx, request.OwnerID); err != nil {
				return err
			}
		}
		expectedTaskID := ""
		if request.Task != nil {
			expectedTaskID = request.Task.TaskID
		}
		if request.ProjectID != "" && !personal {
			if err := lockProjectByIDInTx(ctx, tx, request.ProjectID); err != nil {
				return err
			}
			var state, actorID, modelID, providerModel, taskID string
			var reservedInput, reservedOutput int64
			err := tx.QueryRow(ctx, `SELECT status,actor_id,model_id,rate_model,COALESCE(task_id,''),reserved_input_tokens,reserved_output_tokens
FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
				request.ProjectID, string(request.Source), request.CallID).
				Scan(&state, &actorID, &modelID, &providerModel, &taskID, &reservedInput, &reservedOutput)
			if err != nil {
				return storeError("find project model reservation release", "project model call", err)
			}
			if state != "reserved" || actorID != request.OwnerID || modelID != request.ModelID || providerModel != request.ProviderModel || taskID != expectedTaskID ||
				reservedInput > math.MaxInt64-reservedOutput || reservedInput+reservedOutput != request.ReservedTokens {
				return domain.NewError(domain.CodeConflict, "project model reservation is no longer undispatched")
			}
		}
		var state, modelID, providerModel, taskID string
		var priorDigest []byte
		var reservedTokens int64
		if err := tx.QueryRow(ctx, `SELECT state,model_id,provider_model,request_digest,COALESCE(task_id,''),reserved_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, request.OwnerID, string(request.Source), request.CallID).
			Scan(&state, &modelID, &providerModel, &priorDigest, &taskID, &reservedTokens); err != nil {
			return storeError("find model reservation release", "model call", err)
		}
		if state != "dispatched" || modelID != request.ModelID || providerModel != request.ProviderModel ||
			taskID != expectedTaskID || reservedTokens != request.ReservedTokens || !bytes.Equal(priorDigest, request.RequestDigest) {
			return domain.NewError(domain.CodeConflict, "model reservation is no longer undispatched")
		}
		if request.ProjectID != "" && !personal {
			result, err := tx.Exec(ctx, `DELETE FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3 AND status='reserved'`,
				request.ProjectID, string(request.Source), request.CallID)
			if err != nil {
				return fmt.Errorf("release project model budget reservation: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "project model reservation changed before release")
			}
		} else {
			var projectHold bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger WHERE actor_id=$1 AND source=$2 AND call_id=$3)`,
				request.OwnerID, string(request.Source), request.CallID).Scan(&projectHold); err != nil {
				return fmt.Errorf("check project model reservation before release: %w", err)
			}
			if projectHold {
				return domain.NewError(domain.CodeConflict, "project model reservation identity is unavailable")
			}
		}
		result, err := tx.Exec(ctx, `DELETE FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3
AND state='dispatched' AND model_id=$4 AND request_digest=$5`, request.OwnerID, string(request.Source), request.CallID,
			request.ModelID, request.RequestDigest)
		if err != nil {
			return fmt.Errorf("release model token reservation: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "model reservation changed before release")
		}
		return nil
	})
}

// ValidateTaskModelDelivery rechecks the same Task/Agent/model authorization
// after provider usage has settled and before any response reaches DSH.
func (s *Store) ValidateTaskModelDelivery(ctx context.Context, request app.ModelCallReservation) error {
	if ctx == nil || request.Source != app.ModelCallTask || request.Task == nil || request.OwnerID != request.Task.InitiatorID {
		return domain.InvalidArgument("model_call", "task model delivery identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return checkModelCaller(ctx, tx, request)
	})
}

// Recovery reads no new user data. It only keeps a previously dispatched
// provider response open while the same authorized model connection exists.
func (s *Store) ValidateTaskModelRecoveryConfiguration(ctx context.Context, request app.ModelCallReservation) error {
	if ctx == nil || request.Source != app.ModelCallTask || request.Task == nil || request.OwnerID != request.Task.InitiatorID {
		return domain.InvalidArgument("model_call", "task model recovery identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var allowed, personal bool
		err := tx.QueryRow(ctx, `SELECT checked_at IS NOT NULL AND capability_text AND (NOT $4 OR capability_tools)
 AND (NOT $5 OR capability_vision) AND (owner_id IS NULL OR owner_id=$2) AND model_name=$3 AND updated_at=$6,
 owner_id IS NOT NULL FROM model_connections WHERE id=$1 FOR SHARE`, request.ModelID, request.OwnerID, request.ProviderModel,
			request.NeedsTools, request.NeedsVision, request.ConnectionVersion).Scan(&allowed, &personal)
		if err != nil || !allowed || personal != request.PersonalModel {
			return domain.NewError(domain.CodeNotFound, "task model recovery configuration is unavailable")
		}
		return nil
	})
}

func checkModelCaller(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation) error {
	var status, role string
	userLock := " FOR SHARE"
	if request.Source == app.ModelCallTask && request.Task != nil {
		permit, err := projectWorkspaceModelPermit(ctx, *request.Task)
		if err != nil {
			return err
		}
		if permit != nil && request.OwnerID == permit.actorID {
			// The active outer Workspace transaction already holds this user.
			userLock = ""
		}
	}
	if err := tx.QueryRow(ctx, `SELECT status,role FROM users WHERE id=$1`+userLock, request.OwnerID).Scan(&status, &role); err != nil || status != "approved" {
		return domain.NewError(domain.CodeUnauthenticated, "model caller is unavailable")
	}
	switch request.Source {
	case app.ModelCallClient, app.ModelCallChat, app.ModelCallProbe:
		if request.Task != nil || request.Principal.UserID != request.OwnerID ||
			(request.Principal.Kind != identity.KindSession && request.Principal.Kind != identity.KindClient) {
			return domain.NewError(domain.CodeUnauthenticated, "model caller is unavailable")
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
		if request.Task == nil || request.Task.InitiatorID != request.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "task model authorization is unavailable")
		}
		if err := checkTaskModelFence(ctx, tx, *request.Task, request.ModelID); err != nil {
			return err
		}
	case app.ModelCallRoute:
		if request.Task != nil || request.Route == nil || request.Route.Claim.Message.ActorID != request.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "route model authorization is unavailable")
		}
		if err := checkConversationRouteModelFence(ctx, tx, *request.Route, request.ModelID); err != nil {
			return err
		}
	default:
		return domain.InvalidArgument("model_call", "invalid model call source")
	}
	var modelAllowed, personal bool
	probe := request.Source == app.ModelCallProbe
	admin := request.Principal.Kind == identity.KindSession && request.Principal.ClientInstallationID == "" &&
		request.Principal.Role.IsAdmin() && identity.Role(role).IsAdmin()
	err := tx.QueryRow(ctx, `SELECT (CASE WHEN $7 THEN (owner_id=$2 OR (owner_id IS NULL AND $8))
 ELSE checked_at IS NOT NULL AND capability_text AND (NOT $4 OR capability_tools) AND (NOT $5 OR capability_vision)
   AND (owner_id IS NULL OR owner_id=$2) END) AND model_name=$3 AND updated_at=$6, owner_id IS NOT NULL
 FROM model_connections WHERE id=$1 FOR SHARE`, request.ModelID, request.OwnerID, request.ProviderModel,
		request.NeedsTools, request.NeedsVision, request.ConnectionVersion, probe, admin).Scan(&modelAllowed, &personal)
	if err != nil || !modelAllowed || personal != request.PersonalModel {
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

func projectWorkspaceModelPermit(ctx context.Context, fence app.ModelTaskFence) (*workspaceMutationPermit, error) {
	permit := projectWorkspaceTaskPermit(ctx, fence.TaskID, fence.InitiatorID)
	if permit == nil {
		return nil, nil
	}
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
	}
	if permit.taskOwnerID != fence.OwnerID || permit.leaseOwner != fence.LeaseOwner || permit.leaseVersion != fence.LeaseVersion ||
		permit.agentID != fence.AgentInstanceID || permit.agentGeneration != fence.AgentGeneration ||
		permit.agentSessionID != rootSessionID || permit.authorization.Version != fence.AuthorizationVersion {
		return nil, domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization changed")
	}
	return permit, nil
}

func checkTaskModelFence(ctx context.Context, tx pgx.Tx, fence app.ModelTaskFence, modelID string) error {
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
	}
	projectID, err := lockTaskInitiatorInTx(ctx, tx, fence.TaskID, fence.OwnerID, fence.InitiatorID)
	if err != nil {
		return err
	}
	// Keep project membership and the selected Workspace resource/grant locked
	// before the Task row; revocation follows the same resource-first order.
	workspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, false)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden ||
			domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeConflict {
			return domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization is unavailable")
		}
		return err
	}
	if workspaceFence.projectID != projectID || workspaceFence.actorID != fence.InitiatorID {
		return domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization is unavailable")
	}
	hostTarget, err := lockTaskModelHostTarget(ctx, tx, fence, workspaceFence)
	if err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, fence.InitiatorID, nil)
	if err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "task execution authorization is unavailable")
	}
	var status, location, leaseOwner, agentID, sessionID, engineVersion, profileVersion, taskModel, authKind, tokenID, installationID string
	var authScopes []string
	var leaseVersion, generation, authVersion, through, executionEpoch, currentRevision, desiredRevision int64
	var leaseValid, authValid bool
	var checkpointDigest []byte
	var hostID, workspaceID *string
	var taskInitiatorID, taskProjectID string
	err = tx.QueryRow(ctx, `SELECT initiator_id,COALESCE(project_id,''),status,execution_location,lease_owner,lease_version,
 agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,auth_version,
 checkpoint_through_seq,checkpoint_digest,model_id,
 auth_kind,auth_token_id,auth_scopes,COALESCE(auth_client_installation_id,''),execution_epoch,host_id,workspace_id,current_revision,desired_revision
	FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, fence.TaskID, fence.OwnerID).Scan(
		&taskInitiatorID, &taskProjectID, &status, &location, &leaseOwner, &leaseVersion,
		&agentID, &generation, &sessionID, &engineVersion, &profileVersion, &authVersion, &through, &checkpointDigest, &taskModel,
		&authKind, &tokenID, &authScopes, &installationID, &executionEpoch, &hostID, &workspaceID, &currentRevision, &desiredRevision)
	if err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "task model execution fence is unavailable")
	}
	if projectID != "" {
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, workspaceFence); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at>clock_timestamp(),false),
COALESCE(auth_kind='schedule_task' OR auth_expires_at>clock_timestamp(),false)
 FROM tasks WHERE id=$1`, fence.TaskID).Scan(&leaseValid, &authValid); err != nil {
		return fmt.Errorf("check task model execution expiry: %w", err)
	}
	if taskInitiatorID != fence.InitiatorID || taskProjectID != projectID ||
		status != "running" || location != "server" || !leaseValid || !authValid || currentRevision != desiredRevision ||
		leaseOwner != fence.LeaseOwner || leaseVersion != fence.LeaseVersion || agentID != fence.AgentInstanceID ||
		generation != fence.AgentGeneration || sessionID != rootSessionID || authVersion != fence.AuthorizationVersion ||
		engineVersion != fence.EngineVersion || profileVersion != fence.ProfileVersion ||
		taskModel != modelID || len(checkpointDigest) != 32 || gate.epoch != executionEpoch {
		return domain.NewError(domain.CodeUnauthenticated, "task model execution fence is unavailable")
	}
	if err := lockActiveAgentLease(ctx, tx, app.AgentInstanceFence{
		TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID, AgentInstanceID: fence.AgentInstanceID,
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
	currentHostID := ""
	if hostID != nil {
		currentHostID = *hostID
	}
	if err := verifyTaskModelHostAuthorization(ctx, tx, fence, hostTarget, currentHostID); err != nil {
		return err
	}
	if workspaceID != nil {
		workspaceOwner := fence.InitiatorID
		if projectID != "" {
			workspaceOwner = workspaceFence.workspaceOwnerID
		}
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT owner_id=$2 AND state='ready' AND deleted_at IS NULL
		FROM workspaces WHERE id=$1 FOR SHARE`, *workspaceID, workspaceOwner).Scan(&allowed)
		if err != nil || !allowed {
			return domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization is unavailable")
		}
	}
	permit, err := projectWorkspaceModelPermit(ctx, fence)
	if err != nil {
		return err
	}
	if permit != nil {
		// The outer Workspace operation already holds these credential rows.
		// Confirm the exact Task and authorization before reusing its locks;
		// locking them again can deadlock behind a queued revocation.
		current, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: fence.TaskID, OwnerID: fence.OwnerID})
		if err != nil || !projectWorkspaceAuthorizationPermit(ctx, current) {
			return domain.NewError(domain.CodeUnauthenticated, "task Workspace authorization changed")
		}
		return checkAgentCheckpointAuthorization(ctx, tx, fence.InitiatorID, fence.TaskID, authKind, tokenID, authScopes, installationID)
	}
	switch authKind {
	case string(identity.KindScheduleTask):
		if !scheduleTaskScopeStringsValid(authScopes) || !schedule.ValidRunID(tokenID) || installationID != "" || hostID == nil {
			return domain.NewError(domain.CodeUnauthenticated, "scheduled Task authorization is unavailable")
		}
		var linked bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM schedule_runs r JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
  WHERE r.id=$1 AND r.owner_id=$2 AND r.task_id=$3 AND s.host_id=$4
)`, tokenID, fence.InitiatorID, fence.TaskID, *hostID).Scan(&linked)
		if err != nil || !linked {
			return domain.NewError(domain.CodeUnauthenticated, "scheduled Task authorization is unavailable")
		}
		return nil
	case string(identity.KindClient):
		if !domain.ValidClientInstallationID(installationID) {
			return domain.NewError(domain.CodeUnauthenticated, "task installation is unavailable")
		}
		return checkModelInstallation(ctx, tx, installationID, fence.InitiatorID)
	case string(identity.KindSession), string(identity.KindAPIKey):
		if !domain.ValidTokenID(tokenID) {
			return domain.NewError(domain.CodeUnauthenticated, "task credential is unavailable")
		}
		var active bool
		query := `SELECT revoked_at IS NULL AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		if authKind == string(identity.KindAPIKey) {
			query = `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		}
		if err := tx.QueryRow(ctx, query, tokenID, fence.InitiatorID, authKind).Scan(&active); err != nil || !active {
			return domain.NewError(domain.CodeUnauthenticated, "task credential is unavailable")
		}
		if installationID != "" {
			return checkModelInstallation(ctx, tx, installationID, fence.InitiatorID)
		}
		return nil
	default:
		return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
}

// Lock the known Host identity rather than re-reading the Task after locking
// its gates; a concurrent bind is rejected when the Task row is rechecked.
func lockTaskModelHostTarget(ctx context.Context, tx pgx.Tx, fence app.ModelTaskFence,
	workspaceFence projectTaskWorkspaceFence) (agentHostTarget, error) {
	target := agentHostTarget{TaskHostID: workspaceFence.hostID, TaskActorID: workspaceFence.actorID}
	var ownerID string
	if target.TaskHostID != "" {
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1`, target.TaskHostID).Scan(&ownerID); err != nil {
			return agentHostTarget{}, domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
		}
	}
	if workspaceFence.projectID != "" && workspaceFence.workspaceID != "" {
		if target.TaskHostID != "" && ownerID != workspaceFence.workspaceOwnerID {
			return agentHostTarget{}, domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
		}
	} else if err := lockTaskHostOwnerGates(ctx, tx, fence.InitiatorID, ownerID); err != nil {
		return agentHostTarget{}, err
	}
	target, err := lockAgentHostTargetIdentity(ctx, tx, fence.TaskID, fence.OwnerID, target.TaskHostID, target.TaskActorID)
	if err != nil || target.OwnerID != ownerID {
		return agentHostTarget{}, domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
	}
	return target, nil
}

// Revocation locks the Host owner's execution gate before the Host row.
func lockTaskHostOwnerGates(ctx context.Context, tx pgx.Tx, actorID, hostOwnerID string) error {
	first, second := actorID, hostOwnerID
	if second != "" && second < first {
		first, second = second, first
	}
	lock := func(ownerID string) error {
		permit, _ := ctx.Value(workspaceMutationPermitKey{}).(*workspaceMutationPermit)
		if permit != nil && permit.active.Load() && (ownerID == permit.actorID || ownerID == permit.workspaceOwnerID) {
			// The outer Workspace operation holds this gate until its nested
			// transaction returns. Do not wait behind a queued revocation.
			return nil
		}
		_, err := lockOwnerExecutionControl(ctx, tx, ownerID, false)
		return err
	}
	if err := lock(first); err != nil {
		return err
	}
	if second != "" && second != first {
		if err := lock(second); err != nil {
			return err
		}
	}
	return nil
}

// Host availability is a tool concern. Model access to an already-bound shared
// Host only requires the same independently authorized member grant captured
// by the Agent; stopped runtimes and control transitions do not block text.
func verifyTaskModelHostAuthorization(ctx context.Context, tx pgx.Tx, fence app.ModelTaskFence,
	target agentHostTarget, hostID string) error {
	unavailable := func() error {
		return domain.NewError(domain.CodeUnauthenticated, "task Host authorization is unavailable")
	}
	if target.TaskHostID != hostID || target.TaskActorID != fence.InitiatorID {
		return unavailable()
	}
	if hostID == "" {
		return nil
	}
	if target.OwnerID == "" || target.Deleted || target.State == "deleting" || target.State == "deleted" {
		return unavailable()
	}
	if target.OwnerID == fence.InitiatorID {
		return nil
	}
	allowed, err := hasControlPermission(ctx, tx, hostID, target.OwnerID, fence.InitiatorID, string(host.PermissionDesktopControl))
	if err != nil {
		return fmt.Errorf("verify task model Host grant: %w", err)
	}
	if !allowed || !target.HasActiveGrant || target.GrantVersion == "" || target.GrantCreatedAt.IsZero() {
		return unavailable()
	}
	fingerprint := fingerprintHostMemberGrant(target.GrantCreatedAt, target.GrantVersion, target.GrantPermissions)
	var matches bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(host_id,'')=$4 AND COALESCE(host_owner_id,'')=$5
 AND COALESCE(host_actor_id,'')=$6 AND COALESCE(host_permission,'')=$7 AND COALESCE(host_authorization_kind,'')='member'
 AND COALESCE(host_grant_fingerprint,'')=$8 FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 AND agent_instance_id=$9 FOR SHARE`,
		fence.TaskID, fence.OwnerID, fence.AgentGeneration, hostID, target.OwnerID, fence.InitiatorID,
		string(host.PermissionDesktopControl), fingerprint, fence.AgentInstanceID).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !matches {
		return unavailable()
	}
	if err != nil {
		return fmt.Errorf("verify task model Agent Host grant: %w", err)
	}
	return nil
}

func (s *Store) SettleModelCall(ctx context.Context, settlement app.ModelCallSettlement) error {
	if settlement.Source == app.ModelCallRoute || !domain.ValidUserID(settlement.OwnerID) || settlement.CallID == "" ||
		(!settlement.Unknown && (!settlement.Usage.Known || settlement.Usage.InputTokens < 0 || settlement.Usage.OutputTokens < 0 ||
			settlement.Usage.InputTokens > math.MaxInt64-settlement.Usage.OutputTokens)) {
		return domain.InvalidArgument("model_call", "invalid model settlement")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, settlement.OwnerID); err != nil {
			return err
		}
		personal, err := modelCallIsPersonal(ctx, tx, settlement.OwnerID, settlement.Source, settlement.CallID)
		if err != nil {
			return err
		}
		if !personal {
			if _, _, err := lockPlatformModelBudget(ctx, tx, settlement.OwnerID); err != nil {
				return err
			}
		}
		var state string
		var taskID *string
		var input, output *int64
		err = tx.QueryRow(ctx, `SELECT state,task_id,input_tokens,output_tokens FROM model_call_ledger
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
						_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL,updated_at=clock_timestamp() WHERE id=$1 AND initiator_id=$2`, *taskID, settlement.OwnerID)
					} else {
						_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=COALESCE((SELECT SUM(input_tokens::numeric+output_tokens::numeric)
 FROM model_call_ledger WHERE task_id=$1 AND state='settled'),0),updated_at=clock_timestamp() WHERE id=$1 AND initiator_id=$2`, *taskID, settlement.OwnerID)
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
				_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL,updated_at=now() WHERE id=$1 AND initiator_id=$2`, *taskID, settlement.OwnerID)
			} else {
				_, err = tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=usage_model_tokens+$3,updated_at=now() WHERE id=$1 AND initiator_id=$2`,
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
