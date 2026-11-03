-- +goose Up
ALTER TABLE hosts
  ADD COLUMN control_actor_id text,
  ADD COLUMN control_lease_expires_at timestamptz,
  ADD CONSTRAINT hosts_control_lease_actor_pair CHECK ((control_actor_id IS NULL) = (control_lease_expires_at IS NULL));

ALTER TABLE host_control_transitions
  ADD COLUMN actor_user_id text;
UPDATE host_control_transitions SET actor_user_id=owner_id WHERE phase <> 'completed';

CREATE INDEX hosts_control_lease_expiry
  ON hosts(control_lease_expires_at)
  WHERE control_actor_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM hosts WHERE control_actor_id IS NOT NULL)
     OR EXISTS (SELECT 1 FROM host_control_transitions WHERE phase <> 'completed' AND actor_user_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove Host control lease state while actor ownership is recorded';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX hosts_control_lease_expiry;
ALTER TABLE host_control_transitions DROP COLUMN actor_user_id;
ALTER TABLE hosts DROP CONSTRAINT hosts_control_lease_actor_pair,
  DROP COLUMN control_lease_expires_at,
  DROP COLUMN control_actor_id;
