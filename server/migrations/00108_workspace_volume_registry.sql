-- +goose Up
CREATE TABLE workspace_volumes (
  workspace_id text PRIMARY KEY,
  owner_id text NOT NULL,
  state text NOT NULL CHECK (state IN ('legacy', 'pending', 'ready')),
  volume_name text,
  legacy_host_id text,
  ready_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CONSTRAINT workspace_volumes_workspace_owner_fk
    FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  CONSTRAINT workspace_volumes_state_shape CHECK (
    (state = 'legacy' AND volume_name IS NULL AND ready_at IS NULL)
    OR (state = 'pending' AND volume_name = 'workspace-' || workspace_id AND legacy_host_id IS NULL AND ready_at IS NULL)
    OR (state = 'ready' AND volume_name = 'workspace-' || workspace_id AND legacy_host_id IS NULL AND ready_at IS NOT NULL)
  )
);

CREATE UNIQUE INDEX workspace_volumes_registered_name_unique
  ON workspace_volumes(volume_name) WHERE volume_name IS NOT NULL;

-- Existing Workspace roots belong to Host volumes. Only a unique active binding
-- can continue using its old Host volume until W22 migrates that data.
WITH active_bindings AS (
  SELECT workspace_id,
         CASE WHEN count(*) = 1 THEN min(host_id) ELSE NULL END AS legacy_host_id
  FROM workspace_bindings
  WHERE state IN ('preparing', 'active')
  GROUP BY workspace_id
)
INSERT INTO workspace_volumes (workspace_id, owner_id, state, legacy_host_id)
SELECT w.id, w.owner_id, 'legacy', active_bindings.legacy_host_id
FROM workspaces AS w
LEFT JOIN active_bindings ON active_bindings.workspace_id = w.id;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_volumes WHERE state <> 'legacy') THEN
    RAISE EXCEPTION 'cannot roll back Workspace volume registration after provisioning';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE workspace_volumes;
