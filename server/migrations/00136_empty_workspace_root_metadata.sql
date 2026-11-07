-- +goose Up
-- Only a never-materialized, verifiably empty initial revision has a known empty root.
INSERT INTO workspace_revision_directories
  (workspace_id,valid_from_revision,directory_path,folded_path,metadata_known)
SELECT w.id,0,'.','.',true
FROM workspaces w
JOIN workspace_revisions r ON r.workspace_id=w.id AND r.revision=0
JOIN workspace_current_materializations m ON m.workspace_id=w.id
JOIN workspace_volumes v ON v.workspace_id=w.id AND v.state='pending'
WHERE w.state='ready' AND w.deleted_at IS NULL AND w.revision=0 AND w.used_bytes=0
  AND r.file_count=0 AND r.directory_count=0 AND r.total_bytes=0
  AND m.desired_revision=0 AND m.applied_revision=-1
  AND NOT EXISTS (SELECT 1 FROM workspace_legacy_volumes l WHERE l.workspace_id=w.id AND l.import_state='pending')
  AND NOT EXISTS (SELECT 1 FROM workspace_revision_directories d WHERE d.workspace_id=w.id)
  AND NOT EXISTS (SELECT 1 FROM workspace_revision_files f WHERE f.workspace_id=w.id)
  AND NOT EXISTS (SELECT 1 FROM workspace_revision_empty_directories e WHERE e.workspace_id=w.id);

-- +goose Down
-- Keep the initialized metadata; dropping it would make the empty revision unrecoverable.
SELECT 1;
