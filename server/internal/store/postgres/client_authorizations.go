package postgres

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) CreateClientAuthorization(ctx context.Context, value identity.ClientAuthorization) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO client_authorizations
  (id, display_name, platform, wants_workspace_sync, renewal_installation_id, secret_hash, user_code_hash, state, expires_at)
  VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',$8)`,
		value.ID, value.DisplayName, value.Platform, value.WantsWorkspaceSync,
		nullableText(value.RenewalInstallationID), value.SecretHash, value.UserCodeHash, value.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create client authorization: %w", err)
	}
	return nil
}

func (s *Store) GetClientAuthorization(ctx context.Context, id string) (identity.ClientAuthorization, error) {
	return scanClientAuthorization(s.pool.QueryRow(ctx, `SELECT id,display_name,platform,wants_workspace_sync,
  renewal_installation_id,secret_hash,user_code_hash,state,installation_id,owner_id,workspace_ids,expires_at,created_at
  FROM client_authorizations WHERE id=$1`, id))
}

func (s *Store) GetClientAuthorizationByCode(ctx context.Context, codeHash []byte) (identity.ClientAuthorization, error) {
	return scanClientAuthorization(s.pool.QueryRow(ctx, `SELECT id,display_name,platform,wants_workspace_sync,
  renewal_installation_id,secret_hash,user_code_hash,state,installation_id,owner_id,workspace_ids,expires_at,created_at
  FROM client_authorizations WHERE user_code_hash=$1`, codeHash))
}

type scanner interface{ Scan(...any) error }

func scanClientAuthorization(row scanner) (identity.ClientAuthorization, error) {
	var result identity.ClientAuthorization
	var renewal, installation, owner pgtype.Text
	err := row.Scan(&result.ID, &result.DisplayName, &result.Platform, &result.WantsWorkspaceSync,
		&renewal, &result.SecretHash, &result.UserCodeHash, &result.State, &installation, &owner,
		&result.WorkspaceIDs, &result.ExpiresAt, &result.CreatedAt)
	if err != nil {
		return identity.ClientAuthorization{}, storeError("get client authorization", "client authorization", err)
	}
	result.RenewalInstallationID = renewal.String
	result.InstallationID = installation.String
	result.OwnerID = owner.String
	return result, nil
}

func (s *Store) ApproveClientAuthorization(ctx context.Context, codeHash []byte, ownerID string, workspaceIDs []string, newID, ip string, expiresAt time.Time) (identity.ClientInstallation, error) {
	var result identity.ClientInstallation
	// Pairing, installation and the approval decision must commit together.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		pairing, err := scanClientAuthorization(tx.QueryRow(ctx, `SELECT id,display_name,platform,wants_workspace_sync,
  renewal_installation_id,secret_hash,user_code_hash,state,installation_id,owner_id,workspace_ids,expires_at,created_at
  FROM client_authorizations WHERE user_code_hash=$1 FOR UPDATE`, codeHash))
		if err != nil {
			return err
		}
		if pairing.State != "pending" || !pairing.ExpiresAt.After(time.Now()) || !expiresAt.After(time.Now()) || (!pairing.WantsWorkspaceSync && len(workspaceIDs) != 0) {
			return domain.NewError(domain.CodeConflict, "client authorization is no longer pending")
		}
		installationID := newID
		if pairing.RenewalInstallationID != "" {
			installationID = pairing.RenewalInstallationID
			result, err = scanClientInstallation(tx.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,workspace_ids,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1 FOR UPDATE`, installationID))
			if err != nil {
				return err
			}
			if result.OwnerID != ownerID || result.Platform != pairing.Platform || result.RevokedAt != nil {
				return domain.NewError(domain.CodeNotFound, "client installation not found")
			}
		} else {
			if !domain.ValidClientInstallationID(newID) {
				return domain.InvalidArgument("installation_id", "invalid installation id")
			}
			result, err = scanClientInstallation(tx.QueryRow(ctx, `INSERT INTO client_installations
  (id,owner_id,display_name,platform,workspace_ids,expires_at) VALUES ($1,$2,$3,$4,$5,$6)
  RETURNING id,owner_id,display_name,platform,workspace_ids,expires_at,created_at,last_used_at,revoked_at`,
				newID, ownerID, pairing.DisplayName, pairing.Platform, workspaceIDs, expiresAt))
			if err != nil {
				return err
			}
		}
		command, err := tx.Exec(ctx, `UPDATE client_authorizations SET state='approved',installation_id=$2,owner_id=$3,workspace_ids=$4
  WHERE id=$1 AND state='pending'`, pairing.ID, installationID, ownerID, workspaceIDs)
		if err != nil {
			return fmt.Errorf("approve client authorization: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "client authorization is no longer pending")
		}
		if pairing.RenewalInstallationID != "" {
			result, err = scanClientInstallation(tx.QueryRow(ctx, `UPDATE client_installations
  SET display_name=$2,workspace_ids=$3,expires_at=$4 WHERE id=$1
  RETURNING id,owner_id,display_name,platform,workspace_ids,expires_at,created_at,last_used_at,revoked_at`,
				installationID, pairing.DisplayName, workspaceIDs, expiresAt))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id=$1 AND revoked_at IS NULL`, installationID); err != nil {
				return fmt.Errorf("revoke client credentials after renewal approval: %w", err)
			}
		}
		detail, err := json.Marshal(map[string]any{"platform": pairing.Platform, "workspace_count": len(workspaceIDs), "renewal": pairing.RenewalInstallationID != ""})
		if err != nil {
			return fmt.Errorf("encode client audit detail: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,detail,ip)
  VALUES ($1,'session','client.pair',$2,$3,$4)`, ownerID, installationID, detail, ip); err != nil {
			return fmt.Errorf("audit client approval: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) DenyClientAuthorization(ctx context.Context, codeHash []byte, actorID, ip string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var pairingID string
		err := tx.QueryRow(ctx, `UPDATE client_authorizations SET state='denied'
  WHERE user_code_hash=$1 AND state='pending' AND expires_at>now() RETURNING id`, codeHash).Scan(&pairingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "client authorization is no longer pending")
		}
		if err != nil {
			return fmt.Errorf("deny client authorization: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,ip)
  VALUES ($1,'session','client.deny',$2,$3)`, actorID, pairingID, ip); err != nil {
			return fmt.Errorf("audit client denial: %w", err)
		}
		return nil
	})
}

func (s *Store) ClaimClientAuthorization(ctx context.Context, id string, secretHash []byte, client identity.Token, syncToken *identity.Token) (identity.ClientInstallation, error) {
	var result identity.ClientInstallation
	// Claim atomically rotates both credentials; a repeated poll cannot retrieve a secret.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		pairing, err := scanClientAuthorization(tx.QueryRow(ctx, `SELECT id,display_name,platform,wants_workspace_sync,
  renewal_installation_id,secret_hash,user_code_hash,state,installation_id,owner_id,workspace_ids,expires_at,created_at
  FROM client_authorizations WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if pairing.State != "approved" || !pairing.ExpiresAt.After(time.Now()) || len(secretHash) != len(pairing.SecretHash) || subtle.ConstantTimeCompare(secretHash, pairing.SecretHash) != 1 {
			return identity.ErrInvalidCredential
		}
		if client.Kind != identity.KindClient || client.OwnerID != pairing.OwnerID || client.ClientInstallationID != pairing.InstallationID ||
			((syncToken == nil) != !pairing.WantsWorkspaceSync) {
			return domain.InvalidArgument("credential", "client credential is invalid")
		}
		if syncToken != nil && (syncToken.Kind != identity.KindDevice || syncToken.OwnerID != pairing.OwnerID ||
			syncToken.ClientInstallationID != pairing.InstallationID || !syncToken.ExpiresAt.Equal(client.ExpiresAt) ||
			!equalStringSlices(syncToken.WorkspaceIDs, pairing.WorkspaceIDs)) {
			return domain.InvalidArgument("credential", "sync credential is invalid")
		}
		var approved bool
		if err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1`, pairing.OwnerID).Scan(&approved); err != nil {
			return fmt.Errorf("check client owner: %w", err)
		}
		if !approved {
			return identity.ErrInvalidCredential
		}
		result, err = scanClientInstallation(tx.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,workspace_ids,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1 FOR UPDATE`, pairing.InstallationID))
		if err != nil {
			return err
		}
		if result.OwnerID != pairing.OwnerID || result.RevokedAt != nil || !result.ExpiresAt.After(time.Now()) ||
			!result.ExpiresAt.Equal(client.ExpiresAt) || !equalStringSlices(result.WorkspaceIDs, pairing.WorkspaceIDs) || result.DisplayName != pairing.DisplayName {
			return identity.ErrInvalidCredential
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id=$1 AND revoked_at IS NULL`, result.ID); err != nil {
			return fmt.Errorf("rotate client credentials: %w", err)
		}
		q := sqlcgen.New(tx)
		if _, err := createToken(ctx, q, client); err != nil {
			return err
		}
		if syncToken != nil {
			if _, err := createToken(ctx, q, *syncToken); err != nil {
				return err
			}
		}
		command, err := tx.Exec(ctx, `UPDATE client_authorizations SET state='claimed',claimed_at=now() WHERE id=$1 AND state='approved'`, pairing.ID)
		if err != nil {
			return fmt.Errorf("claim client authorization: %w", err)
		}
		if command.RowsAffected() != 1 {
			return identity.ErrInvalidCredential
		}
		return nil
	})
	return result, err
}

func scanClientInstallation(row scanner) (identity.ClientInstallation, error) {
	var result identity.ClientInstallation
	var lastUsed, revoked pgtype.Timestamptz
	err := row.Scan(&result.ID, &result.OwnerID, &result.DisplayName, &result.Platform,
		&result.WorkspaceIDs, &result.ExpiresAt, &result.CreatedAt, &lastUsed, &revoked)
	if err != nil {
		return identity.ClientInstallation{}, storeError("get client installation", "client installation", err)
	}
	if lastUsed.Valid {
		used := lastUsed.Time
		result.LastUsedAt = &used
	}
	if revoked.Valid {
		at := revoked.Time
		result.RevokedAt = &at
	}
	return result, nil
}

func (s *Store) ListClientInstallations(ctx context.Context, ownerID string) ([]identity.ClientInstallation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,owner_id,display_name,platform,workspace_ids,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at
  FROM client_installations WHERE owner_id=$1 ORDER BY created_at DESC,id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list client installations: %w", err)
	}
	defer rows.Close()
	items := make([]identity.ClientInstallation, 0)
	for rows.Next() {
		value, err := scanClientInstallation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list client installations: %w", err)
	}
	return items, nil
}

func (s *Store) GetClientInstallation(ctx context.Context, id string) (identity.ClientInstallation, error) {
	return scanClientInstallation(s.pool.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,workspace_ids,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1`, id))
}

func (s *Store) RevokeClientInstallation(ctx context.Context, id, ownerID, ip string) error {
	// Installation, both token kinds and the audit entry commit together.
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE client_installations SET revoked_at=COALESCE(revoked_at,now())
  WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL`, id, ownerID)
		if err != nil {
			return fmt.Errorf("revoke client installation: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeNotFound, "client installation not found")
		}
		_, err = tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id=$1 AND revoked_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("revoke client credentials: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,ip)
  VALUES ($1,'session','client.revoke',$2,$3)`, ownerID, id, ip); err != nil {
			return fmt.Errorf("audit client revocation: %w", err)
		}
		return nil
	})
}

func (s *Store) ActiveClientInstallation(ctx context.Context, id, userID string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())
  FROM client_installations WHERE id=$1 AND owner_id=$2`, id, userID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check client installation: %w", err)
	}
	return active, nil
}
