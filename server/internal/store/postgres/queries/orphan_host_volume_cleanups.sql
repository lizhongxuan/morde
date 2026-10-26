-- name: EnsureOrphanHostVolumeCleanup :one
INSERT INTO orphan_host_volume_cleanups (
  instance_id, host_id, runtime_profile, sandbox_id, runtime_generation
)
VALUES (sqlc.arg(instance_id), sqlc.arg(host_id), sqlc.arg(runtime_profile), sqlc.arg(sandbox_id), sqlc.arg(runtime_generation))
ON CONFLICT (instance_id, host_id, runtime_profile, sandbox_id, runtime_generation) DO UPDATE SET
  created_at = CASE WHEN sqlc.arg(active)::boolean THEN now() ELSE orphan_host_volume_cleanups.created_at END,
  completed_at = CASE WHEN sqlc.arg(active)::boolean THEN NULL ELSE orphan_host_volume_cleanups.completed_at END
RETURNING completed_at;

-- name: ListOrphanHostVolumeCleanups :many
SELECT DISTINCT host_id
FROM orphan_host_volume_cleanups
WHERE instance_id = sqlc.arg(instance_id) AND completed_at IS NULL
ORDER BY host_id;

-- name: CompleteOrphanHostVolumeCleanup :exec
UPDATE orphan_host_volume_cleanups AS cleanup
SET completed_at = now()
WHERE cleanup.instance_id = sqlc.arg(instance_id) AND cleanup.host_id = sqlc.arg(host_id) AND cleanup.completed_at IS NULL;

-- name: HasPendingOrphanHostVolumeCleanup :one
SELECT EXISTS (
  SELECT 1 FROM orphan_host_volume_cleanups WHERE host_id = sqlc.arg(host_id) AND completed_at IS NULL
);
