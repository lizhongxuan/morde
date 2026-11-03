-- +goose Up
ALTER TABLE conversation_messages
  ADD COLUMN delivery_status text NOT NULL DEFAULT 'applied';

ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_delivery_status_check
  CHECK (delivery_status IN ('pending', 'applied', 'unknown'));

CREATE INDEX conversation_messages_pending_delivery
  ON conversation_messages(task_id, sequence)
  WHERE delivery_status IN ('pending', 'unknown');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM conversation_messages WHERE delivery_status <> 'applied') THEN
    RAISE EXCEPTION 'cannot remove conversation message delivery state while pending or unknown messages exist';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX conversation_messages_pending_delivery;
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_messages_delivery_status_check;
ALTER TABLE conversation_messages DROP COLUMN delivery_status;
