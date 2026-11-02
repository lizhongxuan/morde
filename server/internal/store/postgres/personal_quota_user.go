package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func (s *Store) GetPersonalDataQuotaUser(ctx context.Context, principal identity.Principal) (app.PersonalDataQuota, error) {
	if err := validatePersonalDataQuotaUserPrincipalStore(principal); err != nil {
		return app.PersonalDataQuota{}, err
	}

	var value app.PersonalDataQuota
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		// Reuse the personal mutation authorization fence so current account,
		// installation, and credential revocations are checked in this transaction.
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := lockPersonalDataQuotaOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		value, err = loadPersonalDataQuota(ctx, tx, instanceID, principal.UserID)
		if err != nil {
			return err
		}
		if value.OwnerID != principal.UserID || value.UsedBytes < 0 || value.ReservedBytes < 0 || value.QuotaBytes < 1 ||
			value.MaxAttachmentBytes < 1 || value.MaxAttachmentBytes > personal.MaxAttachmentBytes || value.Version < 1 {
			return domain.NewError(domain.CodeInternal, "personal data quota row is invalid")
		}
		return nil
	})
	if err != nil {
		return app.PersonalDataQuota{}, err
	}
	return value, nil
}

func validatePersonalDataQuotaUserPrincipalStore(principal identity.Principal) error {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeForbidden, "personal data owner is not authorized")
	}
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.HostScope != "" || principal.HostGeneration != 0 || principal.ReplicaWorkspaceID != "" ||
		principal.BindingVersion != 0 || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 {
		return domain.NewError(domain.CodeForbidden, "host-scoped credentials cannot read personal data quota")
	}
	if !principal.HasScope(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeForbidden, "personal data quota scope is required")
	}
	if principal.Role != identity.RoleUser && (principal.Kind != identity.KindSession || !principal.Role.IsAdmin()) {
		return domain.NewError(domain.CodeForbidden, "personal data owner is not authorized")
	}
	if principal.MustChangePassword || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "personal data owner is not authorized")
	}
	if principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.ClientInstallationID != "" && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}

var _ app.PersonalDataQuotaUserStore = (*Store)(nil)
