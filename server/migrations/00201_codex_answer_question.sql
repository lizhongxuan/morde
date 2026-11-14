-- +goose Up
ALTER TABLE codex_commands
  DROP CONSTRAINT codex_commands_type_check,
  ADD CONSTRAINT codex_commands_type_check
    CHECK (type IN ('send', 'interrupt', 'approval', 'answer_question'));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM codex_commands WHERE type = 'answer_question') THEN
    RAISE EXCEPTION 'cannot remove persisted Codex question-answer commands';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE codex_commands
  DROP CONSTRAINT codex_commands_type_check,
  ADD CONSTRAINT codex_commands_type_check
    CHECK (type IN ('send', 'interrupt', 'approval'));
