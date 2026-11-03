-- Read-only preflight before a later strict one-unfinished-Task-per-conversation index.
-- Keep every returned Task unchanged; have the owner resolve ambiguity before the index release.
SELECT conversation_id,
       owner_id,
       array_agg(id ORDER BY created_at, id) AS task_ids,
       array_agg(status ORDER BY created_at, id) AS statuses,
       count(*) AS unfinished_count
FROM tasks
WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling')
GROUP BY conversation_id, owner_id
HAVING count(*) > 1
ORDER BY owner_id, conversation_id;
