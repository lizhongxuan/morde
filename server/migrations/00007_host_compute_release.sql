-- +goose Up
ALTER TABLE hosts DROP CONSTRAINT hosts_state_check;
ALTER TABLE hosts ADD CONSTRAINT hosts_state_check
  CHECK (state IN ('creating', 'running', 'stopping', 'stopped', 'failed', 'deleting', 'deleted'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM hosts WHERE state IN ('stopping', 'stopped')) THEN
    RAISE EXCEPTION 'cannot roll back host compute release while hosts are stopping or stopped';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE hosts DROP CONSTRAINT hosts_state_check;
ALTER TABLE hosts ADD CONSTRAINT hosts_state_check
  CHECK (state IN ('creating', 'running', 'failed', 'deleting', 'deleted'));
