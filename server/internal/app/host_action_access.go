package app

import (
	"context"
	"sort"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func validateTaskPrincipal(principal identity.Principal) error {
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindAPIKey && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}

func authorizeTaskHost(ctx context.Context, authorizer TaskAuthorizer, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if !domain.ValidHostID(hostID) {
		return task.Authorization{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if principal.Kind == identity.KindAPIKey {
		if !principal.HasScope(scope) {
			return task.Authorization{}, domain.NewError(domain.CodeForbidden, "task scope is required")
		}
		if !principal.AllowsHost(hostID) {
			return task.Authorization{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
	}
	approvedScopes := append([]identity.Scope(nil), principal.Scopes...)
	if err := authorizer.AuthorizeTaskAction(ctx, principal.UserID, principal.Kind, principal.TokenID, hostID, approvedScopes, scope); err != nil {
		return task.Authorization{}, err
	}
	return authorizationSnapshot(principal), nil
}

func authorizationSnapshot(principal identity.Principal) task.Authorization {
	scopes := make([]string, 0, len(principal.Scopes))
	for _, scope := range principal.Scopes {
		scopes = append(scopes, string(scope))
	}
	sort.Strings(scopes)
	return task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID, ClientInstallationID: principal.ClientInstallationID, Scopes: scopes}
}

func hideTaskAccessError(err error) error {
	if domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "resource not found")
	}
	return err
}
