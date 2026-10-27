-- +goose Up
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_actor_kind_check;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_actor_kind_check
  CHECK (actor_kind IN ('session', 'api_key', 'host_cookie', 'system', 'client'));

-- +goose Down
-- Restoring the previous check fails atomically if client audit records exist.
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_actor_kind_check;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_actor_kind_check
  CHECK (actor_kind IN ('session', 'api_key', 'host_cookie', 'system'));
