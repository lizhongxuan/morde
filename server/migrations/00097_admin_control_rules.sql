-- +goose Up
CREATE TABLE admin_control_rules (
  id bigserial PRIMARY KEY,
  threshold bigint NOT NULL CHECK (threshold > 0),
  enabled boolean NOT NULL DEFAULT true,
  note text NOT NULL DEFAULT '' CHECK (char_length(note) <= 256),
  version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  created_by text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  last_fired_at timestamptz,
  fired_accounts bigint NOT NULL DEFAULT 0 CHECK (fired_accounts >= 0),
  CHECK ((last_fired_at IS NULL) = (fired_accounts = 0))
);

CREATE INDEX admin_control_rules_pending_idx
  ON admin_control_rules (threshold, id)
  WHERE enabled AND last_fired_at IS NULL;

CREATE TABLE admin_control_notifications (
  id bigserial PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('rule', 'test')),
  rule_id bigint,
  accounts_total bigint NOT NULL CHECK (accounts_total >= 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK ((kind = 'rule') = (rule_id IS NOT NULL))
);

CREATE INDEX admin_control_notifications_recent_idx
  ON admin_control_notifications (created_at DESC, id DESC);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM admin_control_notifications) OR EXISTS (SELECT 1 FROM admin_control_rules) THEN
    RAISE EXCEPTION 'cannot roll back admin control rules while rules or local notifications exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE admin_control_notifications;
DROP TABLE admin_control_rules;
