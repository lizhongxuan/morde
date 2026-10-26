-- +goose Up
ALTER TABLE task_batch_items
  ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK (attempt > 0),
  ADD CONSTRAINT task_batch_items_task_id_id_key UNIQUE (task_id, id);

ALTER TABLE task_artifacts
  ADD COLUMN batch_item_id text,
  ADD COLUMN batch_attempt integer,
  ADD CONSTRAINT task_artifacts_batch_attempt_check CHECK (
    (batch_item_id IS NULL AND batch_attempt IS NULL) OR
    (batch_item_id IS NOT NULL AND batch_attempt IS NOT NULL AND batch_attempt > 0)
  ),
  ADD CONSTRAINT task_artifacts_batch_item_task_fk
    FOREIGN KEY (task_id, batch_item_id)
    REFERENCES task_batch_items (task_id, id)
    ON DELETE CASCADE;

CREATE UNIQUE INDEX task_artifacts_task_path_unique
  ON task_artifacts (task_id, path)
  WHERE batch_item_id IS NULL;

CREATE UNIQUE INDEX task_artifacts_batch_attempt_path_unique
  ON task_artifacts (task_id, batch_item_id, batch_attempt, path)
  WHERE batch_item_id IS NOT NULL;

-- +goose Down
DROP INDEX task_artifacts_batch_attempt_path_unique;
DROP INDEX task_artifacts_task_path_unique;

ALTER TABLE task_artifacts
  DROP CONSTRAINT task_artifacts_batch_item_task_fk,
  DROP CONSTRAINT task_artifacts_batch_attempt_check,
  DROP COLUMN batch_attempt,
  DROP COLUMN batch_item_id;

ALTER TABLE task_batch_items
  DROP CONSTRAINT task_batch_items_task_id_id_key,
  DROP COLUMN attempt;
