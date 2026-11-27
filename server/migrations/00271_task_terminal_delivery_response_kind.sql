-- Distinguish a model-generated explanation from a deterministic fallback so
-- a late model settlement cannot overwrite or invalidate a published fallback.
-- +goose Up
ALTER TABLE task_terminal_delivery_outbox
  ADD COLUMN response_kind text;

UPDATE task_terminal_delivery_outbox
SET response_kind='legacy'
WHERE response_ciphertext IS NOT NULL;

ALTER TABLE task_terminal_delivery_outbox
  ADD CONSTRAINT task_terminal_delivery_response_kind_check CHECK (
    (response_ciphertext IS NULL) = (response_kind IS NULL)
    AND (response_kind IS NULL OR response_kind IN ('model_response','system_state','legacy'))
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_terminal_delivery_outbox WHERE response_kind='system_state') THEN
    RAISE EXCEPTION 'cannot remove Task terminal delivery response kind while fallback replies exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_terminal_delivery_outbox
  DROP CONSTRAINT task_terminal_delivery_response_kind_check,
  DROP COLUMN response_kind;
