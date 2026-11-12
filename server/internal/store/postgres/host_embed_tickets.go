package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func (s *Store) CreateHostEmbedTicket(ctx context.Context, value app.HostEmbedTicketRecord) error {
	if !validHostEmbedTicketRecord(value) || !value.ExpiresAt.After(time.Now()) {
		return domain.InvalidArgument("ticket", "Host embed ticket is invalid")
	}
	targetGrants, err := json.Marshal(value.TargetGrants)
	if err != nil {
		return fmt.Errorf("encode Host embed target grants: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM host_embed_tickets WHERE expires_at <= clock_timestamp()`); err != nil {
		return fmt.Errorf("prune expired Host embed tickets: %w", err)
	}
	command, err := s.pool.Exec(ctx, `
INSERT INTO host_embed_tickets (
  ticket_hash, host_id, conversation_id, actor_user_id, portal_token_id,
  client_installation_id, service_instance_id, portal_origin, initial_target,
  target_grants, embed_nonce, runtime_generation, expires_at
)
SELECT $1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10::jsonb,$11,$12,$13
FROM tokens portal_token, hosts current_host
WHERE portal_token.id=$5 AND portal_token.owner_id=$4
  AND portal_token.kind IN ('session','client')
  AND portal_token.revoked_at IS NULL AND portal_token.expires_at > clock_timestamp()
  AND portal_token.client_installation_id IS NOT DISTINCT FROM NULLIF($6,'')
  AND current_host.id=$2 AND current_host.state='running' AND current_host.deleted_at IS NULL
  AND current_host.runtime_generation=$12
  AND NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements($10::jsonb) AS target_grant
    WHERE CASE
      WHEN target_grant->>'target'='browser' AND target_grant->>'permission_ceiling'='desktop:control' THEN
        current_host.owner_id<>$4 AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=current_host.id AND member.owner_id=current_host.owner_id AND member.user_id=$4 AND member.revoked_at IS NULL AND 'desktop:control'=ANY(member.permissions))
      WHEN target_grant->>'target'='browser' AND target_grant->>'permission_ceiling'='desktop:view' THEN
        current_host.owner_id<>$4 AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=current_host.id AND member.owner_id=current_host.owner_id AND member.user_id=$4 AND member.revoked_at IS NULL AND member.permissions && ARRAY['desktop:view','desktop:control']::text[])
      WHEN target_grant->>'target'='terminal' AND target_grant->>'permission_ceiling'='terminal:control' THEN
        current_host.owner_id<>$4 AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=current_host.id AND member.owner_id=current_host.owner_id AND member.user_id=$4 AND member.revoked_at IS NULL AND member.permissions @> ARRAY['terminal:control','desktop:control']::text[])
      WHEN target_grant->>'target'='workspace' AND target_grant->>'permission_ceiling'='workspace:file:read' THEN
        current_host.owner_id<>$4 AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=current_host.id AND member.owner_id=current_host.owner_id AND member.user_id=$4 AND member.revoked_at IS NULL AND 'workspace:file:read'=ANY(member.permissions))
      WHEN target_grant->>'target' IN ('files','vscode') AND target_grant->>'permission_ceiling'='desktop:control' THEN current_host.owner_id<>$4
      ELSE true
    END
  )
  AND ($6='' OR EXISTS (
    SELECT 1 FROM client_installations installation
    WHERE installation.id=$6 AND installation.owner_id=$4 AND installation.revoked_at IS NULL
      AND (installation.expires_at IS NULL OR installation.expires_at > clock_timestamp())
	))
`, value.TicketHash, value.HostID, value.ConversationID, value.ActorID, value.PortalTokenID,
		value.ClientInstallationID, value.ServiceInstanceID, value.PortalOrigin, value.InitialTarget,
		targetGrants, value.EmbedNonce, value.RuntimeGeneration, timestamp(value.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create Host embed ticket: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeUnauthenticated, "Portal credential is no longer active")
	}
	return nil
}

// ConsumeHostEmbedTicket marks a ticket used only while all immutable bindings,
// the current Portal credential, Host generation, and current target grant
// still match. The UPDATE predicate is the single-use concurrency boundary.
func (s *Store) ConsumeHostEmbedTicket(ctx context.Context, ticketHash []byte, hostID, serviceInstanceID, portalOrigin, embedNonce string) (app.HostEmbedTicketRecord, error) {
	if len(ticketHash) != 32 || !domain.ValidHostID(hostID) || !domain.ValidServiceInstanceID(serviceInstanceID) || !validHostEmbedOrigin(portalOrigin) {
		return app.HostEmbedTicketRecord{}, hostEmbedTicketNotFound(nil)
	}
	var result app.HostEmbedTicketRecord
	var installationID *string
	var usedAt time.Time
	err := s.pool.QueryRow(ctx, `
UPDATE host_embed_tickets ticket
SET used_at=clock_timestamp()
FROM hosts host, tokens portal_token
WHERE ticket.ticket_hash=$1 AND ticket.host_id=$2 AND ticket.service_instance_id=$3
  AND ticket.portal_origin=$4 AND ticket.used_at IS NULL AND ticket.expires_at > clock_timestamp()
  AND host.id=ticket.host_id AND host.state='running' AND host.deleted_at IS NULL
  AND host.runtime_generation=ticket.runtime_generation
  AND portal_token.id=ticket.portal_token_id AND portal_token.owner_id=ticket.actor_user_id
  AND portal_token.kind IN ('session','client') AND portal_token.revoked_at IS NULL
  AND portal_token.expires_at > clock_timestamp()
  AND portal_token.client_installation_id IS NOT DISTINCT FROM ticket.client_installation_id
  AND (ticket.client_installation_id IS NULL OR EXISTS (
    SELECT 1 FROM client_installations installation
    WHERE installation.id=ticket.client_installation_id AND installation.owner_id=ticket.actor_user_id
      AND installation.revoked_at IS NULL
      AND (installation.expires_at IS NULL OR installation.expires_at > clock_timestamp())
  ))
  AND ticket.embed_nonce=$5
  AND NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements(ticket.target_grants) AS target_grant
    WHERE CASE
      WHEN target_grant->>'target'='browser' AND target_grant->>'permission_ceiling'='desktop:control' THEN
        host.owner_id<>ticket.actor_user_id AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=host.id AND member.owner_id=host.owner_id AND member.user_id=ticket.actor_user_id AND member.revoked_at IS NULL AND 'desktop:control'=ANY(member.permissions))
      WHEN target_grant->>'target'='browser' AND target_grant->>'permission_ceiling'='desktop:view' THEN
        host.owner_id<>ticket.actor_user_id AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=host.id AND member.owner_id=host.owner_id AND member.user_id=ticket.actor_user_id AND member.revoked_at IS NULL AND member.permissions && ARRAY['desktop:view','desktop:control']::text[])
      WHEN target_grant->>'target'='terminal' AND target_grant->>'permission_ceiling'='terminal:control' THEN
        host.owner_id<>ticket.actor_user_id AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=host.id AND member.owner_id=host.owner_id AND member.user_id=ticket.actor_user_id AND member.revoked_at IS NULL AND member.permissions @> ARRAY['terminal:control','desktop:control']::text[])
      WHEN target_grant->>'target'='workspace' AND target_grant->>'permission_ceiling'='workspace:file:read' THEN
        host.owner_id<>ticket.actor_user_id AND NOT EXISTS (SELECT 1 FROM host_members member WHERE member.host_id=host.id AND member.owner_id=host.owner_id AND member.user_id=ticket.actor_user_id AND member.revoked_at IS NULL AND 'workspace:file:read'=ANY(member.permissions))
      WHEN target_grant->>'target' IN ('files','vscode') AND target_grant->>'permission_ceiling'='desktop:control' THEN host.owner_id<>ticket.actor_user_id
      ELSE true
    END
  )
RETURNING ticket.ticket_hash,ticket.host_id,ticket.conversation_id,ticket.actor_user_id,
  ticket.portal_token_id,ticket.client_installation_id,ticket.service_instance_id,ticket.portal_origin,
	ticket.initial_target,ticket.target_grants,ticket.embed_nonce,ticket.runtime_generation,ticket.expires_at,ticket.used_at`,
		ticketHash, hostID, serviceInstanceID, portalOrigin, embedNonce).Scan(
		&result.TicketHash, &result.HostID, &result.ConversationID, &result.ActorID,
		&result.PortalTokenID, &installationID, &result.ServiceInstanceID, &result.PortalOrigin,
		&result.InitialTarget, &result.TargetGrants, &result.EmbedNonce, &result.RuntimeGeneration, &result.ExpiresAt, &usedAt)
	if err != nil {
		return app.HostEmbedTicketRecord{}, hostEmbedTicketNotFound(err)
	}
	if installationID != nil {
		result.ClientInstallationID = *installationID
	}
	result.ExpiresAt = result.ExpiresAt.UTC()
	usedAt = usedAt.UTC()
	result.UsedAt = &usedAt
	return result, nil
}

func validHostEmbedTicketRecord(value app.HostEmbedTicketRecord) bool {
	if len(value.TicketHash) != 32 || !domain.ValidHostID(value.HostID) || !domain.ValidConversationID(value.ConversationID) ||
		!domain.ValidUserID(value.ActorID) || !domain.ValidTokenID(value.PortalTokenID) ||
		(value.ClientInstallationID != "" && !domain.ValidClientInstallationID(value.ClientInstallationID)) ||
		!domain.ValidServiceInstanceID(value.ServiceInstanceID) || !validHostEmbedOrigin(value.PortalOrigin) ||
		!validHostEmbedTarget(value.InitialTarget) || value.RuntimeGeneration < 1 || value.ExpiresAt.IsZero() || value.UsedAt != nil {
		return false
	}
	if len(value.TargetGrants) == 0 || len(value.TargetGrants) > 5 || !validEmbedNonce(value.EmbedNonce) {
		return false
	}
	seen := make(map[string]struct{}, len(value.TargetGrants))
	initialAllowed := false
	for _, grant := range value.TargetGrants {
		if _, exists := seen[grant.Target]; exists || !validHostEmbedTargetGrant(grant) {
			return false
		}
		seen[grant.Target] = struct{}{}
		initialAllowed = initialAllowed || grant.Target == value.InitialTarget
	}
	return initialAllowed
}

func validHostEmbedTargetGrant(value app.HostEmbedTargetGrant) bool {
	switch value.Target {
	case app.HostEmbedTargetBrowser:
		return value.PermissionCeiling == host.PermissionDesktopView || value.PermissionCeiling == host.PermissionDesktopControl
	case app.HostEmbedTargetTerminal:
		return value.PermissionCeiling == host.PermissionTerminalControl
	case app.HostEmbedTargetWorkspace:
		return value.PermissionCeiling == host.PermissionWorkspaceFileRead
	case app.HostEmbedTargetFiles, app.HostEmbedTargetVSCode:
		return value.PermissionCeiling == host.PermissionDesktopControl
	default:
		return false
	}
}

func validHostEmbedTarget(value string) bool {
	switch value {
	case app.HostEmbedTargetBrowser, app.HostEmbedTargetTerminal, app.HostEmbedTargetWorkspace, app.HostEmbedTargetFiles, app.HostEmbedTargetVSCode:
		return true
	default:
		return false
	}
}

func validEmbedNonce(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 24
}

func validHostEmbedOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Opaque == "" &&
		parsed.String() == value && !strings.Contains(parsed.Host, "@")
}

func hostEmbedTicketNotFound(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "Host embed ticket not found")
	}
	return fmt.Errorf("consume Host embed ticket: %w", err)
}
