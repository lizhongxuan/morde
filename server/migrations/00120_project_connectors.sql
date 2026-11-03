-- +goose Up
CREATE TABLE project_mcp_connector_grants (
  project_id         text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  connector_id       text NOT NULL CHECK (connector_id ~ '^k[a-z0-9]{7}$'),
  connector_owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  version            bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  authorized_by      text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  active             boolean NOT NULL DEFAULT true,
  revoked_at         timestamptz,
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, connector_id),
  CHECK (authorized_by = connector_owner_id),
  CHECK ((active AND revoked_at IS NULL) OR (NOT active AND revoked_at IS NOT NULL))
);

CREATE INDEX project_mcp_connector_grants_active
  ON project_mcp_connector_grants(project_id, connector_id)
  WHERE active;

-- Task owner scopes the Task and operation ledgers; connector_owner_id scopes
-- the personal connector. These IDs differ for project Tasks initiated by a
-- member other than the project conversation owner.
ALTER TABLE task_mcp_connector_grants
  ADD COLUMN connector_owner_id text,
  ADD COLUMN project_id text,
  ADD COLUMN project_grant_version bigint;

UPDATE task_mcp_connector_grants
SET connector_owner_id = owner_id;

ALTER TABLE task_mcp_connector_grants
  ALTER COLUMN connector_owner_id SET NOT NULL,
  ADD CONSTRAINT task_mcp_connector_grants_connector_owner_fkey
    FOREIGN KEY (connector_owner_id) REFERENCES users(id) ON DELETE RESTRICT,
  ADD CONSTRAINT task_mcp_connector_grants_project_fkey
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT,
  ADD CONSTRAINT task_mcp_connector_grants_project_version_check
    CHECK ((project_id IS NULL AND project_grant_version IS NULL) OR
           (project_id IS NOT NULL AND project_grant_version > 0));

-- A Task-level selection is not an authorization to share a connector with a
-- project. Require a fresh explicit project grant for new project Tasks.
UPDATE task_mcp_connector_grants g
SET revoked_at = COALESCE(g.revoked_at, clock_timestamp()),
    grant_version = g.grant_version + CASE WHEN g.revoked_at IS NULL THEN 1 ELSE 0 END,
    updated_at = clock_timestamp()
FROM tasks t
WHERE t.id = g.task_id AND t.owner_id = g.owner_id AND t.project_id IS NOT NULL
  AND g.revoked_at IS NULL;

-- Fence any in-flight operation that predates the explicit project-grant
-- model. A dispatched request may already have changed the remote system, so
-- preserve it for unknown-result recovery rather than treating it as cancelled.
UPDATE task_mcp_connector_operations o
SET status = 'cancelled', result = 'project connector authorization requires a fresh explicit grant',
    updated_at = clock_timestamp()
WHERE o.status = 'prepared'
  AND EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    JOIN tasks t ON t.id = g.task_id AND t.owner_id = g.owner_id
    WHERE g.task_id = o.task_id AND g.owner_id = o.owner_id AND g.connector_id = o.connector_id
      AND t.project_id IS NOT NULL AND g.revoked_at IS NOT NULL
  );

UPDATE task_mcp_connector_operations o
SET cancel_requested = true, result = 'project connector authorization requires a fresh explicit grant',
    updated_at = clock_timestamp()
WHERE o.status = 'dispatched'
  AND EXISTS (
    SELECT 1 FROM task_mcp_connector_grants g
    JOIN tasks t ON t.id = g.task_id AND t.owner_id = g.owner_id
    WHERE g.task_id = o.task_id AND g.owner_id = o.owner_id AND g.connector_id = o.connector_id
      AND t.project_id IS NOT NULL AND g.revoked_at IS NOT NULL
  );

-- +goose Down
ALTER TABLE task_mcp_connector_grants
  DROP CONSTRAINT task_mcp_connector_grants_project_version_check,
  DROP CONSTRAINT task_mcp_connector_grants_project_fkey,
  DROP CONSTRAINT task_mcp_connector_grants_connector_owner_fkey,
  DROP COLUMN project_grant_version,
  DROP COLUMN project_id,
  DROP COLUMN connector_owner_id;
DROP INDEX project_mcp_connector_grants_active;
DROP TABLE project_mcp_connector_grants;
