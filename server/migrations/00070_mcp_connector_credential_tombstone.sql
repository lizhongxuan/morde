-- +goose Up
ALTER TABLE mcp_connectors
  DROP CONSTRAINT mcp_connectors_check,
  ADD CONSTRAINT mcp_connectors_credential_state_check CHECK (
    credential_cipher IS NULL OR
    (octet_length(credential_cipher) BETWEEN 29 AND 32796 AND credential_version > 0)
  );

-- +goose Down
UPDATE mcp_connectors
SET credential_version = 0
WHERE credential_cipher IS NULL AND credential_version > 0;

ALTER TABLE mcp_connectors
  DROP CONSTRAINT mcp_connectors_credential_state_check,
  ADD CONSTRAINT mcp_connectors_check CHECK (
    (credential_cipher IS NULL AND credential_version = 0) OR
    (credential_cipher IS NOT NULL AND octet_length(credential_cipher) BETWEEN 29 AND 32796 AND credential_version > 0)
  );
