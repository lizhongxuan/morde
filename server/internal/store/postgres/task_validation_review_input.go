package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

type taskValidationReviewScope struct {
	Objective       string
	Result          string
	RootSessionID   string
	ProjectID       string
	ExecutionCallID string
}

func loadTaskValidationReviewScopeInTx(ctx context.Context, tx pgx.Tx,
	claim app.TaskValidationReviewClaim) (taskValidationReviewScope, bool, error) {
	if !app.ValidTaskValidationReviewClaim(claim) {
		return taskValidationReviewScope{}, false, domain.InvalidArgument("task_validation_review", "Task validation review claim is invalid")
	}
	var scope taskValidationReviewScope
	err := tx.QueryRow(ctx, `SELECT task.objective,task.result,COALESCE(task.agent_session_id,''),
COALESCE(task.project_id,''),COALESCE(review.execution_model_call_id,'')
FROM tasks task
JOIN task_validation_reviews review ON review.task_id=task.id AND review.owner_id=task.owner_id
JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
  AND event.task_revision=review.task_revision AND event.event_type='task.result'
  AND event.result_content_sha256=review.result_sha256
WHERE task.id=$1 AND task.owner_id=$2 AND task.initiator_id=$3 AND task.conversation_id=$4
  AND task.status='completed' AND task.current_revision=$5 AND task.desired_revision=$5
  AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=$6
  AND review.task_revision=$5 AND review.result_event_sequence=$7 AND review.result_sha256=$6
  AND review.state='processing' AND review.lease_owner=$8 AND review.claim_generation=$9
  AND review.lease_expires_at>clock_timestamp()
FOR UPDATE OF task,review`, claim.TaskID, claim.OwnerID, claim.InitiatorID, claim.ConversationID,
		claim.TaskRevision, claim.ResultSHA256, claim.ResultEventSequence, claim.LeaseOwner, claim.ClaimGeneration).
		Scan(&scope.Objective, &scope.Result, &scope.RootSessionID, &scope.ProjectID, &scope.ExecutionCallID)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskValidationReviewScope{}, false, nil
	}
	if err != nil {
		return taskValidationReviewScope{}, false, fmt.Errorf("read current Task validation review input: %w", err)
	}
	if scope.Objective == "" || len(scope.Objective) > app.MaxAssistantMessageBytes || !utf8.ValidString(scope.Objective) ||
		strings.ContainsRune(scope.Objective, '\x00') || scope.Result == "" || len(scope.Result) > app.MaxAssistantMessageBytes ||
		!utf8.ValidString(scope.Result) || strings.ContainsRune(scope.Result, '\x00') {
		return taskValidationReviewScope{}, false, domain.NewError(domain.CodeInvalidState, "Task validation review input exceeds its bounds")
	}
	if scope.ProjectID != "" && !project.ValidID(scope.ProjectID) {
		return taskValidationReviewScope{}, false, domain.NewError(domain.CodeInvalidState, "Task validation review project is invalid")
	}
	return scope, true, nil
}

func (s *Store) RecoverTaskValidationReviewExecutionModelResponse(ctx context.Context,
	claim app.TaskValidationReviewClaim) (app.TaskValidationReviewExecutionModelResponse, bool, error) {
	if ctx == nil || !app.ValidTaskValidationReviewClaim(claim) {
		return app.TaskValidationReviewExecutionModelResponse{}, false,
			domain.InvalidArgument("task_validation_review", "Task validation review execution model claim is invalid")
	}
	var result app.TaskValidationReviewExecutionModelResponse
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		scope, current, err := loadTaskValidationReviewScopeInTx(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !current || scope.ExecutionCallID == "" || scope.RootSessionID == "" {
			return nil
		}
		var taskOwnerID, initiatorID, projectID, instanceID, sessionID, rootSessionID, leaseOwner string
		var modelID, endpoint, providerModel, reportedModel, engineVersion, profileVersion string
		var generation, leaseVersion, authorizationVersion, through int64
		var personalModel bool
		var connectionVersion time.Time
		var requestDigest, ciphertext, responseDigest []byte
		err = tx.QueryRow(ctx, `SELECT response.task_owner_id,response.initiator_id,response.project_id,response.personal_model,
response.agent_instance_id,response.agent_generation,response.agent_session_id,response.agent_root_session_id,
response.lease_owner,response.lease_version,response.authorization_version,response.through_sequence,
response.engine_version,response.profile_version,response.model_id,COALESCE(response.provider_endpoint,''),
response.provider_model,COALESCE(response.provider_reported_model,''),response.connection_version,response.request_digest,
response.response_ciphertext,response.response_digest
FROM task_model_call_responses response
JOIN model_call_ledger ledger ON ledger.owner_id=response.owner_id AND ledger.source=response.source
  AND ledger.call_id=response.call_id AND ledger.task_id=response.task_id AND ledger.state='settled'
WHERE response.owner_id=$1 AND response.source='task' AND response.call_id=$2 AND response.task_id=$3
  AND response.task_owner_id=$4 AND response.initiator_id=$1
  AND response.agent_session_id=$5 AND response.agent_root_session_id=$5
FOR SHARE OF response,ledger`, claim.InitiatorID, scope.ExecutionCallID, claim.TaskID, claim.OwnerID, scope.RootSessionID).
			Scan(&taskOwnerID, &initiatorID, &projectID, &personalModel, &instanceID, &generation, &sessionID, &rootSessionID,
				&leaseOwner, &leaseVersion, &authorizationVersion, &through, &engineVersion, &profileVersion, &modelID,
				&endpoint, &providerModel, &reportedModel, &connectionVersion, &requestDigest, &ciphertext, &responseDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read persisted Task execution model response: %w", err)
		}
		if taskOwnerID != claim.OwnerID || initiatorID != claim.InitiatorID || projectID != scope.ProjectID ||
			!domain.ValidModelConnectionID(modelID) || endpoint == "" || providerModel == "" || !app.ValidProviderReportedModel(reportedModel) ||
			connectionVersion.IsZero() || len(requestDigest) != 32 || len(ciphertext) == 0 ||
			len(ciphertext) > app.MaxTaskModelResponseCiphertext || len(responseDigest) != 32 ||
			!domain.ValidTaskID(claim.TaskID) || !domain.ValidModelConnectionID(modelID) ||
			leaseOwner == "" || generation < 1 || leaseVersion < 1 || authorizationVersion < 1 || through < 0 ||
			instanceID == "" || sessionID != scope.RootSessionID || rootSessionID != scope.RootSessionID || engineVersion == "" || profileVersion == "" {
			return domain.NewError(domain.CodeInvalidState, "persisted Task execution model identity is incomplete")
		}
		fence := &app.ModelTaskFence{TaskID: claim.TaskID, OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID,
			LeaseOwner: leaseOwner, LeaseVersion: leaseVersion, AgentInstanceID: instanceID, AgentGeneration: generation,
			AgentSessionID: sessionID, AgentRootSessionID: rootSessionID, AuthorizationVersion: authorizationVersion,
			ThroughSequence: through, EngineVersion: engineVersion, ProfileVersion: profileVersion}
		result.Reservation = app.ModelCallReservation{Source: app.ModelCallTask, Task: fence, OwnerID: claim.InitiatorID,
			ProjectID: projectID, PersonalModel: personalModel, ModelID: modelID, ProviderEndpoint: endpoint,
			ProviderModel: providerModel, ConnectionVersion: connectionVersion, CallID: scope.ExecutionCallID,
			RequestDigest: append([]byte(nil), requestDigest...)}
		result.Response = app.TaskModelResponseRecord{AgentInstanceID: instanceID, AgentGeneration: generation,
			ProviderEndpoint: endpoint, ProviderReportedModel: reportedModel,
			Ciphertext: append([]byte(nil), ciphertext...), Digest: append([]byte(nil), responseDigest...)}
		found = true
		return nil
	})
	return result, found, err
}

var _ app.TaskValidationReviewExecutionModelStore = (*Store)(nil)
