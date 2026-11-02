package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

// PersonalDataQuotaUserStore reads quota state for the authenticated owner.
// Implementations must recheck the principal's live authorization in storage.
type PersonalDataQuotaUserStore interface {
	GetPersonalDataQuotaUser(context.Context, identity.Principal) (PersonalDataQuota, error)
}

type PersonalDataQuotaUserService struct {
	store PersonalDataQuotaUserStore
}

func NewPersonalDataQuotaUserService(store PersonalDataQuotaUserStore) (*PersonalDataQuotaUserService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("personal_data_quota", "personal data quota user store is required")
	}
	return &PersonalDataQuotaUserService{store: store}, nil
}

func (s *PersonalDataQuotaUserService) GetPersonalDataQuota(ctx context.Context, principal identity.Principal) (PersonalDataQuota, error) {
	if err := validatePersonalDataQuotaUserPrincipal(principal); err != nil {
		return PersonalDataQuota{}, err
	}
	value, err := s.store.GetPersonalDataQuotaUser(ctx, principal)
	if err != nil {
		return PersonalDataQuota{}, err
	}
	if err := validatePersonalDataQuota(value, principal.UserID); err != nil {
		return PersonalDataQuota{}, domain.NewError(domain.CodeInternal, "personal data quota receipt is invalid")
	}
	return value, nil
}

func validatePersonalDataQuotaUserPrincipal(principal identity.Principal) error {
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
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if principal.MustChangePassword {
		return domain.NewError(domain.CodeForbidden, "password change is required before continuing")
	}
	return nil
}
