-- +goose Up
-- Retire credentials from the removed standalone device pairing flow.
UPDATE tokens SET revoked_at = now()
WHERE kind = 'device' AND client_installation_id IS NULL AND revoked_at IS NULL;
ALTER TABLE tokens ADD CONSTRAINT tokens_device_requires_client_installation
  CHECK (kind <> 'device' OR client_installation_id IS NOT NULL OR revoked_at IS NOT NULL);
-- An authorization approved before this migration may not yet have been claimed.
UPDATE client_installations AS installation
SET display_name = pending.display_name,
    workspace_ids = pending.workspace_ids,
    expires_at = now() + interval '30 days'
FROM client_authorizations AS pending
WHERE pending.installation_id = installation.id
  AND pending.state = 'approved'
  AND installation.expires_at IS NULL;

-- +goose Down
ALTER TABLE tokens DROP CONSTRAINT tokens_device_requires_client_installation;
