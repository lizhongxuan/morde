-- +goose Up
ALTER TABLE task_batch_items
  ADD COLUMN file_delivery_intent text NOT NULL DEFAULT 'none',
  ADD CONSTRAINT task_batch_items_file_delivery_intent_check
    CHECK (file_delivery_intent IN ('none', 'required', 'unclear'));

ALTER TABLE task_clarification_requests
  ADD COLUMN batch_item_id text,
  ADD COLUMN batch_attempt integer,
  ADD CONSTRAINT task_clarification_batch_target_check CHECK (
    (batch_item_id IS NULL AND batch_attempt IS NULL) OR
    (batch_item_id IS NOT NULL AND batch_attempt IS NOT NULL AND batch_attempt > 0)
  ),
  ADD CONSTRAINT task_clarification_batch_item_task_fk
    FOREIGN KEY (task_id, batch_item_id)
    REFERENCES task_batch_items (task_id, id)
    ON DELETE CASCADE;

-- +goose Down
ALTER TABLE task_clarification_requests
  DROP CONSTRAINT task_clarification_batch_item_task_fk,
  DROP CONSTRAINT task_clarification_batch_target_check,
  DROP COLUMN batch_attempt,
  DROP COLUMN batch_item_id;

ALTER TABLE task_batch_items
  DROP CONSTRAINT task_batch_items_file_delivery_intent_check,
  DROP COLUMN file_delivery_intent;
