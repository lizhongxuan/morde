-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1 AND owner_id = $2;

-- name: ListConversations :many
SELECT * FROM conversations
WHERE owner_id = $1 AND project_id IS NULL
  AND ($2::text[] IS NULL OR host_id = ANY($2::text[]))
  AND ($3::text = '' OR strpos(lower(title), lower($3)) > 0)
  AND ($4::timestamptz IS NULL OR updated_at < $4::timestamptz OR (updated_at = $4::timestamptz AND id < $5))
ORDER BY updated_at DESC, id DESC
LIMIT $6;

-- name: GetMessage :one
SELECT * FROM conversation_messages WHERE id = $1 AND owner_id = $2;

-- name: ListMessages :many
SELECT * FROM conversation_messages
WHERE owner_id = $1 AND conversation_id = $2
  AND ($3::bigint = 0 OR sequence < $3::bigint)
ORDER BY sequence DESC
LIMIT $4;
