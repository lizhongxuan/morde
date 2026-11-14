-- +goose Up
ALTER TABLE codex_profile_connections
  ADD COLUMN session_discovery_state text CHECK (
    session_discovery_state IN ('syncing','synced','partial')
  ),
  ADD COLUMN session_discovery_error_code text CHECK (
    session_discovery_error_code IN ('profile_unavailable','session_list_failed','connection_lost','unknown')
  ),
  ADD COLUMN history_discovered_sessions bigint CHECK (history_discovered_sessions >= 0),
  ADD COLUMN history_complete_sessions bigint CHECK (history_complete_sessions >= 0),
  ADD COLUMN pending_upload_event_count bigint CHECK (pending_upload_event_count >= 0),
  ADD COLUMN session_discovery_updated_at timestamptz,
  ADD CONSTRAINT codex_profile_connections_session_discovery_status_consistent CHECK (
    (session_discovery_state IS NULL
      AND session_discovery_error_code IS NULL
      AND history_discovered_sessions IS NULL
      AND history_complete_sessions IS NULL
      AND pending_upload_event_count IS NULL
      AND session_discovery_updated_at IS NULL)
    OR
    (session_discovery_state IS NOT NULL
      AND history_discovered_sessions IS NOT NULL
      AND history_complete_sessions IS NOT NULL
      AND pending_upload_event_count IS NOT NULL
      AND session_discovery_updated_at IS NOT NULL
      AND history_complete_sessions <= history_discovered_sessions
      AND ((session_discovery_state = 'partial' AND session_discovery_error_code IS NOT NULL)
        OR (session_discovery_state IN ('syncing','synced') AND session_discovery_error_code IS NULL)))
  );

-- +goose Down
ALTER TABLE codex_profile_connections
  DROP CONSTRAINT codex_profile_connections_session_discovery_status_consistent,
  DROP COLUMN session_discovery_updated_at,
  DROP COLUMN pending_upload_event_count,
  DROP COLUMN history_complete_sessions,
  DROP COLUMN history_discovered_sessions,
  DROP COLUMN session_discovery_error_code,
  DROP COLUMN session_discovery_state;
