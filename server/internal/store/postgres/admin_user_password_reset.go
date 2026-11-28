package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ResetUserPasswordWithAudit(ctx context.Context, actorID string, expectedStatus identity.AccountStatus,
	userID, passwordHash string, entry audit.Entry, disableRelay func(context.Context) error) error {
	if !domain.ValidUserID(actorID) || !domain.ValidUserID(userID) || !expectedStatus.Valid() || passwordHash == "" ||
		entry.ActorID != actorID || entry.Action != audit.ActionUserUpdate || entry.Target != userID {
		return domain.InvalidArgument("password_reset", "password reset fields are invalid")
	}
	auditRecord, err := auditParams(entry)
	if err != nil {
		return err
	}
	return s.withWorkspaceSyncOwnerTx(ctx, userID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('morde:admin-user-update', 0))`); err != nil {
			return fmt.Errorf("lock admin user updates: %w", err)
		}
		if _, err := lockOwnerExecutionControl(ctx, tx, userID, true); err != nil {
			return err
		}
		var currentStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&currentStatus); err != nil {
			return storeError("reset user password", "user", err)
		}
		if identity.AccountStatus(currentStatus) != expectedStatus {
			return domain.NewError(domain.CodeConflict, "account status changed; refresh the user list")
		}
		current, err := sqlcgen.New(tx).GetUserByID(ctx, userID)
		if err != nil {
			return storeError("reset user password", "user", err)
		}
		if disableRelay != nil {
			if err := disableRelay(ctx); err != nil {
				return fmt.Errorf("disable Workspace sync relay before password reset: %w", err)
			}
		}
		if _, err := sqlcgen.New(tx).UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID: current.ID, Name: current.Name, PasswordHash: passwordHash, Role: current.Role,
			Status: current.Status, MustChangePassword: true,
			QuotaHosts: current.QuotaHosts, QuotaCpuMilli: current.QuotaCpuMilli, QuotaMemMib: current.QuotaMemMib,
			DefaultHostCpuMilli: current.DefaultHostCpuMilli, DefaultHostMemMib: current.DefaultHostMemMib,
			ExpectedStatus: currentStatus,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "account status changed; refresh the user list")
			}
			return storeError("reset user password", "user", err)
		}
		if err := stopAllOwnerTasks(ctx, tx, userID); err != nil {
			return err
		}
		if err := requestMCPRuntimeCancellationsForActor(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=COALESCE(revoked_at, now()) WHERE owner_id=$1 AND revoked_at IS NULL`, userID); err != nil {
			return fmt.Errorf("revoke account credentials: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE client_installations SET revoked_at=COALESCE(revoked_at, now())
WHERE owner_id=$1 AND revoked_at IS NULL`, userID); err != nil {
			return fmt.Errorf("revoke account client installations: %w", err)
		}
		if err := revokeWorkspaceSyncDevicesForOwner(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, auditRecord); err != nil {
			return fmt.Errorf("write password reset audit: %w", err)
		}
		return nil
	}, nil, nil)
}
