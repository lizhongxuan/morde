-- +goose Up
ALTER TABLE instant_chat_exchanges
  ADD COLUMN requested_conversation_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE instant_chat_exchanges
  DROP COLUMN requested_conversation_id;
