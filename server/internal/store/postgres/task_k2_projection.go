package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

// currentTaskValidationVerdictInTx returns a reviewed verdict only when the
// verdict is still tied to the exact persisted execution response and the
// Task's current authorization remains valid. Read snapshots cannot take the
// row locks used by requireCurrentTaskAuthorization, so the authorization
// check below mirrors its read predicates without mutating state.
func currentTaskValidationVerdictInTx(ctx context.Context, tx pgx.Tx, taskID, ownerID string, revision, eventSequence int64, resultSHA256 []byte) (app.TaskValidationState, bool, error) {
	var state app.TaskValidationState
	err := tx.QueryRow(ctx, `SELECT review.validation_state
FROM task_validation_reviews review
JOIN tasks task ON task.id=review.task_id AND task.owner_id=review.owner_id
JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
  AND event.task_revision=review.task_revision AND event.event_type='task.result'
  AND event.result_content_sha256=review.result_sha256
JOIN task_model_call_responses response ON response.owner_id=task.initiator_id
  AND response.source='task' AND response.call_id=review.execution_model_call_id AND response.task_id=task.id
  AND response.task_owner_id=task.owner_id AND response.initiator_id=task.initiator_id
  AND response.project_id=COALESCE(task.project_id,'')
  AND response.agent_instance_id=task.agent_instance_id AND response.agent_generation=task.agent_generation
  AND response.agent_session_id=task.agent_session_id AND response.agent_root_session_id=task.agent_session_id
  AND response.authorization_version=task.auth_version
JOIN model_call_ledger ledger ON ledger.owner_id=response.owner_id AND ledger.source=response.source
  AND ledger.call_id=response.call_id AND ledger.task_id=response.task_id AND ledger.state='settled'
JOIN task_agent_instances agent ON agent.task_id=response.task_id AND agent.owner_id=response.task_owner_id
  AND agent.agent_instance_id=response.agent_instance_id AND agent.agent_generation=response.agent_generation
  AND agent.agent_session_id=response.agent_session_id AND agent.auth_version=response.authorization_version
  AND agent.engine_version=response.engine_version AND agent.profile_version=response.profile_version
WHERE review.task_id=$1 AND review.owner_id=$2 AND review.task_revision=$3 AND review.state='reviewed'
  AND ($4::bigint=0 OR review.result_event_sequence=$4)
  AND (COALESCE(octet_length($5::bytea),0)=0 OR review.result_sha256=$5)
  AND task.status='completed' AND task.current_revision=review.task_revision AND task.desired_revision=review.task_revision
  AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
  AND task_validation_artifact_evidence_current(review.task_id,review.owner_id,review.artifact_content_evidence)`,
		taskID, ownerID, revision, eventSequence, resultSHA256).Scan(&state)
	if err == pgx.ErrNoRows {
		return app.TaskValidationPending, false, nil
	}
	if err != nil {
		return app.TaskValidationPending, false, fmt.Errorf("read current Task validation verdict provenance: %w", err)
	}
	if !app.ValidTaskValidationState(state) {
		return app.TaskValidationPending, false, domain.NewError(domain.CodeInvalidState, "Task validation state is invalid")
	}
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return app.TaskValidationPending, false, fmt.Errorf("read Task authorization for validation projection: %w", err)
	}
	authorized, err := taskValidationAuthorizationCurrentInTx(ctx, tx, row)
	if err != nil {
		return app.TaskValidationPending, false, err
	}
	if !authorized {
		return app.TaskValidationPending, false, nil
	}
	return state, true, nil
}

// This predicate mirrors the persistent facts checked by
// requireCurrentTaskAuthorization without row locks, which are unavailable in
// read-only Task snapshots. Schedule scope-set validation is performed in Go
// from the same Task row after the linked ScheduleRun check succeeds.
const taskValidationCurrentAuthorizationPredicate = `task.auth_token_id<>'' AND task.auth_version>=1
  AND EXISTS (SELECT 1 FROM users auth_user WHERE auth_user.id=task.initiator_id AND auth_user.status='approved')
  AND (
    (task.project_id IS NULL AND task.initiator_id=task.owner_id)
    OR (task.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM projects project
      JOIN project_members member ON member.project_id=project.id AND member.user_id=task.initiator_id
      JOIN users member_user ON member_user.id=member.user_id AND member_user.status='approved'
      WHERE project.id=task.project_id AND member.role IN ('owner','member')
    ))
  )
  AND (
    (task.auth_kind='schedule_task' AND task.auth_client_installation_id IS NULL AND EXISTS (
      SELECT 1 FROM schedule_runs run
      JOIN schedules schedule ON schedule.id=run.schedule_id AND schedule.owner_id=run.owner_id
      JOIN tasks run_task ON run_task.id=run.task_id AND run_task.owner_id=run.owner_id AND run_task.initiator_id=run.owner_id
      WHERE run.id=task.auth_token_id AND run.owner_id=task.initiator_id AND run.task_id=task.id
        AND run_task.auth_kind='schedule_task' AND run_task.auth_token_id=run.id
        AND run_task.execution_location='server' AND schedule.host_id IS NOT DISTINCT FROM run_task.host_id
        AND run_task.host_id IS NOT DISTINCT FROM task.host_id
    ))
    OR (task.auth_kind<>'schedule_task' AND EXISTS (
      SELECT 1 FROM tokens token
      WHERE token.id=task.auth_token_id AND token.owner_id=task.initiator_id AND token.kind=task.auth_kind
        AND token.client_installation_id IS NOT DISTINCT FROM task.auth_client_installation_id
        AND token.expires_at>clock_timestamp()
        AND (task.auth_expires_at IS NULL OR task.auth_expires_at>clock_timestamp())
        AND (token.revoked_at IS NULL OR
          (token.client_installation_id IS NOT NULL AND task.created_at<token.revoked_at))
        AND (token.kind<>'client' OR token.client_installation_id IS NOT NULL)
        AND (token.client_installation_id IS NULL OR EXISTS (
          SELECT 1 FROM client_installations installation
          WHERE installation.id=token.client_installation_id AND installation.owner_id=token.owner_id
            AND installation.revoked_at IS NULL AND installation.expires_at>clock_timestamp()
        ))
    ))
  )`

// taskValidationAuthorizationCurrentInTx mirrors requireCurrentTaskAuthorization
// for immutable read snapshots. It intentionally does not refresh, extend, or
// otherwise mutate credentials; any missing or malformed authorization fact
// causes the verdict to remain pending.
func taskValidationAuthorizationCurrentInTx(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) (bool, error) {
	if value.AuthTokenID == "" || value.AuthVersion < 1 || !domain.ValidUserID(value.InitiatorID) {
		return false, nil
	}
	if value.AuthKind == string(identity.KindScheduleTask) &&
		(!scheduleTaskScopeStringsValidForHost(value.AuthScopes, value.HostID.Valid) || !schedule.ValidRunID(value.AuthTokenID) || value.AuthClientInstallationID.Valid) {
		return false, nil
	}
	var current bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM tasks task WHERE task.id=$1 AND task.owner_id=$2 AND task.initiator_id=$3
    AND task.auth_token_id=$4 AND task.auth_version=$5 AND `+taskValidationCurrentAuthorizationPredicate+`
)`, value.ID, value.OwnerID, value.InitiatorID, value.AuthTokenID, value.AuthVersion).Scan(&current)
	if err != nil {
		return false, fmt.Errorf("check Task validation current authorization: %w", err)
	}
	return current, nil
}

// lockTaskValidationAuthorizationCurrentInTx is used by receipt publication,
// which runs in a writable transaction and must serialize authorization
// revocation with publishing a validation verdict. Detail/list/EventChain
// snapshots remain read-only and use the predicate above instead.
func lockTaskValidationAuthorizationCurrentInTx(ctx context.Context, tx pgx.Tx, taskID, ownerID string) (bool, error) {
	value, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return false, fmt.Errorf("read Task authorization for result receipt: %w", err)
	}
	return requireCurrentTaskAuthorization(ctx, tx, value)
}

func loadTaskK2Projection(ctx context.Context, tx pgx.Tx, taskID string, revision int64) ([]app.Dependency, []app.ChildJob, *app.ResultReview, app.TaskValidationSummary, error) {
	currentRevision := revision
	projections, err := loadTaskK2Projections(ctx, tx, []task.Task{{ID: taskID, CurrentRevision: &currentRevision}})
	if err != nil {
		return nil, nil, nil, app.TaskValidationSummary{}, err
	}
	projection, ok := projections[taskID]
	if !ok {
		return nil, nil, nil, app.TaskValidationSummary{}, domain.NewError(domain.CodeInvalidState, "Task K2 projection is missing")
	}
	return projection.Dependencies, projection.ChildJobs, projection.ResultReview, projection.Validation, nil
}

// loadTaskK2Projections hydrates every summary from the caller's existing
// repeatable-read snapshot. It performs one query per K2 relation, independent
// of the page length, and only exposes child/review records from each Task's
// current revision.
func loadTaskK2Projections(ctx context.Context, tx pgx.Tx, items []task.Task) (map[string]app.TaskK2Projection, error) {
	projections := make(map[string]app.TaskK2Projection, len(items))
	if ctx == nil || tx == nil {
		return nil, domain.InvalidArgument("task_projection", "Task projection context is invalid")
	}
	if len(items) == 0 {
		return projections, nil
	}
	ids := make([]string, 0, len(items))
	revisions := make([]int64, 0, len(items))
	for _, item := range items {
		if !domain.ValidTaskID(item.ID) || item.CurrentRevision == nil || *item.CurrentRevision < 0 {
			return nil, domain.InvalidArgument("task_projection", "Task projection identity is invalid")
		}
		if _, exists := projections[item.ID]; exists {
			return nil, domain.InvalidArgument("task_projection", "Task projection identity is duplicated")
		}
		ids = append(ids, item.ID)
		revisions = append(revisions, *item.CurrentRevision)
		projections[item.ID] = app.TaskK2Projection{
			Dependencies: make([]app.Dependency, 0),
			ChildJobs:    make([]app.ChildJob, 0),
			Validation:   app.TaskValidationSummary{State: app.TaskValidationPending},
		}
	}

	rows, err := tx.Query(ctx, `SELECT dependency.task_id,dependency.dependency_task_id,dependency.required_revision,target.status,
 (target.status='completed' AND target.current_revision=dependency.required_revision AND target.desired_revision=dependency.required_revision
  AND NULLIF(btrim(target.result),'') IS NOT NULL)
FROM task_dependencies dependency
JOIN tasks parent ON parent.id=dependency.task_id AND parent.owner_id=dependency.owner_id
 AND parent.conversation_id=dependency.conversation_id
JOIN tasks target ON target.id=dependency.dependency_task_id AND target.owner_id=dependency.owner_id
 AND target.conversation_id=dependency.conversation_id AND target.initiator_id=parent.initiator_id
 AND target.project_id IS NOT DISTINCT FROM parent.project_id
WHERE dependency.task_id=ANY($1::text[])
ORDER BY dependency.task_id,dependency.created_at,dependency.dependency_task_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("list Task dependencies: %w", err)
	}
	for rows.Next() {
		var taskID string
		var dependency app.Dependency
		var status string
		if err := rows.Scan(&taskID, &dependency.TaskID, &dependency.RequiredRevision, &status, &dependency.Satisfied); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Task dependency: %w", err)
		}
		projection, exists := projections[taskID]
		if !exists {
			rows.Close()
			return nil, domain.NewError(domain.CodeInvalidState, "Task dependency projection escaped its page")
		}
		dependency.TaskStatus = task.Status(status)
		projection.Dependencies = append(projection.Dependencies, dependency)
		projections[taskID] = projection
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate Task dependencies: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT session.task_id,session.session_id,session.step_id,step.title,session.task_revision,step.status
FROM task_agent_sessions session
JOIN unnest($1::text[],$2::bigint[]) AS requested(task_id,task_revision)
 ON requested.task_id=session.task_id AND requested.task_revision=session.task_revision
JOIN task_steps step ON step.task_id=session.task_id AND step.id=session.step_id
WHERE session.parent_session_id IS NOT NULL
ORDER BY session.task_id,step.sequence,session.session_id`, ids, revisions)
	if err != nil {
		return nil, fmt.Errorf("list Task child jobs: %w", err)
	}
	for rows.Next() {
		var taskID string
		var child app.ChildJob
		if err := rows.Scan(&taskID, &child.JobID, &child.TaskStepID, &child.Title, &child.TaskRevision, &child.Status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Task child job: %w", err)
		}
		projection, exists := projections[taskID]
		if !exists {
			rows.Close()
			return nil, domain.NewError(domain.CodeInvalidState, "Task child-job projection escaped its page")
		}
		projection.ChildJobs = append(projection.ChildJobs, child)
		projections[taskID] = projection
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate Task child jobs: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT source.task_id,source.task_revision,source.event_sequence,source.state,batch.state,batch.trigger_message_id,batch.published_message_id
FROM unnest($1::text[],$2::bigint[]) AS requested(task_id,task_revision)
JOIN conversation_result_sources source ON source.task_id=requested.task_id AND source.task_revision=requested.task_revision
JOIN conversation_result_batches batch ON batch.message_id=source.batch_message_id
ORDER BY source.task_id`, ids, revisions)
	if err != nil {
		return nil, fmt.Errorf("read Task result reviews: %w", err)
	}
	for rows.Next() {
		var taskID, sourceState, batchState string
		var review app.ResultReview
		var triggerMessageID, replyMessageID pgtype.Text
		if err := rows.Scan(&taskID, &review.SourceTaskRevision, &review.SourceEventSequence,
			&sourceState, &batchState, &triggerMessageID, &replyMessageID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Task result review: %w", err)
		}
		review.SourceTaskID = taskID
		if sourceState == "superseded" || batchState == "superseded" {
			continue
		}
		switch batchState {
		case "pending", "processing", "published", "failed":
		default:
			rows.Close()
			return nil, fmt.Errorf("task result review has unknown batch state %q", batchState)
		}
		projection, exists := projections[taskID]
		if !exists {
			rows.Close()
			return nil, domain.NewError(domain.CodeInvalidState, "Task result-review projection escaped its page")
		}
		review.State = batchState
		if triggerMessageID.Valid {
			value := triggerMessageID.String
			review.TriggerMessageID = &value
		}
		if replyMessageID.Valid {
			value := replyMessageID.String
			review.ReplyMessageID = &value
		}
		projection.ResultReview = &review
		projections[taskID] = projection
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate Task result reviews: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT review.task_id,review.validation_state,task.auth_kind,task.auth_scopes,task.host_id IS NOT NULL
FROM unnest($1::text[],$2::bigint[]) AS requested(task_id,task_revision)
JOIN task_validation_reviews review ON review.task_id=requested.task_id AND review.task_revision=requested.task_revision
JOIN tasks task ON task.id=review.task_id AND task.status='completed'
  AND task.current_revision=review.task_revision AND task.desired_revision=review.task_revision
  AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
  AND event.task_revision=review.task_revision AND event.event_type='task.result'
  AND event.result_content_sha256=review.result_sha256
JOIN task_model_call_responses response ON response.owner_id=task.initiator_id
  AND response.source='task' AND response.call_id=review.execution_model_call_id AND response.task_id=task.id
  AND response.task_owner_id=task.owner_id AND response.initiator_id=task.initiator_id
  AND response.project_id=COALESCE(task.project_id,'')
  AND response.agent_instance_id=task.agent_instance_id AND response.agent_generation=task.agent_generation
  AND response.agent_session_id=task.agent_session_id AND response.agent_root_session_id=task.agent_session_id
  AND response.authorization_version=task.auth_version
JOIN model_call_ledger ledger ON ledger.owner_id=response.owner_id AND ledger.source=response.source
  AND ledger.call_id=response.call_id AND ledger.task_id=response.task_id AND ledger.state='settled'
JOIN task_agent_instances agent ON agent.task_id=response.task_id AND agent.owner_id=response.task_owner_id
  AND agent.agent_instance_id=response.agent_instance_id AND agent.agent_generation=response.agent_generation
  AND agent.agent_session_id=response.agent_session_id AND agent.auth_version=response.authorization_version
  AND agent.engine_version=response.engine_version AND agent.profile_version=response.profile_version
WHERE review.state='reviewed'
  AND task_validation_artifact_evidence_current(review.task_id,review.owner_id,review.artifact_content_evidence)
  AND `+taskValidationCurrentAuthorizationPredicate, ids, revisions)
	if err != nil {
		return nil, fmt.Errorf("read Task validation verdicts: %w", err)
	}
	for rows.Next() {
		var taskID, authKind string
		var state app.TaskValidationState
		var authScopes []string
		var hostBound bool
		if err := rows.Scan(&taskID, &state, &authKind, &authScopes, &hostBound); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Task validation verdict: %w", err)
		}
		projection, exists := projections[taskID]
		if !exists {
			rows.Close()
			return nil, domain.NewError(domain.CodeInvalidState, "Task validation projection escaped its page")
		}
		if !app.ValidTaskValidationState(state) {
			rows.Close()
			return nil, fmt.Errorf("Task validation verdict has unknown state %q", state)
		}
		if authKind == string(identity.KindScheduleTask) && !scheduleTaskScopeStringsValidForHost(authScopes, hostBound) {
			continue
		}
		projection.Validation.State = state
		projections[taskID] = projection
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate Task validation verdicts: %w", err)
	}
	rows.Close()
	return projections, nil
}
