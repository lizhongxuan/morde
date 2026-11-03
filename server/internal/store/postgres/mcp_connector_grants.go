package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func validateNormalizedTaskMCPConnectorIDs(connectorIDs []string) error {
	if len(connectorIDs) > 16 {
		return domain.InvalidArgument("mcp_connector_ids", "too many MCP connectors were selected")
	}
	for index, id := range connectorIDs {
		if !domain.ValidMCPConnectorID(id) || index > 0 && connectorIDs[index-1] >= id {
			return domain.InvalidArgument("mcp_connector_ids", "MCP connector IDs must be sorted and unique")
		}
	}
	return nil
}

// insertTaskMCPConnectorGrants keeps the grant keyed by Task owner while
// snapshotting the separately owned personal connector. Project Tasks must
// select only connectors with an active, explicit project grant.
func insertTaskMCPConnectorGrants(ctx context.Context, tx pgx.Tx, taskValue task.Task, connectorIDs []string) error {
	if !domain.ValidUserID(taskValue.OwnerID) || !domain.ValidUserID(taskValue.InitiatorID) || !domain.ValidTaskID(taskValue.ID) ||
		(taskValue.ProjectID != "" && !project.ValidID(taskValue.ProjectID)) {
		return domain.InvalidArgument("mcp_connector_ids", "MCP connector grant fields are invalid")
	}
	if err := validateNormalizedTaskMCPConnectorIDs(connectorIDs); err != nil {
		return err
	}
	if len(connectorIDs) == 0 {
		return nil
	}
	query := `
SELECT id, owner_id, revision, credential_version, NULL::text, NULL::bigint
FROM mcp_connectors
WHERE owner_id=$1 AND enabled=true AND id=ANY($2::text[])
ORDER BY id
FOR SHARE`
	args := []any{taskValue.OwnerID, connectorIDs}
	if taskValue.ProjectID != "" {
		query = `
SELECT g.connector_id, g.connector_owner_id, c.revision, c.credential_version, g.project_id, g.version
FROM project_mcp_connector_grants g
JOIN project_members actor_member ON actor_member.project_id=g.project_id AND actor_member.user_id=$2
JOIN users actor_user ON actor_user.id=actor_member.user_id AND actor_user.status='approved'
JOIN project_members connector_owner_member ON connector_owner_member.project_id=g.project_id AND connector_owner_member.user_id=g.connector_owner_id
JOIN users connector_owner_user ON connector_owner_user.id=connector_owner_member.user_id AND connector_owner_user.status='approved'
JOIN mcp_connectors c ON c.id=g.connector_id AND c.owner_id=g.connector_owner_id AND c.enabled
WHERE g.project_id=$1 AND g.active AND g.authorized_by=g.connector_owner_id AND g.connector_id=ANY($3::text[])
ORDER BY g.connector_id
FOR SHARE OF g,actor_member,actor_user,connector_owner_member,connector_owner_user,c`
		args = []any{taskValue.ProjectID, taskValue.InitiatorID, connectorIDs}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("lock selected MCP connectors: %w", err)
	}
	type connectorSnapshot struct {
		id                  string
		ownerID             string
		revision            int64
		credentialVersion   int64
		projectID           *string
		projectGrantVersion *int64
	}
	snapshots := make([]connectorSnapshot, 0, len(connectorIDs))
	for rows.Next() {
		var snapshot connectorSnapshot
		if err := rows.Scan(&snapshot.id, &snapshot.ownerID, &snapshot.revision, &snapshot.credentialVersion,
			&snapshot.projectID, &snapshot.projectGrantVersion); err != nil {
			rows.Close()
			return fmt.Errorf("scan selected MCP connector: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read selected MCP connectors: %w", err)
	}
	rows.Close()
	if len(snapshots) != len(connectorIDs) {
		// Do not distinguish missing, disabled, and foreign-owned connector IDs.
		return domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	for _, snapshot := range snapshots {
		var projectID any
		if snapshot.projectID != nil {
			projectID = *snapshot.projectID
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO task_mcp_connector_grants
  (task_id, owner_id, connector_id, connector_revision, credential_version, connector_owner_id, project_id, project_grant_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, taskValue.ID, taskValue.OwnerID, snapshot.id, snapshot.revision,
			snapshot.credentialVersion, snapshot.ownerID, projectID, snapshot.projectGrantVersion); err != nil {
			return fmt.Errorf("create task MCP connector grant: %w", err)
		}
	}
	return nil
}

func requireTaskMCPConnectorGrantSet(ctx context.Context, tx pgx.Tx, ownerID, taskID string, selected []string) error {
	rows, err := tx.Query(ctx, `SELECT connector_id FROM task_mcp_connector_grants
WHERE owner_id=$1 AND task_id=$2 AND revoked_at IS NULL ORDER BY connector_id`, ownerID, taskID)
	if err != nil {
		return fmt.Errorf("read task MCP connector grant set: %w", err)
	}
	defer rows.Close()
	granted := make([]string, 0, len(selected))
	for rows.Next() {
		var connectorID string
		if err := rows.Scan(&connectorID); err != nil {
			return fmt.Errorf("scan task MCP connector grant set: %w", err)
		}
		granted = append(granted, connectorID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read task MCP connector grant set: %w", err)
	}
	if len(granted) != len(selected) {
		return domain.NewError(domain.CodeConflict, "active task MCP connector grants cannot be changed")
	}
	for index := range granted {
		if granted[index] != selected[index] {
			return domain.NewError(domain.CodeConflict, "active task MCP connector grants cannot be changed")
		}
	}
	return nil
}

func listTaskMCPConnectorGrantsTx(ctx context.Context, tx pgx.Tx, ownerID, taskID string) ([]app.TaskMCPConnectorGrant, error) {
	rows, err := tx.Query(ctx, `
SELECT task_id, owner_id, connector_owner_id, COALESCE(project_id,''), COALESCE(project_grant_version,0),
       connector_id, connector_revision, credential_version, grant_version, tool_schema, tool_schema_sha256,
       revoked_at, created_at, updated_at
FROM task_mcp_connector_grants
WHERE owner_id=$1 AND task_id=$2
ORDER BY connector_id`, ownerID, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task MCP connector grants: %w", err)
	}
	defer rows.Close()
	items := make([]app.TaskMCPConnectorGrant, 0)
	for rows.Next() {
		grant, err := scanTaskMCPConnectorGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task MCP connector grant: %w", err)
		}
		items = append(items, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read task MCP connector grants: %w", err)
	}
	return items, nil
}

type taskMCPConnectorGrantScanner interface {
	Scan(dest ...any) error
}

func scanTaskMCPConnectorGrant(row taskMCPConnectorGrantScanner) (app.TaskMCPConnectorGrant, error) {
	var grant app.TaskMCPConnectorGrant
	var schema []byte
	var digest []byte
	var revokedAt *time.Time
	err := row.Scan(
		&grant.TaskID, &grant.OwnerID, &grant.ConnectorOwnerID, &grant.ProjectID, &grant.ProjectGrantVersion,
		&grant.ConnectorID, &grant.ConnectorRevision, &grant.CredentialVersion, &grant.GrantVersion,
		&schema, &digest, &revokedAt, &grant.CreatedAt, &grant.UpdatedAt,
	)
	if err != nil {
		return app.TaskMCPConnectorGrant{}, err
	}
	grant.ToolSchema = append([]byte(nil), schema...)
	grant.ToolSchemaSHA256 = append([]byte(nil), digest...)
	if revokedAt != nil {
		revoked := revokedAt.UTC()
		grant.RevokedAt = &revoked
	}
	return grant, nil
}

func (s *Store) ListTaskMCPConnectorGrants(ctx context.Context, ownerID, taskID string) ([]app.TaskMCPConnectorGrant, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) {
		return nil, domain.NewError(domain.CodeNotFound, "task not found")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin task MCP connector grant read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	items, err := listTaskMCPConnectorGrantsTx(ctx, tx, ownerID, taskID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit task MCP connector grant read: %w", err)
	}
	return items, nil
}

func (s *Store) SetTaskMCPConnectorToolSchema(ctx context.Context, ownerID, taskID, connectorID string, expectedGrantVersion int64, schema, digest []byte) (app.TaskMCPConnectorGrant, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidMCPConnectorID(connectorID) || expectedGrantVersion < 1 ||
		len(schema) == 0 || len(schema) > 1<<20 || !json.Valid(schema) || len(digest) != sha256.Size {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("tool_schema", "MCP connector tool schema is invalid")
	}
	expectedDigest := sha256.Sum256(schema)
	if !bytes.Equal(expectedDigest[:], digest) {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("tool_schema", "MCP connector tool schema digest is invalid")
	}
	var grant app.TaskMCPConnectorGrant
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var existingSchema, existingDigest []byte
		var revokedAt *time.Time
		var currentVersion int64
		err := tx.QueryRow(ctx, `
SELECT grant_version, tool_schema, tool_schema_sha256, revoked_at
FROM task_mcp_connector_grants
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3
FOR UPDATE`, ownerID, taskID, connectorID).Scan(&currentVersion, &existingSchema, &existingDigest, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task MCP connector grant not found")
		}
		if err != nil {
			return fmt.Errorf("lock task MCP connector grant schema: %w", err)
		}
		if revokedAt != nil || currentVersion != expectedGrantVersion {
			return domain.NewError(domain.CodeConflict, "task MCP connector grant changed")
		}
		if existingSchema != nil || existingDigest != nil {
			if !bytes.Equal(existingDigest, digest) || !jsonEquivalent(existingSchema, schema) {
				return domain.NewError(domain.CodeConflict, "task MCP connector tool schema was already fixed")
			}
		} else {
			if _, err := tx.Exec(ctx, `
UPDATE task_mcp_connector_grants
SET tool_schema=$4, tool_schema_sha256=$5, updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3
  AND grant_version=$6 AND revoked_at IS NULL AND tool_schema IS NULL`,
				ownerID, taskID, connectorID, schema, digest, expectedGrantVersion); err != nil {
				return fmt.Errorf("pin task MCP connector tool schema: %w", err)
			}
		}
		grant, err = scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `
SELECT task_id, owner_id, connector_owner_id, COALESCE(project_id,''), COALESCE(project_grant_version,0),
       connector_id, connector_revision, credential_version, grant_version, tool_schema, tool_schema_sha256,
       revoked_at, created_at, updated_at
FROM task_mcp_connector_grants WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3`, ownerID, taskID, connectorID))
		if err != nil {
			return fmt.Errorf("read pinned task MCP connector grant: %w", err)
		}
		return nil
	})
	return grant, err
}

func jsonEquivalent(left, right []byte) bool {
	if !json.Valid(left) || !json.Valid(right) {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftCanonical, err := json.Marshal(leftValue)
	if err != nil {
		return false
	}
	rightCanonical, err := json.Marshal(rightValue)
	return err == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func (s *Store) RevokeTaskMCPConnectorGrant(ctx context.Context, ownerID, taskID, connectorID string, expectedGrantVersion int64) (app.TaskMCPConnectorGrant, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidMCPConnectorID(connectorID) || expectedGrantVersion < 1 {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("grant", "task, connector and positive grant version are required")
	}
	var grant app.TaskMCPConnectorGrant
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentVersion int64
		var revokedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT grant_version, revoked_at FROM task_mcp_connector_grants
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 FOR UPDATE`, ownerID, taskID, connectorID).Scan(&currentVersion, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task MCP connector grant not found")
		}
		if err != nil {
			return fmt.Errorf("lock task MCP connector grant: %w", err)
		}
		if revokedAt != nil || currentVersion != expectedGrantVersion {
			return domain.NewError(domain.CodeConflict, "task MCP connector grant changed")
		}
		grant, err = scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `
UPDATE task_mcp_connector_grants
SET revoked_at=clock_timestamp(), grant_version=grant_version+1, updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 AND grant_version=$4 AND revoked_at IS NULL
RETURNING task_id, owner_id, connector_owner_id, COALESCE(project_id,''), COALESCE(project_grant_version,0),
          connector_id, connector_revision, credential_version, grant_version, tool_schema, tool_schema_sha256,
          revoked_at, created_at, updated_at`,
			ownerID, taskID, connectorID, expectedGrantVersion))
		if err != nil {
			return fmt.Errorf("revoke task MCP connector grant: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET status='cancelled', result='connector grant revoked before dispatch', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 AND status='prepared'`, ownerID, taskID, connectorID); err != nil {
			return fmt.Errorf("cancel prepared MCP operations after grant revocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET cancel_requested=true, result='connector grant revoked while call was in flight', updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 AND status='dispatched'`, ownerID, taskID, connectorID); err != nil {
			return fmt.Errorf("request cancellation of in-flight MCP operations after grant revocation: %w", err)
		}
		return nil
	})
	return grant, err
}

var _ app.TaskMCPConnectorGrantStore = (*Store)(nil)
