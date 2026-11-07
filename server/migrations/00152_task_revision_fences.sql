-- +goose Up
-- An accepted revision has one immutable predecessor. Concurrent messages
-- cannot silently create two different successors to the same Task revision.
CREATE UNIQUE INDEX task_revision_intents_task_predecessor
  ON task_revision_intents(task_id, expected_revision);
ALTER TABLE task_revision_intents
  ADD COLUMN available_after timestamptz NOT NULL DEFAULT clock_timestamp(),
  ADD COLUMN stop_request_id text REFERENCES task_stop_requests(id) ON DELETE RESTRICT,
  ADD COLUMN interrupted_lease_owner text NOT NULL DEFAULT '',
  ADD COLUMN interrupted_lease_version bigint NOT NULL DEFAULT 0 CHECK (interrupted_lease_version >= 0),
  ADD COLUMN interrupted_agent_instance_id text NOT NULL DEFAULT '',
  ADD COLUMN interrupted_agent_generation bigint NOT NULL DEFAULT 0 CHECK (interrupted_agent_generation >= 0);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_revision_intents)
     OR EXISTS (SELECT 1 FROM tasks WHERE current_revision <> desired_revision) THEN
    RAISE EXCEPTION 'cannot remove revision fences while revision records exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_revision_intents_task_predecessor;
ALTER TABLE task_revision_intents DROP COLUMN available_after;
ALTER TABLE task_revision_intents DROP COLUMN stop_request_id, DROP COLUMN interrupted_lease_owner,
  DROP COLUMN interrupted_lease_version, DROP COLUMN interrupted_agent_instance_id, DROP COLUMN interrupted_agent_generation;
