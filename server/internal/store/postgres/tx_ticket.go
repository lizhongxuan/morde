package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ConsumeTicket is a single atomic conditional update: exactly one caller can
// set used_at while the ticket is live, unrevoked, and still unused.
func (s *Store) ConsumeTicket(ctx context.Context, tokenID string) (app.Ticket, error) {
	var result app.Ticket
	var hostID, actorUserID, clientInstallationID pgtype.Text
	err := s.pool.QueryRow(ctx, `
UPDATE tokens
SET used_at = now(), last_used_at = now()
WHERE id = $1 AND kind = 'ticket' AND used_at IS NULL
  AND host_id IS NOT NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING id, owner_id, COALESCE(actor_user_id, owner_id), host_id, COALESCE(preview_port, 0), client_installation_id`, tokenID).Scan(&result.TokenID, &result.UserID, &actorUserID, &hostID, &result.PreviewPort, &clientInstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Ticket{}, identity.ErrInvalidCredential
	}
	if err != nil {
		return app.Ticket{}, fmt.Errorf("consume ticket: %w", err)
	}
	if !hostID.Valid || hostID.String == "" {
		return app.Ticket{}, identity.ErrInvalidCredential
	}
	result.HostID = hostID.String
	result.ActorUserID = actorUserID.String
	result.ClientInstallationID = clientInstallationID.String
	return result, nil
}
