-- +goose Up
ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_auto_retry_count_check,
  ADD CONSTRAINT conversation_message_routes_auto_retry_count_check
    CHECK (auto_retry_count BETWEEN 0 AND 16);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes WHERE auto_retry_count > 3) THEN
    RAISE EXCEPTION 'cannot lower conversation route retry limit while retry evidence exceeds 3';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_auto_retry_count_check,
  ADD CONSTRAINT conversation_message_routes_auto_retry_count_check
    CHECK (auto_retry_count BETWEEN 0 AND 3);
