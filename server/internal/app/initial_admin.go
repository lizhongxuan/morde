package app

import (
	"context"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type initialAdminStore interface {
	GetUserByName(context.Context, string) (identity.User, error)
	EnsureInitialAdmin(context.Context, identity.User) error
}

// EnsureInitialAdmin creates the default administrator only for an empty user table.
func EnsureInitialAdmin(ctx context.Context, store initialAdminStore) error {
	if store == nil {
		return fmt.Errorf("initial administrator store is required")
	}
	user, err := store.GetUserByName(ctx, "admin")
	if err == nil {
		if user.Role == identity.RoleSuperAdmin && user.Status == identity.StatusApproved {
			return nil
		}
		return domain.NewError(domain.CodeConflict, "admin account is not the super administrator")
	}
	if domain.CodeOf(err) != domain.CodeNotFound {
		return fmt.Errorf("check initial administrator: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash initial administrator password: %w", err)
	}
	id, err := domain.NewUserID()
	if err != nil {
		return err
	}
	return store.EnsureInitialAdmin(ctx, identity.User{
		ID: id, Name: "admin", PasswordHash: string(hash), Role: identity.RoleSuperAdmin,
		Status: identity.StatusApproved, MustChangePassword: true,
		QuotaHosts: 3, QuotaCPUMilli: 4000, QuotaMemMiB: 8192,
	})
}
