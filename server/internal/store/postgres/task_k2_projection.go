package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func loadTaskK2Projection(ctx context.Context, tx pgx.Tx, taskID string, revision int64) ([]app.Dependency, []app.ChildJob, *app.ResultReview, error) {
	currentRevision := revision
	projections, err := loadTaskK2Projections(ctx, tx, []task.Task{{ID: taskID, CurrentRevision: &currentRevision}})
	if err != nil {
		return nil, nil, nil, err
	}
	projection, ok := projections[taskID]
	if !ok {
		return nil, nil, nil, domain.NewError(domain.CodeInvalidState, "Task K2 projection is missing")
	}
	return projection.Dependencies, projection.ChildJobs, projection.ResultReview, nil
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

	rows, err = tx.Query(ctx, `SELECT source.task_id,source.task_revision,source.event_sequence,source.state,batch.state,batch.published_message_id
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
		var replyMessageID pgtype.Text
		if err := rows.Scan(&taskID, &review.SourceTaskRevision, &review.SourceEventSequence,
			&sourceState, &batchState, &replyMessageID); err != nil {
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

	return projections, nil
}
