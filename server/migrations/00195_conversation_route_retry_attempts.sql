-- +goose Up
-- Retriable text-only route classification keeps every unknown attempt as
-- billing evidence, while only a reserved/dispatched call occupies the live
-- execution slot for a message.
DROP INDEX conversation_route_model_calls_one_unresolved;
CREATE UNIQUE INDEX conversation_route_model_calls_one_unresolved
  ON conversation_route_model_calls(message_id) WHERE state IN ('reserved', 'dispatched');

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM conversation_route_model_calls
    WHERE state IN ('reserved', 'dispatched', 'unknown')
    GROUP BY message_id HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'cannot restore single unresolved route call while retry evidence coexists';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX conversation_route_model_calls_one_unresolved;
CREATE UNIQUE INDEX conversation_route_model_calls_one_unresolved
  ON conversation_route_model_calls(message_id) WHERE state IN ('reserved', 'dispatched', 'unknown');
