package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type taskModelResponseRow struct {
	taskOwnerID          string
	initiatorID          string
	projectID            string
	personalModel        bool
	agentInstanceID      string
	agentGeneration      int64
	agentSessionID       string
	agentRootSessionID   string
	leaseOwner           string
	leaseVersion         int64
	authorizationVersion int64
	throughSequence      int64
	engineVersion        string
	profileVersion       string
	modelID              string
	providerModel        string
	connectionVersion    time.Time
	requestDigest        []byte
	ciphertext           []byte
	digest               []byte
}

func (s *Store) RecoverTaskModelResponse(ctx context.Context, request app.ModelCallReservation) (app.TaskModelResponseRecord, bool, error) {
	if ctx == nil || request.Source != app.ModelCallTask || request.Task == nil || request.OwnerID != request.Task.InitiatorID ||
		len(request.RequestDigest) != 32 || request.CallID == "" {
		return app.TaskModelResponseRecord{}, false, domain.InvalidArgument("task_model_response", "task model response recovery is invalid")
	}
	var recovered app.TaskModelResponseRecord
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var state, modelID, providerModel string
		var taskID *string
		var requestDigest []byte
		var inputTokens, outputTokens *int64
		err := tx.QueryRow(ctx, `SELECT state,task_id,model_id,provider_model,request_digest,input_tokens,output_tokens
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`, request.OwnerID, request.CallID).
			Scan(&state, &taskID, &modelID, &providerModel, &requestDigest, &inputTokens, &outputTokens)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Task model response ledger: %w", err)
		}
		if taskID == nil || *taskID != request.Task.TaskID || modelID != request.ModelID || providerModel != request.ProviderModel ||
			!bytes.Equal(requestDigest, request.RequestDigest) {
			return domain.NewError(domain.CodeIdempotencyConflict, "Task model response call identity changed")
		}
		if state != "settled" || inputTokens == nil || outputTokens == nil {
			return domain.NewError(domain.CodeInvalidState, "Task model call has no recoverable settled response")
		}
		row, err := readTaskModelResponseInTx(ctx, tx, request.OwnerID, request.CallID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeInvalidState, "settled Task model response is unavailable and requires reconciliation")
		}
		if err != nil {
			return err
		}
		if row.taskOwnerID != request.Task.OwnerID || row.initiatorID != request.OwnerID || row.projectID != request.ProjectID ||
			row.personalModel != request.PersonalModel || row.agentSessionID != request.Task.AgentSessionID ||
			row.agentRootSessionID != request.Task.AgentRootSessionID || row.authorizationVersion != request.Task.AuthorizationVersion ||
			row.throughSequence != request.Task.ThroughSequence || row.engineVersion != request.Task.EngineVersion ||
			row.profileVersion != request.Task.ProfileVersion || row.modelID != request.ModelID || row.providerModel != request.ProviderModel ||
			!row.connectionVersion.Equal(request.ConnectionVersion) || !bytes.Equal(row.requestDigest, request.RequestDigest) ||
			row.agentGeneration < 1 || row.agentGeneration > request.Task.AgentGeneration || len(row.ciphertext) == 0 ||
			len(row.ciphertext) > app.MaxTaskModelResponseCiphertext || len(row.digest) != 32 ||
			row.agentGeneration == request.Task.AgentGeneration && row.agentInstanceID != request.Task.AgentInstanceID {
			return domain.NewError(domain.CodeIdempotencyConflict, "Task model response recovery fence changed")
		}
		if row.agentGeneration < request.Task.AgentGeneration {
			var stopped bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_agent_instances
  WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 AND agent_instance_id=$4
    AND agent_session_id=$5 AND engine_version=$6 AND profile_version=$7 AND auth_version=$8
    AND state='stopped' AND stop_receipt IS NOT NULL AND stop_receipt_digest IS NOT NULL
)`, request.Task.TaskID, request.Task.OwnerID, row.agentGeneration, row.agentInstanceID,
				row.agentRootSessionID, row.engineVersion, row.profileVersion, row.authorizationVersion).Scan(&stopped); err != nil {
				return fmt.Errorf("verify stopped Task model response generation: %w", err)
			}
			if !stopped {
				return domain.NewError(domain.CodeInvalidState, "prior Task model response generation has not been confirmed stopped")
			}
		}
		recovered = app.TaskModelResponseRecord{AgentInstanceID: row.agentInstanceID, AgentGeneration: row.agentGeneration,
			Ciphertext: append([]byte(nil), row.ciphertext...), Digest: append([]byte(nil), row.digest...)}
		found = true
		return nil
	})
	return recovered, found, err
}

func readTaskModelResponseInTx(ctx context.Context, tx pgx.Tx, ownerID, callID string) (taskModelResponseRow, error) {
	var row taskModelResponseRow
	err := tx.QueryRow(ctx, `SELECT task_owner_id,initiator_id,project_id,personal_model,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,lease_owner,lease_version,authorization_version,through_sequence,engine_version,
profile_version,model_id,provider_model,connection_version,request_digest,response_ciphertext,response_digest
FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`, ownerID, callID).
		Scan(&row.taskOwnerID, &row.initiatorID, &row.projectID, &row.personalModel, &row.agentInstanceID, &row.agentGeneration,
			&row.agentSessionID, &row.agentRootSessionID, &row.leaseOwner, &row.leaseVersion, &row.authorizationVersion,
			&row.throughSequence, &row.engineVersion, &row.profileVersion, &row.modelID, &row.providerModel,
			&row.connectionVersion, &row.requestDigest, &row.ciphertext, &row.digest)
	if err != nil {
		return taskModelResponseRow{}, err
	}
	return row, nil
}

var _ app.TaskModelResponseStore = (*Store)(nil)

func recordTaskModelResponseInTx(ctx context.Context, tx pgx.Tx, settlement app.ModelCallSettlement) (bool, error) {
	request := settlement.TaskCall
	if request == nil {
		if settlement.ResponseInvalid {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_model_call_responses
WHERE owner_id=$1 AND source='task' AND call_id=$2)`, settlement.OwnerID, settlement.CallID).Scan(&exists); err != nil {
				return false, fmt.Errorf("check invalid Task model response record: %w", err)
			}
			if exists {
				return false, domain.NewError(domain.CodeIdempotencyConflict, "Task model response was already stored")
			}
		}
		return true, nil
	}
	if request.Source != app.ModelCallTask || request.Task == nil || request.OwnerID != settlement.OwnerID ||
		request.CallID != settlement.CallID || len(settlement.ResponseCiphertext) == 0 || len(settlement.ResponseCiphertext) > app.MaxTaskModelResponseCiphertext ||
		len(settlement.ResponseDigest) != 32 || !settlement.Usage.Known {
		return false, domain.InvalidArgument("task_model_response", "Task model response settlement is invalid")
	}
	var state, taskID, modelID, providerModel, projectID string
	var personal bool
	var requestDigest []byte
	var inputTokens, outputTokens *int64
	if err := tx.QueryRow(ctx, `SELECT state,COALESCE(task_id,''),model_id,provider_model,request_digest,personal_model,input_tokens,output_tokens
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`, request.OwnerID, request.CallID).
		Scan(&state, &taskID, &modelID, &providerModel, &requestDigest, &personal, &inputTokens, &outputTokens); err != nil {
		return false, fmt.Errorf("verify Task model response ledger: %w", err)
	}
	if state != "settled" || taskID != request.Task.TaskID || modelID != request.ModelID || providerModel != request.ProviderModel ||
		personal != request.PersonalModel || !bytes.Equal(requestDigest, request.RequestDigest) || inputTokens == nil || outputTokens == nil ||
		*inputTokens != settlement.Usage.InputTokens || *outputTokens != settlement.Usage.OutputTokens {
		return false, domain.NewError(domain.CodeIdempotencyConflict, "Task model response settlement identity changed")
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`, request.Task.TaskID, request.Task.OwnerID, request.OwnerID).Scan(&projectID); err != nil {
		return false, domain.NewError(domain.CodeUnauthenticated, "Task model response Task is unavailable")
	}
	if projectID != request.ProjectID {
		return false, domain.NewError(domain.CodeUnauthenticated, "Task model response project scope changed")
	}
	if err := checkModelCaller(ctx, tx, *request); err != nil {
		return false, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO task_model_call_responses
(owner_id,source,call_id,task_id,task_owner_id,initiator_id,project_id,personal_model,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,lease_owner,lease_version,authorization_version,through_sequence,engine_version,
 profile_version,model_id,provider_model,connection_version,request_digest,response_ciphertext,response_digest)
VALUES ($1,'task',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		request.OwnerID, request.CallID, request.Task.TaskID, request.Task.OwnerID, request.OwnerID, request.ProjectID, request.PersonalModel,
		request.Task.AgentInstanceID, request.Task.AgentGeneration, request.Task.AgentSessionID, request.Task.AgentRootSessionID,
		request.Task.LeaseOwner, request.Task.LeaseVersion, request.Task.AuthorizationVersion, request.Task.ThroughSequence,
		request.Task.EngineVersion, request.Task.ProfileVersion, request.ModelID, request.ProviderModel,
		request.ConnectionVersion, request.RequestDigest, settlement.ResponseCiphertext, settlement.ResponseDigest)
	if err != nil {
		return false, fmt.Errorf("persist Task model response: %w", err)
	}
	return true, nil
}

func verifySettledTaskModelResponseInTx(ctx context.Context, tx pgx.Tx, settlement app.ModelCallSettlement) error {
	if settlement.TaskCall == nil {
		if settlement.ResponseInvalid {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_model_call_responses
WHERE owner_id=$1 AND source='task' AND call_id=$2)`, settlement.OwnerID, settlement.CallID).Scan(&exists); err != nil {
				return fmt.Errorf("check settled Task response identity: %w", err)
			}
			if exists {
				return domain.NewError(domain.CodeIdempotencyConflict, "Task model response was already stored")
			}
		}
		return nil
	}
	request := settlement.TaskCall
	row, err := readTaskModelResponseInTx(ctx, tx, settlement.OwnerID, settlement.CallID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeInvalidState, "settled Task model call has no stored response")
	}
	if err != nil {
		return err
	}
	if request.Task == nil || row.taskOwnerID != request.Task.OwnerID || row.initiatorID != request.OwnerID || row.projectID != request.ProjectID ||
		row.personalModel != request.PersonalModel || row.agentInstanceID != request.Task.AgentInstanceID ||
		row.agentGeneration != request.Task.AgentGeneration || row.agentSessionID != request.Task.AgentSessionID ||
		row.agentRootSessionID != request.Task.AgentRootSessionID || row.leaseOwner != request.Task.LeaseOwner ||
		row.leaseVersion != request.Task.LeaseVersion || row.authorizationVersion != request.Task.AuthorizationVersion ||
		row.throughSequence != request.Task.ThroughSequence || row.engineVersion != request.Task.EngineVersion ||
		row.profileVersion != request.Task.ProfileVersion || row.modelID != request.ModelID || row.providerModel != request.ProviderModel ||
		!row.connectionVersion.Equal(request.ConnectionVersion) || !bytes.Equal(row.requestDigest, request.RequestDigest) ||
		!bytes.Equal(row.digest, settlement.ResponseDigest) || len(row.ciphertext) == 0 {
		return domain.NewError(domain.CodeIdempotencyConflict, "Task model response was already recorded differently")
	}
	return nil
}

func taskModelResponseFenceRejected(err error) bool {
	switch domain.CodeOf(err) {
	case domain.CodeUnauthenticated, domain.CodeForbidden, domain.CodeNotFound, domain.CodeConflict,
		domain.CodeInvalidState, domain.CodeInvalidArgument:
		return true
	default:
		return false
	}
}
