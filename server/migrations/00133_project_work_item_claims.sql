-- +goose Up
ALTER TABLE project_work_items
  ALTER COLUMN assignee_id DROP NOT NULL,
  ADD COLUMN category text NOT NULL DEFAULT 'unclassified'
    CHECK (category IN ('bug', 'request', 'unclassified'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM project_work_items WHERE assignee_id IS NULL) THEN
    RAISE EXCEPTION 'cannot roll back unassigned project work items';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_work_items
  DROP COLUMN category,
  ALTER COLUMN assignee_id SET NOT NULL;
