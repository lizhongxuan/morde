-- +goose Up
ALTER TABLE task_step_retry_intents
  DROP CONSTRAINT task_step_retry_intents_host_runtime_generation_check,
  ADD CONSTRAINT task_step_retry_intents_host_runtime_generation_check
    CHECK (host_runtime_generation >= 0),
  ADD CONSTRAINT task_step_retry_intents_target_check CHECK (
    (host_id = '' AND host_runtime_generation = 0 AND workspace_id IS NULL AND input_revision IS NULL AND
     workspace_checkpoint_revision IS NULL AND workspace_binding_version = 0 AND workspace_revision IS NULL) OR
    (host_id <> '' AND host_runtime_generation > 0)
  );

-- +goose Down
ALTER TABLE task_step_retry_intents
  DROP CONSTRAINT task_step_retry_intents_target_check,
  DROP CONSTRAINT task_step_retry_intents_host_runtime_generation_check,
  ADD CONSTRAINT task_step_retry_intents_host_runtime_generation_check
    CHECK (host_runtime_generation > 0);
