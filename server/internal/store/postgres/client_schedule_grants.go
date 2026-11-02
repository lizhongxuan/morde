package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) IssueClientScheduleGrant(ctx context.Context, principal identity.Principal, token identity.Token) error {
	if !validScheduleGrantPrincipal(principal) || token.Kind != identity.KindClient || token.OwnerID != principal.UserID ||
		token.ClientInstallationID != principal.ClientInstallationID || !identity.IsScheduleGrantScopeSet(token.Scopes) ||
		!domain.ValidTokenID(token.ID) || len(token.SecretHash) != 32 || !token.ExpiresAt.After(time.Now()) {
		return domain.InvalidArgument("schedule_grant", "schedule grant fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockClientScheduleGrantMutation(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, true); err != nil {
			return err
		}
		if err := validateScheduleGrantPrincipalInTx(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := createToken(ctx, sqlcgen.New(tx), token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE schedules s SET auth_token_id=$3
WHERE s.owner_id=$1 AND s.auth_kind='client' AND s.auth_client_installation_id=$2
  AND s.auth_scopes=ARRAY['schedule:read','schedule:manage']::text[]
  AND EXISTS (
    SELECT 1 FROM tokens previous
    WHERE previous.id=s.auth_token_id AND previous.owner_id=s.owner_id
      AND previous.client_installation_id=s.auth_client_installation_id AND previous.kind='client'
      AND previous.scopes=ARRAY['schedule:read','schedule:manage']::text[]
      AND previous.revoked_at IS NULL AND previous.expires_at>clock_timestamp()
  )`, principal.UserID, principal.ClientInstallationID, token.ID); err != nil {
			return fmt.Errorf("move schedules to renewed grant: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,clock_timestamp())
WHERE owner_id=$1 AND client_installation_id=$2 AND kind='client'
  AND scopes=ARRAY['schedule:read','schedule:manage']::text[] AND revoked_at IS NULL AND id<>$3`,
			principal.UserID, principal.ClientInstallationID, token.ID); err != nil {
			return fmt.Errorf("rotate client schedule grants: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,actor_kind,action,target)
VALUES($1,'client',$2,$3)`, principal.UserID, string(audit.ActionClientScheduleGrant), principal.ClientInstallationID); err != nil {
			return fmt.Errorf("audit client schedule grant: %w", err)
		}
		return nil
	})
}

func (s *Store) RevokeClientScheduleGrants(ctx context.Context, principal identity.Principal) error {
	if !validScheduleGrantPrincipal(principal) {
		return domain.NewError(domain.CodeForbidden, "regular client credential required")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockClientScheduleGrantMutation(ctx, tx, principal); err != nil {
			return err
		}
		if err := validateScheduleGrantPrincipalInTx(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,clock_timestamp())
WHERE owner_id=$1 AND client_installation_id=$2 AND kind='client'
  AND scopes=ARRAY['schedule:read','schedule:manage']::text[] AND revoked_at IS NULL`, principal.UserID, principal.ClientInstallationID); err != nil {
			return fmt.Errorf("revoke client schedule grants: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,actor_kind,action,target)
VALUES($1,'client',$2,$3)`, principal.UserID, string(audit.ActionClientScheduleGrant), principal.ClientInstallationID); err != nil {
			return fmt.Errorf("audit client schedule grant revocation: %w", err)
		}
		return nil
	})
}

func lockClientScheduleGrantMutation(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('client-schedule-grant:' || $1 || ':' || $2, 0))`,
		principal.UserID, principal.ClientInstallationID); err != nil {
		return fmt.Errorf("lock client schedule grant mutation: %w", err)
	}
	return nil
}

func validScheduleGrantPrincipal(principal identity.Principal) bool {
	return principal.Kind == identity.KindClient && domain.ValidUserID(principal.UserID) &&
		domain.ValidTokenID(principal.TokenID) && domain.ValidClientInstallationID(principal.ClientInstallationID) &&
		len(principal.Scopes) == 0 && len(principal.HostIDs) == 0 && len(principal.WorkspaceIDs) == 0
}

func validateScheduleGrantPrincipalInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT t.kind='client' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
  AND i.revoked_at IS NULL AND i.expires_at>clock_timestamp() AND i.platform='ios'
FROM tokens t JOIN client_installations i ON i.id=t.client_installation_id AND i.owner_id=t.owner_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.client_installation_id=$3 FOR SHARE OF t,i`,
		principal.TokenID, principal.UserID, principal.ClientInstallationID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return identity.ErrInvalidCredential
	}
	if err != nil {
		return fmt.Errorf("verify iOS client before schedule grant: %w", err)
	}
	var approved bool
	if err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, principal.UserID).Scan(&approved); errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return identity.ErrInvalidCredential
	} else if err != nil {
		return fmt.Errorf("verify schedule grant account: %w", err)
	}
	return nil
}

var _ app.ClientLoginStore = (*Store)(nil)
