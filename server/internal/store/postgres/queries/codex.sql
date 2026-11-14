-- name: CodexListSessions :many
SELECT s.id, s.owner_id, s.installation_id, s.profile_key, s.thread_id, s.title, s.cwd_label,
       s.provider_updated_at, s.status, g.scopes, g.grant_version, s.last_sequence, s.created_at, s.updated_at
FROM codex_sessions AS s
JOIN codex_session_grants AS g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations AS i ON i.id=s.installation_id AND i.owner_id=s.owner_id
WHERE s.owner_id=$1 AND s.installation_id=$2 AND s.id>$3
  AND g.revoked_at IS NULL AND 'read'=ANY(g.scopes)
  AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp())
ORDER BY s.id
LIMIT $4;

-- name: CodexListEvents :many
SELECT e.session_id, e.sequence, e.provider_event_id, e.kind, e.occurred_at,
       e.turn_id, e.item_id, e.parent_item_id, e.status, e.payload, e.stored_at
FROM codex_events AS e
JOIN codex_sessions AS s ON s.id=e.session_id AND s.owner_id=e.owner_id AND s.installation_id=e.installation_id
JOIN codex_session_grants AS g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations AS i ON i.id=s.installation_id AND i.owner_id=s.owner_id
WHERE e.owner_id=$1 AND e.session_id=$2 AND g.scopes @> $3::text[] AND e.sequence>$4
  AND g.revoked_at IS NULL
  AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp())
ORDER BY e.sequence
LIMIT $5;

-- name: CodexGetSessionForRead :one
SELECT s.id, s.owner_id, s.installation_id, s.profile_key, s.thread_id, s.title, s.cwd_label,
       s.provider_updated_at, s.status, g.scopes, g.grant_version, s.last_sequence, s.created_at, s.updated_at
FROM codex_sessions AS s
JOIN codex_session_grants AS g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations AS i ON i.id=s.installation_id AND i.owner_id=s.owner_id
WHERE s.owner_id=$1 AND s.id=$2 AND g.revoked_at IS NULL AND g.scopes @> $3::text[]
  AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp());
