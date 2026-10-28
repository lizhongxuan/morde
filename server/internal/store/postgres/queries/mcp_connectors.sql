-- name: CreateMCPConnector :one
INSERT INTO mcp_connectors (
  id, owner_id, name, endpoint_url, enabled, credential_cipher, credential_version, revision
) VALUES (
  sqlc.arg(id), sqlc.arg(owner_id), sqlc.arg(name), sqlc.arg(endpoint_url),
  false, sqlc.arg(credential_cipher), sqlc.arg(credential_version), 1
)
RETURNING *;

-- name: ListMCPConnectors :many
SELECT * FROM mcp_connectors WHERE owner_id = sqlc.arg(owner_id)
ORDER BY lower(name), id;

-- name: GetMCPConnector :one
SELECT * FROM mcp_connectors WHERE owner_id = sqlc.arg(owner_id) AND id = sqlc.arg(id);

-- name: UpdateMCPConnector :one
UPDATE mcp_connectors SET
  name = sqlc.arg(name), endpoint_url = sqlc.arg(endpoint_url), enabled = sqlc.arg(enabled),
  credential_cipher = sqlc.arg(credential_cipher), credential_version = sqlc.arg(credential_version),
  revision = revision + 1, updated_at = clock_timestamp()
WHERE owner_id = sqlc.arg(owner_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision)
RETURNING *;

-- name: DeleteMCPConnector :one
DELETE FROM mcp_connectors
WHERE owner_id = sqlc.arg(owner_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision)
RETURNING id;
