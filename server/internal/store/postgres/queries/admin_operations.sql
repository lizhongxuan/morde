-- name: CreateUserInviteCode :one
INSERT INTO user_invite_codes (owner_id, code)
VALUES ($1, $2)
ON CONFLICT (owner_id) DO UPDATE SET owner_id=EXCLUDED.owner_id
RETURNING code;

-- name: AdminInviteStats :one
SELECT count(*)::bigint AS total, count(DISTINCT invite.inviter_id)::bigint AS total_inviters
FROM user_invites AS invite
JOIN users AS inviter ON inviter.id=invite.inviter_id
JOIN users AS invitee ON invitee.id=invite.invitee_id
WHERE sqlc.arg(search)::text = '' OR
  strpos(lower(inviter.id), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(inviter.name), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(invitee.id), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(invitee.name), lower(sqlc.arg(search)::text)) > 0;

-- name: ListAdminInvites :many
WITH inviter_counts AS (
  SELECT inviter_id, count(*)::bigint AS invitees FROM user_invites GROUP BY inviter_id
)
SELECT invite.inviter_id, inviter.name AS inviter_name, invite.invitee_id,
       invitee.name AS invitee_name, invitee.status AS invitee_status,
       invite.created_at, inviter_counts.invitees AS inviter_invitees
FROM user_invites AS invite
JOIN users AS inviter ON inviter.id=invite.inviter_id
JOIN users AS invitee ON invitee.id=invite.invitee_id
JOIN inviter_counts ON inviter_counts.inviter_id=invite.inviter_id
WHERE sqlc.arg(search)::text = '' OR
  strpos(lower(inviter.id), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(inviter.name), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(invitee.id), lower(sqlc.arg(search)::text)) > 0 OR
  strpos(lower(invitee.name), lower(sqlc.arg(search)::text)) > 0
ORDER BY invite.created_at DESC, invite.invitee_id
LIMIT sqlc.arg(page_limit)::integer OFFSET sqlc.arg(page_offset)::integer;

-- name: AdminSiteSummary :one
SELECT count(DISTINCT site.id)::bigint AS sites,
       count(DISTINCT site.id) FILTER (WHERE site.state='active')::bigint AS active_sites,
       count(release.id)::bigint AS releases,
       count(release.id) FILTER (WHERE release.state='healthy')::bigint AS healthy_releases
FROM sites AS site
LEFT JOIN site_releases AS release ON release.site_id=site.id;

-- name: ListAdminSites :many
SELECT site.id, site.owner_id, owner.name AS owner_name, site.host_id, site.state,
       site.created_at, count(release.id)::bigint AS release_count,
       count(release.id) FILTER (WHERE release.state='healthy')::bigint AS healthy_release_count,
       max(release.updated_at)::timestamptz AS latest_release_at
FROM sites AS site
JOIN users AS owner ON owner.id=site.owner_id
LEFT JOIN site_releases AS release ON release.site_id=site.id
GROUP BY site.id, owner.name
ORDER BY site.created_at DESC, site.id
LIMIT sqlc.arg(page_limit)::integer OFFSET sqlc.arg(page_offset)::integer;

-- name: CountAdminSites :one
SELECT count(*)::bigint FROM sites;

-- name: GetAdminNudgePolicy :one
SELECT enabled, url, text_zh, text_en, version, updated_at
FROM admin_nudge_policy WHERE id=1;
