-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT workspace_id
    FROM workspace_bindings
    WHERE state IN ('preparing', 'active', 'unbinding', 'failed')
    GROUP BY workspace_id
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'resolve duplicate non-detached workspace bindings before applying migration 00026';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_bindings_one_active_workspace;
CREATE UNIQUE INDEX workspace_bindings_one_non_detached_workspace
  ON workspace_bindings(workspace_id)
  WHERE state IN ('preparing', 'active', 'unbinding', 'failed');

-- +goose Down
DROP INDEX workspace_bindings_one_non_detached_workspace;
CREATE UNIQUE INDEX workspace_bindings_one_active_workspace
  ON workspace_bindings(workspace_id)
  WHERE state IN ('preparing', 'active', 'unbinding');
