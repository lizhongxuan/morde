package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

// validateScheduleAuthorizationInTx locks the owner, credential and (for an
// iOS grant) installation while a schedule is created, re-enabled or claimed.
// The due-occurrence transaction calls it while holding the schedule row, so a
// concurrent grant revocation cannot race a cloud task into the queue.
func validateScheduleAuthorizationInTx(ctx context.Context, tx pgx.Tx, ownerID string, authorization task.Authorization) (bool, error) {
	kind := identity.Kind(authorization.Kind)
	if !domain.ValidUserID(ownerID) || !domain.ValidTokenID(authorization.TokenID) ||
		(kind != identity.KindSession && kind != identity.KindClient) {
		return false, nil
	}
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, ownerID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate schedule owner: %w", err)
	}
	var tokenKind string
	var installID pgtype.Text
	var tokenScopes []string
	var expiresAt time.Time
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT kind,client_installation_id,scopes,expires_at,revoked_at
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, authorization.TokenID, ownerID).
		Scan(&tokenKind, &installID, &tokenScopes, &expiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate schedule credential: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, fmt.Errorf("read schedule authorization time: %w", err)
	}
	if tokenKind != string(kind) || !expiresAt.After(now) || revokedAt.Valid ||
		installID.String != authorization.ClientInstallationID || !sameStringSlices(tokenScopes, authorization.Scopes) {
		return false, nil
	}
	if kind == identity.KindSession {
		return authorization.ClientInstallationID == "" && len(authorization.Scopes) == 0, nil
	}
	grantScopes := []string{string(identity.ScopeScheduleRead), string(identity.ScopeScheduleManage)}
	if !domain.ValidClientInstallationID(authorization.ClientInstallationID) ||
		!sameStringSlices(authorization.Scopes, grantScopes) || !sameStringSlices(tokenScopes, grantScopes) {
		return false, nil
	}
	var installationActive bool
	err = tx.QueryRow(ctx, `SELECT platform='ios' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, authorization.ClientInstallationID, ownerID).Scan(&installationActive)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !installationActive {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate schedule grant installation: %w", err)
	}
	return true, nil
}

func sameTaskAuthorization(left, right task.Authorization) bool {
	return left.Kind == right.Kind && left.TokenID == right.TokenID &&
		left.ClientInstallationID == right.ClientInstallationID && sameStringSlices(left.Scopes, right.Scopes)
}

func sameStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func scheduleTaskScopeStringsValid(values []string) bool {
	scopes := make([]identity.Scope, 0, len(values))
	for _, value := range values {
		scopes = append(scopes, identity.Scope(value))
	}
	return identity.IsScheduleTaskExecutionScopeSet(scopes)
}

// AuthorizeScheduledTaskAction only authorizes an internal Task when its
// token-id slot refers to a ScheduleRun attached to that exact Task.
func (s *Store) AuthorizeScheduledTaskAction(ctx context.Context, ownerID, runID, hostID string, required identity.Scope) error {
	if !domain.ValidUserID(ownerID) || !schedule.ValidRunID(runID) || !domain.ValidHostID(hostID) ||
		!containsScope(identity.ScheduleTaskExecutionScopes(), required) {
		return domain.NewError(domain.CodeUnauthenticated, "scheduled task authorization is unavailable")
	}
	var authorized bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM schedule_runs r
  JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
  JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id
  WHERE r.id=$1 AND r.owner_id=$2 AND s.host_id=$3
    AND t.host_id=$3 AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
    AND t.execution_location='server' AND t.auth_kind='schedule_task' AND t.auth_token_id=r.id
)`, runID, ownerID, hostID).Scan(&authorized)
	if err != nil {
		return fmt.Errorf("authorize scheduled Task action: %w", err)
	}
	if !authorized {
		return domain.NewError(domain.CodeUnauthenticated, "scheduled task authorization is unavailable")
	}
	return nil
}

func containsScope(values []identity.Scope, required identity.Scope) bool {
	for _, value := range values {
		if value == required {
			return true
		}
	}
	return false
}

var _ app.ScheduledTaskActionAuthorizer = (*Store)(nil)
