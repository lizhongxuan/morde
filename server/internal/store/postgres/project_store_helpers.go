package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

var projectIdempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

func validProjectIdempotencyKey(value string) bool {
	return projectIdempotencyPattern.MatchString(value)
}

func projectRequestDigest(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode project request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func requireApprovedProjectUserInTx(ctx context.Context, tx pgx.Tx, userID string) error {
	if !domain.ValidUserID(userID) {
		return domain.NewError(domain.CodeNotFound, "project user not found")
	}
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, userID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return domain.NewError(domain.CodeNotFound, "project user not found")
	}
	if err != nil {
		return fmt.Errorf("verify project user: %w", err)
	}
	return nil
}

func lockProjectForActorInTx(ctx context.Context, tx pgx.Tx, actorID, projectID string, minimum project.Role, forUpdate bool) (project.Project, project.Member, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidRole(minimum) {
		return project.Project{}, project.Member{}, projectNotFound()
	}
	lock := "FOR SHARE OF p,m,u"
	if forUpdate {
		lock = "FOR UPDATE OF p,m,u"
	}
	var value project.Project
	var member project.Member
	var role string
	err := tx.QueryRow(ctx, `SELECT p.id,p.owner_id,p.name,p.description,p.version,m.role,m.version
FROM projects p
JOIN project_members m ON m.project_id=p.id AND m.user_id=$2
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE p.id=$1 `+lock, projectID, actorID).
		Scan(&value.ID, &value.OwnerID, &value.Name, &value.Description, &value.Version, &role, &member.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Project{}, project.Member{}, projectNotFound()
	}
	if err != nil {
		return project.Project{}, project.Member{}, fmt.Errorf("lock project membership: %w", err)
	}
	member.ProjectID, member.UserID, member.Role = projectID, actorID, project.Role(role)
	if value.Validate() != nil || member.Version < 1 || !project.ValidRole(member.Role) {
		return project.Project{}, project.Member{}, domain.NewError(domain.CodeInternal, "project membership is invalid")
	}
	if !project.RoleAtLeast(member.Role, minimum) {
		return project.Project{}, project.Member{}, domain.NewError(domain.CodeForbidden, "project role is insufficient")
	}
	return value, member, nil
}

func lockProjectByIDInTx(ctx context.Context, tx pgx.Tx, projectID string) error {
	if !project.ValidID(projectID) {
		return projectNotFound()
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectNotFound()
	}
	if err != nil {
		return fmt.Errorf("lock project: %w", err)
	}
	return nil
}

func loadProjectMemberInTx(ctx context.Context, tx pgx.Tx, projectID, userID string, forUpdate bool) (project.Member, error) {
	if !project.ValidID(projectID) || !domain.ValidUserID(userID) {
		return project.Member{}, projectNotFound()
	}
	query := `SELECT project_id,user_id,role,version FROM project_members WHERE project_id=$1 AND user_id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	var result project.Member
	var role string
	err := tx.QueryRow(ctx, query, projectID, userID).Scan(&result.ProjectID, &result.UserID, &role, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Member{}, projectNotFound()
	}
	if err != nil {
		return project.Member{}, fmt.Errorf("load project member: %w", err)
	}
	result.Role = project.Role(role)
	if result.ProjectID != projectID || result.UserID != userID || !project.ValidRole(result.Role) || result.Version < 1 {
		return project.Member{}, domain.NewError(domain.CodeInternal, "project member is invalid")
	}
	return result, nil
}

func loadProjectInvitationInTx(ctx context.Context, tx pgx.Tx, projectID, invitationID string) (project.Invitation, error) {
	return queryProjectInvitation(ctx, tx, projectID, invitationID, false)
}

func loadProjectInvitationForUpdateInTx(ctx context.Context, tx pgx.Tx, projectID, invitationID string) (project.Invitation, error) {
	return queryProjectInvitation(ctx, tx, projectID, invitationID, true)
}

func queryProjectInvitation(ctx context.Context, tx pgx.Tx, projectID, invitationID string, forUpdate bool) (project.Invitation, error) {
	if !project.ValidID(projectID) || !domain.ValidPendingRequestID(invitationID) {
		return project.Invitation{}, domain.NewError(domain.CodeNotFound, "project invitation not found")
	}
	query := `SELECT id,project_id,inviter_id,invitee_id,role,state,version
FROM project_invitations WHERE project_id=$1 AND id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	var result project.Invitation
	var role, state string
	err := tx.QueryRow(ctx, query, projectID, invitationID).Scan(&result.ID, &result.ProjectID, &result.InviterID,
		&result.InviteeID, &role, &state, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Invitation{}, domain.NewError(domain.CodeNotFound, "project invitation not found")
	}
	if err != nil {
		return project.Invitation{}, fmt.Errorf("load project invitation: %w", err)
	}
	result.Role, result.State = project.Role(role), project.InvitationState(state)
	if !validStoredProjectInvitation(result) {
		return project.Invitation{}, domain.NewError(domain.CodeInternal, "project invitation is invalid")
	}
	return result, nil
}

func validStoredProjectInvitation(value project.Invitation) bool {
	return domain.ValidPendingRequestID(value.ID) && project.ValidID(value.ProjectID) && domain.ValidUserID(value.InviterID) &&
		domain.ValidUserID(value.InviteeID) && value.InviterID != value.InviteeID && project.ValidInvitationRole(value.Role) &&
		project.ValidInvitationState(value.State) && value.Version >= 1
}

func insertProjectIdempotencyInTx(ctx context.Context, tx pgx.Tx, actorID, operation, key, projectID, resultKind, resultID string, resultVersion int64, digest []byte) error {
	if !domain.ValidUserID(actorID) || !validProjectIdempotencyKey(key) || !project.ValidID(projectID) ||
		resultVersion < 0 || len(digest) != sha256.Size || !validProjectIdempotencyResult(resultKind, resultID, resultVersion) {
		return domain.InvalidArgument("idempotency_key", "project idempotency record is invalid")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_idempotency_keys
(actor_id,operation,idempotency_key,project_id,result_kind,result_id,result_version,request_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, actorID, operation, key, projectID, resultKind, resultID, resultVersion, digest); err != nil {
		if isProjectUniqueViolation(err) {
			return domain.NewError(domain.CodeIdempotencyConflict, "project idempotency key was already used")
		}
		return fmt.Errorf("save project idempotency record: %w", err)
	}
	return nil
}

func validProjectIdempotencyResult(kind, id string, version int64) bool {
	switch kind {
	case "project", "member", "invitation", "host", "workspace", "memory", "goal", "document", "budget", "work_item":
		return id != "" && len(id) <= 128 && version >= 1
	case "":
		return id == "" && version == 0
	default:
		return false
	}
}

func findProjectIdempotencyInTx(ctx context.Context, tx pgx.Tx, actorID, operation, key, projectID string, digest []byte) (projectIdempotencyResult, bool, error) {
	if !domain.ValidUserID(actorID) || !validProjectIdempotencyKey(key) || !project.ValidID(projectID) || len(digest) != sha256.Size {
		return projectIdempotencyResult{}, false, domain.InvalidArgument("idempotency_key", "project idempotency request is invalid")
	}
	var result projectIdempotencyResult
	err := tx.QueryRow(ctx, `SELECT project_id,result_kind,result_id,result_version,request_digest
FROM project_idempotency_keys WHERE actor_id=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`, actorID, operation, key).
		Scan(&result.ProjectID, &result.Kind, &result.ID, &result.Version, &result.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectIdempotencyResult{}, false, nil
	}
	if err != nil {
		return projectIdempotencyResult{}, false, fmt.Errorf("read project idempotency record: %w", err)
	}
	if result.ProjectID != projectID || !validProjectIdempotencyResult(result.Kind, result.ID, result.Version) || !bytes.Equal(result.Digest, digest) {
		return projectIdempotencyResult{}, false, domain.NewError(domain.CodeIdempotencyConflict, "project idempotency key was used for a different request")
	}
	return result, true, nil
}

func bumpProjectVersionInTx(ctx context.Context, tx pgx.Tx, projectID string, expectedVersion int64) error {
	if !project.ValidID(projectID) || expectedVersion < 1 || expectedVersion == int64(^uint64(0)>>1) {
		return domain.NewError(domain.CodeInvalidState, "project version cannot be advanced")
	}
	tag, err := tx.Exec(ctx, `UPDATE projects SET version=version+1,updated_at=clock_timestamp()
WHERE id=$1 AND version=$2`, projectID, expectedVersion)
	if err != nil {
		return fmt.Errorf("advance project version: %w", err)
	}
	if tag.RowsAffected() != 1 {
		var actual int64
		if err := tx.QueryRow(ctx, `SELECT version FROM projects WHERE id=$1`, projectID).Scan(&actual); errors.Is(err, pgx.ErrNoRows) {
			return projectNotFound()
		} else if err != nil {
			return fmt.Errorf("read current project version: %w", err)
		}
		return projectVersionConflict(expectedVersion, actual)
	}
	return nil
}

func projectVersionConflict(expected, actual int64) error {
	err := domain.NewError(domain.CodeConflict, fmt.Sprintf("project version changed: expected %d, current %d", expected, actual))
	err.Details["expected_version"] = expected
	err.Details["current_version"] = actual
	return err
}

func projectNotFound() error {
	return domain.NewError(domain.CodeNotFound, "project not found")
}

func isProjectUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
