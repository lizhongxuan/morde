-- +goose Up
ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_state_check,
  ADD COLUMN cancel_requested_at timestamptz,
  ADD CONSTRAINT conversation_message_routes_state_check
    CHECK (state IN ('pending','processing','routed','waiting_user','waiting_adjustment','failed','partial','cancelled')),
  ADD CONSTRAINT conversation_message_routes_cancel_long_form_check
    CHECK (cancel_requested_at IS NULL OR request_metadata->>'response_mode'='long_form'),
  ADD CONSTRAINT conversation_message_routes_cancel_state_check
    CHECK (cancel_requested_at IS NULL OR state IN ('processing','cancelled'));

CREATE INDEX conversation_message_routes_cancel_requested
  ON conversation_message_routes(cancel_requested_at,lease_expires_at)
  WHERE cancel_requested_at IS NOT NULL AND state='processing';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes
      WHERE request_metadata->>'response_mode'='long_form' OR state IN ('partial','cancelled') OR cancel_requested_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove persisted long-form conversation route state';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX conversation_message_routes_cancel_requested;
ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_cancel_state_check,
  DROP CONSTRAINT conversation_message_routes_cancel_long_form_check,
  DROP COLUMN cancel_requested_at,
  DROP CONSTRAINT conversation_message_routes_state_check,
  ADD CONSTRAINT conversation_message_routes_state_check
    CHECK (state IN ('pending','processing','routed','waiting_user','waiting_adjustment','failed'));
