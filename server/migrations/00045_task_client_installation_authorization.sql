-- +goose Up
ALTER TABLE tasks
  ADD COLUMN auth_client_installation_id text REFERENCES client_installations(id);

UPDATE tasks AS task
SET auth_client_installation_id = token.client_installation_id
FROM tokens AS token
WHERE task.auth_kind = 'client'
  AND token.id = task.auth_token_id
  AND token.kind = 'client'
  AND token.client_installation_id IS NOT NULL;

-- +goose StatementBegin
DO $$
DECLARE
  orphaned_client_task_count bigint;
BEGIN
  SELECT count(*) INTO orphaned_client_task_count
  FROM tasks
  WHERE auth_kind = 'client'
    AND auth_client_installation_id IS NULL;

  IF orphaned_client_task_count > 0 THEN
    RAISE EXCEPTION 'cannot apply client task installation snapshot migration: % client task(s) have no matching client token', orphaned_client_task_count;
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE tasks ADD CONSTRAINT tasks_client_installation_snapshot_check CHECK (
  (auth_kind = 'client' AND auth_client_installation_id IS NOT NULL)
  OR (auth_kind <> 'client' AND auth_client_installation_id IS NULL)
);

-- +goose Down
ALTER TABLE tasks DROP CONSTRAINT tasks_client_installation_snapshot_check;
ALTER TABLE tasks DROP COLUMN auth_client_installation_id;
