-- +goose Up
-- Stop challenges are created at stop time, so existing lifecycles can be
-- safely upgraded without inventing a proof or retaining a long-lived secret.
ALTER TABLE task_sandbox_lifecycles
  ADD COLUMN guardian_nonce_sha256 text NOT NULL DEFAULT ''
  CHECK (guardian_nonce_sha256 = '' OR guardian_nonce_sha256 ~ '^[0-9a-f]{64}$');

-- +goose Down
-- A Guardian proof or a prepared reservation may depend on this nonce. Refuse
-- to remove the fence once any new lifecycle has stored one.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_sandbox_lifecycles WHERE guardian_nonce_sha256 <> '') THEN
    RAISE EXCEPTION 'cannot remove Task Sandbox Guardian nonce while nonce-bound lifecycle records exist';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE task_sandbox_lifecycles DROP COLUMN guardian_nonce_sha256;
