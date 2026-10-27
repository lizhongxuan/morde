-- +goose Up
-- Old scoped keys must not silently acquire account-wide permissions.
UPDATE tokens SET revoked_at = now()
WHERE kind = 'api_key' AND revoked_at IS NULL;

ALTER TABLE tokens ADD CONSTRAINT tokens_api_key_account_permissions CHECK (
  kind <> 'api_key' OR revoked_at IS NOT NULL OR
  (cardinality(host_ids) = 0 AND cardinality(scopes) = 0)
);

-- +goose Down
ALTER TABLE tokens DROP CONSTRAINT tokens_api_key_account_permissions;
