package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5/pgtype"
)

type projectIdempotencyResult struct {
	ProjectID string
	Kind      string
	ID        string
	Version   int64
	Digest    []byte
}

type projectMemberPageRow struct {
	Member    project.Member
	CreatedAt time.Time
}

type projectInvitationPageRow struct {
	Invitation project.Invitation
	CreatedAt  time.Time
}

type projectResourcePageRow struct {
	Resource  project.Resource
	CreatedAt time.Time
}

var _ app.ProjectStore = (*Store)(nil)

func (s *Store) CreateProject(ctx context.Context, actorID string, value project.Project, idempotencyKey string) (project.Project, bool, error) {
	if !domain.ValidUserID(actorID) || value.OwnerID != actorID || value.Version != 1 ||
		value.Validate() != nil || !validProjectIdempotencyKey(idempotencyKey) {
		return project.Project{}, false, domain.InvalidArgument("project", "project fields are invalid")
	}
	digest, err := projectRequestDigest(struct {
		ActorID     string
		Name        string
		Description string
	}{actorID, value.Name, value.Description})
	if err != nil {
		return project.Project{}, false, err
	}

	var result project.Project
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO projects
(id,owner_id,name,description,version,create_idempotency_key,create_request_digest)
VALUES ($1,$2,$3,$4,1,$5,$6)
ON CONFLICT (owner_id,create_idempotency_key) DO NOTHING
RETURNING id,owner_id,name,description,version`,
			value.ID, actorID, value.Name, value.Description, idempotencyKey, digest).
			Scan(&result.ID, &result.OwnerID, &result.Name, &result.Description, &result.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			var storedDigest []byte
			err = tx.QueryRow(ctx, `SELECT id,owner_id,name,description,version,create_request_digest
FROM projects WHERE owner_id=$1 AND create_idempotency_key=$2 FOR SHARE`, actorID, idempotencyKey).
				Scan(&result.ID, &result.OwnerID, &result.Name, &result.Description, &result.Version, &storedDigest)
			if err != nil {
				return fmt.Errorf("load created project replay: %w", err)
			}
			if !bytes.Equal(storedDigest, digest) {
				return domain.NewError(domain.CodeIdempotencyConflict, "project create key was used for a different request")
			}
			replay = true
			return nil
		}
		if err != nil {
			if isProjectUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "project ID already exists")
			}
			return fmt.Errorf("create project: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'owner',1)`, value.ID, actorID); err != nil {
			return fmt.Errorf("create project owner membership: %w", err)
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "create_project", idempotencyKey,
			value.ID, "project", value.ID, 1, digest); err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return project.Project{}, false, err
	}
	return result, replay, nil
}

func (s *Store) GetProject(ctx context.Context, actorID, projectID string) (project.Project, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return project.Project{}, projectNotFound()
	}
	var result project.Project
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false)
		if err != nil {
			return err
		}
		result = locked
		return nil
	})
	if err != nil {
		return project.Project{}, err
	}
	return result, nil
}

func (s *Store) ListProjects(ctx context.Context, actorID string, page app.PageRequest) (app.ProjectPage, error) {
	if !domain.ValidUserID(actorID) {
		return app.ProjectPage{}, projectNotFound()
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ProjectPage{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id,p.owner_id,p.name,p.description,p.version,p.created_at
FROM projects p
JOIN project_members m ON m.project_id=p.id AND m.user_id=$1
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE ($2::timestamptz IS NULL OR (p.created_at,p.id) < ($2,$3))
ORDER BY p.created_at DESC,p.id DESC LIMIT $4`, actorID, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.ProjectPage{}, fmt.Errorf("list projects: %w", err)
	}
	type row struct {
		value     project.Project
		createdAt time.Time
	}
	values := make([]row, 0, int(limit)+1)
	for rows.Next() {
		var value row
		if err := rows.Scan(&value.value.ID, &value.value.OwnerID, &value.value.Name, &value.value.Description,
			&value.value.Version, &value.createdAt); err != nil {
			rows.Close()
			return app.ProjectPage{}, fmt.Errorf("scan project: %w", err)
		}
		if value.value.Validate() != nil {
			rows.Close()
			return app.ProjectPage{}, domain.NewError(domain.CodeInternal, "project store returned an invalid project")
		}
		values = append(values, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return app.ProjectPage{}, fmt.Errorf("iterate projects: %w", err)
	}
	result := app.ProjectPage{Items: make([]project.Project, 0, min(len(values), int(limit)))}
	for index := range values {
		if int32(index) == limit {
			last := values[int(limit)-1]
			result.NextCursor = encodePageCursor(last.createdAt, last.value.ID)
			break
		}
		result.Items = append(result.Items, values[index].value)
	}
	return result, nil
}

func (s *Store) GetProjectMember(ctx context.Context, projectID, userID string) (project.Member, error) {
	if !project.ValidID(projectID) || !domain.ValidUserID(userID) {
		return project.Member{}, projectNotFound()
	}
	var result project.Member
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		_, member, err := lockProjectForActorInTx(ctx, tx, userID, projectID, project.RoleViewer, false)
		if err != nil {
			return err
		}
		result = member
		return nil
	})
	if err != nil {
		return project.Member{}, err
	}
	return result, nil
}

func (s *Store) ListProjectMembers(ctx context.Context, projectID, actorID string, page app.PageRequest) (app.ProjectMemberPage, error) {
	if !project.ValidID(projectID) || !domain.ValidUserID(actorID) {
		return app.ProjectMemberPage{}, projectNotFound()
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ProjectMemberPage{}, err
	}
	values := make([]projectMemberPageRow, 0)
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT m.project_id,m.user_id,m.role,m.version,m.created_at
FROM project_members m
WHERE m.project_id=$1 AND ($2::timestamptz IS NULL OR (m.created_at,m.user_id) < ($2,$3))
ORDER BY m.created_at DESC,m.user_id DESC LIMIT $4`, projectID, cursorAt, cursorID, limit+1)
		if err != nil {
			return fmt.Errorf("list project members: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value projectMemberPageRow
			var role string
			if err := rows.Scan(&value.Member.ProjectID, &value.Member.UserID, &role, &value.Member.Version, &value.CreatedAt); err != nil {
				return fmt.Errorf("scan project member: %w", err)
			}
			value.Member.Role = project.Role(role)
			if value.Member.ProjectID != projectID || !domain.ValidUserID(value.Member.UserID) ||
				!project.ValidRole(value.Member.Role) || value.Member.Version < 1 {
				return domain.NewError(domain.CodeInternal, "project member store returned an invalid member")
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate project members: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.ProjectMemberPage{}, err
	}
	result := app.ProjectMemberPage{Items: make([]project.Member, 0, min(len(values), int(limit)))}
	for index := range values {
		if int32(index) == limit {
			last := values[int(limit)-1]
			result.NextCursor = encodePageCursor(last.CreatedAt, last.Member.UserID)
			break
		}
		result.Items = append(result.Items, values[index].Member)
	}
	return result, nil
}

func (s *Store) CreateProjectInvitation(ctx context.Context, actorID string, value project.Invitation, expectedVersion int64, idempotencyKey string) (project.Invitation, bool, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidPendingRequestID(value.ID) || !project.ValidID(value.ProjectID) ||
		value.InviterID != actorID || !domain.ValidUserID(value.InviteeID) || value.InviteeID == actorID ||
		!project.ValidInvitationRole(value.Role) || value.State != project.InvitationPending || value.Version != 1 ||
		expectedVersion < 1 || expectedVersion == math.MaxInt64 || !validProjectIdempotencyKey(idempotencyKey) {
		return project.Invitation{}, false, domain.InvalidArgument("invitation", "project invitation fields are invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		InviteeID       string
		Role            project.Role
		ExpectedVersion int64
	}{value.ProjectID, value.InviteeID, value.Role, expectedVersion})
	if err != nil {
		return project.Invitation{}, false, err
	}
	var result project.Invitation
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, value.ProjectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "invite_member", idempotencyKey, value.ProjectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "invitation" || !domain.ValidPendingRequestID(stored.ID) || stored.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project invitation result is invalid")
			}
			result, err = loadProjectInvitationInTx(ctx, tx, value.ProjectID, stored.ID)
			replay = err == nil
			return err
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		if err := requireApprovedProjectUserInTx(ctx, tx, value.InviteeID); err != nil {
			return domain.NewError(domain.CodeNotFound, "project invitee not found")
		}
		var memberExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_members WHERE project_id=$1 AND user_id=$2)`, value.ProjectID, value.InviteeID).Scan(&memberExists); err != nil {
			return fmt.Errorf("check existing project member: %w", err)
		}
		if memberExists {
			return domain.NewError(domain.CodeConflict, "user is already a project member")
		}
		_, err = tx.Exec(ctx, `INSERT INTO project_invitations
(id,project_id,inviter_id,invitee_id,role,state,version,create_idempotency_key,create_request_digest)
VALUES ($1,$2,$3,$4,$5,'pending',1,$6,$7)`, value.ID, value.ProjectID, actorID,
			value.InviteeID, string(value.Role), idempotencyKey, digest)
		if err != nil {
			if isProjectUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "a pending project invitation already exists")
			}
			return fmt.Errorf("create project invitation: %w", err)
		}
		if err := bumpProjectVersionInTx(ctx, tx, value.ProjectID, locked.Version); err != nil {
			return err
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "invite_member", idempotencyKey, value.ProjectID,
			"invitation", value.ID, value.Version, digest); err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return project.Invitation{}, false, err
	}
	return result, replay, nil
}

func (s *Store) RevokeProjectInvitation(ctx context.Context, actorID, projectID, invitationID string, expectedVersion int64, idempotencyKey string) error {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidPendingRequestID(invitationID) ||
		expectedVersion < 1 || expectedVersion == math.MaxInt64 || !validProjectIdempotencyKey(idempotencyKey) {
		return domain.InvalidArgument("invitation", "project invitation revoke request is invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		InvitationID    string
		ExpectedVersion int64
	}{projectID, invitationID, expectedVersion})
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "revoke_invitation", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "" || stored.ID != "" || stored.Version != 0 {
				return domain.NewError(domain.CodeInternal, "idempotent invitation revoke result is invalid")
			}
			return nil
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		invitation, err := loadProjectInvitationForUpdateInTx(ctx, tx, projectID, invitationID)
		if err != nil {
			return err
		}
		if invitation.State != project.InvitationPending {
			return domain.NewError(domain.CodeConflict, "project invitation is no longer pending")
		}
		tag, err := tx.Exec(ctx, `UPDATE project_invitations SET state='revoked',version=version+1,
revoked_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE project_id=$1 AND id=$2 AND state='pending' AND version=$3`,
			projectID, invitationID, invitation.Version)
		if err != nil {
			return fmt.Errorf("revoke project invitation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project invitation version changed")
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, locked.Version); err != nil {
			return err
		}
		return insertProjectIdempotencyInTx(ctx, tx, actorID, "revoke_invitation", idempotencyKey, projectID,
			"", "", 0, digest)
	})
}

func (s *Store) ListProjectInvitations(ctx context.Context, actorID string, page app.PageRequest) (app.ProjectInvitationPage, error) {
	if !domain.ValidUserID(actorID) {
		return app.ProjectInvitationPage{}, projectNotFound()
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ProjectInvitationPage{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.project_id,i.inviter_id,i.invitee_id,i.role,i.state,i.version,i.created_at
FROM project_invitations i
JOIN users u ON u.id=i.invitee_id AND u.status='approved'
WHERE i.invitee_id=$1 AND i.state='pending'
  AND ($2::timestamptz IS NULL OR (i.created_at,i.id) < ($2,$3))
ORDER BY i.created_at DESC,i.id DESC LIMIT $4`, actorID, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.ProjectInvitationPage{}, fmt.Errorf("list project invitations: %w", err)
	}
	values := make([]projectInvitationPageRow, 0, int(limit)+1)
	for rows.Next() {
		var value projectInvitationPageRow
		var role, state string
		if err := rows.Scan(&value.Invitation.ID, &value.Invitation.ProjectID, &value.Invitation.InviterID,
			&value.Invitation.InviteeID, &role, &state, &value.Invitation.Version, &value.CreatedAt); err != nil {
			rows.Close()
			return app.ProjectInvitationPage{}, fmt.Errorf("scan project invitation: %w", err)
		}
		value.Invitation.Role, value.Invitation.State = project.Role(role), project.InvitationState(state)
		if !validStoredProjectInvitation(value.Invitation) {
			rows.Close()
			return app.ProjectInvitationPage{}, domain.NewError(domain.CodeInternal, "project invitation store returned an invalid invitation")
		}
		values = append(values, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return app.ProjectInvitationPage{}, fmt.Errorf("iterate project invitations: %w", err)
	}
	result := app.ProjectInvitationPage{Items: make([]project.Invitation, 0, min(len(values), int(limit)))}
	for index := range values {
		if int32(index) == limit {
			last := values[int(limit)-1]
			result.NextCursor = encodePageCursor(last.CreatedAt, last.Invitation.ID)
			break
		}
		result.Items = append(result.Items, values[index].Invitation)
	}
	return result, nil
}

func (s *Store) AcceptProjectInvitation(ctx context.Context, invitationID, inviteeID, idempotencyKey string) (project.Member, bool, error) {
	if !domain.ValidPendingRequestID(invitationID) || !domain.ValidUserID(inviteeID) ||
		!validProjectIdempotencyKey(idempotencyKey) {
		return project.Member{}, false, domain.InvalidArgument("invitation", "project invitation accept request is invalid")
	}
	var projectID string
	err := s.pool.QueryRow(ctx, `SELECT project_id FROM project_invitations WHERE id=$1 AND invitee_id=$2`,
		invitationID, inviteeID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Member{}, false, domain.NewError(domain.CodeNotFound, "project invitation not found")
	}
	if err != nil {
		return project.Member{}, false, fmt.Errorf("find project invitation: %w", err)
	}
	digest, err := projectRequestDigest(struct{ InvitationID string }{invitationID})
	if err != nil {
		return project.Member{}, false, err
	}
	var result project.Member
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, inviteeID); err != nil {
			return err
		}
		if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, inviteeID, "accept_invitation", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "member" || stored.ID != inviteeID {
				return domain.NewError(domain.CodeInternal, "idempotent project invitation result is invalid")
			}
			result, err = loadProjectMemberInTx(ctx, tx, projectID, inviteeID, false)
			replay = err == nil
			return err
		}
		invitation, err := loadProjectInvitationForUpdateInTx(ctx, tx, projectID, invitationID)
		if err != nil {
			return err
		}
		if invitation.InviteeID != inviteeID {
			return domain.NewError(domain.CodeNotFound, "project invitation not found")
		}
		if invitation.State != project.InvitationPending {
			return domain.NewError(domain.CodeConflict, "project invitation is no longer pending")
		}
		if invitation.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "project invitation version cannot be advanced")
		}
		var memberExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_members WHERE project_id=$1 AND user_id=$2)`,
			projectID, inviteeID).Scan(&memberExists); err != nil {
			return fmt.Errorf("check existing project membership: %w", err)
		}
		if memberExists {
			return domain.NewError(domain.CodeConflict, "user is already a project member")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,$3,1)`,
			projectID, inviteeID, string(invitation.Role)); err != nil {
			return fmt.Errorf("accept project invitation membership: %w", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE project_invitations SET state='accepted',version=version+1,
accepted_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE project_id=$1 AND id=$2 AND state='pending' AND version=$3`, projectID, invitationID, invitation.Version)
		if err != nil {
			return fmt.Errorf("mark project invitation accepted: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project invitation version changed")
		}
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM projects WHERE id=$1`, projectID).Scan(&version); err != nil {
			return fmt.Errorf("read project version for invitation acceptance: %w", err)
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, version); err != nil {
			return err
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, inviteeID, "accept_invitation", idempotencyKey, projectID,
			"member", inviteeID, 1, digest); err != nil {
			return err
		}
		result = project.Member{ProjectID: projectID, UserID: inviteeID, Role: invitation.Role, Version: 1}
		return nil
	})
	if err != nil {
		return project.Member{}, false, err
	}
	return result, replay, nil
}

func (s *Store) UpdateProjectMemberRole(ctx context.Context, actorID, projectID, userID string, role project.Role, expectedVersion int64, idempotencyKey string) (project.Member, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidUserID(userID) ||
		userID == actorID || !project.ValidInvitationRole(role) || expectedVersion < 1 ||
		expectedVersion == math.MaxInt64 || !validProjectIdempotencyKey(idempotencyKey) {
		return project.Member{}, domain.InvalidArgument("member", "project member update is invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		UserID          string
		Role            project.Role
		ExpectedVersion int64
	}{projectID, userID, role, expectedVersion})
	if err != nil {
		return project.Member{}, err
	}
	var result project.Member
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "update_member_role", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "member" || stored.ID != userID || stored.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project member result is invalid")
			}
			result, err = loadProjectMemberInTx(ctx, tx, projectID, stored.ID, false)
			return err
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		member, err := loadProjectMemberInTx(ctx, tx, projectID, userID, true)
		if err != nil {
			return err
		}
		if member.Role == project.RoleOwner {
			return domain.NewError(domain.CodeConflict, "project owner role cannot be changed directly")
		}
		if member.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "project member version cannot be advanced")
		}
		tag, err := tx.Exec(ctx, `UPDATE project_members SET role=$3,version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND user_id=$2 AND version=$4`, projectID, userID, string(role), member.Version)
		if err != nil {
			if isProjectUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "project member role conflicts with current membership")
			}
			return fmt.Errorf("update project member role: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project member version changed")
		}
		if member.Role == project.RoleMember && role != project.RoleMember {
			if err := invalidateProjectConversationSummariesInTx(ctx, tx, projectID, userID); err != nil {
				return err
			}
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, locked.Version); err != nil {
			return err
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "update_member_role", idempotencyKey, projectID,
			"member", userID, member.Version+1, digest); err != nil {
			return err
		}
		result = project.Member{ProjectID: projectID, UserID: userID, Role: role, Version: member.Version + 1}
		return nil
	})
	if err != nil {
		return project.Member{}, err
	}
	return result, nil
}

func (s *Store) RemoveProjectMember(ctx context.Context, actorID, projectID, userID string, expectedVersion int64, idempotencyKey string) error {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidUserID(userID) ||
		userID == actorID || expectedVersion < 1 || expectedVersion == math.MaxInt64 ||
		!validProjectIdempotencyKey(idempotencyKey) {
		return domain.InvalidArgument("member", "project member removal is invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		UserID          string
		ExpectedVersion int64
	}{projectID, userID, expectedVersion})
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "remove_member", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "" || stored.ID != "" || stored.Version != 0 {
				return domain.NewError(domain.CodeInternal, "idempotent project member removal result is invalid")
			}
			return nil
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		member, err := loadProjectMemberInTx(ctx, tx, projectID, userID, true)
		if err != nil {
			return err
		}
		if member.Role == project.RoleOwner {
			return domain.NewError(domain.CodeConflict, "project owner cannot be removed; transfer ownership first")
		}
		tag, err := tx.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2 AND role <> 'owner'`, projectID, userID)
		if err != nil {
			return fmt.Errorf("remove project member: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project membership changed")
		}
		if err := invalidateProjectConversationSummariesInTx(ctx, tx, projectID, userID); err != nil {
			return err
		}
		if err := revokeProjectMCPConnectorMemberInTx(ctx, tx, projectID, userID); err != nil {
			return err
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, locked.Version); err != nil {
			return err
		}
		return insertProjectIdempotencyInTx(ctx, tx, actorID, "remove_member", idempotencyKey, projectID,
			"", "", 0, digest)
	})
}

func (s *Store) TransferProjectOwner(ctx context.Context, actorID, projectID, userID string, expectedVersion int64, idempotencyKey string) (project.Project, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidUserID(userID) ||
		userID == actorID || expectedVersion < 1 || expectedVersion == math.MaxInt64 ||
		!validProjectIdempotencyKey(idempotencyKey) {
		return project.Project{}, domain.InvalidArgument("owner", "project owner transfer is invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		UserID          string
		ExpectedVersion int64
	}{projectID, userID, expectedVersion})
	if err != nil {
		return project.Project{}, err
	}
	var result project.Project
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		// A successful transfer demotes the actor to member. Let that same
		// member replay an already-recorded transfer, while still requiring a
		// current approved membership before exposing any idempotency result.
		locked, member, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleMember, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "transfer_owner", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "project" || !domain.ValidUserID(stored.ID) || stored.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project owner transfer result is invalid")
			}
			result, err = loadProjectInTx(ctx, tx, projectID)
			if err == nil {
				result.OwnerID = stored.ID
				result.Version = stored.Version
			}
			return err
		}
		if member.Role != project.RoleOwner {
			return domain.NewError(domain.CodeForbidden, "project role is insufficient")
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		target, err := loadProjectMemberInTx(ctx, tx, projectID, userID, true)
		if err != nil {
			return err
		}
		if target.Role == project.RoleOwner {
			return domain.NewError(domain.CodeConflict, "project ownership already changed")
		}
		if target.Role == project.RoleViewer {
			return domain.NewError(domain.CodeConflict, "project owner transfer requires a member role")
		}
		if err := requireApprovedProjectUserInTx(ctx, tx, userID); err != nil {
			return domain.NewError(domain.CodeNotFound, "project member not found")
		}
		if target.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "project member version cannot be advanced")
		}
		oldOwner, err := loadProjectMemberInTx(ctx, tx, projectID, actorID, true)
		if err != nil {
			return err
		}
		if oldOwner.Version == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "project member version cannot be advanced")
		}
		if _, err := tx.Exec(ctx, `UPDATE project_members SET role='member',version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND user_id=$2 AND role='owner' AND version=$3`, projectID, actorID, oldOwner.Version); err != nil {
			return fmt.Errorf("demote former project owner: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE project_members SET role='owner',version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND user_id=$2 AND role='member' AND version=$3`, projectID, userID, target.Version); err != nil {
			return fmt.Errorf("promote new project owner: %w", err)
		}
		nextVersion := locked.Version + 1
		tag, err := tx.Exec(ctx, `UPDATE projects SET owner_id=$2,version=$3,updated_at=clock_timestamp()
WHERE id=$1 AND version=$4`, projectID, userID, nextVersion, locked.Version)
		if err != nil {
			return fmt.Errorf("update project owner: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project version changed")
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "transfer_owner", idempotencyKey, projectID,
			"project", userID, nextVersion, digest); err != nil {
			return err
		}
		locked.OwnerID = userID
		locked.Version = nextVersion
		result = locked
		return nil
	})
	if err != nil {
		return project.Project{}, err
	}
	return result, nil
}

func (s *Store) ListProjectResources(ctx context.Context, actorID, projectID string, page app.PageRequest) (app.ProjectResourcePage, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ProjectResourcePage{}, projectNotFound()
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ProjectResourcePage{}, err
	}
	values := make([]projectResourcePageRow, 0)
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, projectResourceSelect+`
WHERE pr.project_id=$1 AND pr.active
  AND ($2::timestamptz IS NULL OR (pr.created_at,pr.kind || ':' || pr.resource_id) < ($2::timestamptz,$3::text))
  AND `+projectResourceAccessPredicate+`
ORDER BY pr.created_at DESC,(pr.kind || ':' || pr.resource_id) DESC LIMIT $4`, projectID, cursorAt, cursorID, limit+1, actorID)
		if err != nil {
			return fmt.Errorf("list project resources: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value projectResourcePageRow
			var kind string
			if err := rows.Scan(&kind, &value.Resource.ResourceID, &value.Resource.Version, &value.CreatedAt); err != nil {
				return fmt.Errorf("scan project resource: %w", err)
			}
			value.Resource.ProjectID = projectID
			value.Resource.Kind = project.ResourceKind(kind)
			if !validProjectResourceValue(value.Resource) {
				return domain.NewError(domain.CodeInternal, "project resource store returned an invalid resource")
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate project resources: %w", err)
		}
		rows.Close()
		visible := values[:0]
		for _, value := range values {
			if value.Resource.Kind == project.ResourceWorkspace {
				if err := requireCurrentProjectWorkspaceResourceInTx(ctx, tx, projectID, value.Resource.ResourceID); err != nil {
					if domain.CodeOf(err) == domain.CodeNotFound {
						continue
					}
					return err
				}
			}
			visible = append(visible, value)
		}
		values = visible
		return nil
	})
	if err != nil {
		return app.ProjectResourcePage{}, err
	}
	result := app.ProjectResourcePage{Items: make([]project.Resource, 0, min(len(values), int(limit)))}
	for index := range values {
		if int32(index) == limit {
			last := values[int(limit)-1]
			result.NextCursor = encodePageCursor(last.CreatedAt, string(last.Resource.Kind)+":"+last.Resource.ResourceID)
			break
		}
		result.Items = append(result.Items, values[index].Resource)
	}
	return result, nil
}

func (s *Store) AddProjectResource(ctx context.Context, actorID string, value project.Resource, expectedVersion int64, idempotencyKey string) (project.Resource, bool, error) {
	if !domain.ValidUserID(actorID) || !validProjectResourceValue(value) || value.Version != 1 ||
		expectedVersion < 1 || expectedVersion == math.MaxInt64 || !validProjectIdempotencyKey(idempotencyKey) {
		return project.Resource{}, false, domain.InvalidArgument("resource", "project resource fields are invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		Kind            project.ResourceKind
		ResourceID      string
		ExpectedVersion int64
	}{value.ProjectID, value.Kind, value.ResourceID, expectedVersion})
	if err != nil {
		return project.Resource{}, false, err
	}
	var result project.Resource
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, value.ProjectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		var resourceOwner string
		if value.Kind == project.ResourceWorkspace {
			resourceOwner, err = requireIndependentResourceAccessInTx(ctx, tx, actorID, value.Kind, value.ResourceID, true)
			if err != nil {
				return err
			}
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "add_resource", idempotencyKey, value.ProjectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != string(value.Kind) || stored.ID != value.ResourceID || stored.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project resource result is invalid")
			}
			if value.Kind == project.ResourceWorkspace {
				if err := requireCurrentProjectWorkspaceResourceInTx(ctx, tx, value.ProjectID, value.ResourceID); err != nil {
					return err
				}
			}
			result = project.Resource{ProjectID: value.ProjectID, Kind: project.ResourceKind(stored.Kind), ResourceID: stored.ID, Version: stored.Version}
			replay = true
			return nil
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		if value.Kind != project.ResourceWorkspace {
			resourceOwner, err = requireIndependentResourceAccessInTx(ctx, tx, actorID, value.Kind, value.ResourceID, true)
			if err != nil {
				return err
			}
		}
		var active bool
		var resourceVersion int64
		err = tx.QueryRow(ctx, `SELECT active,version FROM project_resources
WHERE project_id=$1 AND kind=$2 AND resource_id=$3 FOR UPDATE`,
			value.ProjectID, string(value.Kind), value.ResourceID).Scan(&active, &resourceVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing project resource binding: %w", err)
		}
		if err == nil && active {
			return domain.NewError(domain.CodeConflict, "project resource is already bound")
		}
		if err == nil && resourceVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "project resource version cannot be advanced")
		}
		var nextResourceVersion int64 = 1
		if err == nil {
			nextResourceVersion = resourceVersion + 1
			_, err = tx.Exec(ctx, `UPDATE project_resources SET resource_owner_id=$4,host_id=$5,workspace_id=$6,
version=$7,active=true,updated_by=$8,idempotency_key=$9,request_digest=$10,updated_at=clock_timestamp()
WHERE project_id=$1 AND kind=$2 AND resource_id=$3`,
				value.ProjectID, string(value.Kind), value.ResourceID, resourceOwner,
				projectResourceHostID(value), projectResourceWorkspaceID(value), nextResourceVersion, actorID, idempotencyKey, digest)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO project_resources
(project_id,kind,resource_id,resource_owner_id,host_id,workspace_id,version,active,updated_by,idempotency_key,request_digest)
VALUES ($1,$2,$3,$4,$5,$6,1,true,$7,$8,$9)`,
				value.ProjectID, string(value.Kind), value.ResourceID, resourceOwner,
				projectResourceHostID(value), projectResourceWorkspaceID(value), actorID, idempotencyKey, digest)
		}
		if err != nil {
			if isProjectUniqueViolation(err) {
				return domain.NewError(domain.CodeIdempotencyConflict, "project resource idempotency key was already used")
			}
			return fmt.Errorf("add project resource: %w", err)
		}
		if err := bumpProjectVersionInTx(ctx, tx, value.ProjectID, locked.Version); err != nil {
			return err
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "add_resource", idempotencyKey, value.ProjectID,
			string(value.Kind), value.ResourceID, nextResourceVersion, digest); err != nil {
			return err
		}
		result = project.Resource{ProjectID: value.ProjectID, Kind: value.Kind, ResourceID: value.ResourceID, Version: nextResourceVersion}
		return nil
	})
	if err != nil {
		return project.Resource{}, false, err
	}
	return result, replay, nil
}

func (s *Store) RemoveProjectResource(ctx context.Context, actorID, projectID string, kind project.ResourceKind, resourceID string, expectedVersion int64, idempotencyKey string) error {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !validProjectResourceKindID(kind, resourceID) ||
		expectedVersion < 1 || expectedVersion == math.MaxInt64 || !validProjectIdempotencyKey(idempotencyKey) {
		return domain.InvalidArgument("resource", "project resource removal is invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		Kind            project.ResourceKind
		ResourceID      string
		ExpectedVersion int64
	}{projectID, kind, resourceID, expectedVersion})
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		locked, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleOwner, true)
		if err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "remove_resource", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != "" || stored.ID != "" || stored.Version != 0 {
				return domain.NewError(domain.CodeInternal, "idempotent project resource removal result is invalid")
			}
			return nil
		}
		if err := checkProjectExpectedVersion(expectedVersion, locked.Version); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE project_resources SET active=false,version=version+1,updated_by=$4,updated_at=clock_timestamp()
WHERE project_id=$1 AND kind=$2 AND resource_id=$3 AND active AND version < $5`,
			projectID, string(kind), resourceID, actorID, int64(math.MaxInt64))
		if err != nil {
			return fmt.Errorf("remove project resource: %w", err)
		}
		if tag.RowsAffected() != 1 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_resources WHERE project_id=$1 AND kind=$2 AND resource_id=$3 AND active)`,
				projectID, string(kind), resourceID).Scan(&exists); err != nil {
				return fmt.Errorf("check project resource before remove: %w", err)
			}
			if !exists {
				return domain.NewError(domain.CodeNotFound, "project resource not found")
			}
			return domain.NewError(domain.CodeInvalidState, "project resource version cannot be advanced")
		}
		if err := bumpProjectVersionInTx(ctx, tx, projectID, locked.Version); err != nil {
			return err
		}
		return insertProjectIdempotencyInTx(ctx, tx, actorID, "remove_resource", idempotencyKey, projectID,
			"", "", 0, digest)
	})
}

func (s *Store) GetProjectContent(ctx context.Context, actorID, projectID string, kind project.ContentKind, contentID string) (project.Content, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidContentKind(kind) || !project.ValidContentID(contentID) {
		return project.Content{}, domain.NewError(domain.CodeNotFound, "project content not found")
	}
	var result project.Content
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		value, err := loadCurrentProjectContentInTx(ctx, tx, projectID, kind, contentID, false)
		if err != nil {
			return err
		}
		if value.deleted {
			return domain.NewError(domain.CodeNotFound, "project content not found")
		}
		result = value.content
		return nil
	})
	if err != nil {
		return project.Content{}, err
	}
	return result, nil
}

func (s *Store) ListProjectAgentContext(ctx context.Context, principal identity.Principal, taskValue task.Task, limit, maxObjectBytes int) ([]project.Content, bool, error) {
	if !domain.ValidUserID(principal.UserID) || principal.UserID != taskValue.InitiatorID || !domain.ValidUserID(taskValue.OwnerID) ||
		!domain.ValidTaskID(taskValue.ID) || !domain.ValidConversationID(taskValue.ConversationID) || !project.ValidID(taskValue.ProjectID) ||
		limit < 1 || limit > 32 || maxObjectBytes < 1 || maxObjectBytes > 24<<10 {
		return nil, false, domain.NewError(domain.CodeNotFound, "project not found")
	}
	items := make([]project.Content, 0)
	omitted := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		if _, _, err := lockProjectForActorInTx(ctx, tx, principal.UserID, taskValue.ProjectID, project.RoleViewer, false); err != nil {
			return err
		}
		var currentProjectID, currentInitiatorID, currentConversationID, currentAuthKind, currentAuthTokenID, currentClientInstallationID, currentLocation, currentStatus string
		var currentAuthScopes []string
		var currentAuthVersion int64
		var currentAuthExpiresAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,''),initiator_id,conversation_id,auth_kind,auth_token_id,auth_scopes,
COALESCE(auth_client_installation_id,''),auth_version,auth_expires_at,execution_location,status
FROM tasks WHERE id=$1 AND owner_id=$2 FOR SHARE`, taskValue.ID, taskValue.OwnerID).Scan(
			&currentProjectID, &currentInitiatorID, &currentConversationID, &currentAuthKind, &currentAuthTokenID,
			&currentAuthScopes, &currentClientInstallationID, &currentAuthVersion, &currentAuthExpiresAt, &currentLocation, &currentStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "project task context not found")
			}
			return fmt.Errorf("lock project Task context authorization: %w", err)
		}
		if taskValue.ConversationContext == nil || currentProjectID != taskValue.ProjectID || currentInitiatorID != taskValue.InitiatorID ||
			currentConversationID != taskValue.ConversationID || currentAuthKind != taskValue.Authorization.Kind ||
			currentAuthKind != string(principal.Kind) || currentAuthTokenID != taskValue.Authorization.TokenID ||
			currentAuthTokenID != principal.TokenID || currentClientInstallationID != taskValue.Authorization.ClientInstallationID ||
			currentClientInstallationID != principal.ClientInstallationID || currentAuthVersion != taskValue.Authorization.Version ||
			!sameStringSlices(currentAuthScopes, taskValue.Authorization.Scopes) || !currentAuthExpiresAt.Valid ||
			taskValue.Authorization.ExpiresAt == nil || !currentAuthExpiresAt.Time.Equal(*taskValue.Authorization.ExpiresAt) ||
			!currentAuthExpiresAt.Time.After(time.Now()) || currentLocation != "server" || currentStatus != string(task.StatusRunning) ||
			taskValue.ExecutionLocation != "server" {
			return domain.NewError(domain.CodeForbidden, "project Task context authorization changed")
		}
		rows, err := tx.Query(ctx, `SELECT kind,content_id,revision,
CASE WHEN octet_length(body::text)<=$4 THEN body::text ELSE '' END,
octet_length(body::text)>$4,updated_by
FROM project_content_revisions
WHERE project_id=$1 AND current AND NOT deleted AND kind=ANY($2::text[])
ORDER BY CASE kind WHEN 'memory' THEN 0 WHEN 'goal' THEN 1 ELSE 2 END,content_id
LIMIT $3`, taskValue.ProjectID, []string{string(project.ContentMemory), string(project.ContentGoal), string(project.ContentDocument)}, limit+1, maxObjectBytes)
		if err != nil {
			return fmt.Errorf("query authorized project Agent context: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if len(items) == limit {
				omitted = true
				break
			}
			var value project.Content
			var kind, body string
			var tooLarge bool
			if err := rows.Scan(&kind, &value.ID, &value.Revision, &body, &tooLarge, &value.UpdatedBy); err != nil {
				return fmt.Errorf("scan authorized project Agent context: %w", err)
			}
			value.ProjectID, value.Kind = taskValue.ProjectID, project.ContentKind(kind)
			if tooLarge {
				omitted = true
				continue
			}
			value.Body = []byte(body)
			if err := project.ValidateContentBody(value.Kind, value.ID, value.Body); err != nil {
				return domain.NewError(domain.CodeInternal, "stored project Agent context is invalid")
			}
			items = append(items, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate authorized project Agent context: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return items, omitted, nil
}

func (s *Store) ApplyProjectContentMutation(ctx context.Context, actorID string, value project.Content, baseRevision int64, deleted bool, idempotencyKey string) (project.Content, bool, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(value.ProjectID) || !project.ValidContentKind(value.Kind) ||
		!project.ValidContentID(value.ID) || value.UpdatedBy != actorID || baseRevision < 0 ||
		baseRevision == math.MaxInt64 || value.Revision != baseRevision+1 ||
		(deleted && len(value.Body) != 0) ||
		!validProjectIdempotencyKey(idempotencyKey) {
		return project.Content{}, false, domain.InvalidArgument("content", "project content mutation is invalid")
	}
	if !deleted {
		if err := project.ValidateContentBody(value.Kind, value.ID, value.Body); err != nil {
			return project.Content{}, false, err
		}
	}
	digest, err := projectRequestDigest(struct {
		ProjectID    string
		Kind         project.ContentKind
		ContentID    string
		BaseRevision int64
		Deleted      bool
		Body         []byte
	}{value.ProjectID, value.Kind, value.ID, baseRevision, deleted, value.Body})
	if err != nil {
		return project.Content{}, false, err
	}
	var result project.Content
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, value.ProjectID, project.RoleMember, true); err != nil {
			return err
		}
		stored, found, err := findProjectIdempotencyInTx(ctx, tx, actorID, "mutate_content", idempotencyKey, value.ProjectID, digest)
		if err != nil {
			return err
		}
		if found {
			if stored.Kind != projectContentIdempotencyResultKind(value.Kind) || stored.ID != value.ID || stored.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project content result is invalid")
			}
			current, err := loadProjectContentRevisionInTx(ctx, tx, value.ProjectID, value.Kind, value.ID, stored.Version)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return domain.NewError(domain.CodeInternal, "idempotent project content revision is missing")
				}
				return err
			}
			result = current.content
			replay = true
			return nil
		}
		current, err := loadCurrentProjectContentInTx(ctx, tx, value.ProjectID, value.Kind, value.ID, true)
		currentRevision := int64(0)
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			return err
		}
		if err == nil {
			currentRevision = current.content.Revision
		}
		if currentRevision != baseRevision {
			return projectVersionConflict(baseRevision, currentRevision)
		}
		if deleted && (currentRevision == 0 || current.deleted) {
			return domain.NewError(domain.CodeNotFound, "project content not found")
		}
		if currentRevision > 0 {
			tag, err := tx.Exec(ctx, `UPDATE project_content_revisions SET current=false
WHERE project_id=$1 AND kind=$2 AND content_id=$3 AND revision=$4 AND current`,
				value.ProjectID, string(value.Kind), value.ID, currentRevision)
			if err != nil {
				return fmt.Errorf("retire current project content revision: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "project content revision changed")
			}
		}
		var body any
		if !deleted {
			body = string(value.Body)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_content_revisions
(project_id,kind,content_id,revision,body,deleted,current,updated_by,idempotency_key,request_digest)
VALUES ($1,$2,$3,$4,$5::jsonb,$6,true,$7,$8,$9)`, value.ProjectID, string(value.Kind), value.ID,
			value.Revision, body, deleted, actorID, idempotencyKey, digest); err != nil {
			return fmt.Errorf("append project content revision: %w", err)
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "mutate_content", idempotencyKey, value.ProjectID,
			projectContentIdempotencyResultKind(value.Kind), value.ID, value.Revision, digest); err != nil {
			return err
		}
		result = value
		if deleted {
			result.Body = nil
		} else {
			result.Body = append([]byte(nil), value.Body...)
		}
		return nil
	})
	if err != nil {
		return project.Content{}, false, err
	}
	return result, replay, nil
}

func projectContentIdempotencyResultKind(kind project.ContentKind) string {
	if project.ValidMuseContentKind(kind) {
		// The shared idempotency table predates Muse and intentionally retains
		// its stable result-kind constraint. The request digest still includes
		// the concrete Muse kind, and replay loads that concrete revision.
		return string(project.ContentDocument)
	}
	return string(kind)
}

func (s *Store) SearchProjectContent(ctx context.Context, actorID, query string, limit int) ([]project.ContentSearchResult, error) {
	if !domain.ValidUserID(actorID) || project.ValidateContentSearchQuery(query) != nil || limit < 1 || limit > project.MaxContentSearchResults {
		return nil, domain.InvalidArgument("search", "project content search request is invalid")
	}
	pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(query) + "%"
	rows, err := s.pool.Query(ctx, `SELECT c.project_id,c.kind,c.content_id,c.revision,c.body,
LEFT(COALESCE(NULLIF(c.body->>'title',''),c.body->>'name',''),200) AS title,
LEFT(concat_ws(' ',c.body->>'content',c.body->>'body',c.body->>'summary',c.body->>'name',c.body->>'title'),240) AS excerpt
FROM project_content_revisions c
JOIN project_members m ON m.project_id=c.project_id AND m.user_id=$1
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE c.current AND NOT c.deleted AND c.kind=ANY($3::text[])
  AND concat_ws(' ',c.body->>'title',c.body->>'name',c.body->>'content',c.body->>'body',c.body->>'summary') ILIKE $2 ESCAPE '!'
ORDER BY c.created_at DESC,c.project_id,c.kind,c.content_id
LIMIT $4`, actorID, pattern, []string{
		string(project.ContentFeedPost), string(project.ContentIdea), string(project.ContentLibrary), string(project.ContentBrief),
	}, limit)
	if err != nil {
		return nil, fmt.Errorf("search authorized project content: %w", err)
	}
	defer rows.Close()
	results := make([]project.ContentSearchResult, 0, limit)
	for rows.Next() {
		var result project.ContentSearchResult
		var body []byte
		var kind string
		if err := rows.Scan(&result.ProjectID, &kind, &result.ContentID, &result.Revision, &body, &result.Title, &result.Excerpt); err != nil {
			return nil, fmt.Errorf("scan authorized project content search result: %w", err)
		}
		result.Kind = project.ContentKind(kind)
		if err := project.ValidateContentBody(result.Kind, result.ContentID, body); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "stored project Muse content is invalid")
		}
		if err := project.ValidateContentSearchResult(result); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized project content search results: %w", err)
	}
	return results, nil
}

func (s *Store) ListProjectContentComments(ctx context.Context, actorID, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID string, limit int) ([]project.ContentComment, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidMuseContentKind(kind) ||
		!project.ValidContentID(contentID) || contentRevision < 1 || !project.ValidContentID(rowID) ||
		limit < 1 || limit > project.MaxContentCommentsPerRow {
		return nil, domain.NewError(domain.CodeNotFound, "project content comments not found")
	}
	var result []project.ContentComment
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		if err := requireProjectContentRevisionInTx(ctx, tx, projectID, kind, contentID, contentRevision); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT comment_id,version,body,deleted,updated_by
FROM project_content_comments
WHERE project_id=$1 AND content_kind=$2 AND content_id=$3 AND content_revision=$4 AND row_id=$5
  AND current AND NOT deleted
ORDER BY created_at,comment_id
LIMIT $6`, projectID, string(kind), contentID, contentRevision, rowID, limit)
		if err != nil {
			return fmt.Errorf("list project content comments: %w", err)
		}
		defer rows.Close()
		result = make([]project.ContentComment, 0, limit)
		for rows.Next() {
			var value project.ContentComment
			value.ProjectID, value.Kind, value.ContentID = projectID, kind, contentID
			value.ContentRevision, value.RowID = contentRevision, rowID
			if err := rows.Scan(&value.ID, &value.Revision, &value.Body, &value.Deleted, &value.UpdatedBy); err != nil {
				return fmt.Errorf("scan project content comment: %w", err)
			}
			if err := project.ValidateContentComment(value); err != nil {
				return domain.NewError(domain.CodeInternal, "stored project content comment is invalid")
			}
			result = append(result, value)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ApplyProjectContentCommentMutation(ctx context.Context, actorID string, value project.ContentComment, baseVersion int64, deleted bool, idempotencyKey string) (project.ContentComment, bool, error) {
	if !domain.ValidUserID(actorID) || value.UpdatedBy != actorID || !project.ValidID(value.ProjectID) ||
		!project.ValidMuseContentKind(value.Kind) || !project.ValidContentID(value.ContentID) || value.ContentRevision < 1 ||
		!project.ValidContentID(value.RowID) || !project.ValidCommentID(value.ID) || baseVersion < 0 || baseVersion == math.MaxInt64 ||
		value.Revision != baseVersion+1 || value.Deleted != deleted || !validProjectIdempotencyKey(idempotencyKey) ||
		(deleted && value.Body != "") || (!deleted && (strings.TrimSpace(value.Body) != value.Body || len([]rune(value.Body)) < 1 ||
		len([]rune(value.Body)) > project.MaxContentCommentBodyRunes)) {
		return project.ContentComment{}, false, domain.InvalidArgument("comment", "project content comment mutation is invalid")
	}
	if err := project.ValidateContentComment(value); err != nil {
		return project.ContentComment{}, false, err
	}
	idempotentCommentID := value.ID
	if baseVersion == 0 {
		idempotentCommentID = ""
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		Kind            project.ContentKind
		ContentID       string
		ContentRevision int64
		RowID           string
		CommentID       string
		BaseVersion     int64
		Deleted         bool
		Body            string
	}{value.ProjectID, value.Kind, value.ContentID, value.ContentRevision, value.RowID, idempotentCommentID, baseVersion, deleted, value.Body})
	if err != nil {
		return project.ContentComment{}, false, err
	}
	var result project.ContentComment
	var replay bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, value.ProjectID, project.RoleMember, true); err != nil {
			return err
		}
		if err := requireProjectContentRevisionInTx(ctx, tx, value.ProjectID, value.Kind, value.ContentID, value.ContentRevision); err != nil {
			return err
		}
		var storedID string
		var storedVersion int64
		var storedDigest []byte
		lookupErr := tx.QueryRow(ctx, `SELECT comment_id,version,request_digest FROM project_content_comments
WHERE project_id=$1 AND updated_by=$2 AND idempotency_key=$3`, value.ProjectID, actorID, idempotencyKey).
			Scan(&storedID, &storedVersion, &storedDigest)
		if lookupErr == nil {
			if !bytes.Equal(storedDigest, digest) {
				return domain.NewError(domain.CodeIdempotencyConflict, "project comment key was used for a different request")
			}
			result, err = loadProjectContentCommentRevisionInTx(ctx, tx, value.ProjectID, value.Kind, value.ContentID,
				value.ContentRevision, value.RowID, storedID, storedVersion)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return domain.NewError(domain.CodeInternal, "idempotent project comment revision is missing")
				}
				return err
			}
			replay = true
			return nil
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return fmt.Errorf("load project comment idempotency record: %w", lookupErr)
		}
		current, exists, err := loadCurrentProjectContentCommentInTx(ctx, tx, value.ProjectID, value.Kind, value.ContentID,
			value.ContentRevision, value.RowID, value.ID, true)
		if err != nil {
			return err
		}
		currentVersion := int64(0)
		if exists {
			currentVersion = current.Revision
		}
		if currentVersion != baseVersion {
			return projectVersionConflict(baseVersion, currentVersion)
		}
		if deleted && (!exists || current.Deleted) {
			return domain.NewError(domain.CodeNotFound, "project content comment not found")
		}
		if exists {
			tag, err := tx.Exec(ctx, `UPDATE project_content_comments SET current=false
WHERE project_id=$1 AND content_kind=$2 AND content_id=$3 AND content_revision=$4 AND row_id=$5 AND comment_id=$6 AND version=$7 AND current`,
				value.ProjectID, string(value.Kind), value.ContentID, value.ContentRevision, value.RowID, value.ID, currentVersion)
			if err != nil {
				return fmt.Errorf("retire project content comment revision: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "project content comment revision changed")
			}
		}
		var body any
		if !deleted {
			body = value.Body
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_content_comments
(project_id,content_kind,content_id,content_revision,row_id,comment_id,version,body,deleted,current,updated_by,idempotency_key,request_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,true,$10,$11,$12)`, value.ProjectID, string(value.Kind), value.ContentID,
			value.ContentRevision, value.RowID, value.ID, value.Revision, body, deleted, actorID, idempotencyKey, digest); err != nil {
			return fmt.Errorf("append project content comment revision: %w", err)
		}
		result = value
		return nil
	})
	if err != nil {
		return project.ContentComment{}, false, err
	}
	return result, replay, nil
}

func requireProjectContentRevisionInTx(ctx context.Context, tx pgx.Tx, projectID string, kind project.ContentKind, contentID string, revision int64) error {
	var deleted bool
	var body []byte
	err := tx.QueryRow(ctx, `SELECT body,deleted FROM project_content_revisions
WHERE project_id=$1 AND kind=$2 AND content_id=$3 AND revision=$4 FOR SHARE`, projectID, string(kind), contentID, revision).Scan(&body, &deleted)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && deleted {
		return domain.NewError(domain.CodeNotFound, "project content revision not found")
	}
	if err != nil {
		return fmt.Errorf("verify project content revision: %w", err)
	}
	if err := project.ValidateContentBody(kind, contentID, body); err != nil {
		return domain.NewError(domain.CodeInternal, "stored project content revision is invalid")
	}
	return nil
}

func loadCurrentProjectContentCommentInTx(ctx context.Context, tx pgx.Tx, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID, commentID string, lock bool) (project.ContentComment, bool, error) {
	query := `SELECT version,COALESCE(body,''),deleted,updated_by FROM project_content_comments
WHERE project_id=$1 AND content_kind=$2 AND content_id=$3 AND content_revision=$4 AND row_id=$5 AND comment_id=$6 AND current`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanProjectContentComment(tx.QueryRow(ctx, query, projectID, string(kind), contentID, contentRevision, rowID, commentID),
		projectID, kind, contentID, contentRevision, rowID, commentID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return project.ContentComment{}, false, nil
	}
	return value, err == nil, err
}

func loadProjectContentCommentRevisionInTx(ctx context.Context, tx pgx.Tx, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID, commentID string, version int64) (project.ContentComment, error) {
	return scanProjectContentComment(tx.QueryRow(ctx, `SELECT version,COALESCE(body,''),deleted,updated_by FROM project_content_comments
WHERE project_id=$1 AND content_kind=$2 AND content_id=$3 AND content_revision=$4 AND row_id=$5 AND comment_id=$6 AND version=$7`,
		projectID, string(kind), contentID, contentRevision, rowID, commentID, version), projectID, kind, contentID, contentRevision, rowID, commentID)
}

func scanProjectContentComment(row interface{ Scan(...any) error }, projectID string, kind project.ContentKind, contentID string, contentRevision int64, rowID, commentID string) (project.ContentComment, error) {
	value := project.ContentComment{
		ProjectID: projectID, Kind: kind, ContentID: contentID, ContentRevision: contentRevision, RowID: rowID, ID: commentID,
	}
	err := row.Scan(&value.Revision, &value.Body, &value.Deleted, &value.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.ContentComment{}, domain.NewError(domain.CodeNotFound, "project content comment not found")
	}
	if err != nil {
		return project.ContentComment{}, fmt.Errorf("load project content comment: %w", err)
	}
	if err := project.ValidateContentComment(value); err != nil {
		return project.ContentComment{}, domain.NewError(domain.CodeInternal, "stored project content comment is invalid")
	}
	return value, nil
}

const projectResourceSelect = `SELECT pr.kind,pr.resource_id,pr.version,pr.created_at
FROM project_resources pr
`

const projectResourceAccessPredicate = `(
  (pr.kind='host' AND EXISTS (
    SELECT 1 FROM hosts h
    WHERE h.id=pr.host_id AND h.owner_id=pr.resource_owner_id
      AND h.deleted_at IS NULL AND h.state NOT IN ('deleting','deleted')
      AND (h.owner_id=$5 OR EXISTS (
        SELECT 1 FROM host_members hm
        WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=$5 AND hm.revoked_at IS NULL
          AND hm.permissions && ARRAY['desktop:view','desktop:control','terminal:control','mcp:control']::text[]
      ))
  ))
  OR
  (pr.kind='workspace' AND EXISTS (
    SELECT 1 FROM workspaces w
    WHERE w.id=pr.workspace_id AND w.owner_id=pr.resource_owner_id AND w.state='ready'
      AND (w.owner_id=$5 OR EXISTS (
        SELECT 1 FROM workspace_bindings wb
        JOIN hosts h ON h.id=wb.host_id AND h.owner_id=wb.owner_id
        WHERE wb.workspace_id=w.id AND wb.owner_id=w.owner_id AND wb.state='active'
          AND h.deleted_at IS NULL AND h.state NOT IN ('deleting','deleted')
          AND (h.owner_id=$5 OR EXISTS (
            SELECT 1 FROM host_members hm
            WHERE hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=$5 AND hm.revoked_at IS NULL
              AND hm.permissions @> ARRAY['workspace:file:read']::text[]
          ))
      ))
  ))
)`

func projectResourceHostID(value project.Resource) any {
	if value.Kind == project.ResourceHost {
		return value.ResourceID
	}
	return nil
}

func projectResourceWorkspaceID(value project.Resource) any {
	if value.Kind == project.ResourceWorkspace {
		return value.ResourceID
	}
	return nil
}

func validProjectResourceValue(value project.Resource) bool {
	return project.ValidID(value.ProjectID) && validProjectResourceKindID(value.Kind, value.ResourceID) && value.Version >= 1
}

func validProjectResourceKindID(kind project.ResourceKind, resourceID string) bool {
	switch kind {
	case project.ResourceHost:
		return domain.ValidHostID(resourceID)
	case project.ResourceWorkspace:
		return domain.ValidWorkspaceID(resourceID)
	default:
		return false
	}
}

type storedProjectContent struct {
	content project.Content
	deleted bool
}

func loadCurrentProjectContentInTx(ctx context.Context, tx pgx.Tx, projectID string, kind project.ContentKind, contentID string, lock bool) (storedProjectContent, error) {
	query := `SELECT revision,body,deleted,updated_by FROM project_content_revisions
WHERE project_id=$1 AND kind=$2 AND content_id=$3 AND current`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanStoredProjectContent(tx.QueryRow(ctx, query, projectID, string(kind), contentID), projectID, kind, contentID)
}

func loadProjectContentRevisionInTx(ctx context.Context, tx pgx.Tx, projectID string, kind project.ContentKind, contentID string, revision int64) (storedProjectContent, error) {
	return scanStoredProjectContent(tx.QueryRow(ctx, `SELECT revision,body,deleted,updated_by FROM project_content_revisions
WHERE project_id=$1 AND kind=$2 AND content_id=$3 AND revision=$4`,
		projectID, string(kind), contentID, revision), projectID, kind, contentID)
}

func scanStoredProjectContent(row interface{ Scan(...any) error }, projectID string, kind project.ContentKind, contentID string) (storedProjectContent, error) {
	var result storedProjectContent
	var body []byte
	if err := row.Scan(&result.content.Revision, &body, &result.deleted, &result.content.UpdatedBy); errors.Is(err, pgx.ErrNoRows) {
		return storedProjectContent{}, domain.NewError(domain.CodeNotFound, "project content not found")
	} else if err != nil {
		return storedProjectContent{}, fmt.Errorf("load project content revision: %w", err)
	}
	result.content.ProjectID, result.content.Kind, result.content.ID = projectID, kind, contentID
	if !result.deleted {
		result.content.Body = append([]byte(nil), body...)
		if err := project.ValidateContentBody(kind, contentID, result.content.Body); err != nil {
			return storedProjectContent{}, domain.NewError(domain.CodeInternal, "stored project content is invalid")
		}
	} else if len(body) != 0 {
		return storedProjectContent{}, domain.NewError(domain.CodeInternal, "deleted project content has a body")
	}
	if result.content.Revision < 1 || !domain.ValidUserID(result.content.UpdatedBy) {
		return storedProjectContent{}, domain.NewError(domain.CodeInternal, "stored project content metadata is invalid")
	}
	return result, nil
}

func requireIndependentResourceAccessInTx(ctx context.Context, tx pgx.Tx, actorID string, kind project.ResourceKind, resourceID string, lock bool) (string, error) {
	if !domain.ValidUserID(actorID) || !validProjectResourceKindID(kind, resourceID) {
		return "", domain.NewError(domain.CodeNotFound, "project resource not found")
	}
	lockClause := ""
	if lock {
		lockClause = " FOR SHARE"
	}
	switch kind {
	case project.ResourceHost:
		var ownerID, state string
		var deletedAt *time.Time
		err := tx.QueryRow(ctx, "SELECT owner_id,state,deleted_at FROM hosts WHERE id=$1"+lockClause, resourceID).
			Scan(&ownerID, &state, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (deletedAt != nil || state == "deleting" || state == "deleted") {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		if err != nil {
			return "", fmt.Errorf("read Host resource access: %w", err)
		}
		if ownerID == actorID {
			return ownerID, nil
		}
		var permissions []string
		err = tx.QueryRow(ctx, `SELECT permissions FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL`+lockClause,
			resourceID, ownerID, actorID).Scan(&permissions)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		if err != nil {
			return "", fmt.Errorf("read Host share access: %w", err)
		}
		if !hasHostLevelProjectPermission(permissions) {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		return ownerID, nil
	case project.ResourceWorkspace:
		var ownerID, state string
		err := tx.QueryRow(ctx, "SELECT owner_id,state FROM workspaces WHERE id=$1"+lockClause, resourceID).Scan(&ownerID, &state)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "ready" {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		if err != nil {
			return "", fmt.Errorf("read Workspace resource access: %w", err)
		}
		if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, resourceID); err != nil {
			return "", err
		}
		if ownerID == actorID {
			return ownerID, nil
		}
		var hostID, hostOwnerID string
		err = tx.QueryRow(ctx, `SELECT h.id,h.owner_id FROM workspace_bindings wb
JOIN hosts h ON h.id=wb.host_id AND h.owner_id=wb.owner_id
WHERE wb.workspace_id=$1 AND wb.owner_id=$2 AND wb.state='active'
  AND h.deleted_at IS NULL AND h.state NOT IN ('deleting','deleted')`+func() string {
			if lock {
				return " FOR SHARE OF wb,h"
			}
			return ""
		}(), resourceID, ownerID).Scan(&hostID, &hostOwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		if err != nil {
			return "", fmt.Errorf("read Workspace Host binding access: %w", err)
		}
		if hostOwnerID == actorID {
			return ownerID, nil
		}
		var permissions []string
		err = tx.QueryRow(ctx, `SELECT permissions FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL`+lockClause,
			hostID, hostOwnerID, actorID).Scan(&permissions)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		if err != nil {
			return "", fmt.Errorf("read Workspace share access: %w", err)
		}
		if !hasWorkspaceProjectPermission(permissions) {
			return "", domain.NewError(domain.CodeNotFound, "project resource not found")
		}
		return ownerID, nil
	default:
		return "", domain.NewError(domain.CodeNotFound, "project resource not found")
	}
}

func hasHostLevelProjectPermission(values []string) bool {
	for _, value := range values {
		switch host.Permission(value) {
		case host.PermissionDesktopView, host.PermissionDesktopControl, host.PermissionTerminalControl, host.PermissionMCPControl:
			return true
		}
	}
	return false
}

func hasWorkspaceProjectPermission(values []string) bool {
	for _, value := range values {
		if host.Permission(value) == host.PermissionWorkspaceFileRead {
			return true
		}
	}
	return false
}

func loadProjectInTx(ctx context.Context, tx pgx.Tx, projectID string) (project.Project, error) {
	var result project.Project
	err := tx.QueryRow(ctx, "SELECT id,owner_id,name,description,version FROM projects WHERE id=$1", projectID).
		Scan(&result.ID, &result.OwnerID, &result.Name, &result.Description, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Project{}, projectNotFound()
	}
	if err != nil {
		return project.Project{}, fmt.Errorf("load project: %w", err)
	}
	if result.Validate() != nil {
		return project.Project{}, domain.NewError(domain.CodeInternal, "stored project is invalid")
	}
	return result, nil
}

func checkProjectExpectedVersion(expectedVersion, currentVersion int64) error {
	if expectedVersion != currentVersion {
		return projectVersionConflict(expectedVersion, currentVersion)
	}
	return nil
}
