package postgres

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func loadTaskDependencyResultsInTx(ctx context.Context, tx pgx.Tx, taskID, ownerID string) ([]task.DependencyResult, error) {
	if ctx == nil || tx == nil || !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("task_dependency", "Task dependency identity is invalid")
	}
	var expected int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_dependencies WHERE task_id=$1 AND owner_id=$2`, taskID, ownerID).Scan(&expected); err != nil {
		return nil, fmt.Errorf("count Task dependencies: %w", err)
	}
	if expected > app.MaxConversationRouteDependencies {
		return nil, domain.NewError(domain.CodeInvalidState, "Task has too many dependencies")
	}
	rows, err := tx.Query(ctx, `SELECT dependency.dependency_task_id,dependency.required_revision,prerequisite.objective,prerequisite.result
FROM task_dependencies dependency JOIN tasks prerequisite ON prerequisite.id=dependency.dependency_task_id
 AND prerequisite.owner_id=dependency.owner_id AND prerequisite.conversation_id=dependency.conversation_id
JOIN tasks dependent ON dependent.id=dependency.task_id AND dependent.owner_id=dependency.owner_id
WHERE dependency.task_id=$1 AND dependency.owner_id=$2 AND prerequisite.status='completed'
 AND prerequisite.initiator_id=dependent.initiator_id AND prerequisite.project_id IS NOT DISTINCT FROM dependent.project_id
 AND prerequisite.current_revision=dependency.required_revision AND prerequisite.desired_revision=dependency.required_revision
 AND prerequisite.result<>''
ORDER BY dependency.created_at,dependency.dependency_task_id FOR SHARE OF prerequisite`, taskID, ownerID)
	if err != nil {
		return nil, fmt.Errorf("load completed Task dependencies: %w", err)
	}
	defer rows.Close()
	results := make([]task.DependencyResult, 0, expected)
	for rows.Next() {
		var result task.DependencyResult
		if err := rows.Scan(&result.TaskID, &result.RequiredRevision, &result.Objective, &result.Result); err != nil {
			return nil, fmt.Errorf("read completed Task dependency: %w", err)
		}
		if !utf8.ValidString(result.Objective) || !utf8.ValidString(result.Result) {
			return nil, domain.NewError(domain.CodeInvalidState, "Task dependency result is invalid")
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed Task dependencies: %w", err)
	}
	if len(results) != expected {
		return nil, domain.NewError(domain.CodeConflict, "Task dependencies changed before execution")
	}
	return results, nil
}
