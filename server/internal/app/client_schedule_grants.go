package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const clientScheduleGrantTTL = 12 * time.Hour

type ClientScheduleGrantResult struct {
	Credential string
	ExpiresAt  time.Time
}

// IssueScheduleGrant returns a separate, short-lived credential for the
// account's iOS schedule editor. It is never included in the ordinary client
// login response.
func (s *ClientLoginService) IssueScheduleGrant(ctx context.Context, principal identity.Principal) (ClientScheduleGrantResult, error) {
	if err := validateScheduleGrantIssuer(principal); err != nil {
		return ClientScheduleGrantResult{}, err
	}
	expiresAt := time.Now().UTC().Add(clientScheduleGrantTTL)
	type issued struct {
		token      identity.Token
		credential string
	}
	value, err := domain.CreateWithIDRetry(domain.NewTokenID, func(id string) (issued, error) {
		credential, secretHash, err := identity.NewCredential(identity.KindClient, id)
		if err != nil {
			return issued{}, err
		}
		token := identity.Token{
			ID: id, OwnerID: principal.UserID, ClientInstallationID: principal.ClientInstallationID,
			Kind: identity.KindClient, Name: "iOS cloud schedule grant",
			Scopes:     []identity.Scope{identity.ScopeScheduleRead, identity.ScopeScheduleManage},
			SecretHash: secretHash, ExpiresAt: expiresAt,
		}
		if err := s.store.IssueClientScheduleGrant(ctx, principal, token); err != nil {
			return issued{}, err
		}
		return issued{token: token, credential: credential}, nil
	}, isUniqueViolation)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredential) {
			return ClientScheduleGrantResult{}, domain.NewError(domain.CodeUnauthenticated, "client installation is unavailable")
		}
		return ClientScheduleGrantResult{}, fmt.Errorf("issue client schedule grant: %w", err)
	}
	return ClientScheduleGrantResult{Credential: value.credential, ExpiresAt: value.token.ExpiresAt}, nil
}

// RevokeScheduleGrant only revokes grants belonging to the current
// installation; it does not revoke the regular client credential or cloud
// tasks that were already accepted from a schedule.
func (s *ClientLoginService) RevokeScheduleGrant(ctx context.Context, principal identity.Principal) error {
	if err := validateScheduleGrantIssuer(principal); err != nil {
		return err
	}
	return s.store.RevokeClientScheduleGrants(ctx, principal)
}

func validateScheduleGrantIssuer(principal identity.Principal) error {
	if principal.Kind != identity.KindClient || !domain.ValidUserID(principal.UserID) ||
		!domain.ValidTokenID(principal.TokenID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) ||
		len(principal.Scopes) != 0 || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 {
		return domain.NewError(domain.CodeForbidden, "regular client credential required")
	}
	return nil
}
