-- name: RenameClientInstallation :execrows
UPDATE client_installations
SET display_name = sqlc.arg(display_name)
WHERE id = sqlc.arg(installation_id)
  AND owner_id = sqlc.arg(owner_id)
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > clock_timestamp());
