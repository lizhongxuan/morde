-- Persist only bounded readback metadata for Task artifacts; file contents stay
-- in the immutable Workspace revision and are never copied into the verdict row.
-- +goose Up
ALTER TABLE task_validation_reviews
  ADD COLUMN artifact_content_evidence jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(artifact_content_evidence) = 'array'),
  ADD CONSTRAINT task_validation_reviews_artifact_evidence_state_check
    CHECK (state = 'reviewed' OR artifact_content_evidence = '[]'::jsonb);

-- +goose StatementBegin
CREATE FUNCTION task_validation_artifact_evidence_current(p_task_id text, p_owner_id text, p_evidence jsonb)
RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT COALESCE(jsonb_typeof(p_evidence)='array',false)
    AND NOT EXISTS (
      SELECT 1 FROM jsonb_array_elements(
        CASE WHEN jsonb_typeof(p_evidence)='array' THEN p_evidence ELSE '[]'::jsonb END
      ) proof
      WHERE proof->>'content_complete' IS DISTINCT FROM 'true'
         OR proof->>'text_content' IS DISTINCT FROM 'true'
         OR NOT EXISTS (
           SELECT 1 FROM task_artifacts artifact
           JOIN task_steps artifact_step ON artifact_step.task_id=artifact.task_id AND artifact_step.id=artifact.step_id
           JOIN tasks task ON task.id=artifact.task_id AND task.owner_id=p_owner_id AND task.workspace_id=artifact.workspace_id
           JOIN workspaces workspace ON workspace.id=artifact.workspace_id
             AND workspace.owner_id=COALESCE(task.workspace_owner_id,task.owner_id)
             AND workspace.state='ready' AND workspace.deleted_at IS NULL
           JOIN workspace_revision_files current_file ON current_file.workspace_id=artifact.workspace_id
             AND current_file.file_path=artifact.path AND current_file.valid_to_revision IS NULL
           WHERE artifact.id=proof->'ref'->>'id' AND artifact.task_id=p_task_id
             AND artifact.workspace_id=proof->>'workspace_id'
             AND artifact.workspace_revision::text=proof->>'revision'
             AND artifact.path=proof->>'path' AND artifact.media_type=proof->>'media_type'
             AND artifact.size_bytes::text=proof->>'size_bytes' AND artifact.sha256=proof->>'sha256'
             AND artifact_step.sequence::text=proof->'ref'->>'sequence'
             AND artifact_step.task_revision::text=proof->'ref'->>'task_revision'
             AND artifact_step.status='succeeded'
             AND COALESCE(artifact_step.tool_call_key,'')=proof->'ref'->>'call_id'
             AND current_file.size_bytes::text=proof->>'size_bytes' AND current_file.sha256=proof->>'sha256'
             AND (SELECT count(*) FROM task_steps source_step WHERE source_step.task_id=artifact.task_id
               AND source_step.task_revision::text=proof->'ref'->>'task_revision'
               AND source_step.tool_call_key=proof->'ref'->>'call_id' AND source_step.status='succeeded')=1
         )
    )
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM task_validation_reviews
    WHERE state='reviewed' AND artifact_content_evidence <> '[]'::jsonb
  ) THEN
    RAISE EXCEPTION 'cannot remove persisted Task artifact validation evidence';
  END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION task_validation_artifact_evidence_current(text, text, jsonb);
-- +goose StatementEnd
ALTER TABLE task_validation_reviews
  DROP CONSTRAINT task_validation_reviews_artifact_evidence_state_check,
  DROP COLUMN artifact_content_evidence;
