package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) LoginClient(ctx context.Context, requested identity.ClientInstallation, instanceID string, generation int64, legacyAndroid bool, keyHash, requestHash []byte, renewalID, ip string,
	issue func(string) (app.ClientIssuedTokens, error)) (identity.ClientInstallation, app.ClientIssuedTokens, error) {
	var installation identity.ClientInstallation
	var issued app.ClientIssuedTokens
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		lockHash := sha256.Sum256(append([]byte(requested.OwnerID+"\x00key:"), keyHash...))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(lockHash[:8]))); err != nil {
			return fmt.Errorf("lock client login key: %w", err)
		}
		if legacyAndroid {
			legacyHash := sha256.Sum256([]byte(requested.OwnerID + "\x00legacy-android"))
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(legacyHash[:8]))); err != nil {
				return fmt.Errorf("lock legacy Android login: %w", err)
			}
		}
		var approved bool
		if err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, requested.OwnerID).Scan(&approved); err != nil || !approved {
			return identity.ErrInvalidCredential
		}
		var savedHash []byte
		var savedInstallationID string
		err := tx.QueryRow(ctx, `SELECT request_hash,installation_id FROM client_login_requests
			WHERE owner_id=$1 AND key_hash=$2 FOR UPDATE`, requested.OwnerID, keyHash).Scan(&savedHash, &savedInstallationID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find client login attempt: %w", err)
		}
		replay := err == nil
		if err == nil {
			if !bytes.Equal(savedHash, requestHash) {
				return domain.NewError(domain.CodeConflict, "idempotency key was used for another login request")
			}
			if !legacyAndroid {
				return domain.NewError(domain.CodeConflict, "login attempt was already processed; retry with a higher attempt_generation and a new Idempotency-Key")
			}
			// The pre-protocol Android client retries the same idempotency key after an
			// unknown response. Preserve its historical rotation behavior on the same
			// installation while keeping this path isolated from ordered clients.
			renewalID = savedInstallationID
		}
		if legacyAndroid {
			seedID := renewalID
			if seedID == "" {
				seedID = requested.ID
			}
			instanceID = legacyAndroidInstanceID(requested.OwnerID, seedID)
		}
		streamHash := sha256.Sum256([]byte(requested.OwnerID + "\x00instance:" + instanceID))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(streamHash[:8]))); err != nil {
			return fmt.Errorf("lock client login stream: %w", err)
		}

		var currentGeneration int64
		var existingID string
		err = tx.QueryRow(ctx, `SELECT generation,installation_id FROM client_login_attempts
  WHERE owner_id=$1 AND client_instance_id=$2 FOR UPDATE`, requested.OwnerID, instanceID).Scan(&currentGeneration, &existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find client login generation: %w", err)
		}
		hasStream := err == nil
		if legacyAndroid {
			generation = 1
			if hasStream {
				if renewalID != "" && renewalID != existingID {
					return domain.NewError(domain.CodeConflict, "legacy Android installation is bound to another login stream")
				}
				generation = currentGeneration + 1
			}
		}
		if hasStream {
			if !legacyAndroid && generation <= currentGeneration {
				return domain.NewError(domain.CodeConflict, "client login attempt is stale; use a higher attempt_generation")
			}
			if renewalID != "" && renewalID != existingID {
				return domain.NewError(domain.CodeConflict, "client instance is bound to another installation")
			}
			installation, err = scanClientInstallation(tx.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1 FOR UPDATE`, existingID))
			if err != nil {
				return err
			}
			if installation.OwnerID != requested.OwnerID || installation.Platform != requested.Platform {
				return domain.NewError(domain.CodeNotFound, "client installation not found")
			}
			if installation.RevokedAt != nil {
				return identity.ErrInvalidCredential
			}
		} else if renewalID != "" {
			installation, err = scanClientInstallation(tx.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1 FOR UPDATE`, renewalID))
			if err != nil {
				return err
			}
			if installation.OwnerID != requested.OwnerID || installation.Platform != requested.Platform {
				return domain.NewError(domain.CodeNotFound, "client installation not found")
			}
			if installation.RevokedAt != nil {
				return identity.ErrInvalidCredential
			}
			var bound bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM client_login_attempts WHERE installation_id=$1)`, renewalID).Scan(&bound); err != nil {
				return fmt.Errorf("check client login stream binding: %w", err)
			}
			if bound {
				return domain.NewError(domain.CodeConflict, "client installation belongs to another ordered login stream")
			}
		} else {
			installation = requested
			_, err = tx.Exec(ctx, `INSERT INTO client_installations
  (id,owner_id,display_name,platform,expires_at)
  VALUES ($1,$2,$3,$4,$5)`, installation.ID, installation.OwnerID,
				installation.DisplayName, installation.Platform, requested.ExpiresAt)
			if err != nil {
				return fmt.Errorf("create client installation: %w", err)
			}
		}
		if hasStream {
			if _, err := tx.Exec(ctx, `UPDATE client_login_attempts SET generation=$3,updated_at=now()
  WHERE owner_id=$1 AND client_instance_id=$2`, requested.OwnerID, instanceID, generation); err != nil {
				return fmt.Errorf("advance client login generation: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `INSERT INTO client_login_attempts(owner_id,client_instance_id,generation,installation_id)
  VALUES ($1,$2,$3,$4)`, requested.OwnerID, instanceID, generation, installation.ID); err != nil {
			return fmt.Errorf("record client login generation: %w", err)
		}
		if !replay {
			if _, err := tx.Exec(ctx, `INSERT INTO client_login_requests(owner_id,key_hash,request_hash,installation_id)
  VALUES ($1,$2,$3,$4)`, requested.OwnerID, keyHash, requestHash, installation.ID); err != nil {
				return fmt.Errorf("record client login attempt: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `UPDATE client_installations SET display_name=$2,expires_at=$3 WHERE id=$1`, installation.ID, requested.DisplayName, requested.ExpiresAt)
		if err != nil {
			return fmt.Errorf("renew client installation: %w", err)
		}
		installation.DisplayName = requested.DisplayName
		installation.ExpiresAt = requested.ExpiresAt
		issued, err = issue(installation.ID)
		if err != nil {
			return err
		}
		if err := validateIssuedClientTokens(installation, issued); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id=$1 AND revoked_at IS NULL`, installation.ID); err != nil {
			return fmt.Errorf("rotate client credentials: %w", err)
		}
		if err := cancelWorkspaceSyncSessions(ctx, tx, requested.OwnerID, installation.ID, "", false); err != nil {
			return err
		}
		q := sqlcgen.New(tx)
		if _, err := createToken(ctx, q, issued.Client); err != nil {
			return err
		}
		if issued.Sync != nil {
			if _, err := createToken(ctx, q, *issued.Sync); err != nil {
				return err
			}
		}
		if issued.Web != nil {
			if _, err := createToken(ctx, q, *issued.Web); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,ip)
  VALUES ($1,'session','client.login',$2,$3)`, requested.OwnerID, installation.ID, ip); err != nil {
			return fmt.Errorf("audit client login: %w", err)
		}
		return nil
	})
	return installation, issued, err
}

func legacyAndroidInstanceID(ownerID, installationID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(ownerID) + "\x00legacy-android\x00" + installationID))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func validateIssuedClientTokens(installation identity.ClientInstallation, issued app.ClientIssuedTokens) error {
	if issued.Client.Kind != identity.KindClient || issued.Client.OwnerID != installation.OwnerID ||
		issued.Client.ClientInstallationID != installation.ID || !issued.Client.ExpiresAt.Equal(installation.ExpiresAt) {
		return domain.InvalidArgument("client_credential", "client credential is invalid")
	}
	if (issued.Sync == nil) != (installation.Platform != "desktop") || (issued.Web == nil) != (installation.Platform != "desktop") {
		return domain.InvalidArgument("platform", "client credential set does not match platform")
	}
	if issued.Sync != nil && (issued.Sync.Kind != identity.KindDevice || issued.Sync.OwnerID != installation.OwnerID ||
		issued.Sync.ClientInstallationID != installation.ID || !issued.Sync.ExpiresAt.Equal(installation.ExpiresAt) ||
		len(issued.Sync.WorkspaceIDs) != 0 || len(issued.Sync.Scopes) != 1 || issued.Sync.Scopes[0] != identity.ScopeWorkspaceSync) {
		return domain.InvalidArgument("sync_credential", "sync credential is invalid")
	}
	if issued.Web != nil && (issued.Web.Kind != identity.KindSession || issued.Web.OwnerID != installation.OwnerID ||
		issued.Web.ClientInstallationID != installation.ID || !issued.Web.ExpiresAt.After(time.Now()) ||
		issued.Web.ExpiresAt.After(installation.ExpiresAt)) {
		return domain.InvalidArgument("web_session_credential", "Web session credential is invalid")
	}
	return nil
}

func (s *Store) IssueClientWebSession(ctx context.Context, principal identity.Principal, token identity.Token) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var platform string
		var active bool
		err := tx.QueryRow(ctx, `SELECT platform,revoked_at IS NULL AND expires_at>now()
  FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, principal.ClientInstallationID, principal.UserID).Scan(&platform, &active)
		if err != nil || !active || (platform != "desktop" && platform != "ios") {
			return identity.ErrInvalidCredential
		}
		var clientActive bool
		err = tx.QueryRow(ctx, `SELECT kind='client' AND revoked_at IS NULL AND expires_at>now()
  FROM tokens WHERE id=$1 AND owner_id=$2 AND client_installation_id=$3 FOR SHARE`, principal.TokenID, principal.UserID, principal.ClientInstallationID).Scan(&clientActive)
		if err != nil || !clientActive {
			return identity.ErrInvalidCredential
		}
		if token.Kind != identity.KindSession || token.OwnerID != principal.UserID || token.ClientInstallationID != principal.ClientInstallationID {
			return domain.InvalidArgument("web_session_credential", "Web session credential is invalid")
		}
		_, err = createToken(ctx, sqlcgen.New(tx), token)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target)
  VALUES ($1,'client','client.web_session',$2)`, principal.UserID, token.ID); err != nil {
			return fmt.Errorf("audit client Web session: %w", err)
		}
		return nil
	})
}

func scanClientInstallation(row interface{ Scan(...any) error }) (identity.ClientInstallation, error) {
	var result identity.ClientInstallation
	var lastUsed, revoked pgtype.Timestamptz
	err := row.Scan(&result.ID, &result.OwnerID, &result.DisplayName, &result.Platform,
		&result.ExpiresAt, &result.CreatedAt, &lastUsed, &revoked)
	if err != nil {
		return identity.ClientInstallation{}, storeError("get client installation", "client installation", err)
	}
	if lastUsed.Valid {
		at := lastUsed.Time
		result.LastUsedAt = &at
	}
	if revoked.Valid {
		at := revoked.Time
		result.RevokedAt = &at
	}
	return result, nil
}

func (s *Store) ListClientInstallations(ctx context.Context, ownerID string) ([]identity.ClientInstallation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,owner_id,display_name,platform,
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
	return scanClientInstallation(s.pool.QueryRow(ctx, `SELECT id,owner_id,display_name,platform,
  COALESCE(expires_at,created_at),created_at,last_used_at,revoked_at FROM client_installations WHERE id=$1`, id))
}

func (s *Store) RenameClientInstallation(ctx context.Context, ownerID, id, displayName string) error {
	updated, err := s.q.RenameClientInstallation(ctx, sqlcgen.RenameClientInstallationParams{
		DisplayName: displayName, InstallationID: id, OwnerID: ownerID,
	})
	if err != nil {
		return fmt.Errorf("rename client installation: %w", err)
	}
	if updated != 1 {
		return domain.NewError(domain.CodeNotFound, "device not found")
	}
	return nil
}

func (s *Store) RevokeClientInstallation(ctx context.Context, id, ownerID, ip string, actorKind audit.ActorKind, disableRelay func(context.Context) error) error {
	if actorKind != audit.ActorSession && actorKind != audit.ActorClient {
		return domain.InvalidArgument("actor_kind", "client revocation actor is invalid")
	}
	// Installation, its tokens and audit commit together so every credential is revoked at once.
	return s.withWorkspaceSyncOwnerTx(ctx, ownerID, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, true); err != nil {
			return err
		}
		var active bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL FROM client_installations WHERE id=$1 AND owner_id=$2 FOR UPDATE`, id, ownerID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			return domain.NewError(domain.CodeNotFound, "client installation not found")
		}
		if err != nil {
			return fmt.Errorf("lock client installation for revocation: %w", err)
		}
		if disableRelay != nil {
			if err := disableRelay(ctx); err != nil {
				return fmt.Errorf("disable Workspace sync relay before installation revocation: %w", err)
			}
		}
		command, err := tx.Exec(ctx, `UPDATE client_installations SET revoked_at=COALESCE(revoked_at,now())
  WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL`, id, ownerID)
		if err != nil {
			return fmt.Errorf("revoke client installation: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeNotFound, "client installation not found")
		}
		if _, err := stopTasksForAuthorization(ctx, tx, ownerID, "", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id=$1 AND revoked_at IS NULL`, id); err != nil {
			return fmt.Errorf("revoke client credentials: %w", err)
		}
		if err := revokeWorkspaceSyncDevicesForInstallation(ctx, tx, ownerID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,ip)
  VALUES ($1,$2,'client.revoke',$3,$4)`, ownerID, string(actorKind), id, ip); err != nil {
			return fmt.Errorf("audit client revocation: %w", err)
		}
		return nil
	}, nil, nil)
}

func (s *Store) ActiveClientInstallation(ctx context.Context, id, userID string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>now()
  FROM client_installations WHERE id=$1 AND owner_id=$2`, id, userID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check client installation: %w", err)
	}
	return active, nil
}
