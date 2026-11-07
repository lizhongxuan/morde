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
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

type taskMCPSelection struct {
	IDs       []string
	MessageID string
	Revision  int64
}

func lockTaskMCPSelection(ctx context.Context, tx pgx.Tx, value task.Task) (int64, error) {
	fence := deferredTargetFence(value)
	current, err := lockAuthorizedAgentTask(ctx, tx, fence)
	if err != nil {
		return 0, err
	}
	if !matchesDeferredTaskAgent(current, value) {
		return 0, domain.NewError(domain.CodeUnauthenticated, "MCP Task Agent is unavailable")
	}
	if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
		return 0, err
	}
	var revision int64
	err = tx.QueryRow(ctx, `SELECT current_revision FROM tasks
WHERE id=$1 AND owner_id=$2 AND current_revision=desired_revision`, value.ID, value.OwnerID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.NewError(domain.CodeConflict, "task revision is being adjusted")
	}
	if err != nil {
		return 0, fmt.Errorf("read MCP Task revision: %w", err)
	}
	stored, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
	if err != nil {
		return 0, fmt.Errorf("read MCP Task authorization: %w", err)
	}
	if err := checkCurrentTaskMCPAuthorization(ctx, tx, stored, &value.Authorization); err != nil {
		return 0, err
	}
	return revision, nil
}

func checkCurrentTaskMCPAuthorization(ctx context.Context, tx pgx.Tx, stored sqlcgen.Task, claimed *task.Authorization) error {
	actual := taskDomain(stored).Authorization
	if claimed != nil {
		sameExpiry := actual.ExpiresAt == nil && claimed.ExpiresAt == nil ||
			actual.ExpiresAt != nil && claimed.ExpiresAt != nil && actual.ExpiresAt.Equal(*claimed.ExpiresAt)
		if actual.Kind != claimed.Kind || actual.TokenID != claimed.TokenID || actual.ClientInstallationID != claimed.ClientInstallationID ||
			actual.Version != claimed.Version || !sameStringSlices(actual.Scopes, claimed.Scopes) || !sameExpiry {
			return domain.NewError(domain.CodeUnauthenticated, "MCP Task authorization changed")
		}
	}
	authorized, err := requireCurrentTaskAuthorization(ctx, tx, stored)
	if err != nil {
		return err
	}
	if !authorized {
		return domain.NewError(domain.CodeUnauthenticated, "MCP Task authorization is unavailable")
	}
	principal := identity.Principal{Kind: identity.Kind(actual.Kind), Scopes: scheduleTaskScopes(actual.Scopes)}
	if !principal.HasScope(identity.ScopeMCPControl) {
		return domain.NewError(domain.CodeForbidden, "MCP Task authorization is unavailable")
	}
	var scopes []string
	var active bool
	err = tx.QueryRow(ctx, `SELECT scopes,revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, actual.TokenID, stored.InitiatorID, actual.Kind).Scan(&scopes, &active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return domain.NewError(domain.CodeUnauthenticated, "MCP Task credential is unavailable")
	}
	if err != nil {
		return fmt.Errorf("check MCP Task credential: %w", err)
	}
	principal.Scopes = scheduleTaskScopes(scopes)
	if !principal.HasScope(identity.ScopeMCPControl) {
		return domain.NewError(domain.CodeForbidden, "MCP Task credential scope changed")
	}
	return nil
}

func loadTaskMCPSelection(ctx context.Context, tx pgx.Tx, value task.Task, revision int64) (taskMCPSelection, error) {
	var selection taskMCPSelection
	var raw []byte
	var sourceInstance, currentInstance string
	err := tx.QueryRow(ctx, `SELECT m.id,r.request_metadata,r.service_instance_id::text,service.instance_id::text FROM conversation_message_routes r
JOIN service_identity service ON service.singleton=true
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.conversation_id=r.conversation_id AND m.actor_id=r.actor_id
WHERE m.task_id=$1 AND m.owner_id=$2 AND m.conversation_id=$3 AND m.actor_id=$4
 AND m.role='user' AND r.state='routed'
 AND NOT EXISTS (SELECT 1 FROM task_revision_intents intent WHERE intent.message_id=m.id AND intent.task_id=m.task_id)
ORDER BY m.sequence LIMIT 1`, value.ID, value.OwnerID, value.ConversationID, value.InitiatorID).
		Scan(&selection.MessageID, &raw, &sourceInstance, &currentInstance)
	if errors.Is(err, pgx.ErrNoRows) {
		rows, queryErr := tx.Query(ctx, `SELECT connector_id FROM task_mcp_connector_grants
WHERE task_id=$1 AND owner_id=$2 AND revoked_at IS NULL ORDER BY connector_id`, value.ID, value.OwnerID)
		if queryErr != nil {
			return taskMCPSelection{}, fmt.Errorf("read standalone Task MCP selection: %w", queryErr)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return taskMCPSelection{}, fmt.Errorf("scan standalone Task MCP selection: %w", err)
			}
			selection.IDs = append(selection.IDs, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return taskMCPSelection{}, fmt.Errorf("iterate standalone Task MCP selection: %w", err)
		}
	} else if err != nil {
		return taskMCPSelection{}, fmt.Errorf("read initial Task MCP selection: %w", err)
	} else {
		if sourceInstance != currentInstance {
			return taskMCPSelection{}, domain.NewError(domain.CodeConflict, "MCP selection belongs to another service instance")
		}
		var input app.ConversationRoutingInput
		if json.Unmarshal(raw, &input) != nil || input.ProjectID != value.ProjectID || validateNormalizedTaskMCPConnectorIDs(input.MCPConnectorIDs) != nil {
			return taskMCPSelection{}, domain.NewError(domain.CodeInvalidState, "initial Task MCP selection is invalid")
		}
		selection.IDs = input.MCPConnectorIDs
	}
	rows, err := tx.Query(ctx, `SELECT intent.message_id,intent.expected_revision,r.request_metadata,
r.service_instance_id::text,service.instance_id::text FROM task_revision_intents intent
JOIN conversation_message_routes r ON r.message_id=intent.message_id AND r.owner_id=intent.owner_id
 AND r.actor_id=intent.initiator_id AND r.conversation_id=intent.conversation_id
JOIN service_identity service ON service.singleton=true
WHERE intent.task_id=$1 AND intent.owner_id=$2 AND intent.initiator_id=$3 AND intent.conversation_id=$4
 AND intent.state='applied' AND intent.expected_revision<$5
ORDER BY intent.expected_revision`, value.ID, value.OwnerID, value.InitiatorID, value.ConversationID, revision)
	if err != nil {
		return taskMCPSelection{}, fmt.Errorf("read revised Task MCP selections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var expected int64
		var metadata []byte
		var routeInstance, serviceInstance string
		if err := rows.Scan(&id, &expected, &metadata, &routeInstance, &serviceInstance); err != nil {
			return taskMCPSelection{}, fmt.Errorf("scan revised Task MCP selection: %w", err)
		}
		if routeInstance != serviceInstance {
			return taskMCPSelection{}, domain.NewError(domain.CodeConflict, "MCP revision belongs to another service instance")
		}
		var input app.ConversationRoutingInput
		if json.Unmarshal(metadata, &input) != nil || input.ProjectID != value.ProjectID || validateNormalizedTaskMCPConnectorIDs(input.MCPConnectorIDs) != nil {
			return taskMCPSelection{}, domain.NewError(domain.CodeInvalidState, "revised Task MCP selection is invalid")
		}
		if len(input.MCPConnectorIDs) != 0 {
			selection.IDs, selection.MessageID, selection.Revision = input.MCPConnectorIDs, id, expected+1
		}
	}
	if err := rows.Err(); err != nil {
		return taskMCPSelection{}, fmt.Errorf("iterate revised Task MCP selections: %w", err)
	}
	rows.Close()
	if selection.MessageID != "" {
		if err := checkTaskMCPSelectionSource(ctx, tx, value, selection); err != nil {
			return taskMCPSelection{}, err
		}
	}
	return selection, nil
}

func checkTaskMCPSelectionSource(ctx context.Context, tx pgx.Tx, value task.Task, selection taskMCPSelection) error {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT r.authorization_snapshot FROM conversation_message_routes r
JOIN service_identity service ON service.singleton=true AND service.instance_id=r.service_instance_id
WHERE r.message_id=$1 AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4 FOR SHARE OF r`,
		selection.MessageID, value.OwnerID, value.InitiatorID, value.ConversationID).Scan(&raw)
	if err != nil {
		return domain.NewError(domain.CodeForbidden, "MCP selection authorization is unavailable")
	}
	var authorization task.Authorization
	if json.Unmarshal(raw, &authorization) != nil || authorization.Version < 1 || !domain.ValidTokenID(authorization.TokenID) ||
		authorization.ExpiresAt == nil || !authorization.ExpiresAt.After(time.Now()) ||
		(authorization.Kind != string(identity.KindSession) && authorization.Kind != string(identity.KindClient)) {
		return domain.NewError(domain.CodeForbidden, "MCP selection authorization is unavailable")
	}
	principal := identity.Principal{Kind: identity.Kind(authorization.Kind), Scopes: scheduleTaskScopes(authorization.Scopes)}
	if !principal.HasScope(identity.ScopeTaskSubmit) || !principal.HasScope(identity.ScopeMCPControl) ||
		(selection.Revision > 0 && !principal.HasScope(identity.ScopeTaskControl)) {
		return domain.NewError(domain.CodeForbidden, "MCP selection scope is unavailable")
	}
	var active bool
	var scopes []string
	err = tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3
 AND COALESCE(client_installation_id,'')=$4,scopes FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`,
		authorization.TokenID, value.InitiatorID, authorization.Kind, authorization.ClientInstallationID).Scan(&active, &scopes)
	if err != nil || !active {
		return domain.NewError(domain.CodeForbidden, "MCP selection credential is unavailable")
	}
	principal.Scopes = scheduleTaskScopes(scopes)
	if !principal.HasScope(identity.ScopeTaskSubmit) || !principal.HasScope(identity.ScopeMCPControl) ||
		(selection.Revision > 0 && !principal.HasScope(identity.ScopeTaskControl)) {
		return domain.NewError(domain.CodeForbidden, "MCP selection credential scope changed")
	}
	if authorization.Kind == string(identity.KindClient) && !domain.ValidClientInstallationID(authorization.ClientInstallationID) {
		return domain.NewError(domain.CodeForbidden, "MCP selection installation is unavailable")
	}
	if authorization.ClientInstallationID != "" && checkModelInstallation(ctx, tx, authorization.ClientInstallationID, value.InitiatorID) != nil {
		return domain.NewError(domain.CodeForbidden, "MCP selection installation is unavailable")
	}
	return nil
}

func (s *Store) ListSelectedTaskMCPConnectors(ctx context.Context, value task.Task) ([]string, error) {
	var result []string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		revision, err := lockTaskMCPSelection(ctx, tx, value)
		if err != nil {
			return err
		}
		selection, err := loadTaskMCPSelection(ctx, tx, value, revision)
		if err != nil {
			return err
		}
		result = append([]string{}, selection.IDs...)
		return nil
	})
	return result, err
}

func selectedTaskMCPGrant(ctx context.Context, tx pgx.Tx, value task.Task, connectorID string) (app.TaskMCPConnectorGrant, error) {
	revision, err := lockTaskMCPSelection(ctx, tx, value)
	if err != nil {
		return app.TaskMCPConnectorGrant{}, err
	}
	selection, err := loadTaskMCPSelection(ctx, tx, value, revision)
	if err != nil {
		return app.TaskMCPConnectorGrant{}, err
	}
	selected := false
	for _, id := range selection.IDs {
		selected = selected || id == connectorID
	}
	if !selected {
		return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "MCP connector was not selected for this task")
	}
	grant, err := scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `SELECT task_id,owner_id,connector_owner_id,COALESCE(project_id,''),
COALESCE(project_grant_version,0),connector_id,connector_revision,credential_version,grant_version,
tool_schema,tool_schema_sha256,revoked_at,created_at,updated_at FROM task_mcp_connector_grants
WHERE task_id=$1 AND owner_id=$2 AND connector_id=$3 FOR UPDATE`, value.ID, value.OwnerID, connectorID))
	if errors.Is(err, pgx.ErrNoRows) {
		if selection.MessageID == "" {
			return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "MCP connector grant is unavailable")
		}
		if err := requireResolvedTaskMCPOperations(ctx, tx, value.ID); err != nil {
			return app.TaskMCPConnectorGrant{}, err
		}
		if err := insertTaskMCPConnectorGrants(ctx, tx, value, []string{connectorID}); err != nil {
			return app.TaskMCPConnectorGrant{}, err
		}
		grant, err = scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `SELECT task_id,owner_id,connector_owner_id,COALESCE(project_id,''),
COALESCE(project_grant_version,0),connector_id,connector_revision,credential_version,grant_version,
tool_schema,tool_schema_sha256,revoked_at,created_at,updated_at FROM task_mcp_connector_grants
WHERE task_id=$1 AND owner_id=$2 AND connector_id=$3`, value.ID, value.OwnerID, connectorID))
		if err != nil {
			return app.TaskMCPConnectorGrant{}, fmt.Errorf("read selected Task MCP grant: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_mcp_selection_sources
(task_id,owner_id,connector_id,message_id,task_revision,grant_version) VALUES ($1,$2,$3,$4,$5,$6)`,
			value.ID, value.OwnerID, connectorID, selection.MessageID, selection.Revision, grant.GrantVersion); err != nil {
			return app.TaskMCPConnectorGrant{}, fmt.Errorf("record Task MCP grant selection: %w", err)
		}
		return verifySelectedTaskMCPGrant(ctx, tx, value, grant)
	}
	if err != nil {
		return app.TaskMCPConnectorGrant{}, fmt.Errorf("lock selected Task MCP grant: %w", err)
	}
	if selection.MessageID == "" {
		if grant.RevokedAt != nil {
			return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "MCP connector grant is unavailable")
		}
		return verifySelectedTaskMCPGrant(ctx, tx, value, grant)
	}
	var sourceID string
	var sourceRevision, sourceVersion int64
	err = tx.QueryRow(ctx, `SELECT message_id,task_revision,grant_version FROM task_mcp_selection_sources
WHERE task_id=$1 AND owner_id=$2 AND connector_id=$3 FOR UPDATE`, value.ID, value.OwnerID, connectorID).
		Scan(&sourceID, &sourceRevision, &sourceVersion)
	if errors.Is(err, pgx.ErrNoRows) && selection.Revision > 0 {
		// A standalone Task may acquire its first routed selection in a later revision.
		sourceRevision = -1
	} else if err != nil {
		return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "MCP grant selection provenance is unavailable")
	}
	if sourceID == selection.MessageID {
		if sourceRevision != selection.Revision || sourceVersion != grant.GrantVersion || grant.RevokedAt != nil {
			return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeForbidden, "MCP connector grant was revoked")
		}
		return verifySelectedTaskMCPGrant(ctx, tx, value, grant)
	}
	if selection.Revision <= sourceRevision {
		return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeConflict, "MCP grant selection is stale")
	}
	if err := requireResolvedTaskMCPOperations(ctx, tx, value.ID); err != nil {
		return app.TaskMCPConnectorGrant{}, err
	}
	var connectorOwner string
	var connectorRevision, credentialVersion int64
	var projectID *string
	var projectVersion *int64
	if value.ProjectID == "" {
		err = tx.QueryRow(ctx, `SELECT owner_id,revision,credential_version FROM mcp_connectors
WHERE id=$1 AND owner_id=$2 AND enabled FOR SHARE`, connectorID, value.InitiatorID).
			Scan(&connectorOwner, &connectorRevision, &credentialVersion)
	} else {
		err = tx.QueryRow(ctx, `SELECT g.connector_owner_id,c.revision,c.credential_version,g.project_id,g.version
FROM project_mcp_connector_grants g
JOIN project_members actor ON actor.project_id=g.project_id AND actor.user_id=$2
JOIN users actor_user ON actor_user.id=actor.user_id AND actor_user.status='approved'
JOIN project_members owner ON owner.project_id=g.project_id AND owner.user_id=g.connector_owner_id
JOIN users owner_user ON owner_user.id=owner.user_id AND owner_user.status='approved'
JOIN mcp_connectors c ON c.id=g.connector_id AND c.owner_id=g.connector_owner_id AND c.enabled
WHERE g.project_id=$1 AND g.connector_id=$3 AND g.active AND g.authorized_by=g.connector_owner_id
FOR SHARE OF g,actor,actor_user,owner,owner_user,c`, value.ProjectID, value.InitiatorID, connectorID).
			Scan(&connectorOwner, &connectorRevision, &credentialVersion, &projectID, &projectVersion)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskMCPConnectorGrant{}, domain.NewError(domain.CodeNotFound, "MCP connector is unavailable")
	}
	if err != nil {
		return app.TaskMCPConnectorGrant{}, fmt.Errorf("authorize revised MCP selection: %w", err)
	}
	grant, err = scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `UPDATE task_mcp_connector_grants
SET connector_owner_id=$4,connector_revision=$5,credential_version=$6,project_id=$7,project_grant_version=$8,
 grant_version=grant_version+1,revoked_at=NULL,tool_schema=NULL,tool_schema_sha256=NULL,updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND connector_id=$3
RETURNING task_id,owner_id,connector_owner_id,COALESCE(project_id,''),COALESCE(project_grant_version,0),
 connector_id,connector_revision,credential_version,grant_version,tool_schema,tool_schema_sha256,revoked_at,created_at,updated_at`,
		value.ID, value.OwnerID, connectorID, connectorOwner, connectorRevision, credentialVersion, projectID, projectVersion))
	if err != nil {
		return app.TaskMCPConnectorGrant{}, fmt.Errorf("renew Task MCP grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_mcp_selection_sources(task_id,owner_id,connector_id,message_id,task_revision,grant_version)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (task_id,connector_id) DO UPDATE
SET message_id=EXCLUDED.message_id,task_revision=EXCLUDED.task_revision,grant_version=EXCLUDED.grant_version`,
		value.ID, value.OwnerID, connectorID, selection.MessageID, selection.Revision, grant.GrantVersion); err != nil {
		return app.TaskMCPConnectorGrant{}, fmt.Errorf("record revised Task MCP selection: %w", err)
	}
	return verifySelectedTaskMCPGrant(ctx, tx, value, grant)
}

func requireResolvedTaskMCPOperations(ctx context.Context, tx pgx.Tx, taskID string) error {
	var unresolved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_mcp_connector_operations
WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))`, taskID).Scan(&unresolved); err != nil {
		return fmt.Errorf("check pending MCP operations: %w", err)
	}
	if unresolved {
		return domain.NewError(domain.CodeConflict, "previous MCP operation requires reconciliation")
	}
	return nil
}

func verifySelectedTaskMCPGrant(ctx context.Context, tx pgx.Tx, value task.Task, grant app.TaskMCPConnectorGrant) (app.TaskMCPConnectorGrant, error) {
	operation := app.MCPConnectorOperation{TaskID: value.ID, OwnerID: value.OwnerID, ConnectorID: grant.ConnectorID,
		ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion, GrantVersion: grant.GrantVersion}
	if err := lockCurrentTaskMCPConnectorGrant(ctx, tx, operation); err != nil {
		return app.TaskMCPConnectorGrant{}, err
	}
	return grant, nil
}

func (s *Store) GrantSelectedTaskMCPConnector(ctx context.Context, value task.Task, connectorID string) (app.TaskMCPConnectorGrant, error) {
	if ctx == nil || !domain.ValidMCPConnectorID(connectorID) {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("connector_id", "MCP connector id is invalid")
	}
	var result app.TaskMCPConnectorGrant
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = selectedTaskMCPGrant(ctx, tx, value, connectorID)
		return err
	})
	return result, err
}

func (s *Store) PinSelectedTaskMCPSchema(ctx context.Context, value task.Task, connectorID string, version int64, schema, digest []byte) (app.TaskMCPConnectorGrant, error) {
	if ctx == nil || !domain.ValidMCPConnectorID(connectorID) || version < 1 || len(schema) == 0 || len(schema) > 1<<20 ||
		!json.Valid(schema) || len(digest) != sha256.Size {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("tool_schema", "MCP tool schema is invalid")
	}
	sum := sha256.Sum256(schema)
	if !bytes.Equal(sum[:], digest) {
		return app.TaskMCPConnectorGrant{}, domain.InvalidArgument("tool_schema", "MCP tool schema digest is invalid")
	}
	var result app.TaskMCPConnectorGrant
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		grant, err := selectedTaskMCPGrant(ctx, tx, value, connectorID)
		if err != nil {
			return err
		}
		if grant.GrantVersion != version {
			return domain.NewError(domain.CodeConflict, "MCP grant changed during discovery")
		}
		operation := app.MCPConnectorOperation{TaskID: value.ID, OwnerID: value.OwnerID, ConnectorID: connectorID,
			ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion, GrantVersion: grant.GrantVersion}
		if err := lockCurrentTaskMCPConnectorGrant(ctx, tx, operation); err != nil {
			return err
		}
		if len(grant.ToolSchema) != 0 || len(grant.ToolSchemaSHA256) != 0 {
			if !bytes.Equal(grant.ToolSchema, schema) || !bytes.Equal(grant.ToolSchemaSHA256, digest) {
				return domain.NewError(domain.CodeConflict, "MCP tool catalog changed after authorization")
			}
			result = grant
			return nil
		}
		grant, err = scanTaskMCPConnectorGrant(tx.QueryRow(ctx, `UPDATE task_mcp_connector_grants
SET tool_schema=$4,tool_schema_sha256=$5,updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND connector_id=$3 AND grant_version=$6 AND revoked_at IS NULL
 AND tool_schema IS NULL AND tool_schema_sha256 IS NULL
RETURNING task_id,owner_id,connector_owner_id,COALESCE(project_id,''),COALESCE(project_grant_version,0),
 connector_id,connector_revision,credential_version,grant_version,tool_schema,tool_schema_sha256,revoked_at,created_at,updated_at`,
			value.ID, value.OwnerID, connectorID, schema, digest, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "MCP grant changed during discovery")
		}
		if err != nil {
			return fmt.Errorf("pin selected Task MCP schema: %w", err)
		}
		result = grant
		return nil
	})
	return result, err
}

var _ app.TaskMCPSelectionStore = (*Store)(nil)
