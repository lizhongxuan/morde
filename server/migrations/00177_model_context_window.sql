-- +goose Up
ALTER TABLE model_connections
  ADD COLUMN context_window_tokens integer;

ALTER TABLE model_connections
  ADD CONSTRAINT model_connections_context_window_tokens_check
  CHECK (context_window_tokens IS NULL OR context_window_tokens BETWEEN 4096 AND 1000000);

-- +goose Down
ALTER TABLE model_connections
  DROP CONSTRAINT model_connections_context_window_tokens_check,
  DROP COLUMN context_window_tokens;
