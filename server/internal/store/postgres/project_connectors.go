package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

var _ app.ProjectMCPConnectorStore = (*Store)(nil)
var _ app.ProjectMCPConnectorAuthorizationStore = (*Store)(nil)

func (s *Store) ListProjectMCPConnectors(ctx context.Context, actorID, projectID string) ([]app.ProjectMCPConnector, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return nil, projectNotFound()
	}
	rows, err := s.pool.Query(ctx, `SELECT g.project_id,g.connector_owner_id,g.connector_id,g.version,g.authorized_by,
       g.active,g.revoked_at,g.created_at,g.updated_at,c.name,c.enabled,c.revision
FROM project_mcp_connector_grants g
JOIN project_members actor_member ON actor_member.project_id=g.project_id AND actor_member.user_id=$2
JOIN users actor_user ON actor_user.id=actor_member.user_id AND actor_user.status='approved'
JOIN project_members owner_member ON owner_member.project_id=g.project_id AND owner_member.user_id=g.connector_owner_id
JOIN users owner_user ON owner_user.id=owner_member.user_id AND owner_user.status='approved'
JOIN mcp_connectors c ON c.id=g.connector_id AND c.owner_id=g.connector_owner_id
WHERE g.project_id=$1 AND g.active
ORDER BY g.connector_id LIMIT 17`, projectID, actorID)
	if err != nil {
		return nil, fmt.Errorf("list project MCP connector grants: %w", err)
	}
	defer rows.Close()
	items := make([]app.ProjectMCPConnector, 0)
	for rows.Next() {
		item, err := scanProjectMCPConnector(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project MCP connector grant: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project MCP connector grants: %w", err)
	}
	if len(items) > 16 {
		return nil, domain.NewError(domain.CodeInternal, "project has too many active MCP connector grants")
	}
	return items, nil
}

func (s *Store) GrantProjectMCPConnector(ctx context.Context, actorID, projectID, connectorID string, expectedVersion int64) (app.ProjectMCPConnector, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidMCPConnectorID(connectorID) || expectedVersion < 0 {
		return app.ProjectMCPConnector{}, domain.InvalidArgument("grant", "project connector grant fields are invalid")
	}
	var result app.ProjectMCPConnector
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, true); err != nil {
			return err
		}
		var name string
		var enabled bool
		var connectorRevision int64
		if err := tx.QueryRow(ctx, `SELECT name,enabled,revision FROM mcp_connectors
WHERE owner_id=$1 AND id=$2 FOR SHARE`, actorID, connectorID).Scan(&name, &enabled, &connectorRevision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "MCP connector not found")
			}
			return fmt.Errorf("verify project connector owner: %w", err)
		}

		var currentVersion int64
		var active bool
		var revokedAt *time.Time
		var currentOwnerID string
		err := tx.QueryRow(ctx, `SELECT connector_owner_id,version,active,revoked_at FROM project_mcp_connector_grants
WHERE project_id=$1 AND connector_id=$2 FOR UPDATE`, projectID, connectorID).Scan(&currentOwnerID, &currentVersion, &active, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			if expectedVersion != 0 {
				return domain.NewError(domain.CodeConflict, "project connector grant changed")
			}
			if err := requireProjectMCPConnectorCapacity(ctx, tx, projectID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO project_mcp_connector_grants
(project_id,connector_id,connector_owner_id,version,authorized_by,active)
VALUES ($1,$2,$3,1,$3,true)`, projectID, connectorID, actorID); err != nil {
				return fmt.Errorf("create project MCP connector grant: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("lock project MCP connector grant: %w", err)
		} else {
			if expectedVersion != currentVersion {
				return domain.NewError(domain.CodeConflict, "project connector grant changed")
			}
			if active {
				if currentOwnerID != actorID {
					return domain.NewError(domain.CodeConflict, "project connector grant belongs to another connector owner")
				}
				// PUT of the current grant is an idempotent confirmation by its owner.
				if revokedAt != nil {
					return domain.NewError(domain.CodeInternal, "active project connector grant has a revoke timestamp")
				}
			} else {
				if revokedAt == nil {
					return domain.NewError(domain.CodeInternal, "inactive project connector grant lacks a revoke timestamp")
				}
				if err := requireProjectMCPConnectorCapacity(ctx, tx, projectID); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `UPDATE project_mcp_connector_grants
SET connector_owner_id=$3,authorized_by=$3,active=true,revoked_at=NULL,version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND connector_id=$2 AND version=$4
RETURNING version`, projectID, connectorID, actorID, expectedVersion).Scan(&currentVersion); err != nil {
					return fmt.Errorf("reactivate project MCP connector grant: %w", err)
				}
			}
		}
		result = app.ProjectMCPConnector{
			Grant: project.MCPConnectorGrant{ProjectID: projectID, ConnectorOwnerID: actorID, ConnectorID: connectorID,
				Version: currentVersionOrOne(currentVersion), AuthorizedBy: actorID, Active: true,
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
			Name: name, Enabled: enabled, ConnectorRevision: connectorRevision,
		}
		return tx.QueryRow(ctx, `SELECT created_at,updated_at FROM project_mcp_connector_grants
WHERE project_id=$1 AND connector_id=$2`, projectID, connectorID).Scan(&result.Grant.CreatedAt, &result.Grant.UpdatedAt)
	})
	return result, err
}

func requireProjectMCPConnectorCapacity(ctx context.Context, tx pgx.Tx, projectID string) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM project_mcp_connector_grants WHERE project_id=$1 AND active`, projectID).Scan(&count); err != nil {
		return fmt.Errorf("count project MCP connector grants: %w", err)
	}
	if count >= 16 {
		return domain.NewError(domain.CodeQuotaExceeded, "project MCP connector grant limit is reached")
	}
	return nil
}

func currentVersionOrOne(value int64) int64 {
	if value < 1 {
		return 1
	}
	return value
}

func (s *Store) RevokeProjectMCPConnector(ctx context.Context, actorID, projectID, connectorID string, expectedVersion int64) (app.ProjectMCPConnector, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidMCPConnectorID(connectorID) || expectedVersion < 1 {
		return app.ProjectMCPConnector{}, domain.InvalidArgument("grant", "project connector revoke fields are invalid")
	}
	var result app.ProjectMCPConnector
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		var projectOwnerID, connectorOwnerID string
		var version int64
		var active bool
		err := tx.QueryRow(ctx, `SELECT p.owner_id,g.connector_owner_id,g.version,g.active
FROM projects p JOIN project_mcp_connector_grants g ON g.project_id=p.id
WHERE p.id=$1 AND g.connector_id=$2 FOR UPDATE OF p,g`, projectID, connectorID).
			Scan(&projectOwnerID, &connectorOwnerID, &version, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "project connector grant not found")
		}
		if err != nil {
			return fmt.Errorf("lock project MCP connector grant for revoke: %w", err)
		}
		if actorID != connectorOwnerID && actorID != projectOwnerID {
			return domain.NewError(domain.CodeForbidden, "project connector grant can only be revoked by its owner or project owner")
		}
		if !active || version != expectedVersion {
			return domain.NewError(domain.CodeConflict, "project connector grant changed")
		}
		if err := tx.QueryRow(ctx, `UPDATE project_mcp_connector_grants
SET active=false,revoked_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND connector_id=$2 AND version=$3 AND active
RETURNING version,revoked_at,created_at,updated_at`, projectID, connectorID, expectedVersion).
			Scan(&result.Grant.Version, &result.Grant.RevokedAt, &result.Grant.CreatedAt, &result.Grant.UpdatedAt); err != nil {
			return fmt.Errorf("revoke project MCP connector grant: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_grants
SET revoked_at=clock_timestamp(),grant_version=grant_version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND connector_id=$2 AND connector_owner_id=$3 AND revoked_at IS NULL`,
			projectID, connectorID, connectorOwnerID); err != nil {
			return fmt.Errorf("revoke project Task MCP connector grants: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET status='cancelled',result='project connector authorization revoked before dispatch',updated_at=clock_timestamp()
FROM tasks t
WHERE o.task_id=t.id AND o.owner_id=t.owner_id AND t.project_id=$1 AND o.connector_id=$2
  AND o.status='prepared' AND EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    WHERE g.task_id=t.id AND g.owner_id=t.owner_id AND g.connector_id=o.connector_id
      AND g.connector_owner_id=$3 AND g.project_id=$1 AND g.revoked_at IS NOT NULL
  )`, projectID, connectorID, connectorOwnerID); err != nil {
			return fmt.Errorf("cancel prepared project MCP operations: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET cancel_requested=true,result='project connector authorization revoked while call was in flight',updated_at=clock_timestamp()
FROM tasks t
WHERE o.task_id=t.id AND o.owner_id=t.owner_id AND t.project_id=$1 AND o.connector_id=$2
  AND o.status='dispatched' AND EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    WHERE g.task_id=t.id AND g.owner_id=t.owner_id AND g.connector_id=o.connector_id
      AND g.connector_owner_id=$3 AND g.project_id=$1 AND g.revoked_at IS NOT NULL
  )`, projectID, connectorID, connectorOwnerID); err != nil {
			return fmt.Errorf("request cancellation of in-flight project MCP operations: %w", err)
		}
		var name string
		var enabled bool
		var connectorRevision int64
		connectorErr := tx.QueryRow(ctx, `SELECT name,enabled,revision FROM mcp_connectors WHERE id=$1 AND owner_id=$2`, connectorID, connectorOwnerID).
			Scan(&name, &enabled, &connectorRevision)
		if connectorErr != nil && !errors.Is(connectorErr, pgx.ErrNoRows) {
			return fmt.Errorf("read project connector after revoke: %w", connectorErr)
		}
		result.Grant.ProjectID, result.Grant.ConnectorOwnerID, result.Grant.ConnectorID = projectID, connectorOwnerID, connectorID
		result.Grant.AuthorizedBy, result.Grant.Active = connectorOwnerID, false
		result.Name, result.Enabled, result.ConnectorRevision = name, enabled, connectorRevision
		return nil
	})
	return result, err
}

func (s *Store) AuthorizeProjectMCPConnector(ctx context.Context, projectID, actorID, connectorOwnerID, connectorID string, expectedVersion int64) error {
	if !project.ValidID(projectID) || !domain.ValidUserID(actorID) || !domain.ValidUserID(connectorOwnerID) ||
		!domain.ValidMCPConnectorID(connectorID) || expectedVersion < 1 {
		return domain.NewError(domain.CodeForbidden, "project connector authorization is unavailable")
	}
	var found int
	err := s.pool.QueryRow(ctx, `SELECT 1
FROM project_mcp_connector_grants g
JOIN project_members actor_member ON actor_member.project_id=g.project_id AND actor_member.user_id=$2
JOIN users actor_user ON actor_user.id=actor_member.user_id AND actor_user.status='approved'
JOIN project_members owner_member ON owner_member.project_id=g.project_id AND owner_member.user_id=g.connector_owner_id
JOIN users owner_user ON owner_user.id=owner_member.user_id AND owner_user.status='approved'
JOIN mcp_connectors c ON c.id=g.connector_id AND c.owner_id=g.connector_owner_id
WHERE g.project_id=$1 AND g.connector_owner_id=$3 AND g.connector_id=$4
  AND g.active AND g.version=$5 AND g.authorized_by=g.connector_owner_id AND c.enabled
FOR SHARE OF g,actor_member,actor_user,owner_member,owner_user,c`,
		projectID, actorID, connectorOwnerID, connectorID, expectedVersion).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeForbidden, "project connector authorization is no longer active")
	}
	if err != nil {
		return fmt.Errorf("recheck project MCP connector authorization: %w", err)
	}
	return nil
}

func revokeProjectMCPConnectorMemberInTx(ctx context.Context, tx pgx.Tx, projectID, userID string) error {
	if _, err := tx.Exec(ctx, `UPDATE project_mcp_connector_grants
SET active=false,revoked_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp()
WHERE project_id=$1 AND connector_owner_id=$2 AND active`, projectID, userID); err != nil {
		return fmt.Errorf("revoke project connectors owned by removed member: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_grants g
SET revoked_at=clock_timestamp(),grant_version=grant_version+1,updated_at=clock_timestamp()
FROM tasks t
WHERE g.task_id=t.id AND g.owner_id=t.owner_id AND g.project_id=$1
  AND (t.initiator_id=$2 OR g.connector_owner_id=$2) AND g.revoked_at IS NULL`, projectID, userID); err != nil {
		return fmt.Errorf("revoke removed member project Task connector grants: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET status='cancelled',result='project connector authorization revoked before dispatch',updated_at=clock_timestamp()
FROM tasks t
WHERE o.task_id=t.id AND o.owner_id=t.owner_id AND t.project_id=$1
  AND (t.initiator_id=$2 OR EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    WHERE g.task_id=t.id AND g.owner_id=t.owner_id AND g.connector_id=o.connector_id
      AND g.project_id=$1 AND g.connector_owner_id=$2
  )) AND o.status='prepared'`, projectID, userID); err != nil {
		return fmt.Errorf("cancel prepared project connector operations after member removal: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET cancel_requested=true,result='project connector authorization revoked while call was in flight',updated_at=clock_timestamp()
FROM tasks t
WHERE o.task_id=t.id AND o.owner_id=t.owner_id AND t.project_id=$1
  AND (t.initiator_id=$2 OR EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    WHERE g.task_id=t.id AND g.owner_id=t.owner_id AND g.connector_id=o.connector_id
      AND g.project_id=$1 AND g.connector_owner_id=$2
	  )) AND o.status='dispatched'`, projectID, userID); err != nil {
		return fmt.Errorf("request cancellation of in-flight project connector operations after member removal: %w", err)
	}
	return nil
}

func revokeProjectMCPConnectorOwnerInTx(ctx context.Context, tx pgx.Tx, ownerID, connectorID string) error {
	if _, err := tx.Exec(ctx, `UPDATE project_mcp_connector_grants
SET active=false,revoked_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp()
WHERE connector_owner_id=$1 AND connector_id=$2 AND active`, ownerID, connectorID); err != nil {
		return fmt.Errorf("revoke project grants for deleted MCP connector: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_grants
SET revoked_at=clock_timestamp(),grant_version=grant_version+1,updated_at=clock_timestamp()
WHERE connector_owner_id=$1 AND connector_id=$2 AND project_id IS NOT NULL AND revoked_at IS NULL`, ownerID, connectorID); err != nil {
		return fmt.Errorf("revoke project Task grants for deleted MCP connector: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET status='cancelled',result='project connector was deleted before dispatch',updated_at=clock_timestamp()
WHERE o.connector_id=$2 AND o.status='prepared' AND EXISTS (
  SELECT 1 FROM task_mcp_connector_grants g
  WHERE g.task_id=o.task_id AND g.owner_id=o.owner_id AND g.connector_id=o.connector_id
    AND g.connector_owner_id=$1 AND g.project_id IS NOT NULL AND g.revoked_at IS NOT NULL
)`, ownerID, connectorID); err != nil {
		return fmt.Errorf("cancel project connector operations before dispatch: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations o
SET cancel_requested=true,result='project connector was deleted while call was in flight',updated_at=clock_timestamp()
WHERE o.connector_id=$2 AND o.status='dispatched' AND EXISTS (
  SELECT 1 FROM task_mcp_connector_grants g
  WHERE g.task_id=o.task_id AND g.owner_id=o.owner_id AND g.connector_id=o.connector_id
    AND g.connector_owner_id=$1 AND g.project_id IS NOT NULL AND g.revoked_at IS NOT NULL
)`, ownerID, connectorID); err != nil {
		return fmt.Errorf("request cancellation of in-flight project connector operations: %w", err)
	}
	return nil
}

type projectMCPConnectorScanner interface {
	Scan(...any) error
}

func scanProjectMCPConnector(row projectMCPConnectorScanner) (app.ProjectMCPConnector, error) {
	var item app.ProjectMCPConnector
	var revokedAt *time.Time
	err := row.Scan(&item.Grant.ProjectID, &item.Grant.ConnectorOwnerID, &item.Grant.ConnectorID,
		&item.Grant.Version, &item.Grant.AuthorizedBy, &item.Grant.Active, &revokedAt,
		&item.Grant.CreatedAt, &item.Grant.UpdatedAt, &item.Name, &item.Enabled, &item.ConnectorRevision)
	if err != nil {
		return app.ProjectMCPConnector{}, err
	}
	item.Grant.RevokedAt = revokedAt
	if err := item.Grant.Validate(); err != nil {
		return app.ProjectMCPConnector{}, err
	}
	return item, nil
}
