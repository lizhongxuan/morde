-- +goose Up
-- A UUID generation prevents a revoke/regrant of identical permissions from
-- recreating the same Agent authorization snapshot, even within one timestamp
-- precision interval.
ALTER TABLE host_members
  ADD COLUMN grant_version uuid NOT NULL DEFAULT gen_random_uuid();

-- +goose Down
-- A persisted member Agent fence depends on this grant generation to detect a
-- rapid revoke/regrant. Keep it until those fences have been retired.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_instances WHERE host_authorization_kind = 'member') THEN
    RAISE EXCEPTION 'cannot roll back Host member grant versions while member Agent records exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE host_members DROP COLUMN grant_version;
